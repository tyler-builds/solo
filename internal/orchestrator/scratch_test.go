package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyler-builds/solo/internal/core"
)

// Solo starts a scratch thread and turns the mode on and off with set_scratch. It hears that a
// scratch thread has nothing to land, propose_merge refuses one, and turning the mode on takes its
// card away.
func TestScratchTools(t *testing.T) {
	c, o := setup(t)
	ctx := context.Background()
	tool := func(name string, args any) (string, bool) {
		raw, _ := json.Marshal(args)
		return o.handleTool(ctx, core.OrchestratorID, name, raw)
	}
	cards := func(id string) int {
		items, _ := c.Items(ctx, core.OrchestratorID, 0)
		n := 0
		for _, it := range items {
			if it.Kind == core.ItemProposal && it.Ref == id {
				n++
			}
		}
		return n
	}

	out := call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Bench", "prompt": "write bench.txt fast", "scratch": true})
	if !strings.Contains(out, "scratch") {
		t.Fatalf("spawn: %s", out)
	}
	eventually(t, "worker counted", func() bool {
		v := onlyWorker(c)
		return v.Status == core.StatusIdle && !v.InTurn && v.Changed == 1
	})
	w := onlyWorker(c)
	if !w.Scratch {
		t.Fatal("spawn_thread didn't make it a scratch thread")
	}
	// The thread update says there is nothing to land.
	eventually(t, "thread update", func() bool {
		for _, m := range syntheticMessages(c) {
			if strings.Contains(m, `"Bench" (web) finished its turn.`) {
				return strings.Contains(m, "It's a scratch thread") && strings.Contains(m, "don't propose a merge")
			}
		}
		return false
	})
	if out := call(t, o, "list_threads", map[string]any{}); !strings.Contains(out, "a scratch thread") {
		t.Fatalf("list_threads: %s", out)
	}
	if out := call(t, o, "get_thread", map[string]any{"id": w.ID}); !strings.Contains(out, "Landing: none, it's a scratch thread") {
		t.Fatalf("get_thread: %s", out)
	}
	if out, isErr := tool("propose_merge", map[string]any{"id": w.ID, "summary": "Benchmarks."}); !isErr || !strings.Contains(out, "is a scratch thread") {
		t.Fatalf("propose_merge on a scratch thread: %v %s", isErr, out)
	}
	if n := cards(w.ID); n != 0 {
		t.Fatalf("%d cards", n)
	}

	// Off: Solo proposes it as usual.
	if out := call(t, o, "set_scratch", map[string]any{"id": w.ID[:8], "on": false}); !strings.Contains(out, "normal thread") {
		t.Fatalf("set_scratch off: %s", out)
	}
	if v, _ := c.Thread(w.ID); v.Scratch {
		t.Fatal("still a scratch thread")
	}
	if out := call(t, o, "get_thread", map[string]any{"id": w.ID}); strings.Contains(out, "scratch") {
		t.Fatalf("get_thread of a normal thread: %s", out)
	}
	call(t, o, "propose_merge", map[string]any{"id": w.ID, "summary": "Benchmarks."})
	if n := cards(w.ID); n != 1 {
		t.Fatalf("%d cards after proposing", n)
	}

	// On: the card goes.
	if out := call(t, o, "set_scratch", map[string]any{"id": w.ID, "on": true}); !strings.Contains(out, "is a scratch thread now") {
		t.Fatalf("set_scratch on: %s", out)
	}
	if v, _ := c.Thread(w.ID); !v.Scratch {
		t.Fatal("not a scratch thread")
	}
	if n := cards(w.ID); n != 0 {
		t.Fatalf("%d cards left", n)
	}
	if out, isErr := tool("set_scratch", map[string]any{"id": w.ID}); !isErr {
		t.Fatalf("set_scratch without on: %s", out)
	}

	// Solo set those itself, so it has no note of them; the user turning it off is noted.
	var notes []string
	if c.State(notesKey, &notes); len(notes) != 0 {
		t.Fatalf("notes for Solo's own changes: %q", notes)
	}
	o.SetUpdates(false)
	if err := c.SetScratch(ctx, w.ID, false); err != nil {
		t.Fatal(err)
	}
	if c.State(notesKey, &notes); len(notes) != 1 || !strings.Contains(notes[0], "a normal thread again") {
		t.Fatalf("notes %q", notes)
	}
}
