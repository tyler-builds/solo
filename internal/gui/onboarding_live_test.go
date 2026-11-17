//go:build live

package gui

// The first-run setup over the real engine and this machine: a fresh data folder, the Claude Code it
// finds, the folders under the home folder, gh and glab. It changes nothing outside its data folder.
//
//	SOLO_SHOTS=/tmp/live go test -tags live -run LiveSetup ./internal/gui -v

import (
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
)

func TestLiveSetup(t *testing.T) {
	cmd, err := agent.FindCommand()
	if err != nil {
		t.Skip(err)
	}
	data := t.TempDir()
	open := func() *core.Core {
		c, err := core.New(core.Config{DataDir: data, Agent: cmd, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	out := os.Getenv("SOLO_SHOTS")
	shot := func(u *uitest.UI, name string) {
		if out == "" {
			return
		}
		u.Settle()
		f, err := os.Create(filepath.Join(out, "live-"+name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		png.Encode(f, u.Screenshot())
		f.Close()
	}
	_, dark := Themes()

	c := open()
	a := New(Live{Core: c})
	if a.setup.Peek() != "welcome" {
		t.Fatalf("a fresh data folder starts the setup at %q", a.setup.Peek())
	}
	u := uitest.New(t, a.Root, uitest.Size(1440, 900), uitest.Theme(dark))
	started(a, u)
	shot(u, "1-welcome")
	u.Press("enter")
	u.WaitFor("the Claude Code step", func() bool { return a.setup.Peek() == "claude" })
	u.WaitForQueries()
	shot(u, "2-claude")
	t.Logf("Claude Code: %v", texts(u)[len(texts(u))/3:])
	u.Click("setup-next")
	u.WaitFor("the models step", func() bool { return a.setup.Peek() == "models" })
	u.WaitForQueries()
	shot(u, "3-models")
	u.Click("model-threads-haiku") // from Claude Code's own list
	u.WaitFor("the model to be saved", func() bool { return c.Settings().WorkerModel == "haiku" })
	u.Click("setup-next")
	u.WaitFor("the repositories step", func() bool { return a.setup.Peek() == "repos" })
	u.WaitForQueries()
	shot(u, "4-repos")

	// Closed halfway, it resumes here.
	a.Close()
	u.Close()
	c.Close()
	c = open()
	a = New(Live{Core: c})
	if a.setup.Peek() != "repos" {
		t.Fatalf("after a restart the setup is at %q, want repos", a.setup.Peek())
	}
	u = uitest.New(t, a.Root, uitest.Size(1440, 900), uitest.Theme(dark))
	started(a, u)
	u.WaitForQueries()
	var key string
	u.WaitFor("a suggestion, if this machine has one", func() bool {
		for _, s := range []string{"~/code", "~/src", "~/Projects", "~/projects", "~/dev"} {
			if u.Has("suggest-add-" + s) {
				key = s
				return true
			}
		}
		return true
	})
	if key != "" {
		u.Click("suggest-add-" + key)
		u.WaitFor("the folder searched", func() bool { return u.Has("repos-found") })
		shot(u, "5-repos-added")
		t.Logf("found: %d repositories in %s", len(c.RepoPaths()), key)
	}
	u.Click("setup-next")
	u.WaitFor("the pull requests step", func() bool { return a.setup.Peek() == "prs" })
	u.WaitForQueries()
	shot(u, "6-prs")
	u.Click("setup-next")
	u.WaitFor("the preferences step", func() bool { return a.setup.Peek() == "prefs" })
	shot(u, "7-prefs")
	u.Click("setup-next")
	u.WaitFor("the chat", func() bool { return u.Has("orch-draft-area") })
	u.WaitFor("the setup saved as done", func() bool { return c.Settings().Setup == core.SetupDone })
	shot(u, "8-chat")
	a.Close()
	u.Close()
	c.Close()

	// What the setup set is what Settings holds, and it doesn't show again.
	c = open()
	defer c.Close()
	if key != "" && !slices.Contains(c.Settings().Folders, key) {
		t.Errorf("folders %v, want %s", c.Settings().Folders, key)
	}
	if c.Settings().WorkerModel != "haiku" {
		t.Errorf("the threads' model is %q after a restart, want haiku", c.Settings().WorkerModel)
	}
	if New(Live{Core: c}).setup.Peek() != "" {
		t.Error("the setup shows again after it was finished")
	}
	time.Sleep(10 * time.Millisecond)
}
