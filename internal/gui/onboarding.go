package gui

// The first-run setup: four short steps in place of the main view (Welcome, Claude Code,
// Repositories, Pull requests) that check the machine and set up what Solo needs, then land in Solo's
// chat. Every step can be skipped, and so can the whole setup. Each step reads the machine through the
// backend's own checks (CheckClaude, api.Setup) and changes it through the settings, as Settings does,
// so what is set here shows there. Where it stands is kept in Settings.Setup: a step to resume at, or
// done. A web page never shows it (its backend isn't an api.Setup): its machine is set up at the
// machine.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

// setupSteps are the setup's steps, in order, by the name Settings.Setup keeps.
var setupSteps = []struct{ key, title string }{
	{"welcome", "Welcome"},
	{"claude", "Claude Code"},
	{"repos", "Repositories"},
	{"prs", "Pull requests"},
}

// setupIndex is a step's place in setupSteps, or -1.
func setupIndex(key string) int {
	return slices.IndexFunc(setupSteps, func(s struct{ key, title string }) bool { return s.key == key })
}

// setupWidth is the width of the setup's column.
const setupWidth = 640

// setupBackend is the backend's side that checks the machine, or nil when it drives another machine.
func (a *App) setupBackend() api.Setup {
	s, _ := a.b.(api.Setup)
	return s
}

// firstRun decides, as the app starts, whether the setup shows and at which step: "" for not at all.
// Someone who already uses Solo (folders to search, threads, or a conversation with Solo) counts as set
// up, so an update doesn't walk them through it, or teach them what they know (hints.go).
func (a *App) firstRun() string {
	if a.setupBackend() == nil {
		return ""
	}
	cfg := a.b.Settings()
	switch {
	case cfg.Setup == core.SetupDone:
		return ""
	case cfg.Setup != "":
		if setupIndex(cfg.Setup) < 0 {
			return setupSteps[0].key
		}
		return cfg.Setup // closed halfway: carry on there
	case a.usedBefore(cfg):
		if err := a.b.UpdateSettings(func(s *core.Settings) {
			s.Setup = core.SetupDone
			for _, h := range allHints {
				if !slices.Contains(s.Hints, h) {
					s.Hints = append(s.Hints, h)
				}
			}
		}); err != nil {
			a.notify(ui.ToastError, "Couldn't save the settings", err.Error())
		}
		return ""
	}
	return setupSteps[0].key
}

// usedBefore reports whether Solo was in use before the setup existed.
func (a *App) usedBefore(cfg core.Settings) bool {
	if len(cfg.Folders) > 0 {
		return true
	}
	for _, th := range a.b.Threads() {
		if th.IsWorker {
			return true
		}
	}
	items, _ := a.b.Items(context.Background(), core.OrchestratorID, 1)
	return len(items) > 0
}

// setupSaver keeps Settings.Setup: saves run off the UI thread, one at a time, and each saves the
// latest step, so two quick steps can't land in the wrong order.
type setupSaver struct {
	mu   sync.Mutex // held while saving
	want string
	wmu  sync.Mutex // guards want
}

// saveSetup records where the setup stands.
func (a *App) saveSetup(step string) {
	a.setupSave.wmu.Lock()
	a.setupSave.want = step
	a.setupSave.wmu.Unlock()
	go func() {
		a.setupSave.mu.Lock()
		defer a.setupSave.mu.Unlock()
		a.setupSave.wmu.Lock()
		v := a.setupSave.want
		a.setupSave.wmu.Unlock()
		if a.b.Settings().Setup == v {
			return
		}
		if err := a.b.UpdateSettings(func(s *core.Settings) { s.Setup = v }); err != nil {
			a.notify(ui.ToastError, "Couldn't save the setup", err.Error())
		}
	}()
}

// goSetup shows a step of the setup and puts the focus on its main button, so Enter carries on.
func (a *App) goSetup(step string) {
	a.setup.Set(step)
	a.setupFocus.Focus()
	a.saveSetup(step)
}

// finishSetup ends the setup, finished or skipped, and lands in Solo's chat with the caret in its
// message box.
func (a *App) finishSetup() {
	a.setup.Set("")
	a.saveSetup(core.SetupDone)
	a.orchFocus.Focus()
}

// startSetup shows the setup again from its start (Settings › General).
func (a *App) startSetup() {
	a.settings.Set(false)
	a.drawer.Set(false)
	if a.openID.Peek() != "" {
		a.closeThread()
	}
	a.goSetup(setupSteps[0].key)
}

