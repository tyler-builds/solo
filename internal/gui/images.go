package gui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/chartspec"
	"github.com/tyler-builds/solo/internal/core"
)

// previewSide is the longest side of the previews a page asks for (see App.RemoteImages).
const previewSide = 640

// inlineSide is the longest side of an image the markdown view draws among a message's text: it draws
// an image at its own size.
const inlineSide = 320

// image returns the decoded attachment at path, or nil when it can't be read or, with RemoteImages, hasn't
// arrived yet (loading). full asks for the image at its own size, for the lightbox; a preview may come
// scaled down. Call on the UI thread, in a render.
func (a *App) image(path string, full bool) (src *ui.ImageSource, loading bool) {
	if !a.RemoteImages {
		if src, ok := a.imgs[path]; ok {
			return src, false
		}
		if data, err := a.b.ReadImage(context.Background(), path, 0); err == nil {
			src, _ = ui.ImageFromBytes(data)
		}
		a.imgs[path] = src
		return src, false
	}
	a.imgVer.Get() // an image that arrives shows
	side, key := previewSide, "preview\x00"+path
	if full {
		side, key = 0, path
	}
	if src, ok := a.imgs[key]; ok {
		return src, false
	}
	if !a.imgLoading[key] {
		a.imgLoading[key] = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			var src *ui.ImageSource
			if data, err := a.b.ReadImage(ctx, path, side); err == nil {
				src, _ = ui.ImageFromBytes(data)
			}
			a.onUI(func() {
				a.imgs[key] = src
				delete(a.imgLoading, key)
				a.imgVer.Update(func(n int) int { return n + 1 })
			})
		}()
	}
	if full {
		// The preview stands in while the full image loads.
		if src := a.imgs["preview\x00"+path]; src != nil {
			return src, true
		}
	}
	return nil, true
}

