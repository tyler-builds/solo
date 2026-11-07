package agent

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent/agenttest"
)

func TestMain(m *testing.M) {
	if agenttest.Main() {
		return
	}
	os.Exit(m.Run())
}

type recorder struct {
	mu     sync.Mutex
	events []Event
	ch     chan Event
}

func newRecorder() *recorder { return &recorder{ch: make(chan Event, 256)} }

func (r *recorder) on(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	r.ch <- e
}

func (r *recorder) waitFor(t *testing.T, pred func(Event) bool) Event {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case e := <-r.ch:
			if pred(e) {
				return e
			}
		case <-timeout:
			t.Fatal("timed out waiting for event")
		}
	}
}

func newTestManager(t *testing.T, cb Callbacks) (*Manager, *recorder) {
	t.Helper()
	exe, env := agenttest.Command()
	rec := newRecorder()
	cb.OnEvent = rec.on
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m := NewManager(Command{Path: exe, Env: env}, cb, log)
	t.Cleanup(m.Shutdown)
	return m, rec
}

func openSession(t *testing.T, m *Manager) string {
	t.Helper()
	id := NewUUID()
	if err := m.Open(context.Background(), OpenParams{SessionID: id, Cwd: t.TempDir(), PermissionMode: PermissionBypass, ToolPolicy: ToolPolicyAllow}); err != nil {
		t.Fatal(err)
	}
	return id
}

func turnEnd(uuid string) func(Event) bool {
	return func(e Event) bool {
		return e.Kind == EventTurnEnd && len(e.UUIDs) == 1 && e.UUIDs[0] == uuid
	}
}

func TestSendBindsTurnToUUID(t *testing.T) {
	m, rec := newTestManager(t, Callbacks{})
	id := openSession(t, m)
	uuid, err := m.Send(context.Background(), id, "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	e := rec.waitFor(t, turnEnd(uuid))
	if e.Status != TurnCompleted || e.Result != "echo: hello resume=false" {
		t.Fatalf("got %+v", e)
	}
}

func TestToolCallAndAsk(t *testing.T) {
	m, rec := newTestManager(t, Callbacks{
		OnToolCall: func(ctx context.Context, p ToolCallParams) (ToolCallResult, error) {
			return ToolCallResult{Text: "tool:" + p.Name}, nil
		},
		OnAsk: func(ctx context.Context, p AskParams) (AskResult, error) {
			return AskResult{Answers: map[string]string{p.Questions[0].Question: "Spaces"}}, nil
		},
	})
	id := openSession(t, m)
	u1, _ := m.Send(context.Background(), id, "tool list_threads", false)
	if e := rec.waitFor(t, turnEnd(u1)); e.Result != "tool:list_threads" {
		t.Fatalf("tool result %q", e.Result)
	}
	u2, _ := m.Send(context.Background(), id, "ask", false)
	if e := rec.waitFor(t, turnEnd(u2)); e.Result != "Spaces" {
		t.Fatalf("ask result %q", e.Result)
	}
}

func TestCancelledRequestCancelsHandlerContext(t *testing.T) {
	cancelled := make(chan struct{})
	m, rec := newTestManager(t, Callbacks{
		OnAsk: func(ctx context.Context, p AskParams) (AskResult, error) {
			<-ctx.Done()
			close(cancelled)
			return AskResult{}, ctx.Err()
		},
	})
	id := openSession(t, m)
	u, _ := m.Send(context.Background(), id, "cancel", false)
	rec.waitFor(t, turnEnd(u))
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler context was not cancelled")
	}
}

func TestSidecarCrashEndsTurnAndResumesOnNextSend(t *testing.T) {
	m, rec := newTestManager(t, Callbacks{})
	id := openSession(t, m)
	u1, err := m.Send(context.Background(), id, "crash", false)
	if err != nil {
		t.Fatal(err)
	}
	end := rec.waitFor(t, func(e Event) bool { return e.Kind == EventTurnEnd })
	if end.Status != TurnFailed || end.Reason != "process_exited" {
		t.Fatalf("crashed turn ended %+v", end)
	}
	rec.waitFor(t, func(e Event) bool { return e.Kind == EventExited })
	_ = u1

	// The next send starts a new sidecar and reopens the session with resume.
	u2, err := m.Send(context.Background(), id, "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	if e := rec.waitFor(t, turnEnd(u2)); e.Result != "echo: hello resume=true" {
		t.Fatalf("after crash: %q", e.Result)
	}
	m.mu.Lock()
	gen := m.gen
	m.mu.Unlock()
	if gen != 2 {
		t.Fatalf("sidecar generation %d, want 2", gen)
	}
}

func TestNewUUID(t *testing.T) {
	u := NewUUID()
	if len(u) != 36 || u[14] != '4' || !strings.ContainsRune("89ab", rune(u[19])) {
		t.Fatalf("bad uuid %s", u)
	}
}
