package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/agent/agenttest"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/store"
)

func TestMain(m *testing.M) {
	if agenttest.Main() {
		return
	}
	// Isolate git from the user's config (signing, hooks) and give it an identity.
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.com",
		"GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.com",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}

type env struct {
	c       *Core
	dataDir string
	root    string
	repo    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	os.MkdirAll(repo, 0o755)
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		if _, err := git.Run(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{dataDir: t.TempDir(), root: root, repo: repo}
	e.c = e.open(t)
	if err := e.c.UpdateSettings(func(s *Settings) { s.Folders = []string{root} }); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) open(t *testing.T) *Core {
	t.Helper()
	exe, vars := agenttest.Command()
	c, err := New(Config{DataDir: e.dataDir, Agent: agent.Command{Path: exe, Env: vars},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// waitStatus waits until the thread has one of the statuses.
func waitStatus(t *testing.T, c *Core, id string, statuses ...string) ThreadView {
	t.Helper()
	changed := make(chan struct{}, 1)
	unsub := c.Subscribe(func(Change) {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	defer unsub()
	deadline := time.After(10 * time.Second)
	for {
		v, _ := c.Thread(id)
		for _, s := range statuses {
			if v.Status == s && !v.InTurn {
				return v
			}
		}
		select {
		case <-changed:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("thread %s is %s, want %v", id, v.Status, statuses)
		}
	}
}

func kinds(items []Item) string {
	var k []string
	for _, it := range items {
		k = append(k, it.Kind)
	}
	return strings.Join(k, ",")
}

func TestWorkerLifecycleAndMerge(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if got := e.c.RepoPaths(); len(got) != 1 || got[0] != e.repo {
		t.Fatalf("repos %v", got)
	}
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Add hello", Prompt: "write hello.txt hi there"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if v.Summary != "wrote hello.txt" || v.BaseBranch != "main" || !strings.HasPrefix(v.Branch, "solo/add-hello-") {
		t.Fatalf("thread %+v", v)
	}
	if _, err := os.Stat(filepath.Join(v.WorktreePath, "hello.txt")); err != nil {
		t.Fatal("file not written in the worktree:", err)
	}
	if _, err := os.Stat(filepath.Join(e.repo, "hello.txt")); err == nil {
		t.Fatal("file written in the main checkout")
	}

	items, _ := e.c.Items(ctx, v.ID, 0)
	if got := kinds(items); got != "notice,user,tool,assistant,turn" {
		t.Fatalf("items %s", got)
	}
	var tool ToolData
	items[2].Decode(&tool)
	if tool.Name != "Write" || !tool.Done || tool.Result != "File created" {
		t.Fatalf("tool item %+v", tool)
	}
	if items[1].TurnUUID == "" || items[1].TurnUUID != items[4].TurnUUID {
		t.Fatal("user message and turn end not bound by uuid")
	}

	changes, err := e.c.Changes(ctx, v.ID)
	if err != nil || len(changes) != 1 || changes[0].Path != "hello.txt" {
		t.Fatalf("changes %+v %v", changes, err)
	}

	if _, err := e.c.Merge(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = e.c.Thread(v.ID)
	if v.Status != StatusMerged || v.WorktreePath != "" {
		t.Fatalf("after merge %+v", v)
	}
	if b, err := os.ReadFile(filepath.Join(e.repo, "hello.txt")); err != nil || string(b) != "hi there\n" {
		t.Fatalf("merged file %q %v", b, err)
	}
	if err := e.c.Send(ctx, v.ID, "more"); err == nil {
		t.Fatal("a merged thread accepted a message")
	}
}

func TestWorkersAllStartAtOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 6; i++ {
		v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: fmt.Sprintf("W%d", i), Prompt: "slow"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, v.ID)
	}
	for _, id := range ids {
		waitStatusAllowTurn(t, e.c, id, StatusRunning)
	}
	for _, id := range ids {
		if err := e.c.Interrupt(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		waitStatus(t, e.c, id, StatusInterrupted)
	}
}

func TestQuestionWaitsForAnswer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: "ask"})
	v = waitStatusAllowTurn(t, e.c, v.ID, StatusWaiting)
	if !v.PendingAsk {
		t.Fatal("no pending question")
	}
	items, _ := e.c.Items(ctx, v.ID, 0)
	q := items[len(items)-1]
	if q.Kind != ItemQuestion {
		t.Fatalf("last item %s", q.Kind)
	}
	if err := e.c.Answer(v.ID, q.Ref, map[string]string{"Tabs or spaces?": "Spaces"}); err != nil {
		t.Fatal(err)
	}
	if v := waitStatus(t, e.c, v.ID, StatusIdle); v.Summary != "Spaces" {
		t.Fatalf("summary %q", v.Summary)
	}
	items, _ = e.c.Items(ctx, v.ID, 0)
	for _, it := range items {
		if it.Kind == ItemQuestion {
			var d QuestionData
			it.Decode(&d)
			if d.Answers["Tabs or spaces?"] != "Spaces" {
				t.Fatalf("question item not updated: %+v", d)
			}
		}
	}
}

// waitStatusAllowTurn is waitStatus without requiring the turn to have ended.
func waitStatusAllowTurn(t *testing.T, c *Core, id, status string) ThreadView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if v, _ := c.Thread(id); v.Status == status {
			return v
		}
		time.Sleep(20 * time.Millisecond)
	}
	v, _ := c.Thread(id)
	t.Fatalf("thread is %s, want %s", v.Status, status)
	return v
}

func TestToolHandler(t *testing.T) {
	e := newEnv(t)
	e.c.SetToolHandler(func(ctx context.Context, threadID, name string, args []byte) (string, bool) {
		return "handled " + name + " " + string(args), false
	})
	v, _ := e.c.CreateWorker(context.Background(), WorkerSpec{Repo: "proj", Prompt: `tool list_threads {"repo":"proj"}`})
	if v := waitStatus(t, e.c, v.ID, StatusIdle); v.Summary != `handled list_threads {"repo":"proj"}` {
		t.Fatalf("summary %q", v.Summary)
	}
}

func TestFailedTurn(t *testing.T) {
	e := newEnv(t)
	v, _ := e.c.CreateWorker(context.Background(), WorkerSpec{Repo: "proj", Prompt: "fail"})
	if v := waitStatus(t, e.c, v.ID, StatusFailed); v.Error != "scripted failure" {
		t.Fatalf("error %q", v.Error)
	}
}

func TestDiscard(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: "write x.txt y"})
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if err := e.c.Discard(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(v.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("worktree still exists")
	}
	if out, _ := git.Run(ctx, e.repo, "branch", "--list", v.Branch); out != "" {
		t.Fatalf("branch still exists: %q", out)
	}
	if v, _ := e.c.Thread(v.ID); v.Status != StatusDiscarded {
		t.Fatalf("status %s", v.Status)
	}
}

