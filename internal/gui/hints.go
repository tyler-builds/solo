package gui

// After the setup, Solo teaches in context rather than up front: Solo's empty chat offers a few tasks
// written about the user's own repositories, and a one-time hint explains each thing that only makes
// sense once it happens (the first thread, the first pinned question, the first merge card), with
// pairing a phone suggested once work has landed. One hint shows at a time, above Solo's message box,
// and each shows once: it is recorded in Settings.Hints when it first shows, and stays this session
// until it is dismissed or what it is about is over. Hints are the machine's (a web page shows none);
// starter prompts show everywhere.

import (
	"path/filepath"
	"slices"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/actions"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// The hints, by the names Settings.Hints keeps.
const (
	hintThread   = "first-thread"
	hintQuestion = "first-question"
	hintMerge    = "first-merge"
	hintRemote   = "pair-device"
)

// allHints are every hint, in the order they take precedence when more than one applies.
var allHints = []string{hintQuestion, hintMerge, hintThread, hintRemote}

// hintState is what this session knows of the hints. UI thread only.
type hintState struct {
	live map[string]bool             // shown this session, so they stay once recorded as seen
	gone *ui.Signal[map[string]bool] // dismissed this session
}

// hint is a hint to show: what it says, and what its button does, if it has one.
type hint struct {
	name, text string
	icon       *icons.Icon
	action     string
	run        func()
}

// currentHint is the one hint that applies now, or nil.
func (a *App) currentHint() *hint {
	if a.setupBackend() == nil {
		return nil
	}
	seen := a.cfg.Get().Hints
	gone := a.hints.gone.Get()
	for _, name := range allHints {
		if gone[name] || (slices.Contains(seen, name) && !a.hints.live[name]) {
			continue
		}
		h := a.hintFor(name)
		if h == nil {
			continue
		}
		if a.hints.live == nil {
			a.hints.live = map[string]bool{}
		}
		if !a.hints.live[name] {
			a.hints.live[name] = true
			a.recordHint(name)
		}
		return h
	}
	return nil
}

// hintFor is the hint by that name when what it is about is on screen, or nil.
func (a *App) hintFor(name string) *hint {
	switch name {
	case hintQuestion:
		for id, qs := range a.pending.Get() {
			if id != core.OrchestratorID && len(qs) > 0 {
				return &hint{name: name, icon: icons.MessageCircleQuestionMark,
					text: "A thread has a question for you. It stays pinned above your message box until it is answered: answer it there, or ask Solo to."}
			}
		}
	case hintMerge:
		items := a.itemsOf(core.OrchestratorID).Get()
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].Kind != core.ItemProposal {
				continue
			}
			th, ok := a.thread(items[i].Ref)
			if !ok || !inRail(th) || core.Finished(th.Status) {
				continue
			}
			return &hint{name: name, icon: icons.GitMerge,
				text: "Work is ready to land. Look over its changes, then land it with the card's button. Only you can land work: Solo and its threads never do it themselves."}
		}
	case hintThread:
		for _, th := range a.threads.Get() {
			if !inRail(th) {
				continue
			}
			id := th.ID
			return &hint{name: name, icon: icons.GitBranch,
				text:   "Solo started a thread. It works in its own copy of " + repoLabel(th) + ", on its own branch, so your checkout doesn't change until you land it. Open it to follow along or talk to it directly.",
				action: "Open thread", run: func() { a.openThread(id, "conversation") }}
		}
	case hintRemote:
		if a.Remote == nil || len(a.remoteState.Get().Devices) > 0 {
			return nil
		}
		if slices.ContainsFunc(a.threads.Get(), func(th core.ThreadView) bool { return th.Status == core.StatusMerged }) {
			return &hint{name: name, icon: icons.MonitorSmartphone,
				text:   "You can follow Solo from your phone or another computer's browser, with this machine doing the work.",
				action: "Pair a device", run: func() { a.openSettings("remote") }}
		}
	}
	return nil
}

