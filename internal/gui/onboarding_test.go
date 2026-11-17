package gui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

// newSetupUI opens the app over a backend on this machine.
func newSetupUI(t *testing.T, b Backend, w float32) (*App, *uitest.UI) {
	t.Helper()
	_, dark := Themes()
	a := New(b)
	u := uitest.New(t, a.Root, uitest.Size(w, 900), uitest.Theme(dark))
	t.Cleanup(u.Close)
	started(a, u)
	u.WaitForQueries()
	return a, u
}

// texts is every name and value in the window, for a test to look in.
func texts(u *uitest.UI) []string {
	var out []string
	u.Tree().Walk(func(n *access.Node) bool {
		out = append(out, n.Name, n.Value)
		return true
	})
	return out
}

// mustShow fails unless the window shows each text.
func mustShow(t *testing.T, u *uitest.UI, want ...string) {
	t.Helper()
	got := texts(u)
	for _, w := range want {
		if !hasText(w, got) {
			t.Errorf("the window doesn't show %q", w)
		}
	}
}

// mustNotShow fails if the window shows any of the texts.
func mustNotShow(t *testing.T, u *uitest.UI, unwanted ...string) {
	t.Helper()
	got := texts(u)
	for _, w := range unwanted {
		if hasText(w, got) {
			t.Errorf("the window shows %q", w)
		}
	}
}

// savedSetup waits for Settings.Setup to be saved as want.
func savedSetup(t *testing.T, u *uitest.UI, f *setupFake, want string) {
	t.Helper()
	u.WaitFor("the setup to be saved as "+want, func() bool { return f.Settings().Setup == want })
}

// The setup shows for someone new, and not for someone who used Solo before it existed: they are
// marked set up, with the hints seen, so an update teaches them nothing they know.
func TestFirstRunDecision(t *testing.T) {
	for _, tc := range []struct {
		name string
		prep func(f *setupFake)
		want string
	}{
		{"new", func(f *setupFake) {}, "welcome"},
		{"folders", func(f *setupFake) { f.settings.Folders = []string{"~/code"} }, ""},
		{"conversation", func(f *setupFake) {
			f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "Hello"})
		}, ""},
		{"threads", func(f *setupFake) {
			f.threads = demoBackend().threads
		}, ""},
		{"finished", func(f *setupFake) { f.settings.Setup = core.SetupDone }, ""},
		{"halfway", func(f *setupFake) { f.settings.Setup = "repos"; f.settings.Folders = []string{"~/code"} }, "repos"},
		{"unknown step", func(f *setupFake) { f.settings.Setup = "later" }, "welcome"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetupFake()
			tc.prep(f)
			a := New(f)
			defer a.Close()
			if got := a.setup.Peek(); got != tc.want {
				t.Errorf("setup at %q, want %q", got, tc.want)
			}
			if tc.name == "folders" || tc.name == "conversation" || tc.name == "threads" {
				s := f.Settings()
				if s.Setup != core.SetupDone || !slices.Equal(s.Hints, allHints) {
					t.Errorf("an earlier user's settings: setup %q, hints %v; want done and every hint", s.Setup, s.Hints)
				}
			}
		})
	}
}

// A web page drives a machine that is set up there: it never shows the setup, nor the hints.
func TestNoSetupInWebClient(t *testing.T) {
	f := &fakeBackend{items: map[string][]core.Item{}} // not an api.Setup
	if _, ok := Backend(f).(api.Setup); ok {
		t.Fatal("the plain fake is a machine's backend")
	}
	a, u := newSetupUI(t, f, 1440)
	if a.setup.Peek() != "" || u.Has("setup-next") {
		t.Error("a web page shows the setup")
	}
	if s := f.Settings(); s.Setup != "" {
		t.Errorf("a web page changed the setup to %q", s.Setup)
	}
	f.threads = demoBackend().threads
	a.setThreads(f.Threads())
	u.Frame()
	if u.Has("hint-" + hintThread) {
		t.Error("a web page shows a hint")
	}
}

