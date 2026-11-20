package gui

import (
	"bytes"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/dialog"
	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
)

// pageWindow answers the file picker as a page's does: files with their contents and no paths. It keeps
// the options the picker was opened with.
type pageWindow struct {
	ui.NopWindow
	files []dialog.File
	opts  []dialog.FileOptions
}

func (w *pageWindow) Focused() bool { return true }
func (w *pageWindow) OpenFiles(o dialog.FileOptions, done func(dialog.FileResult)) {
	w.opts = append(w.opts, o)
	done(dialog.FileResult{Files: w.files})
}

// hasNode reports whether the UI shows a node of role whose name starts with name.
func hasNode(u *uitest.UI, role access.Role, name string) bool {
	found := false
	u.Tree().Walk(func(nd *access.Node) bool {
		found = found || nd.Role == role && strings.HasPrefix(nd.Name, name)
		return !found
	})
	return found
}

// waitNode waits for the UI to show, or with gone to stop showing, a node of role whose name starts with
// name.
func waitNode(u *uitest.UI, role access.Role, name string, gone bool) {
	u.WaitFor(name, func() bool { return hasNode(u, role, name) != gone })
}

// The attach button is a paperclip that takes any file. On the desktop a file is copied as a file
// attachment and shows as a chip with its name and size; an image is still an image.
func TestAttachAnyFile(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "release notes.pdf")
	if err := os.WriteFile(doc, bytes.Repeat([]byte("%PDF"), 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	shot := sampleScreenshot(dir, color.RGBA{R: 0x40, G: 0x80, B: 0xff, A: 0xff})
	for _, tc := range []struct{ name, area, attach, thread string }{
		{"Solo", "orch-draft-area", "orch-draft-attach", ""},
		{"thread", "draft-a1f3c9d2-oauth-area", "draft-a1f3c9d2-oauth-attach", "a1f3c9d2-oauth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := demoBackend()
			a, u := newUI(t, f)
			if tc.thread != "" {
				a.openThread(tc.thread, "conversation")
				u.WaitFor("the panel to slide in", func() bool { return a.panelAnim.Peek() == 1 })
				u.Frame()
			}
			if hasNode(u, access.RoleButton, "Attach images") {
				t.Fatal("the composer still offers to attach images only")
			}
			u.Runtime().SetWindow(&filesWindow{files: []string{shot, doc}})
			u.Get(tc.area).Type("Both of these")
			u.Get(tc.attach).Click()
			waitAttached(u, 1)
			waitNode(u, access.RoleGroup, "release notes.pdf", false)
			if !hasNode(u, access.RoleButton, "Remove file") {
				t.Fatal("the file has no button to remove it")
			}
			if !u.Get(tc.area).Focused() {
				t.Fatalf("after attaching, focus is on %s, not the composer", focusedName(u))
			}
			u.Press("enter")
			refs := sentWith(t, u, f, "Both of these")
			if len(refs) != 2 || refs[0] != shot || !core.IsFileAttachment(refs[1]) {
				t.Fatalf("sent with %v, want the image and a file attachment", refs)
			}
			if name, _ := core.FileName(refs[1]); name != "release notes.pdf" {
				t.Errorf("the file was attached as %q", name)
			}
			if data, err := os.ReadFile(refs[1]); err != nil || len(data) != 20000 {
				t.Errorf("the attachment holds %d bytes (%v), want 20000", len(data), err)
			}
		})
	}
}

// In a page, a file is sent to the machine in parts: its chip shows how much has arrived, and Send waits
// for it, so a message never names a file half sent.
func TestUploadHoldsSend(t *testing.T) {
	f := demoBackend()
	gate := make(chan struct{})
	f.upload = gate
	_, u := newUI(t, f)
	w := &pageWindow{files: []dialog.File{{Name: "trace.zip", Data: make([]byte, 5<<20)}}}
	u.Runtime().SetWindow(w)
	u.Get("orch-draft-area").Type("Here is the trace")
	u.ByRole(access.RoleButton, "Attach files").Click()
	if len(w.opts) != 1 || len(w.opts[0].Filters) != 0 || !w.opts[0].Multiple {
		t.Fatalf("the picker was opened with %+v, want any files, several at once", w.opts)
	}
	waitNode(u, access.RoleGroup, "trace.zip, uploading", false)
	u.WaitFor("the progress to show", func() bool { return hasNode(u, access.RoleProgressBar, "Uploading trace.zip") })
	u.Press("enter")
	if send := u.ByRole(access.RoleButton, "Send"); !send.Disabled() {
		t.Error("Send is enabled while the file uploads")
	}
	for range 5 {
		u.Frame()
	}
	f.mu.Lock()
	early := slices.Contains(f.sent, "Here is the trace")
	f.mu.Unlock()
	if early {
		t.Fatal("the message was sent before its file arrived")
	}
	if u.Get("orch-draft-area").Node().Value != "Here is the trace" {
		t.Error("the held message lost its text")
	}
	close(gate)
	waitNode(u, access.RoleGroup, "trace.zip, uploading", true)
	waitNode(u, access.RoleGroup, "trace.zip", false)
	u.Press("enter")
	refs := sentWith(t, u, f, "Here is the trace")
	if len(refs) != 1 || !core.IsFileAttachment(refs[0]) {
		t.Fatalf("sent with %v", refs)
	}
}

