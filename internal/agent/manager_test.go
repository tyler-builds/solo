package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as a fake sidecar when SOLO_FAKE_SIDECAR is set. It speaks the real protocol
// with scripted behavior keyed by the message text:
//
//	"tool"   calls tool.call and puts the answer in the turn's result
//	"ask"    calls question.ask and puts the answer in the turn's result
//	"cancel" calls question.ask, then cancels it with $/cancel
//	"crash"  starts a turn and exits
//	other    answers "resume=<bool>" (whether the session was opened with resume)
func TestMain(m *testing.M) {
	if os.Getenv("SOLO_FAKE_SIDECAR") == "1" {
		fakeSidecar()
		return
	}
	os.Exit(m.Run())
}

func fakeSidecar() {
	var wm sync.Mutex
	write := func(v any) {
		b, _ := json.Marshal(v)
		wm.Lock()
		os.Stdout.Write(append(b, '\n'))
		wm.Unlock()
	}
	var mu sync.Mutex
	resumed := map[string]bool{}
	seq := map[string]int{}
	pending := map[int64]chan json.RawMessage{}
	nextID := int64(1000)
	event := func(sid string, body map[string]any) {
		mu.Lock()
		seq[sid]++
		body["sessionId"], body["seq"] = sid, seq[sid]
		mu.Unlock()
		write(map[string]any{"method": "event", "params": body})
	}
	request := func(method string, params any) (int64, chan json.RawMessage) {
		mu.Lock()
		nextID++
		id := nextID
		ch := make(chan json.RawMessage, 1)
		pending[id] = ch
		mu.Unlock()
		write(map[string]any{"id": id, "method": method, "params": params})
		return id, ch
	}

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method == "" && m.ID != nil {
			mu.Lock()
			ch := pending[*m.ID]
			mu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
			continue
		}
		reply := func(v any) { write(map[string]any{"id": *m.ID, "result": v}) }
		switch m.Method {
		case "hello":
			reply(map[string]any{"protocol": ProtocolVersion, "runtime": "fake"})
		case "session.open":
			var p OpenParams
			json.Unmarshal(m.Params, &p)
			mu.Lock()
			resumed[p.SessionID] = p.Resume
			mu.Unlock()
			reply(struct{}{})
		case "session.send":
			var p SendParams
			json.Unmarshal(m.Params, &p)
			reply(struct{}{})
			go func() {
				event(p.SessionID, map[string]any{"kind": "turn_start", "uuids": []string{p.UUID}})
				result := ""
				switch p.Text {
				case "crash":
					os.Exit(3)
				case "tool":
					_, ch := request("tool.call", ToolCallParams{SessionID: p.SessionID, Name: "list_threads", Args: json.RawMessage(`{}`)})
					var r ToolCallResult
					json.Unmarshal(<-ch, &r)
					result = r.Text
				case "ask":
					_, ch := request("question.ask", AskParams{SessionID: p.SessionID, Questions: []Question{{Question: "Tabs?"}}})
					var r AskResult
					json.Unmarshal(<-ch, &r)
					result = r.Answers["Tabs?"]
				case "cancel":
					id, _ := request("question.ask", AskParams{SessionID: p.SessionID, Questions: []Question{{Question: "Q?"}}})
					time.Sleep(50 * time.Millisecond)
					write(map[string]any{"method": "$/cancel", "params": map[string]any{"id": id}})
					result = "cancelled"
				default:
					mu.Lock()
					result = fmt.Sprintf("resume=%v", resumed[p.SessionID])
					mu.Unlock()
				}
				event(p.SessionID, map[string]any{"kind": "text_delta", "messageId": "m1", "text": "hi"})
				event(p.SessionID, map[string]any{"kind": "turn_end", "uuids": []string{p.UUID}, "status": "completed", "result": result})
			}()
		case "session.close", "session.interrupt":
			reply(struct{}{})
		case "shutdown":
			reply(struct{}{})
			os.Exit(0)
		}
	}
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
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	cb.OnEvent = rec.on
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m := NewManager(Command{Path: exe, Env: []string{"SOLO_FAKE_SIDECAR=1"}}, cb, log)
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
	if e.Status != TurnCompleted || e.Result != "resume=false" {
		t.Fatalf("got %+v", e)
	}
}

func TestToolCallAndAsk(t *testing.T) {
	m, rec := newTestManager(t, Callbacks{
		OnToolCall: func(ctx context.Context, p ToolCallParams) (ToolCallResult, error) {
			return ToolCallResult{Text: "tool:" + p.Name}, nil
		},
		OnAsk: func(ctx context.Context, p AskParams) (AskResult, error) {
			return AskResult{Answers: map[string]string{p.Questions[0].Question: "spaces"}}, nil
		},
	})
	id := openSession(t, m)
	u1, _ := m.Send(context.Background(), id, "tool", false)
	if e := rec.waitFor(t, turnEnd(u1)); e.Result != "tool:list_threads" {
		t.Fatalf("tool result %q", e.Result)
	}
	u2, _ := m.Send(context.Background(), id, "ask", false)
	if e := rec.waitFor(t, turnEnd(u2)); e.Result != "spaces" {
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
	if e := rec.waitFor(t, turnEnd(u2)); e.Result != "resume=true" {
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
