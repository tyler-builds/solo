package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/store"
)

func TestBriefAfterTurn(t *testing.T) {
	defer func(f, e time.Duration) { briefFirstWait, briefEvery = f, e }(briefFirstWait, briefEvery)
	briefFirstWait, briefEvery = 10*time.Millisecond, 50*time.Millisecond

	var mu sync.Mutex
	var prompts []string
	e := newEnv(t)
	e.summarize = func(ctx context.Context, req SummarizeRequest) (string, error) {
		mu.Lock()
		prompts = append(prompts, req.Prompt)
		mu.Unlock()
		if req.Model != "haiku" || req.System != briefSystem {
			t.Errorf("request %+v", req)
		}
		if strings.Contains(req.Prompt, "still running") {
			return `{"now": "Writing the file.", "turn": "- Writing hello"}`, nil
		}
		return "Here you go:\n```json\n" + `{"turn": "- Wrote hello.txt", "overall": "The hello file exists.", "need": "review"}` + "\n```", nil
	}
	e.c.Close()
	e.c = e.open(t)
	if err := e.c.UpdateSettings(func(s *Settings) { s.Board = true }); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Add hello", Prompt: "write hello.txt hi there"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
	v = waitBrief(t, e.c, v.ID, func(b *Brief) bool { return b.Need != "" })
	if v.Brief.Turn != "- Wrote hello.txt" || v.Brief.Overall != "The hello file exists." || v.Brief.Need != BriefNeedReview || v.Now != "" {
		t.Fatalf("brief %+v, now %q", v.Brief, v.Now)
	}
	mu.Lock()
	last := prompts[len(prompts)-1]
	mu.Unlock()
	for _, want := range []string{"Task: Add hello", "said: write hello.txt hi there", "Wrote ", "hello.txt", "The turn ended: completed", "(this is the task's first turn)"} {
		if !strings.Contains(last, want) {
			t.Errorf("prompt lacks %q:\n%s", want, last)
		}
	}

	// The brief is saved with the thread.
	e.c.Close()
	e.summarize = nil
	e.c = e.open(t)
	if v, _ := e.c.Thread(v.ID); v.Brief == nil || v.Brief.Overall != "The hello file exists." {
		t.Fatalf("after restart: %+v", v.Brief)
	}
}

func waitBrief(t *testing.T, c *Core, id string, ok func(*Brief) bool) ThreadView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, _ := c.Thread(id)
		if v.Brief != nil && ok(v.Brief) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("no brief: %+v", v.Brief)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBriefsOffWithoutSummarizer(t *testing.T) {
	e := newEnv(t)
	v, err := e.c.CreateWorker(context.Background(), WorkerSpec{Repo: "proj", Title: "Add hello", Prompt: "write hello.txt hi"})
	if err != nil {
		t.Fatal(err)
	}
	if v = waitStatus(t, e.c, v.ID, StatusIdle); v.Brief != nil {
		t.Fatalf("brief %+v", v.Brief)
	}
}

