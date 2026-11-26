//go:build !js

package rpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent/agenttest"
	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
)

// filesRig is a rig whose machine stores files with a real engine, in dataDir.
func filesRig(t *testing.T, dev Device, host ...func(*Options)) (*rig, string) {
	t.Helper()
	dataDir := t.TempDir()
	c, err := core.New(core.Config{DataDir: dataDir, ClaudePath: agenttest.ClaudePath(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	m := newMem()
	m.addThread("a", core.StatusIdle)
	m.files = c
	return newRigWith(t, m, dev, ClientOptions{}, host...), dataDir
}

// partials lists what uploads left in the machine's folder for files on their way.
func partials(dataDir string) []string {
	names, _ := filepath.Glob(filepath.Join(dataDir, "uploads", "*"))
	return names
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// A file crosses in parts and arrives whole, under its name, with the progress told as it goes. The
// audit log has its beginning and end, not each part.
func TestUploadFile(t *testing.T) {
	r, dataDir := filesRig(t, Device{ID: "d1", Name: "phone"})
	data := randomBytes(2*maxUploadPart + 12345)
	var mu sync.Mutex
	var seen []int64
	ref, err := r.c.SaveFile(context.Background(), "Field notes.pdf", data, func(n int64) {
		mu.Lock()
		seen = append(seen, n)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := core.FileName(ref); !ok || name != "Field notes.pdf" || filepath.Dir(ref) != filepath.Join(dataDir, "attachments") {
		t.Fatalf("stored as %s", ref)
	}
	got, err := os.ReadFile(ref)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the file arrived as %d bytes (%v), want %d", len(got), err, len(data))
	}
	if len(seen) != 3 || seen[2] != int64(len(data)) || seen[0] != maxUploadPart {
		t.Errorf("progress %v", seen)
	}
	if p := partials(dataDir); len(p) != 0 {
		t.Errorf("left in uploads: %v", p)
	}
	r.amu.Lock()
	audit := strings.Join(r.audit, "\n")
	r.amu.Unlock()
	if !strings.Contains(audit, "phone uploadBegin Field notes.pdf <nil>") || !strings.Contains(audit, "phone uploadEnd Field notes.pdf <nil>") ||
		strings.Contains(audit, "uploadPart") {
		t.Errorf("audit log:\n%s", audit)
	}

	// An empty file arrives too, and the file can go in a message.
	empty, err := r.c.SaveFile(context.Background(), "empty.txt", nil, nil)
	if err != nil || !core.IsFileAttachment(empty) {
		t.Fatalf("an empty file: %q, %v", empty, err)
	}
	if err := r.c.Send(context.Background(), "a", "both", ref, empty); err != nil {
		t.Fatal(err)
	}
}

// The machine checks what arrives: a file whose checksum doesn't match, or that is longer than it said,
// or whose parts come out of order, is refused and removed, and a part too large for one request is
// refused before it is read.
func TestUploadChecks(t *testing.T) {
	r, dataDir := filesRig(t, Device{ID: "d1"})
	ctx := context.Background()
	attachments := func() []string {
		names, _ := filepath.Glob(filepath.Join(dataDir, "attachments", "*"))
		return names
	}
	begin := func(size int64) string {
		t.Helper()
		var res uploadResult
		if err := r.c.do(ctx, "uploadBegin", uploadBeginParams{Name: "x.bin", Size: size}, &res); err != nil {
			t.Fatal(err)
		}
		return res.Upload
	}
	part := func(id string, off int64, data []byte) error {
		_, err := r.c.call(ctx, "uploadPart", uploadParams{Upload: id, Offset: off}, data)
		return err
	}
	data := randomBytes(3000)

	id := begin(3000)
	if err := part(id, 0, data); err != nil {
		t.Fatal(err)
	}
	wrong := sha256.Sum256([]byte("not this"))
	err := r.c.do(ctx, "uploadEnd", uploadParams{Upload: id, Sum: hex.EncodeToString(wrong[:])}, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("a wrong checksum: %v", err)
	}

	id = begin(1000)
	if err := part(id, 0, data); err == nil {
		t.Error("took more than the file said")
	}
	if err := part(id, 0, data[:10]); err == nil {
		t.Error("an upload went on after it was refused")
	}

	id = begin(3000)
	part(id, 0, data[:1000])
	if err := part(id, 2000, data[2000:]); err == nil {
		t.Error("took a part out of order")
	}

	id = begin(3000)
	part(id, 0, data[:1000])
	sum := sha256.Sum256(data)
	if err := r.c.do(ctx, "uploadEnd", uploadParams{Upload: id, Sum: hex.EncodeToString(sum[:])}, nil); err == nil {
		t.Error("finished a file that hadn't all arrived")
	}

	if len(attachments()) != 0 || len(partials(dataDir)) != 0 {
		t.Errorf("left: %v %v", attachments(), partials(dataDir))
	}

	id = begin(3 << 20)
	if err := part(id, 0, make([]byte, maxUploadPart+1)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("a part over the limit: %v", err)
	}
	if err := r.c.do(ctx, "uploadBegin", uploadBeginParams{Name: "x.bin", Size: api.MaxFileUpload + 1}, nil); err == nil {
		t.Error("began a file over the limit")
	}
	if _, err := r.c.SaveFile(ctx, "big.bin", make([]byte, api.MaxFileUpload+1), nil); err == nil {
		t.Error("the client sent a file over the limit")
	}
	if err := r.c.do(ctx, "uploadBegin", uploadBeginParams{Name: "", Size: 1}, nil); err == nil {
		t.Error("began a file without a name")
	}
	if err := part("0123", 0, data); err == nil {
		t.Error("wrote to an upload that never began")
	}
}

// Limits: a device sends a few files at a time, a device that may only look sends none, one device
// can't write to another's upload, and other requests still carry no bytes.
func TestUploadLimits(t *testing.T) {
	r, _ := filesRig(t, Device{ID: "d1"})
	ctx := context.Background()
	var ids []string
	for i := 0; i < maxUploads; i++ {
		var res uploadResult
		if err := r.c.do(ctx, "uploadBegin", uploadBeginParams{Name: "x", Size: 10}, &res); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Upload)
	}
	if err := r.c.do(ctx, "uploadBegin", uploadBeginParams{Name: "x", Size: 10}, nil); err == nil {
		t.Errorf("began more than %d uploads at once", maxUploads)
	}
	if _, err := r.c.call(ctx, "send", sendParams{Thread: "a", Text: "x"}, []byte("bytes")); err == nil {
		t.Error("a message carried bytes")
	}

	// Another device, on the same machine, can't touch them.
	other := NewClient(ClientOptions{Random: countingReader{}})
	host, device := pipe()
	t.Cleanup(func() { device.Close() })
	go r.h.Serve(host, Device{ID: "d2"})
	go other.Run(device)
	r.wait("the other device to sync", func() bool { return other.Status().Synced })
	if _, err := other.call(ctx, "uploadPart", uploadParams{Upload: ids[0]}, []byte("0123456789")); err == nil {
		t.Error("a device wrote to another's upload")
	}

	ro, _ := filesRig(t, Device{ID: "d3", ReadOnly: true})
	if _, err := ro.c.SaveFile(ctx, "x.txt", []byte("hello"), nil); err == nil {
		t.Error("a read-only device sent a file")
	}
}

// Removing a file while it uploads stops it, and the machine removes what arrived; so does a
// connection that drops and doesn't come back in time. One that comes back in time carries on.
func TestUploadStops(t *testing.T) {
	r, dataDir := filesRig(t, Device{ID: "d1"}, func(o *Options) { o.UploadIdle = 2 * time.Second })
	data := randomBytes(3 * maxUploadPart)

	ctx, cancel := context.WithCancel(context.Background())
	_, err := r.c.SaveFile(ctx, "stopped.bin", data, func(n int64) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled upload: %v", err)
	}
	r.wait("the machine to remove what arrived", func() bool { return len(partials(dataDir)) == 0 })

	// The connection drops after the first part and comes back: the upload carries on.
	once := sync.Once{}
	ref, err := r.c.SaveFile(context.Background(), "resumed.bin", data, func(n int64) {
		once.Do(func() {
			r.disconnect()
			go r.connect()
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(ref); !bytes.Equal(got, data) {
		t.Fatal("the resumed upload arrived changed")
	}

	// The connection drops for good: the machine removes the part that arrived once no more come.
	var res uploadResult
	if err := r.c.do(context.Background(), "uploadBegin", uploadBeginParams{Name: "dropped.bin", Size: int64(len(data))}, &res); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.call(context.Background(), "uploadPart", uploadParams{Upload: res.Upload}, data[:maxUploadPart]); err != nil {
		t.Fatal(err)
	}
	if len(partials(dataDir)) != 1 {
		t.Fatalf("partials: %v", partials(dataDir))
	}
	r.disconnect()
	r.wait("the machine to remove the dropped upload", func() bool { return len(partials(dataDir)) == 0 })
	r.connect()
	if _, err := r.c.call(context.Background(), "uploadPart", uploadParams{Upload: res.Upload, Offset: maxUploadPart}, data[maxUploadPart:2*maxUploadPart]); err == nil {
		t.Error("an upload went on after the machine gave it up")
	}
	// A client whose connection is gone says so plainly.
	if err := uploadError(context.Background(), ErrOffline); !errors.Is(err, errUploadDropped) {
		t.Errorf("a dropped upload says %v", err)
	}
}
