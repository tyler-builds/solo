package gui

// The first-run setup: six short steps in place of the main view (Welcome, Claude Code, Models,
// Repositories, Pull requests, Preferences) that check the machine and set what a new user wants set
// from the start, then land in Solo's chat. Every step can be skipped, and so can the whole setup, which
// leaves the defaults. Each step reads the machine through the backend's own checks (CheckClaude,
// api.Setup) and changes the same settings Settings does, through UpdateSettings, so what is set here
// shows there and running the setup again shows the current values. Where it stands is kept in
// Settings.Setup: a step to resume at, or done. A web page never shows it (its backend isn't an
// api.Setup): its machine is set up at the machine. The steps themselves are in onboarding_steps.go.

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
)

// setupSteps are the setup's steps, in order, by the name Settings.Setup keeps.
var setupSteps = []struct{ key, title string }{
	{"welcome", "Welcome"},
	{"claude", "Claude Code"},
	{"models", "Models"},
	{"repos", "Repositories"},
	{"prs", "Pull requests"},
	{"prefs", "Preferences"},
}

// setupIndex is a step's place in setupSteps, or -1.
func setupIndex(key string) int {
	return slices.IndexFunc(setupSteps, func(s struct{ key, title string }) bool { return s.key == key })
}

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

// setupSaver saves what the setup changes in the settings: off the UI thread, in the order the changes
// were made, with changes that come while one saves joined into the next save. Typed text waits until
// the typing stops (saveLater).
type setupSaver struct {
	mu      sync.Mutex
	queue   []func(*core.Settings)
	running bool

	later map[string]func(*core.Settings) // typed changes not saved yet, by field (UI thread only)
	timer *time.Timer
}

// saveSettings changes the settings as Settings does (UpdateSettings), showing the change at once.
func (a *App) saveSettings(f func(*core.Settings)) {
	next := a.cfg.Peek()
	f(&next)
	a.cfg.Set(next)
	sv := &a.setupSave
	sv.mu.Lock()
	sv.queue = append(sv.queue, f)
	start := !sv.running
	sv.running = true
	sv.mu.Unlock()
	if start {
		go a.drainSettings()
	}
}

// drainSettings saves the changes queued, until there are none.
func (a *App) drainSettings() {
	sv := &a.setupSave
	for {
		sv.mu.Lock()
		fs := sv.queue
		sv.queue = nil
		if len(fs) == 0 {
			sv.running = false
			sv.mu.Unlock()
			return
		}
		sv.mu.Unlock()
		err := a.b.UpdateSettings(func(s *core.Settings) {
			for _, f := range fs {
				f(s)
			}
		})
		if err != nil {
			a.notify(ui.ToastError, "Couldn't save the settings", err.Error())
		}
		a.onUI(func() {
			sv.mu.Lock()
			idle := len(sv.queue) == 0
			sv.mu.Unlock()
			if idle { // a later change shown already would be undone until its own save
				a.cfg.Set(a.b.Settings())
			}
		})
	}
}

// saveLater saves a typed change once the typing stops, or when the step changes. UI thread only.
func (a *App) saveLater(field string, f func(*core.Settings)) {
	sv := &a.setupSave
	if sv.later == nil {
		sv.later = map[string]func(*core.Settings){}
	}
	sv.later[field] = f
	if sv.timer != nil {
		sv.timer.Stop()
	}
	sv.timer = time.AfterFunc(600*time.Millisecond, func() { a.onUI(a.flushLater) })
}

// flushLater saves the typed changes that wait. UI thread only.
func (a *App) flushLater() {
	sv := &a.setupSave
	if sv.timer != nil {
		sv.timer.Stop()
		sv.timer = nil
	}
	for _, f := range sv.later {
		a.saveSettings(f)
	}
	sv.later = nil
}

// goSetup shows a step of the setup and puts the focus on its main button, so Enter carries on.
func (a *App) goSetup(step string) {
	a.flushLater()
	a.setup.Set(step)
	a.setupFocus.Focus()
	a.saveSettings(func(s *core.Settings) { s.Setup = step })
}

// finishSetup ends the setup, finished or skipped, and lands in Solo's chat with the caret in its
// message box. What wasn't chosen keeps its default.
func (a *App) finishSetup() {
	a.flushLater()
	a.setup.Set("")
	a.saveSettings(func(s *core.Settings) { s.Setup = core.SetupDone })
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
		settingRow(ctx, "First-run setup", "Checks Claude Code and your folders, and goes through models, pull requests and preferences again.",
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
			Tooltip("Go straight to Solo with the defaults. Settings has all of this, and can run the setup again.").
			OnClick(a.finishSetup))
}