func TestRestartResumesThreads(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Keep", Prompt: "hello"})
	waitStatus(t, e.c, v.ID, StatusIdle)
	e.c.Close()

	c2 := e.open(t)
	v2, ok := c2.Thread(v.ID)
	if !ok || v2.Status != StatusIdle || !v2.HasTranscript {
		t.Fatalf("after restart %+v", v2)
	}
	if err := c2.Send(ctx, v.ID, "again"); err != nil {
		t.Fatal(err)
	}
	if v2 := waitStatus(t, c2, v.ID, StatusIdle); v2.Summary != "echo: again resume=true" {
		t.Fatalf("summary %q", v2.Summary)
	}
}

func TestResolveRepo(t *testing.T) {
	e := newEnv(t)
	if p, err := e.c.ResolveRepo("proj"); err != nil || p != e.repo {
		t.Fatalf("%q %v", p, err)
	}
	if _, err := e.c.ResolveRepo("nope"); err == nil {
		t.Fatal("resolved an unknown repo")
	}
}

func TestImages(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// A big screenshot is scaled down when imported; a small one is copied as it is.
	big := filepath.Join(e.root, "big.png")
	writePNG(t, big, 3000, 1500)
	a, err := e.c.ImportImage(big)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(a) != filepath.Join(e.dataDir, "attachments") {
		t.Fatalf("attachment %s", a)
	}
	if w, h := pngSize(t, a); w != 2000 || h != 1000 {
		t.Fatalf("imported %dx%d, want 2000x1000", w, h)
	}
	if _, err := e.c.ImportImage(filepath.Join(e.root, "proj")); err == nil {
		t.Fatal("imported a folder")
	}
	notImage := filepath.Join(e.root, "notes.txt")
	os.WriteFile(notImage, []byte("hello"), 0o644)
	if _, err := e.c.ImportImage(notImage); err == nil {
		t.Fatal("imported a text file")
	}
	pasted, err := e.c.SaveImage(image.NewRGBA(image.Rect(0, 0, 10, 10)))
	if err != nil {
		t.Fatal(err)
	}

	// A thread started with images gets them with its prompt, and they're kept on the message.
	small := filepath.Join(e.root, "small.png")
	writePNG(t, small, 40, 20)
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Look", Prompt: "look at this", Images: []string{small}})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if v.Summary != "echo: look at this +image/png resume=false" {
		t.Fatalf("summary %q", v.Summary)
	}
	items, _ := e.c.Items(ctx, v.ID, 0)
	var u UserData
	items[1].Decode(&u)
	if len(u.Images) != 1 || filepath.Dir(u.Images[0]) != filepath.Join(e.dataDir, "attachments") {
		t.Fatalf("user item %+v", u)
	}

	// A message can be images alone.
	if err := e.c.Send(ctx, v.ID, "", pasted, a); err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if v.Summary != "echo:  +image/png +image/png resume=false" {
		t.Fatalf("summary %q", v.Summary)
	}
	if err := e.c.Send(ctx, v.ID, "  "); err == nil {
		t.Fatal("sent an empty message")
	}

	// After a restart the history still has the images, and their files are there.
	e.c.Close()
	c := e.open(t)
	items, _ = c.Items(ctx, v.ID, 0)
	var got []string
	for _, it := range items {
		if it.Kind == ItemUser {
			var d UserData
			it.Decode(&d)
			got = append(got, d.Images...)
		}
	}
	if len(got) != 3 {
		t.Fatalf("images after restart: %v", got)
	}
	for _, p := range got {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
}

func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
}

func pngSize(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func TestDoneRestoreAndDiscardCleanup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	counted := func(id string) ThreadView {
		t.Helper()
		v := waitStatus(t, e.c, id, StatusIdle)
		for deadline := time.Now().Add(5 * time.Second); v.Changed < 0 && time.Now().Before(deadline); v, _ = e.c.Thread(id) {
			time.Sleep(20 * time.Millisecond)
		}
		return v
	}

	// A worker that changes nothing has nothing to merge; it stays until the user marks it done.
	shared, _ := e.c.SaveImage(image.NewRGBA(image.Rect(0, 0, 4, 4)))
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Look around", Prompt: "just answer", Images: []string{shared}})
	if err != nil {
		t.Fatal(err)
	}
	v = counted(v.ID)
	if !v.NoChanges || v.CanMerge || !v.CanDone || v.Changed != 0 || !v.DoneAt.IsZero() {
		t.Fatalf("no-change worker %+v", v)
	}
	if err := e.c.AddProposal(v.ID, "ship it"); err == nil {
		t.Fatal("proposed merging a thread with no changes")
	}
	wt := v.WorktreePath
	if err := e.c.Done(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = e.c.Thread(v.ID)
	if v.Status != StatusArchived || v.DoneAt.IsZero() || v.CanDone {
		t.Fatalf("done %+v", v)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Fatal("worktree kept after done")
	}

	// Restore brings it back at the same path, and it can work again (and its session resumes).
	if err := e.c.Restore(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = e.c.Thread(v.ID)
	if v.Status != StatusIdle || !v.DoneAt.IsZero() || v.WorktreePath != wt {
		t.Fatalf("restored %+v", v)
	}
	if err := e.c.Send(ctx, v.ID, "write b.txt more"); err != nil {
		t.Fatal(err)
	}
	// With changes that haven't landed it can still be marked done, after the user confirms. The thread
	// is idle until the message's turn begins, so wait for what the turn leaves, and not for idle.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if v, _ = e.c.Thread(v.ID); v.Status == StatusIdle && !v.InTurn && v.Changed == 1 {
			break
		}
	}
	if v.Changed != 1 || !v.CanMerge || !v.CanDone || !v.Unlanded {
		t.Fatalf("after more work %+v", v)
	}

	// A merged thread stays in the rail until marked done.
	if _, err := e.c.Merge(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = e.c.Thread(v.ID)
	if !v.DoneAt.IsZero() || !v.CanDone {
		t.Fatalf("merged %+v", v)
	}
	if err := e.c.Done(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ = e.c.Thread(v.ID); v.Status != StatusMerged || v.DoneAt.IsZero() {
		t.Fatalf("merged and done %+v", v)
	}

	// Discarding leaves the rail at once, deleting the thread's own images but not one another
	// thread still shows.
	own, _ := e.c.SaveImage(image.NewRGBA(image.Rect(0, 0, 4, 4)))
	w, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Write", Prompt: "write a.txt hi", Images: []string{shared, own}})
	if err != nil {
		t.Fatal(err)
	}
	counted(w.ID)
	if err := e.c.Discard(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if w, _ = e.c.Thread(w.ID); w.DoneAt.IsZero() {
		t.Fatal("discarded thread still in the rail")
	}
	if _, err := os.Stat(own); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded thread's image kept: %v", err)
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("shared image removed: %v", err)
	}

	// Done survives a restart.
	e.c.Close()
	c := e.open(t)
	if v, _ = c.Thread(v.ID); v.DoneAt.IsZero() {
		t.Fatal("done lost after restart")
	}
}

func TestBranchNamingAndLatestBase(t *testing.T) {
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
	// An origin that has moved on since the local checkout last pulled.
	origin := filepath.Join(e.root, "origin.git")
	run(e.root, "clone", "-q", "--bare", e.repo, origin)
	run(e.repo, "remote", "add", "origin", origin)
	run(e.repo, "fetch", "-q", "origin")
	other := filepath.Join(e.root, "other")
	run(e.root, "clone", "-q", origin, other)
	run(other, "commit", "-q", "--allow-empty", "-m", "upstream work")
	run(other, "push", "-q", "origin", "main")
	latest := run(other, "rev-parse", "HEAD")

	start := func(title, prompt string) ThreadView {
		t.Helper()
		v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: title, Prompt: prompt})
		if err != nil {
			t.Fatal(err)
		}
		return waitStatus(t, e.c, v.ID, StatusIdle)
	}

	// No rules: solo/<title>-<id>, from origin's latest main.
	v := start("Plain", "just answer")
	if !strings.HasPrefix(v.Branch, "solo/plain-") || v.BaseBranch != "main" || v.BaseCommit != latest {
		t.Fatalf("plain: branch %s base %s@%s, want main@%s", v.Branch, v.BaseBranch, v.BaseCommit, latest)
	}

	// Rules: the naming model names it; a taken name gets a suffix; a repo's rules win.
	e.c.UpdateSettings(func(s *Settings) { s.BranchRules = "feat/<summary>" })
	if v = start("Add login", "just answer"); v.Branch != "feat/add-login" {
		t.Fatalf("named %s", v.Branch)
	}
	if v = start("Add login", "just answer"); v.Branch != "feat/add-login-2" {
		t.Fatalf("second name %s", v.Branch)
	}
	if v = start("Odd", "BADNAME please"); !strings.HasPrefix(v.Branch, "solo/odd-") {
		t.Fatalf("bad answer kept: %s", v.Branch)
	}
	e.c.UpdateSettings(func(s *Settings) { s.BranchRules = ""; s.RepoBranchRules = map[string]string{e.repo: "feat/<summary>"} })
	if v = start("Repo rules", "just answer"); v.Branch != "feat/repo-rules" {
		t.Fatalf("repo rules: %s", v.Branch)
	}
}

