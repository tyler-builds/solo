// Package gui is Solo's desktop UI, built with jamb: a sidebar of repositories, the orchestrator chat in
// the middle, and the threads in a rail on the right. A thread opens in a drawer.
package gui

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
)

const (
	railWidth = 304
	itemLimit = 400 // transcript items loaded per thread
)

// App is the window's state.
type App struct {
	b Backend

	threads *ui.Signal[[]core.ThreadView]
	repos   *ui.Signal[[]string]
	rate    *ui.Signal[agent.Event]
	pending *ui.Signal[map[string][]core.PendingQuestion] // worker questions, by thread
	navSel  *ui.Signal[string]                            // sidebar: "orchestrator" or "repo:<path>"
	openID  *ui.Signal[string]                            // the thread in the side panel
	openTab *ui.Signal[string]
	cfg     *ui.Signal[core.Settings]
	// The thread panel's share of the center, which the user can drag, and its open/close animation.
	panelSize *ui.Signal[ui.Length]
	panelAnim *ui.Signal[float32] // 0 closed … 1 open
	panelGoal float32             // the size, in percent, the panel opens to
	animStop  func()
	settings  *ui.Signal[bool]
	confirm   *ui.Signal[*confirmation]
	palette   *ui.Signal[bool]
	built     map[string]bool // transcript rows that are built, by scope/key (UI thread only)
	builtVer  *ui.Signal[int]

	items map[string]*ui.Signal[[]core.Item] // UI thread only

	// Changes arrive on core goroutines; refresh collects them and applies them on the UI thread.
	mu        sync.Mutex
	post      func(func())
	toast     func(variant ui.ToastVariant, title, msg string)
	dirtyAll  bool
	dirty     map[string]bool
	watched   map[string]bool
	scheduled bool
	unsub     func()
}

// confirmation is a question to confirm before a destructive action.
type confirmation struct {
	title, body, action string
	run                 func()
}

// New creates the UI over a backend.
func New(b Backend) *App {
	a := &App{
		b:         b,
		threads:   ui.NewSignal(b.Threads()),
		repos:     ui.NewSignal(b.RepoPaths()),
		rate:      ui.NewSignal(b.RateLimit()),
		pending:   ui.NewSignal(loadPending(b, b.Threads())),
		navSel:    ui.NewSignal("orchestrator"),
		openID:    ui.NewSignal(""),
		openTab:   ui.NewSignal("conversation"),
		cfg:       ui.NewSignal(b.Settings()),
		panelSize: ui.NewSignal(ui.Percent(50)),
		panelAnim: ui.NewSignal[float32](0),
		panelGoal: 50,
		settings:  ui.NewSignal(false),
		confirm:   ui.NewSignal[*confirmation](nil),
		palette:   ui.NewSignal(false),
		builtVer:  ui.NewSignal(0),
		items:     map[string]*ui.Signal[[]core.Item]{},
		dirty:     map[string]bool{},
		watched:   map[string]bool{},
	}
	a.unsub = b.Subscribe(a.onChange)
	return a
}

// Close stops listening to the backend.
func (a *App) Close() { a.unsub() }

func loadPending(b Backend, threads []core.ThreadView) map[string][]core.PendingQuestion {
	out := map[string][]core.PendingQuestion{}
	for _, t := range threads {
		if t.PendingAsk {
			if qs := b.PendingQuestions(t.ID); len(qs) > 0 {
				out[t.ID] = qs
			}
		}
	}
	return out
}

func (a *App) onChange(ch core.Change) {
	a.mu.Lock()
	switch ch.Kind {
	case core.ChangeItems:
		a.dirty[ch.ThreadID] = true
	default:
		a.dirtyAll = true
	}
	if a.scheduled || a.post == nil {
		a.mu.Unlock()
		return
	}
	a.scheduled = true
	a.mu.Unlock()
	// Coalesce bursts (streaming text arrives many times a second) into one update per frame or two.
	time.AfterFunc(33*time.Millisecond, a.refresh)
}

