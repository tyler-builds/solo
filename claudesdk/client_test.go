package claudesdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake claude: run with CLAUDESDK_FAKE set, it speaks the stream-json
// control protocol on stdin and stdout.
func TestMain(m *testing.M) {
	if mode := os.Getenv("CLAUDESDK_FAKE"); mode != "" {
		fakeCLI(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeCLI(mode string) {
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "Error: not logged in")
		os.Exit(1)
	}
	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 16<<20)
	permitted := make(chan string, 1)
	lines := make(chan map[string]any, 16)
	go func() {
		defer close(lines)
		for in.Scan() {
			var m map[string]any
			if json.Unmarshal(in.Bytes(), &m) != nil {
				continue
			}
			if m["type"] == "control_response" {
				b, _ := json.Marshal(m["response"].(map[string]any)["response"])
				permitted <- string(b)
				continue
			}
			lines <- m
		}
	}()
	pendingUUID := ""
	for m := range lines {
		switch m["type"] {
		case "control_request":
			req := m["request"].(map[string]any)
			resp := map[string]any{}
			switch req["subtype"] {
			case "initialize":
				resp = map[string]any{"models": []any{map[string]any{"value": "haiku"}}, "args": os.Args[1:],
					"entrypoint": os.Getenv("CLAUDE_CODE_ENTRYPOINT"), "claudecode": os.Getenv("CLAUDECODE")}
			case "interrupt":
				out.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": m["request_id"], "response": map[string]any{}}})
				out.Encode(map[string]any{"type": "result", "subtype": "success", "terminal_reason": "aborted_streaming", "user_message_uuids": []string{pendingUUID}})
				continue
			case "set_model":
				if req["model"] == "bogus" {
					out.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": m["request_id"], "error": "unknown model"}})
					continue
				}
			}
			out.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": m["request_id"], "response": resp}})
		case "user":
			uuid, _ := m["uuid"].(string)
			msg := m["message"].(map[string]any)
			text, _ := msg["content"].(string)
			if blocks, ok := msg["content"].([]any); ok {
				var kinds []string
				for _, b := range blocks {
					kinds = append(kinds, b.(map[string]any)["type"].(string))
				}
				text = strings.Join(kinds, ",")
			}
			out.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s1", "model": "haiku"})
			switch text {
			case "hang":
				pendingUUID = uuid
				continue // until interrupted
			case "ask":
				out.Encode(map[string]any{"type": "control_request", "request_id": "cli_1", "request": map[string]any{
					"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "ls"}, "tool_use_id": "tu1"}})
				text = <-permitted
			case "unknown":
				out.Encode(map[string]any{"type": "control_request", "request_id": "cli_2", "request": map[string]any{"subtype": "hook_callback"}})
				text = <-permitted
			}
			out.Encode(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": "m1", "content": []any{map[string]any{"type": "text", "text": text}}}})
			out.Encode(map[string]any{"type": "result", "subtype": "success", "result": text, "session_id": "s1", "user_message_uuids": []string{uuid}, "total_cost_usd": 0.01})
		}
	}
}

func fakeOptions(mode string) Options {
	exe, _ := os.Executable()
	return Options{CLIPath: exe, Env: map[string]string{"CLAUDESDK_FAKE": mode}}
}

func start(t *testing.T, o Options) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// nextResult reads messages until a result, returning it and the assistant texts before it.
func nextResult(t *testing.T, c *Client) (ResultMessage, []Message) {
	t.Helper()
	var seen []Message
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-c.Messages():
			if !ok {
				t.Fatalf("messages closed: %v", c.Err())
			}
			if r, ok := m.Result(); ok {
				return r, seen
			}
			seen = append(seen, m)
		case <-timeout:
			t.Fatal("no result")
		}
	}
}

func TestTurn(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	o := fakeOptions("ok")
	o.Model, o.Resume, o.Tools, o.SettingSources = "haiku", "abc", []string{}, []string{}
	c := start(t, o)

	var init struct {
		Args       []string `json:"args"`
		Entrypoint string   `json:"entrypoint"`
		ClaudeCode string   `json:"claudecode"`
	}
	if err := json.Unmarshal(c.InitResult(), &init); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--model", "--resume=abc", "--setting-sources=", "--input-format"} {
		if !slices.Contains(init.Args, want) {
			t.Errorf("args %v lack %s", init.Args, want)
		}
	}
	if i := slices.Index(init.Args, "--tools"); i < 0 || init.Args[i+1] != "" {
		t.Errorf("args %v: want --tools \"\"", init.Args)
	}
	if init.Entrypoint != DefaultEntrypoint || init.ClaudeCode != "" {
		t.Errorf("entrypoint %q, CLAUDECODE %q", init.Entrypoint, init.ClaudeCode)
	}

	if err := c.Send(UserMessage{UUID: "u1", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	r, seen := nextResult(t, c)
	if r.Status() != TurnCompleted || r.Result != "hello" || !slices.Equal(r.UserMessageUUIDs, []string{"u1"}) {
		t.Fatalf("result %+v", r)
	}
	if len(seen) != 2 || seen[0].Type != "system" || seen[0].Subtype != "init" || seen[1].Type != "assistant" || !seen[1].IsRoot() {
		t.Fatalf("messages %+v", seen)
	}

	// A second turn goes into the same process; images go before the text.
	if err := c.Send(UserMessage{UUID: "u2", Text: "look", Images: []Image{{MediaType: "image/png", Data: "AA=="}}}); err != nil {
		t.Fatal(err)
	}
	if r, _ := nextResult(t, c); r.Result != "image,text" {
		t.Fatalf("second turn %+v", r)
	}
}

func TestPermission(t *testing.T) {
	o := fakeOptions("ok")
	var got PermissionRequest
	o.CanUseTool = func(ctx context.Context, req PermissionRequest) PermissionResult {
		got = req
		return PermissionResult{Message: "no shell", Interrupt: true}
	}
	c := start(t, o)
	if err := c.Send(UserMessage{Text: "ask"}); err != nil {
		t.Fatal(err)
	}
	r, _ := nextResult(t, c)
	if got.ToolName != "Bash" || got.ToolUseID != "tu1" || string(got.Input) != `{"command":"ls"}` {
		t.Errorf("request %+v", got)
	}
	if r.Result != `{"behavior":"deny","interrupt":true,"message":"no shell"}` {
		t.Errorf("answer %s", r.Result)
	}
}

func TestUnsupportedControlRequest(t *testing.T) {
	c := start(t, fakeOptions("ok"))
	_ = c.Send(UserMessage{Text: "unknown"})
	// The fake records the response body; an error response has none.
	if r, _ := nextResult(t, c); r.Result != "null" {
		t.Errorf("answer %s", r.Result)
	}
}

func TestInterrupt(t *testing.T) {
	c := start(t, fakeOptions("ok"))
	_ = c.Send(UserMessage{UUID: "u1", Text: "hang"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := nextResult(t, c); r.Status() != TurnInterrupted || r.UserMessageUUIDs[0] != "u1" {
		t.Fatalf("result %+v", r)
	}
}

func TestControlError(t *testing.T) {
	c := start(t, fakeOptions("ok"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.SetModel(ctx, "haiku"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetModel(ctx, "bogus"); err == nil || err.Error() != "unknown model" {
		t.Fatalf("err %v", err)
	}
}

func TestStartFailureShowsStderr(t *testing.T) {
	_, err := Start(context.Background(), fakeOptions("fail"))
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err %v", err)
	}
}

func TestCloseEndsMessages(t *testing.T) {
	c := start(t, fakeOptions("ok"))
	c.Close()
	select {
	case _, ok := <-c.Messages():
		if ok {
			t.Fatal("message after close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("messages not closed")
	}
	if err := c.Send(UserMessage{Text: "x"}); err != ErrClosed {
		t.Fatalf("send after close: %v", err)
	}
}

func TestStatus(t *testing.T) {
	cases := []struct {
		r    ResultMessage
		want string
	}{
		{ResultMessage{Subtype: "success"}, TurnCompleted},
		{ResultMessage{Subtype: "success", APIErrorStatus: 429}, TurnFailed},
		{ResultMessage{Subtype: "success", TerminalReason: "prompt_too_long"}, TurnFailed},
		{ResultMessage{Subtype: "success", TerminalReason: "aborted_tools"}, TurnInterrupted},
		{ResultMessage{Subtype: "error_during_execution", Errors: []string{"Request was cancelled"}}, TurnCancelled},
		{ResultMessage{Subtype: "error_max_turns"}, TurnFailed},
	}
	for _, c := range cases {
		if got := c.r.Status(); got != c.want {
			t.Errorf("%+v: %s, want %s", c.r, got, c.want)
		}
	}
}

func TestComplete(t *testing.T) {
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	text, err := Complete(ctx, CompleteParams{CLIPath: exe, Prompt: "  pong  ", Env: map[string]string{"CLAUDESDK_FAKE": "ok"}})
	if err != nil || text != "pong" {
		t.Fatalf("%q, %v", text, err)
	}
}
