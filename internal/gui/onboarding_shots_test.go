package gui

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

// TestSetupShots renders the first-run setup, the empty chat and the hints to PNGs for design review,
// as TestShots does the rest:
//
//	SOLO_SHOTS=/tmp/shots go test ./internal/gui -run TestSetupShots
//
// A name ending in -narrow is a compact window; -phone is a web page on a phone.
func TestSetupShots(t *testing.T) {
	out := os.Getenv("SOLO_SHOTS")
	if out == "" {
		t.Skip("set SOLO_SHOTS to an output directory")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	light, dark := Themes()
	step := func(s string) func(*App) { return func(a *App) { a.setup.Set(s) } }
	fresh := func() *setupFake { return newSetupFake().projects() }
	type shot struct {
		name  string
		light bool // also in the light theme
		back  func() *setupFake
		setup func(a *App)
		typed [2]string // a field's key and what is typed in it once the window is up
		sizes bool      // also at a mid width (-mid) and at Solo's smallest window (-min)
	}
	ghMissing := func() *setupFake {
		f := fresh().withFolders("~/code")
		f.forges[0] = core.ForgeStatus{Kind: forge.GitHub, CLI: "gh", Repos: f.forges[0].Repos, Login: "gh auth login",
			Install: "sudo dnf install gh", InstallURL: "https://cli.github.com"}
		return f
	}
	steps := []shot{
		{name: "setup-welcome", light: true, back: fresh, sizes: true},
		{name: "setup-claude-ok", light: true, back: fresh, setup: step("claude"), sizes: true},
		{name: "setup-claude-missing", light: true, back: func() *setupFake { f := fresh(); f.claude = &claudeMissing; return f }, setup: step("claude"), sizes: true},
		{name: "setup-claude-signed-out", back: func() *setupFake { f := fresh(); f.claude = &claudeSignedOut; return f }, setup: step("claude")},
		{name: "setup-claude-error", back: func() *setupFake { f := fresh(); f.claude = &claudeBroken; return f }, setup: step("claude")},
		{name: "setup-claude-path", back: func() *setupFake {
			f := fresh()
			f.settings.ClaudePath = "~/tools/claude"
			return f
		}, setup: step("claude")},
		{name: "setup-models", light: true, back: fresh, setup: step("models"), sizes: true},
		// Other choices than the defaults, among them a pinned version from the list under the cards.
		{name: "setup-models-picked", back: func() *setupFake {
			f := fresh()
			f.settings.OrchestratorModel, f.settings.OrchestratorEffort = "fable", "high"
			f.settings.WorkerModel, f.settings.WorkerEffort = "claude-sonnet-4-6", "low"
			return f
		}, setup: step("models")},
		// A pinned version on both sides: five cards in each panel, which still fit.
		{name: "setup-models-pinned", light: true, back: func() *setupFake {
			f := fresh()
			f.settings.OrchestratorModel, f.settings.WorkerModel = "claude-opus-5", "claude-sonnet-4-6"
			return f
		}, setup: step("models")},
		// Before Claude Code reports its models: the fallback list.
		{name: "setup-models-unknown", back: func() *setupFake { f := fresh(); f.claude = &claudeMissing; return f }, setup: step("models")},
		{name: "setup-repos-empty", light: true, back: fresh, setup: step("repos"), sizes: true},
		{name: "setup-repos-none-suggested", back: func() *setupFake { f := fresh(); f.suggest = nil; return f }, setup: step("repos")},
		{name: "setup-repos-found", light: true, back: func() *setupFake { return fresh().withFolders("~/code", "~/notes") }, setup: step("repos"), sizes: true},
		{name: "setup-repos-nothing", back: func() *setupFake { return fresh().withFolders("~/empty") }, setup: func(a *App) {
			a.setup.Set("repos")
			a.setupAdding.Set([]string{"~/src"})
		}},
		{name: "setup-prs-ready", back: func() *setupFake {
			f := fresh().withFolders("~/code")
			f.forges[1].SignedIn, f.forges[1].Account = true, "octocat on gitlab.com"
			return f
		}, setup: step("prs"), sizes: true},
		{name: "setup-prs-problems", light: true, back: ghMissing, setup: step("prs"), sizes: true},
		{name: "setup-prs-local-only", back: func() *setupFake { f := fresh().withFolders("~/code"); f.forges = nil; return f }, setup: step("prs")},
		{name: "setup-prs-no-repos", back: fresh, setup: step("prs")},
		{name: "setup-prefs", light: true, back: fresh, setup: step("prefs"), sizes: true},
	}
	var shots []shot
	for _, sh := range steps {
		shots = append(shots, sh)
		if sh.sizes {
			for _, size := range []string{"-mid", "-min"} {
				v := sh
				v.name += size
				shots = append(shots, v)
			}
		}
	}
	shots = append(shots, []shot{
		// After the setup: the empty chat's tasks to start with, and without repositories.
		{name: "chat-starters", light: true, back: func() *setupFake { return done(fresh().withFolders("~/code")) }},
		{name: "chat-starters-narrow", back: func() *setupFake { return done(fresh().withFolders("~/code")) }},
		{name: "chat-starters-phone", back: func() *setupFake { return done(fresh().withFolders("~/code")) }},
		{name: "chat-starter-picked", back: func() *setupFake { return done(fresh().withFolders("~/code")) },
			setup: func(a *App) {}, typed: [2]string{"starter-1", ""}},
		{name: "chat-no-repos", back: func() *setupFake { return done(fresh()) }},
		// The hints, each as it first shows.
		{name: "hint-thread", light: true, back: func() *setupFake { return hinted(t, "thread") }},
		{name: "hint-thread-narrow", back: func() *setupFake { return hinted(t, "thread") }},
		{name: "hint-question", back: func() *setupFake { return hinted(t, "question") }},
		{name: "hint-merge", light: true, back: func() *setupFake { return hinted(t, "merge") }},
		{name: "hint-remote", back: func() *setupFake { return hinted(t, "remote") },
			setup: func(a *App) { a.Remote = &fakeRemote{st: api.RemoteState{Connection: api.RemoteOff}} }},
		// Settings › General, with Run setup again.
		{name: "settings-setup-again", light: true, back: func() *setupFake { return done(fresh().withFolders("~/code")) },
			setup: func(a *App) { a.settings.Set(true) }},
	}...)
	for _, theme := range []struct {
		name string
		t    *ui.Theme
	}{{"dark", dark}, {"light", light}} {
		for _, sh := range shots {
			if only := os.Getenv("SOLO_SHOTS_ONLY"); only != "" && !strings.HasPrefix(sh.name, only) {
				continue
			}
			if theme.name == "light" && !sh.light {
				continue
			}
			f := sh.back()
			a := New(f)
			phone := strings.HasSuffix(sh.name, "-phone")
			if phone {
				// A web page drives the machine through a backend that isn't on it.
				a = New(f.fakeBackend)
				a.Touch = true
			}
			if sh.setup != nil {
				sh.setup(a)
			}
			w, h := float32(1440), float32(900)
			switch {
			case strings.HasSuffix(sh.name, "-narrow"):
				w, h = 560, 900
			case strings.HasSuffix(sh.name, "-mid"):
				w, h = 1000, 760
			case strings.HasSuffix(sh.name, "-min"):
				w, h = 360, 480 // Solo's smallest window
			}
			if phone {
				w, h = 390, 844
			}
			opts := []uitest.Option{uitest.Size(w, h), uitest.Theme(theme.t), uitest.Bindings(Bindings()...)}
			if phone {
				opts = append(opts, uitest.Settings(ui.SystemSettings{Dark: theme.name == "dark", CaptionButtons: ":",
					CaretBlink: 530 * time.Millisecond, CaretWidth: 1}))
			}
			u := uitest.New(t, a.Root, opts...)
			for i := 0; i < 10; i++ {
				u.Advance(100 * time.Millisecond)
				time.Sleep(20 * time.Millisecond)
			}
			u.WaitForQueries()
			if sh.typed[0] != "" {
				u.Get(sh.typed[0]).Click()
				if sh.typed[1] != "" {
					u.Type(sh.typed[1])
				}
				for i := 0; i < 5; i++ {
					u.Advance(100 * time.Millisecond)
				}
			}
			u.Settle()
			file, err := os.Create(filepath.Join(out, theme.name+"-"+sh.name+".png"))
			if err != nil {
				t.Fatal(err)
			}
			png.Encode(file, u.Screenshot())
			file.Close()
			u.Close()
		}
	}
}

// done is the fake with the setup finished.
func done(f *setupFake) *setupFake {
	f.settings.Setup = core.SetupDone
	return f
}

// hinted is the fake after the setup, at the moment a hint first applies: Solo started its first
// thread ("thread"), a thread asks ("question"), Solo proposed landing its work ("merge"), or work
// landed ("remote").
func hinted(t *testing.T, what string) *setupFake {
	f := done(newSetupFake().projects().withFolders("~/code"))
	demo := demoBackend()
	f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "The login test fails about one run in ten. Can you find out why and fix it?"})
	o := core.OrchestratorID
	th := demo.threads[0]
	for _, d := range demo.threads {
		if d.ID == "c3d4e5f6-forms" {
			th = d
		}
	}
	th.Title, th.RepoPath = "Fix the flaky login test", filepath.Join(homeDir("code"), "web")
	th.ID = "f1a2b3c4-login"
	f.add(o, core.ItemAssistant, core.TextData{Text: "I started **Fix the flaky login test** in web. It will run the test in a loop to catch the failure, then fix it."})
	switch what {
	case "thread":
		th.Status, th.InTurn, th.TurnStarted = core.StatusRunning, true, time.Now().Add(-40*time.Second)
	case "question":
		th.Status, th.PendingAsk = core.StatusWaiting, true
		f.pending = map[string][]core.PendingQuestion{th.ID: demo.pending["b7e21a90-limit"]}
		if len(f.pending[th.ID]) == 0 {
			for _, qs := range demo.pending {
				f.pending[th.ID] = qs
			}
		}
	case "merge":
		th.Status = core.StatusIdle
		f.add(o, core.ItemAssistant, core.TextData{Text: "**Fix the flaky login test** is done: the test waited on a timer that the CI machines sometimes ran late. It now waits for the login event instead, and passed 200 runs in a row."})
		f.add(o, core.ItemProposal, core.ProposalData{ThreadID: th.ID, Summary: "The login test waits for the login event instead of a timer; 200 runs in a row pass."})
	case "remote":
		th.Status, th.DoneAt = core.StatusMerged, time.Now()
		f.add(o, core.ItemAssistant, core.TextData{Text: "**Fix the flaky login test** is merged."})
	}
	f.threads = append([]core.ThreadView{demo.threads[0]}, th)
	f.threads[0] = orchestratorOf(demo)
	return f
}

// orchestratorOf is the demo's orchestrator thread.
func orchestratorOf(f *fakeBackend) core.ThreadView {
	for _, th := range f.threads {
		if th.ID == core.OrchestratorID {
			th.InTurn, th.Status = false, core.StatusIdle
			return th
		}
	}
	return core.ThreadView{}
}