// refresh reads what changed off the UI thread, then hands it to the UI thread.
func (a *App) refresh() {
	a.mu.Lock()
	all, dirty := a.dirtyAll, a.dirty
	a.dirtyAll, a.dirty, a.scheduled = false, map[string]bool{}, false
	var load []string
	for id := range dirty {
		if a.watched[id] {
			load = append(load, id)
		}
	}
	post := a.post
	a.mu.Unlock()

	var threads []core.ThreadView
	var repos []string
	var rate agent.Event
	var pending map[string][]core.PendingQuestion
	var cfg core.Settings
	if all || len(dirty) > 0 {
		cfg = a.b.Settings()
		threads = a.b.Threads()
		repos = a.b.RepoPaths()
		rate = a.b.RateLimit()
		pending = loadPending(a.b, threads)
	}
	loaded := map[string][]core.Item{}
	for _, id := range load {
		if items, err := a.b.Items(context.Background(), id, itemLimit); err == nil {
			loaded[id] = items
		}
	}
	post(func() {
		if threads != nil {
			a.threads.Set(threads)
			if !slices.Equal(repos, a.repos.Peek()) {
				a.repos.Set(repos)
			}
			a.rate.Set(rate)
			a.pending.Set(pending)
			a.cfg.Set(cfg)
		}
		for id, items := range loaded {
			if s := a.items[id]; s != nil {
				s.Set(items)
			}
		}
	})
}

// itemsOf returns the signal holding a thread's transcript, loading it the first time. UI thread only.
func (a *App) itemsOf(id string) *ui.Signal[[]core.Item] {
	if s := a.items[id]; s != nil {
		return s
	}
	items, _ := a.b.Items(context.Background(), id, itemLimit)
	s := ui.NewSignal(items)
	a.items[id] = s
	a.mu.Lock()
	a.watched[id] = true
	a.mu.Unlock()
	return s
}

// thread returns the current snapshot of a thread from the threads signal (tracked).
func (a *App) thread(id string) (core.ThreadView, bool) {
	for _, t := range a.threads.Get() {
		if t.ID == id {
			return t, true
		}
	}
	return core.ThreadView{}, false
}

// run does slow work off the UI thread, and reports a failure in a toast.
func (a *App) run(what string, f func(ctx context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := f(ctx); err != nil {
			a.notify(ui.ToastError, what+" failed", err.Error())
		}
	}()
}

func (a *App) notify(v ui.ToastVariant, title, msg string) {
	a.mu.Lock()
	post, toast := a.post, a.toast
	a.mu.Unlock()
	if post != nil && toast != nil {
		post(func() { toast(v, title, msg) })
	}
}

// Root renders the window.
func (a *App) Root(ctx *ui.Context) ui.Element {
	a.mu.Lock()
	if a.post == nil {
		a.post = ctx.Post
		a.toast = func(v ui.ToastVariant, title, msg string) { ctx.Toast(v, msg).Title(title) }
		// Pick up what changed before the window existed.
		a.scheduled = true
		go a.refresh()
	}
	a.mu.Unlock()
	t := ctx.Theme()
	root := ui.Div().Column().Size(ui.Full, ui.Full).Background(t.Panel).TextColor(t.Text).
		OnAction(nextAction, a.nextAttention).
		OnAction(paletteAction, func() { a.palette.Set(true) })
	for i, act := range jumpActions {
		n := i
		root = root.OnAction(act, func() { a.jumpTo(n) })
	}
	return root.
		Child(a.titleBar(ctx)).
		Child(ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).ClipContent().
			Child(ui.Component(a.center))).
		Child(ui.Component(a.settingsDialog)).
		Child(ui.Component(a.confirmDialog)).
		Child(ui.CommandPalette(a.palette).ItemsOnly().Placeholder("Go to a thread").Items(a.paletteItems)).
		Child(ui.Toaster().Key("toaster"))
}

// titleBar is the window's own title bar: the Solo mark, a "go to a thread" box in the middle, and the
// window's buttons.
func (a *App) titleBar(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	find := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space3).Height(ui.Px(26)).Width(ui.Px(320)).
		Radius(radiusS).Border(1, t.Border).Background(t.Background).Cursor(ui.CursorPointer).
		Hover(ui.Style{BorderColor: t.BorderStrong}).
		Role(access.RoleButton).Label("Go to a thread").TabIndex(0).
		OnClick(func() { a.palette.Set(true) }).
		Child(ui.Icon(icons.Search).Size(13).Color(t.TextSubtle)).
		Child(ui.Text("Go to a thread").FontSize(12).TextColor(t.TextSubtle).Grow(1)).
		Child(ui.Kbd("Ctrl+K"))
	var trailing []ui.Element
	// Claude's usage shows only when it matters.
	if rate := a.rate.Get(); rate.Kind == agent.EventRateLimit && rate.Status != "allowed" {
		status, c := "Near the usage limit", t.Warning
		if rate.Status == "rejected" {
			status, c = "Usage limit reached", t.Danger
		}
		trailing = append(trailing, ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).Height(ui.Px(24)).Radius(12).
			Background(c.Alpha(0.12)).
			Child(dot(c, 6)).
			Child(ui.Text(status).FontSize(12).Medium().TextColor(c)))
	}
	if a.cfg.Get().ShowCosts {
		var cost float64
		for _, th := range a.threads.Get() {
			cost += th.CostUSD
		}
		trailing = append(trailing, ui.Div().Tooltip("Spent across threads").Child(subtle(ctx, fmt.Sprintf("$%.2f", cost))))
	}
	trailing = append(trailing, ui.IconButton(icons.Settings).Ghost().Small().Label("Settings").Tooltip("Settings").
		Key("open-settings").OnClick(func() { a.settings.Set(true) }))
	return ui.TitleBar().Height(ui.Px(40)).
		Child(ui.Div().Row().Align(ui.Center).Gap(space2).PaddingLeft(space4).
			Child(brandMark(18)).
			Child(ui.Text("Solo").FontSize(13).Semibold())).
		Center(find).
		Trailing(trailing...)
}