// thumb is a square preview of an image; a click shows it full size. With remove, it has a button
// that takes it out of the message being written.
func (a *App) thumb(ctx *ui.Context, path string, size float32, remove func()) ui.Element {
	t := ctx.Theme()
	box := ui.Div().Width(ui.Px(size)).Height(ui.Px(size)).Shrink(0).Radius(radiusM).
		Border(1, t.Border).Background(t.Background).ClipContent()
	src, loading := a.image(path, false)
	if src == nil && loading {
		box = box.Row().Justify(ui.Center).Align(ui.Center).Label("Loading image").
			Child(ui.Icon(icons.Image).Size(16).Color(t.TextSubtle))
	} else if src == nil {
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
	src, _ := a.image(path, true)
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

// imageLoader loads the images of a message in markdown (see md): those among images, its own
// attachments, as small previews. Any other image, such as a file or a web address the text names, shows
// as a placeholder.
func (a *App) imageLoader(images []string) func(src string) (*ui.ImageSource, error) {
	return func(src string) (*ui.ImageSource, error) {
		p, ok := shownImage(src, images)
		if !ok {
			return nil, errors.New("not an image the message brought")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		data, err := a.b.ReadImage(ctx, p, inlineSide)
		if err != nil {
			return nil, err
		}
		return ui.ImageFromBytes(data)
	}
}

// shownImage returns the attachment among images that an image's source or a link names.
func shownImage(src string, images []string) (string, bool) {
	src = strings.TrimSpace(src)
	for _, p := range images {
		if core.SamePath(src, p) {
			return p, true
		}
	}
	return "", false
}

// inlineImages puts each image in markdown that is one of images in a link to itself, so a click
// opens it full size. An image already in a link keeps its link.
func inlineImages(md string, images []string) string {
	var b strings.Builder
	last := 0
	for _, im := range core.MarkdownImages(md) {
		if _, ok := shownImage(im.Dest, images); !ok || (im.Start > 0 && md[im.Start-1] == '[') {
			continue
		}
		b.WriteString(md[last:im.Start])
		b.WriteString("[" + md[im.Start:im.End] + "](" + md[im.DestStart:im.DestEnd] + ")")
		last = im.End
	}
	if last == 0 {
		return md
	}
	b.WriteString(md[last:])
	return b.String()
}

// agentText draws an agent's message, which may show images (core.TextData.Images) and charts. A line
// that only shows its images is drawn as a row of previews where it stands, each scaled to fit with its
// caption below; a click opens one full size. An image among other text is left to the markdown view
// (see md). A chart block (see chartspec) is drawn as a chart where it stands, unless it doesn't read as
// one: then it shows as code.
func (a *App) agentText(ctx *ui.Context, text string, bases []string, images []string) ui.Element {
	parts := a.chartParts(text, images)
	if len(parts) == 0 || len(parts) == 1 && parts[0].shots == nil && parts[0].chart == nil {
		return a.md(ctx, text, bases, images...)
	}
	col := ui.Div().Column().Gap(space3).MinWidth(ui.Px(0))
	for i, p := range parts {
		var e ui.Element
		switch {
		case p.chart != nil:
			e = chartView(ctx, p.chart)
		case p.shots != nil:
			e = a.imageRow(ctx, p.shots)
		default:
			e = a.md(ctx, p.text, bases, images...)
		}
		col = col.Child(ui.Div().Column().Key(fmt.Sprintf("part%d", i)).Child(e))
	}
	return col
}

// messagePart is a run of a message's markdown, a row of the images it shows, or a chart.
type messagePart struct {
	text  string
	shots []shot
	chart *chartspec.Spec
}

// shot is an image a message shows: the attachment and its caption (the image's alternative text).
type shot struct{ path, alt string }

// imageLines splits markdown at its lines that show nothing but images among images (blank lines
// between them too), each run of those becoming a row of shots.
func imageLines(md string, images []string) []messagePart {
	if len(images) == 0 {
		return nil
	}
	ims := core.MarkdownImages(md)
	var parts []messagePart
	var text strings.Builder
	var row []shot
	blanks := ""
	flush := func() {
		if row != nil {
			parts = append(parts, messagePart{shots: row})
			row = nil
		}
	}
	off := 0
	for _, line := range strings.SplitAfter(md, "\n") {
		start, end := off, off+len(line)
		off = end
		var shots []shot
		rest := ""
		last := start
		for _, im := range ims {
			if im.Start < start || im.Start >= end {
				continue
			}
			p, ok := shownImage(im.Dest, images)
			if !ok {
				shots = nil
				rest = "x"
				break
			}
			shots = append(shots, shot{p, im.Alt})
			rest += md[last:im.Start]
			last = im.End
		}
		rest += md[last:end]
		switch {
		case shots != nil && strings.TrimSpace(rest) == "":
			if row == nil {
				if t := strings.TrimSpace(text.String()); t != "" {
					parts = append(parts, messagePart{text: t})
				}
				text.Reset()
			}
			row = append(row, shots...)
			blanks = ""
		case row != nil && strings.TrimSpace(line) == "":
			blanks += line // between two lines of images, or before the text after them
		default:
			flush()
			text.WriteString(blanks)
			blanks = ""
			text.WriteString(line)
		}
	}
	flush()
	if t := strings.TrimSpace(text.String()); t != "" {
		parts = append(parts, messagePart{text: t})
	}
	return parts
}

// imageRow shows a message's images side by side, wrapping when the column is narrow.
func (a *App) imageRow(ctx *ui.Context, shots []shot) ui.Element {
	w := float32(440)
	if len(shots) > 1 {
		w = 236
	}
	if ww, _ := ctx.WindowSize(); ww > 0 {
		w = min(w, ww-96)
	}
	row := ui.Div().Row().Wrap().Gap(space3).Align(ui.Start)
	for _, s := range shots {
		row = row.Child(a.shotView(ctx, s, w))
	}
	return row
}

// shotView is one image of a message: a preview at most maxW wide and as tall, which keeps the
// image's shape, with its caption under it.
func (a *App) shotView(ctx *ui.Context, s shot, maxW float32) ui.Element {
	t := ctx.Theme()
	name := s.alt
	if name == "" {
		name = "Image"
	}
	w, h := maxW, maxW*5/8
	src, loading := a.image(s.path, false)
	if src != nil {
		if iw, ih := src.Size(); iw > 0 && ih > 0 {
			scale := min(maxW/float32(iw), maxW/float32(ih), 1)
			w, h = max(float32(iw)*scale, 24), max(float32(ih)*scale, 24)
		}
	}
	box := ui.Div().Width(ui.Px(w)).Height(ui.Px(h)).Shrink(0).Radius(radiusM).
		Border(1, t.Border).Background(t.Background).ClipContent()
	switch {
	case src == nil && loading:
		box = box.Row().Justify(ui.Center).Align(ui.Center).Label("Loading " + name).
			Child(ui.Icon(icons.Image).Size(16).Color(t.TextSubtle))
	case src == nil:
		box = box.Row().Justify(ui.Center).Align(ui.Center).Tooltip(name + " can't be shown").
			Child(ui.Icon(icons.ImageOff).Size(16).Color(t.TextSubtle))
	default:
		path := s.path
		box = box.Cursor(ui.CursorPointer).Role(access.RoleButton).Label("View " + name).TabIndex(0).
			OnClick(func() { a.lightbox.Set(path) }).
			Child(ui.Image(src).Size(ui.Full, ui.Full).Fit(ui.FitContain).Alt(name))
	}
	col := ui.Div().Column().Gap(space1).Width(ui.Px(w)).Child(box)
	if s.alt != "" {
		col = col.Child(caption(ctx, s.alt))
	}
	return col
}
