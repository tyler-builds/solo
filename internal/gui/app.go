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

	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
)

const (
	sidebarWidth = 224
	railWidth    = 304
	itemLimit    = 400 // transcript items loaded per thread
)

// App is the window's state.
type App struct {
	b Backend

	threads  *ui.Signal[[]core.ThreadView]
	repos    *ui.Signal[[]string]
	rate     *ui.Signal[agent.Event]
	pending  *ui.Signal[map[string][]core.PendingQuestion] // worker questions, by thread
	navSel   *ui.Signal[string]                            // sidebar: "orchestrator" or "repo:<path>"
	openID   *ui.Signal[string]                            // the thread in the drawer
	openTab  *ui.Signal[string]
	settings *ui.Signal[bool]
	confirm  *ui.Signal[*confirmation]

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
		b:        b,
		threads:  ui.NewSignal(b.Threads()),
		repos:    ui.NewSignal(b.RepoPaths()),
		rate:     ui.NewSignal(b.RateLimit()),
		pending:  ui.NewSignal(loadPending(b, b.Threads())),
		navSel:   ui.NewSignal("orchestrator"),
		openID:   ui.NewSignal(""),
		openTab:  ui.NewSignal("conversation"),
		settings: ui.NewSignal(false),
		confirm:  ui.NewSignal[*confirmation](nil),
		items:    map[string]*ui.Signal[[]core.Item]{},
		dirty:    map[string]bool{},
		watched:  map[string]bool{},
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
	if all || len(dirty) > 0 {
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
	return ui.Div().Row().Size(ui.Full, ui.Full).Background(t.Panel).TextColor(t.Text).
		Child(a.sidebar(ctx)).
		Child(ui.Div().Column().Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(0)).
			MarginTop(space2).MarginBottom(space2).
			Radius(radiusXL).Border(1, t.Border).Background(t.Background).ClipContent().
			Child(ui.Component(a.orchestratorView))).
		Child(ui.Component(a.rail)).
		Child(ui.Component(a.threadDrawer)).
		Child(ui.Component(a.settingsDialog)).
		Child(ui.Component(a.confirmDialog)).
		Child(ui.Toaster().Key("toaster"))
}

// sidebar lists the repositories; choosing one filters the thread rail.
func (a *App) sidebar(ctx *ui.Context) ui.Element {
	threads := a.threads.Get()
	open := map[string]int{}
	for _, th := range threads {
		if th.IsWorker && !core.Finished(th.Status) {
			open[th.RepoPath]++
		}
	}
	active := 0
	for _, th := range threads {
		if th.IsWorker && core.Active(th.Status) {
			active++
		}
	}
	items := []ui.SidebarItem{
		{Key: "orchestrator", Label: "All threads", Icon: icons.Layers, Badge: active},
		ui.SidebarHeading("Repositories"),
	}
	repos := a.repos.Get()
	for _, r := range repos {
		items = append(items, ui.SidebarItem{Key: "repo:" + r, Label: filepath.Base(r), Icon: icons.FolderGit2, Badge: open[r]})
	}
	sb := ui.Sidebar(a.navSel, items...).Label("Repositories").Width(ui.Px(sidebarWidth)).NoIndicator().
		Header(ui.Div().Row().Align(ui.Center).Gap(space2 + 2).PaddingX(space2).PaddingTop(space2).PaddingBottom(space3).
			Child(brandMark(24)).
			Child(ui.Text("Solo").FontSize(15).Semibold())).
		Footer(a.sidebarFooter(ctx))
	if len(repos) == 0 {
		sb = sb.Footer(ui.Div().Column().Gap(space3).
			Child(ui.Div().Column().Gap(space2).PaddingX(space2 + 2).
				Child(caption(ctx, "No repositories yet. Add the folders that hold your projects.")).
				Child(ui.Button("Add folders").Icon(icons.Plus).Secondary().Small().OnClick(func() { a.settings.Set(true) }))).
			Child(a.sidebarFooter(ctx)))
	}
	return sb
}

func (a *App) sidebarFooter(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	rate := a.rate.Get()
	status, c := "Claude ready", t.Success.Alpha(0.7)
	if rate.Kind == agent.EventRateLimit {
		switch rate.Status {
		case "allowed_warning":
			status, c = "Near the usage limit", t.Warning
		case "rejected":
			status, c = "Usage limit reached", t.Danger
		}
	}
	var cost float64
	for _, th := range a.threads.Get() {
		cost += th.CostUSD
	}
	return ui.Div().Column().Gap(space2).
		Child(ui.Div().Column().Gap(2).PaddingX(space2 + 2).PaddingY(space1).
			Child(ui.Div().Row().Align(ui.Center).Gap(6).
				Child(dot(c, 6)).
				Child(ui.Text(status).FontSize(12).Medium())).
			Child(caption(ctx, fmt.Sprintf("$%.2f spent across threads", cost)))).
		Child(ui.Button("Settings").Icon(icons.Settings).Ghost().Small().OnClick(func() { a.settings.Set(true) }).
			Key("open-settings"))
}

// repoFilter is the repository the rail shows, or "" for all.
func (a *App) repoFilter() string {
	sel := a.navSel.Get()
	if len(sel) > 5 && sel[:5] == "repo:" {
		return sel[5:]
	}
	return ""
}