// setupLayout is how the setup lays out in the window: the content's width, the space at its sides,
// and whether a step can put its two kinds of content side by side.
type setupLayout struct {
	w, gutter float32
	two       bool
	compact   bool
}

// setupLayoutOf lays the setup out for the window: the content grows with it up to setupMaxWidth.
func setupLayoutOf(ctx *ui.Context) setupLayout {
	winW, _ := ctx.WindowSize()
	l := setupLayout{compact: isCompact(ctx)}
	panelW := winW - 2*space2 - 2 // the content panel, inset in the window
	switch {
	case l.compact:
		l.gutter, panelW = space4, winW
	case winW < 1200:
		l.gutter = space8
	default:
		l.gutter = space12
	}
	l.w = max(min(panelW-2*l.gutter, setupMaxWidth), 200)
	l.two = l.w >= setupSplit
	return l
}

// setupView is the setup: where it stands at the top, the step in the middle of the space under it, and
// the buttons that move between steps at the foot, all as wide as the step's content.
func (a *App) setupView(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	step := a.setup.Get()
	i := max(setupIndex(step), 0)
	l := setupLayoutOf(ctx)
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
	case "models":
		body = ui.Component(a.setupModels)
	case "repos":
		body = ui.Component(a.setupRepos)
	case "prs":
		body = ui.Component(a.setupPRs)
	case "prefs":
		body, primary, icon = ui.Component(a.setupPrefs), "Start using Solo", nil
	}
	band := func() *ui.DivElement { // a band across the panel, with the content's width in its middle
		return ui.Div().Column().Align(ui.Center).Width(ui.Full).PaddingX(l.gutter).Shrink(0)
	}
	content := func() *ui.DivElement { return ui.Div().Column().Width(ui.Px(l.w)) }

	head := band().PaddingTop(space8).PaddingBottom(space2).Child(content().Child(a.setupProgress(ctx, i)))
	if l.compact {
		head = head.PaddingTop(space4)
	}
	// The step sits in the middle of the space between the steps and the buttons, a little above it,
	// and scrolls when it is taller.
	inner := band().MinHeight(ui.Full).PaddingTop(space5).PaddingBottom(space8).
		Child(content().Key("setup-step-" + setupSteps[i].key).Child(body))
	if !l.compact {
		inner = inner.Justify(ui.Center)
	}
	scroll := ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).ScrollY().Key("setup-scroll").Child(inner)

	main := ui.Button(primary).Primary().Key("setup-next").FocusHandle(a.setupFocus).OnClick(next)
	if !l.compact {
		main = main.Large()
	}
	if icon != nil {
		main = main.IconEnd(icon)
	}
	footRow := content().Row().Align(ui.Center).Gap(space3)
	if i > 0 {
		prev := setupSteps[i-1].key
		back := ui.Button("Back").Icon(icons.ArrowLeft).Ghost().Key("setup-back").OnClick(func() { a.goSetup(prev) })
		if !l.compact {
			back = back.Large()
		}
		footRow = footRow.Child(back)
	}
	footRow = footRow.Child(ui.Div().Grow(1))
	if !l.compact && !a.Touch {
		footRow = footRow.Child(ui.Kbd("Enter"))
	}
	footRow = footRow.Child(main)
	foot := band().PaddingY(space4).BorderEdges(ui.Edges{Top: 1}, t.Border).Child(footRow)

	panel := ui.Div().Column().Size(ui.Full, ui.Full).
		OnKeyDown(func(e *ui.KeyEvent) {
			// Enter carries on from anywhere a field doesn't take it.
			if e.Key == input.KeyEnter && e.Mods == 0 {
				e.StopPropagation()
				next()
			}
		}).
		Child(head).Child(scroll).Child(foot)
	if l.compact {
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
		label := ui.Text(s.title).FontSize(13).TextColor(t.TextSubtle).NoWrap().Truncate()
		switch {
		case i == cur:
			label = ui.Text(s.title).FontSize(13).Semibold().TextColor(t.Text).NoWrap().Truncate()
		case i < cur:
			label = ui.Text(s.title).FontSize(13).TextColor(t.TextMuted).NoWrap().Truncate()
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
	return ui.Div().Column().Gap(space3).
		Child(displayTitle(ctx, heading)).
		Child(leadText(ctx, lead).MaxWidth(ui.Px(setupProseWidth)))
}

// setupSplit lays out a step's two kinds of content side by side where there is room, the second a
// little wider, and one over the other where there isn't.
func setupSplitView(l setupLayout, left, right ui.Element) ui.Element {
	if !l.two || right == nil {
		return ui.Div().Column().Gap(space8).Width(ui.Full).Child(left).Child(right)
	}
	return ui.Div().Row().Align(ui.Start).Gap(space12).Width(ui.Full).
		Child(ui.Div().Column().Grow(5).Basis(ui.Px(0)).MinWidth(ui.Px(0)).Child(left)).
		Child(ui.Div().Column().Grow(6).Basis(ui.Px(0)).MinWidth(ui.Px(0)).Child(right))
}

// setupPairView lays out two panels of one kind side by side, as wide as each other, where there is
// room, and one over the other where there isn't.
func setupPairView(l setupLayout, first, second ui.Element) ui.Element {
	if !l.two || second == nil {
		return ui.Div().Column().Gap(space6).Width(ui.Full).Child(first).Child(second)
	}
	return ui.Div().Row().Align(ui.Stretch).Gap(space6).Width(ui.Full).
		Child(ui.Div().Column().Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(0)).Child(first)).
		Child(ui.Div().Column().Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(0)).Child(second))
}

