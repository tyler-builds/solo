package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tyler-builds/solo/internal/api"
)

// A file attached in a page crosses to the machine in parts, each a request of its own: a message is at
// most a megabyte, and a request that carried the whole file would hold the link, and the machine's
// memory, for as long as it took. uploadBegin says the file's name and size; uploadPart carries each
// part in order, at the offset of what has arrived; uploadEnd gives the SHA-256 of the whole, which the
// machine checks before the file becomes an attachment (api.Uploads). Until then it is in a folder
// nothing reads. The device can stop it (uploadCancel), and a file that waits for its next part longer
// than Options.UploadIdle is removed: a connection that drops for good leaves nothing behind, and one
// that comes back in time carries on, since a part sent again on a new connection is answered once.

const (
	// maxUploadPart is the most bytes one uploadPart carries: about six parts of a message.
	maxUploadPart = 1 << 20
	// maxUploads is how many files one device may be sending at a time.
	maxUploads = 4
	// uploadIdle is how long an upload waits for its next part. A part's request is tried on up to
	// three connections, each waited for up to connectWait.
	uploadIdle = 90 * time.Second
)

// upload is a file a device is sending.
type upload struct {
	dev  string
	name string
	size int64

	mu   sync.Mutex // one part at a time, in order
	f    api.FileUpload
	n    int64
	idle *time.Timer
}

func uploadKey(dev, id string) string { return dev + "\x00" + id }

// takeUpload finds a device's upload and holds it for one part, or returns nil.
func (h *Host) takeUpload(dev, id string) *upload {
	h.mu.Lock()
	u := h.uploads[uploadKey(dev, id)]
	h.mu.Unlock()
	if u == nil {
		return nil
	}
	u.mu.Lock()
	h.mu.Lock()
	gone := h.uploads[uploadKey(dev, id)] != u
	h.mu.Unlock()
	if gone { // it ended while this waited
		u.mu.Unlock()
		return nil
	}
	return u
}

// dropUpload forgets an upload, and removes what arrived of it unless it was finished.
func (h *Host) dropUpload(key string, u *upload, abort bool) {
	h.mu.Lock()
	if h.uploads[key] == u {
		delete(h.uploads, key)
	}
	h.mu.Unlock()
	u.idle.Stop()
	if abort {
		u.f.Abort()
	}
}

// abortUploads removes the uploads of a device, or every one for "".
func (h *Host) abortUploads(dev string) {
	h.mu.Lock()
	var drop []*upload
	for key, u := range h.uploads {
		if dev == "" || u.dev == dev {
			drop = append(drop, u)
			delete(h.uploads, key)
		}
	}
	h.mu.Unlock()
	for _, u := range drop {
		u.idle.Stop()
		u.f.Abort()
	}
}

func uploadBegin(_ context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
	a, err := decode[uploadBeginParams](p)
	if err != nil {
		return nil, nil, "", err
	}
	if a.Name == "" || len(a.Name) > 1024 || !utf8.ValidString(a.Name) {
		return nil, nil, "", invalid("that isn't a file's name")
	}
	if a.Size < 0 || a.Size > api.MaxFileUpload {
		return nil, nil, a.Name, invalid("a file is at most %d MB", api.MaxFileUpload>>20)
	}
	ub, ok := c.h.b.(api.Uploads)
	if !ok {
		return nil, nil, a.Name, invalid("this machine doesn't take files")
	}
	h := c.h
	h.mu.Lock()
	n := 0
	for _, u := range h.uploads {
		if u.dev == c.dev.ID {
			n++
		}
	}
	h.mu.Unlock()
	if n >= maxUploads {
		return nil, nil, a.Name, &Error{Code: CodeBusy, Message: "Too many files are on their way at once."}
	}
	f, err := ub.BeginFile(a.Name, a.Size)
	if err != nil {
		return nil, nil, a.Name, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		f.Abort()
		return nil, nil, a.Name, err
	}
	id := hex.EncodeToString(raw[:])
	key := uploadKey(c.dev.ID, id)
	u := &upload{dev: c.dev.ID, name: a.Name, size: a.Size, f: f}
	u.idle = time.AfterFunc(h.opts.UploadIdle, func() { h.dropUpload(key, u, true) })
	h.mu.Lock()
	h.uploads[key] = u
	h.mu.Unlock()
	return uploadResult{Upload: id}, nil, a.Name, nil
}

func uploadPart(_ context.Context, c *conn, p json.RawMessage, blob []byte) (any, []byte, string, error) {
	a, err := decode[uploadParams](p)
	if err == nil {
		err = checkID("upload", a.Upload)
	}
	if err != nil {
		return nil, nil, "", err
	}
	if len(blob) == 0 {
		return nil, nil, "", invalid("the part is empty")
	}
	u := c.h.takeUpload(c.dev.ID, a.Upload)
	if u == nil {
		return nil, nil, "", invalid("that upload has ended")
	}
	defer u.mu.Unlock()
	key := uploadKey(c.dev.ID, a.Upload)
	if a.Offset != u.n {
		c.h.dropUpload(key, u, true)
		return nil, nil, u.name, invalid("a part of the file arrived out of order")
	}
	if int64(len(blob)) > u.size-u.n {
		c.h.dropUpload(key, u, true)
		return nil, nil, u.name, invalid("the file is larger than it said")
	}
	if _, err := u.f.Write(blob); err != nil {
		c.h.dropUpload(key, u, true)
		return nil, nil, u.name, err
	}
	u.n += int64(len(blob))
	u.idle.Reset(c.h.opts.UploadIdle)
	return nil, nil, u.name, nil
}

func uploadEnd(_ context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
	a, err := decode[uploadParams](p)
	if err == nil {
		err = checkID("upload", a.Upload)
	}
	if err != nil {
		return nil, nil, "", err
	}
	sum, err := hex.DecodeString(a.Sum)
	if err != nil || len(sum) != 32 {
		return nil, nil, "", invalid("that isn't a file's checksum")
	}
	u := c.h.takeUpload(c.dev.ID, a.Upload)
	if u == nil {
		return nil, nil, "", invalid("that upload has ended")
	}
	defer u.mu.Unlock()
	c.h.dropUpload(uploadKey(c.dev.ID, a.Upload), u, false)
	ref, err := u.f.Finish(sum) // which removes a file that doesn't check out
	if err != nil {
		return nil, nil, u.name, err
	}
	return imageResult{Ref: ref}, nil, u.name, nil
}

func uploadCancel(_ context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
	a, err := decode[uploadParams](p)
	if err == nil {
		err = checkID("upload", a.Upload)
	}
	if err != nil {
		return nil, nil, "", err
	}
	if u := c.h.takeUpload(c.dev.ID, a.Upload); u != nil {
		c.h.dropUpload(uploadKey(c.dev.ID, a.Upload), u, true)
		u.mu.Unlock()
	}
	return nil, nil, "", nil
}
