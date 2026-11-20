package gui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/dialog"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
)

// attachment is a file in the message being written: an image, shown as a thumbnail, or any other file,
// shown as a chip with its name and size. One on its way to the machine (a page's upload, or a copy) has
// no ref yet and shows as a chip with its progress; Send waits for it.
type attachment struct {
	id    int64  // tells it apart while it loads
	ref   string // the attachment, once stored (see api.Backend.Send)
	image bool   // ref is an image: a thumbnail
	name  string // the file's name, as the user knows it
	size  int64  // in bytes; -1 when not known
	sent  int64  // bytes that have arrived, while it uploads
	err   string // why it couldn't be attached
	// cancel stops it while it loads; retry starts it again once it has failed.
	cancel context.CancelFunc
	retry  func()
}

// loading reports whether the attachment is still on its way.
func (at attachment) loading() bool { return at.ref == "" && at.err == "" }

var attachmentIDs atomic.Int64

// attachmentRefs returns the references of the attachments that are stored, in order.
func attachmentRefs(list []attachment) []string {
	var out []string
	for _, at := range list {
		if at.ref != "" {
			out = append(out, at.ref)
		}
	}
	return out
}

// attachmentImages returns the images among the attachments, for the lightbox.
func attachmentImages(list []attachment) []string {
	var out []string
	for _, at := range list {
		if at.ref != "" && at.image {
			out = append(out, at.ref)
		}
	}
	return out
}

// attachmentsBusy reports whether any attachment is still loading or failed: a message isn't sent
// without all of them.
func attachmentsBusy(list []attachment) bool {
	return slices.ContainsFunc(list, func(at attachment) bool { return at.ref == "" })
}

// loader stores one attachment and returns its reference and whether it is an image. progress is told
// the bytes that have arrived.
type loader func(ctx context.Context, progress func(int64)) (ref string, image bool, err error)

// attacher adds attachments to a message being written (atts), each shown at once and stored in the
// background. refocus puts the caret back in the text, for files that came from outside it (the file
// picker or a drop), so Enter sends them.
type attacher struct {
	a     *App
	atts  *ui.Signal[[]attachment]
	focus func()
}

// update changes the attachment with id, if it is still there.
func (k attacher) update(id int64, f func(*attachment)) {
	list := slices.Clone(k.atts.Peek())
	for i := range list {
		if list[i].id == id {
			f(&list[i])
			k.atts.Set(list)
			return
		}
	}
}

// add shows an attachment named name of size bytes (-1 for unknown) and stores it with load. Call on
// the UI thread.
func (k attacher) add(name string, size int64, image, refocus bool, load loader) {
	id := attachmentIDs.Add(1)
	k.atts.Set(append(slices.Clone(k.atts.Peek()), attachment{id: id, name: name, size: size, image: image}))
	k.start(id, load)
	if refocus && k.focus != nil {
		k.focus()
	}
}

// start stores the attachment with id, which is shown.
func (k attacher) start(id int64, load loader) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	k.update(id, func(at *attachment) { at.cancel, at.err, at.sent, at.retry = cancel, "", 0, nil })
	go func() {
		defer cancel()
		var last time.Time
		progress := func(n int64) {
			// A page's upload reports each part; the chip needn't show more than a few a second.
			if now := time.Now(); now.Sub(last) > 100*time.Millisecond {
				last = now
				k.a.onUI(func() { k.update(id, func(at *attachment) { at.sent = n }) })
			}
		}
		ref, image, err := load(ctx, progress)
		k.a.onUI(func() {
			// One removed while it loaded is gone, and so is what arrived of it.
			k.update(id, func(at *attachment) {
				at.cancel = nil
				if err != nil {
					at.err = err.Error()
					at.retry = func() {
						k.start(id, load)
						if k.focus != nil {
							k.focus()
						}
					}
					k.a.notify(ui.ToastError, "Couldn't attach "+at.name, err.Error())
					return
				}
				at.ref, at.image = ref, image
			})
		})
	}()
}

// remove takes the attachment at index i out of the message, stopping it if it still loads.
func (k attacher) remove(i int) {
	list := k.atts.Peek()
	if i < 0 || i >= len(list) {
		return
	}
	if c := list[i].cancel; c != nil {
		c()
	}
	k.atts.Set(slices.Delete(slices.Clone(list), i, i+1))
}

// importPath attaches a file on this machine (the desktop app's picker or a drop): an image as an image,
// and any other file, or an image Claude can't take, as a file.
func (k attacher) importPath(path string, refocus bool) {
	size := int64(-1)
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	b := k.a.b
	k.add(filepath.Base(path), size, isImageFile(path), refocus, func(context.Context, func(int64)) (string, bool, error) {
		if isImageFile(path) {
			if p, err := b.ImportImage(path); err == nil {
				return p, true, nil
			}
		}
		p, err := b.ImportFile(path)
		return p, false, err
	})
}

// importData attaches a file by its contents (a page's picker, whose files have no paths): an image as
// an image, and any other file, or an image Claude can't take, as a file sent to the machine in parts.
func (k attacher) importData(name string, data []byte, refocus bool) {
	b := k.a.b
	img := isImageFile(name) && len(data) <= api.MaxImageUpload
	k.add(name, int64(len(data)), img, refocus, func(ctx context.Context, progress func(int64)) (string, bool, error) {
		if img {
			if p, err := b.SaveImageData(ctx, data); err == nil {
				return p, true, nil
			} else if ctx.Err() != nil {
				return "", false, err
			}
		}
		if len(data) > api.MaxFileUpload {
			return "", false, fmt.Errorf("%s is larger than %d MB", name, api.MaxFileUpload>>20)
		}
		p, err := b.SaveFile(ctx, name, data, progress)
		return p, false, err
	})
}