// setupAgainSection is Settings › General's way back into the setup; nil where there is none (a web
// page).
func (a *App) setupAgainSection(ctx *ui.Context, closeSettings func()) ui.Element {
	if a.setupBackend() == nil {
		return nil
	}
	return settingsSection(ctx, "Setup", "", nil,
		settingRow(ctx, "First-run setup", "Checks Claude Code, your repository folders and pull request tools again.",
			ui.Button("Run setup again").Icon(icons.RotateCcw).Secondary().Small().Key("run-setup").
				OnClick(func() {
					closeSettings()
					a.startSetup()
				})))
}

// setupBar is the window's title bar during the setup: the Solo mark, and Skip setup, which is always
// there.
func (a *App) setupBar(ctx *ui.Context) ui.Element {
	h := float32(40)
	if isCompact(ctx) {
		h = 48
	}
	return ui.TitleBar().Height(ui.Px(h)).
		Child(ui.Div().Row().Align(ui.Center).Gap(space2).PaddingLeft(space2).
			Child(brandMark(20)).
			Child(ui.Text("Set up Solo").FontSize(13).Medium().NoWrap())).
		Trailing(ui.Button("Skip setup").Ghost().Small().Key("setup-skip").
			Tooltip("Go straight to Solo. Settings has all of this, and can run the setup again.").
			OnClick(a.finishSetup))
}

// setupView is the setup: where it stands, the step, and the buttons that move between steps.
func (a *App) setupView(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	step := a.setup.Get()
	i := max(setupIndex(step), 0)
	compact := isCompact(ctx)
	next := func() {
		if i+1 < len(setupSteps) {
			a.goSetup(setupSteps[i+1].key)
		} else {
			a.finishSetup()
		}
	}
	// The step's main button takes the focus when the setup first shows, as each step change gives it.
	if first := ui.Local(ctx, "focused", false); !first.Peek() {
		first.Set(true)
		a.setupFocus.Focus()
	}

	var body ui.Element
	primary, icon := "Continue", icons.ArrowRight
	switch setupSteps[i].key {
	case "welcome":
		body, primary = ui.Component(a.setupWelcome), "Get started"
	case "claude":
		body = ui.Component(a.setupClaude)
	case "repos":
		body = ui.Component(a.setupRepos)
	case "prs":
		body, primary, icon = ui.Component(a.setupPRs), "Start using Solo", nil
	}
	gutterX := float32(space6)
	if compact {
		gutterX = space4
	}
	column := func() *ui.DivElement {
		return ui.Div().Column().Width(ui.Full).MaxWidth(ui.Px(setupWidth))
	}
	content := ui.Div().Column().Align(ui.Center).Grow(1).MinHeight(ui.Px(0)).ScrollY().
		PaddingX(gutterX).PaddingTop(space8).PaddingBottom(space8).Key("setup-scroll").
		Child(column().Gap(space8).
			Child(a.setupProgress(ctx, i)).
			Child(ui.Div().Key("setup-step-" + setupSteps[i].key).Child(body)))

	main := ui.Button(primary).Primary().Key("setup-next").FocusHandle(a.setupFocus).OnClick(next)
	if icon != nil {
		main = main.IconEnd(icon)
	}
	footRow := column().Row().Align(ui.Center).Gap(space2)
	if i > 0 {
		prev := setupSteps[i-1].key
		footRow = footRow.Child(ui.Button("Back").Icon(icons.ArrowLeft).Ghost().Key("setup-back").
			OnClick(func() { a.goSetup(prev) }))
	}
	footRow = footRow.Child(ui.Div().Grow(1))
	if !compact && !a.Touch {
		footRow = footRow.Child(ui.Kbd("Enter"))
	}
	footRow = footRow.Child(main)
	foot := ui.Div().Column().Align(ui.Center).Shrink(0).PaddingX(gutterX).PaddingY(space3).
		BorderEdges(ui.Edges{Top: 1}, t.Border).Child(footRow)

	panel := ui.Div().Column().Size(ui.Full, ui.Full).
		OnKeyDown(func(e *ui.KeyEvent) {
			// Enter carries on from anywhere a field doesn't take it.
			if e.Key == input.KeyEnter && e.Mods == 0 {
				e.StopPropagation()
				next()
			}
		}).
		Child(content).Child(foot)
	if compact {
		return ui.Div().Column().Size(ui.Full, ui.Full).Background(t.Background).
			BorderEdges(ui.Edges{Top: 1}, t.Border).Child(panel)
	}
	return ui.Div().Column().Size(ui.Full, ui.Full).PaddingX(space2).PaddingBottom(space2).
		Child(inset(ctx, panel))
}

