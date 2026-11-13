package gui

import (
	"strings"
	"testing"

	"github.com/tyler-builds/solo/internal/core"
)

// Solo's chat says what threads reported and what Solo did in sentences, an exchange at a time, in
// place of a line for each update and a folded line of steps.
func TestSoloChatSumsUpUpdatesAndSteps(t *testing.T) {
	a, u := newUI(t, busyBackend())
	u.Settle()
	chat := u.Get("orch-transcript").Text()
	for _, want := range []string{
		// Threads that reported the same thing share a sentence; the others have their own.
		"Dark mode for the docs site and Retry failed webhook deliveries have questions. Load translations lazily opened as PR #91.",
		"Tighten the docs Content-Security-Policy and Backfill ledger entries for March merged into main.",
		// What Solo did for a message, with the thread it acted on.
		"Solo messaged Handle expired magic-link deep links.",
	} {
		if !strings.Contains(chat, want) {
			t.Errorf("Solo's chat doesn't say %q:\n%s", want, chat)
		}
	}
	for _, gone := range []string{"1 step", "has a question", "auto-merge on", "step by Solo"} {
		if strings.Contains(chat, gone) {
			t.Errorf("Solo's chat still shows %q", gone)
		}
	}

	// A summary comes before the card and the reply it led to, and each exchange starts a topic: a
	// message from the user, or the first of the updates since Solo last spoke.
	rows := a.transcriptRows("orch", a.itemsOf(core.OrchestratorID).Get())
	var kinds []string
	for _, r := range rows {
		k := "other"
		switch {
		case r.summary:
			k = "summary"
		case r.item != nil && r.item.Kind == core.ItemUser:
			k = "user"
			var d core.UserData
			r.item.Decode(&d)
			if d.Synthetic {
				t.Errorf("a thread update still has a row of its own: %q", d.Text)
			}
		case r.item != nil && r.item.Kind == core.ItemProposal:
			k = "card"
		case r.item != nil && r.item.Kind == core.ItemAssistant:
			k = "reply"
		case r.steps != nil:
			t.Errorf("steps still have a row of their own (%d steps)", len(r.steps))
		}
		if r.topic {
			k = "| " + k
		}
		kinds = append(kinds, k)
	}
	got := strings.Join(kinds, " ")
	// The end of the busy day: a message and what Solo did, three updates and the reply, two updates
	// and a merge card, then two merges and the reply.
	if want := "| user summary | summary reply | summary card | summary reply"; !strings.HasSuffix(got, want) {
		t.Errorf("rows end\n%s\nwant them to end\n%s", got, want)
	}
	if strings.HasPrefix(got, "|") {
		t.Errorf("the chat's first row has a rule above it: %s", got)
	}
}

// What an update says about one thread, and about several that reported the same.
func TestUpdateWords(t *testing.T) {
	for _, c := range []struct{ what, one, many, word string }{
		{"has a question", "has a question", "have questions", "question"},
		{"finished · Signs every delivery; 96 tests pass", "finished", "finished", "finished"},
		{"failed · terraform validate: Reference to undeclared resource", "failed", "failed", "failed"},
		{"opened PR #91 · auto-merge on", "opened as PR #91", "opened as PR #91", "opened"},
		{"merged into main", "merged into main", "merged into main", "merged"},
		{"PR #40 merged into main", "merged into main", "merged into main", "merged"},
		{"checks failed on PR #57 · ios-build, e2e (android)", "failed its checks on PR #57", "failed their checks on PR #57", "failed"},
		{"checks passed on web PR #3", "passed its checks on web PR #3", "passed their checks on web PR #3", "passed"},
		{"was stopped", "was stopped", "were stopped", ""},
		{"waiting on background work · Run benchmarks", "is waiting on background work", "are waiting on background work", ""},
		{"resolved its merge conflicts", "resolved its merge conflicts", "resolved their merge conflicts", "resolved"},
		{"PR #3 closed without merging", "had PR #3 closed without merging", "had PR #3 closed without merging", ""},
	} {
		one, many := updateWords(c.what)
		if one != c.one || many != c.many {
			t.Errorf("updateWords(%q) = %q, %q; want %q, %q", c.what, one, many, c.one, c.many)
		}
		before, word, after := cutActionWord(one)
		if word != c.word || before+word+after != one {
			t.Errorf("cutActionWord(%q) = %q, %q, %q; want the word %q", one, before, word, after, c.word)
		}
	}
	if _, word, _ := cutActionWord("have questions"); word != "questions" {
		t.Errorf(`the word of "have questions" is %q`, word)
	}
}

// What Solo did, a phrase a kind of step: counted, or with the threads it acted on.
func TestStepPhrases(t *testing.T) {
	f := busyBackend()
	a, _ := newUI(t, f)
	item := func(name string, in map[string]any) *core.Item {
		f.add("scratch", core.ItemTool, tool(name, name, in, ""))
		return &f.items["scratch"][len(f.items["scratch"])-1]
	}
	steps := []*core.Item{
		item("Grep", map[string]any{"pattern": "Deliver"}),
		item("mcp__solo__list_repos", nil),
		item("mcp__solo__spawn_thread", map[string]any{"title": "One"}),
		item("mcp__solo__spawn_thread", map[string]any{"title": "Two"}),
		item("mcp__solo__get_diff", map[string]any{"id": "c5c5c5c5"}),
		item("mcp__solo__get_thread", map[string]any{"id": "c5c5c5c5"}),
		item("mcp__solo__update_todo", map[string]any{"done": true}),
		item("mcp__solo__propose_merge", map[string]any{"id": "c5c5c5c5"}),
	}
	var got []string
	for _, p := range a.stepPhrases(steps) {
		s := p.text
		for _, th := range p.threads {
			s += " [" + th.title + "]"
		}
		got = append(got, s)
	}
	want := []string{"looked through the code", "started 2 threads", "checked [Sign outgoing webhooks with HMAC]", "updated the to-do list"}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("phrases\n%q\nwant\n%q", got, want)
	}
}