// Continue, Back and Enter move through the steps; each is saved, so a restart resumes there, and the
// focus is on the step's main button. Finishing lands in Solo's chat with the caret in its message box.
func TestSetupNavigation(t *testing.T) {
	f := newSetupFake().projects()
	a, u := newSetupUI(t, f, 1440)
	if !u.Has("setup-next") || u.Has("orch-draft-area") {
		t.Fatal("a new user doesn't see the setup in place of the main view")
	}
	u.WaitFor("the focus on Get started", func() bool { return u.Get("setup-next").Focused() })
	u.Press("enter")
	u.WaitFor("the Claude Code step", func() bool { return a.setup.Peek() == "claude" })
	savedSetup(t, u, f, "claude")
	u.WaitFor("the focus on Continue", func() bool { return u.Get("setup-next").Focused() })
	u.Click("setup-next")
	u.WaitFor("the models step", func() bool { return a.setup.Peek() == "models" })
	u.Click("setup-back")
	u.WaitFor("back to Claude Code", func() bool { return a.setup.Peek() == "claude" })
	u.Click("setup-go-prs") // a step can be picked from the progress bar
	u.WaitFor("the pull requests step", func() bool { return a.setup.Peek() == "prs" })
	savedSetup(t, u, f, "prs")

	// Closed here, it resumes here.
	b := New(f)
	if b.setup.Peek() != "prs" {
		t.Errorf("after a restart the setup is at %q, want prs", b.setup.Peek())
	}
	b.Close()

	u.Click("setup-next")
	u.WaitFor("the preferences step", func() bool { return a.setup.Peek() == "prefs" })
	u.Click("setup-next") // Start using Solo
	u.WaitFor("the chat", func() bool { return u.Has("orch-draft-area") })
	savedSetup(t, u, f, core.SetupDone)
	u.WaitFor("the caret in Solo's message box", func() bool { return u.Get("orch-draft-area").Focused() })
	if New(f).setup.Peek() != "" {
		t.Error("the setup shows again after it was finished")
	}
}

// Skip setup is there on every step and leaves a working app, with today's fallbacks.
func TestSkipSetup(t *testing.T) {
	for _, step := range []string{"welcome", "claude", "models", "repos", "prs", "prefs"} {
		t.Run(step, func(t *testing.T) {
			f := newSetupFake()
			f.settings.Setup = step
			a, u := newSetupUI(t, f, 1440)
			if a.setup.Peek() != step {
				t.Fatalf("setup at %q, want %q", a.setup.Peek(), step)
			}
			u.Click("setup-skip")
			u.WaitFor("the chat", func() bool { return u.Has("orch-draft-area") })
			savedSetup(t, u, f, core.SetupDone)
			// No folders: the empty chat offers to add them, in Settings.
			u.Click("add-folders")
			u.WaitFor("Settings on Repositories", func() bool { return u.Has("settings-tab-repos") && u.Get("settings-tab-repos").Selected() })
		})
	}
}

// The setup holds up in a compact window: its steps, and the way through them.
func TestSetupCompact(t *testing.T) {
	f := newSetupFake().projects()
	a, u := newSetupUI(t, f, 560)
	mustShow(t, u, "Step 1 of 6 · Welcome", "Skip setup")
	u.Click("setup-next")
	u.WaitFor("the Claude Code step", func() bool { return a.setup.Peek() == "claude" })
	mustShow(t, u, "Step 2 of 6 · Claude Code")
}

// The Claude Code step shows what was found, in each state, with what to do about a problem.
func TestSetupClaudeStates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		st      *core.ClaudeStatus
		want    []string
		problem bool
	}{
		{"found", nil, []string{"Claude Code 2.1.319 is ready", "Signed in as you@example.com · Max", "found on PATH"}, false},
		{"missing", &claudeMissing, []string{"Claude Code isn't installed", claudeInstall(), "Check again"}, true},
		{"signed out", &claudeSignedOut, []string{"Claude Code isn't signed in", "sign in with /login", "Check again"}, true},
		{"broken", &claudeBroken, []string{"Solo can't use this Claude Code", "the probe timed out", "Check again"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetupFake()
			f.claude = tc.st
			f.settings.Setup = "claude"
			_, u := newSetupUI(t, f, 1440)
			mustShow(t, u, tc.want...)
			if u.Has("claude-again") != tc.problem {
				t.Errorf("Check again shown: %v, want %v", u.Has("claude-again"), tc.problem)
			}
			if !tc.problem {
				return
			}
			// Fixed in a terminal: Check again finds it ready.
			f.smu.Lock()
			f.claude = nil
			f.smu.Unlock()
			u.Click("claude-again")
			u.WaitForQueries()
			mustShow(t, u, "Claude Code 2.1.319 is ready")
		})
	}
}