// setupProgress shows how far the setup has come: a bar for each step, filled up to this one, with the
// steps' names under them (in a compact window, the one step's name). A step can be picked to go to it.
func (a *App) setupProgress(ctx *ui.Context, cur int) ui.Element {
	t := ctx.Theme()
	compact := isCompact(ctx)
	bars := ui.Div().Row().Gap(space2).Width(ui.Full).Role(access.RoleList).Label("Setup steps")
	for i, s := range setupSteps {
		key := s.key
		c := t.Border
		if i <= cur {
			c = t.Text
		}
		label := ui.Text(s.title).FontSize(12).TextColor(t.TextSubtle).NoWrap().Truncate()
		switch {
		case i == cur:
			label = ui.Text(s.title).FontSize(12).Semibold().TextColor(t.Text).NoWrap().Truncate()
		case i < cur:
			label = ui.Text(s.title).FontSize(12).TextColor(t.TextMuted).NoWrap().Truncate()
		}
		state := "to do"
		if i == cur {
			state = "current"
		} else if i < cur {
			state = "done"
		}
		item := ui.Div().Column().Gap(space2).Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(0)).Cursor(ui.CursorPointer).
			Role(access.RoleButton).Label(fmt.Sprintf("Step %d of %d: %s, %s", i+1, len(setupSteps), s.title, state)).
			TabIndex(0).AccessSelected(i == cur).Key("setup-go-" + key).
			OnClick(func() { a.goSetup(key) }).
			Child(ui.Div().Height(ui.Px(3)).Radius(2).Background(c))
		if !compact {
			item = item.Child(label)
		}
		bars = bars.Child(item)
	}
	if !compact {
		return bars
	}
	return ui.Div().Column().Gap(space2).Width(ui.Full).
		Child(bars).
		Child(caption(ctx, fmt.Sprintf("Step %d of %d · %s", cur+1, len(setupSteps), setupSteps[cur].title)))
}

// setupHead is a step's title and what it is about.
func setupHead(ctx *ui.Context, heading, lead string) ui.Element {
	return ui.Div().Column().Gap(space2).
		Child(title(heading)).
		Child(ui.Text(lead).FontSize(14).TextColor(ctx.Theme().TextMuted))
}

// setupCard is a rounded card of rows, as Settings draws them.
func setupCard(ctx *ui.Context, rows ...ui.Element) *ui.DivElement {
	t := ctx.Theme()
	card := ui.Div().Column().Radius(radiusL).Border(1, t.Border).Background(t.Surface)
	n := 0
	for _, r := range rows {
		if r == nil {
			continue
		}
		cell := ui.Div().Column().Child(r)
		if n > 0 {
			cell = cell.BorderEdges(ui.Edges{Top: 1}, t.Border)
		}
		card = card.Child(cell)
		n++
	}
	return card
}

// statusLine is a check's result: an icon in its color, a line in words, and what follows under it.
func statusLine(ctx *ui.Context, icon *icons.Icon, c ui.Color, line string, under ...ui.Element) *ui.DivElement {
	t := ctx.Theme()
	col := ui.Div().Column().Gap(space1).Grow(1).MinWidth(ui.Px(0)).
		Child(ui.Text(line).FontSize(13).Medium().TextColor(t.Text))
	for _, u := range under {
		if u != nil {
			col = col.Child(u)
		}
	}
	return ui.Div().Row().Align(ui.Start).Gap(space3).
		Child(ui.Div().PaddingTop(1).Child(ui.Icon(icon).Size(16).Color(c))).
		Child(col)
}

// commandBox is a command to run in a terminal, with a button that copies it.
func commandBox(ctx *ui.Context, cmd string) ui.Element {
	t := ctx.Theme()
	return ui.Div().Row().Align(ui.Center).Gap(space2).PaddingLeft(space3).PaddingRight(space1).Height(ui.Px(34)).
		MarginTop(space1).Radius(radiusS).Border(1, t.Border).Background(t.Background).
		Child(ui.Text(cmd).Mono().FontSize(12).TextColor(t.Text).Selectable().Truncate().Grow(1).MinWidth(ui.Px(0))).
		Child(ui.CopyButton(cmd).Small().Tooltip("Copy the command").Key("copy-" + cmd))
}

// checking is a check under way.
func checking(ctx *ui.Context, what string) ui.Element {
	return ui.Div().Row().Align(ui.Center).Gap(space3).
		Child(ui.ProgressRing().Size(16)).
		Child(muted(ctx, what))
}