// Only the board shows briefs, so without it none are written.
func TestBriefsOffWithoutBoard(t *testing.T) {
	defer func(f, e time.Duration) { briefFirstWait, briefEvery = f, e }(briefFirstWait, briefEvery)
	briefFirstWait, briefEvery = 10*time.Millisecond, 50*time.Millisecond
	e := newEnv(t)
	calls := 0
	var mu sync.Mutex
	e.summarize = func(ctx context.Context, req SummarizeRequest) (string, error) {
		if req.System == briefSystem {
			mu.Lock()
			calls++
			mu.Unlock()
		}
		return `{"turn": "- x", "overall": "x", "need": "review"}`, nil
	}
	e.c.Close()
	e.c = e.open(t)
	v, err := e.c.CreateWorker(context.Background(), WorkerSpec{Repo: "proj", Title: "Add hello", Prompt: "write hello.txt hi"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 || v.Brief != nil {
		t.Fatalf("%d brief calls, brief %+v", calls, v.Brief)
	}
}

func item(kind string, data any) *store.Item {
	b, _ := json.Marshal(data)
	return &store.Item{Kind: kind, Data: b}
}

func TestTurnItems(t *testing.T) {
	items := []*store.Item{
		item(ItemUser, UserData{Text: "one"}), item(ItemTurn, TurnData{Status: "completed"}),
		item(ItemUser, UserData{Text: "two"}), item(ItemAssistant, TextData{Text: "a"}), item(ItemTurn, TurnData{Status: "completed"}),
		item(ItemUser, UserData{Text: "three"}),
	}
	if got := turnItems(items, false); len(got) != 1 || got[0] != items[5] {
		t.Fatalf("running turn %d items", len(got))
	}
	if got := turnItems(items[:5], true); len(got) != 3 || got[0] != items[2] || got[2] != items[4] {
		t.Fatalf("ended turn %d items", len(got))
	}
	if got := turnItems(items[:2], true); len(got) != 2 {
		t.Fatalf("first turn %d items", len(got))
	}
}

func TestBriefDigest(t *testing.T) {
	tool := func(name string, input map[string]any, isErr bool, result string) *store.Item {
		in, _ := json.Marshal(input)
		return item(ItemTool, ToolData{Name: name, Input: in, IsError: isErr, Result: result})
	}
	items := []*store.Item{
		item(ItemUser, UserData{Text: "fix login", Images: []string{"a.png"}}),
		item(ItemThinking, TextData{Text: "secret thoughts"}),
		tool("Read", map[string]any{"file_path": "/w/src/auth.go"}, false, ""),
		tool("Read", map[string]any{"file_path": "/w/src/user.go"}, false, ""),
		tool("Bash", map[string]any{"command": "go test ./...", "description": "Run tests"}, true, "FAIL auth\nmore"),
		tool("TodoWrite", map[string]any{"todos": []any{map[string]any{"content": "Fix it", "status": "in_progress"}}}, false, ""),
		tool("mcp__solo__spawn_thread", nil, false, ""),
		item(ItemAssistant, TextData{Text: Noted}),
		item(ItemAssistant, TextData{Text: "Fixed the token check."}),
	}
	got := briefDigest(items, true)
	want := strings.Join([]string{
		"The user said: fix login [with 1 attachment(s)]",
		"Read src/auth.go, src/user.go",
		"Ran: Run tests (go test ./...) — failed: FAIL auth",
		"Its plan: [>] Fix it",
		"Used spawn_thread",
		"Agent said: Fixed the token check.",
	}, "\n")
	if got != want {
		t.Fatalf("digest:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "secret") {
		t.Fatal("thinking reached the digest")
	}
}

func TestBriefDigestKeepsAskAndNewest(t *testing.T) {
	items := []*store.Item{item(ItemUser, UserData{Text: "the ask"})}
	for range 400 {
		items = append(items, item(ItemAssistant, TextData{Text: strings.Repeat("x", 100)}))
	}
	items = append(items, item(ItemAssistant, TextData{Text: "the end"}))
	got := briefDigest(items, false)
	if len(got) > briefDigestMax || !strings.HasPrefix(got, "The user said: the ask\n… (") || !strings.HasSuffix(got, "Agent said: the end") {
		t.Fatalf("digest (%d bytes) %q … %q", len(got), got[:60], got[len(got)-40:])
	}
}

func TestParseBrief(t *testing.T) {
	out, err := parseBrief(`{"now": "Running tests.", "turn": "- a", "need": "input"}`, false)
	if err != nil || out.Now != "Running tests" || out.Need != "" {
		t.Fatalf("%+v %v", out, err)
	}
	out, err = parseBrief(`{"turn": "- a", "overall": "o", "need": "whatever"}`, true)
	if err != nil || out.Need != BriefNeedReview {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := parseBrief("no json here", true); err == nil {
		t.Fatal("parsed text without JSON")
	}
	if _, err := parseBrief(`{"turn": ""}`, true); err == nil {
		t.Fatal("parsed an empty summary")
	}
}