// picked attaches the files of an Open dialog: by path, or by contents in a web page, whose files have
// no paths.
func (k attacher) picked(files []dialog.File) {
	for i, f := range files {
		refocus := i == len(files)-1
		if f.Path != "" {
			k.importPath(f.Path, refocus)
		} else {
			k.importData(f.Name, f.Data, refocus)
		}
	}
}

// attachmentStrip shows a message's attachments while it is written: images as thumbnails, other files
// as chips, each with a button that takes it out.
func (a *App) attachmentStrip(ctx *ui.Context, key string, k attacher, list []attachment) ui.Element {
	strip := ui.Div().Key(key + "-images").Row().Wrap().Gap(space2).PaddingTop(space1).PaddingBottom(space2)
	for i, at := range list {
		idx := i
		remove := func() {
			k.remove(idx)
			if k.focus != nil {
				k.focus() // the remove button goes with the attachment
			}
		}
		if at.ref != "" && at.image {
			strip = strip.Child(a.thumb(ctx, at.ref, 56, func() []string { return attachmentImages(k.atts.Peek()) }, remove))
			continue
		}
		strip = strip.Child(fileChip(ctx, at, remove))
	}
	return strip
}

// fileChip is a file in a message: its name and size, how much of it has arrived while it loads, or why
// it failed. With remove, it has a button that takes it out of the message being written, and a failed
// one offers to try again.
func fileChip(ctx *ui.Context, at attachment, remove func()) ui.Element {
	t := ctx.Theme()
	icon := ui.Element(ui.Icon(icons.File).Size(18).Color(t.TextMuted))
	detail := ""
	if at.size >= 0 {
		detail = core.FormatSize(at.size)
	}
	var bar ui.Element
	border := t.Border
	label := at.name
	switch {
	case at.err != "":
		icon = ui.Icon(icons.CircleAlert).Size(18).Color(t.Danger)
		detail, border = "Didn't attach", t.Danger
		label = at.name + ": didn't attach. " + at.err
	case at.loading() && at.size > 0 && at.sent > 0:
		frac := float64(at.sent) / float64(at.size)
		detail = fmt.Sprintf("%d%% of %s", int(frac*100), core.FormatSize(at.size))
		bar = ui.ProgressBar(frac).HideValue().Label("Uploading " + at.name)
		label = at.name + ", uploading"
	case at.loading():
		icon = ui.ProgressRing().Size(ui.RingSmall).Label("Attaching " + at.name)
		if detail != "" {
			detail = "Attaching · " + detail
		} else {
			detail = "Attaching"
		}
		label = at.name + ", attaching"
	}
	text := ui.Div().Column().Gap(2).MinWidth(ui.Px(0)).Grow(1).
		Child(ui.Text(at.name).FontSize(13).Medium().NoWrap().TruncateMiddle()).
		Child(ui.Text(detail).FontSize(12).TextColor(t.TextSubtle).NoWrap().Truncate())
	if bar != nil {
		text = text.Child(bar)
	}
	chip := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingLeft(space2).PaddingRight(space3).
		Height(ui.Px(56)).MaxWidth(ui.Px(240)).Shrink(0).Radius(radiusM).Border(1, border).Background(t.Background).
		Role(access.RoleGroup).Label(label).Tooltip(chipTooltip(at)).
		Child(ui.Div().Row().Justify(ui.Center).Align(ui.Center).Width(ui.Px(28)).Height(ui.Px(28)).Shrink(0).
			Radius(radiusM).Background(t.Surface).Child(icon)).
		Child(text)
	if at.retry != nil {
		chip = chip.Child(ui.IconButton(icons.RotateCw).Ghost().Small().Label("Try again").Tooltip("Try again").
			OnClick(at.retry))
	}
	if remove == nil {
		return chip
	}
	what := "Remove file"
	if at.loading() {
		what = "Stop and remove file"
	}
	return ui.Div().Shrink(0).
		Child(chip).
		Child(ui.Div().Absolute().Top(-6).Right(-6).
			Child(ui.Div().Row().Justify(ui.Center).Align(ui.Center).Width(ui.Px(18)).Height(ui.Px(18)).Radius(9).
				Background(t.SurfaceActive).Border(1, t.BorderStrong).Cursor(ui.CursorPointer).
				Hover(ui.Style{Background: t.SurfaceHover}).
				Role(access.RoleButton).Label(what).Tooltip("Remove").OnClick(remove).
				Child(ui.Icon(icons.X).Size(11).Color(t.TextMuted))))
}

// chipTooltip is what a file chip's tooltip says: its name in full, and where it is or why it failed.
func chipTooltip(at attachment) string {
	switch {
	case at.err != "":
		return at.name + "\n" + at.err
	case at.ref != "":
		return at.name + "\nOn the machine at " + at.ref
	}
	return at.name
}

// messageFiles shows the files sent with a message, aligned to its side of the conversation.
func messageFiles(ctx *ui.Context, files []core.FileRef, side ui.Alignment) ui.Element {
	if len(files) == 0 {
		return nil
	}
	row := ui.Div().Row().Wrap().Gap(space2)
	if side == ui.End {
		row = row.Justify(ui.End)
	}
	for _, f := range files {
		row = row.Child(fileChip(ctx, attachment{ref: f.Path, name: f.Name, size: f.Size}, nil))
	}
	return row
}
