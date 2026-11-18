package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/git"
)

// openSetup starts a thread with the given first prompt, and waits for Solo to have answered the update
// its first turn sends (the thread isn't open yet).
func openSetup(t *testing.T, prompt string) (*core.Core, *Orchestrator, core.ThreadView) {
	t.Helper()
	c, o := setup(t)
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Open", "prompt": prompt})
	eventually(t, "Solo answered the first update", func() bool {
		v, _ := c.Thread(core.OrchestratorID)
		w := onlyWorker(c)
		return len(syntheticMessages(c)) == 1 && v.Status == core.StatusIdle && !v.InTurn &&
			w.Status == core.StatusIdle && !w.InTurn && w.Changed >= 0
	})
	return c, o, onlyWorker(c)
}

// turn sends the thread a message and waits for its turn to end on status (or, for StatusWaiting, to
// stop on its question).
func turn(t *testing.T, c *core.Core, id, text, status string) {
	t.Helper()
	if err := c.Send(context.Background(), id, text); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the thread's turn to end", func() bool {
		v, _ := c.Thread(id)
		return v.Status == status && (!v.InTurn || status == core.StatusWaiting)
	})
}

func notes(c *core.Core) []string {
	var n []string
	c.State(notesKey, &n)
	return n
}

// quiet waits out the update delay, then fails if Solo was sent more than want updates.
func quiet(t *testing.T, c *core.Core, want int) {
	t.Helper()
	time.Sleep(6 * UpdateDelay)
	if got := syntheticMessages(c); len(got) != want {
		t.Fatalf("%d updates, want %d: %q", len(got), want, got)
	}
	var pending []string
	if c.State(pendingKey, &pending); len(pending) != 0 {
		t.Fatalf("pending updates %q", pending)
	}
}

// A turn that ends in the thread the user has open starts no Solo turn: the user read the reply. Solo
// gets a note with its next message instead, one per thread, of where it stands.
func TestOpenThreadTurnIsANote(t *testing.T) {
	c, o, w := openSetup(t, "look around")
	c.SetViewing("window", w.ID, time.Time{})

	turn(t, c, w.ID, "and again", core.StatusIdle)
	prefix := `[Solo: the user had Thread ` + w.ID[:8] + ` "Open" (web) open when `
	want := prefix + "it finished its turn, and read its reply there. It changed no files: nothing to land. " +
		"No review or recap is owed unless they ask. Summary: echo: and again resume=false]"
	eventually(t, "the note", func() bool { return slices.Equal(notes(c), []string{want}) })
	quiet(t, c, 1)

	// A question there is the user's to answer: noted, not relayed. It takes the turn's place.
	turn(t, c, w.ID, "ask", core.StatusWaiting)
	eventually(t, "the question's note", func() bool {
		n := notes(c)
		return len(n) == 1 && strings.HasPrefix(n[0], prefix+"it asked a question")
	})
	quiet(t, c, 1)
	qs := c.PendingQuestions(w.ID)
	if err := c.Answer(w.ID, qs[0].ToolUseID, map[string]string{"Tabs or spaces?": "Tabs"}); err != nil {
		t.Fatal(err)
	}
	// The answer is noted, and the turn it ends replaces the question's note.
	eventually(t, "the answer's note", func() bool {
		n := notes(c)
		return len(n) == 2 && strings.Contains(n[0], "answered") && strings.HasPrefix(n[1], prefix+"it finished its turn") && strings.HasSuffix(n[1], "Summary: Tabs]")
	})
	quiet(t, c, 1)

	// Solo's next message carries the notes, out of the transcript.
	if err := o.Send(context.Background(), "what next"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "reply with the notes", func() bool {
		items, _ := c.Items(context.Background(), core.OrchestratorID, 0)
		for _, it := range items {
			var d core.TextData
			if it.Kind == core.ItemAssistant && it.Decode(&d) == nil && strings.Contains(d.Text, "what next") {
				return strings.Contains(d.Text, prefix+"it finished its turn") && strings.Contains(d.Text, "answered")
			}
		}
		return false
	})
	if n := notes(c); len(n) != 0 {
		t.Fatalf("notes not spent: %q", n)
	}
}

// A thread that isn't the open one (another is) sends its updates as before.
func TestOtherThreadTurnStillReachesSolo(t *testing.T) {
	c, _, w := openSetup(t, "look around")
	c.SetViewing("window", "some-other-thread", time.Time{})
	turn(t, c, w.ID, "and again", core.StatusIdle)
	eventually(t, "the update", func() bool {
		m := syntheticMessages(c)
		return len(m) == 2 && strings.Contains(m[1], `"Open" (web) finished its turn. Summary: echo: and again`) && !strings.Contains(m[1], openLine)
	})
	if n := notes(c); len(n) != 0 {
		t.Fatalf("notes %q", n)
	}
	// A device's view lapses when it isn't renewed.
	c.SetViewing("device", w.ID, time.Now().Add(-time.Second))
	turn(t, c, w.ID, "once more", core.StatusIdle)
	eventually(t, "the update", func() bool { return len(syntheticMessages(c)) == 3 })
}