// fakeGH puts a gh on PATH that opens pull request #7, answers views from view.json in its folder,
// and logs merges.
func fakeGH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
d="$(dirname "$0")"
case "$1 $2" in
"pr create") echo "$@" > "$d/create"; echo "Creating pull request"; echo "https://github.com/acme/proj/pull/7" ;;
"pr view") cat "$d/view.json" ;;
"pr merge") echo "$@" >> "$d/merge" ;;
*) echo "unexpected: $@" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func setView(t *testing.T, dir, state, merge string, checks ...string) {
	t.Helper()
	var rollup []string
	for _, c := range checks {
		name, concl, _ := strings.Cut(c, "=")
		status := "COMPLETED"
		if concl == "PENDING" {
			status, concl = "IN_PROGRESS", ""
		}
		rollup = append(rollup, fmt.Sprintf(`{"__typename":"CheckRun","name":%q,"status":%q,"conclusion":%q,"detailsUrl":"https://ci/%s"}`, name, status, concl, name))
	}
	j := fmt.Sprintf(`{"number":7,"url":"https://github.com/acme/proj/pull/7","state":%q,"isDraft":%t,"autoMergeRequest":null,"mergeStateStatus":%q,"reviewDecision":"","statusCheckRollup":[%s]}`,
		state, merge == "DRAFT", merge, strings.Join(rollup, ","))
	if err := os.WriteFile(filepath.Join(dir, "view.json"), []byte(j), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPullRequestFlow(t *testing.T) {
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
	e.c.UpdateSettings(func(s *Settings) { s.AutoMerge = true })
	events := make(chan string, 20)
	e.c.Subscribe(func(ch Change) {
		if ch.PREvent != "" {
			events <- ch.PREvent
		}
	})
	waitEvent := func(want string) {
		t.Helper()
		for {
			select {
			case got := <-events:
				if got == want {
					return
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no %s event", want)
			}
		}
	}
	waitCounted := func(id string) ThreadView {
		t.Helper()
		v := waitStatus(t, e.c, id, StatusIdle)
		for deadline := time.Now().Add(5 * time.Second); (v.Changed < 0 || v.Forge == "") && time.Now().Before(deadline); v, _ = e.c.Thread(id) {
			time.Sleep(20 * time.Millisecond)
		}
		return v
	}

	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Add hello", Prompt: "write hello.txt hi"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitCounted(v.ID)
	if v.Forge != "github" || !v.CanOpenPR || v.CanMerge || !v.CanMergeLocal {
		t.Fatalf("before the PR %+v", v)
	}
	if _, err := e.c.Merge(ctx, v.ID); err == nil {
		t.Fatal("merged locally in a repository with a forge")
	}
	// Solo proposes it; proposing again replaces the card rather than adding a second.
	if _, err := e.c.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	proposals := func() (all []Item, last Item) {
		t.Helper()
		items, err := e.c.Items(ctx, OrchestratorID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Kind == ItemProposal && it.Ref == v.ID {
				all = append(all, it)
			}
		}
		return all, items[len(items)-1]
	}
	for _, summary := range []string{"first", "ship it"} {
		e.c.addNotice(OrchestratorID, "info", "the conversation moves on")
		if err := e.c.AddProposal(v.ID, summary); err != nil {
			t.Fatal(err)
		}
	}
	if all, last := proposals(); len(all) != 1 || last.ID != all[0].ID || !strings.Contains(string(last.Data), "ship it") {
		t.Fatalf("proposed twice: %d cards, last item %+v", len(all), last)
	}

	// Opening pushes the branch, opens the PR against main and turns on auto-merge.
	setView(t, gh, "OPEN", "BLOCKED", "test=PENDING")
	pr, err := e.c.OpenPR(ctx, v.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(PROpened)
	if pr.Number != 7 || pr.State != "open" || pr.Checks != "pending" || !pr.AutoMerge {
		t.Fatalf("pr %+v", pr)
	}
	create, _ := os.ReadFile(filepath.Join(gh, "create"))
	if !strings.Contains(string(create), "--base main --head "+v.Branch) || !strings.Contains(string(create), "--title Add hello --body Did it.") {
		t.Fatalf("gh pr create %s", create)
	}
	if merge, _ := os.ReadFile(filepath.Join(gh, "merge")); !strings.Contains(string(merge), "--auto --squash") {
		t.Fatalf("auto-merge: %q", merge)
	}
	if remote := run(e.repo, "ls-remote", "origin", v.Branch); remote == "" {
		t.Fatal("branch not pushed")
	}
	if v, _ = e.c.Thread(v.ID); v.CanOpenPR || v.CanMerge || !v.CanDone || !v.Unlanded {
		t.Fatalf("with the PR open %+v", v)
	}

	// Failing checks are noticed; new commits are pushed to the PR.
	setView(t, gh, "OPEN", "BLOCKED", "test=FAILURE", "lint=SUCCESS")
	e.c.CheckPRs(ctx)
	waitEvent(PRChecksFailed)
	if v, _ = e.c.Thread(v.ID); v.PR.Checks != "failing" || len(v.PR.Failed) != 1 || v.PR.Failed[0].Name != "test" || v.PR.Blocked != "checks failed" {
		t.Fatalf("failing %+v", v.PR)
	}
	failedCard, _ := proposals()
	if len(failedCard) != 1 || !strings.Contains(string(failedCard[0].Data), `"checksFailed":true`) {
		t.Fatalf("card after failing checks %+v", failedCard)
	}
	e.c.addNotice(OrchestratorID, "info", "the conversation moves on")
	if err := e.c.Send(ctx, v.ID, "write fix.txt fixed"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
	run(v.WorktreePath, "add", "-A")
	run(v.WorktreePath, "commit", "-q", "-m", "fix")
	e.c.countChanges(v.ID)
	head := run(v.WorktreePath, "rev-parse", "HEAD")
	if remote := run(e.repo, "ls-remote", "origin", v.Branch); !strings.HasPrefix(remote, head) {
		t.Fatalf("new commit not pushed: remote %q, local %s", remote, head)
	}

	// Passing and mergeable: Merge merges the PR; when the forge says merged, the thread closes.
	setView(t, gh, "OPEN", "CLEAN", "test=SUCCESS")
	e.c.CheckPRs(ctx)
	waitEvent(PRChecksPassed)
	if v, _ = e.c.Thread(v.ID); !v.CanMerge {
		t.Fatalf("mergeable PR can't merge: %+v", v.PR)
	}
	// Checks recovered: the card is posted anew at the end, and the old one is gone.
	if all, last := proposals(); len(all) != 1 || last.ID != all[0].ID || all[0].ID == failedCard[0].ID ||
		strings.Contains(string(last.Data), "checksFailed") || !strings.Contains(string(last.Data), "ship it") {
		t.Fatalf("card after checks recovered: %d cards, last item %+v", len(all), last)
	}
	setView(t, gh, "MERGED", "UNKNOWN")
	if _, err := e.c.Merge(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	waitEvent(PRMerged)
	if v, _ = e.c.Thread(v.ID); v.Status != StatusMerged || v.WorktreePath != "" || !v.CanDone {
		t.Fatalf("after merge %+v", v)
	}
	if merge, _ := os.ReadFile(filepath.Join(gh, "merge")); !strings.Contains(string(merge), "pr merge https://github.com/acme/proj/pull/7 --squash") {
		t.Fatalf("merge log %q", merge)
	}
}

// New commits pushed to an open pull request post the thread's merge card anew at the end of Solo's
// chat, with its summary, once Solo is between turns with no updates on their way to it.
func TestPushRepostsCard(t *testing.T) {
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
	var busy atomic.Bool
	e.c.SetSoloBusy(busy.Load)
	events := make(chan string, 20)
	e.c.Subscribe(func(ch Change) {
		if ch.PREvent != "" {
			events <- ch.PREvent
		}
	})
	waitEvent := func(want string) {
		t.Helper()
		for {
			select {
			case got := <-events:
				if got == want {
					return
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no %s event", want)
			}
		}
	}
	if _, err := e.c.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Add hello", Prompt: "write hello.txt hi"})
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
	waitEvent(PROpened)

	cards := func() (all []Item, last Item) {
		t.Helper()
		items, err := e.c.Items(ctx, OrchestratorID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Kind == ItemProposal && it.Ref == v.ID {
				all = append(all, it)
			}
		}
		if len(items) > 0 {
			last = items[len(items)-1]
		}
		return all, last
	}
	n := 0
	push := func() {
		t.Helper()
		n++
		os.WriteFile(filepath.Join(v.WorktreePath, fmt.Sprintf("more%d.txt", n)), []byte("more"), 0o644)
		run(v.WorktreePath, "add", "-A")
		run(v.WorktreePath, "commit", "-q", "-m", fmt.Sprintf("more %d", n))
		e.c.countChanges(v.ID)
	}
	// reposted waits for the thread's one card to be a new one, the last item in Solo's chat.
	reposted := func(old Item) Item {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			all, last := cards()
			if len(all) == 1 && all[0].ID != old.ID && last.ID == all[0].ID {
				if !strings.Contains(string(last.Data), "ship it") {
					t.Fatalf("summary not kept: %s", last.Data)
				}
				return last
			}
			if time.Now().After(deadline) {
				t.Fatalf("card not posted anew: %d cards, last item %+v", len(all), last)
			}
		}
	}
	unmoved := func(old Item, when string) {
		t.Helper()
		if all, _ := cards(); len(all) != 1 || all[0].ID != old.ID {
			t.Fatalf("%s: cards %+v, want the one card %d left in place", when, all, old.ID)
		}
	}

	// A push with no card posts none.
	push()
	if all, _ := cards(); len(all) != 0 {
		t.Fatalf("a card appeared for a thread that had none: %+v", all)
	}
	// Solo can propose a thread whose pull request is open but can't merge yet.
	if v, _ = e.c.Thread(v.ID); v.CanMerge || v.CanOpenPR {
		t.Fatalf("want an open PR that can't merge yet: %+v", v)
	}
	if err := e.c.AddProposal(v.ID, "ship it"); err != nil {
		t.Fatal(err)
	}
	card, _ := cards()

	// While Solo has an update on its way, the card waits; it moves after Solo's next turn.
	busy.Store(true)
	push()
	head := run(v.WorktreePath, "rev-parse", "HEAD")
	if remote := run(e.repo, "ls-remote", "origin", v.Branch); !strings.HasPrefix(remote, head) {
		t.Fatalf("new commit not pushed: remote %q, local %s", remote, head)
	}
	unmoved(card[0], "Solo busy")
	busy.Store(false)
	if err := e.c.Send(ctx, OrchestratorID, "what's new?"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, OrchestratorID, StatusIdle)
	c := reposted(card[0])

	// Nothing new to push: the card stays.
	e.c.addNotice(OrchestratorID, "info", "the conversation moves on")
	e.c.countChanges(v.ID)
	unmoved(c, "nothing pushed")

	// Solo idle: the card moves as the commits are pushed.
	push()
	c = reposted(c)

	// Closed: no more pushes, and the card stays where it is.
	setView(t, gh, "CLOSED", "UNKNOWN")
	e.c.CheckPRs(ctx)
	waitEvent(PRClosed)
	e.c.addNotice(OrchestratorID, "info", "the conversation moves on")
	push()
	e.c.cardStale(v.ID)
	unmoved(c, "PR closed")
}

func TestDraftPullRequest(t *testing.T) {
	gh := fakeGH(t)
	// gh pr ready is logged with the merges.
	script, _ := os.ReadFile(filepath.Join(gh, "gh"))
	os.WriteFile(filepath.Join(gh, "gh"), []byte(strings.Replace(string(script), `"pr merge")`, `"pr ready"|"pr merge")`, 1)), 0o755)
	e := newEnv(t)
	ctx := context.Background()
	origin := filepath.Join(e.root, "origin.git")
	for _, args := range [][]string{{"clone", "-q", "--bare", e.repo, origin}} {
		if _, err := git.Run(ctx, e.root, args...); err != nil {
			t.Fatal(err)
		}
	}
	git.Run(ctx, e.repo, "remote", "add", "origin", origin)
	git.Run(ctx, e.repo, "config", "solo.forge", "github")
	e.c.UpdateSettings(func(s *Settings) { s.AutoMerge, s.SkipPRDescription = true, true })

	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Add hello", Prompt: "write hello.txt hi"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if v, _ = e.c.Thread(v.ID); v.CanOpenPR {
			break
		}
	}
	setView(t, gh, "OPEN", "DRAFT")
	pr, err := e.c.OpenPR(ctx, v.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	create, _ := os.ReadFile(filepath.Join(gh, "create"))
	if !strings.HasSuffix(strings.TrimSpace(string(create)), "--title Add hello --body  --draft") {
		t.Fatalf("draft create without a description: %q", create)
	}
	if merge, _ := os.ReadFile(filepath.Join(gh, "merge")); len(merge) > 0 || pr.AutoMerge {
		t.Fatalf("auto-merge turned on for a draft: %q", merge)
	}
	if v, _ = e.c.Thread(v.ID); !v.CanMarkReady || v.CanMerge {
		t.Fatalf("draft %+v", v)
	}
	if err := e.c.MarkReady(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	merge, _ := os.ReadFile(filepath.Join(gh, "merge"))
	if !strings.Contains(string(merge), "pr ready https://github.com/acme/proj/pull/7") || !strings.Contains(string(merge), "--auto --squash") {
		t.Fatalf("ready and auto-merge: %q", merge)
	}
	if v, _ = e.c.Thread(v.ID); v.CanMarkReady || v.PR.Draft || !v.PR.AutoMerge {
		t.Fatalf("after ready %+v", v.PR)
	}
}

func TestParsePRText(t *testing.T) {
	for _, c := range []struct{ in, title, desc string }{
		{"TITLE: feat: add login\nDESCRIPTION:\nAdds it.\n\n- one", "feat: add login", "Adds it.\n\n- one"},
		{"```\nTITLE: \"Fix it\"\n```", "Fix it", ""},
		{"Sure! Here's a title", "", ""},
		// An agent's answer after what it did, fenced, with a code block of its own.
		{"I ran the skill.\n\n```markdown\nTITLE: feat: x\nDESCRIPTION:\nRun:\n```sh\nmake\n```\n```", "feat: x", "Run:\n```sh\nmake\n```"},
		{"Done.\nTITLE: fix: y\nDESCRIPTION:\nEnds in code:\n```\nz\n```", "fix: y", "Ends in code:\n```\nz\n```"},
	} {
		if title, desc := parsePRText(c.in); title != c.title || desc != c.desc {
			t.Errorf("parsePRText(%q) = %q, %q", c.in, title, desc)
		}
	}
}

// fakeGHMulti is a gh for several repositories: each pull request is numbered in turn and named after
// the folder gh runs in; views answer from view.json with the asked URL and number filled in.
func fakeGHMulti(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
d="$(dirname "$0")"
case "$1 $2" in
"pr create")
  n=$(( $(cat "$d/n" 2>/dev/null || echo 0) + 1 )); echo $n > "$d/n"
  echo "$(basename "$PWD") $@" >> "$d/create"
  echo "https://github.com/acme/$(basename "$PWD")/pull/$n" ;;
"pr view") num="${3##*/}"; sed -e "s|__URL__|$3|" -e "s|__NUM__|$num|" "$d/view.json" ;;
"pr edit") echo "$@" >> "$d/edit" ;;
"pr merge") echo "$@" >> "$d/merge" ;;
*) echo "unexpected: $@" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func setViewMulti(t *testing.T, dir, state, merge string) {
	t.Helper()
	j := fmt.Sprintf(`{"number":__NUM__,"url":"__URL__","state":%q,"isDraft":false,"autoMergeRequest":null,"mergeStateStatus":%q,"reviewDecision":"","statusCheckRollup":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}`, state, merge)
	if err := os.WriteFile(filepath.Join(dir, "view.json"), []byte(j), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMultiRepoThread(t *testing.T) {
	gh := fakeGHMulti(t)
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
	// Three repositories, each with an origin on GitHub.
	repos := map[string]string{}
	for _, name := range []string{"client", "api", "db"} {
		r := filepath.Join(e.root, name)
		os.MkdirAll(r, 0o755)
		run(r, "init", "-q", "-b", "main")
		run(r, "commit", "-q", "--allow-empty", "-m", "init")
		origin := filepath.Join(e.root, name+".git")
		run(e.root, "clone", "-q", "--bare", r, origin)
		run(r, "remote", "add", "origin", origin)
		run(r, "config", "solo.forge", "github")
		repos[name] = r
	}
	e.c.RescanRepos()

	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repos: []string{"client", "api", "db"}, Title: "Audit export", Prompt: "write api/handler.go hi"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if !v.MultiRepo() || len(v.Parts) != 3 {
		t.Fatalf("parts %+v", v.Parts)
	}
	dir := e.c.threadDir(v.Thread)
	for i, name := range []string{"client", "api", "db"} {
		p := v.Parts[i]
		if p.Worktree != filepath.Join(dir, name) || p.Branch != v.Parts[0].Branch || p.BaseBranch != "main" {
			t.Fatalf("part %s: %+v", name, p.Part)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "api", "handler.go")); err != nil || string(b) != "hi\n" {
		t.Fatalf("the session didn't run in the thread folder: %q %v", b, err)
	}
	if err := e.c.Send(ctx, v.ID, "write db/0042_audit.sql create"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if v, _ = e.c.Thread(v.ID); v.Parts[2].Changed == 1 && v.Parts[0].Forge != "" {
			break
		}
	}
	if v.Changed != 2 || v.Parts[0].Changed != 0 || v.Parts[0].CanOpenPR || !v.Parts[1].CanOpenPR || !v.Parts[2].CanOpenPR || v.PR != nil {
		t.Fatalf("after work: changed %d, parts %+v", v.Changed, v.Parts)
	}
	changes, err := e.c.Changes(ctx, v.ID)
	if err != nil || len(changes) != 2 || changes[0].Path != "api/handler.go" || changes[1].Path != "db/0042_audit.sql" {
		t.Fatalf("changes %+v %v", changes, err)
	}
	if d, _ := e.c.Diff(ctx, v.ID); !strings.Contains(d, "b/api/handler.go") || !strings.Contains(d, "b/db/0042_audit.sql") {
		t.Fatalf("diff:\n%s", d)
	}

	// One click opens a PR in each repository it changed, linked to each other.
	setViewMulti(t, gh, "OPEN", "BLOCKED")
	prs, err := e.c.OpenPRFor(ctx, v.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 {
		t.Fatalf("opened %+v", prs)
	}
	create, _ := os.ReadFile(filepath.Join(gh, "create"))
	if strings.Contains(string(create), "client ") || !strings.Contains(string(create), "api ") || !strings.Contains(string(create), "db ") {
		t.Fatalf("gh pr create:\n%s", create)
	}
	edit, _ := os.ReadFile(filepath.Join(gh, "edit"))
	if !strings.Contains(string(edit), "- db: https://github.com/acme/db/pull/2") || !strings.Contains(string(edit), "- api: https://github.com/acme/api/pull/1") {
		t.Fatalf("links:\n%s", edit)
	}
	for _, r := range []string{"api", "db"} {
		if run(repos[r], "ls-remote", "origin", v.Parts[0].Branch) == "" {
			t.Fatalf("%s's branch not pushed", r)
		}
	}

	// One card for the thread; new commits in both repositories post it anew once, at the end.
	if _, err := e.c.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	if err := e.c.AddProposal(v.ID, "ship it"); err != nil {
		t.Fatal(err)
	}
	cards := func() (all []Item, last Item) {
		items, _ := e.c.Items(ctx, OrchestratorID, 0)
		for _, it := range items {
			if it.Kind == ItemProposal && it.Ref == v.ID {
				all = append(all, it)
			}
		}
		return all, items[len(items)-1]
	}
	before, _ := cards()
	e.c.addNotice(OrchestratorID, "info", "the conversation moves on")
	for _, name := range []string{"api", "db"} {
		wt := filepath.Join(dir, name)
		os.WriteFile(filepath.Join(wt, "more.txt"), []byte("more"), 0o644)
		run(wt, "add", "-A")
		run(wt, "commit", "-q", "-m", "more")
	}
	e.c.countChanges(v.ID)
	for _, name := range []string{"api", "db"} {
		if head := run(filepath.Join(dir, name), "rev-parse", "HEAD"); !strings.HasPrefix(run(repos[name], "ls-remote", "origin", v.Parts[0].Branch), head) {
			t.Fatalf("%s's new commit not pushed", name)
		}
	}
	if all, last := cards(); len(before) != 1 || len(all) != 1 || all[0].ID == before[0].ID || last.ID != all[0].ID ||
		!strings.Contains(string(last.Data), "ship it") {
		t.Fatalf("card after pushing to both PRs: %d cards (was %+v), last item %+v", len(all), before, last)
	}

	// Merging one repository leaves the thread open; when the other merges, it closes.
	setViewMulti(t, gh, "OPEN", "CLEAN")
	e.c.CheckPRs(ctx)
	if v, _ = e.c.Thread(v.ID); !v.Parts[1].CanMerge || !v.Parts[2].CanMerge {
		t.Fatalf("mergeable parts %+v", v.Parts)
	}
	if _, err := e.c.MergeRepo(ctx, v.ID, "db"); err != nil {
		t.Fatal(err)
	}
	merge, _ := os.ReadFile(filepath.Join(gh, "merge"))
	if !strings.Contains(string(merge), "acme/db/pull/2") || strings.Contains(string(merge), "acme/api") {
		t.Fatalf("merge log %q", merge)
	}
	setViewMulti(t, gh, "MERGED", "UNKNOWN")
	e.c.CheckPRs(ctx)
	v, _ = e.c.Thread(v.ID)
	if v.Status != StatusMerged || v.HasWorktree || !v.CanDone {
		t.Fatalf("after both merged: %+v", v)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("thread folder kept")
	}

	// Done, then Restore: every repository's worktree comes back in the thread folder.
	if err := e.c.Done(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Restore(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = e.c.Thread(v.ID)
	for _, name := range []string{"client", "api", "db"} {
		if _, err := os.Stat(filepath.Join(dir, name, ".git")); err != nil {
			t.Fatalf("%s not restored: %v", name, err)
		}
	}
	if v.Status != StatusIdle || v.Parts[1].PR != nil {
		t.Fatalf("restored %+v", v)
	}
}

func TestSearchMessages(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Tokens", Prompt: "refresh the OAuth Token on a 401"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
	hits, err := e.c.SearchMessages(ctx, "oauth 401", 10)
	if err != nil {
		t.Fatal(err)
	}
	// The prompt and the fake's echo of it both match.
	if len(hits) != 2 || hits[0].ThreadID != v.ID || hits[0].Kind != ItemAssistant || !strings.Contains(hits[0].Snippet, "OAuth Token") {
		t.Fatalf("hits %+v", hits)
	}
	if hits, _ := e.c.SearchMessages(ctx, "oauth nowhere", 10); len(hits) != 0 {
		t.Fatalf("all words must match: %+v", hits)
	}
	if got := snippet(strings.Repeat("a", 100)+" needle "+strings.Repeat("b", 200), 101, 6); !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") || !strings.Contains(got, "needle") {
		t.Errorf("snippet %q", got)
	}
}

// A thread that was working when Solo closed carries on after a restart, resuming its session with a
// note; the steps it had running end instead of spinning. A thread the user stopped stays stopped.
func TestRestartCarriesOnWorkingThreads(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	busy, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Busy", Prompt: "hello"})
	waitStatus(t, e.c, busy.ID, StatusIdle)
	if err := e.c.Send(ctx, busy.ID, "slow"); err != nil {
		t.Fatal(err)
	}
	waitStatusAllowTurn(t, e.c, busy.ID, StatusRunning)
	tool, _ := json.Marshal(ToolData{ID: "b1", Name: "Bash", Input: json.RawMessage(`{"command":"sleep 100","description":"Run the slow suite"}`)})
	if err := e.c.db.AddItem(&store.Item{ThreadID: busy.ID, Kind: ItemTool, Ref: "b1", Data: tool, CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	stopped, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Stopped", Prompt: "hello"})
	waitStatus(t, e.c, stopped.ID, StatusIdle)
	e.c.Send(ctx, stopped.ID, "slow")
	waitStatusAllowTurn(t, e.c, stopped.ID, StatusRunning)
	if err := e.c.Interrupt(ctx, stopped.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, stopped.ID, StatusInterrupted)
	e.c.Close()

	c2 := e.open(t)
	if v, _ := c2.Thread(busy.ID); v.Status != StatusInterrupted {
		t.Fatalf("before resuming: %s", v.Status)
	}
	items, _ := c2.Items(ctx, busy.ID, 0)
	for _, it := range items {
		if it.Kind == ItemTool {
			var d ToolData
			it.Decode(&d)
			if d.ID == "b1" && !d.Done {
				t.Fatal("the running command still spins after the restart")
			}
		}
	}
	c2.ResumeInterrupted()
	v := waitStatus(t, c2, busy.ID, StatusIdle)
	if !strings.HasPrefix(v.Summary, "echo: Solo was closed while you were working") || !strings.HasSuffix(v.Summary, "resume=true") ||
		!strings.Contains(v.Summary, "\n- command: Run the slow suite (sleep 100)") {
		t.Fatalf("summary %q", v.Summary)
	}
	time.Sleep(100 * time.Millisecond)
	if v, _ := c2.Thread(stopped.ID); v.Status != StatusInterrupted {
		t.Fatalf("the thread the user stopped is %s", v.Status)
	}
}

// runningAtClose leaves a worker mid-turn and closes Solo, returning the thread's ID.
func runningAtClose(t *testing.T, e *env) string {
	t.Helper()
	ctx := context.Background()
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Busy", Prompt: "hello"})
	waitStatus(t, e.c, v.ID, StatusIdle)
	if err := e.c.Send(ctx, v.ID, "slow"); err != nil {
		t.Fatal(err)
	}
	waitStatusAllowTurn(t, e.c, v.ID, StatusRunning)
	e.c.Close()
	return v.ID
}

// A thread Solo already carried on resumeMax times in a few minutes stays stopped, so a crash it causes
// can't loop.
func TestRestartLoopGuard(t *testing.T) {
	e := newEnv(t)
	id := runningAtClose(t, e)
	c2 := e.open(t)
	ms := time.Now().UnixMilli()
	if err := c2.db.SetSetting(resumeLogKey, map[string][]int64{id: {ms - 3000, ms - 2000, ms - 1000}}); err != nil {
		t.Fatal(err)
	}
	c2.ResumeInterrupted()
	time.Sleep(200 * time.Millisecond)
	if v, _ := c2.Thread(id); v.Status != StatusInterrupted {
		t.Fatalf("a thread restarted too often is %s", v.Status)
	}
	items, _ := c2.Items(context.Background(), id, 0)
	var d NoticeData
	items[len(items)-1].Decode(&d)
	if !strings.Contains(d.Text, "stays stopped") {
		t.Fatalf("last notice %q", d.Text)
	}
}

// With the setting off, threads that were working stay stopped after a restart.
func TestRestartResumeOff(t *testing.T) {
	e := newEnv(t)
	if err := e.c.UpdateSettings(func(s *Settings) { s.NoResumeAfterRestart = true }); err != nil {
		t.Fatal(err)
	}
	id := runningAtClose(t, e)
	c2 := e.open(t)
	c2.ResumeInterrupted()
	time.Sleep(200 * time.Millisecond)
	if v, _ := c2.Thread(id); v.Status != StatusInterrupted {
		t.Fatalf("with resuming off the thread is %s", v.Status)
	}
}

// A thread waiting on a question when Solo closed still waits on it after a restart, and the answer
// reaches it as a message that resumes its session.
func TestRestartKeepsQuestion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: "ask"})
	waitStatusAllowTurn(t, e.c, v.ID, StatusWaiting)
	e.c.Close()

	c2 := e.open(t)
	c2.ResumeInterrupted()
	v2, _ := c2.Thread(v.ID)
	qs := c2.PendingQuestions(v.ID)
	if v2.Status != StatusWaiting || !v2.PendingAsk || len(qs) != 1 || qs[0].Questions[0].Question != "Tabs or spaces?" {
		t.Fatalf("after restart: %s, pending %v, %+v", v2.Status, v2.PendingAsk, qs)
	}
	answered := watchAnswers(c2)
	if err := c2.Answer(v.ID, qs[0].ToolUseID, map[string]string{"Tabs or spaces?": "Spaces"}); err != nil {
		t.Fatal(err)
	}
	if got := answered(); len(got) != 1 || got[0].From != FromUser || got[0].Answers["Tabs or spaces?"] != "Spaces" ||
		len(got[0].Questions) != 1 || got[0].Questions[0].Question != "Tabs or spaces?" {
		t.Fatalf("answers reported %+v", got)
	}
	var v3 ThreadView
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if v3, _ = c2.Thread(v.ID); v3.Status == StatusIdle && v3.Summary != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(v3.Summary, "Tabs or spaces?\n  Their answer: Spaces") || !strings.HasSuffix(v3.Summary, "resume=true") {
		t.Fatalf("summary %q", v3.Summary)
	}
	if len(c2.PendingQuestions(v.ID)) != 0 {
		t.Fatal("the question is still pending")
	}
	items, _ := c2.Items(ctx, v.ID, 0)
	for _, it := range items {
		if it.Kind == ItemQuestion {
			var d QuestionData
			it.Decode(&d)
			if d.Answers["Tabs or spaces?"] != "Spaces" {
				t.Fatalf("question row not answered: %+v", d)
			}
		}
	}
}

// watchAnswers collects the answers c reports (ChangeAnswer).
func watchAnswers(c *Core) func() []Answered {
	var mu sync.Mutex
	var got []Answered
	c.Subscribe(func(ch Change) {
		if ch.Kind == ChangeAnswer {
			mu.Lock()
			got = append(got, *ch.Answer)
			mu.Unlock()
		}
	})
	return func() []Answered {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// Answering or skipping a question is reported with the question, the answer and who gave it.
func TestAnswersAreReported(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	answered := watchAnswers(e.c)
	resolve := []func(id, key string) error{
		func(id, key string) error { return e.c.Answer(id, key, map[string]string{"Tabs or spaces?": "Tabs"}) },
		func(id, key string) error { return e.c.Dismiss(id, key) },
		func(id, key string) error {
			return e.c.AnswerFromOrchestrator(id, key, map[string]string{"Tabs or spaces?": "Spaces"})
		},
	}
	for _, f := range resolve {
		v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: "ask"})
		waitStatusAllowTurn(t, e.c, v.ID, StatusWaiting)
		qs := e.c.PendingQuestions(v.ID)
		if err := f(v.ID, qs[0].ToolUseID); err != nil {
			t.Fatal(err)
		}
		waitStatus(t, e.c, v.ID, StatusIdle)
	}
	got := answered()
	if len(got) != 3 {
		t.Fatalf("%d answers reported: %+v", len(got), got)
	}
	for i, want := range []Answered{
		{Answers: map[string]string{"Tabs or spaces?": "Tabs"}, From: FromUser},
		{Skipped: true, From: FromUser},
		{Answers: map[string]string{"Tabs or spaces?": "Spaces"}, From: FromOrchestrator},
	} {
		g := got[i]
		if g.From != want.From || g.Skipped != want.Skipped || !maps.Equal(g.Answers, want.Answers) ||
			len(g.Questions) != 1 || g.Questions[0].Question != "Tabs or spaces?" {
			t.Fatalf("answer %d: %+v, want %+v", i, g, want)
		}
	}
}

func TestSteerCarriesOnTheTurn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: "slow"})
	deadline := time.Now().Add(10 * time.Second)
	for v, _ = e.c.Thread(v.ID); !v.InTurn; v, _ = e.c.Thread(v.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the turn never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := e.c.Send(ctx, v.ID, "use tabs"); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.c.Thread(v.ID); len(v.Queued) != 0 {
		t.Fatalf("a message to a worker queued: %+v", v.Queued) // only Solo's messages wait for the turn
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle, StatusInterrupted)
	if v.Status != StatusIdle || !strings.HasPrefix(v.Summary, "echo: use tabs") {
		t.Fatalf("after the steer: %s, summary %q", v.Status, v.Summary)
	}
	items, _ := e.c.Items(ctx, v.ID, 0)
	turns, steered := 0, false
	for _, it := range items {
		switch it.Kind {
		case ItemTurn:
			turns++
		case ItemUser:
			var d UserData
			it.Decode(&d)
			steered = steered || (d.Text == "use tabs" && d.Steer)
		}
	}
	if turns != 1 || !steered {
		t.Fatalf("want one turn and a steered message; got %d turns, steered=%v (%s)", turns, steered, kinds(items))
	}
}

// Solo's own AskUserQuestion is refused while a worker's question is pinned, so the user isn't asked
// the same thing twice.
func TestOrchestratorAskRefusedWhileWorkerAsks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	w, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: "ask"})
	waitStatusAllowTurn(t, e.c, w.ID, StatusWaiting)
	if _, err := e.c.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Send(ctx, OrchestratorID, "ask"); err != nil {
		t.Fatal(err)
	}
	if v := waitStatus(t, e.c, OrchestratorID, StatusIdle); v.Summary != "cancelled" {
		t.Fatalf("orchestrator summary %q, want its ask refused", v.Summary)
	}
	items, _ := e.c.Items(ctx, OrchestratorID, 0)
	for _, it := range items {
		if it.Kind == ItemQuestion {
			t.Fatal("the refused question was shown")
		}
	}
}

