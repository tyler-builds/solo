package gui

import (
	"image"
	"strings"
	"testing"

	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
)

// Inline code in Solo's message has no box: unselected, its line shows only the page around the
// teal text; and a selection over the plain text and the code shows on both, in either theme, and
// Quote quotes the code with the text.
func TestSelectionShowsOverInlineCode(t *testing.T) {
	light, dark := Themes()
	for _, th := range []*ui.Theme{light, dark} {
		name := "light"
		if th.Dark {
			name = "dark"
		}
		t.Run(name, func(t *testing.T) {
			const text = "The four causes are jamb gaps 34 to 37 in `docs/jamb-gaps.md`."
			f := demoBackend()
			f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{Text: text})
			a := New(f)
			a.items = map[string]*ui.Signal[[]core.Item]{}
			u := uitest.New(t, a.Root, uitest.Size(1440, 900), uitest.Theme(th))
			t.Cleanup(u.Close)
			started(a, u)
			u.Settle()

			x0, y := textStart(t, u, "The four causes")
			b := byNamePrefix(t, u, "The four causes").Bounds()
			row := image.Rect(int(x0), int(y-6), int(b.X+b.W), int(y+6))
			bg := th.Background
			// The code's columns are where its teal text is.
			code, img := codeColor(th), u.Screenshot()
			first, last := -1, -1
			for x := row.Min.X; x < row.Max.X; x++ {
				for yy := row.Min.Y; yy < row.Max.Y; yy++ {
					if near(img.RGBAAt(x, yy), code) {
						if first < 0 {
							first = x
						}
						last = x
						break
					}
				}
			}
			if first < 0 {
				t.Fatal("no teal inline code in the message")
			}
			if n := countNot(img, image.Rect(first, row.Min.Y, last, row.Max.Y), bg, code); n > 0 {
				t.Errorf("unselected, %d pixels around the code are neither the page nor its text: a box", n)
			}

			rt := u.Runtime()
			rt.PointerMove(x0, y, 0)
			rt.PointerDown(x0, y, input.ButtonLeft, 1, 0)
			rt.PointerMove(float32(last+30), y, 0)
			rt.PointerUp(float32(last+30), y, input.ButtonLeft, 0)
			u.Frame()
			img = u.Screenshot()
			plain := image.Rect(int(x0)+40, row.Min.Y, first-20, row.Max.Y)
			if n := countIs(img, plain, bg); n > 0 {
				t.Fatalf("%d pixels of the selected plain text show the page: no selection", n)
			}
			if n := countIs(img, image.Rect(first, row.Min.Y, last, row.Max.Y), bg); n > 0 {
				t.Errorf("%d pixels of the selected code show the page, not the selection", n)
			}

			if !u.Has("quote-selection") {
				t.Fatal("no Quote button by the selection")
			}
			u.Click("quote-selection")
			u.Frame()
			if qs := a.quotesOf(core.OrchestratorID).Peek(); len(qs) != 1 || !strings.Contains(qs[0], "37 in docs/jamb-gaps.md") {
				t.Errorf("quoted %q; want the code with the text", qs)
			}
		})
	}
}

// near reports whether c is within a few levels of want in each channel.
func near(c interface{ RGBA() (r, g, b, a uint32) }, want ui.Color) bool {
	r, g, b, _ := c.RGBA()
	d := func(x uint32, y float32) bool {
		v := float32(x>>8) - y*255
		return v > -8 && v < 8
	}
	return d(r, want.R) && d(g, want.G) && d(b, want.B)
}

// countIs counts the pixels of r in img that are c.
func countIs(img *image.RGBA, r image.Rectangle, c ui.Color) int {
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if near(img.RGBAAt(x, y), c) {
				n++
			}
		}
	}
	return n
}

// countNot counts the pixels of r in img that are neither of the two colors nor a blend of them, as
// the edges of glyphs are: the pixels of a box behind the text.
func countNot(img *image.RGBA, r image.Rectangle, page, text ui.Color) int {
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := img.RGBAAt(x, y)
			if !between(c.R, page.R, text.R) || !between(c.G, page.G, text.G) || !between(c.B, page.B, text.B) {
				n++
			}
		}
	}
	return n
}

// between reports whether the channel v lies between the channels a and b, within a few levels.
func between(v uint8, a, b float32) bool {
	lo, hi := min(a, b)*255-8, max(a, b)*255+8
	return float32(v) >= lo && float32(v) <= hi
}
