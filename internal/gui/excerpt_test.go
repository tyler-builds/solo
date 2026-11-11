package gui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
)

func TestThoughtExcerpt(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"empty", "", ""},
		{"whitespace", "  \n\n \t\n", ""},
		{"one finished line", "Check the branch first.\n", "Check the branch first."},
		{"short and unfinished", "Check the bra", ""},
		{"long and unfinished", "Three independent tasks in two repos, so I'll sta", "Three independent tasks in two repos, so I'll"},
		{"unfinished, ends in a space", "Three independent tasks in two repos ", "Three independent tasks in two repos"},
		{"newest paragraph", "First I'll look at the auth module.\n\nThe refresh path drops the retry.\nIt should retry once.", "The refresh path drops the retry."},
		{"newest paragraph is too short yet", "First I'll look at the auth module.\n\nThe refr", "First I'll look at the auth module."},
		{"blank line after", "First thought.\n\nSecond thought.\n\n", "Second thought."},
		{"whitespace-only line separates", "First thought.\n  \t\nSecond thought.\n", "Second thought."},
		{"crlf", "First thought.\r\n\r\nSecond thought.\r\n", "Second thought."},
		{"bold heading", "**Planning the threads**\n\nnot yet", "Planning the threads"},
		{"heading", "## Checking the `refresh.ts` retry\n", "Checking the refresh.ts retry"},
		{"bullet", "- Look at **session.ts** first\n- then the tests", "Look at session.ts first"},
		{"numbered", "1. Spawn a thread for web\n", "Spawn a thread for web"},
		{"quote", "> the user wants it in main\n", "the user wants it in main"},
		{"rule only", "Earlier thought.\n\n---\n", "Earlier thought."},
	} {
		if got := thoughtExcerpt(c.text); got != c.want {
			t.Errorf("%s: thoughtExcerpt(%q) = %q, want %q", c.name, c.text, got, c.want)
		}
	}
}

// Solo's working line shows a line of its newest thought from a recorded turn, after the finished
// thinking block has taken its live block away, as Claude's summarized thinking does.
func TestWorkingLabelShowsRecordedThought(t *testing.T) {
	raw, err := os.ReadFile("../../sidecar/test/fixtures/orchestrator-thinking.events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	thought := ""
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e agent.Event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		if e.Kind == agent.EventAssistant && len(e.Blocks) > 0 && e.Blocks[0].Type == "thinking" {
			thought = e.Blocks[0].Text
		}
	}
	a := &App{}
	v := core.ThreadView{InTurn: true, Thought: thought}
	if got, want := a.workingLabel(v), "The timeout in deploy.yaml is the likely cause, so I'll say that and suggest lowering it."; got != want {
		t.Errorf("workingLabel = %q, want %q", got, want)
	}
	if got := a.workingLabel(core.ThreadView{InTurn: true}); got != "Thinking…" {
		t.Errorf("workingLabel without a thought = %q, want Thinking…", got)
	}
	v.Retry = "Retrying (1/10): overloaded"
	if got := a.workingLabel(v); got != v.Retry {
		t.Errorf("workingLabel during a retry = %q, want %q", got, v.Retry)
	}
}
