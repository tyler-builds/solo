//go:build !js

package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tyler-builds/solo/internal/agent"
)

// uploadsDir holds files being sent from another device (BeginFile) until they have arrived whole. It is
// beside the attachments folder, never in it, so a file half written is never an attachment.
func (c *Core) uploadsDir() string { return filepath.Join(c.cfg.DataDir, "uploads") }

// sweepUploads removes what uploads left when Solo last stopped.
func (c *Core) sweepUploads() {
	if err := os.RemoveAll(c.uploadsDir()); err != nil {
		c.log.Warn("core: removing unfinished uploads", "err", err)
	}
}

// isFileAttachment reports whether path is a file attachment (see FileName) in the attachments folder.
func (c *Core) isFileAttachment(path string) bool {
	return c.isAttachment(path) && IsFileAttachment(path)
}

// newFileAttachment claims a new attachment's path for a file named name: the name is made safe, and the
// file is created empty, by this call alone, so nothing else's is overwritten. It is the user's to read
// and write, and never executable.
func (c *Core) newFileAttachment(name string) (string, *os.File, error) {
	dir := c.attachmentsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	p := filepath.Join(dir, agent.NewUUID()+"-"+SafeFileName(name))
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", nil, err
	}
	return p, f, nil
}

// ImportFile stores a copy of a file on the machine as an attachment and returns its path. A file
// attachment is used as it is. An image file is copied as a file: it reaches Claude as a path to read,
// not as an image (for that, see ImportImage).
func (c *Core) ImportFile(path string) (string, error) {
	path = filepath.Clean(path)
	if c.isFileAttachment(path) {
		if _, err := os.Stat(path); err != nil {
			return "", err
		}
		return path, nil
	}
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return "", err
	}
	name := filepath.Base(path)
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s isn't a file", name)
	}
	if fi.Size() > MaxFileSize {
		return "", fmt.Errorf("%s is larger than %d MB", name, MaxFileSize>>20)
	}
	p, f, err := c.newFileAttachment(name)
	if err != nil {
		return "", err
	}
	// One byte past the limit says the file grew while it was copied.
	n, err := io.Copy(f, io.LimitReader(src, MaxFileSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > MaxFileSize {
		err = fmt.Errorf("%s is larger than %d MB", name, MaxFileSize>>20)
	}
	if err != nil {
		os.Remove(p)
		return "", err
	}
	return p, nil
}

// ImportFiles imports each path (see ImportFile).
func (c *Core) ImportFiles(paths []string) ([]string, error) {
	if len(paths) > MaxFiles {
		return nil, fmt.Errorf("at most %d files per message", MaxFiles)
	}
	var out []string
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		a, err := c.ImportFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// SaveFile stores a file's contents, such as a file picked in a page, as an attachment named name, and
// returns its path. progress, when not nil, is told how many bytes are stored.
func (c *Core) SaveFile(_ context.Context, name string, data []byte, progress func(int64)) (string, error) {
	u, err := c.BeginFile(name, int64(len(data)))
	if err != nil {
		return "", err
	}
	if _, err := u.Write(data); err != nil {
		u.Abort()
		return "", err
	}
	sum := sha256.Sum256(data)
	p, err := u.Finish(sum[:])
	if err == nil && progress != nil {
		progress(int64(len(data)))
	}
	return p, err
}

// FileUpload is a file arriving in parts, as a device sends one (see BeginFile).
type FileUpload struct {
	c    *Core
	name string // as the device gave it

	mu   sync.Mutex
	f    *os.File
	path string // in the uploads folder
	size int64  // the size the device said
	n    int64  // the bytes written
	sum  hash.Hash
	done bool
}

// BeginFile starts storing a file of size bytes, named name, that arrives in parts: Write them in order,
// then Finish, which checks it and makes it an attachment, or Abort. Until then it is in the uploads
// folder, which nothing reads and which is emptied when Solo starts.
func (c *Core) BeginFile(name string, size int64) (*FileUpload, error) {
	if size < 0 || size > MaxFileSize {
		return nil, fmt.Errorf("a file is at most %d MB", MaxFileSize>>20)
	}
	dir := c.uploadsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, agent.NewUUID()+".part")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &FileUpload{c: c, name: name, f: f, path: p, size: size, sum: sha256.New()}, nil
}

// errUploadEnded is the error of a call on an upload that was finished or abandoned.
var errUploadEnded = errors.New("the upload has ended")

// Write appends a part. More than the size the upload began with is refused, and ends it.
func (u *FileUpload) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return 0, errUploadEnded
	}
	if int64(len(p)) > u.size-u.n {
		u.abortLocked()
		return 0, errors.New("the file is larger than it said")
	}
	n, err := u.f.Write(p)
	u.n += int64(n)
	u.sum.Write(p[:n])
	if err != nil {
		u.abortLocked()
	}
	return n, err
}

