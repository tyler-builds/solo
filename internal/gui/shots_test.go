package gui

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
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
		"tall-settings-models-prompts": "card-briefer-prompt",
		"scrolled-up":                  "orch-transcript",
		"scrolled-up-thread":           "thread-c3d4e5f6-forms-transcript",
	}
	// Text selected once the window is up, by its start: the Quote button floats by it.
	selecting := map[string]string{"quote-select": "The settings migration is done"}
	// Text typed in a composer (by its text area's key) once the window is up: the "/" and "@" lists.
	composing := map[string][2]string{
		"composer-slash":       {"orch-draft-area", "Before you merge, run /re"},
		"composer-slash-lines": {"orch-draft-area", "Two things.\nFirst the docs, then a careful pass with /"},
		"composer-at-solo":     {"orch-draft-area", "Compare the auth code in @"},
		"composer-at":          {"draft-a1f3c9d2-oauth-area", "Also check @tok"},
		"composer-at-narrow":   {"draft-a1f3c9d2-oauth-area", "Also check @tok"},
		"worker-steer":         {"draft-a1f3c9d2-oauth-area", "Use the v2 token endpoint instead"},
	}
	// A code block's copy button (the index among the chat's), hovered or clicked once the window is up.
	codeHover := map[string]int{"code-hover": 0}
	codeCopy := map[string]int{"code-copied": 1}
	// Elements clicked, by key, once the window is up.
	clicking := map[string][]string{"merge-pill-menu": {"merge-pill-more-" + longReposID}}
	// Elements right-clicked, by key, once the window is up: their context menus.
	rightClicking := map[string]string{"lightbox-menu": "lightbox-view"}
	// The lightbox among three large screenshots, on the second, so that both arrows show.
	gallery := func(zoom viewer) func(a *App, f *fakeBackend) {
		return func(a *App, f *fakeBackend) {
			dir := t.TempDir()
			var paths []string
			for _, c := range []color.RGBA{{47, 214, 193, 255}, {90, 120, 230, 255}, {240, 170, 60, 255}} {
				paths = append(paths, largeScreenshot(dir, c))
			}
			v := openViewer(paths[1], paths)
			v.scale, v.x, v.y = zoom.scale, zoom.x, zoom.y
			a.lightbox.Set(v)
		}
	}
	// pill puts the merge pill's cards in the chat (mergePillBackend), the conversation moved past them,
	// with its list open or not.
	pill := func(cards []string, open bool) func(a *App, f *fakeBackend) {
		return func(a *App, f *fakeBackend) {
			g := mergePillBackend(cards, true)
			f.threads, f.items, f.pending = g.threads, g.items, g.pending
			a.setThreads(f.Threads())
			a.pending.Set(map[string][]core.PendingQuestion{})
			a.items = map[string]*ui.Signal[[]core.Item]{}
			a.pillOpen.Set(open)
		}
	}
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
			rm.StartPairing(false)
			rm.st.Requests = []api.RemoteRequest{{At: time.Now()}}
			a.Remote = rm
			a.settingsTab = "remote"
			a.settings.Set(true)
		}},
		{"settings-remote-pairing-open", func(a *App, f *fakeBackend) {
			rm := pairedRemote()
			rm.StartPairing(true)
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
		// The Usage page: the limits, the line of what is left, and what the threads used.
		{"usage-page", usagePage},
		{"usage-page-narrow", usagePage},
		{"usage-page-empty", func(a *App, f *fakeBackend) { f.rate = demoLimits(); a.rate.Set(f.rate); a.openUsage() }},
		// Many threads at work: the pinned Working group caps its height and scrolls within itself.
		{"working-many", func(a *App, f *fakeBackend) {
			manyWorking(f)
			a.setThreads(f.Threads())
			f.rate = demoLimits()
			a.rate.Set(f.rate)
		}},
		{"working-many-narrow", func(a *App, f *fakeBackend) {
			manyWorking(f)
			a.setThreads(f.Threads())
			f.rate = demoLimits()
			a.rate.Set(f.rate)
		}},
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
			a.orchAttachments.Set([]attachment{{id: 1, ref: sampleScreenshot(dir, color.RGBA{200, 140, 60, 255}), image: true}})
		}},
		// Files beside images: a sent message with a file, and the composer holding an image and a file.
		{"attach-files", attachShot(t, false)},
		// A file on its way from a page, part of it arrived, and Send waiting for it.
		{"attach-uploading", attachShot(t, true)},
		{"phone-attach-files", attachShot(t, false)},
		{"phone-attach-uploading", attachShot(t, true)},
		// An agent's message shows images inline: Solo shows a thread's before and after screenshots, and
		// the thread's own summary has them.
		{"agent-images", func(a *App, f *fakeBackend) {
			before, after := formScreenshots(t.TempDir())
			f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "How did the email field fix turn out?"})
			f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{Text: "**Fix the email field overlap** is done: on narrow windows the email field " +
				"now sits under its label instead of over the Save button.\n\n![Before](" + core.ImageDest(before) + ") ![After](" + core.ImageDest(after) + ")",
				Images: []string{before, after}})
			a.items = map[string]*ui.Signal[[]core.Item]{}
		}},
		// An agent's code blocks, each with its copy button: several lines, one line, one in a list and
		// one too long for the chat. Inline code has none.
		{"code", codeChat},
		{"code-hover", codeChat},
		{"code-copied", codeChat},
		{"agent-images-thread", func(a *App, f *fakeBackend) {
			before, after := formScreenshots(t.TempDir())
			f.add("c3d4e5f6-forms", core.ItemAssistant, core.TextData{Text: "Moved the email field under its label below 720 px, so it no longer " +
				"covers Save. Screenshots from `go test ./ui -run TestFormShots`:\n\n![Before](" + core.ImageDest(before) + ") ![After](" + core.ImageDest(after) + ")",
				Images: []string{before, after}})
			a.items = map[string]*ui.Signal[[]core.Item]{}
			a.openThread("c3d4e5f6-forms", "conversation")
		}},
		// An agent's chart block draws as a chart where it stands: in Solo's chat, and in a thread's summary.
		{"agent-chart", func(a *App, f *fakeBackend) {
			f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "Did the test speedup work? Where does the time go now?"})
			f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{Text: "**Speed up the test suite** is done: the suite " +
				"runs in 41 s instead of 74 s. Most of the gain is in gui, which no longer starts a window per test.\n\n" + shotChart +
				"\nstore barely moved; its time is the SQLite setup, which the thread left alone."})
			a.items = map[string]*ui.Signal[[]core.Item]{}
		}},
		{"agent-chart-thread", func(a *App, f *fakeBackend) {
			f.add("c3d4e5f6-forms", core.ItemAssistant, core.TextData{Text: "Shared one window across the gui tests and " +
				"cached the fixture repos. Committed on this branch.\n\n" + shotTrend})
			a.items = map[string]*ui.Signal[[]core.Item]{}
			a.openThread("c3d4e5f6-forms", "conversation")
		}},
		// A chart block Solo can't draw (here, a kind it doesn't know) shows as code.
		{"agent-chart-fallback", func(a *App, f *fakeBackend) {
			f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "Compare the two branches on each metric."})
			f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{Text: "Here they are side by side:\n\n" +
				"```chart\n{\"type\": \"radar\", \"title\": \"Branch comparison\",\n \"series\": [{\"name\": \"main\", " +
				"\"data\": [[\"speed\", 3], [\"memory\", 4]]}]}\n```\n\nThe branch wins on speed."})
			a.items = map[string]*ui.Signal[[]core.Item]{}
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
		// Solo's reply, then a turn that work it ran in the background started, with a line of its own:
		// both show. With "(noted)", the wake adds no message.
		{"solo-wake", func(a *App, f *fakeBackend) {
			soloWoken(f, "", "That was the old background task, nothing else.")
			a.items = map[string]*ui.Signal[[]core.Item]{}
		}},
		{"solo-wake-noted", func(a *App, f *fakeBackend) {
			soloWoken(f, "", core.Noted)
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
		// Solo mid-turn with a message typed: the button sends it (to be read when Solo finishes) rather
		// than steering, as it does for a worker (worker-steer).
		{"solo-working-draft", func(a *App, f *fakeBackend) {
			soloWorking(f, nil)
			a.setThreads(f.Threads())
			a.orchDraft.Set("Also, can a thread add a dark mode toggle to Settings?")
		}},
		// Solo mid-turn with two messages the user sent meanwhile, waiting their turn at the foot of the
		// chat; the composer is empty, so its button stops the turn.
		{"solo-queued", func(a *App, f *fakeBackend) {
			soloWorking(f, []core.QueuedMessage{
				{Text: "Also, can a thread add a dark mode toggle to Settings?", At: time.Now().Add(-40 * time.Second)},
				{Text: "And bump the Go version in CI while you're at it.", At: time.Now().Add(-10 * time.Second)},
			})
			a.setThreads(f.Threads())
		}},
		{"worker-steer", func(a *App, f *fakeBackend) { a.openThread("a1f3c9d2-oauth", "conversation") }},
		// The merge pill: closed and open, with one card, three in mixed states and one in every state; a
		// row's menu; beside an open thread; in a narrow and a compact window; on a phone.
		{"merge-pill-one", pill([]string{formsID}, false)},
		{"merge-pill-one-open", pill([]string{formsID}, true)},
		{"merge-pill-three", pill(pillThree, false)},
		{"merge-pill-three-open", pill(pillThree, true)},
		{"merge-pill-many", pill(pillMany, false)},
		{"merge-pill-many-open", pill(pillMany, true)},
		{"merge-pill-menu", pill(pillThree, true)},
		{"merge-pill-thread", func(a *App, f *fakeBackend) { pill(pillThree, true)(a, f); a.openThread(formsID, "conversation") }},
		{"merge-pill-narrow", pill(pillThree, true)},
		{"merge-pill-compact", pill(pillThree, true)},
		{"merge-pill-none", pill(nil, false)},
		// Solo's turn ended while a script it started runs on in the background: its chat says it waits on it.
		{"solo-background", func(a *App, f *fakeBackend) {
			soloOnBackground(f)
			a.setThreads(f.Threads())
		}},
		{"solo-background-many", func(a *App, f *fakeBackend) {
			soloOnBackground(f, core.BackgroundTask{ID: "a1", Type: "local_agent", Description: "Find large folders", Started: time.Now().Add(-time.Minute)},
				core.BackgroundTask{ID: "a2", Type: "local_bash", Description: "Watch free space", Started: time.Now().Add(-30 * time.Second)})
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
		// The thread the user has open finished work to land: Solo reviewed it and proposed the merge
		// without recapping what the user had just read there, so the card stands alone.
		{"open-thread-card", func(a *App, f *fakeBackend) {
			openThreadCard(f, false)
			a.items = map[string]*ui.Signal[[]core.Item]{}
			a.openThread("c3d4e5f6-forms", "conversation")
		}},
		// Scrolled up from the latest message: the round button above the composer goes back down.
		{"scrolled-up", func(a *App, f *fakeBackend) { longChat(f); a.items = map[string]*ui.Signal[[]core.Item]{} }},
		{"scrolled-up-thread", func(a *App, f *fakeBackend) {
			longChat(f)
			a.items = map[string]*ui.Signal[[]core.Item]{}
			a.openThread("c3d4e5f6-forms", "conversation")
		}},
		{"lightbox", gallery(viewer{})},
		{"lightbox-single", func(a *App, f *fakeBackend) {
			a.lightbox.Set(openViewer(sampleScreenshot(t.TempDir(), color.RGBA{47, 214, 193, 255}), nil))
		}},
		// Zoomed in to the image's own size, over the field the screenshot shows broken.
		{"lightbox-zoomed", gallery(viewer{scale: 1, x: -700, y: -400})},
		{"lightbox-menu", gallery(viewer{})},
		{"lightbox-compact", gallery(viewer{})},
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
		{"composer-slash", func(a *App, f *fakeBackend) {}},
		{"composer-slash-lines", func(a *App, f *fakeBackend) {}},
		{"composer-at-solo", func(a *App, f *fakeBackend) {}},
		{"composer-at", func(a *App, f *fakeBackend) { a.openThread("a1f3c9d2-oauth", "conversation") }},
		{"composer-at-narrow", func(a *App, f *fakeBackend) { a.openThread("a1f3c9d2-oauth", "conversation") }},
		{"history", func(a *App, f *fakeBackend) { a.settingsTab = "history"; a.settings.Set(true) }},
		{"settings-repos", func(a *App, f *fakeBackend) { a.settingsTab = "repos"; a.settings.Set(true) }},
		// Settings pages at full height, for review.
		{"tall-settings-general", func(a *App, f *fakeBackend) { a.settings.Set(true) }},
		{"tall-settings-repos", func(a *App, f *fakeBackend) { a.settingsTab = "repos"; a.settings.Set(true) }},
		{"tall-settings-models", func(a *App, f *fakeBackend) { a.settingsTab = "models"; a.settings.Set(true) }},
		{"tall-settings-models-prompts", func(a *App, f *fakeBackend) { a.settingsTab = "models"; a.settings.Set(true) }},
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
			f.settings.PRSkills = []string{"pr-style", "anthropic-skills:pdf", "gone-skill"}
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
		// The drawer with Claude's usage limits at its foot, in their one-line form.
		{"phone-usage", func(a *App, f *fakeBackend) { f.rate = demoLimits(); a.rate.Set(f.rate); a.drawer.Set(true) }},
		{"phone-usage-page", usagePage},
		{"phone-thread", func(a *App, f *fakeBackend) { a.openThread("c3d4e5f6-forms", "conversation") }},
		{"phone-changes", func(a *App, f *fakeBackend) { diffSplit.Set(false); a.openThread("c3d4e5f6-forms", "changes") }},
		{"phone-working", func(a *App, f *fakeBackend) { a.openThread("a1f3c9d2-oauth", "conversation") }},
		{"phone-multi", func(a *App, f *fakeBackend) { a.openThread("e5e6e7e8-landing", "conversation") }},
		{"phone-question", func(a *App, f *fakeBackend) { a.openThread("b7e21a90-limit", "conversation") }},
		{"phone-settings", func(a *App, f *fakeBackend) { a.settings.Set(true) }},
		{"phone-settings-branches", func(a *App, f *fakeBackend) { a.settingsTab = "branches"; a.settings.Set(true) }},
		{"phone-palette", func(a *App, f *fakeBackend) { a.palette.Set(true) }},
		{"phone-lightbox", gallery(viewer{})},
		{"phone-merge-pill", pill(pillThree, false)},
		{"phone-merge-pill-open", pill(pillMany, true)},
		{"phone-empty", nil},
		{"phone-solo-background", func(a *App, f *fakeBackend) {
			soloOnBackground(f)
			a.setThreads(f.Threads())
		}},
		{"phone-code", codeChat},
		// The board: cards with briefs in every column; beside an open card; in a narrow window; on a phone.
		{"board", boardDemo},
		{"board-thread", func(a *App, f *fakeBackend) { boardDemo(a, f); a.openThread("a1f3c9d2-oauth", "conversation") }},
		{"board-narrow", boardDemo},
		{"board-card", func(a *App, f *fakeBackend) { boardDemo(a, f); a.openCard("c3d4e5f6-forms") }},
		{"board-card-input", func(a *App, f *fakeBackend) { boardDemo(a, f); a.openCard("d9a8b7c6-flaky") }},
		{"board-card-working", func(a *App, f *fakeBackend) { boardDemo(a, f); a.openCard("a1f3c9d2-oauth") }},
		{"board-card-history", func(a *App, f *fakeBackend) { boardDemo(a, f); a.openCard("c3d4e5f6-forms"); a.cardTab.Set("history") }},
		{"phone-board-card", func(a *App, f *fakeBackend) { boardDemo(a, f); a.openCard("c3d4e5f6-forms") }},
		{"busy-board", boardDemo},
		{"phone-board", boardDemo},
		{"phone-thinking", func(a *App, f *fakeBackend) {
			soloWorking(f, nil)
			a.setThreads(f.Threads())
		}},
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
				sh.name != "scrolled-up" && sh.name != "usage" && sh.name != "usage-page" && sh.name != "phone-main" && !busy &&
				sh.name != "merge-pill-three" && sh.name != "merge-pill-three-open" && sh.name != "board" &&
				!strings.HasPrefix(sh.name, "code") && !strings.HasPrefix(sh.name, "lightbox") {
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
			if strings.HasSuffix(sh.name, "-compact") {
				w = 680
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
			for _, key := range clicking[sh.name] {
				u.Click(key)
				for i := 0; i < 4; i++ {
					u.Advance(100 * time.Millisecond)
				}
			}
			if key := rightClicking[sh.name]; key != "" {
				u.Get(key).RightClick()
				for i := 0; i < 4; i++ {
					u.Advance(100 * time.Millisecond)
				}
			}
			if i, ok := codeHover[sh.name]; ok {
				if b := codeCopyButton(u, i); b != nil {
					u.Runtime().PointerMove(b.Bounds.X+b.Bounds.W/2, b.Bounds.Y+b.Bounds.H/2, 0)
					for i := 0; i < 10; i++ { // long enough for the tooltip
						u.Advance(100 * time.Millisecond)
					}
				}
			}
			if i, ok := codeCopy[sh.name]; ok {
				if b := codeCopyButton(u, i); b != nil {
					u.ClickAt(b.Bounds.X+b.Bounds.W/2, b.Bounds.Y+b.Bounds.H/2)
					u.Runtime().PointerMove(10, 10, 0) // the check, not the hover look
					u.Advance(100 * time.Millisecond)
				}
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
			if c, ok := composing[sh.name]; ok {
				u.Get(c[0]).Focus()
				u.Type(c[1])
				for i := 0; i < 5; i++ {
					u.Advance(100 * time.Millisecond)
					time.Sleep(20 * time.Millisecond)
				}
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

// shotCode is an agent's reply with code blocks of each shape.
const shotCode = "Run the GUI's screenshot test with `SOLO_SHOTS` set:\n\n" +
	"```sh\nSOLO_SHOTS=/tmp/shots go test ./internal/gui -run TestShots\n```\n\n" +
	"Messages go through one view, which colors code with the shared highlighter:\n\n" +
	"```go\nfunc (a *App) md(ctx *ui.Context, text string) ui.Element {\n\treturn markdown.View(text).Code(code).CopyCode(true)\n}\n```\n\n" +
	"1. Rebuild and install:\n\n   ```sh\n   go install ./cmd/solo\n   ```\n\n" +
	"2. The log says where the shots went:\n\n" +
	"```\nshots: wrote 64 files to /tmp/shots (dark-main.png, dark-thread.png, dark-changes.png, dark-split.png, light-main.png, light-changes.png and 58 more)\n```"

// codeChat ends Solo's chat with a reply that has code blocks (shotCode).
func codeChat(a *App, f *fakeBackend) {
	f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "How do I take the screenshots again?"})
	f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{Text: shotCode})
	a.items = map[string]*ui.Signal[[]core.Item]{}
}

// codeCopyButton returns the copy button of the code block i of the window, or nil when code blocks
// have none.
func codeCopyButton(u *uitest.UI, i int) *access.Node {
	var blocks, buttons []*access.Node
	u.Tree().Walk(func(n *access.Node) bool {
		switch {
		case n.Role == access.RoleGroup && (n.Name == "Code" || strings.HasPrefix(n.Name, "Code, ")):
			blocks = append(blocks, n)
		case n.Role == access.RoleButton && n.Name == "Copy":
			buttons = append(buttons, n)
		}
		return true
	})
	var inBlocks []*access.Node
	for _, b := range buttons {
		for _, bl := range blocks {
			if r := bl.Bounds; b.Bounds.X >= r.X && b.Bounds.X+b.Bounds.W <= r.X+r.W && b.Bounds.Y >= r.Y && b.Bounds.Y+b.Bounds.H <= r.Y+r.H {
				inBlocks = append(inBlocks, b)
				break
			}
		}
	}
	if i >= len(inBlocks) {
		return nil
	}
	return inBlocks[i]
}

// largeScreenshot is sampleScreenshot at three times its size, 1920 by 1200: larger than the lightbox
// shows it fitted.
func largeScreenshot(dir string, accent color.RGBA) string {
	small := sampleScreenshot(dir, accent)
	in, err := os.Open(small)
	if err != nil {
		panic(err)
	}
	defer in.Close()
	img, err := png.Decode(in)
	if err != nil {
		panic(err)
	}
	big := image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx()*3, img.Bounds().Dy()*3))
	for y := range big.Rect.Dy() {
		for x := range big.Rect.Dx() {
			big.Set(x, y, img.At(x/3, y/3))
		}
	}
	p := strings.TrimSuffix(small, ".png") + "-large.png"
	out, err := os.Create(p)
	if err != nil {
		panic(err)
	}
	defer out.Close()
	png.Encode(out, big)
	return p
}

// openThreadCard ends Solo's conversation with its answer to the settings thread finishing while the
// user had it open: the update, Solo's review and its merge card, then "(noted)", or with recap, the
// summary Solo wrote before it knew the user had read the thread.
func openThreadCard(f *fakeBackend, recap bool) {
	o := core.OrchestratorID
	f.items[o] = nil
	f.add(o, core.ItemUser, core.UserData{Text: "Finish moving the settings page to the new form components."})
	f.add(o, core.ItemTool, tool("s1", "mcp__solo__spawn_thread", map[string]any{"repo": "web", "title": "Move settings page to new form components"}, "Started thread c3d4e5f6"))
	f.add(o, core.ItemAssistant, core.TextData{Text: "Started **Move settings page to new form components** in web."})
	line := "- Thread c3d4e5f6 \"Move settings page to new form components\" (web) finished its turn. Summary: Moved the settings page to the new Form components; 214 tests pass."
	if !recap {
		line += " [The user had this thread open and read its reply there: review it as usual and propose_merge if it's right, but don't recap it.]"
	}
	f.add(o, core.ItemUser, core.UserData{Synthetic: true, Text: "[thread update from Solo, not from the user]\n" + line})
	f.add(o, core.ItemTool, tool("s2", "mcp__solo__get_thread", map[string]any{"id": "c3d4e5f6"}, "Status: idle"))
	f.add(o, core.ItemTool, tool("s3", "mcp__solo__get_diff", map[string]any{"id": "c3d4e5f6"}, "diff --git …"))
	f.add(o, core.ItemTool, tool("s4", "mcp__solo__propose_merge", map[string]any{"id": "c3d4e5f6"}, "Proposed."))
	f.add(o, core.ItemProposal, core.ProposalData{ThreadID: "c3d4e5f6-forms", Title: "Move settings page to new form components",
		Summary: "Replaces the settings page's fields with the new Form components and deletes LegacyField. The diff is contained to settings and the web tests pass."})
	reply := "(noted)"
	if recap {
		reply = "The settings page now uses the new Form components: the thread added a `Toggle` to match `Checkbox`, moved the profile and notifications sections over, and deleted `LegacyField` once nothing used it. All 214 web tests pass, and the diff stays inside `src/settings` and `src/components/form`.\n\nIt looks right, so it's ready to merge."
	}
	f.add(o, core.ItemAssistant, core.TextData{Text: reply})
}

// soloWorking puts Solo mid-turn, starting a thread for the user's request, with queued messages waiting.
func soloWorking(f *fakeBackend, queued []core.QueuedMessage) {
	o := core.OrchestratorID
	f.add(o, core.ItemUser, core.UserData{Text: "Can you update my local Solo to match main?"})
	f.add(o, core.ItemTool, tool("c1", "Bash", map[string]any{"command": "git status"}, "On branch main"))
	for i := range f.threads {
		if th := &f.threads[i]; th.ID == o {
			th.Status, th.InTurn, th.TurnStarted = core.StatusRunning, true, time.Now().Add(-25*time.Second)
			th.Thought = "**Finding the install script** so the thread knows how to rebuild the AppImage."
			th.Queued = queued
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

// shotChart compares the test time of each package before and after a change.
const shotChart = "```chart\n" + `{"type": "bar", "title": "Test time by package", "x": "Package", "y": "Seconds",
 "series": [{"name": "main", "data": [["core", 12.5], ["gui", 38.2], ["orchestrator", 9.1], ["store", 4.3], ["remote", 10]]},
            {"name": "this branch", "data": [["core", 9.8], ["gui", 14.6], ["orchestrator", 6.2], ["store", 4.1], ["remote", 6.4]]}]}` + "\n```\n"

// shotTrend is the suite's time over its last runs.
const shotTrend = "```chart\n" + `{"type": "line", "title": "Suite time per run", "x": "Run", "y": "Seconds",
 "series": [{"name": "main", "data": [[1, 74], [2, 76], [3, 73], [4, 75], [5, 74], [6, 77]]},
            {"name": "this branch", "data": [[1, 74], [2, 61], [3, 52], [4, 47], [5, 43], [6, 41]]}]}` + "\n```\n"

// composerMarkdown is a draft with each kind of inline Markdown the composer formats.
const composerMarkdown = "Can you check these?\n\n**Skip writes** that change nothing: `Signal.Set` re-renders.\n" +
	"**A benchmark:** time a ~~write~~ *change* among 3,000 rows.\n**A clock:** see [the post](https://example.com/post)."

// formScreenshots writes a window's worth of a settings form before a fix (the email field over the
// Save button) and after it, as a thread would screenshot them.
func formScreenshots(dir string) (before, after string) {
	shot := func(name string, fixed bool) string {
		img := image.NewRGBA(image.Rect(0, 0, 1280, 800))
		fill := func(r image.Rectangle, c color.RGBA) { draw.Draw(img, r, &image.Uniform{c}, image.Point{}, draw.Src) }
		fill(img.Bounds(), color.RGBA{246, 246, 248, 255})
		fill(image.Rect(0, 0, 1280, 72), color.RGBA{32, 33, 38, 255})
		for i := 0; i < 3; i++ {
			y := 140 + i*140
			fill(image.Rect(80, y, 520, y+28), color.RGBA{120, 122, 130, 255})
			fill(image.Rect(80, y+48, 1200, y+112), color.RGBA{255, 255, 255, 255})
		}
		save := image.Rect(80, 660, 320, 724)
		fill(save, color.RGBA{47, 214, 193, 255})
		if fixed {
			fill(image.Rect(80, 560, 1200, 624), color.RGBA{255, 255, 255, 255})
			fill(image.Rect(80, 532, 360, 552), color.RGBA{120, 122, 130, 255})
		} else {
			fill(image.Rect(200, 640, 1200, 704), color.RGBA{230, 70, 70, 255}) // the email field, over Save
		}
		p := filepath.Join(dir, name)
		out, _ := os.Create(p)
		defer out.Close()
		png.Encode(out, img)
		return p
	}
	return shot("before.png", false), shot("after.png", true)
}

// attachShot is Solo's conversation with a message the user sent with a screenshot and two files, and
// the composer holding an image and a file: stored, or with uploading, part of the way to the machine.
func attachShot(t *testing.T, uploading bool) func(a *App, f *fakeBackend) {
	return func(a *App, f *fakeBackend) {
		dir := t.TempDir()
		log, _ := fileAttachment("build-2026-11-19.log", make([]byte, 2_400_000))
		csv, _ := fileAttachment("Q3 incident export.csv", make([]byte, 48_000))
		f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "The nightly build failed again. Here is its log and the export; can a thread find what broke?",
			Images: []string{sampleScreenshot(dir, color.RGBA{47, 214, 193, 255})},
			Files:  []core.FileRef{{Path: log, Name: "build-2026-11-19.log", Size: 2_400_000}, {Path: csv, Name: "Q3 incident export.csv", Size: 48_000}}})
		f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{Text: "I've started **Find the nightly build failure** with the log and the export. It will report back when it knows what broke."})
		a.items = map[string]*ui.Signal[[]core.Item]{}
		spec, _ := fileAttachment("Payment flow spec v3.pdf", make([]byte, 31_500_000))
		pdf := attachment{id: 2, ref: spec, name: "Payment flow spec v3.pdf", size: 31_500_000}
		if uploading {
			pdf = attachment{id: 2, name: "Payment flow spec v3.pdf", size: 31_500_000, sent: 12_900_000}
		}
		a.orchDraft.Set("Also use the spec for the payment flow.")
		a.orchAttachments.Set([]attachment{{id: 1, ref: sampleScreenshot(dir, color.RGBA{200, 140, 60, 255}), image: true}, pdf})
	}
}