// setupPanel is one of the setup's large panels.
func setupPanel(ctx *ui.Context, children ...ui.Element) *ui.DivElement {
	t := ctx.Theme()
	pad := float32(space6)
	if isCompact(ctx) {
		pad = space4
	}
	p := ui.Div().Column().Gap(space4).Padding(pad).Radius(radius2X).Border(1, t.Border).Background(t.Surface)
	for _, c := range children {
		if c != nil {
			p = p.Child(c)
		}
	}
	return p
}

// setupCard is a rounded card of rows, divided by rules, as Settings draws them.
func setupCard(ctx *ui.Context, rows ...ui.Element) *ui.DivElement {
	t := ctx.Theme()
	card := ui.Div().Column().Radius(radiusL+2).Border(1, t.Border).Background(t.Surface)
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
	col := ui.Div().Column().Gap(space2).Grow(1).MinWidth(ui.Px(0)).
		Child(ui.Text(line).FontSize(16).Semibold().TextColor(t.Text))
	for _, u := range under {
		if u != nil {
			col = col.Child(u)
		}
	}
	return ui.Div().Row().Align(ui.Start).Gap(space3).
		Child(ui.Div().PaddingTop(1).Child(ui.Icon(icon).Size(20).Color(c))).
		Child(col)
}

// commandBox is a command to run in a terminal, with a button that copies it.
func commandBox(ctx *ui.Context, cmd string) ui.Element {
	t := ctx.Theme()
	return ui.Div().Row().Align(ui.Center).Gap(space2).PaddingLeft(space3).PaddingRight(space1).Height(ui.Px(40)).
		MarginTop(space1).Radius(radiusM).Border(1, t.Border).Background(t.Background).
		Child(ui.Text(cmd).Mono().FontSize(13).TextColor(t.Text).Selectable().Truncate().Grow(1).MinWidth(ui.Px(0))).
		Child(ui.CopyButton(cmd).Small().Tooltip("Copy the command").Key("copy-" + cmd))
}

// checking is a check under way.
func checking(ctx *ui.Context, what string) ui.Element {
	return ui.Div().Row().Align(ui.Center).Gap(space3).
		Child(ui.ProgressRing().Size(18)).
		Child(bodyText(ctx, what))
}

// setupRow is a choice in one of the setup's panels: its name and what it does, with its control
// beside it (under it, where there is no room).
func setupRow(ctx *ui.Context, name, help string, control ui.Element) ui.Element {
	t := ctx.Theme()
	label := ui.Div().Column().Gap(space1).Grow(1).Basis(ui.Px(200)).MinWidth(ui.Px(0)).
		Child(ui.Text(name).FontSize(15).Medium().TextColor(t.Text)).
		Child(bodyText(ctx, help))
	return ui.Div().Row().Wrap().Align(ui.Center).Gap(space4).
		Child(label).
		Child(ui.Div().Row().Align(ui.Center).MaxWidth(ui.Full).Child(control))
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
	l := setupLayoutOf(ctx)
	mark := float32(56)
	if l.compact {
		mark = 44
	}
	return ui.Div().Column().Gap(space10).
		Child(ui.Div().Column().Gap(space5).
			Child(brandMark(mark)).
			Child(setupHead(ctx, "Welcome to Solo",
				"You tell Solo what you want done, and it runs each task as a thread on its own branch until the work is ready for you to land."))).
		Child(a.loopPicture(ctx, l)).
		Child(bodyText(ctx, "Next: Claude Code, models, your repositories, pull requests and a few preferences. Each takes a moment, and you can skip any of them.").
			MaxWidth(ui.Px(setupProseWidth)))
}

