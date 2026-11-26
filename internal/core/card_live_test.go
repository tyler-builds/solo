//go:build live

package core

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/git"
)

// A board card through real Claude: the lead (Sonnet, to keep it cheap) briefs the builder (Haiku),
// reviews its work and writes a review.
//
//	SOLO_AGENT=… go test -tags live -run LiveCard ./internal/core/ -v
func TestLiveCard(t *testing.T) {
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
	c, err := New(Config{DataDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.UpdateSettings(func(s *Settings) {
		s.Folders = []string{root}
		s.OrchestratorModel, s.WorkerModel = "sonnet", "haiku"
		s.CardBriefModel, s.CardBriefEffort = "sonnet", "medium"
		s.CardReviewModel, s.CardReviewEffort = "opus", "low"
	})
	v, err := c.CreateWorker(ctx, WorkerSpec{Repo: "calc", Title: "Fix add", Prompt: "add() is broken.", FromUser: true})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Minute)
	for {
		v, _ = c.Thread(v.ID)
		if v.Card != nil && v.Card.Phase == CardReview && !v.InTurn {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %+v, builder %s", v.Card, v.Status)
		}
		time.Sleep(time.Second)
	}
	b, _ := os.ReadFile(filepath.Join(v.WorktreePath, "calc.py"))
	t.Logf("calc.py:\n%s\nneed: %s · rounds: %d\nreview:\n%s", b, v.Card.Need, v.Card.Rounds, v.Card.Review)
	items, _ := c.Items(ctx, v.ID, 0)
	for _, it := range items {
		var d UserData
		if it.Kind == ItemUser && it.Decode(&d) == nil {
			t.Logf("to the builder (%s):\n%s", d.From, d.Text)
		}
	}
	if r, ok := c.Thread(v.Card.Reviewer); !ok || r.ActualModel == "" || !strings.Contains(r.ActualModel, "opus") {
		t.Errorf("reviewer ran on %q", r.ActualModel)
	} else {
		t.Logf("briefer and reviewer: %s, %s", func() string { l, _ := c.Thread(v.Card.Lead); return l.ActualModel }(), r.ActualModel)
	}
	for who, id := range map[string]string{"briefer": v.Card.Lead, "reviewer": v.Card.Reviewer} {
		items, _ := c.Items(ctx, id, 0)
		for _, it := range items {
			var d ToolData
			if it.Kind == ItemTool && it.Decode(&d) == nil {
				t.Logf("%s tool %s %s error=%v → %s", who, d.Name, firstLine(string(d.Input), 120), d.IsError, firstLine(d.Result, 120))
			}
		}
	}
	if !strings.Contains(string(b), "a + b") {
		t.Errorf("add() isn't fixed")
	}
	if v.Card.Review == "" || strings.Contains(v.Card.Review, "<review") {
		t.Errorf("review %q", v.Card.Review)
	}
}
