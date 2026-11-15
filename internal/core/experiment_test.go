package core

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/git"
)

// An experiment is a thread the user doesn't mean to land: Solo can't propose it, turning the mode on
// takes its card away, turning it off brings normal landing back, and the mode survives a restart. Its
// branch and what Done does with it don't change.
func TestExperimentThread(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.c.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []Change
	e.c.Subscribe(func(ch Change) {
		mu.Lock()
		seen = append(seen, ch)
		mu.Unlock()
	})
	cards := func(id string) int {
		t.Helper()
		items, err := e.c.Items(ctx, OrchestratorID, 0)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, it := range items {
			if it.Kind == ItemProposal && it.Ref == id {
				n++
			}
		}
		return n
	}
	counted := func(c *Core, id string) ThreadView {
		t.Helper()
		v := waitStatus(t, c, id, StatusIdle)
		for deadline := time.Now().Add(5 * time.Second); v.Changed < 0 && time.Now().Before(deadline); v, _ = c.Thread(id) {
			time.Sleep(20 * time.Millisecond)
		}
		return v
	}

	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Bench GPUs", Prompt: "write bench.txt results", Experiment: true})
	if err != nil {
		t.Fatal(err)
	}
	v = counted(e.c, v.ID)
	// It works as any thread does: a worktree, a branch and changes it could still land.
	if !v.Experiment || !v.HasWorktree || v.Branch == "" || v.Changed != 1 || !v.CanMerge {
		t.Fatalf("experiment %+v", v)
	}
	err = e.c.AddProposal(v.ID, "ship it")
	if err == nil || !strings.Contains(err.Error(), "experiment") || !strings.Contains(err.Error(), "nothing to propose") {
		t.Fatalf("proposing an experiment: %v", err)
	}
	if n := cards(v.ID); n != 0 {
		t.Fatalf("%d cards for an experiment", n)
	}

	// Off: a normal thread again, which Solo proposes.
	if err := e.c.SetExperiment(ctx, v.ID, false); err != nil {
		t.Fatal(err)
	}
	if v, _ = e.c.Thread(v.ID); v.Experiment {
		t.Fatal("still an experiment")
	}
	if err := e.c.AddProposal(v.ID, "ship it"); err != nil {
		t.Fatal(err)
	}
	if n := cards(v.ID); n != 1 {
		t.Fatalf("%d cards after proposing", n)
	}

	// On again, from Solo: the card goes, and subscribers hear who set it.
	mu.Lock()
	seen = nil
	mu.Unlock()
	if v, err = e.c.SetExperimentFromOrchestrator(v.ID, true); err != nil || !v.Experiment {
		t.Fatalf("set from Solo: %+v %v", v, err)
	}
	if n := cards(v.ID); n != 0 {
		t.Fatalf("%d cards left after making it an experiment", n)
	}
	mu.Lock()
	var set, items bool
	for _, ch := range seen {
		set = set || (ch.ThreadID == v.ID && ch.ExperimentSet && ch.ExperimentBy == FromOrchestrator)
		items = items || (ch.Kind == ChangeItems && ch.ThreadID == OrchestratorID)
	}
	mu.Unlock()
	if !set || !items {
		t.Fatalf("changes %+v", seen)
	}

	// It survives a restart.
	e.c.Close()
	c2 := e.open(t)
	v2, ok := c2.Thread(v.ID)
	if !ok || !v2.Experiment {
		t.Fatalf("after restart %+v", v2)
	}
	if err := c2.AddProposal(v.ID, "ship it"); err == nil {
		t.Fatal("proposed an experiment after a restart")
	}

	// Done keeps its unlanded branch, as for any thread.
	v2 = counted(c2, v.ID)
	if !v2.Unlanded {
		t.Fatalf("experiment's changes aren't unlanded: %+v", v2)
	}
	if err := c2.Done(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if out, err := git.Run(ctx, e.repo, "branch", "--list", v.Branch); err != nil || !strings.Contains(out, strings.TrimPrefix(v.Branch, "refs/heads/")) {
		t.Fatalf("branch %s not kept after done: %q %v", v.Branch, out, err)
	}
}
