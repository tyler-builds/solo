package gui

import (
	"sync"

	"github.com/tyler-builds/jamb/ui"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

// faceKey is a face of the bundled UI font at a size.
type faceKey struct {
	weight ui.FontWeight
	size   float32
}

var (
	measureMu    sync.Mutex
	measureFonts map[ui.FontWeight]*opentype.Font
	measureFaces = map[faceKey]font.Face{}
)

// textWidth is how wide s is set on one line in the UI font (Inter, see Fonts) at size and weight,
// with the Text size setting applied (none without ctx): for laying out by what fits, which jamb measures only at paint.
func textWidth(ctx *ui.Context, s string, size float32, weight ui.FontWeight) float32 {
	measureMu.Lock()
	defer measureMu.Unlock()
	if measureFonts == nil {
		measureFonts = map[ui.FontWeight]*opentype.Font{}
		for _, f := range Fonts() {
			if p, err := opentype.Parse(f.Data); err == nil {
				measureFonts[f.Weight] = p
			}
		}
	}
	scale := float32(1)
	if ctx != nil && ctx.TextScale() > 0 {
		scale = float32(ctx.TextScale()) / 100
	}
	k := faceKey{weight, size * scale}
	face, ok := measureFaces[k]
	if !ok {
		f := measureFonts[weight]
		if f == nil {
			f = measureFonts[ui.FontRegular]
		}
		if f == nil {
			return float32(len(s)) * k.size * 0.6 // no font to measure with: about Inter's average
		}
		var err error
		face, err = opentype.NewFace(f, &opentype.FaceOptions{Size: float64(k.size), DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			return float32(len(s)) * k.size * 0.6
		}
		measureFaces[k] = face
	}
	return float32(font.MeasureString(face, s)) / 64
}