// Removing a file while it uploads stops the upload, and the message goes without it.
func TestRemoveUploadingFile(t *testing.T) {
	f := demoBackend()
	f.upload = make(chan struct{}) // never opened
	_, u := newUI(t, f)
	u.Runtime().SetWindow(&pageWindow{files: []dialog.File{{Name: "dump.bin", Data: make([]byte, 1<<20)}}})
	u.Get("orch-draft-area").Type("Never mind the dump")
	u.ByRole(access.RoleButton, "Attach files").Click()
	waitNode(u, access.RoleGroup, "dump.bin, uploading", false)
	u.ByRole(access.RoleButton, "Stop and remove file").Click()
	waitNode(u, access.RoleGroup, "dump.bin", true)
	u.WaitFor("the upload to stop", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Equal(f.cancelled, []string{"dump.bin"})
	})
	u.Press("enter")
	if refs := sentWith(t, u, f, "Never mind the dump"); len(refs) != 0 {
		t.Fatalf("sent with %v", refs)
	}
}

// A file that doesn't arrive says why in its chip, offers to try again, and holds Send until it is
// removed or arrives.
func TestFailedUpload(t *testing.T) {
	f := demoBackend()
	f.fileErr = errors.New("The connection to the machine dropped, so the file didn't arrive. Attach it again.")
	_, u := newUI(t, f)
	u.Runtime().SetWindow(&pageWindow{files: []dialog.File{{Name: "notes.txt", Data: []byte("hello")}}})
	u.Get("orch-draft-area").Type("With notes")
	u.ByRole(access.RoleButton, "Attach files").Click()
	waitNode(u, access.RoleGroup, "notes.txt: didn't attach. The connection to the machine dropped", false)
	if send := u.ByRole(access.RoleButton, "Send"); !send.Disabled() {
		t.Error("Send is enabled with a file that didn't attach")
	}
	f.mu.Lock()
	f.fileErr = nil
	f.mu.Unlock()
	u.ByRole(access.RoleButton, "Try again").Click()
	waitNode(u, access.RoleGroup, "notes.txt: didn't attach", true)
	u.WaitFor("the file to arrive", func() bool { return !hasNode(u, access.RoleGroup, "notes.txt, attaching") })
	u.Press("enter")
	if refs := sentWith(t, u, f, "With notes"); len(refs) != 1 || !core.IsFileAttachment(refs[0]) {
		t.Fatalf("sent with %v", refs)
	}
}

// A page's image is still sent as an image, by its contents.
func TestPageImageStaysImage(t *testing.T) {
	f := demoBackend()
	_, u := newUI(t, f)
	data, err := os.ReadFile(sampleScreenshot(t.TempDir(), color.RGBA{R: 0x40, A: 0xff}))
	if err != nil {
		t.Fatal(err)
	}
	u.Runtime().SetWindow(&pageWindow{files: []dialog.File{{Name: "IMG_0042.png", Data: data}}})
	u.Get("orch-draft-area").Type("From my phone")
	u.ByRole(access.RoleButton, "Attach files").Click()
	waitAttached(u, 1)
	u.Press("enter")
	if refs := sentWith(t, u, f, "From my phone"); len(refs) != 1 || core.IsFileAttachment(refs[0]) {
		t.Fatalf("sent with %v, want one image", refs)
	}
}

// A sent message, and one queued for Solo, show their files as chips.
func TestMessageShowsFiles(t *testing.T) {
	f := demoBackend()
	p, err := fileAttachment("budget.xlsx", []byte("cells"))
	if err != nil {
		t.Fatal(err)
	}
	f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "The budget", Files: []core.FileRef{{Path: p, Name: "budget.xlsx", Size: 5}}})
	for i := range f.threads {
		if f.threads[i].ID == core.OrchestratorID {
			f.threads[i].Queued = []core.QueuedMessage{{Text: "And the plan", Files: []core.FileRef{{Path: p, Name: "plan.docx", Size: 2048}}}}
		}
	}
	_, u := newUI(t, f)
	waitNode(u, access.RoleGroup, "budget.xlsx", false)
	waitNode(u, access.RoleGroup, "plan.docx", false)
	if hasNode(u, access.RoleButton, "Remove file") {
		t.Error("a sent file can be removed")
	}
}
