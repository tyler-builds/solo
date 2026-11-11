package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/git"
)

// fakeGHOwn is a gh for threads that open pull requests themselves: pr create prints #93's address,
// pr view answers from view.json, pr list from list.json, and every call is logged to calls.
func fakeGHOwn(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
d="$(dirname "$0")"
echo "$@" >> "$d/calls"
case "$1 $2" in
"pr create") echo "Creating pull request for perf/text-draw-cache into main"; echo "https://github.com/acme/proj/pull/93" ;;
"pr view") cat "$d/view.json" ;;
"pr list") if [ -f "$d/list.json" ]; then cat "$d/list.json"; else echo "[]"; fi ;;
"pr merge") ;;
*) echo "unexpected: $@" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func setOwnView(t *testing.T, dir string, number int, state, head string) {
	t.Helper()
	j := fmt.Sprintf(`{"number":%d,"url":"https://github.com/acme/proj/pull/%d","state":%q,"isDraft":false,"autoMergeRequest":null,"mergeStateStatus":"BLOCKED","reviewDecision":"","headRefName":%q,"statusCheckRollup":[]}`,
		number, number, state, head)
	if err := os.WriteFile(filepath.Join(dir, "view.json"), []byte(j), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withOrigin gives the env's repository a bare origin, and makes GitHub its forge.
func withOrigin(t *testing.T, e *env) string {
	t.Helper()
	ctx := context.Background()
	origin := filepath.Join(e.root, "origin.git")
	for _, c := range []struct {
		dir  string
		args []string
	}{
		{e.root, []string{"clone", "-q", "--bare", e.repo, origin}},
		{e.repo, []string{"remote", "add", "origin", origin}},
		{e.repo, []string{"fetch", "-q", "origin"}},
		{e.repo, []string{"config", "solo.forge", "github"}},
	} {
		if _, err := git.Run(ctx, c.dir, c.args...); err != nil {
			t.Fatal(err)
		}
	}
	return origin
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A thread that makes its own branch, pushes it and opens a pull request with gh pr create, as the
// jamb thread did: Solo links that PR, follows it, and closes the thread when it merges, ticking off
// its todo, as if Solo had opened it.
func TestThreadOpensItsOwnPR(t *testing.T) {
	gh := fakeGHOwn(t)
	e := newEnv(t)
	withOrigin(t, e)
	ctx := context.Background()
	events := make(chan string, 20)
	e.c.Subscribe(func(ch Change) {
		if ch.PREvent != "" {
			events <- ch.PREvent
		}
	})

	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Glyph cache", Prompt: "write cache.go package text"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if _, err := e.c.AddTodo("Speed up text drawing", []string{v.ID}); err != nil {
		t.Fatal(err)
	}
	setOwnView(t, gh, 93, "OPEN", "perf/text-draw-cache")
	err = e.c.Send(ctx, v.ID, "bash git checkout -q -b perf/text-draw-cache && git add -A && git commit -qm 'perf: cache glyphs' && "+
		"git push -q -u origin perf/text-draw-cache && gh pr create --fill")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the PR to be linked", func() bool {
		v, _ = e.c.Thread(v.ID)
		return v.PR != nil
	})
	select {
	case ev := <-events:
		if ev != PRFound {
			t.Fatalf("event %s, want %s", ev, PRFound)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no found event")
	}
	if v.PR.Number != 93 || v.PR.State != "open" || v.PR.Head != "perf/text-draw-cache" || v.CanOpenPR || v.Status != StatusIdle {
		t.Fatalf("linked %+v, PR %+v", v, v.PR)
	}
	if open := e.c.Todos(false); len(open) != 0 {
		t.Fatalf("an open PR lands a todo, as Solo's own does: %+v", open)
	}

	// It merges on GitHub: the watcher sees it, and the thread closes as merged.
	setOwnView(t, gh, 93, "MERGED", "perf/text-draw-cache")
	e.c.CheckPRs(ctx)
	v = waitStatus(t, e.c, v.ID, StatusMerged)
	if v.WorktreePath != "" || !v.CanDone || v.Unlanded {
		t.Fatalf("after merge %+v", v)
	}
	if all := e.c.Todos(true); len(all) != 1 || all[0].DoneBy != TodoByThread {
		t.Fatalf("todos %+v", all)
	}
}

// Only a command that opens a pull request links what it prints: merging Dependabot's with gh pr
// merge, or viewing one, doesn't make it the thread's.
func TestPRHintsOf(t *testing.T) {
	bash := func(cmd, out string, isErr bool) ToolData {
		return ToolData{Name: "Bash", Input: []byte(fmt.Sprintf(`{"command":%q}`, cmd)), Result: out, IsError: isErr, Done: true}
	}
	pr := "https://github.com/acme/proj/pull/93"
	for _, c := range []struct {
		d    ToolData
		want int
	}{
		{bash("git push -u origin perf/x && gh pr create --fill", "Creating pull request\n"+pr+"\n", false), 1},
		{bash("glab mr create --fill --yes", "https://gitlab.com/acme/proj/-/merge_requests/4\n", false), 1},
		{bash("gh pr merge 84 --squash", "Merged https://github.com/acme/proj/pull/84", false), 0},
		{bash("gh pr view 84", pr, false), 0},
		{bash("gh pr create --fill", "a pull request already exists: "+pr, true), 0},
		{ToolData{Name: "Read", Input: []byte(`{"file_path":"gh pr create"}`), Result: pr}, 0},
	} {
		if got := prHintsOf(c.d); len(got) != c.want {
			t.Errorf("%s: %v", c.d.Input, got)
		}
	}
}

// A PR the thread printed before Solo started (so no live tool result saw it) is found in its saved
// transcript; and one opened on the forge from a branch it pushed is found by that branch.
func TestFindsPRsFromTranscriptAndBranch(t *testing.T) {
	gh := fakeGHOwn(t)
	e := newEnv(t)
	withOrigin(t, e)
	ctx := context.Background()

	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Pushed", Prompt: "write a.txt a"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	mustGit(t, v.WorktreePath, "add", "-A")
	mustGit(t, v.WorktreePath, "commit", "-qm", "a")
	mustGit(t, v.WorktreePath, "push", "-q", "-u", "origin", v.Branch)
	created := time.Now().UTC().Format(time.RFC3339)
	list := fmt.Sprintf(`[{"number":12,"url":"https://github.com/acme/proj/pull/12","state":"OPEN","isDraft":false,"headRefName":%q,"createdAt":"2020-01-01T00:00:00Z"},`+
		`{"number":41,"url":"https://github.com/acme/proj/pull/41","state":"OPEN","isDraft":false,"headRefName":%q,"createdAt":%q}]`, v.Branch, v.Branch, created)
	os.WriteFile(filepath.Join(gh, "list.json"), []byte(list), 0o644)
	setOwnView(t, gh, 41, "OPEN", v.Branch)
	e.c.countChanges(v.ID)
	if v, _ = e.c.Thread(v.ID); v.PR == nil || v.PR.Number != 41 {
		t.Fatalf("by branch: PR %+v (the older #12 predates the thread)", v.PR)
	}

	// From the transcript, after a restart.
	w, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Own", Prompt: "write b.txt b"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, w.ID, StatusIdle)
	os.Remove(filepath.Join(gh, "list.json"))
	setOwnView(t, gh, 93, "OPEN", "perf/own")
	// Not linked live: view fails, as if gh were offline.
	os.Rename(filepath.Join(gh, "view.json"), filepath.Join(gh, "view.off"))
	if err := e.c.Send(ctx, w.ID, "bash git checkout -q -b perf/own && git add -A && git commit -qm b && git push -q -u origin perf/own && gh pr create --fill"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, w.ID, StatusIdle)
	time.Sleep(200 * time.Millisecond)
	e.c.Close()
	os.Rename(filepath.Join(gh, "view.off"), filepath.Join(gh, "view.json"))
	c := e.open(t)
	waitFor(t, "the PR from the transcript", func() bool {
		w, _ = c.Thread(w.ID)
		return w.PR != nil
	})
	if w.PR.Number != 93 {
		t.Fatalf("from transcript %+v", w.PR)
	}
}

// A thread whose changes were squash-merged on the forge, in another clone: its commits aren't in
// main, but its changes are. Solo fetches, sees that, and closes the thread as merged, ticking off its
// todo, as its own merge does.
func TestSquashMergedElsewhereLands(t *testing.T) {
	e := newEnv(t)
	origin := withOrigin(t, e)
	ctx := context.Background()
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Two commits", Prompt: "write one.txt 1"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if _, err := e.c.AddTodo("Ship one and two", []string{v.ID}); err != nil {
		t.Fatal(err)
	}
	mustGit(t, v.WorktreePath, "add", "-A")
	mustGit(t, v.WorktreePath, "commit", "-qm", "one")
	os.WriteFile(filepath.Join(v.WorktreePath, "two.txt"), []byte("2\n"), 0o644)
	mustGit(t, v.WorktreePath, "add", "-A")
	mustGit(t, v.WorktreePath, "commit", "-qm", "two")
	mustGit(t, v.WorktreePath, "push", "-q", "origin", v.Branch)
	e.c.countChanges(v.ID)
	if v, _ = e.c.Thread(v.ID); v.Status != StatusIdle || v.Changed != 2 || !v.Unlanded {
		t.Fatalf("before the squash %+v", v)
	}
	refetch := func() {
		e.c.mu.Lock()
		e.c.fetched = map[string]time.Time{} // as once fetchEvery has passed
		e.c.mu.Unlock()
	}

	// Elsewhere: someone else's work lands first, then the branch is squash-merged, and the branch
	// deleted, as GitHub does.
	other := filepath.Join(e.root, "other")
	mustGit(t, e.root, "clone", "-q", origin, other)
	os.WriteFile(filepath.Join(other, "unrelated.txt"), []byte("x\n"), 0o644)
	mustGit(t, other, "add", "-A")
	mustGit(t, other, "commit", "-qm", "unrelated")
	mustGit(t, other, "merge", "-q", "--squash", "origin/"+v.Branch)
	mustGit(t, other, "commit", "-qm", "Two commits (#93)")
	mustGit(t, other, "push", "-q", "origin", "main", ":"+v.Branch)
	if _, err := git.Run(ctx, v.WorktreePath, "merge-base", "--is-ancestor", "HEAD", "origin/main"); err == nil {
		t.Fatal("the test's squash left the branch an ancestor")
	}

	refetch()
	go e.c.countChanges(v.ID) // as at the end of a turn
	v = waitStatus(t, e.c, v.ID, StatusMerged)
	if v.WorktreePath != "" || !v.CanDone || v.Unlanded {
		t.Fatalf("after the squash %+v", v)
	}
	if all := e.c.Todos(true); len(all) != 1 || all[0].DoneBy != TodoByThread {
		t.Fatalf("todos %+v", all)
	}

	// Work only partly in main is still to land.
	w, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Partly", Prompt: "write p.txt p"})
	if err != nil {
		t.Fatal(err)
	}
	w = waitStatus(t, e.c, w.ID, StatusIdle)
	os.WriteFile(filepath.Join(w.WorktreePath, "q.txt"), []byte("q\n"), 0o644)
	mustGit(t, w.WorktreePath, "add", "-A")
	mustGit(t, w.WorktreePath, "commit", "-qm", "p and q")
	mustGit(t, other, "pull", "-q", "--ff-only")
	os.WriteFile(filepath.Join(other, "p.txt"), []byte("p\n"), 0o644)
	mustGit(t, other, "add", "-A")
	mustGit(t, other, "commit", "-qm", "just p")
	mustGit(t, other, "push", "-q", "origin", "main")
	refetch()
	e.c.countChanges(w.ID)
	if w, _ = e.c.Thread(w.ID); w.Status != StatusIdle || w.Changed != 2 {
		t.Fatalf("partly landed %+v", w)
	}
}

