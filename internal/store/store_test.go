package store

import (
	"context"
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
