//go:build live

package core

// Live tests run real Claude (cheap model) through the real sidecar:
//
//	go test -tags live -run Live ./internal/core/ -v

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/store"
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

func TestLiveWorkerSeesImage(t *testing.T) {
	cmd, err := agent.FindCommand()
	if err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	repo := filepath.Join(root, "pic")
	os.MkdirAll(repo, 0o755)
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
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

	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{220, 20, 20, 255}}, image.Point{}, draw.Src)
	p, err := c.SaveImage(img)
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.CreateWorker(ctx, WorkerSpec{Repo: "pic", Title: "Color", Images: []string{p},
		Prompt: "Don't use any tools. Reply with one word: the color of the attached image."})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
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
	t.Logf("status=%s summary=%q", v.Status, v.Summary)
	if !strings.Contains(strings.ToLower(v.Summary), "red") {
		t.Fatalf("Claude didn't see the image: %q %s", v.Summary, v.Error)
	}
}

func TestLiveBranchNaming(t *testing.T) {
	cmd, err := agent.FindCommand()
	if err != nil {
		t.Skip(err)
	}
	c, err := New(Config{DataDir: t.TempDir(), Agent: cmd, Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	repo := t.TempDir()
	git.Run(context.Background(), repo, "init", "-q", "-b", "main")
	c.UpdateSettings(func(s *Settings) {
		s.BranchRules = "Use fix/<ticket>-<two-word-summary> in lowercase. The ticket is OW-1234 style when the task names one; otherwise leave it out."
	})
	name := c.nameBranch(context.Background(), store.Thread{ID: "abcdef123456", RepoPath: repo, Title: "Fix token refresh",
		Prompt: "OW-4821: the OAuth access token isn't refreshed when it expires mid-request."}, []string{repo})
	t.Logf("branch: %s", name)
	if !strings.HasPrefix(strings.ToLower(name), "fix/ow-4821-") {
		t.Fatalf("branch %q doesn't follow the rules", name)
	}
}

func TestLivePRText(t *testing.T) {
	cmd, err := agent.FindCommand()
	if err != nil {
		t.Skip(err)
	}
	c, err := New(Config{DataDir: t.TempDir(), Agent: cmd, Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	text, err := c.agent.Complete(context.Background(), agent.CompleteParams{Model: "haiku", System: prSystem,
		Prompt:     "Write the pull request for this change.\n\nRules:\nConventional Commit title with the ticket. Description: Summary, Changes, Testing.\n\nTask title: Fix token refresh\nTask:\nOW-4821: refresh the OAuth token on a 401 and retry once.\n\nWhat the agent that made the change reported:\nAdded a single retry after refreshing on 401 in src/auth/refresh.ts, with a test. Auth tests pass.\n\nCommits:\nfix(auth): retry once after refreshing on 401\n\nFiles changed:\nM src/auth/refresh.ts (+14 −3)\nM src/auth/refresh.test.ts (+38 −0)",
		ClaudePath: agent.FindClaude("").Path})
	if err != nil {
		t.Fatal(err)
	}
	title, desc := parsePRText(text)
	t.Logf("title: %s\ndescription:\n%s", title, desc)
	if title == "" || desc == "" || !strings.Contains(strings.ToUpper(title), "OW-4821") {
		t.Fatalf("unusable answer:\n%s", text)
	}
}

// A real worker runs a command in the background and ends its turn: it waits on that work (not ready,
// nothing to land) until the command finishes and Claude's own turn reads it. A second, stopped by
// the user while it waits, is stopped at once.
func TestLiveBackgroundWork(t *testing.T) {
	cmd, err := agent.FindCommand()
	if err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	repo := filepath.Join(root, "notes")
	os.MkdirAll(repo, 0o755)
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
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
	const prompt = "Do exactly this, nothing else. 1) Write note.txt containing hi, then git add and commit it. " +
		"2) Use the Bash tool with run_in_background: true and description %q to run: %s " +
		"3) End your turn at once, saying only: waiting. 4) When you are told the command finished, reply only: finished."
	wait := func(id string, want ...string) ThreadView {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for {
			v, _ := c.Thread(id)
			for _, s := range want {
				if v.Status == s && !v.InTurn {
					return v
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out at %s, want %v", v.Status, want)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	v, err := c.CreateWorker(ctx, WorkerSpec{Repo: "notes", Title: "Bench", Prompt: fmt.Sprintf(prompt, "Run benchmarks", "sleep 20; echo done")})
	if err != nil {
		t.Fatal(err)
	}
	log := watchStatuses(t, c, v.ID)
	v = wait(v.ID, StatusBackground, StatusIdle, StatusFailed)
	t.Logf("after its turn: %s, background %+v, summary %q", v.Status, v.Background, v.Summary)
	if v.Status != StatusBackground || len(v.Background) != 1 || v.Background[0].Description != "Run benchmarks" {
		t.Fatalf("want it waiting on Run benchmarks: %s %+v", v.Status, v.Background)
	}
	if v.CanMerge || v.CanMergeLocal || v.CanOpenPR {
		t.Fatal("it can land while its background work runs")
	}
	v = wait(v.ID, StatusIdle, StatusFailed)
	t.Logf("statuses %s; summary %q, cost $%.4f", log, v.Summary, v.CostUSD)
	if v.Status != StatusIdle || !strings.HasSuffix(log.String(), "running>background background>running running>idle") {
		t.Fatalf("statuses %s", log)
	}

	s, err := c.CreateWorker(ctx, WorkerSpec{Repo: "notes", Title: "Stop", Prompt: fmt.Sprintf(prompt, "Long sleep", "sleep 300")})
	if err != nil {
		t.Fatal(err)
	}
	if s = wait(s.ID, StatusBackground, StatusIdle, StatusFailed); s.Status != StatusBackground {
		t.Fatalf("want it waiting: %s", s.Status)
	}
	if err := c.Interrupt(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	s = wait(s.ID, StatusInterrupted, StatusIdle)
	time.Sleep(3 * time.Second)
	if s, _ = c.Thread(s.ID); s.Status != StatusInterrupted || len(s.Background) != 0 || s.InTurn {
		t.Fatalf("after Stop: %s, background %+v, in turn %v", s.Status, s.Background, s.InTurn)
	}
}