// loopPicture draws the loop with the app's own pieces: a message, threads at work, a branch, and a
// merge card's button. Four across where they fit, two by two where they don't, and a list in a
// compact window. The numbers keep their order where there are no arrows.
func (a *App) loopPicture(ctx *ui.Context, l setupLayout) ui.Element {
	t := ctx.Theme()
	threadLine := func(title string) ui.Element {
		return ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(statusMark(ctx, core.StatusRunning)).
			Child(ui.Text(title).FontSize(13).TextColor(t.Text).NoWrap())
	}
	tiles := []struct {
		label, detail string
		visual        ui.Element
	}{
		{"You ask", "In plain words, as you would a colleague.", ui.Div().PaddingX(space3).PaddingY(space2).Radius(radiusL).Background(t.SurfaceActive).
			Child(ui.Text("Fix the flaky login test").FontSize(13).TextColor(t.Text))},
		{"Solo starts threads", "One for each task, working at once.", ui.Div().Column().Gap(space2).
			Child(threadLine("Fix login test")).Child(threadLine("Add retry test"))},
		{"Each on its own branch", "In a copy of the repository.", ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space3).Height(ui.Px(30)).
			Radius(radiusS).Border(1, t.Border).Background(t.Background).
			Child(ui.Icon(icons.GitBranch).Size(14).Color(t.TextMuted)).
			Child(ui.Text("solo/login-fix").Mono().FontSize(12).TextColor(t.TextMuted).NoWrap())},
		{"You review and land", "Only you can merge the work.", ui.Div().Row().Align(ui.Center).Gap(space3).
			Child(ui.Icon(icons.GitMerge).Size(16).Color(t.Success)).
			Child(ui.Div().Row().Align(ui.Center).PaddingX(space4).Height(ui.Px(30)).Radius(radiusS).Background(t.Accent).
				Child(ui.Text("Merge").FontSize(13).Medium().TextColor(t.AccentText)))},
	}
	pic := ui.Div().Role(access.RoleImage).Width(ui.Full).
		Label("You ask. Solo starts threads, one for each task. Each works on its own branch. You review and land the work.")
	number := func(n int) ui.Element {
		return ui.Div().Center().Size(ui.Px(22), ui.Px(22)).Radius(11).Background(t.SurfaceActive).Shrink(0).
			Child(ui.Text(fmt.Sprint(n)).FontSize(12).Semibold().TextColor(t.TextMuted))
	}
	if l.compact {
		col := ui.Div().Column().Gap(space2).AccessHidden()
		for i, tl := range tiles {
			col = col.Child(ui.Div().Column().Gap(space3).Padding(space4).
				Radius(radiusL+2).Border(1, t.Border).Background(t.Surface).
				Child(ui.Div().Row().Align(ui.Center).Gap(space3).
					Child(number(i + 1)).
					Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
						Child(ui.Text(tl.label).FontSize(15).Semibold().TextColor(t.Text)).
						Child(caption(ctx, tl.detail)))).
				Child(ui.Div().Row().PaddingLeft(22 + space3).MinWidth(ui.Px(0)).ClipContent().Child(tl.visual)))
		}
		return pic.Child(col)
	}
	tile := func(i int) *ui.DivElement {
		tl := tiles[i]
		return ui.Div().Column().Gap(space4).Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(0)).
			Padding(space5).Radius(radius2X).Border(1, t.Border).Background(t.Surface).ClipContent().
			Child(number(i + 1)).
			Child(ui.Div().Column().Justify(ui.Center).Height(ui.Px(72)).Child(tl.visual)).
			Child(ui.Div().Column().Gap(space1).
				Child(ui.Text(tl.label).FontSize(15).Semibold().TextColor(t.Text)).
				Child(bodyText(ctx, tl.detail)))
	}
	if l.w >= 860 { // four across; with arrows between them where there is room for those too
		arrows := l.w >= 1000
		row := ui.Div().Row().Align(ui.Stretch).Gap(space2).AccessHidden()
		if !arrows {
			row = row.Gap(space3)
		}
		for i := range tiles {
			if i > 0 && arrows {
				row = row.Child(ui.Div().Column().Justify(ui.Center).Shrink(0).
					Child(ui.Icon(icons.ArrowRight).Size(16).Color(t.TextSubtle)))
			}
			row = row.Child(tile(i))
		}
		return pic.Child(row)
	}
	grid := ui.Div().Column().Gap(space4).AccessHidden()
	for i := 0; i < len(tiles); i += 2 {
		grid = grid.Child(ui.Div().Row().Align(ui.Stretch).Gap(space4).Child(tile(i)).Child(tile(i + 1)))
	}
	return pic.Child(grid)
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
	l := setupLayoutOf(ctx)
	path := strings.TrimSpace(a.cfg.Get().ClaudePath)
	q := a.claudeQuery(ctx, path)
	st := q.Data
	busy := q.Loading || q.Fetching
	a.recheckOnFocus(ctx, !busy && st.Err != "", q.Refetch)

	again := ui.Div().Row().PaddingTop(space2).
		Child(ui.Button("Check again").Icon(icons.RefreshCw).Secondary().Key("claude-again").OnClick(q.Refetch))
	where := caption(ctx, claudeWhere(st, "the path you set")).TruncateHead()
	var status ui.Element
	switch {
	case busy:
		status = checking(ctx, "Looking for Claude Code…")
	case st.Path == "":
		status = statusLine(ctx, icons.CircleAlert, t.Warning, "Claude Code isn't installed",
			bodyText(ctx, "Install it with this command in a terminal, then run claude once to sign in. Solo checks again when you come back to this window."),
			commandBox(ctx, claudeInstall()), again)
	case st.SignedOut:
		status = statusLine(ctx, icons.CircleAlert, t.Warning, "Claude Code isn't signed in",
			bodyText(ctx, "Run claude in a terminal and sign in with /login. Solo checks again when you come back to this window."),
			commandBox(ctx, "claude"), where, again)
	case st.Err != "":
		status = statusLine(ctx, icons.CircleAlert, t.Danger, "Solo can't use this Claude Code",
			bodyText(ctx, st.Err), where, again)
	default:
		var who ui.Element
		if st.Account != "" {
			who = bodyText(ctx, "Signed in as "+st.Account)
		}
		status = statusLine(ctx, icons.CircleCheck, t.Success, "Claude Code "+st.Version+" is ready", who, where)
	}
	var note ui.Element
	if !busy && st.Err != "" {
		note = bodyText(ctx, "You can carry on and fix this later: Settings › General shows the same check. Solo can't start threads until Claude Code works.")
	}
	left := ui.Div().Column().Gap(space8).
		Child(setupHead(ctx, "Claude Code",
			"Solo runs your own Claude Code, with its sign-in and plan. If it works in your terminal, there is nothing to do here."))
	statusPanel := setupPanel(ctx, ui.Div().Key("claude-status").MinHeight(ui.Px(48)).Child(status))
	if !l.two {
		return left.Child(statusPanel).Child(a.claudePathField(ctx, path)).Child(note)
	}
	left = left.Child(a.claudePathField(ctx, path)).Child(note)
	return setupSplitView(l, left, statusPanel)
}