// Written is how many bytes have been written.
func (u *FileUpload) Written() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

// Finish checks that the whole file arrived, its size and its SHA-256 sum, and stores it as an
// attachment, whose path it returns. A file that doesn't check out is removed.
func (u *FileUpload) Finish(sum []byte) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return "", errUploadEnded
	}
	if u.n != u.size {
		u.abortLocked()
		return "", fmt.Errorf("only %s of %s arrived", FormatSize(u.n), FormatSize(u.size))
	}
	if !bytes.Equal(u.sum.Sum(nil), sum) {
		u.abortLocked()
		return "", errors.New("the file arrived changed: its checksum doesn't match")
	}
	u.done = true
	err := u.f.Close()
	if err != nil {
		os.Remove(u.path)
		return "", err
	}
	// The attachment's name is claimed first, so the rename can't replace another file.
	p, f, err := u.c.newFileAttachment(u.name)
	if err != nil {
		os.Remove(u.path)
		return "", err
	}
	f.Close()
	if err := os.Rename(u.path, p); err != nil {
		os.Remove(u.path)
		os.Remove(p)
		return "", err
	}
	return p, nil
}

// Abort stops the upload and removes what arrived. It does nothing to one that has ended.
func (u *FileUpload) Abort() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.abortLocked()
}

func (u *FileUpload) abortLocked() {
	if u.done {
		return
	}
	u.done = true
	u.f.Close()
	os.Remove(u.path)
}

// splitAttachments sorts a message's attachments into its images and its files. A file must be a file
// attachment that is there (isFileAttachment); an image is checked when it is loaded (loadImages).
func (c *Core) splitAttachments(refs []string) (images []string, files []FileRef, err error) {
	for _, p := range refs {
		if !IsFileAttachment(p) {
			images = append(images, p)
			continue
		}
		f, err := c.statFile(p)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, f)
	}
	if len(files) > MaxFiles {
		return nil, nil, fmt.Errorf("at most %d files per message", MaxFiles)
	}
	return images, files, nil
}

// statFile describes a file attachment, and fails for a path that isn't one or a file that is gone.
func (c *Core) statFile(p string) (FileRef, error) {
	name, _ := FileName(p)
	if !c.isFileAttachment(p) {
		return FileRef{}, fmt.Errorf("file %s isn't an attachment", name)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return FileRef{}, fmt.Errorf("the attached file %s is gone", name)
	}
	if !fi.Mode().IsRegular() {
		return FileRef{}, fmt.Errorf("file %s isn't an attachment", name)
	}
	return FileRef{Path: p, Name: name, Size: fi.Size()}, nil
}

// checkFiles checks that a message's files are still there, as when a queued message is sent.
func (c *Core) checkFiles(files []FileRef) error {
	for _, f := range files {
		if _, err := c.statFile(f.Path); err != nil {
			return err
		}
	}
	return nil
}

// filesNote tells the agent where a message's files are, after the message's text: the transcript shows
// them as chips, and Claude has only paths to go by. Solo is told how to pass them on to a thread.
func filesNote(files []FileRef, solo bool) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Attached files, copied to this machine. Read them with your tools:")
	for _, f := range files {
		fmt.Fprintf(&b, "\n- %s (%s): %s", f.Name, FormatSize(f.Size), f.Path)
	}
	if solo {
		b.WriteString("\nTo give them to a thread, pass these paths in files when you call spawn_thread or message_thread.")
	}
	b.WriteString("]")
	return b.String()
}
