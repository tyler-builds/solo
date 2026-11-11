package gui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// Quoting: text selected in a conversation offers a Quote button; a quote waits above that
// conversation's composer and is sent ahead of what the user types, as a Markdown blockquote.

// quoteButtonWidth is the Quote button's width; it is fixed so the button can end where the selection does.
const quoteButtonWidth, quoteButtonHeight = 74, 28

// convID is the conversation a composer key ("orch-draft", "draft-<id>") or transcript scope
// ("orch", "thread-<id>") belongs to: the thread's ID, or the orchestrator's.
func convID(key string) string {
	if key == "orch" || key == "orch-draft" {
		return core.OrchestratorID
	}
	return strings.TrimPrefix(strings.TrimPrefix(key, "draft-"), "thread-")
}

// quotesOf is what waits to be quoted in a conversation's next message. Like a draft, it is kept
// while the user looks at other threads.
func (a *App) quotesOf(id string) *ui.Signal[[]string] {
	if a.quotes == nil {
		a.quotes = map[string]*ui.Signal[[]string]{}
	}
	s := a.quotes[id]
	if s == nil {
		s = ui.NewSignal([]string(nil))
		a.quotes[id] = s
	}
	return s
}

// composerFocus is the handle that focuses a conversation's composer.
func (a *App) composerFocus(id string) *ui.FocusHandle {
	if id == core.OrchestratorID {
		return a.orchFocus
	}
	if a.focuses == nil {
		a.focuses = map[string]*ui.FocusHandle{}
	}
	h := a.focuses[id]
	if h == nil {
		h = ui.NewFocusHandle()
		a.focuses[id] = h
	}
	return h
}

// addQuote attaches text to a conversation's next message and puts the caret in its composer.
func (a *App) addQuote(id, text string) {
	text = strings.Trim(text, "\r\n\t ")
	if text == "" {
		return
	}
	q := a.quotesOf(id)
	if qs := q.Peek(); len(qs) > 0 && qs[len(qs)-1] == text {
		a.composerFocus(id).Focus() // the same selection, quoted twice
		return
	}
	q.Set(append(slices.Clone(q.Peek()), text))
	a.composerFocus(id).Focus()
}

// removeQuote takes the i-th quote off a conversation's next message.
func (a *App) removeQuote(id string, i int) {
	q := a.quotesOf(id)
	if qs := q.Peek(); i >= 0 && i < len(qs) {
		q.Set(slices.Delete(slices.Clone(qs), i, i+1))
	}
}

// quoteMessage is the message sent for what the user typed with quotes: each quote as a Markdown
// blockquote, in the order they were added, then the text. Quotes alone make a message too.
func quoteMessage(quotes []string, text string) string {
	var b strings.Builder
	for _, q := range quotes {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		for i, line := range strings.Split(strings.ReplaceAll(q, "\r\n", "\n"), "\n") {
			if i > 0 {
				b.WriteByte('\n')
			}
			if line = strings.TrimRight(line, " \t"); line == "" {
				b.WriteByte('>')
			} else {
				b.WriteString("> " + line)
			}
		}
	}
	if text != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
	}
	return b.String()
}

// quotable lets text selected in a conversation's transcript be quoted: a Quote button floats by
// the selection, and Ctrl+Shift+Q does the same. Escape puts the selection away. A conversation
// that can't be messaged (a finished or starting thread) offers no Quote.
func (a *App) quotable(ctx *ui.Context, view *ui.DivElement, v core.ThreadView, scope string) *ui.DivElement {
	if scope != "orch" && (core.Finished(v.Status) || v.Status == core.StatusQueued) {
		return view
	}
	id := convID(scope)
	sel := ui.Local(ctx, scope+"-selection", ui.TextSelection{})
	quote := func() {
		if s := sel.Peek(); strings.TrimSpace(s.Text) != "" {
			ctx.ClearTextSelection()
			a.addQuote(id, s.Text)
		}
	}
	keys := func(e *ui.KeyEvent) {
		if sel.Peek().Text == "" {
			return
		}
		switch {
		case e.Key == input.KeyEscape && e.Mods == 0:
			e.StopPropagation()
			ctx.ClearTextSelection()
		case e.Key == input.KeyQ && e.Mods == input.ModCtrl|input.ModShift:
			e.StopPropagation()
			quote()
		}
	}
	// Clipped, so the button stays inside the conversation and off the composer.
	return view.ClipContent().OnTextSelect(sel.Set).OnKeyDown(keys).
		Child(ui.Component(func(ctx *ui.Context) ui.Element { return quoteButton(ctx, sel.Get(), quote) }))
}

// quoteButton floats over the end of the selected text, just above its last line (below it when
// there is no room above), and quotes the selection when clicked. A press on it keeps the
// selection's focus, so the selection stays until the click.
func quoteButton(ctx *ui.Context, s ui.TextSelection, quote func()) ui.Element {
	if strings.TrimSpace(s.Text) == "" {
		return ui.Div().Absolute().Size(ui.Px(0), ui.Px(0))
	}
	t := ctx.Theme()
	x := max(s.Last.X+s.Last.W-quoteButtonWidth, space1)
	y := s.Last.Y - quoteButtonHeight - space1
	if y < space1 {
		y = s.Last.Y + s.Last.H + space1
	}
	return ui.Div().Absolute().Left(x).Top(y).ZIndex(2).KeepFocus().
		Size(ui.Px(quoteButtonWidth), ui.Px(quoteButtonHeight)).Radius(radiusS).
		Row().Align(ui.Center).Justify(ui.Center).Gap(space1+2).
		Background(t.Surface).Border(1, t.BorderStrong).Shadow(ui.ShadowMd).
		Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
		Role(access.RoleButton).Label("Quote the selection").Tooltip("Quote in your message (Ctrl+Shift+Q)").
		Key("quote-selection").
		OnClick(quote).
		Child(ui.Icon(icons.Quote).Size(13).Color(t.TextMuted)).
		Child(ui.Text("Quote").FontSize(12).Medium().TextColor(t.Text).NoWrap())
}

// quoteStrip shows the quotes waiting above a composer, each with a button that removes it.
// It is keyed by the composer's key, as the composer's other rows are.
func (a *App) quoteStrip(ctx *ui.Context, key, id string, quotes []string) ui.Element {
	if len(quotes) == 0 {
		return nil
	}
	t := ctx.Theme()
	col := ui.Div().Key(key + "-quotes").Column().Gap(space1).PaddingTop(space1).PaddingBottom(space2)
	for i, q := range quotes {
		idx := i
		col = col.Child(ui.Div().Row().Align(ui.Center).Gap(space2).Key(fmt.Sprintf("quote-%d", i)).
			PaddingLeft(space2).BorderEdges(ui.Edges{Left: 2}, t.BorderStrong).
			Child(ui.Text(q).FontSize(12).TextColor(t.TextMuted).MaxLines(2).Grow(1).MinWidth(ui.Px(0))).
			Child(ui.IconButton(icons.X).Ghost().Small().Label("Remove the quote").Tooltip("Remove").
				OnClick(func() { a.removeQuote(id, idx) })))
	}
	return col
}