// claudePathField sets the path of the claude to run, the setting Settings › General shows. It is
// folded away until asked for, unless a path is set.
func (a *App) claudePathField(ctx *ui.Context, path string) ui.Element {
	open := ui.Local(ctx, "path-open", path != "")
	text := ui.Local(ctx, "path", path)
	if !open.Get() {
		return ui.Div().Row().Child(ui.Button("Claude Code is somewhere else?").Ghost().Key("claude-path-open").
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
	row := ui.Div().Row().Wrap().Align(ui.Center).Gap(space2).
		Child(ui.Div().Grow(1).Basis(ui.Px(220)).MinWidth(ui.Px(0)).
			Child(ui.TextInput(text).Placeholder("Path to claude").Label("Path to claude").Width(ui.Full).
				Key("claude-path").AutoFocus().OnSubmit(save))).
		Child(ui.Button("Use this path").Secondary().Key("claude-path-use").
			Disabled(strings.TrimSpace(text.Get()) == "" || strings.TrimSpace(text.Get()) == path).
			OnClick(func() { save(text.Peek()) }))
	if path != "" {
		row = row.Child(ui.Button("Find it myself").Ghost().Key("claude-path-clear").
			Tooltip("Forget this path, and look on PATH and where the installer puts it").
			OnClick(func() { text.Set(""); save("") }))
	}
	return ui.Div().Column().Gap(space2).
		Child(bodyText(ctx, "The claude program Solo runs. Leave it empty to find it on PATH or where the installer puts it.")).
		Child(row)
}
