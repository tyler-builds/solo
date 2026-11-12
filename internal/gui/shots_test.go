package gui

import (
	"fmt"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

// TestShots renders the main views to PNGs for design review:
//
//	SOLO_SHOTS=/tmp/shots go test ./internal/gui -run TestShots
func TestShots(t *testing.T) {
	out := os.Getenv("SOLO_SHOTS")
	if out == "" {
		t.Skip("set SOLO_SHOTS to an output directory")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	light, dark := Themes()
	typing := map[string]string{"search": "token"} // typed once the window is up
	// The transcript scrolled up from its end, once the window is up.
	scrollUp := map[string]string{
		"scrolled-up":        "orch-transcript",
		"scrolled-up-thread": "thread-c3d4e5f6-forms-transcript",
	}
	// Text selected once the window is up, by its start: the Quote button floats by it.
	selecting := map[string]string{"quote-select": "The settings migration is done"}
	shots := []struct {
		name  string
		setup func(a *App, f *fakeBackend)
	}{
		{"main", func(a *App, f *fakeBackend) {}},
		{"thread", func(a *App, f *fakeBackend) { a.openThread("c3d4e5f6-forms", "conversation") }},
		{"changes", func(a *App, f *fakeBackend) { diffSplit.Set(false); a.openThread("c3d4e5f6-forms", "changes") }},
		{"split", func(a *App, f *fakeBackend) {
			diffSplit.Set(true)
			a.panelGoal = 60 // as if Split had widened the panel to fit
			a.openThread("c3d4e5f6-forms", "changes")
		}},
		{"working", func(a *App, f *fakeBackend) { a.openThread("a1f3c9d2-oauth", "conversation") }},
		// A thread whose turn ended while its benchmarks run in the background: at work, not ready.
		{"background", func(a *App, f *fakeBackend) {
			th := waitingOnBackground(f)
			a.setThreads(f.Threads())
			a.openThread(th.ID, "conversation")
		}},
		{"repo-filter", func(a *App, f *fakeBackend) { a.navSel.Set("repo:/home/dev/code/api") }},
		{"opening", func(a *App, f *fakeBackend) { a.openID.Set("c3d4e5f6-forms"); a.panelAnim.Set(0.35) }},
		{"narrow-thread", func(a *App, f *fakeBackend) { a.openThread("c3d4e5f6-forms", "conversation") }},
		{"settings", func(a *App, f *fakeBackend) { diffSplit.Set(false); a.settings.Set(true) }},
		{"settings-remote", func(a *App, f *fakeBackend) {
			a.Remote = pairedRemote()
			a.settingsTab = "remote"
			a.settings.Set(true)
		}},
		{"settings-remote-pairing", func(a *App, f *fakeBackend) {
			rm := pairedRemote()
			rm.StartPairing()
			rm.st.Requests = []api.RemoteRequest{{At: time.Now()}}
			a.Remote = rm
			a.settingsTab = "remote"
			a.settings.Set(true)
		}},
		{"settings-remote-new", func(a *App, f *fakeBackend) {
			a.Remote = &fakeRemote{st: api.RemoteState{Connection: api.RemoteOff, Cloud: api.RemoteCloud{CanDeploy: true}}}
			a.settingsTab = "remote"
			a.settings.Set(true)
		}},
		{"palette", func(a *App, f *fakeBackend) { a.palette.Set(true) }},
		// Claude's usage limits at the foot of the rail, and the notice when only a status is known.
		{"usage", func(a *App, f *fakeBackend) { f.rate = demoLimits(); a.rate.Set(f.rate) }},
		{"usage-narrow", func(a *App, f *fakeBackend) { f.rate = demoLimits(); a.rate.Set(f.rate) }},
		// Many threads at work: the pinned Working group caps its height and scrolls within itself.
		{"working-many", func(a *App, f *fakeBackend) { manyWorking(f); a.setThreads(f.Threads()); f.rate = demoLimits(); a.rate.Set(f.rate) }},
		{"working-many-narrow", func(a *App, f *fakeBackend) { manyWorking(f); a.setThreads(f.Threads()); f.rate = demoLimits(); a.rate.Set(f.rate) }},
		{"usage-notice", func(a *App, f *fakeBackend) {
			f.rate = []agent.Event{{Kind: agent.EventRateLimit, RateLimitType: "five_hour", Status: "rejected",
				ResetsAt: float64(time.Now().Add(time.Hour).Unix())}}
			a.rate.Set(f.rate)
		}},
		{"pill", func(a *App, f *fakeBackend) {
			// A long exchange after the merge card pushes it out of view.
			for i := 0; i < 8; i++ {
				f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: fmt.Sprintf("How is the OAuth fix going? (%d)", i+1)})
				f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{Text: "Still running the auth tests. The refresh path now retries once on a 401; the remaining work is the test for an expired refresh token, which the worker is writing now. I'll tell you when it finishes."})
			}
			a.items = map[string]*ui.Signal[[]core.Item]{}
		}},
		{"images", func(a *App, f *fakeBackend) {
			dir := t.TempDir()
			f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "The email field overlaps the save button on narrow windows. Can a thread fix it?",
				Images: []string{sampleScreenshot(dir, color.RGBA{47, 214, 193, 255}), sampleScreenshot(dir, color.RGBA{90, 120, 230, 255})}})
			a.items = map[string]*ui.Signal[[]core.Item]{}
			a.orchImages.Set([]string{sampleScreenshot(dir, color.RGBA{200, 140, 60, 255})})
		}},
		{"paths", func(a *App, f *fakeBackend) {
			dir := filepath.Join(t.TempDir(), "solo-brand")
			_ = os.MkdirAll(dir, 0o755)
			for _, n := range []string{"preview-orbit.png", "preview-branch.png"} {
				_ = os.WriteFile(filepath.Join(dir, n), nil, 0o644)
			}
			f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{Text: "The previews are in " + dir +
				": `" + filepath.Join(dir, "preview-orbit.png") + "` and " + filepath.Join(dir, "preview-branch.png") + "."})
			a.items = map[string]*ui.Signal[[]core.Item]{}
		}},
		{"calm", func(a *App, f *fakeBackend) {
			// A busy turn: thoughts, tool calls and words between them fold into one line above the reply.
			o := core.OrchestratorID
			f.add(o, core.ItemUser, core.UserData{Text: "Can you update my local Solo to match main?"})
			f.add(o, core.ItemThinking, core.TextData{Text: "Check the branch first."})
			f.add(o, core.ItemTool, tool("c1", "Bash", map[string]any{"command": "git status"}, "On branch main"))
			f.add(o, core.ItemAssistant, core.TextData{Text: "It's on main. Let me find the install script."})
			f.add(o, core.ItemTool, tool("c2", "mcp__solo__list_repos", map[string]any{}, "solo"))
			f.add(o, core.ItemTool, tool("c3", "mcp__solo__spawn_thread", map[string]any{"repo": "web", "title": "Install Solo from main"}, "Started thread a1f3c9d2"))
			f.add(o, core.ItemAssistant, core.TextData{Text: "The repo is already on `main`, so I started **Install Solo from main** to rebuild the AppImage and install it."})
			a.items = map[string]*ui.Signal[[]core.Item]{}
		}},
		{"thinking", func(a *App, f *fakeBackend) {
			// Solo mid-turn: its working line shows the first line of its newest thought's last paragraph.
			for i := range f.threads {
				if th := &f.threads[i]; th.ID == core.OrchestratorID {
					th.Status, th.InTurn = core.StatusRunning, true
					th.Thought = "The user wants the forms thread merged.\n\n" +
						"**Checking the merge state** of the forms thread before I land it, since its checks were still pending an hour ago and the base may have moved.\nThen I'll merge."
				}
			}
			a.setThreads(f.Threads())
		}},
		{"land-cards", func(a *App, f *fakeBackend) {
			// Merge cards in each checks state, a local merge, and one whose Merge is under way.
			o := core.OrchestratorID
			f.items[o] = nil
			for i := range f.threads {
				th := &f.threads[i]
				switch th.ID {
				case "c3d4e5f6-forms": // local: no remote
					th.Forge, th.CanOpenPR, th.CanMerge = forge.None, false, true
				case "a1f3c9d2-oauth": // passed, and merging now
					th.Status, th.InTurn, th.Forge, th.CanMerge = core.StatusIdle, false, forge.GitHub, true
					th.PR = &forge.PR{Kind: forge.GitHub, Number: 61, State: forge.StateOpen, Checks: forge.ChecksPassing, Mergeable: true}
					a.busy.Set(map[string]bool{th.ID: true})
				}
			}
			for _, id := range []string{"c3d4e5f6-forms", "a1f3c9d2-oauth", "b1b2b3b4-paginate", "c1c2c3c4-csv"} {
				f.add(o, core.ItemProposal, core.ProposalData{ThreadID: id, Summary: "Ready to land."})
			}
			a.setThreads(f.Threads())
			a.items = map[string]*ui.Signal[[]core.Item]{}
		}},
		// Scrolled up from the latest message: the round button above the composer goes back down.
		{"scrolled-up", func(a *App, f *fakeBackend) { longChat(f); a.items = map[string]*ui.Signal[[]core.Item]{} }},
		{"scrolled-up-thread", func(a *App, f *fakeBackend) {
			longChat(f)
			a.items = map[string]*ui.Signal[[]core.Item]{}
			a.openThread("c3d4e5f6-forms", "conversation")
		}},
		{"lightbox", func(a *App, f *fakeBackend) {
			a.lightbox.Set(sampleScreenshot(t.TempDir(), color.RGBA{47, 214, 193, 255}))
		}},
		{"done", func(a *App, f *fakeBackend) { a.openThread("a0b1c2d3-audit", "conversation") }},
		{"in-place", func(a *App, f *fakeBackend) { a.openThread("a7a7a7a7-subs", "conversation") }},
		// Quoting: text selected in the chat offers Quote; quotes wait above the composer, and a
		// message sent with one shows it as a blockquote.
		{"quote-select", func(a *App, f *fakeBackend) {}},
		{"quote", func(a *App, f *fakeBackend) {
			f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: quoteMessage([]string{"Redis works across instances, and the API already runs it for sessions."}, "Go with Redis, then.")})
			a.items = map[string]*ui.Signal[[]core.Item]{}
			a.addQuote(core.OrchestratorID, "The settings migration is done and reviewed; it's ready to merge.")
			a.addQuote(core.OrchestratorID, "Redis works across instances")
			a.orchDraft.Set("Merge it first")
		}},
		// Markdown in the composer, formatted: its syntax shows only where the caret is.
		{"composer-markdown", func(a *App, f *fakeBackend) { a.orchDraft.Set(composerMarkdown) }},
		{"history", func(a *App, f *fakeBackend) { a.settingsTab = "history"; a.settings.Set(true) }},
		{"settings-repos", func(a *App, f *fakeBackend) { a.settingsTab = "repos"; a.settings.Set(true) }},
		// Settings pages at full height, for review.
		{"tall-settings-general", func(a *App, f *fakeBackend) { a.settings.Set(true) }},
		{"tall-settings-repos", func(a *App, f *fakeBackend) { a.settingsTab = "repos"; a.settings.Set(true) }},
		{"tall-settings-models", func(a *App, f *fakeBackend) { a.settingsTab = "models"; a.settings.Set(true) }},
		{"tall-settings-branches", func(a *App, f *fakeBackend) {
			f.settings.BranchRules = "feature/<ticket>-<short-summary>, lowercase, under 50 characters. fix/ for bug fixes."
			f.settings.RepoBranchRules = map[string]string{"/home/dev/code/api": "api-<ticket>-<summary>"}
			f.settings.CommitRules = "Start with the ticket, like OW-123: Refresh tokens. Imperative mood, under 72 characters."
			a.settingsTab = "branches"
			a.settings.Set(true)
		}},
		{"tall-settings-prs", func(a *App, f *fakeBackend) {
			f.settings.PRTitleRules = "Conventional Commit title with the ticket, like feat(auth): OW-123 refresh tokens."
			f.settings.PRDescriptionRules = "Summary, Changes, Testing."
			a.settingsTab = "prs"
			a.settings.Set(true)
		}},
		// Pull requests that get a title only: no description rules.
		{"tall-settings-prs-title-only", func(a *App, f *fakeBackend) {
			f.settings.SkipPRDescription = true
			a.settingsTab = "prs"
			a.settings.Set(true)
		}},
		{"tall-settings-history", func(a *App, f *fakeBackend) { a.settingsTab = "history"; a.settings.Set(true) }},
		{"settings-models", func(a *App, f *fakeBackend) { a.settingsTab = "models"; a.settings.Set(true) }},
		{"settings-git", func(a *App, f *fakeBackend) {
			f.settings.BranchRules = "feature/<ticket>-<short-summary>, lowercase, under 50 characters. fix/ for bug fixes."
			f.settings.RepoBranchRules = map[string]string{"/home/dev/code/api": "api-<ticket>-<summary>"}
			f.settings.PRTitleRules = "Conventional Commit title with the ticket."
			a.settingsTab = "git"
			a.settings.Set(true)
		}},
		{"review", func(a *App, f *fakeBackend) { a.openThread("b1b2b3b4-paginate", "conversation") }},
		{"multi-open", func(a *App, f *fakeBackend) { a.openThread("d1d2d3d4-export", "conversation") }},
		{"multi-prs", func(a *App, f *fakeBackend) { a.openThread("e5e6e7e8-landing", "conversation") }},
		{"multi-changes", func(a *App, f *fakeBackend) { diffSplit.Set(false); a.openThread("d1d2d3d4-export", "changes") }},
		{"filter", func(a *App, f *fakeBackend) { a.railQuery.Set("audit"); a.openThread("c3d4e5f6-forms", "conversation") }},
		{"search", func(a *App, f *fakeBackend) { a.palette.Set(true) }},
		{"empty", nil},
		// A heavy day, from busyBackend: many threads, several questions, a long chat with Solo.
		{"busy-main", func(a *App, f *fakeBackend) {}},
		{"busy-thread", func(a *App, f *fakeBackend) { a.openThread("a1f3c9d2-oauth", "conversation") }},
		{"busy-question", func(a *App, f *fakeBackend) { a.openThread("b7e21a90-limit", "conversation") }},
		// A phone: one pane at a time, switched in the title bar.
		{"phone-main", func(a *App, f *fakeBackend) {}},
		{"phone-threads", func(a *App, f *fakeBackend) { a.drawer.Set(true) }},
		{"phone-thread", func(a *App, f *fakeBackend) { a.openThread("c3d4e5f6-forms", "conversation") }},
		{"phone-changes", func(a *App, f *fakeBackend) { diffSplit.Set(false); a.openThread("c3d4e5f6-forms", "changes") }},
		{"phone-working", func(a *App, f *fakeBackend) { a.openThread("a1f3c9d2-oauth", "conversation") }},
		{"phone-multi", func(a *App, f *fakeBackend) { a.openThread("e5e6e7e8-landing", "conversation") }},
		{"phone-question", func(a *App, f *fakeBackend) { a.openThread("b7e21a90-limit", "conversation") }},
		{"phone-settings", func(a *App, f *fakeBackend) { a.settings.Set(true) }},
		{"phone-settings-branches", func(a *App, f *fakeBackend) { a.settingsTab = "branches"; a.settings.Set(true) }},
		{"phone-palette", func(a *App, f *fakeBackend) { a.palette.Set(true) }},
		{"phone-empty", nil},
	}
	for _, theme := range []struct {
		name string
		t    *ui.Theme
	}{{"dark", dark}, {"light", light}} {
		for _, sh := range shots {
			busy := strings.HasPrefix(sh.name, "busy-")
			phone := strings.HasPrefix(sh.name, "phone-")
			if only := os.Getenv("SOLO_SHOTS_ONLY"); only != "" && !strings.HasPrefix(sh.name, only) {
				continue
			}
			if theme.name == "light" && sh.name != "main" && sh.name != "changes" && sh.name != "land-cards" &&
				sh.name != "scrolled-up" && sh.name != "usage" && sh.name != "phone-main" && !busy {
				continue
			}
			f := demoBackend()
			if busy {
				f = busyBackend()
			}
			if sh.setup == nil {
				f = &fakeBackend{items: map[string][]core.Item{}, repos: f.repos, settings: f.settings}
			}
			a := New(f)
			a.Touch = phone
			if sh.setup != nil {
				sh.setup(a, f)
			}
			w := float32(1440)
			if strings.HasSuffix(sh.name, "-narrow") || sh.name == "narrow-thread" {
				w = 1000
			}
			h := float32(900)
			if strings.HasPrefix(sh.name, "tall-") {
				h = 1400
			}
			if phone {
				w, h = 390, 844
			}
			opts := []uitest.Option{uitest.Size(w, h), uitest.Theme(theme.t), uitest.Bindings(Bindings()...)}
			if phone {
				// A web page has no caption buttons (":" is none at either end).
				opts = append(opts, uitest.Settings(ui.SystemSettings{Dark: theme.name == "dark", CaptionButtons: ":",
					CaretBlink: 530 * time.Millisecond, CaretWidth: 1}))
			}
			u := uitest.New(t, a.Root, opts...)
			for i := 0; i < 10; i++ {
				u.Advance(100 * time.Millisecond)
				time.Sleep(20 * time.Millisecond)
			}
			if key := scrollUp[sh.name]; key != "" {
				u.Get(key).Scroll(0, -3000)
				for i := 0; i < 10; i++ {
					u.Advance(100 * time.Millisecond)
				}
			}
			if prefix := selecting[sh.name]; prefix != "" {
				selectSpan(t, u, prefix, 300)
				u.Advance(100 * time.Millisecond)
			}
			if text := typing[sh.name]; text != "" {
				u.Type(text)
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

// manyWorking puts more threads to work than the rail's Working group has room for.
func manyWorking(f *fakeBackend) {
	for i, title := range []string{"Add retries to the webhook sender", "Cache the dashboard query",
		"Split the billing worker", "Upgrade the date picker", "Lint the migrations folder",
		"Profile the export job", "Translate the settings page", "Tidy the logging setup",
		"Speed up the CI cache", "Retire the v1 endpoints", "Document the deploy steps"} {
		th := f.threads[1] // a running worker to copy
		th.ID, th.Title = fmt.Sprintf("w%07d-many", i), title
		th.TurnStarted = time.Now().Add(-time.Duration(i+1) * 37 * time.Second)
		f.threads = append(f.threads, th)
	}
}

// longChat gives Solo's chat and the forms thread more than a window's worth of messages.
func longChat(f *fakeBackend) {
	for _, id := range []string{core.OrchestratorID, "c3d4e5f6-forms"} {
		for i := 0; i < 8; i++ {
			f.add(id, core.ItemUser, core.UserData{Text: fmt.Sprintf("How is the OAuth fix going? (%d)", i+1)})
			f.add(id, core.ItemAssistant, core.TextData{Text: "Still running the auth tests. The refresh path now retries once on a 401; the remaining work is the test for an expired refresh token, which the worker is writing now. I'll tell you when it finishes."})
		}
	}
}

// composerMarkdown is a draft with each kind of inline Markdown the composer formats.
const composerMarkdown = "Can you check these?\n\n**Skip writes** that change nothing: `Signal.Set` re-renders.\n" +
	"**A benchmark:** time a ~~write~~ *change* among 3,000 rows.\n**A clock:** see [the post](https://example.com/post)."
