package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Images reach Claude and the UI by path, and a path outside the attachments folder is never read.
func TestOnlyAttachmentsAreRead(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	secret := filepath.Join(e.root, "secret.png")
	writePNG(t, secret, 8, 8)

	if _, err := e.c.ReadImage(ctx, secret, 0); err == nil {
		t.Error("read an image outside the attachments")
	}
	att, err := e.c.SaveImage(image.NewRGBA(image.Rect(0, 0, 10, 10)))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(filepath.Dir(att), "..", "secret.png"),
		filepath.Join(filepath.Dir(att), "sub", "x.png"),
		filepath.Base(att),
		"",
		filepath.Dir(att),
	} {
		if _, err := e.c.ReadImage(ctx, p, 0); err == nil {
			t.Errorf("read %q", p)
		}
	}
	// A link in the attachments folder to a file elsewhere isn't followed.
	link := filepath.Join(filepath.Dir(att), "link.png")
	if err := os.Symlink(secret, link); err == nil {
		if _, err := e.c.ReadImage(ctx, link, 0); err == nil {
			t.Error("followed a link out of the attachments")
		}
	}
	if data, err := e.c.ReadImage(ctx, att, 0); err != nil || len(data) == 0 {
		t.Errorf("an attachment: %d bytes, %v", len(data), err)
	}

	// A message can't name a file that isn't an attachment either.
	v, err := e.c.EnsureOrchestrator()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.Send(ctx, v.ID, "look", secret); err == nil || !strings.Contains(err.Error(), "attachment") {
		t.Errorf("sent a file outside the attachments: %v", err)
	}
}

func TestSaveImageData(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	encode := func(w, h int) []byte {
		var b bytes.Buffer
		if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	small, err := e.c.SaveImageData(ctx, encode(40, 20))
	if err != nil {
		t.Fatal(err)
	}
	if w, h := pngSize(t, small); w != 40 || h != 20 {
		t.Errorf("stored %dx%d", w, h)
	}
	if fi, err := os.Stat(small); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("attachment mode %v, %v", fi.Mode(), err)
	}
	big, err := e.c.SaveImageData(ctx, encode(3000, 1500))
	if err != nil {
		t.Fatal(err)
	}
	if w, h := pngSize(t, big); w != 2000 || h != 1000 {
		t.Errorf("stored %dx%d, want 2000x1000", w, h)
	}
	if _, err := e.c.SaveImageData(ctx, []byte("hello")); err == nil {
		t.Error("stored text as an image")
	}
	if _, err := e.c.SaveImageData(ctx, make([]byte, maxImageFile+1)); err == nil {
		t.Error("stored a file over the limit")
	}
	// A header that claims more pixels than Solo will decode is refused before decoding: this PNG says
	// it is 60000 by 60000 and holds no pixels.
	huge := encode(1, 1)
	copy(huge[16:24], []byte{0, 0, 0xea, 0x60, 0, 0, 0xea, 0x60})
	binary.BigEndian.PutUint32(huge[29:33], crc32.ChecksumIEEE(huge[12:29]))
	if _, err := e.c.SaveImageData(ctx, huge); err == nil || !strings.Contains(err.Error(), "pixels") {
		t.Errorf("a 3.6 gigapixel header: %v", err)
	}

	// A preview comes scaled down; the full image as it is stored.
	prev, err := e.c.ReadImage(ctx, big, 640)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(prev))
	if err != nil || cfg.Width != 640 || cfg.Height != 320 {
		t.Errorf("preview %dx%d, %v", cfg.Width, cfg.Height, err)
	}
	same, err := e.c.ReadImage(ctx, small, 640)
	if err != nil || len(same) == 0 {
		t.Fatal(err)
	}
	if cfg, _, _ := image.DecodeConfig(bytes.NewReader(same)); cfg.Width != 40 {
		t.Errorf("a small image was scaled to %d", cfg.Width)
	}
}

// Two landing actions on a thread can't run at once: the second is refused while the first runs, and
// goes ahead after it.
func TestLandingLock(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Lock", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)

	release, err := e.c.land(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Merge(ctx, v.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("Merge during a landing action: %v", err)
	}
	if _, err := e.c.MergeLocal(ctx, v.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("MergeLocal: %v", err)
	}
	if _, err := e.c.OpenPR(ctx, v.ID, false); !errors.Is(err, ErrBusy) {
		t.Errorf("OpenPR: %v", err)
	}
	if err := e.c.MarkReady(ctx, v.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("MarkReady: %v", err)
	}
	if err := e.c.Discard(ctx, v.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("Discard: %v", err)
	}
	if err := e.c.Done(ctx, v.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("Done: %v", err)
	}
	if err := e.c.Restore(ctx, v.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("Restore: %v", err)
	}
	if got, _ := e.c.Thread(v.ID); got.Status != StatusIdle {
		t.Errorf("a refused action changed the thread to %s", got.Status)
	}
	// Another thread isn't held up.
	if other, err := e.c.land("other"); err != nil {
		t.Errorf("a second thread: %v", err)
	} else {
		other()
	}
	release()
	if err := e.c.Discard(ctx, v.ID); err != nil {
		t.Errorf("Discard after the first action ended: %v", err)
	}
	if err := e.c.Discard(ctx, v.ID); err == nil || errors.Is(err, ErrBusy) {
		t.Errorf("a second Discard: %v", err)
	}
}