// recheckOnFocus fetches a check again when the window comes back to the front while it shows a
// problem: the user was likely in a terminal fixing it.
func (a *App) recheckOnFocus(ctx *ui.Context, problem bool, refetch func()) {
	gained := a.focusGained.Get()
	last := ui.Local(ctx, "focus-seen", gained)
	if gained != last.Peek() {
		last.Set(gained)
		if problem {
			ctx.Post(refetch)
		}
	}
}

// nameList names up to n of the paths by their folder names: "api, web and 3 more".
func nameList(paths []string, n int) string {
	var names []string
	for i, p := range paths {
		if i == n {
			return strings.Join(names, ", ") + fmt.Sprintf(" and %d more", len(paths)-n)
		}
		names = append(names, filepath.Base(p))
	}
	if len(names) > 1 {
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
	return strings.Join(names, "")
}

// count says how many of something there are: "1 repository", "6 repositories".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// ---- Welcome ----

// setupWelcome says what Solo is, with a picture of how work goes round: you ask, Solo starts threads,
// each works on its own branch, and you review and land what they made.
func (a *App) setupWelcome(ctx *ui.Context) ui.Element {
	return ui.Div().Column().Gap(space6).
		Child(ui.Div().Column().Gap(space4).
			Child(brandMark(44)).
			Child(setupHead(ctx, "Welcome to Solo",
				"You tell Solo what you want done, and it runs each task as a thread on its own branch until the work is ready for you to land."))).
		Child(a.loopPicture(ctx)).
		Child(caption(ctx, "Three short checks follow: Claude Code, your repositories, and pull requests. You can skip any of them."))
}

// loopPicture draws the loop with the app's own pieces: a message, threads at work, a branch, and a
// merge card's button.
func (a *App) loopPicture(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	compact := isCompact(ctx)
	threadLine := func(title string) ui.Element {
		return ui.Div().Row().Align(ui.Center).Gap(6).
			Child(statusMark(ctx, core.StatusRunning)).
			Child(ui.Text(title).FontSize(12).TextColor(t.Text).NoWrap())
	}
	tiles := []struct {
		label, detail string
		visual        ui.Element
	}{
		{"You ask", "in plain words", ui.Div().PaddingX(space2 + 2).PaddingY(space1 + 2).Radius(radiusL).Background(t.SurfaceActive).
			Child(ui.Text("Fix the flaky login test").FontSize(12).TextColor(t.Text))},
		{"Solo starts threads", "one for each task", ui.Div().Column().Gap(space1 + 2).
			Child(threadLine("Fix login test")).Child(threadLine("Add retry test"))},
		{"Each on its own branch", "in a copy of the repository", ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).Height(ui.Px(24)).
			Radius(radiusS).Border(1, t.Border).Background(t.Background).
			Child(ui.Icon(icons.GitBranch).Size(12).Color(t.TextMuted)).
			Child(ui.Text("solo/login").Mono().FontSize(11).TextColor(t.TextMuted).NoWrap())},
		{"You review and land", "only you can merge", ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(ui.Icon(icons.GitMerge).Size(14).Color(t.Success)).
			Child(ui.Div().Row().Align(ui.Center).PaddingX(space3).Height(ui.Px(24)).Radius(radiusS).Background(t.Accent).
				Child(ui.Text("Merge").FontSize(12).Medium().TextColor(t.AccentText)))},
	}
	pic := ui.Div().Role(access.RoleImage).
		Label("You ask. Solo starts threads, one for each task. Each works on its own branch. You review and land the work.")
	if compact {
		col := ui.Div().Column().Gap(space1).AccessHidden()
		for i, tl := range tiles {
			if i > 0 {
				col = col.Child(ui.Div().PaddingLeft(space4 + 4).Child(ui.Icon(icons.ArrowDown).Size(14).Color(t.TextSubtle)))
			}
			col = col.Child(ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space3).PaddingY(space3).
				Radius(radiusL).Border(1, t.Border).Background(t.Surface).
				Child(ui.Div().Column().Gap(2).Width(ui.Px(130)).Shrink(0).
					Child(subhead(tl.label)).Child(caption(ctx, tl.detail))).
				Child(ui.Div().Row().Grow(1).MinWidth(ui.Px(0)).ClipContent().Child(tl.visual)))
		}
		return pic.Child(col)
	}
	row := ui.Div().Row().Align(ui.Stretch).Gap(space1).AccessHidden()
	for i, tl := range tiles {
		if i > 0 {
			row = row.Child(ui.Div().Column().Justify(ui.Center).Shrink(0).
				Child(ui.Icon(icons.ArrowRight).Size(14).Color(t.TextSubtle)))
		}
		row = row.Child(ui.Div().Column().Gap(space3).Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(0)).
			PaddingX(space3).PaddingY(space3).Radius(radiusL).Border(1, t.Border).Background(t.Surface).ClipContent().
			Child(ui.Div().Column().Justify(ui.Center).Height(ui.Px(48)).Child(tl.visual)).
			Child(ui.Div().Column().Gap(2).
				Child(ui.Text(tl.label).FontSize(13).Medium().TextColor(t.Text)).
				Child(caption(ctx, tl.detail))))
	}
	return pic.Child(row)
}

