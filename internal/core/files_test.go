package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":                 "report.pdf",
		"Q3 incident export.csv":     "Q3 incident export.csv",
		"../../etc/passwd":           "passwd",
		`C:\Users\me\Desktop\a.txt`:  "a.txt",
		`..\..\boot.ini`:             "boot.ini",
		"..":                         "file",
		".":                          "file",
		"":                           "file",
		"/":                          "file",
		".bashrc":                    "bashrc",
		"name. . ":                   "name",
		`a<b>:"c"|d?e*.txt`:          "a_b___c__d_e_.txt",
		"tab\there\nnew\x00line.log": "tabherenewline.log",
		"invoice\u202Efdp.exe":       "invoicefdp.exe", // a right-to-left override can't disguise the extension
		"caf\xffe.txt":               "caf_e.txt",
		"résumé.docx":                "résumé.docx",
	} {
		if got := SafeFileName(in); got != want {
			t.Errorf("SafeFileName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 200) + ".tar.gz"
	got := SafeFileName(long)
	if len(got) > maxNameBytes || !strings.HasSuffix(got, ".gz") || !strings.HasPrefix(got, "éé") || !utf8Valid(got) {
		t.Errorf("a long name became %q (%d bytes)", got, len(got))
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "\uFFFD") == s }

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"/data/attachments/0b8e5a3c-1f2d-4e5f-8a9b-0c1d2e3f4a5b-report.pdf":   "report.pdf",
		`C:\solo\attachments\0b8e5a3c-1f2d-4e5f-8a9b-0c1d2e3f4a5b-a b.txt`:    "a b.txt",
		"0b8e5a3c-1f2d-4e5f-8a9b-0c1d2e3f4a5b-x":                              "x",
		"/data/attachments/0b8e5a3c-1f2d-4e5f-8a9b-0c1d2e3f4a5b.png":          "",
		"/data/attachments/0b8e5a3c-1f2d-4e5f-8a9b-0c1d2e3f4a5b-":             "",
		"/data/attachments/0B8E5A3C-1F2D-4E5F-8A9B-0C1D2E3F4A5B-report.pdf":   "",
		"/data/attachments/0b8e5a3cx1f2d-4e5f-8a9b-0c1d2e3f4a5b-report.pdf":   "",
		"/data/attachments/report.pdf":                                        "",
		"/data/attachments/0b8e5a3c-1f2d-4e5f-8a9b-0c1d2e3f4a5b-sub/inner.md": "",
	} {
		got, ok := FileName(in)
		if got != want || ok != (want != "") {
			t.Errorf("FileName(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if got := FormatSize(2_400_000); got != "2.3 MB" {
		t.Errorf("FormatSize = %q", got)
	}
}

// A file is copied into the attachments folder under its own name, never executable; a folder, a file
// over the limit and one that is already an attachment are handled.
func TestImportFile(t *testing.T) {
	e := newEnv(t)
	src := filepath.Join(e.root, "build log.txt")
	if err := os.WriteFile(src, []byte("FAIL TestThing"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := e.c.ImportFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != filepath.Join(e.dataDir, "attachments") || !IsFileAttachment(p) {
		t.Fatalf("attachment %s", p)
	}
	if name, _ := FileName(p); name != "build log.txt" {
		t.Errorf("named %q", name)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	// The original moves on; the attachment doesn't.
	os.WriteFile(src, []byte("changed"), 0o644)
	if data, _ := os.ReadFile(p); string(data) != "FAIL TestThing" {
		t.Errorf("attachment holds %q", data)
	}
	if again, err := e.c.ImportFile(p); err != nil || again != p {
		t.Errorf("importing an attachment gave %q, %v", again, err)
	}
	if p2, err := e.c.ImportFile(src); err != nil || p2 == p {
		t.Errorf("a second copy of the same name: %q, %v", p2, err)
	}
	if _, err := e.c.ImportFile(e.repo); err == nil {
		t.Error("imported a folder")
	}
	huge := filepath.Join(e.root, "huge.bin")
	f, _ := os.Create(huge)
	f.Truncate(MaxFileSize + 1)
	f.Close()
	if _, err := e.c.ImportFile(huge); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Errorf("imported a file over the limit: %v", err)
	}
	// An image passed as a file stays a file; one attached as a file becomes an image when asked.
	png := filepath.Join(e.root, "shot.png")
	writePNG(t, png, 20, 10)
	fp, err := e.c.ImportFile(png)
	if err != nil || !IsFileAttachment(fp) {
		t.Fatalf("an image as a file: %q, %v", fp, err)
	}
	if ip, err := e.c.ImportImage(fp); err != nil || IsFileAttachment(ip) || ip == fp {
		t.Errorf("a file attachment as an image: %q, %v", ip, err)
	}
}

// A file sent in parts becomes an attachment only once it has all arrived and its checksum matches;
// otherwise nothing of it is left.
func TestFileUpload(t *testing.T) {
	e := newEnv(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), 70_000) // about 1.1 MB
	sum := sha256.Sum256(data)
	uploads := filepath.Join(e.dataDir, "uploads")
	leftovers := func() []string {
		names, _ := filepath.Glob(filepath.Join(uploads, "*"))
		return names
	}

	u, err := e.c.BeginFile("../../evil/archive.tar.gz", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(data); off += 300_000 {
		if _, err := u.Write(data[off:min(off+300_000, len(data))]); err != nil {
			t.Fatal(err)
		}
	}
	p, err := u.Finish(sum[:])
	if err != nil {
		t.Fatal(err)
	}
	if name, _ := FileName(p); name != "archive.tar.gz" || filepath.Dir(p) != filepath.Join(e.dataDir, "attachments") {
		t.Fatalf("stored as %s", p)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, data) {
		t.Fatal("the stored file differs")
	}
	if l := leftovers(); len(l) != 0 {
		t.Errorf("left in uploads: %v", l)
	}
	if _, err := u.Write([]byte("x")); err == nil {
		t.Error("wrote to a finished upload")
	}

	// A wrong checksum, a short file and a long one are refused and removed.
	bad := sha256.Sum256([]byte("something else"))
	for _, tc := range []struct {
		name  string
		write []byte
		sum   []byte
	}{
		{"checksum", data, bad[:]},
		{"short", data[:1000], sum[:]},
	} {
		u, err := e.c.BeginFile("x.bin", int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		u.Write(tc.write)
		if p, err := u.Finish(tc.sum); err == nil {
			t.Errorf("%s: stored %s", tc.name, p)
		}
		if l := leftovers(); len(l) != 0 {
			t.Errorf("%s: left in uploads: %v", tc.name, l)
		}
	}
	u, _ = e.c.BeginFile("x.bin", 10)
	if _, err := u.Write(make([]byte, 11)); err == nil {
		t.Error("wrote more than the file said")
	}
	if l := leftovers(); len(l) != 0 {
		t.Errorf("a long file left: %v", l)
	}
	u, _ = e.c.BeginFile("x.bin", 10)
	u.Write(make([]byte, 5))
	u.Abort()
	if l := leftovers(); len(l) != 0 {
		t.Errorf("an abandoned upload left: %v", l)
	}
	if _, err := e.c.BeginFile("x.bin", MaxFileSize+1); err == nil {
		t.Error("began a file over the limit")
	}

	// What an upload left when Solo stopped is removed when it starts.
	u, _ = e.c.BeginFile("x.bin", 10)
	u.Write(make([]byte, 5))
	e.c.Close()
	e.open(t)
	if l := leftovers(); len(l) != 0 {
		t.Errorf("after a restart, left in uploads: %v", l)
	}
}

// A message's files reach the agent as their paths and names after its text, and the transcript keeps
// them beside the text; Solo is told how to pass them on. Only file attachments are taken, and an image
// read never reads one.
func TestSendFiles(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := filepath.Join(e.root, "spec.md")
	os.WriteFile(src, []byte("# Spec"), 0o644)
	p, err := e.c.ImportFile(src)
	if err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(e.root, "small.png")
	writePNG(t, img, 40, 20)

	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Read", Prompt: "read the spec", Images: []string{img}, Files: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if !strings.Contains(v.Summary, "echo: read the spec\n\n[Attached files, copied to this machine. Read them with your tools:\n- spec.md (6 B): ") ||
		!strings.Contains(v.Summary, "+image/png") || strings.Contains(v.Summary, "spawn_thread") {
		t.Fatalf("the agent got %q", v.Summary)
	}
	items, _ := e.c.Items(ctx, v.ID, 0)
	var u UserData
	items[1].Decode(&u)
	if u.Text != "read the spec" || len(u.Images) != 1 || len(u.Files) != 1 || u.Files[0].Name != "spec.md" || u.Files[0].Size != 6 {
		t.Fatalf("user item %+v", u)
	}
	prompt := u.Files[0].Path

	if err := e.c.Send(ctx, v.ID, "", p); err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if !strings.Contains(v.Summary, "- spec.md (6 B): "+p) {
		t.Fatalf("the agent got %q", v.Summary)
	}

	solo, err := e.c.EnsureOrchestrator()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.Send(ctx, solo.ID, "pass it on", p); err != nil {
		t.Fatal(err)
	}
	solo = waitStatus(t, e.c, solo.ID, StatusIdle)
	if !strings.Contains(solo.Summary, "pass these paths in files when you call spawn_thread or message_thread") {
		t.Fatalf("Solo got %q", solo.Summary)
	}

	// A path shaped like a file attachment's but elsewhere, or one that is gone, isn't sent; a file
	// attachment isn't read as an image.
	outside := filepath.Join(e.root, filepath.Base(p))
	os.WriteFile(outside, []byte("secret"), 0o644)
	gone := filepath.Join(filepath.Dir(p), "0b8e5a3c-1f2d-4e5f-8a9b-0c1d2e3f4a5b-gone.txt")
	for _, bad := range []string{outside, gone, filepath.Join(filepath.Dir(p), "..", filepath.Base(p))} {
		if err := e.c.Send(ctx, v.ID, "look", bad); err == nil {
			t.Errorf("sent %s", bad)
		}
	}
	if link := filepath.Join(filepath.Dir(p), "0b8e5a3c-1f2d-4e5f-8a9b-0c1d2e3f4a5c-link.txt"); os.Symlink(outside, link) == nil {
		if err := e.c.Send(ctx, v.ID, "look", link); err == nil {
			t.Error("sent a link out of the attachments")
		}
	}
	if _, err := e.c.ReadImage(ctx, p, 0); err == nil {
		t.Error("read a file attachment as an image")
	}

	// Discarding the thread removes its files, but not one Solo's conversation still names.
	if err := e.c.Discard(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	waitGone := func(path string, gone bool) {
		t.Helper()
		_, err := os.Stat(path)
		if os.IsNotExist(err) != gone {
			t.Errorf("%s: exists %v, want %v", filepath.Base(path), err == nil, !gone)
		}
	}
	waitGone(prompt, true)
	waitGone(p, false)
}
