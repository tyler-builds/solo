package core

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitThread waits until the thread's view satisfies ok.
func waitThread(t *testing.T, c *Core, id, what string, ok func(ThreadView) bool) ThreadView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, _ := c.Thread(id)
		if ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s: thread is %s, in turn %v, queued %d, summary %q", what, v.Status, v.InTurn, len(v.Queued), v.Summary)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// userTexts lists the thread's user messages, in transcript order, and fails on one marked steered.
func userTexts(t *testing.T, c *Core, id string) []string {
	t.Helper()
	items, _ := c.Items(context.Background(), id, 0)
	var out []string
	for _, it := range items {
		if it.Kind != ItemUser {
			continue
		}
		var d UserData
		it.Decode(&d)
		if d.Steer {
			t.Fatalf("message %q to %s steered", d.Text, id)
		}
		out = append(out, d.Text)
	}
	return out
}

// startSolo starts Solo on a turn that runs until it is stopped.
func startSolo(t *testing.T, c *Core) {
	t.Helper()
	if _, err := c.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background(), OrchestratorID, "slow"); err != nil {
		t.Fatal(err)
	}
	waitThread(t, c, OrchestratorID, "the turn to start", func(v ThreadView) bool { return v.InTurn })
}

// The user's messages to Solo while it works wait for its turn to end, shown as queued meanwhile, and
// then each is read as a turn of its own, in the order sent. Stopping the turn moves on to them. The
// hidden context for Solo's next message goes with the first of them, taken when it is handed over.
func TestSoloQueuesMessagesSentWhileItWorks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	startSolo(t, e.c)

	var mu sync.Mutex
	note, takes := "NOTE", 0
	e.c.SetSoloContext(func() (string, func()) {
		mu.Lock()
		defer mu.Unlock()
		takes++
		n := note
		note = ""
		return n, func() {
			mu.Lock()
			note = n + note
			mu.Unlock()
		}
	})
	for _, text := range []string{"first", "second"} {
		if err := e.c.Send(ctx, OrchestratorID, text); err != nil {
			t.Fatal(err)
		}
	}
	v, _ := e.c.Thread(OrchestratorID)
	if !v.InTurn || len(v.Queued) != 2 || v.Queued[0].Text != "first" || v.Queued[1].Text != "second" {
		t.Fatalf("while Solo works: in turn %v, queued %+v", v.InTurn, v.Queued)
	}
	if got := userTexts(t, e.c, OrchestratorID); len(got) != 1 {
		t.Fatalf("queued messages are in the transcript already: %q", got)
	}
	mu.Lock()
	if note != "NOTE" {
		t.Fatalf("the note was used up by a message that only queued: %q", note)
	}
	mu.Unlock()

	if err := e.c.Interrupt(ctx, OrchestratorID); err != nil {
		t.Fatal(err)
	}
	v = waitThread(t, e.c, OrchestratorID, "both messages to be read", func(v ThreadView) bool {
		return v.Status == StatusIdle && !v.InTurn && len(v.Queued) == 0 && strings.HasPrefix(v.Summary, "echo: second")
	})
	if got := userTexts(t, e.c, OrchestratorID); strings.Join(got, "|") != "slow|first|second" {
		t.Fatalf("messages %q", got)
	}
	items, _ := e.c.Items(ctx, OrchestratorID, 0)
	var turns []string
	for _, it := range items {
		if it.Kind == ItemTurn {
			var d TurnData
			it.Decode(&d)
			turns = append(turns, d.Status)
		}
	}
	if strings.Join(turns, ",") != "interrupted,completed,completed" {
		t.Fatalf("turns %v (%s)", turns, kinds(items))
	}
	mu.Lock()
	defer mu.Unlock()
	if note != "" {
		t.Fatalf("the note never went: %q", note)
	}
}