// A problem is checked again when the window comes back to the front: the user was likely in a
// terminal fixing it. A good result isn't checked again.
func TestSetupClaudeRechecksOnFocus(t *testing.T) {
	f := newSetupFake()
	f.claude = &claudeSignedOut
	f.settings.Setup = "claude"
	a, u := newSetupUI(t, f, 1440)
	f.smu.Lock()
	f.claude = nil
	before := len(f.checked)
	f.smu.Unlock()
	a.focusGained.Update(func(n int) int { return n + 1 })
	u.WaitFor("a check on focus", func() bool {
		f.smu.Lock()
		defer f.smu.Unlock()
		return len(f.checked) > before
	})
	u.WaitForQueries()
	mustShow(t, u, "Claude Code 2.1.319 is ready")
	f.smu.Lock()
	after := len(f.checked)
	f.smu.Unlock()
	if after != before+1 {
		t.Errorf("checked %d times on focus, want once", after-before)
	}
	a.focusGained.Update(func(n int) int { return n + 1 })
	u.Frame()
	u.WaitForQueries()
	f.smu.Lock()
	defer f.smu.Unlock()
	if len(f.checked) != after {
		t.Error("a ready Claude Code was checked again on focus")
	}
}

// The path to claude is the setting Settings › General shows: set here, it is saved and checked.
func TestSetupClaudePath(t *testing.T) {
	f := newSetupFake()
	f.claude = &claudeMissing
	f.settings.Setup = "claude"
	a, u := newSetupUI(t, f, 1440)
	u.Click("claude-path-open")
	u.Get("claude-path").Focus()
	u.Type("~/tools/claude")
	u.Press("enter") // the field takes Enter: it uses the path, and doesn't move on
	u.WaitFor("the path to be saved", func() bool { return f.Settings().ClaudePath == "~/tools/claude" })
	u.WaitForQueries()
	if a.setup.Peek() != "claude" {
		t.Errorf("Enter in the path field moved to %q", a.setup.Peek())
	}
	u.WaitFor("the path to be checked", func() bool { return hasText("~/tools/claude doesn't exist", texts(u)) })
	mustShow(t, u, "the path you set")
	f.smu.Lock()
	defer f.smu.Unlock()
	if !slices.Contains(f.checked, "~/tools/claude") {
		t.Errorf("checked %v, not the path set", f.checked)
	}
}

// The repositories step: suggestions the user confirms (never added by themselves), the folder
// picker, and at once what was found in each folder, a folder outside git included. What is added
// here is what Settings searches.
func TestSetupRepos(t *testing.T) {
	f := newSetupFake().projects()
	f.settings.Setup = "repos"
	a, u := newSetupUI(t, f, 1440)
	mustShow(t, u, "~/code", "6 repositories: web, api, docs, infra, solo and 1 more", "~/src", "A folder that isn't in git works too")
	if len(f.Settings().Folders) != 0 {
		t.Fatalf("folders added by themselves: %v", f.Settings().Folders)
	}
	if u.Has("repos-found") {
		t.Error("found repositories before any folder was added")
	}

	// A suggestion, added.
	u.Click("suggest-add-~/code")
	u.WaitFor("the folder to be searched", func() bool { return u.Has("repos-found") })
	mustShow(t, u, "Found 6 repositories: web, api, docs, infra, solo and dotfiles", "1 folder not in git")
	if u.Has("suggest-~/code") {
		t.Error("an added folder is still suggested")
	}

	// The picker: a folder outside git.
	a.pickFolder = func(done func([]string)) { done([]string{homeDir("notes")}) }
	u.Click("repos-choose")
	u.WaitFor("the picked folder", func() bool { return u.Has("folder-~/notes") })
	u.WaitForQueries()
	mustShow(t, u, "Not in git. Threads work in it directly.")

	// A typed folder that holds nothing.
	u.Get("repos-path").Focus()
	u.Type("~/empty")
	u.Press("enter")
	u.WaitFor("the typed folder", func() bool { return slices.Contains(f.Settings().Folders, "~/empty") })
	u.WaitFor("what was found in it", func() bool { return hasText("Nothing found here", texts(u)) })
	if a.setup.Peek() != "repos" {
		t.Errorf("Enter in the path field moved to %q", a.setup.Peek())
	}
	if got := f.Settings().Folders; !slices.Equal(got, []string{"~/code", "~/notes", "~/empty"}) {
		t.Errorf("folders %v", got)
	}

	// Removed here, it is gone from Settings too.
	u.Get("folder-~/empty").ByName("Remove ~/empty").Click()
	u.WaitFor("the folder to go", func() bool { return !slices.Contains(f.Settings().Folders, "~/empty") })

	// Settings shows what was set here.
	u.Click("setup-skip")
	a.openSettings("repos")
	u.Frame()
	mustShow(t, u, "~/code", "~/notes", "FOUND 6 REPOSITORIES")
}