// ---- Claude Code ----

// claudeInstall is the command that installs Claude Code here.
func claudeInstall() string {
	if runtime.GOOS == "windows" {
		return "irm https://claude.ai/install.ps1 | iex"
	}
	return "curl -fsSL https://claude.ai/install.sh | bash"
}

// setupClaude finds Claude Code and shows what it found: ready, or what is wrong and what to run. It
// checks again when the window comes back to the front. A path can be set, as in Settings › General.
func (a *App) setupClaude(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	path := strings.TrimSpace(a.cfg.Get().ClaudePath)
	q := a.claudeQuery(ctx, path)
	st := q.Data
	busy := q.Loading || q.Fetching
	a.recheckOnFocus(ctx, !busy && st.Err != "", q.Refetch)

	again := ui.Button("Check again").Icon(icons.RefreshCw).Secondary().Small().Key("claude-again").OnClick(q.Refetch)
	where := caption(ctx, claudeWhere(st, "the path you set")).TruncateHead()
	var status ui.Element
	switch {
	case busy:
		status = checking(ctx, "Looking for Claude Code…")
	case st.Path == "":
		status = statusLine(ctx, icons.CircleAlert, t.Warning, "Claude Code isn't installed",
			muted(ctx, "Install it with this command in a terminal, then run claude once to sign in. Solo checks again when you come back to this window."),
			commandBox(ctx, claudeInstall()),
			ui.Div().Row().PaddingTop(space2).Child(again))
	case st.SignedOut:
		status = statusLine(ctx, icons.CircleAlert, t.Warning, "Claude Code isn't signed in",
			muted(ctx, "Run claude in a terminal and sign in with /login. Solo checks again when you come back to this window."),
			commandBox(ctx, "claude"),
			where,
			ui.Div().Row().PaddingTop(space2).Child(again))
	case st.Err != "":
		status = statusLine(ctx, icons.CircleAlert, t.Danger, "Solo can't use this Claude Code",
			muted(ctx, st.Err), where,
			ui.Div().Row().PaddingTop(space2).Child(again))
	default:
		line := "Claude Code " + st.Version + " is ready"
		var who ui.Element
		if st.Account != "" {
			who = muted(ctx, "Signed in as "+st.Account)
		}
		status = statusLine(ctx, icons.CircleCheck, t.Success, line, who, where)
	}
	var note ui.Element
	if !busy && st.Err != "" {
		note = caption(ctx, "You can carry on and fix this later: Settings › General shows the same check. Solo can't start threads until Claude Code works.")
	}
	return ui.Div().Column().Gap(space6).
		Child(setupHead(ctx, "Claude Code",
			"Solo runs your own Claude Code, with its sign-in and plan. If it works in your terminal, there is nothing to do here.")).
		Child(setupCard(ctx, ui.Div().Padding(space4).Key("claude-status").Child(status))).
		Child(a.claudePathField(ctx, path)).
		Child(note)
}

