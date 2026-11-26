//go:build live

package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// A steer while a subagent runs: Direct moves the subagent to the background first, the turn ends
// steered, and Claude carries on with the steer.
//
//	go test -tags live -run LiveSteer ./internal/agent -v
func TestLiveSteerBackgroundsSubagent(t *testing.T) {
	var mu sync.Mutex
	var events []Event
	got := make(chan Event, 256)
	d := NewDirect(Callbacks{OnEvent: func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		got <- e
	}}, nil)
	defer d.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sid := NewUUID()
	if err := d.Open(ctx, OpenParams{SessionID: sid, Cwd: t.TempDir(), Model: "haiku", PermissionMode: PermissionBypass,
		ToolPolicy: ToolPolicyAllow, Tools: []string{"Agent", "Bash"}, ClaudePath: FindClaude("").Path, Ephemeral: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.SendUUID(ctx, SendParams{SessionID: sid, UUID: NewUUID(),
		Text: "Use the Agent tool once, in the foreground (run_in_background false), to start a general-purpose subagent whose only job is to run `sleep 45` with Bash and then say done. Wait for its answer. Do nothing else yourself."}); err != nil {
		t.Fatal(err)
	}
	wait := func(what string, ok func(Event) bool) Event {
		t.Helper()
		for {
			select {
			case e := <-got:
				if ok(e) {
					return e
				}
			case <-ctx.Done():
				t.Fatalf("no %s", what)
			}
		}
	}
	call := wait("Agent call", func(e Event) bool {
		if e.Kind == EventTurnEnd {
			t.Fatalf("the turn ended before a foreground subagent ran: %s", e.Result)
		}
		if e.Kind != EventAssistant || !e.IsRoot() {
			return false
		}
		for _, b := range e.Blocks {
			if b.Type == "tool_use" && b.Name == "Agent" {
				return true
			}
		}
		return false
	})
	t.Logf("Agent call: %s", call.Blocks[len(call.Blocks)-1].Input)
	time.Sleep(2 * time.Second) // the subagent is under way
	steerUUID := NewUUID()
	if err := d.SendUUID(ctx, SendParams{SessionID: sid, UUID: steerUUID, Steer: true,
		Text: "Change of plan: don't wait for the subagent. Reply with just the word STEERED."}); err != nil {
		t.Fatal(err)
	}
	steered := wait("steered turn end", func(e Event) bool { return e.Kind == EventTurnEnd })
	if !steered.Steered {
		t.Fatalf("first turn end after the steer isn't steered: %+v", steered)
	}
	end := wait("the steer's turn end", func(e Event) bool { return e.Kind == EventTurnEnd && !e.Steered })
	if end.Status != TurnCompleted || !strings.Contains(strings.ToUpper(end.Result), "STEERED") {
		t.Fatalf("after the steer: %+v", end)
	}
	mu.Lock()
	defer mu.Unlock()
	backgrounded := false
	for _, e := range events {
		if (e.Kind == EventTask && e.Background) || (e.Kind == EventBackground && len(e.Tasks) > 0) {
			backgrounded = true
		}
	}
	if !backgrounded {
		t.Error("the subagent never showed as backgrounded")
	}
	t.Logf("after the steer: %s", end.Result)
}
