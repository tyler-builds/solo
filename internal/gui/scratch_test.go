package gui

import (
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/store"
)

// A scratch thread with changes to land doesn't need the user, and reads as finished; a question or a
// failure still needs them.
func TestScratchNeedsUser(t *testing.T) {
	ready := core.ThreadView{Thread: store.Thread{Kind: store.KindWorker, Status: core.StatusIdle}, IsWorker: true,
		HasWorktree: true, Changed: 3, CanMerge: true, CanOpenPR: true, CanDone: true, Unlanded: true, Forge: forge.GitHub}
	withPR := ready
	withPR.CanOpenPR = false
	withPR.PR = &forge.PR{Kind: forge.GitHub, Number: 7, State: forge.StateOpen, Mergeable: true, Checks: forge.ChecksFailing}
	for _, c := range []struct {
		name string
		th   core.ThreadView
	}{{"ready to land", ready}, {"open pull request", withPR}} {
		if !needsUser(c.th) || !asksToLand(c.th) {
			t.Errorf("%s: a normal thread doesn't need the user", c.name)
		}
		c.th.Scratch = true
		if needsUser(c.th) {
			t.Errorf("%s: a scratch thread needs the user", c.name)
		}
		if asksToLand(c.th) {
			t.Errorf("%s: marking a scratch thread done asks first", c.name)
		}
		if s, l := shownStatus(c.th), statusLabel(c.th); s != statusFinished || l != "Finished" {
			t.Errorf("%s: a scratch thread shows %s, %q", c.name, s, l)
		}
	}
	for _, status := range []string{core.StatusWaiting, core.StatusFailed} {
		th := ready
		th.Scratch, th.Status = true, status
		if !needsUser(th) {
			t.Errorf("a scratch thread that is %s doesn't need the user", status)
		}
	}
}

// In the app: a scratch thread isn't counted as needing the user, sits with the finished threads, has
// the rail's one-click Done and a label in its header, and is marked done without asking. Its menu
// turns the mode off, and a normal thread's turns it on.
func TestScratchInTheApp(t *testing.T) {
	f := demoBackend()
	const id = "c3d4e5f6-forms"
	for i := range f.threads {
		if f.threads[i].ID == id {
			f.threads[i].CanDone, f.threads[i].Unlanded, f.threads[i].Changed = true, true, 6
		}
	}
	count := func() int {
		n := 0
		for _, th := range f.threads {
			if inRail(th) && needsUser(th) {
				n++
			}
		}
		return n
	}
	before := count()
	for i := range f.threads {
		if f.threads[i].ID == id {
			f.threads[i].Scratch = true
		}
	}
	a, u := newUI(t, f)
	if got := a.needsUser.Peek(); got != before-1 {
		t.Fatalf("%d threads need the user, want %d", got, before-1)
	}
	if th, _ := f.Thread(id); shownStatus(th) != statusFinished {
		t.Fatalf("the scratch thread shows %s", shownStatus(th))
	}
	// The fake reports no changes; the app reads the thread again when told.
	reread := func(scratch bool) {
		t.Helper()
		waitThread(t, f, id, func(th core.ThreadView) bool { return th.Scratch == scratch })
		a.onChange(core.Change{Kind: core.ChangeThread, ThreadID: id})
		u.WaitFor("the app to read the thread", func() bool { th, _ := a.thread(id); return th.Scratch == scratch })
		u.Settle()
	}
	a.railQuery.Set("settings page")
	u.Settle()
	if !u.HasRole(access.RoleButton, "Mark “Move settings page to new form components” done") {
		t.Fatal("no one-click Done in the rail for a scratch thread")
	}

	a.openThread(id, "conversation")
	u.WaitFor("the panel to slide in", func() bool { return a.panelAnim.Peek() == 1 })
	u.Settle()
	if !u.Has("scratch-label") {
		t.Fatal("no scratch label in the header")
	}
	u.ByRole(access.RoleButton, "More actions").Click()
	u.Settle()
	if u.HasRole(access.RoleMenuItem, "Mark done…") {
		t.Fatal("the menu asks before marking a scratch thread done")
	}
	u.ByRole(access.RoleMenuItem, "Not scratch").Click()
	u.Settle()
	reread(false)
	if u.Has("scratch-label") {
		t.Fatal("the label stays on a normal thread")
	}
	u.ByRole(access.RoleButton, "More actions").Click()
	u.Settle()
	u.ByRole(access.RoleMenuItem, "Mark as scratch").Click()
	u.Settle()
	reread(true)

	th, _ := a.thread(id)
	a.markDone(th)
	u.Frame()
	if c := a.confirm.Peek(); c != nil {
		t.Fatalf("asked before marking a scratch thread done: %+v", c)
	}
	waitThread(t, f, id, func(th core.ThreadView) bool { return !th.DoneAt.IsZero() })
}

func waitThread(t *testing.T, f *fakeBackend, id string, ok func(core.ThreadView) bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if th, _ := f.Thread(id); ok(th) {
			return
		}
	}
	th, _ := f.Thread(id)
	t.Fatalf("thread %+v", th)
}
