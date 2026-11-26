//go:build live

package claudesdk

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// go test -tags live ./claudesdk: runs the real claude. Costs a fraction of a cent.
func TestLiveComplete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	text, err := Complete(ctx, CompleteParams{Model: "haiku", System: "Be terse.", Prompt: "Reply with the single word: pong"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(text), "pong") {
		t.Fatalf("answer %q", text)
	}
}

func TestLiveInterrupt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := Start(ctx, Options{Model: "haiku", Tools: []string{}, SettingSources: []string{}, NoSessionPersistence: true,
		StrictMCPConfig: true, MCPConfig: `{"mcpServers":{}}`, IncludePartialMessages: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Send(UserMessage{UUID: "2b1f7c9e-0f4a-4d0e-9a51-3d6f2c8e7a10", Text: "Count from 1 to 500, one number per line."}); err != nil {
		t.Fatal(err)
	}
	interrupted := false
	for m := range c.Messages() {
		if m.Type == "stream_event" && !interrupted {
			interrupted = true
			if _, err := c.Interrupt(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if r, ok := m.Result(); ok {
			if r.Status() != TurnInterrupted {
				t.Fatalf("status %s (%+v)", r.Status(), r)
			}
			return
		}
	}
	t.Fatalf("no result: %v", c.Err())
}

// An in-process MCP tool and a PreToolUse hook, through the real CLI.
func TestLiveToolsAndHooks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var called, hooked atomic.Bool
	c, err := Start(ctx, Options{
		Model: "haiku", Tools: []string{"Bash"}, SettingSources: []string{}, NoSessionPersistence: true,
		PermissionMode: "bypassPermissions", ExtraArgs: map[string]string{"allow-dangerously-skip-permissions": ""},
		Env: map[string]string{"ENABLE_CLAUDEAI_MCP_SERVERS": "false"}, StrictMCPConfig: true,
		MCPServers: []*MCPServer{{Name: "app", Tools: []Tool{{
			Name: "secret_number", Description: "Returns the secret number.", AlwaysLoad: true, ReadOnly: true,
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Handler: func(context.Context, json.RawMessage) (ToolResult, error) {
				called.Store(true)
				return ToolResult{Text: "4817"}, nil
			},
		}}}},
		Hooks: map[string][]HookMatcher{"PreToolUse": {{Matcher: "Bash", Callback: func(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
			hooked.Store(true)
			return json.RawMessage(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"The shell is off today."}}`), nil
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ask := func(text string) string {
		if err := c.Send(UserMessage{Text: text}); err != nil {
			t.Fatal(err)
		}
		for m := range c.Messages() {
			if r, ok := m.Result(); ok {
				return r.Result
			}
		}
		t.Fatalf("no result: %v", c.Err())
		return ""
	}
	if got := ask("Call the secret_number tool and reply with just the number it returns."); !called.Load() || !strings.Contains(got, "4817") {
		t.Fatalf("tool called %v, answer %q", called.Load(), got)
	}
	got := ask("Run `echo hi` with the Bash tool, then say in one line what happened.")
	if !hooked.Load() {
		t.Fatalf("hook not called; answer %q", got)
	}
	t.Logf("after the denied Bash call: %s", got)
}
