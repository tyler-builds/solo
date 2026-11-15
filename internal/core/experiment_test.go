package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// New commits pushed to an experiment's open pull request bring no merge card back: turning the mode
// on removed its card, and one that got past that (posted as the mode was turned on) isn't posted anew.
func TestExperimentPushPostsNoCard(t *testing.T) {
	gh := fakeGH(t)
	e := newEnv(t)
	ctx := context.Background()
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := git.Run(ctx, dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	origin := filepath.Join(e.root, "origin.git")
	run(e.root, "clone", "-q", "--bare", e.repo, origin)
	run(e.repo, "remote", "add", "origin", origin)
	run(e.repo, "config", "solo.forge", "github")
	if _, err := e.c.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Bench GPUs", Prompt: "write bench.txt results"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	for deadline := time.Now().Add(5 * time.Second); (v.Changed < 0 || v.Forge == "") && time.Now().Before(deadline); v, _ = e.c.Thread(v.ID) {
		time.Sleep(20 * time.Millisecond)
	}
	setView(t, gh, "OPEN", "BLOCKED")
	if _, err := e.c.OpenPR(ctx, v.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := e.c.AddProposal(v.ID, "ship it"); err != nil {
		t.Fatal(err)
	}
	cards := func() []Item {
		t.Helper()
		items, err := e.c.Items(ctx, OrchestratorID, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []Item
		for _, it := range items {
			if it.Kind == ItemProposal && it.Ref == v.ID {
				out = append(out, it)
			}
		}
		return out
	}
	n := 0
	push := func() {
		t.Helper()
		n++
		os.WriteFile(filepath.Join(v.WorktreePath, fmt.Sprintf("more%d.txt", n)), []byte("more"), 0o644)
		run(v.WorktreePath, "add", "-A")
		run(v.WorktreePath, "commit", "-q", "-m", fmt.Sprintf("more %d", n))
		e.c.countChanges(v.ID)
		head := run(v.WorktreePath, "rev-parse", "HEAD")
		if remote := run(e.repo, "ls-remote", "origin", v.Branch); !strings.HasPrefix(remote, head) {
			t.Fatalf("new commit not pushed: remote %q, local %s", remote, head)
		}
	}

	if err := e.c.SetExperiment(ctx, v.ID, true); err != nil {
		t.Fatal(err)
	}
	push()
	if all := cards(); len(all) != 0 {
		t.Fatalf("a push brought a card back: %+v", all)
	}

	// A card that got past the mode stays where it is, not posted anew at the end of Solo's chat.
	e.c.mu.Lock()
	err = e.c.postProposalLocked(ProposalData{ThreadID: v.ID, Title: v.Title, Summary: "ship it"})
	e.c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	stray := cards()
	e.c.addNotice(OrchestratorID, "info", "the conversation moves on")
	push()
	e.c.cardStale(v.ID)
	if all := cards(); len(all) != 1 || all[0].ID != stray[0].ID {
		t.Fatalf("an experiment's card was posted anew: %+v, was %+v", all, stray)
	}
}
