package gui

import (
	"strings"
	"testing"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
)

func TestQuoteMessage(t *testing.T) {
	for _, c := range []struct {
		name   string
		quotes []string
		text   string
		want   string
	}{
		{"no quote", nil, "Hello", "Hello"},
		{"one quote", []string{"ready to merge"}, "Merge it?", "> ready to merge\n\nMerge it?"},
		{"lines", []string{"line one\n\nline three  "}, "Why?", "> line one\n>\n> line three\n\nWhy?"},
		{"windows newlines", []string{"a\r\nb"}, "", "> a\n> b"},
		{"two quotes", []string{"first", "second"}, "Both?", "> first\n\n> second\n\nBoth?"},
		{"quote alone", []string{"just this"}, "", "> just this"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := quoteMessage(c.quotes, c.text); got != c.want {
				t.Errorf("quoteMessage(%q, %q) = %q, want %q", c.quotes, c.text, got, c.want)
			}
		})
	}
}

// Quotes show above the composer, each with a button that removes it.
func TestQuoteAttachAndRemove(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	a.addQuote(core.OrchestratorID, "  first quote\n")
	a.addQuote(core.OrchestratorID, "second quote")
	a.addQuote(core.OrchestratorID, "second quote") // quoting the same selection again adds nothing
	a.addQuote(core.OrchestratorID, " \n ")         // nor does a blank one
	u.Frame()
	if got := a.quotesOf(core.OrchestratorID).Peek(); len(got) != 2 || got[0] != "first quote" || got[1] != "second quote" {
		t.Fatalf("quotes %q", got)
	}
	if !u.Has("quote-0") || !u.Has("quote-1") {
		t.Fatal("the quotes don't show above the composer")
	}
	if !u.Get("orch-draft-area").Focused() {
		t.Error("adding a quote didn't focus the composer")
	}
	u.Get("quote-0").ByRole(access.RoleButton, "Remove the quote").Click()
	u.Frame()
	if got := a.quotesOf(core.OrchestratorID).Peek(); len(got) != 1 || got[0] != "second quote" {
		t.Fatalf("after removing the first, quotes %q", got)
	}
	if u.Has("quote-1") {
		t.Error("a removed quote still shows")
	}
	// A thread's quotes are its own.
	if got := a.quotesOf("a1f3c9d2-oauth").Peek(); len(got) != 0 {
		t.Errorf("a thread has Solo's quotes: %q", got)
	}
}

// A quote alone can be sent: the message is the blockquote.
func TestQuoteAloneSends(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	a.addQuote(core.OrchestratorID, "ready to merge")
	u.Frame()
	if u.ByRole(access.RoleButton, "Send").Disabled() {
		t.Fatal("Send is disabled with a quote and no text")
	}
	u.Press("enter")
	waitSent(t, f, "> ready to merge")
	u.Frame()
	if got := a.quotesOf(core.OrchestratorID).Peek(); len(got) != 0 {
		t.Errorf("the quote stayed after sending: %q", got)
	}
}

// byNamePrefix returns the innermost node whose name starts with prefix: the paragraph rather
// than the message around it, where the paragraph has a node of its own.
func byNamePrefix(t *testing.T, u *uitest.UI, prefix string) *uitest.Element {
	t.Helper()
	var name string
	u.Tree().Walk(func(n *access.Node) bool {
		if strings.HasPrefix(n.Name, prefix) {
			name = n.Name
		}
		return true
	})
	if name == "" {
		t.Fatalf("no text starts with %q", prefix)
	}
	return u.ByName(name)
}

// selectSpan drags across width pixels of the first line of the text whose name starts with
// prefix, from its start.
func selectSpan(t *testing.T, u *uitest.UI, prefix string, width float32) {
	t.Helper()
	x, y := textStart(t, u, prefix)
	rt := u.Runtime()
	rt.PointerMove(x, y, 0)
	rt.PointerDown(x, y, input.ButtonLeft, 1, 0)
	rt.PointerMove(x+width, y, 0)
	rt.PointerUp(x+width, y, input.ButtonLeft, 0)
	u.Frame()
}

