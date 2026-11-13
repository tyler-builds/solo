package core

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
)

// statusLog records a thread's status changes, as "prev>status".
type statusLog struct {
	mu  sync.Mutex
	log []string
}

func watchStatuses(t *testing.T, c *Core, id string) *statusLog {
	l := &statusLog{}
	unsub := c.Subscribe(func(ch Change) {
		if ch.Kind == ChangeThread && ch.ThreadID == id && ch.PrevStatus != ch.Status {
			l.mu.Lock()
			l.log = append(l.log, ch.PrevStatus+">"+ch.Status)
			l.mu.Unlock()
		}
	})
	t.Cleanup(unsub)
	return l
}

func (l *statusLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.log, " ")
}

// waitFor waits for the log to say want, and fails the test if it doesn't: a change reaches the log a
// moment after the thread shows it.
func (l *statusLog) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := l.String()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("statuses %q, want %q", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// readyWorker starts a worker that has changed a file, and waits for it to be ready.
func readyWorker(t *testing.T, e *env) ThreadView {
	t.Helper()
	v, err := e.c.CreateWorker(context.Background(), WorkerSpec{Repo: "proj", Title: "Bench", Prompt: "write a.txt hello"})
	if err != nil {
		t.Fatal(err)
	}
	return waitLanding(t, e.c, v.ID)
}

// waitLanding waits until an idle thread's changes are counted, so it can land.
func waitLanding(t *testing.T, c *Core, id string) ThreadView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := waitStatus(t, c, id, StatusIdle)
		if v.CanMerge {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("thread never became landable: changed %d", v.Changed)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A turn that ends with a background command still running leaves the thread waiting on it, not ready:
// nothing lands meanwhile. When the command finishes, Claude's own turn reads it, and only then is the
// thread ready, with no ready moment in between.
func TestBackgroundWorkThenWake(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := readyWorker(t, e)
	log := watchStatuses(t, e.c, v.ID)
	if err := e.c.Send(ctx, v.ID, "bg 600 Run benchmarks"); err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusBackground)
	if len(v.Background) != 1 || v.Background[0].Description != "Run benchmarks" || v.Background[0].Kind() != "command" ||
		v.Background[0].Started.IsZero() {
		t.Fatalf("background: %+v", v.Background)
	}
	if !Active(v.Status) || v.CanMerge || v.CanMergeLocal || v.CanOpenPR || v.CanDone || !v.CanInterrupt {
		t.Fatalf("while waiting: active %v, merge %v/%v, PR %v, done %v, interrupt %v",
			Active(v.Status), v.CanMerge, v.CanMergeLocal, v.CanOpenPR, v.CanDone, v.CanInterrupt)
	}
	if err := e.c.AddProposal(v.ID, "Looks good"); err == nil || !strings.Contains(err.Error(), "background") {
		t.Fatalf("proposing a merge while it waits: %v", err)
	}
	v = waitLanding(t, e.c, v.ID)
	if !strings.HasPrefix(v.Summary, "echo: wake Run benchmarks") || len(v.Background) != 0 {
		t.Fatalf("after the wake: summary %q, background %+v", v.Summary, v.Background)
	}
	log.waitFor(t, "idle>running running>background background>running running>idle")
}

// A command Claude stops itself ends inside its turn: the turn ends ready, and no turn follows.
func TestBackgroundWorkKilledByAgent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := readyWorker(t, e)
	e.c.Send(ctx, v.ID, "bg 60000 Long sleep")
	waitStatus(t, e.c, v.ID, StatusBackground)
	log := watchStatuses(t, e.c, v.ID)
	if err := e.c.Send(ctx, v.ID, "kill"); err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if v.Summary != "stopped 1" {
		t.Fatalf("summary %q", v.Summary)
	}
	if len(v.Background) != 0 {
		t.Fatalf("background after the kill: %+v", v.Background)
	}
	time.Sleep(200 * time.Millisecond)
	log.waitFor(t, "background>running running>idle")
	items, _ := e.c.Items(ctx, v.ID, 0)
	for _, it := range items {
		if it.Kind == ItemTask {
			var d TaskData
			it.Decode(&d)
			if d.Status != "stopped" {
				t.Fatalf("task row %+v", d)
			}
		}
	}
}

// Stop on a thread waiting on background work stops that work; the thread is stopped at once, with no
// turn after.
func TestStopBackgroundWork(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := readyWorker(t, e)
	e.c.Send(ctx, v.ID, "bg 60000 Long sleep")
	waitStatus(t, e.c, v.ID, StatusBackground)
	if err := e.c.Interrupt(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusInterrupted)
	if len(v.Background) != 0 || v.CanInterrupt {
		t.Fatalf("after stopping: background %+v, can interrupt %v", v.Background, v.CanInterrupt)
	}
	time.Sleep(200 * time.Millisecond)
	if v, _ := e.c.Thread(v.ID); v.Status != StatusInterrupted || v.InTurn {
		t.Fatalf("a turn followed the stop: %s", v.Status)
	}
}

// A new effort level restarts the session, which would kill the background work: it waits until the
// work is done and its turn ends.
func TestEffortChangeWaitsForBackgroundWork(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := readyWorker(t, e)
	e.c.Send(ctx, v.ID, "bg 400 Run benchmarks")
	waitStatus(t, e.c, v.ID, StatusBackground)
	if err := e.c.SetThreadModel(ctx, v.ID, "", "max"); err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle, StatusInterrupted)
	if v.Status != StatusIdle || !strings.HasPrefix(v.Summary, "echo: wake Run benchmarks") {
		t.Fatalf("the session closed under its background work: %s, summary %q", v.Status, v.Summary)
	}
}

