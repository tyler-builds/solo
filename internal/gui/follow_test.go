package gui

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
)

// chatRig drives Solo's chat as the core would: new items, a turn's live text and its working line
// arrive through the signals the app reads.
type chatRig struct {
	t *testing.T
	a *App
	f *fakeBackend
	u *uitest.UI
}

func newChatRig(t *testing.T) *chatRig {
	f := demoBackend()
	longChat(f)
	a, u := newUI(t, f)
	c := &chatRig{t, a, f, u}
	c.settle()
	return c
}

func (c *chatRig) settle() {
	for range 10 {
		c.u.Advance(50 * time.Millisecond)
	}
}

// add appends an item to Solo's transcript and shows it.
func (c *chatRig) add(kind string, data any) {
	c.f.add(core.OrchestratorID, kind, data)
	items, _ := c.f.Items(context.Background(), core.OrchestratorID, 0)
	c.a.itemsOf(core.OrchestratorID).Set(items)
}

// turn changes Solo's thread view (its live text, its working line) and shows it.
func (c *chatRig) turn(fn func(v *core.ThreadView)) {
	c.f.mu.Lock()
	for i := range c.f.threads {
		if c.f.threads[i].ID == core.OrchestratorID {
			fn(&c.f.threads[i])
		}
	}
	c.f.mu.Unlock()
	c.a.threads.Set(c.f.Threads())
}

// atLatest reports whether the chat shows its latest row: the list is at its end and the button
// back to it is gone.
func (c *chatRig) atLatest() bool {
	return c.a.listHandle("orch").AtEnd() && !c.u.Has("to-latest")
}

func (c *chatRig) mustBeAtLatest(what string) {
	c.t.Helper()
	if !c.atLatest() {
		c.t.Fatalf("%s: the chat is not at its latest message (at end %v, button %v)",
			what, c.a.listHandle("orch").AtEnd(), c.u.Has("to-latest"))
	}
}

// Everything that appears at the foot of the chat keeps it at the bottom while it is there.
func TestChatFollowsNewRowsAtTheBottom(t *testing.T) {
	c := newChatRig(t)
	c.mustBeAtLatest("on open")
	steps := []struct {
		what string
		do   func()
	}{
		{"a message to Solo", func() { c.add(core.ItemUser, core.UserData{Text: "Is the OAuth fix merged?"}) }},
		{"the working line", func() { c.turn(func(v *core.ThreadView) { v.InTurn = true }) }},
		{"a step", func() { c.add(core.ItemTool, tool("f1", "Bash", map[string]any{"command": "git log -1"}, "abc123")) }},
		{"more steps", func() {
			for i := range 4 {
				c.add(core.ItemTool, tool(fmt.Sprintf("f2-%d", i), "Read", map[string]any{"file_path": "/wt/auth.go"}, "ok"))
			}
		}},
		{"live text", func() {
			c.turn(func(v *core.ThreadView) { v.Live = []core.LiveBlock{{Text: "Checking the merge."}} })
		}},
		{"Solo's reply", func() {
			c.turn(func(v *core.ThreadView) { v.Live, v.InTurn = nil, false })
			c.add(core.ItemAssistant, core.TextData{Text: strings.Repeat("The fix is merged and the tests pass. ", 12)})
		}},
		{"a thread update", func() {
			c.add(core.ItemUser, core.UserData{Text: `Thread c3d4e5f6 "Validate the signup form" (web) finished its turn. Summary: Done.`})
		}},
	}
	for _, s := range steps {
		s.do()
		c.settle()
		c.mustBeAtLatest("after " + s.what)
	}
}

// A row that grows where it is, as Solo's reply streams in, keeps the chat at the bottom.
func TestChatFollowsARowGrowingInPlace(t *testing.T) {
	c := newChatRig(t)
	c.turn(func(v *core.ThreadView) { v.InTurn = true })
	text := ""
	for i := range 30 {
		text += fmt.Sprintf("Sentence %d of a reply that keeps streaming in, a few words at a time. ", i)
		c.turn(func(v *core.ThreadView) { v.Live = []core.LiveBlock{{Text: text}} })
		c.u.Advance(33 * time.Millisecond)
		if c.u.Has("to-latest") {
			t.Fatalf("the button showed while the reply streamed in (step %d)", i)
		}
	}
	c.settle()
	c.mustBeAtLatest("after the reply streamed in")
}

// A strip appearing over the composer (a thread's question) makes the chat shorter; it stays at
// its latest message.
func TestChatStaysAtTheBottomWhenItsViewShrinks(t *testing.T) {
	c := newChatRig(t)
	pending := maps.Clone(c.a.pending.Peek())
	if len(pending) == 0 {
		t.Fatal("the demo has no pending question")
	}
	c.a.pending.Set(nil)
	c.settle()
	c.mustBeAtLatest("without the question")
	before := c.u.Get("orch-transcript").Bounds().H
	c.a.pending.Set(pending)
	c.settle()
	if after := c.u.Get("orch-transcript").Bounds().H; after >= before {
		t.Fatalf("the question strip didn't shrink the chat (%v to %v)", before, after)
	}
	c.mustBeAtLatest("after the question appeared")
}

// Scrolled up, nothing new moves the chat, and the button stays.
func TestChatScrolledUpDoesNotFollow(t *testing.T) {
	c := newChatRig(t)
	c.u.Get("orch-transcript").Scroll(0, -600)
	c.settle()
	if c.atLatest() {
		t.Fatal("still at the latest message after scrolling up")
	}
	c.add(core.ItemUser, core.UserData{Text: `Thread c3d4e5f6 "Validate the signup form" (web) finished its turn. Summary: Done.`})
	c.turn(func(v *core.ThreadView) { v.InTurn, v.Live = true, []core.LiveBlock{{Text: "Looking at it."}} })
	c.settle()
	c.add(core.ItemTool, tool("s1", "Bash", map[string]any{"command": "go test ./..."}, "ok"))
	c.turn(func(v *core.ThreadView) { v.Live = []core.LiveBlock{{Text: strings.Repeat("Looking at it. ", 40)}} })
	c.settle()
	if c.a.listHandle("orch").AtEnd() || !c.u.Has("to-latest") {
		t.Fatal("new content scrolled a chat the user had scrolled up")
	}
}

// Sending a message takes the chat to the bottom even when it was scrolled up.
func TestChatSendScrollsToTheBottom(t *testing.T) {
	c := newChatRig(t)
	c.u.Get("orch-transcript").Scroll(0, -3000)
	c.settle()
	if c.atLatest() {
		t.Fatal("still at the latest message after scrolling up")
	}
	c.u.Get("orch-draft-area").Type("Merge the signup form")
	c.u.Press("enter")
	c.settle()
	c.mustBeAtLatest("after sending")
	waitSent(t, c.f, "Merge the signup form")
	// The message arrives after the send; the chat follows it in.
	c.add(core.ItemUser, core.UserData{Text: "Merge the signup form"})
	c.settle()
	c.mustBeAtLatest("after the sent message arrived")
}
