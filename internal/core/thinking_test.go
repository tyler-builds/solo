package core

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/tyler-builds/solo/internal/agent"
)

// replayEvents reads events the sidecar emitted, one JSON object per line, as the agent package parses them.
func replayEvents(t *testing.T, path string) []agent.Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []agent.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e agent.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

// A recorded orchestrator turn: a Read call, a thought, the reply. Claude sends the thought's summary in
// one burst just before the finished thinking block, which takes the live block away at once, so the
// working line has to read the thought from Thought, which outlasts it.
func TestOrchestratorThoughtOutlastsItsLiveBlock(t *testing.T) {
	e := newEnv(t)
	o, err := e.c.EnsureOrchestrator()
	if err != nil {
		t.Fatal(err)
	}
	events := replayEvents(t, "../../sidecar/test/fixtures/orchestrator-thinking.events.jsonl")

	var thought string
	for _, ev := range events {
		if ev.Kind == agent.EventAssistant && len(ev.Blocks) > 0 && ev.Blocks[0].Type == "thinking" {
			thought = ev.Blocks[0].Text
		}
	}
	if thought == "" {
		t.Fatal("the recording has no finished thought")
	}

	view := func() ThreadView {
		v, ok := e.c.Thread(OrchestratorID)
		if !ok {
			t.Fatal("no orchestrator")
		}
		return v
	}
	streamed := ""
	sawThinking, sawThought := false, false
	for _, ev := range events {
		ev.SessionID = o.SessionID
		e.c.onEvent(ev)
		v := view()
		switch {
		case ev.Kind == agent.EventThinkingDelta:
			streamed += ev.Text
			if n := len(v.Live); n == 0 || !v.Live[n-1].Thinking || v.Live[n-1].Text != streamed {
				t.Fatalf("after a thinking delta, live = %+v, want a thinking block %q", v.Live, streamed)
			}
			if v.Thought != streamed {
				t.Fatalf("Thought = %q while streaming, want %q", v.Thought, streamed)
			}
			sawThinking = true
		case ev.Kind == agent.EventAssistant && ev.Blocks[0].Type == "thinking":
			if len(v.Live) != 0 {
				t.Fatalf("live = %+v after the finished thought, want none", v.Live)
			}
			if !v.InTurn || v.Thought != thought {
				t.Fatalf("after the finished thought: in turn %v, Thought = %q, want %q", v.InTurn, v.Thought, thought)
			}
			sawThought = true
		case ev.Kind == agent.EventTextDelta:
			if v.Thought != thought {
				t.Fatalf("Thought = %q while the reply streams, want it kept", v.Thought)
			}
		case ev.Kind == agent.EventTurnEnd:
			if v.InTurn || v.Thought != "" {
				t.Fatalf("after the turn: in turn %v, Thought = %q, want it cleared", v.InTurn, v.Thought)
			}
		}
	}
	if !sawThinking || !sawThought {
		t.Fatalf("thinking deltas seen %v, finished thought seen %v", sawThinking, sawThought)
	}
	if streamed != thought {
		t.Errorf("streamed %q, finished %q", streamed, thought)
	}
	if !strings.HasSuffix(streamed, "\n\n") {
		t.Errorf("the recorded summary should end its paragraph: %q", streamed)
	}

	// The next turn starts without the last one's thought.
	e.c.onEvent(agent.Event{SessionID: o.SessionID, Kind: agent.EventTurnStart, UUIDs: []string{"u2"}})
	if v := view(); v.Thought != "" {
		t.Errorf("Thought = %q at a new turn's start, want none", v.Thought)
	}
}