// claudePathField sets the path of the claude to run, the setting Settings › General shows. It is
// folded away until asked for, unless a path is set.
func (a *App) claudePathField(ctx *ui.Context, path string) ui.Element {
	open := ui.Local(ctx, "path-open", path != "")
	text := ui.Local(ctx, "path", path)
	if !open.Get() {
		return ui.Div().Row().Child(ui.Button("Claude Code is somewhere else?").Ghost().Small().Key("claude-path-open").
			OnClick(func() { open.Set(true) }))
	}
	save := func(p string) {
		p = strings.TrimSpace(p)
		a.run("Saving the path", func(context.Context) error {
			err := a.b.UpdateSettings(func(s *core.Settings) { s.ClaudePath = p })
			a.onUI(func() { a.cfg.Set(a.b.Settings()) })
			return err
		})
	}
	row := ui.Div().Row().Align(ui.Center).Gap(space2).
		Child(ui.TextInput(text).Placeholder("Path to claude").Label("Path to claude").Small().Grow(1).
			Key("claude-path").AutoFocus().OnSubmit(save)).
		Child(ui.Button("Use this path").Secondary().Small().Key("claude-path-use").
			Disabled(strings.TrimSpace(text.Get()) == "" || strings.TrimSpace(text.Get()) == path).
			OnClick(func() { save(text.Peek()) }))
	if path != "" {
		row = row.Child(ui.Button("Find it myself").Ghost().Small().Key("claude-path-clear").
			Tooltip("Forget this path, and look on PATH and where the installer puts it").
			OnClick(func() { text.Set(""); save("") }))
	}
	return ui.Div().Column().Gap(space2).
		Child(caption(ctx, "The claude program Solo runs. Leave it empty to find it on PATH or where the installer puts it.")).
		Child(row)
}

// ---- Repositories ----

// expandHome undoes tildePath.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return filepath.Clean(p)
}

// inFolder lists the paths that are folder or inside it.
func inFolder(paths []string, folder string) []string {
	dir := expandHome(folder)
	var out []string
	for _, p := range paths {
		if p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) {
			out = append(out, p)
		}
	}
	return out
}

// addFolders adds folders to the ones searched, as Settings › Repositories does, and shows what the
// search finds as soon as it is done.
func (a *App) addFolders(paths []string) {
	var add []string
	for _, p := range paths {
		if p = tildePath(strings.TrimSpace(p)); p != "" && !slices.Contains(a.cfg.Peek().Folders, p) {
			add = append(add, p)
		}
	}
	if len(add) == 0 {
		return
	}
	a.setupAdding.Set(append(slices.Clone(a.setupAdding.Peek()), add...))
	a.run("Adding the folder", func(context.Context) error {
		err := a.b.UpdateSettings(func(s *core.Settings) {
			for _, p := range add {
				s.Folders = withFolder(s.Folders, p)
			}
		})
		a.onUI(func() {
			a.setupAdding.Set(slices.DeleteFunc(slices.Clone(a.setupAdding.Peek()), func(p string) bool { return slices.Contains(add, p) }))
			a.syncFolders()
		})
		return err
	})
}

// removeFolder stops searching a folder.
func (a *App) removeFolder(folder string) {
	a.run("Removing the folder", func(context.Context) error {
		err := a.b.UpdateSettings(func(s *core.Settings) {
			s.Folders = slices.DeleteFunc(slices.Clone(s.Folders), func(f string) bool { return f == folder })
		})
		a.onUI(a.syncFolders)
		return err
	})
}

// syncFolders reads the folders and what was found in them now, rather than at the next refresh.
func (a *App) syncFolders() {
	a.cfg.Set(a.b.Settings())
	a.repos.Set(a.b.RepoPaths())
	a.plain.Set(a.b.PlainFolders())
}