// recordHint records that a hint showed, so it doesn't show again in a later session.
func (a *App) recordHint(name string) {
	go func() {
		if err := a.b.UpdateSettings(func(s *core.Settings) {
			if !slices.Contains(s.Hints, name) {
				s.Hints = append(s.Hints, name)
			}
		}); err != nil {
			a.notify(ui.ToastError, "Couldn't save the settings", err.Error())
		}
	}()
}

// dismissHint puts a hint away.
func (a *App) dismissHint(name string) {
	next := map[string]bool{name: true}
	for k, v := range a.hints.gone.Peek() {
		next[k] = v
	}
	a.hints.gone.Set(next)
}

// hintBar is the hint that applies now, above Solo's message box; nothing when none does.
func (a *App) hintBar(ctx *ui.Context) ui.Element {
	h := a.currentHint()
	if h == nil {
		return nil
	}
	t := ctx.Theme()
	name := h.name
	text := ui.Div().Column().Gap(space2).Grow(1).MinWidth(ui.Px(0)).
		Child(ui.Text(h.text).FontSize(13).TextColor(t.TextMuted))
	row := ui.Div().Row().Align(ui.Start).Gap(space3).PaddingLeft(space3).PaddingRight(space1).PaddingY(space2).
		Radius(radiusL).Border(1, t.Border).Background(t.Surface).Key("hint-" + name).
		Child(ui.Div().PaddingTop(2).Child(ui.Icon(h.icon).Size(15).Color(ui.Hex(pal(t).brand))))
	actions := ui.Div().Row().Align(ui.Center).Gap(space1).Shrink(0)
	if h.action != "" {
		run := h.run
		btn := ui.Button(h.action).Secondary().Small().Key("hint-action").OnClick(func() {
			a.dismissHint(name)
			run()
		})
		if isCompact(ctx) {
			text = text.Child(ui.Div().Row().Child(btn))
		} else {
			actions = actions.Child(btn)
		}
	}
	actions = actions.Child(ui.IconButton(icons.X).Ghost().Small().Label("Dismiss").Tooltip("Got it").Key("hint-dismiss").
		OnClick(func() { a.dismissHint(name) }))
	return row.Child(text).Child(actions)
}

// starter is a task the empty chat offers.
type starter struct {
	icon *icons.Icon
	text string
}

// starterPrompts are a few tasks to start with, about the user's own repositories (and folders outside
// git), spread across them; none without any.
func starterPrompts(repos, plain []string) []starter {
	var names []string
	for _, p := range append(slices.Clone(repos), plain...) {
		if n := filepath.Base(p); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	pick := func(i int) string { return names[i%len(names)] }
	return []starter{
		{icons.Map, "Explain how " + pick(0) + " is laid out"},
		{icons.Bug, "Find a small bug in " + pick(1) + " and fix it"},
		{icons.FlaskConical, "Add tests for the least tested part of " + pick(2)},
	}
}

// starterList is the empty chat's tasks to start with. A click puts the task in the message box to
// edit; it doesn't send it.
func (a *App) starterList(ctx *ui.Context, prompts []starter) ui.Element {
	t := ctx.Theme()
	list := ui.Div().Column().Gap(space2).Width(ui.Full)
	for i, p := range prompts {
		text := p.text
		list = list.Child(ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space3).PaddingY(space2).MinHeight(ui.Px(40)).
			Radius(radiusL).Border(1, t.Border).Background(t.Surface).Cursor(ui.CursorPointer).
			Hover(ui.Style{Background: t.SurfaceHover, BorderColor: t.BorderStrong}).
			Role(access.RoleButton).Label(text).TabIndex(0).Key(starterKey(i)).
			Tooltip("Puts this in your message box, to change before you send it").
			OnClick(func() {
				a.orchFocus.FocusNow()
				a.orchDraft.Set(text)
				ctx.Dispatch(actions.MoveToEnd)
			}).
			Child(ui.Icon(p.icon).Size(15).Color(t.TextMuted)).
			Child(ui.Text(text).FontSize(13).TextColor(t.Text).Grow(1).MinWidth(ui.Px(0))))
	}
	return list
}

// starterKey keys the empty chat's nth task.
func starterKey(i int) string { return "starter-" + string(rune('0'+i)) }
