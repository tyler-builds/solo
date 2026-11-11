package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
