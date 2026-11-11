package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// replies returns the thread's assistant texts and its error notices.
func replies(t *testing.T, c *Core, id string) (said, errs []string) {
	t.Helper()
	items, err := c.Items(context.Background(), id, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		switch it.Kind {
		case ItemAssistant:
			var d TextData
			if it.Decode(&d) == nil {
				said = append(said, d.Text)
			}
		case ItemNotice:
			var n NoticeData
			if it.Decode(&n) == nil && n.Level == "error" {
				errs = append(errs, n.Text)
			}
		}
	}
	return said, errs
}

func saidOnce(said []string, want string) bool {
	n := 0
	for _, s := range said {
		if strings.Contains(s, want) {
			n++
		}
	}
	return n == 1
}

// A message sent while the session is being replaced waits for the new session and goes to it. It used
// to open the old session again just after it closed, and leave the new one marked open though the
// agent never had it, so every later message failed with "agent: unknown session <new id>".
func TestSendDuringRotation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := readyWorker(t, e)
	if err := e.c.Send(ctx, v.ID, "slowclose 600"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
	old := v.SessionID

	rotated := make(chan error, 1)
	go func() { rotated <- e.c.RotateSession(ctx, v.ID) }()
	time.Sleep(150 * time.Millisecond) // the old session is closing
	if err := e.c.SendSynthetic(ctx, v.ID, "update during rotation"); err != nil {
		t.Fatalf("send during rotation: %v", err)
	}
	if err := <-rotated; err != nil {
		t.Fatal(err)
	}
	// Both reach the new session: neither resumes, and the first isn't lost to the old one.
	waitReply(t, e.c, v.ID, "echo: update during rotation resume=false")
	if err := e.c.SendSynthetic(ctx, v.ID, "update after rotation"); err != nil {
		t.Fatalf("send after rotation: %v", err)
	}
	waitReply(t, e.c, v.ID, "echo: update after rotation resume=false")
	if v, _ := e.c.Thread(v.ID); v.SessionID == old {
		t.Fatal("the session was not replaced")
	}
	if said, errs := replies(t, e.c, v.ID); len(errs) > 0 || len(said) != 4 {
		t.Fatalf("replies %q, errors %q", said, errs)
	}
}

// waitReply waits until the thread has said want once.
func waitReply(t *testing.T, c *Core, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		said, errs := replies(t, c, id)
		if len(errs) > 0 {
			t.Fatalf("errors: %q", errs)
		}
		if saidOnce(said, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no single %q in %q", want, said)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A session the agent manager no longer has (closed or dropped behind core's back) is opened again,
// resuming its conversation, and the message delivered, instead of failing with "unknown session".
func TestSendReopensLostSession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := readyWorker(t, e)
	if err := e.c.Agent().Close(ctx, v.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Send(ctx, v.ID, "still there?"); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitReply(t, e.c, v.ID, "echo: still there? resume=true")
}
