package gui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // decoders for copying an attachment as pixels (see copyImage)
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/dialog"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// The lightbox shows an image as large as the window allows, and steps through the other images of the
// conversation it was opened from: the arrows under it, or the Left and Right keys. It stops at either
// end rather than wrapping, and says where it is ("3 of 7"). The wheel zooms around the pointer, a drag
// pans the zoomed image, a double-click goes between fitting the window and the image's own size, and
// the buttons and the +, - and 0 keys zoom in, zoom out and fit. A right click copies or saves it.

// maxZoom is the most the lightbox enlarges an image: eight times its own size.
const maxZoom = 8

// zoomStep is how much one step zooms: a button, a key, or one notch of the wheel.
const zoomStep = 1.25

// wheelNotch is the scroll of one notch of a mouse wheel, in logical pixels; a touchpad scrolls less at a
// time, and zooms by as much less.
const wheelNotch = 60

// gallery lists the images the lightbox steps through when it opens from somewhere: every image of the
// conversation, in the order they show. It is called when an image is clicked, so it lists what is
// shown then. nil is the clicked image alone.
type gallery func() []string

// viewer is the lightbox: the images it steps through, the one it shows, and its zoom.
type viewer struct {
	paths []string // none: the lightbox is closed
	at    int      // the index of the image shown
	// scale is how many logical pixels an image pixel takes; 0 fits the image to the view. x and y are
	// where the zoomed image's top-left corner is in the view.
	scale, x, y float32
}

// openViewer opens path among the images all lists, or alone when they don't include it.
func openViewer(path string, all []string) viewer {
	all = uniqueImages(all)
	at := slices.Index(all, path)
	if at < 0 {
		return viewer{paths: []string{path}}
	}
	return viewer{paths: all, at: at}
}

func (v viewer) open() bool { return len(v.paths) > 0 }

// path is the image shown, or "".
func (v viewer) path() string {
	if !v.open() {
		return ""
	}
	return v.paths[v.at]
}

func (v viewer) hasPrev() bool { return v.at > 0 }
func (v viewer) hasNext() bool { return v.at < len(v.paths)-1 }

// step moves by d images, stopping at either end, and fits the image it lands on.
func (v viewer) step(d int) viewer {
	at := min(max(v.at+d, 0), len(v.paths)-1)
	if at == v.at || !v.open() {
		return v
	}
	return viewer{paths: v.paths, at: at}
}

// frame is the size of the image shown, in its pixels, and of the view it is shown in, in logical pixels.
type frame struct{ iw, ih, vw, vh float32 }

// fit is the scale at which the image fits the view: never larger than its own size.
func (f frame) fit() float32 {
	if f.iw <= 0 || f.ih <= 0 {
		return 1
	}
	return min(1, f.vw/f.iw, f.vh/f.ih)
}

// zoom is the scale the image is shown at.
func (v viewer) zoom(f frame) float32 {
	if v.scale == 0 {
		return f.fit()
	}
	return v.scale
}

// fitted reports whether the image is shown fitted to the view, as large as it can be zoomed out.
func (v viewer) fitted() bool { return v.scale == 0 }

// origin is where the image's top-left corner is in the view.
func (v viewer) origin(f frame) (x, y float32) {
	if v.scale == 0 {
		s := f.fit()
		return (f.vw - f.iw*s) / 2, (f.vh - f.ih*s) / 2
	}
	return v.x, v.y
}

// zoomTo shows the image at scale s, between fitting the view and maxZoom, keeping the point (px, py) of
// the view over the same point of the image.
func (v viewer) zoomTo(f frame, s, px, py float32) viewer {
	fit := f.fit()
	s = min(max(s, fit), max(maxZoom, fit))
	if s <= fit*1.001 {
		v.scale, v.x, v.y = 0, 0, 0
		return v
	}
	cur := v.zoom(f)
	ox, oy := v.origin(f)
	u, w := (px-ox)/cur, (py-oy)/cur // the point of the image under (px, py)
	v.scale, v.x, v.y = s, px-u*s, py-w*s
	return v.clamped(f)
}

