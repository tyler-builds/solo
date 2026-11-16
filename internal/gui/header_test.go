package gui

import (
	"fmt"
	"image"
	"image/color"
	"testing"

	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"
)

// The title bar's ends line up with the panels below it, as drawn: the Solo mark's left edge with the
// rail panel's left border, and, where the desktop puts no window buttons after it, the settings
// gear's glyph with the right panel's right border. Window buttons stay where the desktop puts them.
func TestTitleBarLinesUpWithPanels(t *testing.T) {
	light, dark := Themes()
	for _, th := range []struct {
		name  string
		theme *ui.Theme
	}{{"dark", dark}, {"light", light}} {
		for _, w := range []float32{1440, 1100} {
			for _, layout := range []string{"appmenu:minimize,maximize,close", ":"} {
				t.Run(fmt.Sprintf("%s/%v/%s", th.name, w, layout), func(t *testing.T) {
					a := New(demoBackend())
					u := uitest.New(t, a.Root, uitest.Size(w, 800), uitest.Theme(th.theme),
						uitest.Settings(ui.SystemSettings{Dark: th.name == "dark", CaptionButtons: layout,
							RoundCaptionButtons: layout != ":", ReducedMotion: true, CaretWidth: 1}))
					t.Cleanup(u.Close)
					started(a, u)
					u.Settle()
					img := u.Screenshot()
					panelL, panelR := panelBorders(img)
					markL, lastR := barInk(img)
					if markL != panelL {
						t.Errorf("the Solo mark starts at x=%d; want it on the rail panel's border at x=%d", markL, panelL)
					}
					if layout == ":" && lastR != panelR {
						t.Errorf("the settings gear ends at x=%d; want it on the right panel's border at x=%d", lastR, panelR)
					}
				})
			}
		}
	}
}

// panelBorders returns the columns of the leftmost and rightmost panel borders, halfway down img.
func panelBorders(img image.Image) (left, right int) {
	b := img.Bounds()
	y := b.Dy() / 2
	bg := img.At(3, 60) // the window between its edge and the rail panel
	left, right = -1, -1
	for x := 3; x < 60 && left < 0; x++ {
		if !nearColor(img.At(x, y), bg) {
			left = x
		}
	}
	for x := b.Dx() - 4; x > b.Dx()-60 && right < 0; x-- {
		if !nearColor(img.At(x, y), bg) {
			right = x
		}
	}
	return left, right
}

// barInk returns the first and last columns drawn in the title bar, between its top and bottom
// margins: the left edge of what it shows first and the right edge of what it shows last.
func barInk(img image.Image) (first, last int) {
	b := img.Bounds()
	bg := img.At(b.Dx()/2+300, 20) // the bar, right of the search field
	drawn := func(x int) bool {
		for y := 8; y <= 32; y++ {
			if !nearColor(img.At(x, y), bg) {
				return true
			}
		}
		return false
	}
	first, last = -1, -1
	for x := 2; x < 200 && first < 0; x++ {
		if drawn(x) {
			first = x
		}
	}
	for x := b.Dx() - 2; x > b.Dx()-300 && last < 0; x-- {
		if drawn(x) {
			last = x
		}
	}
	return first, last
}

// nearColor reports whether a and b differ by at most 6 in each channel, out of 255.
func nearColor(a, b color.Color) bool {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	d := func(x, y uint32) bool { return x>>8 <= y>>8+6 && y>>8 <= x>>8+6 }
	return d(ar, br) && d(ag, bg) && d(ab, bb)
}