// setupRepos adds the folders that hold the user's projects, with the system's folder picker or from
// the usual places Solo found, and shows at once what it found in each.
func (a *App) setupRepos(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	folders := a.cfg.Get().Folders
	repos, plain := a.repos.Get(), a.plain.Get()
	adding := a.setupAdding.Get()
	typed := ui.Local(ctx, "typed", "")

	choose := ui.Button("Choose a folder…").Icon(icons.FolderOpen).Secondary().Key("repos-choose").
		OnClick(func() { a.chooseFolders(ctx, a.addFolders) })
	addTyped := func() {
		a.addFolders([]string{typed.Peek()})
		typed.Set("")
	}
	pickRow := ui.Div().Row().Wrap().Align(ui.Center).Gap(space2).
		Child(choose).
		Child(ui.Div().Row().Align(ui.Center).Gap(space2).Grow(1).Basis(ui.Px(220)).
			Child(ui.TextInput(typed).Placeholder("or type a path, such as ~/projects").Label("Folder to add").Small().
				Grow(1).Key("repos-path").OnSubmit(func(string) { addTyped() })).
			Child(ui.Button("Add").Secondary().Small().Key("repos-path-add").
				Disabled(strings.TrimSpace(typed.Get()) == "").OnClick(addTyped)))

	col := ui.Div().Column().Gap(space6).
		Child(setupHead(ctx, "Your repositories",
			"Add the folders that hold your projects. Solo finds the git repositories in them, up to three folders deep.")).
		Child(ui.Div().Column().Gap(space3).
			Child(pickRow).
			Child(caption(ctx, "A folder that isn't in git works too: its threads edit it in place, without a branch of their own.")))

	// What was found, over the folders searched.
	var rows []ui.Element
	shown := append(slices.Clone(folders), adding...)
	for _, f := range shown {
		folder := f
		pending := slices.Contains(adding, f) && !slices.Contains(folders, f)
		found, outside := inFolder(repos, f), inFolder(plain, f)
		var detail ui.Element
		switch {
		case pending:
			detail = ui.Div().Row().Align(ui.Center).Gap(space2).
				Child(ui.ProgressRing().Size(12)).Child(caption(ctx, "Looking for repositories…"))
		case len(found) > 0:
			line := count(len(found), "repository", "repositories") // the line over the folders names them
			if len(outside) > 0 {
				line += " · " + count(len(outside), "folder", "folders") + " not in git"
			}
			detail = caption(ctx, line)
		case len(outside) == 1 && outside[0] == expandHome(f):
			detail = caption(ctx, "Not in git. Threads work in it directly.")
		case len(outside) > 0:
			detail = caption(ctx, "No repositories; "+count(len(outside), "folder", "folders")+" not in git: "+nameList(outside, 4))
		default:
			detail = ui.Text("Nothing found here. Pick the folder that holds your projects.").FontSize(12).TextColor(t.Warning)
		}
		icon := icons.FolderGit2
		if !pending && len(found) == 0 {
			icon = icons.Folder
		}
		row := ui.Div().Row().Align(ui.Center).Gap(space3).PaddingLeft(space4).PaddingRight(space2).PaddingY(space3).
			Key("folder-" + folder).
			Child(ui.Icon(icon).Size(16).Color(t.TextMuted)).
			Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
				Child(ui.Text(folder).FontSize(13).Medium().TextColor(t.Text).TruncateHead()).
				Child(detail))
		if !pending {
			row = row.Child(ui.IconButton(icons.X).Ghost().Small().Label("Remove " + folder).Tooltip("Stop searching here").
				OnClick(func() { a.removeFolder(folder) }))
		}
		rows = append(rows, row)
	}
	if len(rows) > 0 {
		var head ui.Element
		var all []string
		for _, f := range folders {
			all = append(all, inFolder(repos, f)...)
		}
		if len(all) > 0 {
			head = ui.Div().Row().Align(ui.Center).Gap(space2).Key("repos-found").
				Child(ui.Icon(icons.CircleCheck).Size(16).Color(t.Success)).
				Child(ui.Text("Found " + count(len(all), "repository", "repositories") + ": " + nameList(all, 6)).
					FontSize(13).Medium().TextColor(t.Text).Grow(1).MinWidth(ui.Px(0)))
		}
		col = col.Child(ui.Div().Column().Gap(space3).Child(head).Child(setupCard(ctx, rows...)))
	}

	// Folders where projects usually are, found to hold repositories: the user adds them, or not.
	if sb := a.setupBackend(); sb != nil {
		q := ui.Query(ctx, "setup-suggest", func(c context.Context) ([]core.FolderSuggestion, error) {
			return sb.SuggestFolders(c), nil
		})
		var sugg []ui.Element
		for _, s := range q.Data {
			p := tildePath(s.Path)
			if slices.Contains(shown, p) {
				continue
			}
			sugg = append(sugg, ui.Div().Row().Align(ui.Center).Gap(space3).PaddingLeft(space4).PaddingRight(space3).PaddingY(space3).
				Key("suggest-"+p).
				Child(ui.Icon(icons.FolderSearch).Size(16).Color(t.TextMuted)).
				Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
					Child(ui.Text(p).FontSize(13).Medium().TextColor(t.Text).TruncateHead()).
					Child(caption(ctx, count(len(s.Repos), "repository", "repositories")+": "+nameList(s.Repos, 5)))).
				Child(ui.Button("Add").Icon(icons.Plus).Secondary().Small().Label("Add "+p).Key("suggest-add-"+p).
					OnClick(func() { a.addFolders([]string{p}) })))
		}
		if len(sugg) > 0 {
			col = col.Child(ui.Div().Column().Gap(space2).
				Child(ui.Text("FOUND ON THIS COMPUTER").FontSize(11).FontWeight(ui.FontSemibold).TextColor(t.TextSubtle)).
				Child(setupCard(ctx, sugg...)))
		}
	}
	if len(shown) == 0 {
		col = col.Child(caption(ctx, "You can also add folders later, in Settings › Repositories."))
	}
	return col
}

// ---- Pull requests ----

