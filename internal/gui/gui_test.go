package gui

import (
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui/uitest"
)

func newUI(t *testing.T, f *fakeBackend) (*App, *uitest.UI) {
	t.Helper()
	_, dark := Themes()
	a := New(f)
	u := uitest.New(t, a.Root, uitest.Size(1440, 900), uitest.Theme(dark))
	t.Cleanup(u.Close)
	return a, u
}

func waitSent(t *testing.T, f *fakeBackend, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		sent := append([]string(nil), f.sent...)
		f.mu.Unlock()
		for _, s := range sent {
			if s == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%q was not sent", want)
}

func TestEnterSendsToTheOrchestrator(t *testing.T) {
	f := demoBackend()
	_, u := newUI(t, f)
	u.Get("orch-draft-area").Type("Fix the login bug")
	u.Press("enter")
	waitSent(t, f, "Fix the login bug")
}

func TestShiftEnterDoesNotSend(t *testing.T) {
	f := demoBackend()
	_, u := newUI(t, f)
	u.Get("orch-draft-area").Type("line one")
	u.Press("shift+enter")
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 0 {
		t.Fatalf("sent %v", f.sent)
	}
}

func TestCardOpensThreadAndOpenPRButton(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	u.Click("card-c3d4e5f6-forms")
	u.Frame()
	if a.openID.Peek() != "c3d4e5f6-forms" {
		t.Fatalf("drawer shows %q", a.openID.Peek())
	}
	u.ByRole(access.RoleButton, "Open PR").Click()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.opened)
		f.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pull request was opened")
}

func TestQuestionOptionAnswers(t *testing.T) {
	f := demoBackend()
	_, u := newUI(t, f)
	if !u.HasRole(access.RoleButton, "Answer: Redis") {
		t.Fatal("the worker's question is not shown")
	}
}

func TestRailDoneButtonMarksDone(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	u.ByRole(access.RoleButton, "Mark “Find where uploads are logged” done").Click()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if th, _ := f.Thread("a0b1c2d3-audit"); !th.DoneAt.IsZero() {
			if a.openID.Peek() != "" {
				t.Fatalf("the click also opened %q", a.openID.Peek())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("not marked done")
}