// Work the open thread finished that still needs a merge card gets a quiet turn: Solo reviews and
// proposes it, without recapping it. Once the card is up, later turns there are notes.
func TestOpenThreadWorkGetsAQuietTurn(t *testing.T) {
	c, o, w := openSetup(t, "look around")
	c.SetViewing("window", w.ID, time.Time{})

	turn(t, c, w.ID, "write a.txt x", core.StatusIdle)
	eventually(t, "the quiet update", func() bool {
		m := syntheticMessages(c)
		return len(m) == 2 && strings.Contains(m[1], `"Open" (web) finished its turn. Summary: wrote a.txt`+openLine) &&
			strings.Contains(m[1], `needs no recap`)
	})
	if n := notes(c); len(n) != 0 {
		t.Fatalf("notes %q", n)
	}
	eventually(t, "Solo idle", func() bool { v, _ := c.Thread(core.OrchestratorID); return v.Status == core.StatusIdle && !v.InTurn })
	call(t, o, "propose_merge", map[string]any{"id": w.ID, "summary": "Adds a."})

	turn(t, c, w.ID, "write b.txt y", core.StatusIdle)
	eventually(t, "the note", func() bool {
		n := notes(c)
		return len(n) == 1 && strings.Contains(n[0], "it finished its turn, and read its reply there. Its merge card is up")
	})
	quiet(t, c, 2)
}

// Pull request events reach Solo whether or not the thread is open; and with a pull request open, a
// turn that ends in the open thread is a note while the app moves the card with its new commits.
func TestOpenThreadPullRequestEventsReachSolo(t *testing.T) {
	c, o := setup(t)
	ctx := context.Background()
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := git.Run(ctx, dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	repo := filepath.Join(c.Settings().Folders[0], "web")
	origin := repo + ".git"
	run(repo, "clone", "-q", "--bare", repo, origin)
	run(repo, "remote", "add", "origin", origin)
	run(repo, "config", "solo.forge", "github")
	gh := t.TempDir()
	os.WriteFile(filepath.Join(gh, "gh"), []byte(`#!/bin/sh
case "$1 $2" in
"pr create") echo "https://github.com/acme/web/pull/7" ;;
"pr view") echo '{"number":7,"url":"https://github.com/acme/web/pull/7","state":"OPEN","isDraft":false,"autoMergeRequest":null,"mergeStateStatus":"BLOCKED","reviewDecision":"","statusCheckRollup":[]}' ;;
*) exit 1 ;;
esac
`), 0o755)
	t.Setenv("PATH", gh+string(os.PathListSeparator)+os.Getenv("PATH"))
	c.RescanRepos()

	idle := func(id string) func() bool {
		return func() bool { v, _ := c.Thread(id); return v.Status == core.StatusIdle && !v.InTurn }
	}
	call(t, o, "spawn_thread", map[string]any{"repo": "web", "title": "Hello", "prompt": "write hello.txt hi"})
	eventually(t, "worker counted", func() bool { v := onlyWorker(c); return idle(v.ID)() && v.Forge != "" && v.Changed > 0 })
	w := onlyWorker(c)
	eventually(t, "Solo answered the update", func() bool { return len(syntheticMessages(c)) == 1 && idle(core.OrchestratorID)() })

	c.SetViewing("window", w.ID, time.Time{})
	if _, err := c.OpenPR(ctx, w.ID, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pull request's update", func() bool {
		m := syntheticMessages(c)
		return len(m) == 2 && strings.Contains(m[1], "the user opened PR #7")
	})
	eventually(t, "Solo idle", idle(core.OrchestratorID))
	call(t, o, "propose_merge", map[string]any{"id": w.ID, "summary": "Adds hello."})
	cards := func() []core.Item {
		items, _ := c.Items(ctx, core.OrchestratorID, 0)
		var out []core.Item
		for _, it := range items {
			if it.Kind == core.ItemProposal && it.Ref == w.ID {
				out = append(out, it)
			}
		}
		return out
	}
	old := cards()

	// A commit in a turn there: pushed, the card posted anew, and a note rather than an update.
	w, _ = c.Thread(w.ID)
	os.WriteFile(filepath.Join(w.WorktreePath, "more.txt"), []byte("more"), 0o644)
	run(w.WorktreePath, "add", "-A")
	run(w.WorktreePath, "commit", "-q", "-m", "more")
	turn(t, c, w.ID, "done?", core.StatusIdle)
	eventually(t, "card posted anew", func() bool {
		all := cards()
		return len(all) == 1 && all[0].ID != old[0].ID
	})
	eventually(t, "the note", func() bool {
		n := notes(c)
		return len(n) == 1 && strings.Contains(n[0], "Its pull request is open and takes its new commits")
	})
	quiet(t, c, 2)
}
