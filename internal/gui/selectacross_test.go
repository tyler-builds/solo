package gui

import (
	"strings"
	"testing"

	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// A drag from one paragraph of Solo's message into the next selects across them, and Quote quotes
// all of it, with the paragraphs apart.
func TestSelectAcrossParagraphs(t *testing.T) {
	f := demoBackend()
	f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{
		Text: "First paragraph of the reply.\n\nSecond paragraph of the reply."})
	a, u := newUIWith(t, f, func(a *App) { a.items = map[string]*ui.Signal[[]core.Item]{} })
	u.Settle()
	// From the message's first word to a word on its last line, the second paragraph's.
	x0, y0 := textStart(t, u, "First paragraph")
	b := byNamePrefix(t, u, "First paragraph").Bounds()
	x1, y1 := x0+60, b.Y+b.H-space2-8
	rt := u.Runtime()
	rt.PointerMove(x0, y0, 0)
	rt.PointerDown(x0, y0, input.ButtonLeft, 1, 0)
	rt.PointerMove(x1, y1, 0)
	rt.PointerUp(x1, y1, input.ButtonLeft, 0)
	u.Frame()
	if !u.Has("quote-selection") {
		t.Fatal("no Quote button by the selection")
	}
	u.Click("quote-selection")
	u.Frame()
	qs := a.quotesOf(core.OrchestratorID).Peek()
	if len(qs) != 1 || !strings.Contains(qs[0], "paragraph of the reply.\n\nSecond") {
		t.Fatalf("quoted %q; want both paragraphs", qs)
	}
}
