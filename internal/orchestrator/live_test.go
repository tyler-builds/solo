//go:build live

package orchestrator

// A live run of the whole loop with real Claude:
//
//	go test -tags live -run Live ./internal/orchestrator/ -v -timeout 15m

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/git"
)

func TestLiveOrchestratorDelegatesAndProposesMerge(t *testing.T) {
	cmd, err := agent.FindCommand()
	if err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	repo := filepath.Join(root, "calc")
	os.MkdirAll(repo, 0o755)
	os.WriteFile(filepath.Join(repo, "calc.py"), []byte("def add(a, b):\n    return a - b\n\n\ndef mul(a, b):\n    return a + b\n"), 0o644)
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if _, err := git.Run(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c, err := core.New(core.Config{DataDir: dataDir, Agent: cmd, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	model := os.Getenv("SOLO_LIVE_MODEL")
	if model == "" {
		model = "sonnet"
	}
	c.UpdateSettings(func(s *core.Settings) {
		s.Folders = []string{root}
		s.OrchestratorModel = model
		s.WorkerModel = "haiku"
	})
	o, err := New(c, dataDir, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Send(ctx, "In the calc repo, add() and mul() are both wrong. Get them fixed in one worker thread, and propose a merge once you've checked the result."); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Minute)
	var proposal core.ProposalData
	for proposal.ThreadID == "" && time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		items, _ := c.Items(ctx, core.OrchestratorID, 0)
		for _, it := range items {
			if it.Kind == core.ItemProposal {
				it.Decode(&proposal)
			}
		}
	}
	items, _ := c.Items(ctx, core.OrchestratorID, 0)
	for _, it := range items {
		switch it.Kind {
		case core.ItemUser, core.ItemAssistant:
			var d struct {
				Text      string `json:"text"`
				Synthetic bool   `json:"synthetic"`
			}
			it.Decode(&d)
			t.Logf("%s%s: %s", it.Kind, map[bool]string{true: " (synthetic)"}[d.Synthetic], d.Text)
		case core.ItemTool:
			var d core.ToolData
			it.Decode(&d)
			t.Logf("tool %s %s", d.Name, d.Input)
		case core.ItemProposal, core.ItemTurn, core.ItemNotice:
			t.Logf("%s %s", it.Kind, it.Data)
		}
	}
	orch, _ := c.Thread(core.OrchestratorID)
	t.Logf("orchestrator cost $%.4f", orch.CostUSD)
	if proposal.ThreadID == "" {
		t.Fatal("no merge proposal")
	}
	if _, err := c.Merge(ctx, proposal.ThreadID); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "calc.py"))
	if !strings.Contains(string(b), "a + b") || !strings.Contains(string(b), "a * b") {
		t.Fatalf("calc.py after merge:\n%s", b)
	}
}
