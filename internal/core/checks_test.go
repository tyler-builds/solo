package core

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/git"
)

// checksEnv is a worker with changes in a GitHub repository (with a GitHub Actions workflow when ci),
// ready to open its pull request through fakeGH. events collects every PREvent.
type checksEnv struct {
	*env
	gh, id string
	mu     sync.Mutex
	events []string
}

func newChecksEnv(t *testing.T, ci bool) *checksEnv {
	t.Helper()
	gh := fakeGH(t)
	e := newEnv(t)
	ctx := context.Background()
	run := func(dir string, args ...string) {
		t.Helper()
		if _, err := git.Run(ctx, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if ci {
		wf := filepath.Join(e.repo, ".github", "workflows")
		if err := os.MkdirAll(wf, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wf, "ci.yml"), []byte("on: pull_request\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(e.repo, "add", "-A")
		run(e.repo, "commit", "-q", "-m", "ci")
	}
	origin := filepath.Join(e.root, "origin.git")
	run(e.root, "clone", "-q", "--bare", e.repo, origin)
	run(e.repo, "remote", "add", "origin", origin)
	run(e.repo, "config", "solo.forge", "github")
	ce := &checksEnv{env: e, gh: gh}
	e.c.Subscribe(func(ch Change) {
		if ch.PREvent != "" {
			ce.mu.Lock()
			ce.events = append(ce.events, ch.PREvent)
			ce.mu.Unlock()
		}
	})
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Add hello", Prompt: "write hello.txt hi"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
	for deadline := time.Now().Add(5 * time.Second); !v.CanOpenPR && time.Now().Before(deadline); v, _ = e.c.Thread(v.ID) {
		time.Sleep(20 * time.Millisecond)
	}
	if !v.CanOpenPR {
		t.Fatalf("can't open a pull request: %+v", v)
	}
	ce.id = v.ID
	return ce
}

// saw reports whether a PREvent was sent.
func (e *checksEnv) saw(event string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev == event {
			return true
		}
	}
	return false
}

// waitPR waits until the thread's pull request is as ok wants: the watcher may still be applying a
// read of the forge from before the test changed its answer.
func (e *checksEnv) waitPR(t *testing.T, what string, ok func(ThreadView) bool) ThreadView {
	t.Helper()
	var v ThreadView
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		e.c.CheckPRs(context.Background())
		if v, _ = e.c.Thread(e.id); v.PR != nil && ok(v) {
			return v
		}
	}
	t.Fatalf("%s: %+v", what, v.PR)
	return v
}

// age moves the pull request's checks back in time, as if its head had changed that long ago.
func (e *checksEnv) age(d time.Duration) {
	e.c.mu.Lock()
	defer e.c.mu.Unlock()
	for _, p := range e.c.threads[e.id].parts {
		if p.PR != nil {
			pr := *p.PR
			pr.ChecksSince = pr.ChecksSince.Add(-d)
			p.PR = &pr
		}
	}
}

// Right after it opens, a pull request in a repository with CI gets no checks from GitHub: they
// aren't queued yet. It waits for them, can't merge, and is polled quickly, until they show up and
// run, then pass.
func TestChecksWaitAfterOpening(t *testing.T) {
	e := newChecksEnv(t, true)
	ctx := context.Background()
	setView(t, e.gh, "OPEN", "CLEAN") // GitHub would merge it: no checks reported, none required yet
	pr, err := e.c.OpenPR(ctx, e.id, false)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Checks != forge.ChecksWaiting || pr.Mergeable || pr.ChecksSince.IsZero() {
		t.Fatalf("opened %+v, want waiting for checks", pr)
	}
	v := e.waitPR(t, "just opened", func(v ThreadView) bool { return v.PR.Checks == forge.ChecksWaiting })
	if v.CanMerge || v.PR.Blocked != "waiting for checks" {
		t.Fatalf("can merge before its checks started: CanMerge %v, %+v", v.CanMerge, v.PR)
	}
	if got := e.c.nextPRPoll(30 * time.Second); got != freshPRPoll {
		t.Fatalf("next poll in %v while waiting for checks, want %v", got, freshPRPoll)
	}
	if e.saw(PRChecksPassed) {
		t.Fatal("checks passed before any ran")
	}

	setView(t, e.gh, "OPEN", "BLOCKED", "test=PENDING")
	v = e.waitPR(t, "checks queued", func(v ThreadView) bool { return v.PR.Checks == forge.ChecksPending })
	if v.CanMerge || e.saw(PRChecksPassed) {
		t.Fatalf("checks running: CanMerge %v, checks passed %v", v.CanMerge, e.saw(PRChecksPassed))
	}
	if got := e.c.nextPRPoll(30 * time.Second); got != 30*time.Second {
		t.Fatalf("next poll in %v while checks run, want 30s", got)
	}

	setView(t, e.gh, "OPEN", "CLEAN", "test=SUCCESS")
	v = e.waitPR(t, "checks passed", func(v ThreadView) bool { return v.PR.Checks == forge.ChecksPassing })
	if !v.CanMerge {
		t.Fatalf("can't merge with its checks passed: %+v", v.PR)
	}
	for deadline := time.Now().Add(5 * time.Second); !e.saw(PRChecksPassed) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if !e.saw(PRChecksPassed) {
		t.Fatal("no checks_passed event once they passed")
	}

	// New commits pushed: the checks start over, and wait again rather than pass.
	since := v.PR.ChecksSince
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(v.WorktreePath, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "fix"}} {
		if _, err := git.Run(ctx, v.WorktreePath, args...); err != nil {
			t.Fatal(err)
		}
	}
	setView(t, e.gh, "OPEN", "CLEAN")
	e.c.countChanges(e.id)
	v = e.waitPR(t, "pushed", func(v ThreadView) bool { return v.PR.Checks == forge.ChecksWaiting })
	if !v.PR.ChecksSince.After(since) || v.CanMerge {
		t.Fatalf("after a push: CanMerge %v, %+v (checks since %v before)", v.CanMerge, v.PR, since)
	}
}