// A merge that conflicts goes back to the worker to resolve, and merges when its turn ends; one that
// keeps conflicting stops after maxResolveTries and says so.
func TestMergeConflictGoesToTheWorker(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Add hello", Prompt: "write hello.txt hi there"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if err := os.WriteFile(filepath.Join(e.repo, "hello.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "other hello"}} {
		if _, err := git.Run(ctx, e.repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	_, err = e.c.Merge(ctx, v.ID)
	var re *ResolvingError
	if !errors.As(err, &re) {
		t.Fatalf("want the conflict handed to the worker, got %v", err)
	}
	// The fake worker only echoes, so the conflict stays; after the second hand-off, it stops. Between
	// its steps the thread is for a moment neither resolving nor in a turn, so wait for the outcome, the
	// notice that it gave up, and not for that moment.
	deadline := time.Now().Add(10 * time.Second)
	asked, failed := 0, false
	for {
		v, _ = e.c.Thread(v.ID)
		items, _ := e.c.Items(ctx, v.ID, 0)
		asked, failed = 0, false
		for _, it := range items {
			var u UserData
			if it.Kind == ItemUser && it.Decode(&u) == nil && strings.Contains(u.Text, "git merge main") {
				asked++
			}
			var n NoticeData
			if it.Kind == ItemNotice && it.Decode(&n) == nil && strings.Contains(n.Text, "still conflicts") {
				failed = true
			}
		}
		if failed && !v.Resolving && !v.InTurn && v.Status == StatusIdle {
			break
		}
		if time.Now().After(deadline) {
			var notices []string
			for _, it := range items {
				var n NoticeData
				if it.Kind == ItemNotice && it.Decode(&n) == nil {
					notices = append(notices, n.Text)
				}
			}
			t.Fatalf("still resolving: asked %d times, failure noticed %v, status %s, resolving %v, in a turn %v; notices: %q",
				asked, failed, v.Status, v.Resolving, v.InTurn, notices)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if asked != maxResolveTries {
		t.Fatalf("asked %d times, want %d", asked, maxResolveTries)
	}
}

// A worker that commits its work straight into the base branch has nothing left to land: the thread is
// merged, ready for Done.
func TestWorkCommittedToBaseLands(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Add hello", Prompt: "write hello.txt hi there"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "hello"}} {
		if _, err := git.Run(ctx, v.WorktreePath, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git.Run(ctx, e.repo, "merge", "-q", "--ff-only", v.Branch); err != nil {
		t.Fatal(err)
	}
	go e.c.countChanges(v.ID) // as at the end of a turn
	if v = waitStatus(t, e.c, v.ID, StatusMerged); !v.CanDone || v.Unlanded {
		t.Fatalf("status %s, can done %v", v.Status, v.CanDone)
	}
}