// Marking a thread with unlanded changes done keeps its branch, with its uncommitted work committed,
// and Restore brings it back; discarding is what deletes it.
func TestDoneKeepsUnlandedWork(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Unlanded", Prompt: "write keep.txt keep me"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	waitFor(t, "the count", func() bool { v, _ = e.c.Thread(v.ID); return v.Changed >= 0 })
	if !v.CanDone || !v.Unlanded || v.Changed != 1 {
		t.Fatalf("before done %+v", v)
	}
	wt := v.WorktreePath
	if err := e.c.Done(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ = e.c.Thread(v.ID); v.Status != StatusArchived || v.DoneAt.IsZero() || v.CanDone {
		t.Fatalf("done %+v", v)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Fatal("worktree kept")
	}
	if got := mustGit(t, e.repo, "show", v.Branch+":keep.txt"); got != "keep me" {
		t.Fatalf("branch lost the work: %q", got)
	}
	if err := e.c.Done(ctx, v.ID); err == nil {
		t.Fatal("marked done twice")
	}
	if err := e.c.Restore(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(wt, "keep.txt")); err != nil || string(b) != "keep me\n" {
		t.Fatalf("restored without its work: %q %v", b, err)
	}
	waitFor(t, "the count", func() bool { v, _ = e.c.Thread(v.ID); return v.Changed >= 0 })
	if v.Changed != 1 || !v.Unlanded {
		t.Fatalf("restored %+v", v)
	}
}