// With no folders and nothing found to suggest, the step says where to add them later.
func TestSetupReposNoFolders(t *testing.T) {
	f := newSetupFake()
	f.settings.Setup = "repos"
	_, u := newSetupUI(t, f, 1440)
	mustShow(t, u, "Choose a folder…", "Settings › Repositories")
	mustNotShow(t, u, "FOUND ON THIS COMPUTER")
}

// The pull requests step shows, for each forge the repositories push to, whether its CLI is ready,
// and the one command that fixes it; repositories without a remote merge locally.
func TestSetupForges(t *testing.T) {
	gh := core.ForgeStatus{Kind: forge.GitHub, CLI: "gh", Repos: []string{"/r/web", "/r/api"}, Login: "gh auth login",
		Install: "brew install gh", InstallURL: "https://cli.github.com"}
	for _, tc := range []struct {
		name string
		edit func(*core.ForgeStatus)
		want []string
	}{
		{"ready", func(s *core.ForgeStatus) {
			s.Path, s.SignedIn, s.Account = "/usr/bin/gh", true, "octocat on github.com"
		},
			[]string{"gh is signed in as octocat on github.com"}},
		{"missing", func(s *core.ForgeStatus) {}, []string{"gh isn't installed", "brew install gh", "Then sign in with gh auth login", "Check again"}},
		{"missing, no command", func(s *core.ForgeStatus) { s.Install = "" }, []string{"gh isn't installed", "Install it from https://cli.github.com"}},
		{"signed out", func(s *core.ForgeStatus) { s.Path = "/usr/bin/gh" }, []string{"gh isn't signed in", "gh auth login", "Check again"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetupFake()
			f.settings.Setup = "prs"
			f.repos = []string{"/r/web", "/r/api", "/r/notes"}
			st := gh
			tc.edit(&st)
			f.forges = []core.ForgeStatus{st}
			_, u := newSetupUI(t, f, 1440)
			mustShow(t, u, append(tc.want, "GitHub", "2 repositories: web and api", "Merge locally", "1 repository has no GitHub or GitLab remote: notes")...)
			if tc.name == "ready" {
				mustNotShow(t, u, "Check again")
			}
		})
	}
}

// Without repositories there is nothing to check, and the step says so.
func TestSetupForgesNoRepos(t *testing.T) {
	f := newSetupFake()
	f.settings.Setup = "prs"
	_, u := newSetupUI(t, f, 1440)
	mustShow(t, u, "No repositories yet")
	if f.forgeRuns != 0 {
		t.Error("checked the forges without repositories")
	}
}

// Settings › General can run the setup again, from its start; a web page has no such row.
func TestRunSetupAgain(t *testing.T) {
	f := done(newSetupFake().projects().withFolders("~/code"))
	a, u := newSetupUI(t, f, 1440)
	a.settings.Set(true)
	u.Frame()
	u.Get("run-setup").Perform(access.ActionClick)
	u.WaitFor("the setup", func() bool { return a.setup.Peek() == "welcome" && u.Has("setup-next") })
	savedSetup(t, u, f, "welcome")
	if a.settings.Peek() {
		t.Error("Settings stayed open over the setup")
	}

	web := &fakeBackend{items: map[string][]core.Item{}, settings: core.Settings{Setup: core.SetupDone}}
	b, w := newSetupUI(t, web, 1440)
	b.settings.Set(true)
	w.Frame()
	if w.Has("run-setup") {
		t.Error("a web page offers to run the setup")
	}
}

