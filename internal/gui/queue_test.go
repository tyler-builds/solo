package gui

import (
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
)

// rowsWith counts the transcript's rows (list items) whose text contains s.
func rowsWith(u *uitest.UI, s string) int {
	n := 0
	u.Tree().Walk(func(nd *access.Node) bool {
		if nd.Role == access.RoleListItem && strings.Contains(nd.Name, s) {
			n++
		}
		return true
	})
	return n
}

func settle(u *uitest.UI) {
	for i := 0; i < 5; i++ {
		u.Advance(100 * time.Millisecond)
	}
	u.Settle()
}

// While Solo works, a message typed to it goes with a plain Send, not Steer, and an empty box still
// offers Stop. A working thread's composer still steers.
func TestSoloComposerSendsWhileWorkerSteers(t *testing.T) {
	f := demoBackend()
	soloWorking(f, nil)
	a, u := newUI(t, f)
	settle(u)
	if buttons(u, "Stop") != 1 || buttons(u, "Steer") != 0 {
		t.Fatalf("empty box while Solo works: %d Stop, %d Steer", buttons(u, "Stop"), buttons(u, "Steer"))
	}
	a.orchDraft.Set("Also, can a thread add a dark mode toggle to Settings?")
	settle(u)
	if buttons(u, "Steer") != 0 || buttons(u, "Send") != 1 || buttons(u, "Stop") != 0 {
		t.Fatalf("typed to Solo while it works: %d Send, %d Steer, %d Stop", buttons(u, "Send"), buttons(u, "Steer"), buttons(u, "Stop"))
	}
	u.Get("orch-draft-area").Focus()
	u.Press("enter")
	settle(u)
	sentWith(t, u, f, "Also, can a thread add a dark mode toggle to Settings?")

	a.openThread("a1f3c9d2-oauth", "conversation")
	settle(u)
	u.Get("draft-a1f3c9d2-oauth-area").Focus()
	u.Type("Use the v2 token endpoint instead")
	settle(u)
	if buttons(u, "Steer") != 1 {
		t.Fatalf("typed to a working thread: %d Steer", buttons(u, "Steer"))
	}
}

// Messages waiting for Solo's turn to end show at the foot of its chat, marked Queued, until Solo is
// handed them; then they are ordinary messages.
func TestSoloQueuedMessagesShowUntilRead(t *testing.T) {
	f := demoBackend()
	soloWorking(f, []core.QueuedMessage{
		{Text: "Also, can a thread add a dark mode toggle to Settings?", At: time.Now().Add(-time.Minute)},
		{Text: "And bump the Go version in CI.", At: time.Now()},
	})
	a, u := newUI(t, f)
	settle(u)
	if n := rowsWith(u, "Queued"); n != 2 {
		t.Fatalf("%d Queued marks, want 2", n)
	}
	if rowsWith(u, "Also, can a thread add a dark mode toggle") == 0 || rowsWith(u, "And bump the Go version") == 0 {
		t.Fatal("the queued messages don't show")
	}

	// Solo finishes and starts on the first: it joins the transcript, and only the second waits.
	f.mu.Lock()
	for i := range f.threads {
		if th := &f.threads[i]; th.ID == core.OrchestratorID {
			th.Queued = th.Queued[1:]
		}
	}
	f.mu.Unlock()
	f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "Also, can a thread add a dark mode toggle to Settings?"})
	a.onChange(core.Change{Kind: core.ChangeThread, ThreadID: core.OrchestratorID})
	a.onChange(core.Change{Kind: core.ChangeItems, ThreadID: core.OrchestratorID})
	u.WaitFor("the app to read Solo again", func() bool { th, _ := a.thread(core.OrchestratorID); return len(th.Queued) == 1 })
	u.WaitFor("the message to join the transcript", func() bool {
		settle(u)
		return rowsWith(u, "Also, can a thread add a dark mode toggle") == 1 && rowsWith(u, "Queued") == 1
	})
	if n := rowsWith(u, "Queued"); n != 1 {
		t.Fatalf("%d Queued marks after Solo took the first, want 1", n)
	}
	if rowsWith(u, "Also, can a thread add a dark mode toggle") != 1 {
		t.Fatal("the message Solo took shows other than once")
	}
}
