package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyler-builds/solo/internal/core"
)

// Solo starts a thread as an experiment and turns the mode on and off with set_experiment. It hears
// that an experiment has nothing to land, propose_merge refuses one, and turning the mode on takes its
// card away.
func TestExperimentTools(t *testing.T) {
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

	out := call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Bench", "prompt": "write bench.txt fast", "experiment": true})
	if !strings.Contains(out, "experiment") {
		t.Fatalf("spawn: %s", out)
	}
	eventually(t, "worker counted", func() bool {
		v := onlyWorker(c)
		return v.Status == core.StatusIdle && !v.InTurn && v.Changed == 1
	})
	w := onlyWorker(c)
	if !w.Experiment {
		t.Fatal("spawn_thread didn't make it an experiment")
	}
	// The thread update says there is nothing to land.
	eventually(t, "thread update", func() bool {
		for _, m := range syntheticMessages(c) {
			if strings.Contains(m, `"Bench" (web) finished its turn.`) {
				return strings.Contains(m, "It's an experiment") && strings.Contains(m, "don't propose a merge")
			}
		}
		return false
	})
	if out := call(t, o, "list_threads", map[string]any{}); !strings.Contains(out, "an experiment") {
		t.Fatalf("list_threads: %s", out)
	}
	if out := call(t, o, "get_thread", map[string]any{"id": w.ID}); !strings.Contains(out, "Landing: none, it's an experiment") {
		t.Fatalf("get_thread: %s", out)
	}
	if out, isErr := tool("propose_merge", map[string]any{"id": w.ID, "summary": "Benchmarks."}); !isErr || !strings.Contains(out, "is an experiment") {
		t.Fatalf("propose_merge on an experiment: %v %s", isErr, out)
	}
	if n := cards(w.ID); n != 0 {
		t.Fatalf("%d cards", n)
	}

	// Off: Solo proposes it as usual.
	if out := call(t, o, "set_experiment", map[string]any{"id": w.ID[:8], "on": false}); !strings.Contains(out, "normal thread") {
		t.Fatalf("set_experiment off: %s", out)
	}
	if v, _ := c.Thread(w.ID); v.Experiment {
		t.Fatal("still an experiment")
	}
	if out := call(t, o, "get_thread", map[string]any{"id": w.ID}); strings.Contains(out, "experiment") {
		t.Fatalf("get_thread of a normal thread: %s", out)
	}
	call(t, o, "propose_merge", map[string]any{"id": w.ID, "summary": "Benchmarks."})
	if n := cards(w.ID); n != 1 {
		t.Fatalf("%d cards after proposing", n)
	}

	// On: the card goes.
	if out := call(t, o, "set_experiment", map[string]any{"id": w.ID, "on": true}); !strings.Contains(out, "is an experiment now") {
		t.Fatalf("set_experiment on: %s", out)
	}
	if v, _ := c.Thread(w.ID); !v.Experiment {
		t.Fatal("not an experiment")
	}
	if n := cards(w.ID); n != 0 {
		t.Fatalf("%d cards left", n)
	}
	if out, isErr := tool("set_experiment", map[string]any{"id": w.ID}); !isErr {
		t.Fatalf("set_experiment without on: %s", out)
	}

	// Solo set those itself, so it has no note of them; the user turning it off is noted.
	var notes []string
	if c.State(notesKey, &notes); len(notes) != 0 {
		t.Fatalf("notes for Solo's own changes: %q", notes)
	}
	o.SetUpdates(false)
	if err := c.SetExperiment(ctx, w.ID, false); err != nil {
		t.Fatal(err)
	}
	if c.State(notesKey, &notes); len(notes) != 1 || !strings.Contains(notes[0], "a normal thread again") {
		t.Fatalf("notes %q", notes)
	}
}
