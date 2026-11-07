package core

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/agent/agenttest"
	"github.com/tyler-builds/solo/internal/git"
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