// The empty chat offers tasks written about the user's repositories; a click puts one in the message
// box to edit, and sends nothing. A web page offers them too.
func TestStarterPrompts(t *testing.T) {
	if got := starterPrompts(nil, nil); got != nil {
		t.Errorf("tasks without repositories: %v", got)
	}
	one := starterPrompts([]string{"/c/web"}, nil)
	for _, p := range one {
		if !strings.Contains(p.text, "web") {
			t.Errorf("%q isn't about web", p.text)
		}
	}
	for _, b := range []Backend{done(newSetupFake().projects().withFolders("~/code")), &fakeBackend{items: map[string][]core.Item{},
		repos: []string{"/c/web", "/c/api", "/c/docs"}}} {
		a, u := newSetupUI(t, b, 1440)
		mustShow(t, u, "Explain how web is laid out", "Find a small bug in api and fix it", "Add tests for the least tested part of docs")
		u.Click(starterKey(1))
		u.WaitFor("the task in the message box", func() bool { return a.orchDraft.Peek() == "Find a small bug in api and fix it" })
		u.WaitFor("the caret in the message box", func() bool { return u.Get("orch-draft-area").Focused() })
		u.Type(" quickly")
		u.Frame()
		if got := a.orchDraft.Peek(); got != "Find a small bug in api and fix it quickly" {
			t.Errorf("typing after the task gave %q: the caret isn't at its end", got)
		}
		var sent []string
		switch f := b.(type) {
		case *setupFake:
			sent = f.sent
		case *fakeBackend:
			sent = f.sent
		}
		if len(sent) > 0 {
			t.Errorf("a task was sent: %v", sent)
		}
	}
}

// The hints teach each thing as it first happens, one at a time: a thread, then its question over
// it; each is recorded the first time it shows, stays until dismissed, and never shows again.
func TestHints(t *testing.T) {
	f := hinted(t, "thread")
	a, u := newSetupUI(t, f, 1440)
	u.WaitFor("the thread hint", func() bool { return u.Has("hint-" + hintThread) })
	u.WaitFor("the hint to be recorded", func() bool { return slices.Contains(f.Settings().Hints, hintThread) })
	a.cfg.Set(f.Settings())
	u.Frame()
	if !u.Has("hint-" + hintThread) {
		t.Fatal("the hint went as soon as it was recorded")
	}

	// A question takes its place; one hint at a time.
	f.pending = map[string][]core.PendingQuestion{"f1a2b3c4-login": demoBackend().pending["b7e21a90-limit"]}
	for _, qs := range demoBackend().pending {
		f.pending["f1a2b3c4-login"] = qs
	}
	a.pending.Set(f.pending)
	u.Frame()
	if !u.Has("hint-"+hintQuestion) || u.Has("hint-"+hintThread) {
		t.Error("the question's hint doesn't take the place of the thread's")
	}
	u.Click("hint-dismiss")
	u.Frame()
	if u.Has("hint-" + hintQuestion) {
		t.Error("a dismissed hint stays")
	}
	if !u.Has("hint-" + hintThread) {
		t.Error("the thread's hint, not dismissed, doesn't come back")
	}
	u.Click("hint-action") // Open thread
	u.WaitFor("the thread to open", func() bool { return a.openID.Peek() == "f1a2b3c4-login" })
	if u.Has("hint-" + hintThread) {
		t.Error("the hint stays after its button was used")
	}

	// A later session shows neither again.
	_, v := newSetupUI(t, f, 1440)
	if v.Has("hint-"+hintThread) || v.Has("hint-"+hintQuestion) {
		t.Error("a hint shows again in a later session")
	}
}

