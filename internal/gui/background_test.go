package gui

import (
	"slices"
	"strings"
	"sync/atomic"
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
// the background mark, and with what it waits on above its composer. Its header and its merge card
// agree that it can't land yet, and say why.
func TestBackgroundWorkIsShownAndBlocksLanding(t *testing.T) {
	f := demoBackend()
	th := waitingOnBackground(f)
	a, u := newUI(t, f)
	a.openThread(th.ID, "conversation")
	u.Frame()

	if !u.Get("rail-working").HasRole(access.RoleButton, th.Title) {
		t.Fatal("the thread is not in the rail's Working group")
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

// soloOnBackground puts Solo where the user found it: it started a long script in the background, ended
// its turn saying it reports when the script ends, and the script still runs. more adds further tasks
// it runs at the same time.
func soloOnBackground(f *fakeBackend, more ...core.BackgroundTask) {
	o := core.OrchestratorID
	f.add(o, core.ItemUser, core.UserData{Text: "ok, clean the safe ones"})
	run := tool("bg1", "Bash", map[string]any{"command": "./clean-temp.sh", "description": "Clean temp folders", "run_in_background": true},
		"Command running in background with ID: b8x2k4q1z")
	f.add(o, core.ItemTool, run)
	f.add(o, core.ItemAssistant, core.TextData{Text: "**In progress.** The script clears the debug sessions, the npm cache and the old installer copies.\n\nI report the final numbers when the script ends."})
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.threads {
		if th := &f.threads[i]; th.ID == o {
			th.Status, th.InTurn, th.Step, th.Live, th.CanInterrupt = core.StatusBackground, false, nil, nil, true
			th.Background = append([]core.BackgroundTask{{ID: "b8x2k4q1z", Type: "local_bash",
				Description: "Clean temp folders", Started: time.Now().Add(-4*time.Minute - 12*time.Second)}}, more...)
		}
	}
}

// setSolo changes Solo's view in the backend and tells the app, as core does when Solo's state changes.
func setSolo(a *App, f *fakeBackend, change func(v *core.ThreadView)) {
	f.mu.Lock()
	for i := range f.threads {
		if th := &f.threads[i]; th.ID == core.OrchestratorID {
			change(th)
		}
	}
	f.mu.Unlock()
	a.onChange(core.Change{Kind: core.ChangeThread, ThreadID: core.OrchestratorID})
}

// Solo's turn ended while a script it started still runs: the foot of its chat says it waits on the
// script, by name and for how long, and the time counts on. Stop ends that work. When the script
// finishes the line goes, and the chat shows nothing more while Solo is idle.
func TestSoloWaitingOnBackgroundShowsUntilItEnds(t *testing.T) {
	f := demoBackend()
	soloOnBackground(f)
	var skew atomic.Int64 // the machine's clock, moved on by the test
	f.machineNow = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	a, u := newUI(t, f)
	settle(u)

	if !u.Has("solo-state") {
		t.Fatal("no state line in Solo's chat while its script runs in the background")
	}
	if line := u.Get("solo-state").Text(); !strings.Contains(line, "Waiting on Clean temp folders") || !strings.Contains(line, "· 4m 1") {
		t.Fatalf("Solo's state line reads %q", line)
	}
	// The time counts on without anything else changing.
	skew.Store(int64(time.Minute))
	u.WaitFor("the time to count on", func() bool {
		u.Advance(time.Second)
		return strings.Contains(u.Get("solo-state").Text(), "· 5m 1")
	})

	u.Click("solo-state-stop")
	u.WaitFor("Stop to ask Solo's background work to stop", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.interrupted, core.OrchestratorID)
	})

	// The script ends and Solo is idle: the line goes.
	setSolo(a, f, func(v *core.ThreadView) {
		v.Status, v.Background, v.CanInterrupt = core.StatusIdle, nil, false
	})
	u.WaitFor("the state line to go once the work ended", func() bool {
		settle(u)
		return !u.Has("solo-state")
	})
}

// Solo running several things in the background names the oldest and counts the rest; the time is
// the oldest's.
func TestSoloWaitingOnSeveralBackgroundTasks(t *testing.T) {
	f := demoBackend()
	soloOnBackground(f,
		core.BackgroundTask{ID: "a1", Type: "local_agent", Description: "Find large folders", Started: time.Now().Add(-time.Minute)},
		core.BackgroundTask{ID: "a2", Type: "local_bash", Description: "Watch free space", Started: time.Now().Add(-30 * time.Second)})
	_, u := newUI(t, f)
	settle(u)
	if line := u.Get("solo-state").Text(); !strings.Contains(line, "Waiting on Clean temp folders and 2 more") || !strings.Contains(line, "· 4m 1") {
		t.Fatalf("Solo's state line reads %q", line)
	}
}

// In a turn, the foot of Solo's chat says it is working and what on; when the turn ends with work
// left in the background, the same line says it waits on that; when Solo starts its next turn to read
// the result, it says it works again. Idle, there is no line.
func TestSoloStateLineFollowsItsTurn(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	settle(u)
	if u.Has("solo-state") {
		t.Fatalf("a state line while Solo is idle: %q", u.Get("solo-state").Text())
	}

	setSolo(a, f, func(v *core.ThreadView) {
		v.Status, v.InTurn, v.CanInterrupt = core.StatusRunning, true, true
		run := tool("c1", "Bash", map[string]any{"command": "./clean-temp.sh", "description": "Clean temp folders"}, "")
		v.Step = &run
	})
	u.WaitFor("the working line", func() bool {
		settle(u)
		return u.Has("solo-state") && u.Get("solo-state").HasRole(access.RoleProgressBar, "Working")
	})
	if u.Has("solo-state-stop") {
		t.Fatal("the line offers Stop in a turn; the composer's button stops a turn")
	}

	setSolo(a, f, func(v *core.ThreadView) {
		v.Status, v.InTurn, v.Step = core.StatusBackground, false, nil
		v.Background = []core.BackgroundTask{{ID: "b1", Type: "local_bash", Description: "Clean temp folders", Started: time.Now()}}
	})
	u.WaitFor("the line to say Solo waits on its background work", func() bool {
		settle(u)
		return u.Has("solo-state") && strings.Contains(u.Get("solo-state").Text(), "Waiting on Clean temp folders") &&
			u.Has("solo-state-stop")
	})

	setSolo(a, f, func(v *core.ThreadView) {
		v.Status, v.InTurn, v.Background = core.StatusRunning, true, nil
	})
	u.WaitFor("the line to say Solo works again", func() bool {
		settle(u)
		return u.Has("solo-state") && !strings.Contains(u.Get("solo-state").Text(), "Waiting on") && !u.Has("solo-state-stop")
	})
}

// A compact window's bar says the same beside Solo's name, and its chat ends with the line too.
func TestCompactBarShowsSoloWaitingOnBackground(t *testing.T) {
	f := demoBackend()
	soloOnBackground(f)
	_, u := newPhoneUI(t, f)
	settle(u)
	if !u.HasName("Solo") {
		t.Fatal("no Solo in the compact bar")
	}
	if !u.Has("solo-state") || !strings.Contains(u.Get("solo-state").Text(), "Waiting on Clean temp folders") {
		t.Fatal("no state line in Solo's chat in a compact window")
	}
	if line := u.Get("solo-bar-state").Text(); !strings.Contains(line, "Waiting on Clean temp folders") || !strings.Contains(line, "· 4m 1") {
		t.Fatalf("the compact bar reads %q", line)
	}
}
