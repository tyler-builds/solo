package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/agent/agenttest"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/git"
)

func TestMain(m *testing.M) {
	if agenttest.Main() {
		return
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.com",
		"GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.com",
	} {
		os.Setenv(k, v)
	}
	UpdateDelay = 50 * time.Millisecond
	RetryDelay = 100 * time.Millisecond
	os.Exit(m.Run())
}

func setup(t *testing.T) (*core.Core, *Orchestrator) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "web")
	os.MkdirAll(repo, 0o755)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		if _, err := git.Run(context.Background(), repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	exe, vars := agenttest.Command()
	dataDir := t.TempDir()
	c, err := core.New(core.Config{DataDir: dataDir, Agent: agent.Command{Path: exe, Env: vars},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	c.UpdateSettings(func(s *core.Settings) { s.Folders = []string{root} })
	o, err := New(c, dataDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, o
}

func call(t *testing.T, o *Orchestrator, name string, args any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, isErr := o.handleTool(context.Background(), core.OrchestratorID, name, raw)
	if isErr {
		t.Fatalf("%s: %s", name, out)
	}
	return out
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			if dbg != nil {
				dbg()
			}
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var dbg func()

func onlyWorker(c *core.Core) core.ThreadView {
	for _, v := range c.Threads() {
		if v.IsWorker {
			return v
		}
	}
	return core.ThreadView{}
}

func syntheticMessages(c *core.Core) []string {
	items, _ := c.Items(context.Background(), core.OrchestratorID, 0)
	var out []string
	for _, it := range items {
		var d core.UserData
		if it.Kind == core.ItemUser && it.Decode(&d) == nil && d.Synthetic {
			out = append(out, d.Text)
		}
	}
	return out
}

func TestToolsDriveAWorker(t *testing.T) {
	c, o := setup(t)
	if out := call(t, o, "list_repos", map[string]any{}); !strings.Contains(out, "web") || !strings.Contains(out, "branch main") {
		t.Fatalf("list_repos: %s", out)
	}
	out := call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Add readme", "prompt": "write README.md hello"})
	if !strings.Contains(out, `"Add readme"`) {
		t.Fatalf("spawn: %s", out)
	}
	eventually(t, "worker idle", func() bool { v := onlyWorker(c); return v.Status == core.StatusIdle && !v.InTurn })
	w := onlyWorker(c)

	if out := call(t, o, "list_threads", map[string]any{}); !strings.Contains(out, w.ID[:8]) || !strings.Contains(out, "idle") {
		t.Fatalf("list_threads: %s", out)
	}
	got := call(t, o, "get_thread", map[string]any{"id": w.ID[:8]})
	for _, want := range []string{"Status: idle", "README.md (+1 -0)", "wrote README.md", "[Tool Write]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("get_thread lacks %q:\n%s", want, got)
		}
	}
	if d := call(t, o, "get_diff", map[string]any{"id": w.ID}); !strings.Contains(d, "+hello") {
		t.Fatalf("diff: %s", d)
	}
	call(t, o, "propose_merge", map[string]any{"id": w.ID[:8], "summary": "Adds a README."})
	items, _ := c.Items(context.Background(), core.OrchestratorID, 0)
	var proposal core.ProposalData
	for _, it := range items {
		if it.Kind == core.ItemProposal {
			it.Decode(&proposal)
		}
	}
	if proposal.ThreadID != w.ID || proposal.Summary != "Adds a README." {
		t.Fatalf("proposal %+v", proposal)
	}

	// Errors go back to the model, not up the stack.
	if out, isErr := o.handleTool(context.Background(), core.OrchestratorID, "get_thread", []byte(`{"id":"nope"}`)); !isErr {
		t.Fatalf("unknown thread: %s", out)
	}
	if _, isErr := o.handleTool(context.Background(), w.ID, "list_repos", nil); !isErr {
		t.Fatal("a worker used orchestrator tools")
	}
}

func TestAnswerQuestionForWorker(t *testing.T) {
	c, o := setup(t)
	o.SetUpdates(false)
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Q", "prompt": "ask"})
	eventually(t, "question", func() bool { return onlyWorker(c).PendingAsk })
	w := onlyWorker(c)
	if got := call(t, o, "get_thread", map[string]any{"id": w.ID}); !strings.Contains(got, `Tabs or spaces? Options: "Tabs" (t); "Spaces" (s)`) {
		t.Fatalf("get_thread:\n%s", got)
	}
	call(t, o, "answer_question", map[string]any{"id": w.ID, "answers": map[string]string{"Tabs or spaces?": "Tabs"}})
	eventually(t, "worker idle", func() bool { v := onlyWorker(c); return v.Status == core.StatusIdle && !v.InTurn })
	if s := onlyWorker(c).Summary; s != "Tabs" {
		t.Fatalf("summary %q", s)
	}
	// Solo answered it, so it gets no note saying so.
	var notes []string
	if c.State(notesKey, &notes); len(notes) != 0 {
		t.Fatalf("notes after answer_question: %q", notes)
	}
}

// A question the user answers in its card is noted, hidden, for Solo's next message (here the user's),
// without a turn of its own; the update saying the thread asks, not yet delivered, is dropped.
func TestUserAnswerIsNotedForTheNextMessage(t *testing.T) {
	c, o := setup(t)
	o.SetUpdates(false)
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Q", "prompt": "ask"})
	eventually(t, "question", func() bool { return onlyWorker(c).PendingAsk })
	w := onlyWorker(c)
	qs := c.PendingQuestions(w.ID)
	if err := c.Answer(w.ID, qs[0].ToolUseID, map[string]string{"Tabs or spaces?": "Spaces"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker idle", func() bool { v := onlyWorker(c); return v.Status == core.StatusIdle && !v.InTurn })
	want := `[Solo: the user answered Thread ` + w.ID[:8] + ` "Q" (web)'s question themselves, and the thread carries on: "Tabs or spaces?" → "Spaces"]`
	var notes, pending []string
	c.State(notesKey, &notes)
	if len(notes) != 1 || notes[0] != want {
		t.Fatalf("notes %q, want %q", notes, want)
	}
	c.State(pendingKey, &pending)
	for _, l := range pending {
		if strings.Contains(l, "is asking a question") {
			t.Fatalf("pending still says the thread asks: %q", pending)
		}
	}
	if v, _ := c.Thread(core.OrchestratorID); v.HasTranscript {
		t.Fatal("the note started a turn of its own")
	}
	// A restart keeps the note.
	o2, err := New(c, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	o2.SetUpdates(false)
	if !slices.Equal(o2.notes, notes) {
		t.Fatalf("notes after restart %q", o2.notes)
	}

	if err := o.Send(context.Background(), "what next"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "reply", func() bool {
		items, _ := c.Items(context.Background(), core.OrchestratorID, 0)
		for _, it := range items {
			var d core.TextData
			if it.Kind == core.ItemAssistant && it.Decode(&d) == nil && strings.Contains(d.Text, "what next") {
				return strings.Contains(d.Text, want)
			}
		}
		return false
	})
	// The transcript shows the user's words only, and the note is spent.
	items, _ := c.Items(context.Background(), core.OrchestratorID, 0)
	for _, it := range items {
		var d core.UserData
		if it.Kind == core.ItemUser && it.Decode(&d) == nil && d.Text != "what next" {
			t.Fatalf("transcript message %q", d.Text)
		}
	}
	if c.State(notesKey, &notes); len(notes) != 0 {
		t.Fatalf("notes not cleared: %q", notes)
	}
}

// A question the user skips is noted too, and goes with the next thread update when that comes first.
func TestUserSkipIsNotedWithTheNextUpdate(t *testing.T) {
	c, o := setup(t)
	var mu sync.Mutex
	var hiddens []string
	send := o.update
	o.mu.Lock()
	o.update = func(ctx context.Context, id, hidden, text string) error {
		mu.Lock()
		hiddens = append(hiddens, hidden)
		mu.Unlock()
		return send(ctx, id, hidden, text)
	}
	o.mu.Unlock()
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Q", "prompt": "ask"})
	eventually(t, "question", func() bool { return onlyWorker(c).PendingAsk })
	w := onlyWorker(c)
	qs := c.PendingQuestions(w.ID)
	if err := c.Dismiss(w.ID, qs[0].ToolUseID); err != nil {
		t.Fatal(err)
	}
	want := `[Solo: the user skipped Thread ` + w.ID[:8] + ` "Q" (web)'s question without answering it, so the thread carries on with its own judgment. It asked: "Tabs or spaces?"]`
	eventually(t, "update with the note", func() bool {
		var finished bool
		for _, m := range syntheticMessages(c) {
			finished = finished || strings.Contains(m, `"Q" (web) finished its turn. Summary: cancelled`)
		}
		mu.Lock()
		defer mu.Unlock()
		return finished && slices.Contains(hiddens, want)
	})
	for _, m := range syntheticMessages(c) {
		if strings.Contains(m, "skipped") {
			t.Fatalf("the note shows in the transcript: %q", m)
		}
	}
	var notes []string
	if c.State(notesKey, &notes); len(notes) != 0 {
		t.Fatalf("notes not cleared: %q", notes)
	}
}

func TestUpdatesReachTheOrchestrator(t *testing.T) {
	c, o := setup(t)
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Fast", "prompt": "write a.txt x"})
	eventually(t, "thread update", func() bool {
		for _, m := range syntheticMessages(c) {
			if strings.Contains(m, `"Fast" (web) finished its turn. Summary: wrote a.txt`) {
				return true
			}
		}
		return false
	})
	// The fake orchestrator answers the update like any message.
	eventually(t, "orchestrator idle", func() bool { v, _ := c.Thread(core.OrchestratorID); return v.Status == core.StatusIdle && !v.InTurn })
}

func TestUpdatesWaitForTheOrchestratorsTurn(t *testing.T) {
	c, o := setup(t)
	if err := o.Send(context.Background(), "slow"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "orchestrator turn", func() bool { v, _ := c.Thread(core.OrchestratorID); return v.InTurn })
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "W", "prompt": "write b.txt y"})
	eventually(t, "worker idle", func() bool { v := onlyWorker(c); return v.Status == core.StatusIdle && !v.InTurn })
	time.Sleep(300 * time.Millisecond)
	if n := len(syntheticMessages(c)); n != 0 {
		t.Fatalf("%d updates sent during the orchestrator's turn", n)
	}
	if err := c.Interrupt(context.Background(), core.OrchestratorID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "delivered after the turn", func() bool { return len(syntheticMessages(c)) == 1 })
}

func TestLostSessionRecovers(t *testing.T) {
	c, o := setup(t)
	o.SetUpdates(false)
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "L", "prompt": "lose"})
	dbg = func() {
		v := onlyWorker(c)
		t.Logf("status=%s inTurn=%v err=%q summary=%q", v.Status, v.InTurn, v.Error, v.Summary)
		items, _ := c.Items(context.Background(), v.ID, 0)
		for _, it := range items {
			t.Logf("%s %s", it.Kind, it.Data)
		}
	}
	defer func() { dbg = nil }()
	eventually(t, "recovered", func() bool {
		v := onlyWorker(c)
		return v.Status == core.StatusIdle && !v.InTurn && strings.Contains(v.Summary, "Your original task:\nlose")
	})
}

// A folder that isn't in git shows in list_repos and takes a thread that works directly in it, as
// t3code does: its edits land in the folder, and marking it done leaves the folder alone. init_repo
// puts the folder in git.
func TestFolderOutsideGit(t *testing.T) {
	c, o := setup(t)
	vfl := filepath.Join(c.Settings().Folders[0], "vfl")
	os.MkdirAll(vfl, 0o755)
	if err := os.WriteFile(filepath.Join(vfl, "index.html"), []byte("<p>hi</p>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.RescanRepos()
	if out := call(t, o, "list_repos", nil); !strings.Contains(out, "Not in git") || !strings.Contains(out, "- vfl ("+vfl+")") {
		t.Fatalf("list_repos:\n%s", out)
	}
	raw, _ := json.Marshal(map[string]any{"repos": []string{"vfl", "web"}, "title": "Both", "prompt": "write a.txt x"})
	if out, isErr := o.handleTool(context.Background(), core.OrchestratorID, "spawn_thread", raw); !isErr || !strings.Contains(out, "init_repo") {
		t.Fatalf("spawn_thread across vfl and web: %v %s", isErr, out)
	}
	if out := call(t, o, "spawn_thread", map[string]any{"repo": "vfl", "title": "Subs", "prompt": "write a.txt x"}); !strings.Contains(out, "works directly in the folder") {
		t.Fatalf("spawn_thread: %s", out)
	}
	eventually(t, "the thread to finish", func() bool { return onlyWorker(c).Status == core.StatusIdle })
	v := onlyWorker(c)
	if !v.InPlace || v.Dir != vfl || v.HasWorktree || v.Branch != "" || !v.CanDone || v.CanMerge || v.CanOpenPR {
		t.Fatalf("in-place thread view: %+v", v)
	}
	if b, err := os.ReadFile(filepath.Join(vfl, "a.txt")); err != nil || !strings.Contains(string(b), "x") {
		t.Fatalf("the thread's edit isn't in the folder: %q %v", b, err)
	}
	if err := c.Done(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Restore(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Discard(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"index.html", "a.txt"} {
		if _, err := os.Stat(filepath.Join(vfl, f)); err != nil {
			t.Fatalf("%s went away: %v", f, err)
		}
	}
	if out := call(t, o, "init_repo", map[string]any{"folder": "vfl"}); !strings.Contains(out, "is a git repository now") {
		t.Fatalf("init_repo: %s", out)
	}
	if out := call(t, o, "list_repos", nil); strings.Contains(out, "Not in git") || !strings.Contains(out, "vfl ("+vfl+")") {
		t.Fatalf("list_repos after init:\n%s", out)
	}
}

func TestTodoTools(t *testing.T) {
	c, o := setup(t)
	out := call(t, o, "add_todo", map[string]any{"text": "Add a readme"})
	todos := c.Todos(false)
	if len(todos) != 1 || !strings.Contains(out, todos[0].ID) {
		t.Fatalf("add_todo: %s %+v", out, todos)
	}
	id := todos[0].ID
	out = call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Add readme", "prompt": "write README.md hello", "todo": id})
	if !strings.Contains(out, "linked to todo") {
		t.Fatalf("spawn with todo: %s", out)
	}
	w := onlyWorker(c)
	if got := call(t, o, "list_todos", map[string]any{}); !strings.Contains(got, `"Add a readme"`) || !strings.Contains(got, w.ID[:8]) {
		t.Fatalf("list_todos: %s", got)
	}
	call(t, o, "update_todo", map[string]any{"id": id, "text": "Add a README", "done": true})
	if len(c.Todos(false)) != 0 {
		t.Fatalf("still open: %+v", c.Todos(false))
	}
	if got := call(t, o, "list_todos", map[string]any{}); !strings.Contains(got, "(none)") || !strings.Contains(got, `"Add a README", threads`) || !strings.Contains(got, "by you") {
		t.Fatalf("list_todos after done: %s", got)
	}
	call(t, o, "remove_todo", map[string]any{"id": id})
	if len(c.Todos(true)) != 0 {
		t.Fatal("remove_todo left it")
	}
}

func TestUpdatesListOpenTodos(t *testing.T) {
	c, o := setup(t)
	call(t, o, "add_todo", map[string]any{"text": "Decide on Redis"})
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Add readme", "prompt": "write README.md hello"})
	eventually(t, "update with todos", func() bool {
		for _, m := range syntheticMessages(c) {
			if strings.Contains(m, "finished its turn") && strings.Contains(m, `"Decide on Redis"`) {
				return true
			}
		}
		return false
	})
}

func TestMemoryTools(t *testing.T) {
	c, o := setup(t)
	out := call(t, o, "remember", map[string]any{"text": "The user prefers squash merges.", "keywords": "merge method pull request"})
	if !strings.Contains(out, "#1") {
		t.Fatalf("remember: %s", out)
	}
	if p := systemPrompt(c); !strings.Contains(p, "#1") || !strings.Contains(p, "squash merges") {
		t.Fatalf("memory missing from the system prompt:\n%s", p)
	}
	if out := call(t, o, "remember", map[string]any{"text": "The user prefers squash merges for pull requests.", "keywords": "merge"}); !strings.Contains(out, "Similar") || !strings.Contains(out, "#1") {
		t.Fatalf("near duplicate not flagged: %s", out)
	}
	if _, isErr := o.handleTool(context.Background(), core.OrchestratorID, "remember", []byte(`{"text":"`+strings.Repeat("x", core.MaxMemoryLen+1)+`","keywords":""}`)); !isErr {
		t.Fatal("saved an overlong memory")
	}

	// recall finds memories by their keywords and stems, and the earlier conversation.
	if err := o.Send(context.Background(), "Our deploy window is Fridays after 4pm."); err != nil {
		t.Fatal(err)
	}
	eventually(t, "reply", func() bool {
		v, _ := c.Thread(core.OrchestratorID)
		return !v.InTurn && strings.HasPrefix(v.Summary, "echo:")
	})
	got := call(t, o, "recall", map[string]any{"queries": []string{"merging strategy", "how should PRs land"}})
	if !strings.Contains(got, "#1") || !strings.Contains(got, "#2") {
		t.Fatalf("recall memories: %s", got)
	}
	if got := call(t, o, "recall", map[string]any{"queries": []string{"deploy schedule friday"}}); !strings.Contains(got, "Earlier conversation") || !strings.Contains(got, "User:") || strings.Contains(got, "Threads:") {
		t.Fatalf("recall conversation: %s", got)
	}
	// The third search in one turn is refused.
	if out, isErr := o.handleTool(context.Background(), core.OrchestratorID, "recall", []byte(`{"queries":["deploy"]}`)); !isErr {
		t.Fatalf("third recall allowed: %s", out)
	}
	call(t, o, "forget", map[string]any{"memory_id": 1})
	if p := systemPrompt(c); strings.Contains(p, "#1 ") {
		t.Fatal("forgotten memory still in the prompt")
	}
}

func TestFreshSessionCarriesARecap(t *testing.T) {
	c, o := setup(t)
	o.mu.Lock()
	o.rotateAt = 500 // the fake reports 1000 tokens of context a turn
	o.mu.Unlock()
	call(t, o, "add_todo", map[string]any{"text": "Decide on Redis"})
	before, _ := c.Thread(core.OrchestratorID)
	if err := o.Send(context.Background(), "hello there"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "fresh session", func() bool {
		v, _ := c.Thread(core.OrchestratorID)
		var carry string
		return v.SessionID != before.SessionID && !v.InTurn && c.State(carryKey, &carry) && carry != ""
	})
	var carry string
	c.State(carryKey, &carry)
	for _, want := range []string{"fresh Claude session", "Decide on Redis", "User: hello there"} {
		if !strings.Contains(carry, want) {
			t.Fatalf("recap lacks %q:\n%s", want, carry)
		}
	}
	// A restart keeps the recap for the next message.
	o2, err := New(c, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if o2.carry != carry {
		t.Fatal("recap lost on restart")
	}
	o.mu.Lock()
	o.rotateAt = 1 << 30
	o.mu.Unlock()
	if err := o.Send(context.Background(), "next one"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "reply", func() bool {
		items, _ := c.Items(context.Background(), core.OrchestratorID, 0)
		for _, it := range items {
			var d core.TextData
			if it.Kind == core.ItemAssistant && it.Decode(&d) == nil && strings.Contains(d.Text, "next one") {
				return strings.Contains(d.Text, "fresh Claude session")
			}
		}
		return false
	})
	// The transcript shows the user's words only, with no notice of the fresh session, and the recap
	// is spent.
	items, _ := c.Items(context.Background(), core.OrchestratorID, 0)
	var last core.UserData
	notices := 0
	for _, it := range items {
		switch it.Kind {
		case core.ItemUser:
			it.Decode(&last)
		case core.ItemNotice:
			notices++
		}
	}
	if last.Text != "next one" || last.Synthetic || notices != 0 {
		t.Fatalf("last message %+v, %d notices", last, notices)
	}
	if c.State(carryKey, &carry); carry != "" {
		t.Fatalf("recap not cleared: %q", carry)
	}
}

// Solo's own session runs any Bash command: the sidecar's allow policy approves every request, with no
// read-only allowlist. Code changes still go to workers by process, so Edit and Write aren't offered.
func TestOrchestratorRunsAnyBash(t *testing.T) {
	_, o := setup(t)
	p := o.openParams()
	if p.ToolPolicy != agent.ToolPolicyAllow {
		t.Fatalf("tool policy = %q, want %q", p.ToolPolicy, agent.ToolPolicyAllow)
	}
	if len(p.BashAllow) != 0 {
		t.Fatalf("bash allowlist = %q, want none", p.BashAllow)
	}
	if !slices.Contains(p.Tools, "Bash") {
		t.Fatalf("tools %q lack Bash", p.Tools)
	}
}

// A thread whose turn ends with background work still running is reported as not finished, in its
// update and in get_thread and list_threads; it is reported finished after the turn that reads the work.
func TestUpdatesSayBackgroundWorkIsNotFinished(t *testing.T) {
	c, o := setup(t)
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Bench", "prompt": "bg 1500 Run benchmarks"})
	eventually(t, "waiting on background work", func() bool { return onlyWorker(c).Status == core.StatusBackground })
	w := onlyWorker(c)
	if got := call(t, o, "get_thread", map[string]any{"id": w.ID}); !strings.Contains(got, `Status: background (not finished: its turn ended, but it waits on work it runs in the background, command "Run benchmarks", running`) {
		t.Fatalf("get_thread:\n%s", got)
	}
	if got := call(t, o, "list_threads", map[string]any{}); !strings.Contains(got, `"Bench" in web: background (not finished`) {
		t.Fatalf("list_threads:\n%s", got)
	}
	eventually(t, "background update", func() bool {
		for _, m := range syntheticMessages(c) {
			if strings.Contains(m, `"Bench" (web) ended its turn but isn't finished: it waits on work it runs in the background (command "Run benchmarks", running`) {
				return !strings.Contains(m, "finished its turn")
			}
		}
		return false
	})
	eventually(t, "finished update", func() bool {
		for _, m := range syntheticMessages(c) {
			if strings.Contains(m, `"Bench" (web) finished its turn. Summary: echo: wake Run benchmarks`) {
				return true
			}
		}
		return false
	})
}

// An update that fails to reach Solo is tried again, with the ones that came in meanwhile; updates not
// yet delivered are saved, so a restart delivers them instead of losing them.
func TestUndeliveredUpdatesAreKept(t *testing.T) {
	c, o := setup(t)
	var mu sync.Mutex
	fails := 2
	send := o.update
	o.mu.Lock()
	o.update = func(ctx context.Context, id, hidden, text string) error {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return errors.New("agent: unknown session")
		}
		return send(ctx, id, hidden, text)
	}
	o.mu.Unlock()
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Retry", "prompt": "write a.txt x"})
	eventually(t, "delivered on retry", func() bool {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			return false
		}
		var saved []string
		c.State(pendingKey, &saved)
		v, _ := c.Thread(core.OrchestratorID)
		return len(saved) == 0 && v.HasTranscript && !v.InTurn
	})
	ms := syntheticMessages(c)
	if last := ms[len(ms)-1]; !strings.Contains(last, `"Retry" (web) finished its turn`) {
		t.Fatalf("last update: %q", last)
	}

	// Updates held back (here by pausing them) survive a restart and go out after it.
	o.SetUpdates(false)
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Held", "prompt": "write b.txt y"})
	eventually(t, "update saved", func() bool {
		var saved []string
		return c.State(pendingKey, &saved) && len(saved) == 1 && strings.Contains(saved[0], `"Held" (web) finished its turn`)
	})
	if _, err := New(c, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "delivered after restart", func() bool {
		ms := syntheticMessages(c)
		var saved []string
		c.State(pendingKey, &saved)
		return strings.Contains(ms[len(ms)-1], `"Held" (web) finished its turn`) && len(saved) == 0
	})
}

// reopen_thread brings a done thread back on its kept branch when its work hadn't landed, and on a new
// branch from the base when it had none; list_threads and get_thread find done threads first.
func TestReopenThread(t *testing.T) {
	c, o := setup(t)
	o.SetUpdates(false)
	ctx := context.Background()
	repo := filepath.Join(c.Settings().Folders[0], "web")
	thread := func(id string) core.ThreadView { v, _ := c.Thread(id); return v }
	settled := func(title, prompt string) core.ThreadView {
		t.Helper()
		out := call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": title, "prompt": prompt})
		var id string
		for _, v := range c.Threads() {
			if v.IsWorker && v.Title == title {
				id = v.ID
			}
		}
		if id == "" {
			t.Fatalf("spawn: %s", out)
		}
		eventually(t, title+" settled", func() bool {
			v := thread(id)
			return v.Status == core.StatusIdle && !v.InTurn && v.Changed >= 0
		})
		return thread(id)
	}

	kept := settled("Unlanded", "write a.txt x")
	if !kept.Unlanded {
		t.Fatalf("not unlanded: %+v", kept)
	}
	if out, isErr := o.handleTool(ctx, core.OrchestratorID, "reopen_thread", []byte(`{"id":"`+kept.ID[:8]+`"}`)); !isErr || !strings.Contains(out, "isn't done") {
		t.Fatalf("reopened a thread in the list: %v %s", isErr, out)
	}
	if err := c.Done(ctx, kept.ID); err != nil {
		t.Fatal(err)
	}
	if out := call(t, o, "list_threads", map[string]any{}); strings.Contains(out, kept.ID[:8]) {
		t.Fatalf("done thread listed as open:\n%s", out)
	}
	if out := call(t, o, "list_threads", map[string]any{"include_closed": true}); !strings.Contains(out, kept.ID[:8]) || !strings.Contains(out, "marked it done") {
		t.Fatalf("done thread not listed with include_closed:\n%s", out)
	}
	if out := call(t, o, "get_thread", map[string]any{"id": kept.ID[:8]}); !strings.Contains(out, "reopen_thread brings it back: Its branch "+kept.Branch+" was kept") {
		t.Fatalf("get_thread on a done thread:\n%s", out)
	}
	out := call(t, o, "reopen_thread", map[string]any{"id": kept.ID[:8]})
	if !strings.Contains(out, "back in the user's list") || !strings.Contains(out, "kept branch "+kept.Branch) {
		t.Fatalf("reopen kept: %s", out)
	}
	v := thread(kept.ID)
	if v.Status != core.StatusIdle || !v.DoneAt.IsZero() || !v.HasWorktree {
		t.Fatalf("reopened %+v", v)
	}
	if b, err := os.ReadFile(filepath.Join(v.WorktreePath, "a.txt")); err != nil || !strings.Contains(string(b), "x") {
		t.Fatalf("reopened without its work: %q %v", b, err)
	}

	none := settled("Nothing", "just answer")
	if none.Unlanded {
		t.Fatalf("unlanded: %+v", none)
	}
	if err := c.Done(ctx, none.ID); err != nil {
		t.Fatal(err)
	}
	if localBranch(ctx, repo, none.Branch) {
		t.Fatal("done kept a branch with no work")
	}
	out = call(t, o, "reopen_thread", map[string]any{"id": none.ID})
	if !strings.Contains(out, "was gone (it had no work left to land") || !strings.Contains(out, "new branch "+none.Branch+" from main") {
		t.Fatalf("reopen without a branch: %s", out)
	}
	if v := thread(none.ID); v.Status != core.StatusIdle || !v.DoneAt.IsZero() || !localBranch(ctx, repo, none.Branch) {
		t.Fatalf("reopened %+v", v)
	}

	// Solo knows what it reopened; the user restoring one from History is noted for it, as no turn
	// finished.
	var notes, pending []string
	if c.State(notesKey, &notes); len(notes) != 0 {
		t.Fatalf("notes after reopen_thread: %q", notes)
	}
	if err := c.Done(ctx, none.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Restore(ctx, none.ID); err != nil {
		t.Fatal(err)
	}
	c.State(notesKey, &notes)
	if len(notes) != 1 || !strings.Contains(notes[0], "the user restored Thread "+none.ID[:8]) {
		t.Fatalf("notes after the user's restore: %q", notes)
	}
	c.State(pendingKey, &pending)
	finished := 0 // its one real turn
	for _, l := range pending {
		if strings.Contains(l, "finished its turn") && strings.Contains(l, none.ID[:8]) {
			finished++
		}
	}
	if finished != 1 {
		t.Fatalf("restores reported as finished turns: %q", pending)
	}
}

// list_threads caps closed threads (newest first) but never open ones, and says how many it left out.
func TestListThreadsCapsClosedThreads(t *testing.T) {
	c, o := setup(t)
	o.SetUpdates(false)
	ctx := context.Background()
	var ids []string
	for _, title := range []string{"Old", "New", "Open"} {
		call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": title, "prompt": "just answer"})
		eventually(t, title+" idle", func() bool {
			for _, v := range c.Threads() {
				if v.IsWorker && v.Title == title && v.Status == core.StatusIdle && !v.InTurn && v.Changed >= 0 {
					ids = append(ids, v.ID)
					return true
				}
			}
			return false
		})
	}
	for _, id := range ids[:2] {
		if err := c.Done(ctx, id); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond) // "New" closes after "Old"
	}
	out := call(t, o, "list_threads", map[string]any{"include_closed": true, "limit": 1})
	if !strings.Contains(out, ids[2][:8]) || !strings.Contains(out, ids[1][:8]) || strings.Contains(out, ids[0][:8]) {
		t.Fatalf("limit 1:\n%s", out)
	}
	if !strings.HasSuffix(out, "1 older closed thread(s) not shown; pass a larger limit (up to 100) or days to see them.\n") {
		t.Fatalf("no omitted line:\n%s", out)
	}
	if out := call(t, o, "list_threads", map[string]any{"include_closed": true}); !strings.Contains(out, ids[0][:8]) || strings.Contains(out, "not shown") {
		t.Fatalf("default:\n%s", out)
	}
}

func TestRecentClosed(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	var closed []core.ThreadView
	for i := range 150 { // one an hour, newest first: i hours ago
		v := core.ThreadView{}
		v.ID = fmt.Sprintf("t%03d", i)
		v.DoneAt = now.Add(-time.Duration(i) * time.Hour)
		closed = append(closed, v)
	}
	old := core.ThreadView{}
	old.ID, old.Status, old.UpdatedAt = "old", core.StatusMerged, now.AddDate(0, 0, -(closedDays + 1)) // merged, never marked done
	closed = append(closed, old)
	slices.Reverse(closed) // the order they come in doesn't matter

	ids := func(vs []core.ThreadView) []string {
		var out []string
		for _, v := range vs {
			out = append(out, v.ID)
		}
		return out
	}
	got, omitted := recentClosed(closed, 0, 0, now)
	if len(got) != closedLimit || omitted != len(closed)-closedLimit || got[0].ID != "t000" || got[closedLimit-1].ID != fmt.Sprintf("t%03d", closedLimit-1) {
		t.Fatalf("defaults: %v, %d omitted", ids(got), omitted)
	}
	if got, omitted = recentClosed(closed, 1000, 0, now); len(got) != maxClosedLimit || omitted != len(closed)-maxClosedLimit {
		t.Fatalf("limit above the max: %d shown, %d omitted", len(got), omitted)
	}
	if got, omitted = recentClosed(closed, 5, 0, now); len(got) != 5 || got[4].ID != "t004" {
		t.Fatalf("limit 5: %v", ids(got))
	}
	// One day back holds the 25 closed in the last 24 hours (0 through 24 hours ago).
	if got, omitted = recentClosed(closed, maxClosedLimit, 1, now); len(got) != 25 || omitted != len(closed)-25 {
		t.Fatalf("1 day: %d shown, %d omitted", len(got), omitted)
	}
	// The default 30 days leaves out the merged thread last updated 31 days ago, however high the limit.
	short := append(slices.Clone(closed[:1]), closed[len(closed)-3:]...) // old, t002, t001, t000
	if got, omitted = recentClosed(short, maxClosedLimit, 0, now); len(got) != 3 || omitted != 1 {
		t.Fatalf("30 days: %v, %d omitted", ids(got), omitted)
	}
	if got, _ = recentClosed(short, maxClosedLimit, closedDays+2, now); len(got) != 4 || got[3].ID != "old" {
		t.Fatalf("32 days: %v", ids(got))
	}
}

// New commits pushed to a thread's open pull request post its merge card anew after Solo answers the
// update about the turn that made them, so the card is the last thing in the chat.
func TestPushedCommitsMoveTheCardAfterSolosAnswer(t *testing.T) {
	c, o := setup(t)
	ctx := context.Background()
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := git.Run(ctx, dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	repo := filepath.Join(c.Settings().Folders[0], "web")
	origin := repo + ".git"
	run(repo, "clone", "-q", "--bare", repo, origin)
	run(repo, "remote", "add", "origin", origin)
	run(repo, "config", "solo.forge", "github")
	gh := t.TempDir()
	os.WriteFile(filepath.Join(gh, "gh"), []byte(`#!/bin/sh
case "$1 $2" in
"pr create") echo "https://github.com/acme/web/pull/7" ;;
"pr view") echo '{"number":7,"url":"https://github.com/acme/web/pull/7","state":"OPEN","isDraft":false,"autoMergeRequest":null,"mergeStateStatus":"BLOCKED","reviewDecision":"","statusCheckRollup":[]}' ;;
*) exit 1 ;;
esac
`), 0o755)
	t.Setenv("PATH", gh+string(os.PathListSeparator)+os.Getenv("PATH"))
	c.RescanRepos()

	idle := func(id string) func() bool {
		return func() bool { v, _ := c.Thread(id); return v.Status == core.StatusIdle && !v.InTurn }
	}
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Hello", "prompt": "write hello.txt hi"})
	eventually(t, "worker counted", func() bool { v := onlyWorker(c); return idle(v.ID)() && v.Forge != "" && v.Changed > 0 })
	w := onlyWorker(c)
	eventually(t, "Solo answered the update", func() bool { return len(syntheticMessages(c)) == 1 && idle(core.OrchestratorID)() })
	if _, err := c.OpenPR(ctx, w.ID, false); err != nil {
		t.Fatal(err)
	}
	call(t, o, "propose_merge", map[string]any{"id": w.ID, "summary": "Adds hello."})
	cards := func() (all []core.Item, items []core.Item) {
		items, _ = c.Items(ctx, core.OrchestratorID, 0)
		for _, it := range items {
			if it.Kind == core.ItemProposal && it.Ref == w.ID {
				all = append(all, it)
			}
		}
		return all, items
	}
	old, _ := cards()

	// The thread commits during a turn; its end pushes the commit while the update about it is on its
	// way to Solo.
	w, _ = c.Thread(w.ID)
	os.WriteFile(filepath.Join(w.WorktreePath, "more.txt"), []byte("more"), 0o644)
	run(w.WorktreePath, "add", "-A")
	run(w.WorktreePath, "commit", "-q", "-m", "more")
	call(t, o, "message_thread", map[string]any{"id": w.ID, "text": "done?"})
	head := run(w.WorktreePath, "rev-parse", "HEAD")
	eventually(t, "commit pushed", func() bool {
		out, _ := git.Run(ctx, repo, "ls-remote", "origin", w.Branch)
		return strings.HasPrefix(out, head)
	})
	eventually(t, "card posted anew", func() bool {
		all, _ := cards()
		return len(all) == 1 && all[0].ID != old[0].ID
	})
	all, items := cards()
	var update, reply int64
	for _, it := range items {
		var d core.UserData
		if it.Kind == core.ItemUser && it.Decode(&d) == nil && strings.Contains(d.Text, "finished its turn. Summary: echo: done?") {
			update = it.ID
		}
		if it.Kind == core.ItemAssistant && update != 0 && reply == 0 {
			reply = it.ID
		}
	}
	if update == 0 || reply == 0 || all[0].ID < reply || items[len(items)-1].ID != all[0].ID {
		t.Fatalf("card %d, update %d, Solo's answer %d, last item %+v", all[0].ID, update, reply, items[len(items)-1])
	}
	var d core.ProposalData
	if all[0].Decode(&d); d.Summary != "Adds hello." {
		t.Fatalf("summary %q", d.Summary)
	}
}

// A thread's screenshots reach Solo as copies it can read and show, even past where its summary is
// clipped; and Solo's prompt asks for screenshots of UI changes and says how to show an image.
func TestThreadImagesReachSolo(t *testing.T) {
	c, o := setup(t)
	shot := filepath.Join(t.TempDir(), "after.png")
	f, _ := os.Create(shot)
	png.Encode(f, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	f.Close()
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Form", "prompt": strings.Repeat("Moved the field. ", 50) + "![after](" + filepath.ToSlash(shot) + ")"})
	var update string
	eventually(t, "thread update", func() bool {
		for _, m := range syntheticMessages(c) {
			if strings.Contains(m, `"Form" (web) finished its turn`) {
				update = m
				return true
			}
		}
		return false
	})
	w := onlyWorker(c)
	images := c.ImagesIn(w.Summary)
	if len(images) != 1 || strings.Contains(w.Summary, shot) {
		t.Fatalf("summary %q", w.Summary)
	}
	if !strings.Contains(update, "[Images: "+core.ImageDest(images[0])+"]") {
		t.Errorf("the update loses the image:\n%s", update)
	}
	if got := call(t, o, "get_thread", map[string]any{"id": w.ID}); !strings.Contains(got, core.ImageDest(images[0])) {
		t.Errorf("get_thread lacks the image:\n%s", got)
	}
	p := strings.Join(strings.Fields(systemPrompt(c)), " ")
	for _, want := range []string{"reported with screenshots of the changed views", "before and after when the view existed",
		"if it couldn't, tell the user so", "![caption](/absolute/path.png)", "Read one to look at it"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
}