// Each hint shows for what it is about.
func TestHintKinds(t *testing.T) {
	for _, tc := range []struct{ what, hint, text string }{
		{"thread", hintThread, "Solo started a thread"},
		{"question", hintQuestion, "A thread has a question for you"},
		{"merge", hintMerge, "Only you can land work"},
		{"remote", hintRemote, "phone or another computer's browser"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			f := hinted(t, tc.what)
			a := New(f)
			a.Remote = &fakeRemote{st: api.RemoteState{Connection: api.RemoteOff}}
			_, dark := Themes()
			u := uitest.New(t, a.Root, uitest.Size(1440, 900), uitest.Theme(dark))
			defer u.Close()
			started(a, u)
			u.WaitFor("the hint", func() bool { return u.Has("hint-" + tc.hint) })
			mustShow(t, u, tc.text)
		})
	}
	// With a device paired already, pairing isn't suggested.
	f := hinted(t, "remote")
	a := New(f)
	a.Remote = &fakeRemote{st: api.RemoteState{Devices: []api.RemoteDevice{{ID: "d1", Name: "Phone"}}}}
	_, dark := Themes()
	u := uitest.New(t, a.Root, uitest.Size(1440, 900), uitest.Theme(dark))
	defer u.Close()
	started(a, u)
	if u.Has("hint-" + hintRemote) {
		t.Error("pairing is suggested with a device paired")
	}
}

// The Models step offers the models Settings › Models offers, from the same list: Claude Code's own,
// by family on cards and every version in a menu, each with its effort levels. What is picked is saved
// as Settings saves it, survives a close halfway, and shows in Settings.
func TestSetupModels(t *testing.T) {
	f := newSetupFake()
	f.settings.Setup = "models"
	a, u := newSetupUI(t, f, 1440)
	if !u.Get("model-solo-opus").Checked() || !u.Get("model-threads-sonnet").Checked() {
		t.Error("the defaults aren't picked")
	}
	if !u.Has("model-solo-fable") || !u.Has("model-threads-haiku") {
		t.Error("Claude Code's models have no cards")
	}
	if u.Has("model-solo-claude-opus-5") {
		t.Error("a pinned version has a card of its own")
	}

	u.Click("model-threads-haiku")
	u.Get("effort-solo").ByName("High").Click()
	u.WaitFor("the choices to be saved", func() bool {
		s := f.Settings()
		return s.WorkerModel == "haiku" && s.OrchestratorEffort == "high"
	})
	if !u.Get("model-threads-haiku").Checked() || u.Get("model-threads-sonnet").Checked() {
		t.Error("the card picked isn't the one shown picked")
	}

	// A version from the menu, which lists every model Settings lists.
	u.Click("model-more-solo")
	u.Frame()
	for _, c := range modelChoices(claudeModels, "opus") {
		if !u.HasName(c.Label) {
			t.Errorf("the menu doesn't offer %s", c.Label)
		}
	}
	u.ByName("Sonnet 4.6").Click()
	u.WaitFor("the version to be saved", func() bool { return f.Settings().OrchestratorModel == "claude-sonnet-4-6" })
	u.WaitFor("its card", func() bool {
		return u.Has("model-solo-claude-sonnet-4-6") && u.Get("model-solo-claude-sonnet-4-6").Checked()
	})

	// Closed here, it resumes here, with what was picked.
	b := New(f)
	if b.setup.Peek() != "models" {
		t.Errorf("after a restart the setup is at %q", b.setup.Peek())
	}
	b.Close()
	_, v := newSetupUI(t, f, 1440)
	if !v.Get("model-threads-haiku").Checked() || !v.Get("model-solo-claude-sonnet-4-6").Checked() {
		t.Error("the choices aren't shown after a restart")
	}

	// Settings › Models shows them.
	u.Click("setup-skip")
	a.openSettings("models")
	u.Frame()
	u.WaitForQueries()
	if got := u.ByRole(access.RoleComboBox, "Threads model").Value(); got != "Haiku 5.5" {
		t.Errorf("Settings shows the threads' model as %q", got)
	}
	if got := u.ByRole(access.RoleComboBox, "Solo effort").Value(); got != "High" {
		t.Errorf("Settings shows Solo's effort as %q", got)
	}
}

