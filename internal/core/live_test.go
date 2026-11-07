//go:build live

package core

// Live tests run real Claude (cheap model) through the real sidecar:
//
//	go test -tags live -run Live ./internal/core/ -v

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/git"
)

func TestLiveWorkerCommitsAndMerges(t *testing.T) {
	cmd, err := agent.FindCommand()
	if err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	repo := filepath.Join(root, "calc")
	os.MkdirAll(repo, 0o755)
	ctx := context.Background()
	os.WriteFile(filepath.Join(repo, "calc.py"), []byte("def add(a, b):\n    return a - b\n"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if _, err := git.Run(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	c, err := New(Config{DataDir: t.TempDir(), Agent: cmd, Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.UpdateSettings(func(s *Settings) { s.Folders = []string{root}; s.WorkerModel = "haiku" })

	v, err := c.CreateWorker(ctx, WorkerSpec{Repo: "calc", Title: "Fix add", Prompt: "add() in calc.py subtracts. Fix it."})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		v, _ = c.Thread(v.ID)
		if !v.InTurn && (v.Status == StatusIdle || v.Status == StatusFailed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", v.Status)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("status=%s cost=$%.4f summary=%q", v.Status, v.CostUSD, v.Summary)
	if v.Status != StatusIdle {
		t.Fatal(v.Error)
	}
	log, _ := git.Run(ctx, v.WorktreePath, "log", "--oneline", v.BaseCommit+"..HEAD")
	t.Logf("worker commits:\n%s", log)
	items, _ := c.Items(ctx, v.ID, 0)
	t.Logf("items: %s", kinds(items))
	if _, err := c.Merge(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "calc.py"))
	if !strings.Contains(string(b), "a + b") {
		t.Fatalf("merged calc.py:\n%s", b)
	}
}
