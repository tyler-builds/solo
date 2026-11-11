package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestMatchQuery(t *testing.T) {
	q, err := MatchQuery("What did we decide about caching? (Redis, redis!)")
	if err != nil || q != `"decide" OR "caching" OR "redis"` {
		t.Fatalf("%q %v", q, err)
	}
	if _, err := MatchQuery("what is it?"); err != ErrNoTerms {
		t.Fatalf("stop words only: %v", err)
	}
}

func TestSearchMemories(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	redis := &Memory{Text: "The rate limiter keeps its counters in Redis, which the API already runs.", Keywords: "cache store upload limits", Repo: "api"}
	accent := &Memory{Text: "Solo's accent colour is crimson and its logo is Orbit.", Keywords: "brand theme color"}
	for _, m := range []*Memory{redis, accent} {
		if err := s.AddMemory(m); err != nil {
			t.Fatal(err)
		}
	}
	// Stemming and keywords: "caching" meets "cache", "colors" meets "color".
	for query, want := range map[string]int64{"how are we caching limits?": redis.ID, "brand colors": accent.ID, "redis": redis.ID} {
		got, err := s.SearchMemories(ctx, query, 5)
		if err != nil || len(got) == 0 || got[0].ID != want {
			t.Errorf("%q: %+v %v", query, got, err)
		}
	}
	// Replacing hides the old memory from search and the current list, and keeps it in the table.
	teal := &Memory{Text: "Solo's accent colour is teal.", Keywords: "brand theme color"}
	if err := s.AddMemory(teal, accent.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.SearchMemories(ctx, "accent colour", 5)
	if len(got) != 1 || got[0].ID != teal.ID {
		t.Fatalf("after replace: %+v", got)
	}
	if err := s.AddMemory(&Memory{Text: "x"}, accent.ID); err == nil {
		t.Fatal("replaced a superseded memory twice")
	}
	all, _ := s.Memories(ctx)
	if len(all) != 2 || all[0].ID != teal.ID {
		t.Fatalf("current memories: %+v", all)
	}
	if err := s.DeleteMemory(teal.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SearchMemories(ctx, "teal", 5); len(got) != 0 {
		t.Fatalf("deleted memory still found: %+v", got)
	}
}

func TestSearchArchive(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "solo.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	th := &Thread{ID: "w1", Kind: KindWorker, SessionID: "s1", Title: "Fix OAuth token refresh", Status: "idle",
		Prompt: "Retry once on a 401", CreatedAt: now, UpdatedAt: now}
	orch := &Thread{ID: "orchestrator", Kind: KindOrchestrator, SessionID: "s0", Title: "Solo", Status: "idle", CreatedAt: now, UpdatedAt: now}
	for _, x := range []*Thread{th, orch} {
		if err := s.SaveThread(x); err != nil {
			t.Fatal(err)
		}
	}
	add := func(kind string, data map[string]any) {
		b, _ := json.Marshal(data)
		if err := s.AddItem(&Item{ThreadID: "orchestrator", Kind: kind, Data: b, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	add("user", map[string]any{"text": "Let's keep the deploy window on Fridays after 4pm."})
	add("user", map[string]any{"text": "[thread update] deploy finished", "synthetic": true})
	add("assistant", map[string]any{"text": "(noted)"})
	add("tool", map[string]any{"text": "deploy"})
	hits, err := s.SearchMessages(ctx, "orchestrator", "when do we deploy?", 5)
	if err != nil || len(hits) != 1 || hits[0].Kind != "user" {
		t.Fatalf("messages: %+v %v", hits, err)
	}
	// A thread's entry follows its row.
	th.Summary = "Refreshes the access token and retries the request."
	if err := s.SaveThread(th); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SearchThreads(ctx, "access tokens", 5); err != nil || len(got) != 1 || got[0].ThreadID != "w1" {
		t.Fatalf("threads: %+v %v", got, err)
	}
	if got, _ := s.SearchThreads(ctx, "oauth", 5); len(got) != 1 {
		t.Fatalf("thread entries duplicated: %+v", got)
	}

	// A database from before search had its rows indexed on open.
	if _, err := s.db.Exec(`DELETE FROM messages_fts; DELETE FROM threads_fts; DELETE FROM settings WHERE key = 'search_version'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(filepath.Join(dir, "solo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if hits, _ := s.SearchMessages(ctx, "orchestrator", "fridays", 5); len(hits) != 1 {
		t.Fatalf("backfilled messages: %+v", hits)
	}
	if got, _ := s.SearchThreads(ctx, "oauth", 5); len(got) != 1 {
		t.Fatalf("backfilled threads: %+v", got)
	}
	if err := s.DeleteThread("w1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SearchThreads(ctx, "oauth", 5); len(got) != 0 {
		t.Fatalf("deleted thread found: %+v", got)
	}
}