// forgeName is a forge's name.
func forgeName(k forge.Kind) string {
	return map[forge.Kind]string{forge.GitHub: "GitHub", forge.GitLab: "GitLab"}[k]
}

// setupPRs shows, for each forge the repositories push to, whether its CLI is ready to open pull
// requests, and the one command to run when it isn't. It is optional: without one, work merges locally.
func (a *App) setupPRs(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	repos := a.repos.Get()
	col := ui.Div().Column().Gap(space6).
		Child(setupHead(ctx, "Pull requests",
			"Optional. Solo opens pull requests with GitHub's gh or GitLab's glab, signed in as you. Without them, work merges on this machine."))
	sb := a.setupBackend()
	if len(repos) == 0 || sb == nil {
		return col.Child(setupCard(ctx, ui.Div().Padding(space4).Child(
			statusLine(ctx, icons.Info, t.TextMuted, "No repositories yet",
				muted(ctx, "Once you add repositories, this shows what they need. Settings › General can run the setup again.")))))
	}
	q := ui.Query(ctx, "setup-forges:"+strings.Join(repos, "\n"), func(c context.Context) ([]core.ForgeStatus, error) {
		return sb.CheckForges(c), nil
	})
	busy := q.Loading || q.Fetching
	problem := slices.ContainsFunc(q.Data, func(f core.ForgeStatus) bool { return !f.Ready() })
	a.recheckOnFocus(ctx, !busy && problem, q.Refetch)
	if q.Loading {
		return col.Child(setupCard(ctx, ui.Div().Padding(space4).Child(checking(ctx, "Checking where your repositories push…"))))
	}
	var rows []ui.Element
	var remote []string
	for _, f := range q.Data {
		remote = append(remote, f.Repos...)
		name := forgeName(f.Kind)
		var status ui.Element
		switch {
		case f.Ready():
			line := f.CLI + " is signed in"
			if f.Account != "" {
				line += " as " + f.Account
			}
			status = statusLine(ctx, icons.CircleCheck, t.Success, line)
		case f.Path == "":
			var cmd, then ui.Element
			if f.Install != "" {
				cmd = commandBox(ctx, f.Install)
				then = caption(ctx, "Then sign in with "+f.Login+". Or see "+f.InstallURL+".")
			} else {
				then = caption(ctx, "Install it from "+f.InstallURL+", then sign in with "+f.Login+".")
			}
			status = statusLine(ctx, icons.CircleAlert, t.Warning, f.CLI+" isn't installed",
				muted(ctx, "Solo needs it to open "+map[forge.Kind]string{forge.GitHub: "pull requests", forge.GitLab: "merge requests"}[f.Kind]+" on "+name+"."), cmd, then)
		case f.Err != "":
			status = statusLine(ctx, icons.CircleAlert, t.Danger, "Couldn't check "+f.CLI, muted(ctx, f.Err))
		default:
			status = statusLine(ctx, icons.CircleAlert, t.Warning, f.CLI+" isn't signed in",
				muted(ctx, "Run this in a terminal and follow its steps."), commandBox(ctx, f.Login))
		}
		rows = append(rows, ui.Div().Column().Gap(space3).Padding(space4).Key("forge-"+string(f.Kind)).
			Child(ui.Div().Row().Align(ui.Center).Gap(space2).
				Child(ui.Icon(icons.GitPullRequest).Size(15).Color(t.TextMuted)).
				Child(subhead(name)).
				Child(caption(ctx, count(len(f.Repos), "repository", "repositories")+": "+nameList(f.Repos, 4)).Truncate().MinWidth(ui.Px(0)))).
			Child(status))
	}
	local := slices.DeleteFunc(slices.Clone(repos), func(r string) bool { return slices.Contains(remote, r) })
	if len(local) > 0 {
		what := count(len(local), "repository has", "repositories have")
		rows = append(rows, ui.Div().Padding(space4).Key("forge-local").Child(
			statusLine(ctx, icons.GitMerge, t.TextMuted, "Merge locally",
				muted(ctx, what+" no GitHub or GitLab remote: "+nameList(local, 4)+". Their work merges on this machine, with nothing to set up."))))
	}
	col = col.Child(setupCard(ctx, rows...))
	if problem {
		col = col.Child(ui.Div().Row().Align(ui.Center).Gap(space3).
			Child(ui.Button("Check again").Icon(icons.RefreshCw).Secondary().Small().Key("forges-again").Loading(busy).OnClick(q.Refetch)).
			Child(caption(ctx, "Solo checks again when you come back to this window.")))
	}
	return col
}