// Closing Solo stops background work with Claude. The thread is carried on after the restart, told
// which command stopped.
func TestRestartResumesBackgroundWork(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := readyWorker(t, e)
	e.c.Send(ctx, v.ID, "bg 60000 Run benchmarks")
	waitStatus(t, e.c, v.ID, StatusBackground)
	e.c.Close()

	c2 := e.open(t)
	if v, _ := c2.Thread(v.ID); v.Status != StatusInterrupted || len(v.Background) != 0 {
		t.Fatalf("before resuming: %s, background %+v", v.Status, v.Background)
	}
	items, _ := c2.Items(ctx, v.ID, 0)
	for _, it := range items {
		if it.Kind == ItemTask {
			var d TaskData
			it.Decode(&d)
			if d.Status == "running" {
				t.Fatal("the background command still spins after the restart")
			}
		}
	}
	c2.ResumeInterrupted()
	v = waitStatus(t, c2, v.ID, StatusIdle)
	if !strings.Contains(v.Summary, "\n- command: Run benchmarks") {
		t.Fatalf("summary %q", v.Summary)
	}
}

// feed delivers scripted session events to a thread, as the sidecar would.
func feed(c *Core, sid string, events ...agent.Event) {
	for _, e := range events {
		e.SessionID = sid
		c.onEvent(e)
	}
}

func bgEvent(descs ...string) agent.Event {
	e := agent.Event{Kind: agent.EventBackground}
	for _, d := range descs {
		e.Tasks = append(e.Tasks, agent.BackgroundTask{TaskID: "t-" + d, TaskType: "local_bash", Description: d})
	}
	return e
}

// The transitions, from scripted events: a task that finishes waits for Claude's turn, a stopped one
// doesn't, a wake that never comes settles after bgWakeWait, and an exit drops the work.
func TestBackgroundTransitions(t *testing.T) {
	old := bgWakeWait
	bgWakeWait = 150 * time.Millisecond
	t.Cleanup(func() { bgWakeWait = old })
	e := newEnv(t)
	v := readyWorker(t, e)
	sid := v.SessionID
	status := func() ThreadView { v, _ := e.c.Thread(v.ID); return v }
	start := func(descs ...string) {
		feed(e.c, sid, agent.Event{Kind: agent.EventTurnStart}, bgEvent(descs...),
			agent.Event{Kind: agent.EventTurnEnd, Status: agent.TurnCompleted, Result: "waiting"})
		if s := status(); s.Status != StatusBackground || len(s.Background) != len(descs) {
			t.Fatalf("after the turn: %s, %+v", s.Status, s.Background)
		}
	}

	// Finished by itself: it stays waiting for Claude's turn rather than turning ready...
	start("a", "b")
	feed(e.c, sid, agent.Event{Kind: agent.EventTask, Phase: "ended", TaskID: "t-a", Status: "completed"}, bgEvent("b"))
	if s := status(); s.Status != StatusBackground || len(s.Background) != 1 || s.Background[0].Description != "b" {
		t.Fatalf("one of two done: %s, %+v", s.Status, s.Background)
	}
	feed(e.c, sid, agent.Event{Kind: agent.EventTask, Phase: "ended", TaskID: "t-b", Status: "completed"}, bgEvent())
	if s := status(); s.Status != StatusBackground {
		t.Fatalf("both done, before the wake: %s", s.Status)
	}
	// ...and the wake turn makes it ready.
	feed(e.c, sid, agent.Event{Kind: agent.EventTurnStart}, agent.Event{Kind: agent.EventTurnEnd, Status: agent.TurnCompleted, Result: "done"})
	if s := status(); s.Status != StatusIdle {
		t.Fatalf("after the wake: %s", s.Status)
	}
	time.Sleep(2 * bgWakeWait) // the stale wait's timer changes nothing
	if s := status(); s.Status != StatusIdle {
		t.Fatalf("a stale timer moved it: %s", s.Status)
	}

	// A wake that never comes: it settles after bgWakeWait.
	start("c")
	feed(e.c, sid, agent.Event{Kind: agent.EventTask, Phase: "ended", TaskID: "t-c", Status: "failed"}, bgEvent())
	if s := status(); s.Status != StatusBackground {
		t.Fatalf("before the timeout: %s", s.Status)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)

	// Stopped (by stopTask or a kill): no turn follows, so it is ready at once.
	start("d")
	feed(e.c, sid, agent.Event{Kind: agent.EventTask, Phase: "ended", TaskID: "t-d", Status: "stopped"}, bgEvent())
	if s := status(); s.Status != StatusIdle {
		t.Fatalf("after a stop: %s", s.Status)
	}

	// The process exits under it: the work is gone, and the thread is stopped, not ready.
	start("e")
	feed(e.c, sid, agent.Event{Kind: agent.EventExited})
	if s := status(); s.Status != StatusInterrupted || len(s.Background) != 0 {
		t.Fatalf("after the exit: %s, %+v", s.Status, s.Background)
	}

	// A failed turn stays failed, its background work listed.
	feed(e.c, sid, agent.Event{Kind: agent.EventTurnStart}, bgEvent("f"),
		agent.Event{Kind: agent.EventTurnEnd, Status: agent.TurnFailed, Error: "boom"})
	if s := status(); s.Status != StatusFailed || len(s.Background) != 1 {
		t.Fatalf("after a failed turn: %s, %+v", s.Status, s.Background)
	}
}
