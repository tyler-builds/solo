package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