// Skipping leaves the defaults; running the setup again shows the current values, not the defaults.
func TestSetupKeepsValues(t *testing.T) {
	f := newSetupFake()
	a, u := newSetupUI(t, f, 1440)
	u.Click("setup-skip")
	savedSetup(t, u, f, core.SetupDone)
	s, def := f.Settings(), core.DefaultSettings()
	s.Setup = ""
	if s.OrchestratorModel != def.OrchestratorModel || s.WorkerModel != def.WorkerModel || s.MergeMethod != def.MergeMethod ||
		s.NoNotifications || s.ShowCosts {
		t.Errorf("skipping changed the defaults: %+v", s)
	}

	_ = f.UpdateSettings(func(s *core.Settings) { s.WorkerModel, s.MergeMethod, s.ShowCosts = "haiku", "rebase", true })
	a.cfg.Set(f.Settings())
	a.startSetup()
	u.Frame()
	u.Click("setup-go-models")
	u.WaitFor("the models step", func() bool { return u.Has("model-threads-haiku") })
	if !u.Get("model-threads-haiku").Checked() {
		t.Error("running the setup again doesn't show the threads' model")
	}
	u.Click("setup-go-prefs")
	u.WaitFor("the preferences step", func() bool { return u.Has("pref-costs") })
	if !u.Get("pref-costs").Checked() {
		t.Error("running the setup again doesn't show costs as on")
	}
}

// How pull requests land is offered where a forge is in play, and saved as Settings saves it; where
// none is, it isn't offered.
func TestSetupLanding(t *testing.T) {
	f := newSetupFake().projects().withFolders("~/code")
	f.settings.Setup = "prs"
	a, u := newSetupUI(t, f, 1440)
	mustShow(t, u, "When work is ready to land", "Each thread's work lands as one commit on the base branch.")
	u.Get("merge-method").ByName("Rebase").Click()
	u.Click("auto-merge")
	u.Get("on-fail").ByName("Just notify me").Click()
	u.WaitFor("the choices to be saved", func() bool {
		s := f.Settings()
		return s.MergeMethod == "rebase" && s.AutoMerge && s.OnChecksFail == core.ChecksNotify
	})
	mustShow(t, u, "The thread's commits land one by one on top of the base branch.", "the thread waits for you")
	u.Click("setup-skip")
	a.openSettings("prs")
	u.Frame()
	if got := u.ByRole(access.RoleComboBox, "Merge method").Value(); got != "Rebase" {
		t.Errorf("Settings shows the merge method as %q", got)
	}

	local := newSetupFake().projects().withFolders("~/code")
	local.forges = nil
	local.settings.Setup = "prs"
	_, w := newSetupUI(t, local, 1440)
	if w.Has("landing") {
		t.Error("how pull requests land is offered without a forge")
	}
	mustShow(t, w, "Merge locally")
}

// The preferences: notifications and costs save at once, the apps once the typing stops (or the step
// changes), and Settings shows them.
func TestSetupPrefs(t *testing.T) {
	f := newSetupFake()
	f.settings.Setup = "prefs"
	a, u := newSetupUI(t, f, 1440)
	if !u.Get("pref-notify").Checked() || u.Get("pref-costs").Checked() {
		t.Error("the defaults aren't shown")
	}
	u.Click("pref-notify")
	u.Click("pref-costs")
	u.Get("pref-editor").Focus()
	u.Type("zed {path}")
	u.WaitFor("the switches to be saved", func() bool { s := f.Settings(); return s.NoNotifications && s.ShowCosts })
	mustShow(t, u, "Branch names, commit and pull request rules")
	u.Click("setup-back") // the typed command is saved when the step changes, if not before
	u.WaitFor("the editor to be saved", func() bool { return f.Settings().EditorCommand == "zed {path}" })
	u.Click("setup-skip")
	a.openSettings("general")
	u.Frame()
	if got := u.ByName("Editor command").Value(); got != "zed {path}" {
		t.Errorf("Settings shows the editor as %q", got)
	}
}

// overflow is how far the setup's step scrolls: it is scrolled down as far as it goes, and the title's
// move is measured, then it is scrolled back. 0 when the step fits.
func overflow(u *uitest.UI) float32 {
	before := u.Get("setup-title").Bounds().Y
	u.Get("setup-scroll").Scroll(0, 5000)
	for i := 0; i < 10; i++ {
		u.Advance(50 * time.Millisecond)
	}
	moved := before - u.Get("setup-title").Bounds().Y
	u.Get("setup-scroll").Scroll(0, -5000)
	for i := 0; i < 10; i++ {
		u.Advance(50 * time.Millisecond)
	}
	return moved
}

