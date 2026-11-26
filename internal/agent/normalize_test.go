package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyler-builds/solo/claudesdk"
)

func msgs(lines ...string) []claudesdk.Message {
	var out []claudesdk.Message
	for _, l := range lines {
		var m claudesdk.Message
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			panic(err)
		}
		m.Raw = json.RawMessage(l)
		out = append(out, m)
	}
	return out
}

func TestNormalizerTurn(t *testing.T) {
	var n normalizer
	var events []Event
	for _, m := range msgs(
		`{"type":"system","subtype":"init","model":"claude-opus-5-5","cwd":"/w","tools":["Read"],"claude_code_version":"2.1.332","permissionMode":"default"}`,
		`{"type":"user","uuid":"u1","parent_tool_use_id":null,"message":{"role":"user","content":"hi"}}`,
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":3,"cache_read_input_tokens":1000}}}}`,
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me look"}}}`,
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Let me look"},{"type":"tool_use","id":"tu1","name":"mcp__card__report","input":{"message":"done"}}]}}`,
		`{"type":"user","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"Queued"}]}]}}`,
		`{"type":"system","subtype":"init","model":"claude-opus-5-5"}`,
		`{"type":"result","subtype":"success","result":"Done.","num_turns":2,"total_cost_usd":0.05,"user_message_uuids":["u1"],"usage":{"input_tokens":5,"output_tokens":9}}`,
	) {
		events = append(events, n.push(m)...)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "init,turn_start,text_delta,assistant,tool_result,turn_end" {
		t.Fatalf("kinds %s", got)
	}
	if events[1].UUIDs[0] != "u1" || events[2].MessageID != "m1" || events[2].Text != "Let me look" {
		t.Errorf("start/delta %+v %+v", events[1], events[2])
	}
	if b := events[3].Blocks; len(b) != 3 || b[2].Name != "mcp__card__report" || b[0].Text != "hmm" || !events[3].IsRoot() {
		t.Errorf("assistant %+v", events[3])
	}
	if events[4].ToolUseID != "tu1" || events[4].Text != "Queued" {
		t.Errorf("tool result %+v", events[4])
	}
	end := events[5]
	if end.Status != TurnCompleted || end.Result != "Done." || end.UUIDs[0] != "u1" || end.ContextTokens != 1003 || end.TotalCostUSD != 0.05 {
		t.Errorf("turn end %+v", end)
	}
	if n.turnActive {
		t.Error("turn still open")
	}
}

func TestNormalizerInterruptAndAbort(t *testing.T) {
	var n normalizer
	evs := n.push(msgs(`{"type":"result","subtype":"success","terminal_reason":"aborted_streaming","user_message_uuids":["u2"]}`)[0])
	if len(evs) != 2 || evs[1].Status != TurnInterrupted || evs[1].Reason != "aborted_streaming" {
		t.Fatalf("%+v", evs)
	}
	n.push(msgs(`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m","content":[]}}`)[0])
	if e, ok := n.abortTurn("gone"); !ok || e.Status != TurnFailed || e.Reason != "process_exited" || e.Error != "gone" {
		t.Fatalf("abort %+v %v", e, ok)
	}
	if _, ok := n.abortTurn("again"); ok {
		t.Fatal("aborted a closed turn")
	}
}

func TestSettingsEntrypoint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if got := sessionEntrypoint(); got != entrypoint {
		t.Fatalf("no settings: %q", got)
	}
	settings := "{\n  // the user's\n  \"env\": {\"CLAUDE_CODE_ENTRYPOINT\": \"mine\", /* x */ \"A\": \"//not a comment\",},\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sessionEntrypoint(); got != "mine" {
		t.Fatalf("from settings: %q", got)
	}
}

// The Go normalizer gives what the TypeScript one Solo had before gave for the same recorded session
// (testdata/orchestrator-thinking.events.jsonl is its output for orchestrator-thinking.jsonl).
func TestNormalizerMatchesRecordedEvents(t *testing.T) {
	raw, err := os.ReadFile("testdata/orchestrator-thinking.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var n normalizer
	var got []Event
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m claudesdk.Message
		if json.Unmarshal([]byte(line), &m) != nil || strings.HasPrefix(m.Type, "$") {
			continue
		}
		m.Raw = json.RawMessage(line)
		got = append(got, n.push(m)...)
	}
	wantRaw, err := os.ReadFile("testdata/orchestrator-thinking.events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var want []Event
	for _, line := range strings.Split(strings.TrimSpace(string(wantRaw)), "\n") {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		e.SessionID, e.Seq = "", 0
		want = append(want, e)
	}
	if len(got) != len(want) {
		var g, w []string
		for _, e := range got {
			g = append(g, e.Kind)
		}
		for _, e := range want {
			w = append(w, e.Kind)
		}
		t.Fatalf("%d events, want %d\ngot  %v\nwant %v", len(got), len(want), g, w)
	}
	for i := range want {
		gb, _ := json.Marshal(got[i])
		wb, _ := json.Marshal(want[i])
		if string(gb) != string(wb) {
			t.Errorf("event %d:\ngot  %s\nwant %s", i, gb, wb)
		}
	}
}

// The other recorded sessions: background work that finishes by itself and wakes a turn, background
// work killed, and a whole worker session.
func TestNormalizerRecordedSessions(t *testing.T) {
	for _, c := range []struct {
		file  string
		kinds []string // kinds that must appear
	}{
		{"background-complete.jsonl", []string{"background", "task", "turn_end"}},
		{"background-kill.jsonl", []string{"background", "task", "turn_end"}},
		{"worker-session.jsonl", []string{"init", "turn_start", "assistant", "tool_result", "turn_end"}},
	} {
		raw, err := os.ReadFile("testdata/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		var n normalizer
		seen := map[string]int{}
		ends := 0
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var m claudesdk.Message
			if json.Unmarshal([]byte(line), &m) != nil || strings.HasPrefix(m.Type, "$") {
				continue
			}
			m.Raw = json.RawMessage(line)
			for _, e := range n.push(m) {
				seen[e.Kind]++
				if e.Kind == EventTurnEnd {
					ends++
					if e.Status == "" {
						t.Errorf("%s: a turn end without a status", c.file)
					}
				}
			}
		}
		for _, k := range c.kinds {
			if seen[k] == 0 {
				t.Errorf("%s: no %s event (%v)", c.file, k, seen)
			}
		}
		if seen[EventTurnStart] != ends {
			t.Errorf("%s: %d turn starts, %d turn ends", c.file, seen[EventTurnStart], ends)
		}
	}
}