// zoomBy multiplies the zoom by k around the point (px, py) of the view.
func (v viewer) zoomBy(f frame, k, px, py float32) viewer { return v.zoomTo(f, v.zoom(f)*k, px, py) }

// panned moves the zoomed image by (dx, dy), keeping it over the view.
func (v viewer) panned(f frame, dx, dy float32) viewer {
	if v.scale == 0 {
		return v
	}
	v.x, v.y = v.x+dx, v.y+dy
	return v.clamped(f)
}

// clamped keeps the zoomed image over the view: no gap opens at an edge while the image is larger than
// the view, and along a side where it is smaller it is centered.
func (v viewer) clamped(f frame) viewer {
	if v.scale == 0 {
		return v
	}
	axis := func(pos, img, view float32) float32 {
		if img <= view {
			return (view - img) / 2
		}
		return min(max(pos, view-img), 0)
	}
	v.x = axis(v.x, f.iw*v.scale, f.vw)
	v.y = axis(v.y, f.ih*v.scale, f.vh)
	return v
}

// toggled is a double-click at (px, py): the image's own size, around the point, when it is fitted (twice
// its size when it fits at its own size already), and fitted otherwise.
func (v viewer) toggled(f frame, px, py float32) viewer {
	if !v.fitted() {
		return v.zoomTo(f, 0, px, py)
	}
	s := float32(1)
	if f.fit() >= 1 {
		s = 2
	}
	return v.zoomTo(f, s, px, py)
}