// textStart is a point in the first word of the text whose name starts with prefix: past its
// row's padding, in the column of at most chatMaxWidth that the row centres.
func textStart(t *testing.T, u *uitest.UI, prefix string) (x, y float32) {
	t.Helper()
	b := byNamePrefix(t, u, prefix).Bounds()
	inner := min(b.W-2*space6, chatMaxWidth)
	return b.X + (b.W-inner)/2 + 4, b.Y + space2 + 8
}

// selectWord double-clicks the first word of the text whose name starts with prefix, which selects
// that word. It returns the text's whole name.
func selectWord(t *testing.T, u *uitest.UI, prefix string) string {
	t.Helper()
	x, y := textStart(t, u, prefix)
	x += 8
	rt := u.Runtime()
	for clicks := 1; clicks <= 2; clicks++ {
		rt.PointerMove(x, y, 0)
		rt.PointerDown(x, y, input.ButtonLeft, clicks, 0)
		rt.PointerUp(x, y, input.ButtonLeft, 0)
		u.Frame()
	}
	return byNamePrefix(t, u, prefix).Name()
}

// selectAndQuote selects a word of the text starting with prefix and clicks the Quote button that
// appears. It returns the quoted word.
func selectAndQuote(t *testing.T, a *App, u *uitest.UI, id, prefix string) string {
	t.Helper()
	if u.Has("quote-selection") {
		t.Fatal("Quote shows with nothing selected")
	}
	whole := selectWord(t, u, prefix)
	if !u.Has("quote-selection") {
		t.Fatal("no Quote button by the selection")
	}
	u.Click("quote-selection")
	u.Frame()
	qs := a.quotesOf(id).Peek()
	if len(qs) != 1 || strings.TrimSpace(qs[0]) == "" || strings.ContainsAny(qs[0], " \n") || !strings.Contains(whole, qs[0]) {
		t.Fatalf("quoted %q, want a word of %q", qs, whole)
	}
	if u.Has("quote-selection") {
		t.Error("the Quote button stayed after quoting")
	}
	return qs[0]
}

// Selecting text in Solo's chat offers Quote; the quote goes before what the user types.
func TestQuoteSelectionInSolosChat(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	u.Settle()
	word := selectAndQuote(t, a, u, core.OrchestratorID, "The settings migration is done")
	area := u.Get("orch-draft-area")
	if !area.Focused() {
		t.Fatal("quoting didn't focus Solo's composer")
	}
	u.Type("Why?")
	u.Press("enter")
	waitSent(t, f, "> "+word+"\n\nWhy?")
}

// Quoting in a thread's panel goes to that thread's composer.
func TestQuoteSelectionInThreadPanel(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	a.openThread("a1f3c9d2-oauth", "conversation")
	u.Settle()
	word := selectAndQuote(t, a, u, "a1f3c9d2-oauth", "The interceptor gives up")
	if got := a.quotesOf(core.OrchestratorID).Peek(); len(got) != 0 {
		t.Fatalf("a thread's quote went to Solo: %q", got)
	}
	if !u.Get("draft-a1f3c9d2-oauth-area").Focused() {
		t.Fatal("quoting didn't focus the thread's composer")
	}
	u.Type("Is this still true?")
	u.Press("enter")
	waitSent(t, f, "> "+word+"\n\nIs this still true?")
}

// Escape puts the selection, and its Quote button, away; Ctrl+Shift+Q quotes from the keyboard.
func TestQuoteKeys(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	u.Settle()
	selectWord(t, u, "The settings migration is done")
	if !u.Has("quote-selection") {
		t.Fatal("no Quote button by the selection")
	}
	u.Press("escape")
	u.Frame()
	if u.Has("quote-selection") {
		t.Fatal("Escape didn't put the Quote button away")
	}
	selectWord(t, u, "The settings migration is done")
	u.Press("ctrl+shift+q")
	u.Frame()
	if got := a.quotesOf(core.OrchestratorID).Peek(); len(got) != 1 {
		t.Fatalf("Ctrl+Shift+Q quoted %q", got)
	}
}
