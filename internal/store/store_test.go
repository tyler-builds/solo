package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "solo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSettings(t *testing.T) {
	s := open(t)
	var folders []string
	if ok, err := s.GetSetting("folders", &folders); ok || err != nil {
		t.Fatalf("unset setting: ok=%v err=%v", ok, err)
	}
	if err := s.SetSetting("folders", []string{"/a", "/b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("folders", []string{"/c"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.GetSetting("folders", &folders); !ok || err != nil || len(folders) != 1 || folders[0] != "/c" {
		t.Fatalf("got %v %v %v", folders, ok, err)
	}
}

func TestThreadsAndItems(t *testing.T) {
	s := open(t)
	now := time.Now().Truncate(time.Millisecond)
	th := &Thread{ID: "t1", Kind: KindWorker, SessionID: "s1", Title: "Fix auth", Status: "running", CreatedAt: now, UpdatedAt: now}
	if err := s.SaveThread(th); err != nil {
		t.Fatal(err)
	}
	th.Status, th.CostUSD = "idle", 0.5
	if err := s.SaveThread(th); err != nil {
		t.Fatal(err)
	}
	got, err := s.Threads()
	if err != nil || len(got) != 1 || got[0].Status != "idle" || got[0].CostUSD != 0.5 || !got[0].CreatedAt.Equal(now) {
		t.Fatalf("threads %+v %v", got, err)
	}

	for i := 0; i < 5; i++ {
		it := &Item{ThreadID: "t1", Kind: "assistant", Data: json.RawMessage(`{"text":"x"}`), CreatedAt: now}
		if err := s.AddItem(it); err != nil {
			t.Fatal(err)
		}
		if i == 4 {
			if err := s.UpdateItemData(it.ID, json.RawMessage(`{"text":"last"}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	items, err := s.Items(context.Background(), "t1", 2)
	if err != nil || len(items) != 2 || string(items[1].Data) != `{"text":"last"}` || items[0].ID >= items[1].ID {
		t.Fatalf("items %v %v", items, err)
	}

	if err := s.DeleteThread("t1"); err != nil {
		t.Fatal(err)
	}
	if items, _ := s.Items(context.Background(), "t1", 0); len(items) != 0 {
		t.Fatalf("items not deleted with thread: %d", len(items))
	}
}

// A thread's scratch mode is saved, and a database from before the column gets it.
func TestThreadScratch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "solo.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, kind TEXT NOT NULL, session_id TEXT NOT NULL,
		has_transcript INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL, repo_path TEXT NOT NULL DEFAULT '',
		worktree_path TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '', base_branch TEXT NOT NULL DEFAULT '',
		base_commit TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
		summary TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL DEFAULT '',
		cost_usd REAL NOT NULL DEFAULT 0, context_tokens INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		INSERT INTO threads(id, kind, session_id, title, status, created_at, updated_at) VALUES ('old', 'worker', 's0', 'Old', 'idle', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Truncate(time.Millisecond)
	if err := s.SaveThread(&Thread{ID: "t1", Kind: KindWorker, SessionID: "s1", Title: "Bench", Status: "idle", Scratch: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Threads()
	if err != nil || len(got) != 2 || got[0].ID != "old" || got[0].Scratch || !got[1].Scratch {
		t.Fatalf("threads %+v %v", got, err)
	}
	got[1].Scratch = false
	if err := s.SaveThread(got[1]); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Threads(); got[1].Scratch {
		t.Fatal("scratch still on after turning it off")
	}
}