// A repository with CI whose checks never show up on the pull request (its workflows don't run on
// pull requests, say) stops waiting once checksGrace passed: it has no checks, and can merge.
func TestChecksSettleWhenNoneShowUp(t *testing.T) {
	e := newChecksEnv(t, true)
	setView(t, e.gh, "OPEN", "CLEAN")
	if _, err := e.c.OpenPR(context.Background(), e.id, false); err != nil {
		t.Fatal(err)
	}
	e.waitPR(t, "just opened", func(v ThreadView) bool { return v.PR.Checks == forge.ChecksWaiting })
	e.age(checksGrace + time.Second)
	v := e.waitPR(t, "after the grace", func(v ThreadView) bool { return v.PR.Checks == forge.ChecksNone })
	if !v.CanMerge || v.PR.Blocked != "" {
		t.Fatalf("can't merge with no checks: CanMerge %v, %+v", v.CanMerge, v.PR)
	}
	if got := e.c.nextPRPoll(30 * time.Second); got != 30*time.Second*settledPRPoll {
		t.Fatalf("next poll in %v once settled, want %v", got, 30*time.Second*settledPRPoll)
	}
	if e.saw(PRChecksPassed) {
		t.Fatal("checks_passed sent with no checks")
	}
}

// A repository without CI set up has no checks to wait for: its pull request has none at once, though
// it's still polled quickly for a while, in case a check from outside the repository shows up.
func TestChecksNoneWithoutCI(t *testing.T) {
	e := newChecksEnv(t, false)
	setView(t, e.gh, "OPEN", "CLEAN")
	pr, err := e.c.OpenPR(context.Background(), e.id, false)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Checks != forge.ChecksNone || !pr.Mergeable {
		t.Fatalf("opened %+v, want no checks", pr)
	}
	v := e.waitPR(t, "just opened", func(v ThreadView) bool { return v.PR.Checks == forge.ChecksNone })
	if !v.CanMerge {
		t.Fatalf("can't merge with no checks: %+v", v.PR)
	}
	if got := e.c.nextPRPoll(30 * time.Second); got != freshPRPoll {
		t.Fatalf("next poll in %v just after opening, want %v", got, freshPRPoll)
	}
	e.age(checksGrace + time.Second)
	if got := e.c.nextPRPoll(30 * time.Second); got != 30*time.Second*settledPRPoll {
		t.Fatalf("next poll in %v once settled, want %v", got, 30*time.Second*settledPRPoll)
	}
}

func TestHasCI(t *testing.T) {
	dir := t.TempDir()
	if hasCI(dir, forge.GitHub) || hasCI(dir, forge.GitLab) || hasCI("", forge.GitHub) {
		t.Fatal("CI in an empty folder")
	}
	wf := filepath.Join(dir, ".github", "workflows")
	os.MkdirAll(wf, 0o755)
	os.WriteFile(filepath.Join(wf, "README.md"), nil, 0o644)
	if hasCI(dir, forge.GitHub) {
		t.Fatal("a workflows folder without a workflow is CI")
	}
	os.WriteFile(filepath.Join(wf, "test.yaml"), nil, 0o644)
	if !hasCI(dir, forge.GitHub) || hasCI(dir, forge.GitLab) {
		t.Fatal("a GitHub workflow")
	}
	os.WriteFile(filepath.Join(dir, ".gitlab-ci.yml"), nil, 0o644)
	if !hasCI(dir, forge.GitLab) {
		t.Fatal("a GitLab pipeline")
	}
}
