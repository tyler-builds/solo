package gui

import (
	"strings"
	"testing"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
)

// The reply Solo ends its turn with, after timing the test suite in the background.
const wakeReply = "**The full suite takes about 3m 50s on main.** The last CI run took 3m 52s; gui (1m 58s) and core " +
	"(54s) are the slow packages. I'm also timing a local run in the background and will say if it differs."

// soloWoken is Solo's chat as core records it when work Solo ran in the background wakes it: a reply
// that ends a turn, then a turn of its own, with no message from the user, that reads the command's
// output, saying narration first if it isn't empty, and ends with last.
func soloWoken(f *fakeBackend, narration, last string) {
	o := core.OrchestratorID
	turn := func() { f.add(o, core.ItemTurn, core.TurnData{Status: agent.TurnCompleted}) }
	f.items[o] = nil
	f.add(o, core.ItemUser, core.UserData{Text: "How long does the full test suite take on main now?"})
	f.add(o, core.ItemThinking, core.TextData{Text: "Time a local run, and check CI meanwhile."})
	f.add(o, core.ItemTool, tool("c1", "Bash", map[string]any{"command": "time go test ./...",
		"description": "Time the full test suite", "run_in_background": true}, "Command running in background with ID: b1"))
	f.add(o, core.ItemTask, core.TaskData{TaskID: "b1", Description: "Time the full test suite", TaskType: "local_bash",
		Status: "completed", ToolUseID: "c1", Background: true})
	f.add(o, core.ItemAssistant, core.TextData{Text: "It runs in the background; meanwhile I'll check the last CI run."})
	f.add(o, core.ItemTool, tool("c2", "Bash", map[string]any{"command": "gh run list --branch main --limit 1"}, "completed success 3m52s"))
	f.add(o, core.ItemAssistant, core.TextData{Text: wakeReply})
	turn()
	// The background command finished: Claude starts a turn to read it.
	f.add(o, core.ItemThinking, core.TextData{Text: "The timing finished."})
	if narration != "" {
		f.add(o, core.ItemAssistant, core.TextData{Text: narration})
	}
	f.add(o, core.ItemTool, tool("c3", "Read", map[string]any{"file_path": "/tmp/claude/tasks/b1.output"}, "real 3m47s"))
	f.add(o, core.ItemAssistant, core.TextData{Text: last})
	turn()
}

// orchKinds names the rows of Solo's chat, as shown: user, summary, reply and so on.
func orchKinds(a *App) []string {
	var kinds []string
	for _, r := range a.transcriptRows("orch", a.itemsOf(core.OrchestratorID).Get()) {
		k := "other"
		switch {
		case r.summary:
			k = "summary"
		case r.steps != nil:
			k = "steps"
		case r.item != nil && r.item.Kind == core.ItemUser:
			k = "user"
		case r.item != nil && r.item.Kind == core.ItemAssistant:
			var d core.TextData
			r.item.Decode(&d)
			k = "reply:" + d.Text
		}
		kinds = append(kinds, k)
	}
	return kinds
}

// Solo replies, then work it ran in the background wakes it, and it says a line with nothing new. Both
// turns' last messages show in full, in order, each after what Solo did in its turn: the later line
// doesn't fold the reply away with the steps.
func TestSoloReplyStaysAfterBackgroundWake(t *testing.T) {
	f := demoBackend()
	const last = "That was the old background task, nothing else."
	soloWoken(f, "", last)
	a, u := newUI(t, f)
	u.Settle()

	got := strings.Join(orchKinds(a), " | ")
	want := "user | summary | reply:" + wakeReply + " | summary | reply:" + last
	if got != want {
		t.Fatalf("rows\n%s\nwant\n%s", got, want)
	}
	chat := u.Get("orch-transcript").Text()
	for _, s := range []string{"The full suite takes about 3m 50s on main.", "will say if it differs.", last} {
		if !strings.Contains(chat, s) {
			t.Errorf("Solo's chat doesn't show %q:\n%s", s, chat)
		}
	}
	// What Solo said between steps still goes with them.
	if strings.Contains(chat, "meanwhile I'll check") {
		t.Errorf("Solo's interim narration has a row of its own:\n%s", chat)
	}
}

// A wake-up with nothing new, answered "(noted)", adds no message: the reply stays Solo's last word,
// and what Solo did on waking, narration included, is only a step.
func TestSoloQuietWakeAddsNoMessage(t *testing.T) {
	f := demoBackend()
	// Narration ahead of the "(noted)" is said between steps, not as the turn's message.
	soloWoken(f, "Let me read its output.", core.Noted)
	a, u := newUI(t, f)
	u.Settle()

	got := strings.Join(orchKinds(a), " | ")
	if want := "user | summary | reply:" + wakeReply + " | summary"; got != want {
		t.Fatalf("rows\n%s\nwant\n%s", got, want)
	}
}

// One turn, as ever: what Solo said between steps folds with them, and its last message shows. A
// steer that pauses a turn isn't its end (core records no turn end for it), so the turn the steer
// carries on keeps folding its narration.
func TestSoloTurnFoldsItsNarration(t *testing.T) {
	f := demoBackend()
	o := core.OrchestratorID
	f.items[o] = nil
	f.add(o, core.ItemUser, core.UserData{Text: "Can you update my local Solo to match main?"})
	f.add(o, core.ItemTool, tool("c1", "Bash", map[string]any{"command": "git status"}, "On branch main"))
	f.add(o, core.ItemAssistant, core.TextData{Text: "It's on main. Let me find the install script."})
	f.add(o, core.ItemTool, tool("c2", "mcp__solo__list_repos", map[string]any{}, "solo"))
	f.add(o, core.ItemAssistant, core.TextData{Text: "Started **Install Solo from main**."})
	f.add(o, core.ItemTurn, core.TurnData{Status: agent.TurnCompleted})
	// A steered turn: the user's message cuts in, and the turn carries on to one end.
	f.add(o, core.ItemUser, core.UserData{Text: "Check the docs first."})
	f.add(o, core.ItemTool, tool("c3", "Grep", map[string]any{"pattern": "install"}, "README.md"))
	f.add(o, core.ItemUser, core.UserData{Text: "Only the README.", Steer: true})
	f.add(o, core.ItemTool, tool("c4", "Read", map[string]any{"file_path": "README.md"}, "…"))
	f.add(o, core.ItemAssistant, core.TextData{Text: "The README says to run make install."})
	f.add(o, core.ItemTool, tool("c5", "Grep", map[string]any{"pattern": "make install"}, "Makefile"))
	f.add(o, core.ItemAssistant, core.TextData{Text: "The README's steps match the Makefile."})
	f.add(o, core.ItemTurn, core.TurnData{Status: agent.TurnCompleted})
	a, u := newUI(t, f)
	u.Settle()

	got := strings.Join(orchKinds(a), " | ")
	want := "user | summary | reply:Started **Install Solo from main**. | user | summary | user | summary | reply:The README's steps match the Makefile."
	if got != want {
		t.Fatalf("rows\n%s\nwant\n%s", got, want)
	}
}