// uniqueImages is images with each path only at its first place.
func uniqueImages(images []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range images {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// showImage opens the lightbox on path, stepping through the images g lists.
func (a *App) showImage(path string, g gallery) {
	var all []string
	if g != nil {
		all = g()
	}
	a.lightbox.Set(openViewer(path, all))
}

// conversationImages lists the images a conversation's transcript shows, in order: those the user and
// Solo attached to their messages, those agents' messages show, and those of the messages waiting in
// Solo's queue. UI thread only.
func (a *App) conversationImages(id string) []string {
	var out []string
	if s := a.items[id]; s != nil {
		for _, it := range s.Peek() {
			switch it.Kind {
			case core.ItemUser:
				var d core.UserData
				it.Decode(&d)
				if !d.Synthetic {
					out = append(out, d.Images...)
				}
			case core.ItemAssistant:
				var d core.TextData
				it.Decode(&d)
				out = append(out, messageImageOrder(d.Text, d.Images)...)
			}
		}
	}
	if s := a.views[id]; s != nil {
		for _, q := range s.Peek().Queued {
			out = append(out, q.Images...)
		}
	}
	return uniqueImages(out)
}

// conversationGallery is the gallery of the conversation v.
func (a *App) conversationGallery(v core.ThreadView) gallery {
	id := v.ID
	return func() []string { return a.conversationImages(id) }
}

// messageImageOrder lists the images among images an agent's message shows, in the order its text shows
// them; any it only links to come after.
func messageImageOrder(text string, images []string) []string {
	if len(images) == 0 {
		return nil
	}
	var out []string
	for _, im := range core.MarkdownImages(text) {
		if p, ok := shownImage(im.Dest, images); ok {
			out = append(out, p)
		}
	}
	return uniqueImages(append(out, images...))
}

// lightboxPad is the padding of the lightbox's dialog in a window ww wide: less than a dialog's in a
// window as narrow as a phone's, which leaves the image and the toolbar room.
func lightboxPad(ww float32) float32 {
	if ww < 480 {
		return 12
	}
	return 20
}

// lightboxView is the lightbox's size for a window ww by wh: the largest view the image can have, which
// leaves room for the dialog's margin, its padding, its title and the toolbar.
func lightboxView(ww, wh float32) (maxW, maxH float32) {
	chrome := 2*24 + 2*lightboxPad(ww)
	maxW = max(min(ww*0.86, ww-chrome), 200)
	maxH = max(min(wh*0.72, wh-chrome-112), 120) // the title, the toolbar and the gaps between them
	return maxW, maxH
}

// lightboxDialog shows the image the viewer is on, as large as the window allows, with its toolbar.
func (a *App) lightboxDialog(ctx *ui.Context) ui.Element {
	v := a.lightbox.Get()
	if !v.open() {
		return nil
	}
	t := ctx.Theme()
	path := v.path()
	src, loading := a.image(path, true)
	focus := ui.Local(ctx, "focus", ui.NewFocusHandle()).Peek()
	ptr := ui.Local(ctx, "pointer", &lightboxPointer{}).Peek()
	ww, wh := ctx.WindowSize()
	maxW, maxH := lightboxView(ww, wh)
	f := frame{iw: 1, ih: 1}
	if src != nil {
		iw, ih := src.Size()
		f.iw, f.ih = float32(iw), float32(ih)
	}
	// The view is the image's size when fitted, and wide enough for the toolbar.
	fit := min(1, maxW/f.iw, maxH/f.ih)
	f.vw = min(max(f.iw*fit, 280), maxW)
	f.vh = min(max(f.ih*fit, 160), maxH)
	fit = f.fit()
	set := func(nv viewer) { a.lightbox.Set(nv) }
	cur := func() viewer { return a.lightbox.Peek().clamped(f) }
	v = v.clamped(f)
	s := v.zoom(f)
	ox, oy := v.origin(f)
	name := filepath.Base(path)
	label := "Image"
	if len(v.paths) > 1 {
		label = fmt.Sprintf("Image %d of %d", v.at+1, len(v.paths))
	}

	view := ui.Div().Key("lightbox-view").Width(ui.Px(f.vw)).Height(ui.Px(f.vh)).Shrink(0).
		Radius(radiusM).ClipContent().Background(t.Background).
		Role(access.RoleImage).Label(label).FocusHandle(focus).TabIndex(0).CapturePointer().
		ContextMenu(func() []ui.MenuItem { return a.imageMenu(path) })
	switch {
	case src == nil && loading:
		view = view.Row().Justify(ui.Center).Align(ui.Center).
			Child(ui.Icon(icons.Image).Size(20).Color(t.TextSubtle)).Tooltip("Loading " + name)
	case src == nil:
		view = view.Row().Justify(ui.Center).Align(ui.Center).
			Child(ui.Icon(icons.ImageOff).Size(20).Color(t.TextSubtle)).Tooltip(name + " can't be shown")
	default:
		view = view.
			OnScroll(func(e *ui.ScrollEvent) {
				e.StopPropagation()
				if e.DeltaY == 0 {
					return
				}
				px, py := ptr.at(f)
				k := float32(math.Pow(zoomStep, float64(-e.DeltaY/wheelNotch)))
				set(cur().zoomBy(f, k, px, py))
			}).
			OnPinch(func(e *ui.PinchEvent) {
				e.StopPropagation()
				if e.Phase != ui.GestureUpdate {
					return
				}
				set(cur().zoomBy(f, e.Scale, e.LocalX, e.LocalY).panned(f, e.PanX, e.PanY))
			}).
			OnMouseDown(func(e *ui.MouseEvent) {
				ptr.x, ptr.y, ptr.in = e.LocalX, e.LocalY, true
				if e.Button != input.ButtonLeft {
					return
				}
				if e.Clicks == 2 {
					ptr.dragging = false
					set(cur().toggled(f, e.LocalX, e.LocalY))
					return
				}
				ptr.dragging, ptr.sx, ptr.sy = true, e.X, e.Y
			}).
			OnMouseMove(func(e *ui.MouseEvent) {
				ptr.x, ptr.y, ptr.in = e.LocalX, e.LocalY, true
				if ptr.dragging {
					dx, dy := e.X-ptr.sx, e.Y-ptr.sy
					ptr.sx, ptr.sy = e.X, e.Y
					if c, nv := cur(), cur().panned(f, dx, dy); nv.x != c.x || nv.y != c.y {
						set(nv)
					}
				}
			}).
			OnMouseUp(func(e *ui.MouseEvent) { ptr.dragging = false }).
			OnMouseLeave(func() { ptr.in = ptr.dragging }).
			Child(ui.Div().Absolute().Left(ox).Top(oy).
				Child(ui.Image(src).Size(ui.Px(f.iw*s), ui.Px(f.ih*s)).Fit(ui.FitFill).Alt(name)))
	}

	center := func(k float32) func() {
		return func() { set(cur().zoomBy(f, k, f.vw/2, f.vh/2)) }
	}
	prev := func() { set(a.lightbox.Peek().step(-1)) }
	next := func() { set(a.lightbox.Peek().step(1)) }
	reset := func() { set(cur().zoomTo(f, 0, 0, 0)) }
	tip := func(s, key string) string {
		if a.Touch {
			return s
		}
		return s + " (" + key + ")"
	}

	bar := ui.Div().Key("lightbox-bar").Row().Align(ui.Center).Justify(ui.SpaceBetween).Wrap().Gap(space2).Width(ui.Full)
	if len(v.paths) > 1 {
		bar = bar.Child(ui.Div().Row().Align(ui.Center).Gap(space1).
			Child(ui.IconButton(icons.ChevronLeft).Ghost().Small().Label("Previous image").Tooltip(tip("Previous image", "←")).
				Disabled(!v.hasPrev()).OnClick(prev).Key("lightbox-prev")).
			Child(ui.Text(fmt.Sprintf("%d of %d", v.at+1, len(v.paths))).FontSize(12).TextColor(t.TextMuted).
				Key("lightbox-position")).
			Child(ui.IconButton(icons.ChevronRight).Ghost().Small().Label("Next image").Tooltip(tip("Next image", "→")).
				Disabled(!v.hasNext()).OnClick(next).Key("lightbox-next")))
	} else {
		bar = bar.Child(ui.Div())
	}
	if src != nil {
		bar = bar.Child(ui.Div().Row().Align(ui.Center).Gap(space1).
			Child(ui.IconButton(icons.ZoomOut).Ghost().Small().Label("Zoom out").Tooltip(tip("Zoom out", "-")).
				Disabled(v.fitted()).OnClick(center(1 / zoomStep)).Key("lightbox-zoom-out")).
			Child(ui.Text(fmt.Sprintf("%d%%", int(math.Round(float64(s*100))))).FontSize(12).TextColor(t.TextMuted).
				Width(ui.Px(36)).Key("lightbox-zoom")).
			Child(ui.IconButton(icons.ZoomIn).Ghost().Small().Label("Zoom in").Tooltip(tip("Zoom in", "+")).
				Disabled(s >= max(maxZoom, fit)).OnClick(center(zoomStep)).Key("lightbox-zoom-in")).
			Child(ui.IconButton(icons.Scan).Ghost().Small().Label("Fit to window").Tooltip(tip("Fit to window", "0")).
				Disabled(v.fitted()).OnClick(reset).Key("lightbox-fit")))
	}

	body := ui.Div().Column().Align(ui.Center).Gap(space3).Width(ui.Full).
		OnKeyDown(func(e *ui.KeyEvent) {
			if e.Mods.Has(input.ModCtrl) || e.Mods.Has(input.ModAlt) || e.Mods.Has(input.ModSuper) {
				return // Ctrl with + - 0 zooms the window
			}
			switch e.Key {
			case input.KeyLeft:
				prev()
			case input.KeyRight:
				next()
			case input.KeyEqual:
				center(zoomStep)()
			case input.KeyMinus:
				center(1 / zoomStep)()
			case input.Key0:
				reset()
			default:
				return
			}
			e.StopPropagation()
		}).
		Child(view).
		Child(bar)
	pad := lightboxPad(ww)
	return ui.Modal("Image", body).Width(ui.Px(f.vw + 2*pad)).MaxWidth(ui.Px(max(ww-48, 240))).
		Styles(ui.ModalStyle{Panel: ui.Style{Padding: ui.All(pad)}}).
		InitialFocus(focus).CloseOnBackdrop().OnClose(func() { a.lightbox.Set(viewer{}) }).Key("lightbox")
}

// lightboxPointer is where the pointer is over the lightbox's view, and a drag that pans the image.
type lightboxPointer struct {
	x, y     float32 // in the view
	in       bool
	dragging bool
	sx, sy   float32 // where the drag last was, in the window
}

// at is the point the wheel zooms around: the pointer, or the view's center when it isn't over the view.
func (p *lightboxPointer) at(f frame) (x, y float32) {
	if !p.in {
		return f.vw / 2, f.vh / 2
	}
	return p.x, p.y
}

// imageMenu is what a right click on an image offers: copy it, and on this machine, save it.
func (a *App) imageMenu(path string) []ui.MenuItem {
	if a.RemoteImages {
		a.image(path, true) // fetched now, so that Copy image has it at hand
	}
	items := []ui.MenuItem{{Label: "Copy image", Icon: icons.Copy, OnClick: func() { a.copyImage(path) }}}
	if !a.RemoteImages && a.saveFile != nil {
		// A web page has no files to save to.
		items = append(items, ui.MenuItem{Label: "Save image as…", Icon: icons.Download, OnClick: func() { a.saveImage(path) }})
	}
	return items
}

// errStillLoading is why an image fetched from another machine can't be copied yet.
var errStillLoading = errors.New("it is still loading; try again in a moment")

// fullImage returns the attachment's file as it is stored: read here, or, with RemoteImages, as fetched for
// the lightbox. UI thread only.
func (a *App) fullImage(path string) ([]byte, error) {
	if a.RemoteImages {
		if data := a.imgData[path]; data != nil {
			return data, nil
		}
		a.image(path, true)
		return nil, errStillLoading
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.b.ReadImage(ctx, path, 0)
}

// copyImage puts the image at path on the clipboard as pixels, which other apps paste as an image.
func (a *App) copyImage(path string) {
	if a.clipImage == nil {
		return
	}
	data, err := a.fullImage(path)
	var img image.Image
	if err == nil {
		img, _, err = image.Decode(bytes.NewReader(data))
	}
	if err != nil {
		a.notify(ui.ToastError, "Couldn't copy the image", err.Error())
		return
	}
	a.clipImage(img)
	a.notify(ui.ToastSuccess, "Copied the image", filepath.Base(path))
}

// saveImage asks where to save the image at path, and writes the file there as it is.
func (a *App) saveImage(path string) {
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	opts := dialog.FileOptions{Title: "Save image", Name: filepath.Base(path), Extension: ext}
	if ext != "" {
		opts.Filters = []dialog.Filter{{Name: strings.ToUpper(ext) + " image", Patterns: []string{"*." + ext}}}
	}
	a.saveFile(opts, func(r dialog.FileResult) {
		if r.Canceled || len(r.Paths) == 0 {
			if r.Err != nil {
				a.notify(ui.ToastError, "Couldn't save the image", r.Err.Error())
			}
			return
		}
		to := r.Paths[0]
		data, err := a.fullImage(path)
		if err == nil {
			err = os.WriteFile(to, data, 0o644)
		}
		if err != nil {
			a.notify(ui.ToastError, "Couldn't save the image", err.Error())
			return
		}
		a.notify(ui.ToastSuccess, "Saved the image", tildePath(to))
	})
}
