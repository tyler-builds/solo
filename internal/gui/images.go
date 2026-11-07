package gui

import (
	"os"
	"path/filepath"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"
)

// image returns the decoded attachment at path, or nil when it can't be read. Call on the UI thread.
func (a *App) image(path string) *ui.ImageSource {
	if src, ok := a.imgs[path]; ok {
		return src
	}
	var src *ui.ImageSource
	if data, err := os.ReadFile(path); err == nil {
		src, _ = ui.ImageFromBytes(data)
	}
	a.imgs[path] = src
	return src
}

// thumb is a square preview of an image; a click shows it full size. With remove, it has a button
// that takes it out of the message being written.
func (a *App) thumb(ctx *ui.Context, path string, size float32, remove func()) ui.Element {
	t := ctx.Theme()
	box := ui.Div().Width(ui.Px(size)).Height(ui.Px(size)).Shrink(0).Radius(radiusM).
		Border(1, t.Border).Background(t.Background).ClipContent()
	src := a.image(path)
	if src == nil {
		box = box.Row().Justify(ui.Center).Align(ui.Center).Tooltip(filepath.Base(path) + " can't be shown").
			Child(ui.Icon(icons.ImageOff).Size(16).Color(t.TextSubtle))
	} else {
		box = box.Cursor(ui.CursorPointer).Role(access.RoleButton).Label("View image").TabIndex(0).
			OnClick(func() { a.lightbox.Set(path) }).
			Child(ui.Image(src).Size(ui.Full, ui.Full).Fit(ui.FitCover).Alt("Attached image"))
	}
	if remove == nil {
		return box
	}
	return ui.Div().Shrink(0).
		Child(box).
		Child(ui.Div().Absolute().Top(-6).Right(-6).
			Child(ui.Div().Row().Justify(ui.Center).Align(ui.Center).Width(ui.Px(18)).Height(ui.Px(18)).Radius(9).
				Background(t.SurfaceActive).Border(1, t.BorderStrong).Cursor(ui.CursorPointer).
				Hover(ui.Style{Background: t.SurfaceHover}).
				Role(access.RoleButton).Label("Remove image").Tooltip("Remove").OnClick(remove).
				Child(ui.Icon(icons.X).Size(11).Color(t.TextMuted))))
}

// messageImages shows a sent message's images, aligned to its side of the conversation.
func (a *App) messageImages(ctx *ui.Context, paths []string, side ui.Alignment) ui.Element {
	if len(paths) == 0 {
		return nil
	}
	size := float32(160)
	if len(paths) > 1 {
		size = 96
	}
	row := ui.Div().Row().Wrap().Gap(space2)
	if side == ui.End {
		row = row.Justify(ui.End)
	}
	for _, p := range paths {
		row = row.Child(a.thumb(ctx, p, size, nil))
	}
	return row
}

// lightboxDialog shows one image as large as the window allows.
func (a *App) lightboxDialog(ctx *ui.Context) ui.Element {
	path := a.lightbox.Get()
	if path == "" {
		return nil
	}
	src := a.image(path)
	if src == nil {
		return nil
	}
	ww, wh := ctx.WindowSize()
	iw, ih := src.Size()
	maxW, maxH := ww*0.8, wh*0.72
	scale := min(1, maxW/float32(iw), maxH/float32(ih))
	w, h := float32(iw)*scale, float32(ih)*scale
	close := func() { a.lightbox.Set("") }
	body := ui.Div().Row().Justify(ui.Center).Width(ui.Full).
		Child(ui.Image(src).Size(ui.Px(w), ui.Px(h)).Fit(ui.FitContain).Radius(radiusM).Alt(filepath.Base(path)))
	return ui.Modal("Image", body).Width(ui.Px(max(w+48, 320))).MaxWidth(ui.Px(ww * 0.9)).
		CloseOnBackdrop().OnClose(close).Key("lightbox")
}