// inset is a content panel: rounded, a step off the window background, with a hairline border.
func inset(ctx *ui.Context, content ui.Element) *ui.DivElement {
	t := ctx.Theme()
	return ui.Div().Column().Grow(1).Height(ui.Full).MinWidth(ui.Px(0)).
		Radius(radiusXL).Border(1, t.Border).Background(t.Background).ClipContent().
		Child(content)
}

// center holds the thread rail on the left, the orchestrator, and, when a thread is open, its panel on
// the right: it slides in from the right edge to half the space (the user can drag the divider) and
// slides back out when closed.
func (a *App) center(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	orch := ui.Div().Column().Size(ui.Full, ui.Full).PaddingBottom(space2).
		Child(inset(ctx, ui.Component(a.orchestratorView)))
	anim := a.panelAnim.Get()
	var main ui.Element = orch
	if anim > 0 && a.openID.Get() != "" {
		panel := ui.Div().Column().Size(ui.Full, ui.Full).PaddingBottom(space2).PaddingRight(space2).
			OnKeyDown(func(e *ui.KeyEvent) {
				if e.Key == input.KeyEscape {
					e.StopPropagation()
					a.closeThread()
				}
			}).
			Child(inset(ctx, ui.Component(a.threadPanel)))
		pane := ui.Pane(panel).Key("thread").Label("Thread")
		if anim >= 1 {
			pane = pane.SizeSignal(a.panelSize).Min(ui.Px(400)).Max(ui.Percent(75))
		} else {
			// While it slides, the panel follows the animation, below its usual minimum.
			pane = pane.Size(ui.Percent(a.panelGoal * anim)).Min(ui.Px(0)).Max(ui.Percent(75))
		}
		thin := ui.Style{Width: ui.Px(space2), Background: t.Panel}
		main = ui.Split(
			ui.Pane(orch).Min(ui.Px(360)).Key("orch").Label("Orchestrator"),
			pane,
		).Grip(map[bool]ui.SplitGrip{true: ui.SplitGripOnHover, false: ui.SplitGripHidden}[anim >= 1]).
			Styles(ui.SplitStyle{
				Divider:       thin,
				DividerHover:  ui.Style{Background: t.Panel},
				DividerActive: ui.Style{Background: t.Panel},
				Grip:          ui.Style{Background: t.BorderStrong},
			}).OnChange(func() {
			if l := a.panelSize.Peek(); l.Unit == ui.Percent(0).Unit {
				a.panelGoal = l.Value
			}
		})
	} else {
		main = ui.Div().Column().Size(ui.Full, ui.Full).PaddingRight(space2).Child(orch)
	}
	return ui.Div().Row().Size(ui.Full, ui.Full).
		Child(ui.Div().Width(ui.Px(railWidth)).Height(ui.Full).Shrink(0).Child(ui.Component(a.rail))).
		Child(ui.Div().Column().Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(0)).Height(ui.Full).Child(main))
}

// openThread shows a thread beside the orchestrator, on a tab ("conversation" or "changes").
func (a *App) openThread(id, tab string) {
	a.openTab.Set(tab)
	a.openID.Set(id)
	a.animatePanel(1, nil)
}

// closeThread hides the thread panel.
func (a *App) closeThread() {
	a.animatePanel(0, func() { a.openID.Set("") })
}