// The setup holds at Solo's smallest window, at a mid width and at the default size: nothing wider
// than the window, each step's title at the same height as every other's, and at the default size
// every step fits without scrolling, Models with a pinned version on both sides too.
func TestSetupSizes(t *testing.T) {
	_, dark := Themes()
	for _, size := range [][2]float32{{360, 480}, {1000, 760}, {1440, 900}} {
		titleTop := float32(-1)
		for _, step := range setupSteps {
			f := newSetupFake().projects().withFolders("~/code")
			f.settings.Setup = step.key
			a := New(f)
			u := uitest.New(t, a.Root, uitest.Size(size[0], size[1]), uitest.Theme(dark))
			started(a, u)
			u.WaitForQueries()
			u.Tree().Walk(func(n *access.Node) bool {
				if n.Bounds.W > 0 && n.Bounds.X+n.Bounds.W > size[0]+0.5 && n.Role != access.RoleWindow {
					t.Errorf("%v %s: %s %q reaches %.0f, past the window's %v", size, step.key, n.Role, n.Name, n.Bounds.X+n.Bounds.W, size[0])
				}
				return true
			})
			top := u.Get("setup-title").Bounds().Y
			if titleTop < 0 {
				titleTop = top
			} else if top != titleTop {
				t.Errorf("%v: the %s step's title is at %.0f, the welcome step's at %.0f", size, step.key, top, titleTop)
			}
			if size[0] == 1440 {
				if o := overflow(u); o > 0 {
					t.Errorf("%v: the %s step needs %.0f px of scrolling", size, step.key, o)
				}
			}
			u.Close()
			a.Close()
		}
	}

	// The steps at their longest also fit at 1440×900.
	for _, tc := range []struct {
		name string
		prep func(f *setupFake)
		want string // a key the state shows
	}{
		{"Models with a pinned version on both sides, five cards a side", func(f *setupFake) {
			f.settings.Setup = "models"
			f.settings.OrchestratorModel, f.settings.WorkerModel = "claude-opus-5", "claude-sonnet-4-6"
		}, "model-threads-claude-sonnet-4-6"},
		{"Claude Code missing, with the path field open", func(f *setupFake) {
			f.settings.Setup, f.claude, f.settings.ClaudePath = "claude", &claudeMissing, ""
		}, "claude-again"},
		{"Pull requests with gh missing and glab signed out", func(f *setupFake) {
			f.settings.Setup = "prs"
			f.forges[0] = core.ForgeStatus{Kind: forge.GitHub, CLI: "gh", Repos: f.forges[0].Repos, Login: "gh auth login",
				Install: "sudo dnf install gh", InstallURL: "https://cli.github.com"}
		}, "forges-again"},
	} {
		f := newSetupFake().projects().withFolders("~/code")
		tc.prep(f)
		a := New(f)
		u := uitest.New(t, a.Root, uitest.Size(1440, 900), uitest.Theme(dark))
		started(a, u)
		u.WaitForQueries()
		if tc.name[0] == 'C' {
			u.Click("claude-path-open")
			u.Frame()
		}
		if !u.Has(tc.want) {
			t.Errorf("%s: no %s", tc.name, tc.want)
		}
		if o := overflow(u); o > 0 {
			t.Errorf("%s needs %.0f px of scrolling at 1440×900", tc.name, o)
		}
		u.Close()
		a.Close()
	}
}

// Adding a folder fills the Repositories step downward: its title stays where it was.
func TestSetupTitleStaysOnAdd(t *testing.T) {
	f := newSetupFake().projects()
	f.settings.Setup = "repos"
	_, u := newSetupUI(t, f, 1440)
	before := u.Get("setup-title").Bounds().Y
	u.Click("suggest-add-~/code")
	u.WaitFor("the folder searched", func() bool { return u.Has("repos-found") })
	if after := u.Get("setup-title").Bounds().Y; after != before {
		t.Errorf("the title moved from %.0f to %.0f when a folder was added", before, after)
	}
}