// The first message's turn carried the note, the second's didn't.
func TestSoloQueuedMessageGetsContextWhenHandedOver(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	startSolo(t, e.c)
	note := ""
	var mu sync.Mutex
	e.c.SetSoloContext(func() (string, func()) {
		mu.Lock()
		defer mu.Unlock()
		n := note
		note = ""
		return n, func() {}
	})
	if err := e.c.Send(ctx, OrchestratorID, "first"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	note = "RECAP" // set after the message queued, as a fresh session would
	mu.Unlock()
	if err := e.c.Interrupt(ctx, OrchestratorID); err != nil {
		t.Fatal(err)
	}
	v := waitThread(t, e.c, OrchestratorID, "the message to be read", func(v ThreadView) bool {
		return v.Status == StatusIdle && len(v.Queued) == 0 && strings.Contains(v.Summary, "first")
	})
	if !strings.HasPrefix(v.Summary, "echo: RECAP\n\n---\n\nfirst") {
		t.Fatalf("summary %q", v.Summary)
	}
	if got := userTexts(t, e.c, OrchestratorID); strings.Join(got, "|") != "slow|first" {
		t.Fatalf("the transcript shows the hidden context: %q", got)
	}
}

// A message sent while Solo waits on its own question isn't held back by the app: as before, it goes
// to Claude (which holds it for after the turn), and the question is still answered in its card.
func TestSoloQuestionStillTakesMessages(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.c.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Send(ctx, OrchestratorID, "ask"); err != nil {
		t.Fatal(err)
	}
	waitThread(t, e.c, OrchestratorID, "the question", func(v ThreadView) bool { return v.PendingAsk })
	if err := e.c.Send(ctx, OrchestratorID, "meanwhile"); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.c.Thread(OrchestratorID); len(v.Queued) != 0 {
		t.Fatalf("a message sent during a question queued: %+v", v.Queued)
	}
	qs := e.c.PendingQuestions(OrchestratorID)
	if len(qs) != 1 {
		t.Fatalf("questions %+v", qs)
	}
	if err := e.c.Answer(OrchestratorID, qs[0].ToolUseID, map[string]string{"Tabs or spaces?": "Tabs"}); err != nil {
		t.Fatal(err)
	}
	waitThread(t, e.c, OrchestratorID, "both turns", func(v ThreadView) bool {
		items, _ := e.c.Items(ctx, OrchestratorID, 0)
		turns := 0
		for _, it := range items {
			if it.Kind == ItemTurn {
				turns++
			}
		}
		return v.Status == StatusIdle && !v.InTurn && !v.PendingAsk && turns == 2
	})
	if got := userTexts(t, e.c, OrchestratorID); strings.Join(got, "|") != "ask|meanwhile" {
		t.Fatalf("messages %q", got)
	}
}

// A message queued for Solo survives a restart: it still shows as waiting, and goes to Solo once, after
// Solo carries on from the restart.
func TestSoloQueuedMessageSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	startSolo(t, e.c)
	if err := e.c.Send(ctx, OrchestratorID, "after the restart"); err != nil {
		t.Fatal(err)
	}
	e.c.Close()

	c2 := e.open(t)
	if v, _ := c2.Thread(OrchestratorID); len(v.Queued) != 1 || v.Queued[0].Text != "after the restart" {
		t.Fatalf("after the restart: queued %+v", v.Queued)
	}
	c2.ResumeInterrupted()
	waitThread(t, c2, OrchestratorID, "the queued message to be read", func(v ThreadView) bool {
		return v.Status == StatusIdle && !v.InTurn && len(v.Queued) == 0 && strings.HasPrefix(v.Summary, "echo: after the restart")
	})
	got := userTexts(t, c2, OrchestratorID)
	if len(got) != 3 || got[0] != "slow" || !strings.HasPrefix(got[1], "Solo was closed") || got[2] != "after the restart" {
		t.Fatalf("messages %q", got)
	}
	c2.Close()

	c3 := e.open(t)
	c3.ResumeInterrupted()
	time.Sleep(200 * time.Millisecond)
	if v, _ := c3.Thread(OrchestratorID); len(v.Queued) != 0 || v.Status != StatusIdle {
		t.Fatalf("after a second restart: %s, queued %+v", v.Status, v.Queued)
	}
	if got := userTexts(t, c3, OrchestratorID); len(got) != 3 {
		t.Fatalf("messages after a second restart %q", got)
	}
}

// With carrying on after a restart off, Solo still gets the message the user queued.
func TestSoloQueuedMessageSentAfterRestartWithoutResume(t *testing.T) {
	e := newEnv(t)
	if err := e.c.UpdateSettings(func(s *Settings) { s.NoResumeAfterRestart = true }); err != nil {
		t.Fatal(err)
	}
	startSolo(t, e.c)
	if err := e.c.Send(context.Background(), OrchestratorID, "still wanted"); err != nil {
		t.Fatal(err)
	}
	e.c.Close()

	c2 := e.open(t)
	c2.ResumeInterrupted()
	waitThread(t, c2, OrchestratorID, "the queued message to be read", func(v ThreadView) bool {
		return v.Status == StatusIdle && len(v.Queued) == 0 && strings.HasPrefix(v.Summary, "echo: still wanted")
	})
	if got := userTexts(t, c2, OrchestratorID); strings.Join(got, "|") != "slow|still wanted" {
		t.Fatalf("messages %q", got)
	}
}
