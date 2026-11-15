package core

import (
	"context"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
)

// A turn's start holds across a question: asked, waited on and answered, the turn and its timer carry
// on from when it began, never restarted or stamped later. Only the next turn starts it again.
func TestTurnStartHoldsAcrossQuestion(t *testing.T) {
	e := newEnv(t)
	v := readyWorker(t, e)
	sid := v.SessionID
	view := func() ThreadView { v, _ := e.c.Thread(v.ID); return v }
	check := func(when string, want time.Time) {
		t.Helper()
		got := view()
		if !got.InTurn || !got.TurnStarted.Equal(want) {
			t.Fatalf("%s: in turn %v, started %v, want %v", when, got.InTurn, got.TurnStarted, want)
		}
		if got.TurnStarted.After(time.Now()) {
			t.Fatalf("%s: the turn started in the future: %v", when, got.TurnStarted)
		}
	}

	before := time.Now()
	feed(e.c, sid, agent.Event{Kind: agent.EventTurnStart})
	start := view().TurnStarted
	if start.Before(before) || start.After(time.Now()) {
		t.Fatalf("turn started %v, want between %v and now", start, before)
	}

	answered := make(chan agent.AskResult, 1)
	go func() {
		res, _ := e.c.onAsk(context.Background(), agent.AskParams{SessionID: sid, ToolUseID: "q1",
			Questions: []agent.Question{{Question: "Tabs or spaces?", Options: []agent.QuestionOption{{Label: "Tabs"}, {Label: "Spaces"}}}}})
		answered <- res
	}()
	waitStatusAllowTurn(t, e.c, v.ID, StatusWaiting)
	time.Sleep(20 * time.Millisecond) // the wait is long enough to tell a restamp from the start
	check("waiting on the question", start)

	if err := e.c.Answer(v.ID, "q1", map[string]string{"Tabs or spaces?": "Spaces"}); err != nil {
		t.Fatal(err)
	}
	<-answered
	waitStatusAllowTurn(t, e.c, v.ID, StatusRunning)
	check("answered", start)

	// Claude carries on in the same turn; another turn_start inside it keeps its start.
	feed(e.c, sid, agent.Event{Kind: agent.EventTurnStart})
	check("carrying on", start)

	feed(e.c, sid, agent.Event{Kind: agent.EventTurnEnd, Status: agent.TurnCompleted, Result: "done"})
	feed(e.c, sid, agent.Event{Kind: agent.EventTurnStart})
	if next := view().TurnStarted; next.Before(start) || next.After(time.Now()) {
		t.Fatalf("the next turn started %v, want between %v and now", next, start)
	}
}

// A duration that comes out negative, which a right clock never gives, reads as none.
func TestRoughDurationNeverNegative(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-3 * time.Minute: "0s", -time.Second: "0s", 0: "0s", 40 * time.Second: "40s", 4 * time.Minute: "4m",
		72 * time.Minute: "1h12m",
	} {
		if got := roughDuration(d); got != want {
			t.Errorf("roughDuration(%v) = %q, want %q", d, got, want)
		}
	}
	bg := []BackgroundTask{{ID: "t", Type: "local_bash", Description: "bench", Started: time.Now().Add(time.Minute)}}
	if got := DescribeBackground(bg); got != `command "bench", running 0s` {
		t.Errorf("work started ahead of the clock: %q", got)
	}
}
