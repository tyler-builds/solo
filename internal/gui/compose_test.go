package gui

import (
	"image"
	"image/color"
	"slices"
	"testing"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/dialog"
	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"
)

func solidImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	img.Set(0, 0, color.RGBA{R: 0x40, A: 0xff})
	return img
}

// waitAttached waits for the composer to show n attached images.
func waitAttached(u *uitest.UI, n int) {
	u.WaitFor("the image to attach", func() bool {
		count := 0
		u.Tree().Walk(func(nd *access.Node) bool {
			if nd.Role == access.RoleButton && nd.Name == "Remove image" {
				count++
			}
			return true
		})
		return count == n
	})
}

// focusedName describes what has keyboard focus, for a failure message.
func focusedName(u *uitest.UI) string {
	tr := u.Tree()
	if n := tr.Node(tr.Focus); tr.Focus != 0 && n != nil {
		return n.Role.String() + " " + n.Name
	}
	return "nothing"
}

// sentWith waits for text to be sent and returns the images sent with it.
func sentWith(t *testing.T, u *uitest.UI, f *fakeBackend, text string) []string {
	t.Helper()
	var imgs []string
	u.WaitFor(text+" to be sent", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		i := slices.Index(f.sent, text)
		if i >= 0 {
			imgs = f.sentImgs[i]
		}
		return i >= 0
	})
	return imgs
}

// Pasting an image leaves the caret in the composer, so Enter sends the message with the image.
func TestPastedImageKeepsComposerFocus(t *testing.T) {
	for _, tc := range []struct{ name, key, thread string }{
		{"Solo", "orch-draft-area", ""},
		{"thread", "draft-a1f3c9d2-oauth-area", "a1f3c9d2-oauth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := demoBackend()
			a, u := newUI(t, f)
			if tc.thread != "" {
				a.openThread(tc.thread, "conversation")
				u.Frame()
			}
			area := u.Get(tc.key)
			area.Type("the screenshot")
			u.Press("home") // the caret stays here through the paste
			u.SetClipboardImage(solidImage())
			u.Press("ctrl+v")
			waitAttached(u, 1)
			if !u.Get(tc.key).Focused() {
				t.Fatalf("after the paste, focus is on %s, not the composer", focusedName(u))
			}
			u.Type("See ")
			u.Press("enter")
			if imgs := sentWith(t, u, f, "See the screenshot"); len(imgs) != 1 {
				t.Fatalf("sent with %d images, want 1", len(imgs))
			}
		})
	}
}

// filesWindow answers the file picker with fixed files.
type filesWindow struct {
	ui.NopWindow
	files []string
}

func (w *filesWindow) Focused() bool { return true }
func (w *filesWindow) OpenFiles(_ dialog.FileOptions, done func(dialog.FileResult)) {
	done(dialog.FileResult{Paths: w.files})
}

// Images attached with the button or by dropping them leave the caret in the composer.
func TestAttachedImagesFocusComposer(t *testing.T) {
	shot := sampleScreenshot(t.TempDir(), color.RGBA{R: 0x40, G: 0x80, B: 0xff, A: 0xff})

	t.Run("button", func(t *testing.T) {
		f := demoBackend()
		_, u := newUI(t, f)
		u.Runtime().SetWindow(&filesWindow{files: []string{shot}})
		u.Get("orch-draft-area").Type("From the picker")
		u.ByRole(access.RoleButton, "Attach images").Click()
		waitAttached(u, 1)
		if !u.Get("orch-draft-area").Focused() {
			t.Fatalf("after attaching, focus is on %s, not the composer", focusedName(u))
		}
		u.Press("enter")
		if imgs := sentWith(t, u, f, "From the picker"); !slices.Equal(imgs, []string{shot}) {
			t.Fatalf("sent with %v", imgs)
		}
	})

	t.Run("drop", func(t *testing.T) {
		f := demoBackend()
		_, u := newUI(t, f)
		area := u.Get("orch-draft-area")
		area.Type("Dropped in")
		u.ByRole(access.RoleButton, "Attach images").Focus() // focus elsewhere: the drop brings it back
		b := area.Bounds()
		u.Runtime().DropFiles(b.X+b.W/2, b.Y+b.H/2, []string{shot})
		waitAttached(u, 1)
		if !u.Get("orch-draft-area").Focused() {
			t.Fatalf("after the drop, focus is on %s, not the composer", focusedName(u))
		}
		u.Press("enter")
		if imgs := sentWith(t, u, f, "Dropped in"); !slices.Equal(imgs, []string{shot}) {
			t.Fatalf("sent with %v", imgs)
		}
	})

	t.Run("remove", func(t *testing.T) {
		f := demoBackend()
		_, u := newUI(t, f)
		u.Runtime().SetWindow(&filesWindow{files: []string{shot}})
		u.Get("orch-draft-area").Type("Never mind the picture")
		u.ByRole(access.RoleButton, "Attach images").Click()
		waitAttached(u, 1)
		u.ByRole(access.RoleButton, "Remove image").Click()
		waitAttached(u, 0)
		u.Frame()
		if !u.Get("orch-draft-area").Focused() {
			t.Fatalf("after removing the image, focus is on %s, not the composer", focusedName(u))
		}
		u.Press("enter")
		if imgs := sentWith(t, u, f, "Never mind the picture"); len(imgs) != 0 {
			t.Fatalf("sent with %v", imgs)
		}
	})
}

// With RemoteImages, an attachment is fetched off the UI thread: its box shows a placeholder, then the
// image, and it asks for a preview rather than the image at full size.
func TestRemoteImagesLoadAsync(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	a.RemoteImages = true
	shot := sampleScreenshot(t.TempDir(), color.RGBA{R: 0x30, G: 0x60, B: 0xc0, A: 0xff})
	a.orchImages.Set([]string{shot})
	u.Frame()
	u.WaitFor("the image to show", func() bool {
		found := false
		u.Tree().Walk(func(nd *access.Node) bool {
			found = found || (nd.Role == access.RoleButton && nd.Name == "View image")
			return true
		})
		return found
	})
	if _, ok := a.imgs["preview\x00"+shot]; !ok {
		t.Errorf("no preview was kept: %v", len(a.imgs))
	}
	if _, ok := a.imgs[shot]; ok {
		t.Error("the full image was fetched for a thumbnail")
	}
}
