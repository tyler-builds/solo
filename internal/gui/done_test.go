package gui

import (
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
)

// A thread whose changes haven't landed can still be marked done, from its panel's menu: Solo asks
// first, saying its branch is kept, and only then marks it done. The rail offers no one-click Done
// for it.
func TestMarkDoneUnlandedAsksFirst(t *testing.T) {
	f := demoBackend()
	const id = "c3d4e5f6-forms"
	for i := range f.threads {
		if f.threads[i].ID == id {
			f.threads[i].CanDone, f.threads[i].Unlanded, f.threads[i].Changed = true, true, 6
		}
	}
	a, u := newUI(t, f)
	a.railQuery.Set("settings page")
	u.Frame()
	if u.HasRole(access.RoleButton, "Mark “Move settings page to new form components” done") {
		t.Fatal("the rail offers one-click Done for unlanded work")
	}
	a.openThread(id, "conversation")
	u.WaitFor("the panel to slide in", func() bool { return a.panelAnim.Peek() == 1 })
	u.Frame()
	u.ByRole(access.RoleButton, "More actions").Click()
	u.Settle()
	u.ByRole(access.RoleMenuItem, "Mark done…").Click()
	u.Frame()
	if c := a.confirm.Peek(); c == nil || !c.safe || c.action != "Mark done" {
		t.Fatalf("no confirmation: %+v", c)
	}
	if th, _ := f.Thread(id); !th.DoneAt.IsZero() {
		t.Fatal("marked done before confirming")
	}
	u.ByRole(access.RoleButton, "Mark done").Click()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if th, _ := f.Thread(id); !th.DoneAt.IsZero() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("not marked done after confirming")
}
