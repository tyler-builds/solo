package gui

import (
	"image/color"
	"math"
	"slices"
	"testing"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// The lightbox steps through the images it opened among, stopping at either end, and fits each image
// it lands on.
func TestViewerSteps(t *testing.T) {
	all := []string{"/a.png", "/b.png", "/a.png", "/c.png"}
	v := openViewer("/b.png", all)
	if !slices.Equal(v.paths, []string{"/a.png", "/b.png", "/c.png"}) || v.at != 1 {
		t.Fatalf("opened on %d of %v", v.at, v.paths)
	}
	var got []string
	for _, d := range []int{-1, -1, 1, 1, 1, 1} {
		v = v.step(d)
		got = append(got, v.path())
	}
	if want := []string{"/a.png", "/a.png", "/b.png", "/c.png", "/c.png", "/c.png"}; !slices.Equal(got, want) {
		t.Errorf("stepped through %v, want %v", got, want)
	}
	if v.hasNext() || !v.hasPrev() {
		t.Error("at the last image, Next is offered or Previous isn't")
	}
	v.scale, v.x, v.y = 2, -10, -10
	if v = v.step(-1); v.scale != 0 || v.x != 0 || v.y != 0 || v.path() != "/b.png" {
		t.Errorf("stepping kept the zoom: %+v", v)
	}
	if v := openViewer("/elsewhere.png", all); !slices.Equal(v.paths, []string{"/elsewhere.png"}) || v.at != 0 {
		t.Errorf("an image outside the gallery opened as %d of %v", v.at, v.paths)
	}
	if v := openViewer("/a.png", nil); v.hasPrev() || v.hasNext() || v.step(1).path() != "/a.png" {
		t.Error("a single image steps")
	}
	if (viewer{}).open() || (viewer{}).path() != "" {
		t.Error("the zero viewer is open")
	}
}

func nearly(a, b float32) bool { return math.Abs(float64(a-b)) < 0.01 }

// Zoom stays between fitting the view and maxZoom, keeps the point it zooms around still, keeps the
// image over the view as it pans, and resets to fit.
func TestViewerZoom(t *testing.T) {
	f := frame{iw: 2000, ih: 1000, vw: 1000, vh: 500} // fits at half size
	var v viewer
	if v.zoom(f) != 0.5 || !v.fitted() {
		t.Fatalf("fitted zoom %v", v.zoom(f))
	}
	// Zooming out from fit does nothing.
	if z := v.zoomBy(f, 1/zoomStep, 500, 250); !z.fitted() {
		t.Errorf("zoomed out past fit: %+v", z)
	}
	// Zooming in around a point keeps the image's point under it there.
	px, py := float32(200), float32(100)
	ox, oy := v.origin(f)
	u, w := (px-ox)/v.zoom(f), (py-oy)/v.zoom(f)
	z := v.zoomBy(f, 2, px, py)
	if !nearly(z.zoom(f), 1) {
		t.Fatalf("zoomed to %v, want 1", z.zoom(f))
	}
	if zx, zy := z.origin(f); !nearly(zx+u*z.zoom(f), px) || !nearly(zy+w*z.zoom(f), py) {
		t.Errorf("the point under the pointer moved: origin %v,%v", zx, zy)
	}
	// It goes no further than maxZoom.
	for range 50 {
		z = z.zoomBy(f, zoomStep, px, py)
	}
	if z.zoom(f) != maxZoom {
		t.Errorf("zoomed in to %v, past %v", z.zoom(f), float32(maxZoom))
	}
	// Panning keeps the image over the view: no gap opens at an edge.
	z = v.zoomTo(f, 1, 0, 0).panned(f, 5000, 5000)
	if z.x != 0 || z.y != 0 {
		t.Errorf("panned past the top-left corner: %v,%v", z.x, z.y)
	}
	z = z.panned(f, -5000, -5000)
	if z.x != f.vw-f.iw || z.y != f.vh-f.ih {
		t.Errorf("panned past the bottom-right corner: %v,%v", z.x, z.y)
	}
	if p := v.panned(f, 10, 10); !p.fitted() || p.x != 0 || p.y != 0 {
		t.Error("a fitted image panned")
	}
	// Zooming all the way out, or reset, fits again.
	if r := z.zoomTo(f, 0, 0, 0); !r.fitted() || r.x != 0 || r.y != 0 {
		t.Errorf("reset to %+v", r)
	}
	if r := z.zoomBy(f, 1e-6, 0, 0); !r.fitted() {
		t.Errorf("zoomed out to %+v", r)
	}
	// A double-click goes to the image's own size and back; an image that fits at its own size
	// doubles.
	if d := v.toggled(f, 500, 250); d.zoom(f) != 1 {
		t.Errorf("double-click zoomed to %v", d.zoom(f))
	} else if !d.toggled(f, 500, 250).fitted() {
		t.Error("a second double-click didn't fit")
	}
	small := frame{iw: 200, ih: 100, vw: 1000, vh: 500}
	if d := v.toggled(small, 500, 250); d.zoom(small) != 2 {
		t.Errorf("double-click on a small image zoomed to %v", d.zoom(small))
	}
	// Along a side where the zoomed image is smaller than the view, it is centered.
	if c := v.zoomTo(small, 2, 0, 0); c.x != 300 || c.y != 150 {
		t.Errorf("a small zoomed image is at %v,%v", c.x, c.y)
	}
}

// A conversation's gallery is its images in the order they show: the user's attachments, an agent's
// images in its text's order, then the queue's.
func TestConversationImages(t *testing.T) {
	f := demoBackend()
	o := core.OrchestratorID
	f.items[o] = nil
	f.add(o, core.ItemUser, core.UserData{Text: "Here", Images: []string{"/u1.png", "/u2.png"}})
	f.add(o, core.ItemUser, core.UserData{Synthetic: true, Text: "an update", Images: []string{"/hidden.png"}})
	f.add(o, core.ItemAssistant, core.TextData{Text: "![After](/a2.png) and ![Before](/a1.png) [link](/a3.png)",
		Images: []string{"/a1.png", "/a2.png", "/a3.png"}})
	f.add(o, core.ItemAssistant, core.TextData{Text: "Again ![u1](/u1.png)", Images: []string{"/u1.png"}})
	a, _ := newUI(t, f)
	got := a.conversationImages(o)
	if want := []string{"/u1.png", "/u2.png", "/a2.png", "/a1.png", "/a3.png"}; !slices.Equal(got, want) {
		t.Errorf("gallery %v, want %v", got, want)
	}
}

// Opened from a message, the lightbox steps through the conversation's images with its arrows and the
// arrow keys, zooms with the wheel, its buttons and keys, copies the image, and closes on Escape but not
// on a click on the image.
func TestLightbox(t *testing.T) {
	f := demoBackend()
	dir := t.TempDir()
	user := sampleScreenshot(dir, color.RGBA{200, 40, 40, 255})
	before := sampleScreenshot(dir, color.RGBA{47, 214, 193, 255})
	after := sampleScreenshot(dir, color.RGBA{90, 120, 230, 255})
	o := core.OrchestratorID
	f.add(o, core.ItemUser, core.UserData{Text: "This field overlaps", Images: []string{user}})
	f.add(o, core.ItemAssistant, core.TextData{
		Text:   "Fixed:\n\n![Before](" + core.ImageDest(before) + ") ![After](" + core.ImageDest(after) + ")",
		Images: []string{before, after},
	})
	a, u := newUI(t, f)
	u.WaitFor("the images to load", func() bool { return u.HasRole(access.RoleButton, "View After") })
	u.Settle()
	u.ByRole(access.RoleButton, "View After").Click()
	u.Settle()
	if v := a.lightbox.Get(); !slices.Equal(v.paths, []string{user, before, after}) || v.at != 2 {
		t.Fatalf("opened on %d of %v", v.at, v.paths)
	}
	if got := u.Get("lightbox-position").Text(); got != "3 of 3" {
		t.Errorf("position %q", got)
	}
	if !u.Get("lightbox-next").Disabled() || u.Get("lightbox-prev").Disabled() {
		t.Error("at the last image, Next is enabled or Previous isn't")
	}
	u.Get("lightbox-prev").Click()
	if a.lightbox.Get().path() != before {
		t.Errorf("Previous went to %q", a.lightbox.Get().path())
	}
	u.Press("left")
	u.Press("left")
	if a.lightbox.Get().path() != user {
		t.Errorf("Left went to %q", a.lightbox.Get().path())
	}
	u.Press("right")
	if a.lightbox.Get().path() != before {
		t.Errorf("Right went to %q", a.lightbox.Get().path())
	}

	// The image is 640×400, which fits the window at its own size.
	view := u.Get("lightbox-view")
	view.Scroll(0, -2*wheelNotch)
	if s := a.lightbox.Get().scale; !nearly(s, zoomStep*zoomStep) {
		t.Errorf("two notches of the wheel zoomed to %v", s)
	}
	// A drag pans the zoomed image, and a click on it leaves the lightbox open.
	b := view.Bounds()
	cx, cy := b.X+b.W/2, b.Y+b.H/2
	x0, y0 := a.lightbox.Get().x, a.lightbox.Get().y
	rt := u.Runtime()
	rt.PointerMove(cx, cy, 0)
	rt.PointerDown(cx, cy, input.ButtonLeft, 1, 0)
	rt.PointerMove(cx+30, cy+20, 0)
	rt.PointerUp(cx+30, cy+20, input.ButtonLeft, 0)
	u.Frame()
	if v := a.lightbox.Get(); !nearly(v.x, x0+30) || !nearly(v.y, y0+20) {
		t.Errorf("a drag of 30,20 moved the image from %v,%v to %v,%v", x0, y0, v.x, v.y)
	}
	view.Click()
	if !a.lightbox.Get().open() {
		t.Fatal("a click on the image closed the lightbox")
	}
	u.Press("0")
	if !a.lightbox.Get().fitted() || !u.Get("lightbox-fit").Disabled() || !u.Get("lightbox-zoom-out").Disabled() {
		t.Errorf("0 left the zoom at %v", a.lightbox.Get().scale)
	}
	u.Press("=")
	if s := a.lightbox.Get().scale; !nearly(s, zoomStep) {
		t.Errorf("+ zoomed to %v", s)
	}
	u.Get("lightbox-zoom-in").Click()
	u.Press("-")
	if s := a.lightbox.Get().scale; !nearly(s, zoomStep) {
		t.Errorf("zoom in then - left %v", s)
	}
	if got := u.Get("lightbox-zoom").Text(); got != "125%" {
		t.Errorf("zoom reads %q", got)
	}
	u.Press("right")
	if v := a.lightbox.Get(); v.path() != after || !v.fitted() {
		t.Errorf("moving on kept the zoom, or went to %q", v.path())
	}

	// Copy image puts the pixels on the clipboard.
	u.Get("lightbox-view").RightClick()
	u.Frame()
	u.ByRole(access.RoleMenuItem, "Copy image").Click()
	u.Settle()
	img := u.ClipboardImage()
	if img == nil {
		t.Fatal("Copy image left no image on the clipboard")
	}
	if b := img.Bounds(); b.Dx() != 640 || b.Dy() != 400 {
		t.Errorf("copied a %v image", b)
	}
	if r, g, b, _ := img.At(100, 370).RGBA(); r>>8 != 90 || g>>8 != 120 || b>>8 != 230 {
		t.Errorf("copied the wrong image: its accent is %d,%d,%d", r>>8, g>>8, b>>8)
	}

	u.Press("escape")
	u.Settle()
	if a.lightbox.Get().open() || u.Has("lightbox-view") {
		t.Error("Escape didn't close the lightbox")
	}
}

// A right click on a message's image copies it without opening the lightbox, and an image that can't be
// read says so rather than copying.
func TestCopyImageFromMessage(t *testing.T) {
	f := demoBackend()
	shot := sampleScreenshot(t.TempDir(), color.RGBA{240, 170, 60, 255})
	f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "Look", Images: []string{shot}})
	a, u := newUI(t, f)
	u.WaitFor("the image to load", func() bool { return u.HasRole(access.RoleButton, "View image") })
	u.Settle()
	u.ByRole(access.RoleButton, "View image").RightClick()
	u.Frame()
	u.ByRole(access.RoleMenuItem, "Copy image").Click()
	u.Settle()
	if img := u.ClipboardImage(); img == nil || img.Bounds().Dx() != 640 {
		t.Fatalf("copied %v", img)
	}
	if a.lightbox.Get().open() {
		t.Error("the right click opened the lightbox")
	}

	u.SetClipboard("before")
	var toasts []string
	a.mu.Lock()
	a.toast = func(_ ui.ToastVariant, title, _ string) { toasts = append(toasts, title) }
	a.mu.Unlock()
	a.copyImage(shot + ".missing")
	u.WaitFor("the toast", func() bool { return len(toasts) > 0 })
	if u.Clipboard() != "before" || !slices.Contains(toasts, "Couldn't copy the image") {
		t.Errorf("copying a missing image: clipboard %q, toasts %v", u.Clipboard(), toasts)
	}
}

// With RemoteImages, Copy image uses the full image the lightbox fetched; before it has arrived, it says
// so and starts fetching it.
func TestCopyRemoteImage(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	a.RemoteImages = true
	shot := sampleScreenshot(t.TempDir(), color.RGBA{240, 170, 60, 255})
	var toasts []string
	a.mu.Lock()
	a.toast = func(_ ui.ToastVariant, title, msg string) { toasts = append(toasts, title+": "+msg) }
	a.mu.Unlock()
	a.copyImage(shot)
	u.WaitFor("the toast", func() bool { return len(toasts) > 0 })
	if u.ClipboardImage() != nil || toasts[0] != "Couldn't copy the image: "+errStillLoading.Error() {
		t.Errorf("copied before the image arrived: %v", toasts)
	}
	u.WaitFor("the full image", func() bool { return a.imgData[shot] != nil })
	a.copyImage(shot)
	u.Settle()
	if img := u.ClipboardImage(); img == nil || img.Bounds().Dx() != 640 {
		t.Errorf("copied %v", img)
	}
}
