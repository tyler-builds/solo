package gui

import (
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"

	"github.com/tyler-builds/solo/internal/core"
)

// waitingOnBackground turns the demo's ready thread into one whose turn ended while its benchmarks
// still run in the background, as core reports it.
func waitingOnBackground(f *fakeBackend) core.ThreadView {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.threads {
		v := &f.threads[i]
		if v.ID != "c3d4e5f6-forms" {
			continue
		}
		v.Status, v.Changed = core.StatusBackground, 6
		v.CanOpenPR, v.CanMerge, v.CanMergeLocal, v.CanDone, v.CanInterrupt = false, false, false, false, true
		v.Summary = "The changelog check passes. Waiting for the benchmarks to finish."
		v.Background = []core.BackgroundTask{{ID: "bj7mb1j7y", Type: "local_bash",
			Description: "Run benchmarks on the finished branch", Started: time.Now().Add(-4*time.Minute - 12*time.Second)}}
		return *v
	}
	panic("no demo thread c3d4e5f6-forms")
}

// A thread waiting on background work is shown at work, not ready: in the rail's Working group with
// the background mark, counted apart in Solo's bar, and with what it waits on above its composer. Its
// header and its merge card agree that it can't land yet, and say why.
func TestBackgroundWorkIsShownAndBlocksLanding(t *testing.T) {
	f := demoBackend()
	th := waitingOnBackground(f)
	a, u := newUI(t, f)
	a.openThread(th.ID, "conversation")
	u.Frame()

	if !u.Get("rail-working").HasRole(access.RoleButton, th.Title) {
		t.Fatal("the thread is not in the rail's Working group")
	}
	if !u.HasRole(access.RoleButton, "1 in background: open the newest") {
		t.Fatal("Solo's bar doesn't count the thread waiting on background work")
	}
	if u.HasRole(access.RoleButton, "1 ready: open the newest") {
		t.Fatal("Solo's bar still counts it ready")
	}
	if line := u.Get("background-line").Text(); !strings.Contains(line, "Waiting on background work · 4m 1") ||
		!strings.Contains(line, "Run benchmarks on the finished branch") {
		t.Fatalf("pinned line %q", line)
	}

	header := u.Get("thread-panel-"+th.ID).ByRole(access.RoleButton, "Open PR")
	card := u.Get("orch-transcript").ByRole(access.RoleButton, "Open PR")
	for name, b := range map[string]interface {
		Disabled() bool
		Node() *access.Node
	}{"header": header, "card": card} {
		if !b.Disabled() {
			t.Errorf("the %s's Open PR is enabled while background work runs", name)
		}
		if d := b.Node().Description; !strings.HasPrefix(d, "Background work is still running: Run benchmarks on the finished branch (4m") {
			t.Errorf("the %s's tooltip: %q", name, d)
		}
	}
	if header.Node().Description != card.Node().Description {
		t.Errorf("header and card disagree:\n%q\n%q", header.Node().Description, card.Node().Description)
	}
}

// Once the work is done and the thread is ready, the header and the card both offer Open PR.
func TestReadyThreadLandsFromHeaderAndCard(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	a.openThread("c3d4e5f6-forms", "conversation")
	u.Frame()
	for name, root := range map[string]string{"header": "thread-panel-c3d4e5f6-forms", "card": "orch-transcript"} {
		b := u.Get(root).ByRole(access.RoleButton, "Open PR")
		if b.Disabled() {
			t.Errorf("the %s's Open PR is disabled for a ready thread", name)
		}
	}
}