// animatePanel eases the panel open (to 1) or closed (to 0), then runs done. UI thread only.
func (a *App) animatePanel(to float32, done func()) {
	if a.animStop != nil {
		a.animStop()
		a.animStop = nil
	}
	from := a.panelAnim.Peek()
	if from == to || a.post == nil {
		a.panelAnim.Set(to)
		if to == 1 {
			a.panelSize.Set(ui.Percent(a.panelGoal))
		}
		if done != nil {
			done()
		}
		return
	}
	const steps = 10
	stopped := false
	a.animStop = func() { stopped = true }
	var step func(i int)
	step = func(i int) {
		if stopped {
			return
		}
		p := float32(i) / steps
		e := 1 - (1-p)*(1-p)*(1-p) // ease out
		a.panelAnim.Set(from + (to-from)*e)
		if i < steps {
			time.AfterFunc(16*time.Millisecond, func() { a.post(func() { step(i + 1) }) })
			return
		}
		a.animStop = nil
		if to == 1 {
			a.panelSize.Set(ui.Percent(a.panelGoal))
		}
		if done != nil {
			done()
		}
	}
	step(1)
}

// repoFilter is the repository the rail shows, or "" for all (set from the rail's repository menu).
func (a *App) repoFilter() string {
	sel := a.navSel.Get()
	if len(sel) > 5 && sel[:5] == "repo:" {
		return sel[5:]
	}
	return ""
}

var (
	nextAction    = ui.NewAction("solo.next", "Go to the next thread that needs you")
	paletteAction = ui.NewAction("solo.palette", "Go to a thread").Hide()
)

// Bindings are the app's keyboard shortcuts.
func Bindings() []ui.Binding {
	b := []ui.Binding{ui.Bind("ctrl+j", nextAction), ui.Bind("ctrl+k", paletteAction)}
	for i, act := range jumpActions {
		b = append(b, ui.Bind(fmt.Sprintf("ctrl+%d", i+1), act))
	}
	return b
}

// nextAttention opens the next thread that needs the user: questions first, then failures, then work
// ready to merge. Pressed again, it moves on to the one after.
func (a *App) nextAttention() {
	rank := map[string]int{core.StatusWaiting: 0, core.StatusFailed: 1, core.StatusIdle: 2}
	var list []core.ThreadView
	for _, th := range a.threads.Peek() {
		if _, ok := rank[th.Status]; ok && th.IsWorker {
			list = append(list, th)
		}
	}
	if len(list) == 0 {
		return
	}
	slices.SortStableFunc(list, func(x, y core.ThreadView) int { return rank[x.Status] - rank[y.Status] })
	next := list[0]
	for i, th := range list {
		if th.ID == a.openID.Peek() {
			next = list[(i+1)%len(list)]
		}
	}
	tab := "conversation"
	if next.Status == core.StatusIdle {
		tab = "changes"
	}
	a.openThread(next.ID, tab)
}

// paletteItems lists the threads for the Ctrl+K palette.
func (a *App) paletteItems(query string) []ui.PaletteItem {
	var out []ui.PaletteItem
	for _, th := range a.threads.Peek() {
		if !th.IsWorker {
			continue
		}
		id := th.ID
		out = append(out, ui.PaletteItem{
			Key:      id,
			Label:    th.Title,
			Detail:   filepath.Base(th.RepoPath) + " · " + statusLabel(th),
			Icon:     icons.MessageSquare,
			Keywords: []string{filepath.Base(th.RepoPath), th.Status},
			Run:      func() { a.openThread(id, "conversation") },
		})
	}
	out = append(out, ui.PaletteItem{Key: "settings", Label: "Settings", Icon: icons.Settings, Run: func() { a.settings.Set(true) }})
	return out
}

var jumpActions = func() []*ui.Action {
	var out []*ui.Action
	for i := 1; i <= 9; i++ {
		out = append(out, ui.NewAction(fmt.Sprintf("solo.jump%d", i), fmt.Sprintf("Open thread %d", i)).Hide())
	}
	return out
}()

// openThreads lists the open worker threads in the rail's order: by group, newest first in each.
func (a *App) openThreads(track bool) []core.ThreadView {
	threads := a.threads.Peek()
	if track {
		threads = a.threads.Get()
	}
	filter := a.repoFilter()
	var out []core.ThreadView
	for _, g := range railGroups {
		var group []core.ThreadView
		for _, th := range threads {
			if th.IsWorker && slices.Contains(g.statuses, th.Status) && (filter == "" || th.RepoPath == filter) {
				group = append(group, th)
			}
		}
		slices.SortStableFunc(group, func(x, y core.ThreadView) int { return y.UpdatedAt.Compare(x.UpdatedAt) })
		out = append(out, group...)
	}
	return out
}

// jumpTo opens the nth open thread (Ctrl+1 … Ctrl+9).
func (a *App) jumpTo(n int) {
	if list := a.openThreads(false); n < len(list) {
		a.openThread(list[n].ID, "conversation")
	}
}
