package core

import (
	"context"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/forge"
)

func TestTodosCompleteWhenTheirThreadLands(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Look around", Prompt: "just answer"})
	if err != nil {
		t.Fatal(err)
	}
	linked, err := e.c.AddTodo("Look around the code", []string{v.ID[:8]})
	if err != nil {
		t.Fatal(err)
	}
	loose, err := e.c.AddTodo("  Write the release notes ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if loose.Text != "Write the release notes" || len(linked.Threads) != 1 || linked.Threads[0] != v.ID {
		t.Fatalf("todos %+v %+v", linked, loose)
	}
	if _, err := e.c.AddTodo("Ask Solo", []string{OrchestratorID}); err == nil {
		t.Fatal("linked a todo to Solo itself")
	}

	// Finishing a turn isn't landing: the todo stays open until the thread is marked done.
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	for deadline := time.Now().Add(5 * time.Second); !v.CanDone && time.Now().Before(deadline); v, _ = e.c.Thread(v.ID) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := e.c.Todos(false); len(got) != 2 {
		t.Fatalf("open todos before landing: %+v", got)
	}
	if err := e.c.Done(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	open := e.c.Todos(false)
	if len(open) != 1 || open[0].ID != loose.ID {
		t.Fatalf("open todos after done: %+v", open)
	}
	all := e.c.Todos(true)
	if len(all) != 2 || all[0].DoneBy != TodoByThread || !all[0].Done() {
		t.Fatalf("all todos: %+v", all)
	}

	// The user ticks one off and takes it back; the list survives a restart.
	if err := e.c.CompleteTodo(loose.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.c.Todos(false)) != 0 {
		t.Fatal("ticked todo still open")
	}
	reopen := false
	if _, err := e.c.UpdateTodo(loose.ID, TodoChange{Done: &reopen}); err != nil {
		t.Fatal(err)
	}
	e.c.Close()
	c := e.open(t)
	if got := c.Todos(false); len(got) != 1 || got[0].ID != loose.ID || got[0].Done() {
		t.Fatalf("after restart: %+v", got)
	}
	if err := c.RemoveTodo(loose.ID); err != nil {
		t.Fatal(err)
	}
	if len(c.Todos(true)) != 1 {
		t.Fatalf("after remove: %+v", c.Todos(true))
	}
}

func TestLandedByPullRequest(t *testing.T) {
	v := ThreadView{Parts: []PartView{{}}}
	if Landed(v) {
		t.Fatal("landed with no pull request")
	}
	for state, want := range map[string]bool{"open": true, "merged": true, "closed": false} {
		v.Parts[0].PR = &forge.PR{State: state}
		if Landed(v) != want {
			t.Errorf("pull request %s: landed %v", state, !want)
		}
	}
}
