package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tyler-builds/solo/internal/store"
)

func TestMarkdownImages(t *testing.T) {
	text := "Before ![the form](/tmp/a.png) and after ![after \\[1\\]](<shots/b c.png> \"title\").\n" +
		"`![code](x.png)` and ``![two](y.png)``\n" +
		"```\n![fenced](z.png)\n```\n" +
		"![parens](shot(1).png) ![](empty.png) ![no dest]() ![open](a.png\n" +
		"[![linked](l.png)](https://x) \\![escaped](e.png)\n"
	var got []string
	for _, im := range MarkdownImages(text) {
		got = append(got, im.Alt+"|"+im.Dest+"|"+text[im.DestStart:im.DestEnd])
		if text[im.Start:im.Start+2] != "![" || text[im.End-1] != ')' {
			t.Errorf("span %q", text[im.Start:im.End])
		}
	}
	want := []string{
		"the form|/tmp/a.png|/tmp/a.png",
		"after [1]|shots/b c.png|<shots/b c.png>",
		"parens|shot(1).png|shot(1).png",
		"|empty.png|empty.png",
		"linked|l.png|l.png",
	}
	if !slices.Equal(got, want) {
		t.Errorf("images\n got %q\nwant %q", got, want)
	}
	if ImageDest("/data/attachments/a b.png") != "</data/attachments/a b.png>" || ImageDest("/d/x.png") != "/d/x.png" {
		t.Error("ImageDest")
	}
}

// An agent's message shows images from its worktree; they are copied into the attachments, the
// message points at the copies, and they outlive the worktree.
func TestEmbeddedImages(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "UI", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	writePNG(t, filepath.Join(v.WorktreePath, "after.png"), 3000, 1000) // relative to the worktree
	before := filepath.Join(e.root, "before shot.png")
	writePNG(t, before, 40, 20)

	// The fake agent echoes the message, so its reply shows the same images.
	msg := "![before](<" + filepath.ToSlash(before) + ">) ![after](after.png) ![web](https://example.com/x.png) ![gone](missing.png)"
	if err := e.c.Send(ctx, v.ID, msg); err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	reply := lastAssistant(t, e.c, v.ID)
	attDir := filepath.Join(e.dataDir, "attachments")
	if len(reply.Images) != 2 {
		t.Fatalf("images %v in %q", reply.Images, reply.Text)
	}
	for _, p := range reply.Images {
		if filepath.Dir(p) != attDir || !strings.Contains(reply.Text, "("+ImageDest(p)+")") {
			t.Errorf("image %s in %q", p, reply.Text)
		}
	}
	if w, h := pngSize(t, reply.Images[1]); w != 2000 || h != 666 {
		t.Errorf("a large image is scaled to fit Claude's limits: %dx%d", w, h)
	}
	for _, s := range []string{"https://example.com/x.png", "(missing.png)"} {
		if !strings.Contains(reply.Text, s) {
			t.Errorf("%s isn't left as written: %q", s, reply.Text)
		}
	}
	// The summary Solo reads shows the copies too, and the same file isn't copied twice.
	if got := e.c.ImagesIn(v.Summary); !slices.Equal(got, reply.Images) {
		t.Errorf("summary images %v, message %v", got, reply.Images)
	}
	if n := countFiles(t, attDir); n != 2 {
		t.Errorf("%d attachments", n)
	}

	// Solo shows a thread's image again: the attachment is used as it is, and is still there after the
	// thread is discarded, until no conversation shows it.
	o, err := e.c.EnsureOrchestrator()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.Send(ctx, o.ID, "It looks like this: ![after]("+ImageDest(reply.Images[1])+")"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, o.ID, StatusIdle)
	if got := lastAssistant(t, e.c, o.ID).Images; !slices.Equal(got, reply.Images[1:]) {
		t.Fatalf("Solo's message shows %v", got)
	}
	if err := e.c.Discard(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(reply.Images[0]); !os.IsNotExist(err) {
		t.Error("kept an image only the discarded thread showed")
	}
	if _, err := os.Stat(reply.Images[1]); err != nil {
		t.Error("removed an image Solo still shows:", err)
	}
}

// Only real image files of the supported types are taken in, within the limits.
func TestEmbeddedImageLimits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "UI", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	dir := t.TempDir()
	notImage := filepath.Join(dir, "secret.png")
	os.WriteFile(notImage, []byte("password=hunter2"), 0o600)
	huge := filepath.Join(dir, "huge.png")
	os.WriteFile(huge, make([]byte, maxImageFile+1), 0o600)
	srcs := []string{notImage, huge, dir, "/etc/passwd", "//server/share/x.png", "file://server/x.png"}
	if fifo := filepath.Join(dir, "pipe.png"); mkfifo(fifo) == nil {
		srcs = append(srcs, fifo) // reading it would block
	}
	// A link in the attachments folder to a file elsewhere isn't taken as an attachment.
	att, err := e.c.ImportImage(writeTemp(t, dir, "real.png"))
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(att), "link.png")
	if os.Symlink(writeTemp(t, dir, "other.png"), link) == nil {
		srcs = append(srcs, link)
	}
	var msg strings.Builder
	for _, s := range srcs {
		msg.WriteString("![x](<" + filepath.ToSlash(s) + ">) ")
	}
	if err := e.c.Send(ctx, v.ID, msg.String()); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
	if got := lastAssistant(t, e.c, v.ID).Images; len(got) != 0 {
		t.Errorf("took in %v", got)
	}

	// At most maxImages a message.
	ok := writeTemp(t, dir, "ok.png")
	msg.Reset()
	for range maxImages + 3 {
		msg.WriteString("![x](" + filepath.ToSlash(ok) + ") ")
	}
	if err := e.c.Send(ctx, v.ID, msg.String()); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
	reply := lastAssistant(t, e.c, v.ID)
	if n := strings.Count(reply.Text, filepath.ToSlash(ok)); n != 3 {
		t.Errorf("%d images left as written, want 3", n)
	}
}

func writeTemp(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	writePNG(t, p, 8, 8)
	return p
}

func lastAssistant(t *testing.T, c *Core, id string) TextData {
	t.Helper()
	items, err := c.Items(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Kind == ItemAssistant {
			var d TextData
			items[i].Decode(&d)
			return d
		}
	}
	t.Fatal("no assistant message")
	return TextData{}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

// Every kind of thread is asked for screenshots of what it changes on screen, and told how to show them.
func TestWorkerInstructionsAskForScreenshots(t *testing.T) {
	parts := func(ps ...Part) string { b, _ := json.Marshal(ps); return string(b) }
	for name, r := range map[string]store.Thread{
		"one repository": {RepoPath: "/r/web", Branch: "b", BaseBranch: "main", WorktreePath: "/w/web"},
		"several":        {Branch: "b", Parts: parts(Part{Repo: "/r/web", BaseBranch: "main"}, Part{Repo: "/r/api", BaseBranch: "main"})},
		"not in git":     {Parts: parts(Part{Repo: "/r/notes", InPlace: true})},
	} {
		p := strings.Join(strings.Fields(workerInstructions(r, "")), " ")
		for _, want := range []string{"screenshots of each changed view in your summary", "before and after when the view existed",
			"If you can't take them, say why in your summary", "write ![caption](path)", "the app keeps a copy"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s: no %q in\n%s", name, want, p)
			}
		}
		inPlace := strings.Contains(p, "render it before you change anything") && strings.Contains(p, "outside this folder")
		if inPlace != (name == "not in git") || !inPlace && !strings.Contains(p, "render the base commit") {
			t.Errorf("%s: before is taken the wrong way:\n%s", name, p)
		}
	}
}
