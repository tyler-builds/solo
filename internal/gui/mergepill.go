package gui

// The merge pill: at the top right of Solo's chat, a pill that counts the merge cards still waiting on
// the user, with a dot for each in its state's color, so they stay one glance away however far the
// conversation has moved past them. It opens a list of the cards, a row each, with every action the
// card itself has. It is in Solo's chat only, and absent when no card waits.

import (
	"fmt"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

const (
	pillListWidth = 520 // the open list, at most
	// pillOneLine is the narrowest list whose rows keep to one line; narrower, a row's title has a line
	// of its own.
	pillOneLine = 500
	pillDots    = 6 // dots on the pill before "+N"
)

// openCard is a thread whose merge card in Solo's chat can still be acted on.
type openCard struct {
	th core.ThreadView
}

// openCards are the threads with a live merge card in Solo's chat, the newest card first. A thread has
// one card (core.ItemProposal); a later one replaces it at the end of the conversation.
func (a *App) openCards(items []core.Item) []openCard {
	seen := map[string]bool{}
	var out []openCard
	for i := len(items) - 1; i >= 0; i-- {
		it := &items[i]
		if it.Kind != core.ItemProposal || seen[it.Ref] {
			continue
		}
		seen[it.Ref] = true
		if th, ok := a.thread(it.Ref); ok && cardLive(th) {
			out = append(out, openCard{th: th})
		}
	}
	return out
}

// cardLive reports whether a thread's merge card can still be acted on: its work hasn't landed, been
// discarded or been marked done. These are the threads whose card in the chat still shows its buttons
// rather than a status (proposalCard), less those marked done.
func cardLive(th core.ThreadView) bool {
	return th.IsWorker && !core.Finished(th.Status) && th.DoneAt.IsZero() && !th.Scratch
}

// chatAreaWidth is the width of Solo's chat pane, uncapped (chatWidth caps it at the column's).
func (a *App) chatAreaWidth(ctx *ui.Context) float32 {
	w, _ := ctx.WindowSize()
	if isCompact(ctx) {
		return w
	}
	chat := w - railW(ctx) - 2*space2
	if a.openID.Get() != "" {
		chat -= a.panelWidth(ctx) + space2
	}
	return chat
}

// setPillOpen opens or closes the merge pill's list. Closing it from inside (Escape, its close button)
// puts focus back on the pill.
func (a *App) setPillOpen(on, refocus bool) {
	a.pillOpen.Set(on)
	if !on && refocus {
		a.pillFocus.Focus()
	}
}

// mergePill is the pill and, while it is open, its list of cards. A component of its own: the cards
// and the threads re-render it, not the conversation.
func (a *App) mergePill(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	a.threads.Get() // the cards' threads: their states, and whether they still wait
	cards := a.openCards(a.itemsOf(core.OrchestratorID).Get())
	open := a.pillOpen.Get()
	if len(cards) == 0 {
		if open {
			ctx.Post(func() { a.pillOpen.Set(false) }) // the next card doesn't open it
		}
		return ui.Div().Absolute().AccessHidden()
	}
	label := fmt.Sprintf("%d to land", len(cards))
	pill := ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space3).Height(ui.Px(30)).Radius(15).Shrink(0).
		Background(t.Surface).Border(1, t.BorderStrong).Shadow(ui.ShadowMd).
		Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
		Role(access.RoleButton).Label(fmt.Sprintf("%s: %s", label, plural(len(cards), "merge card"))).
		Tooltip("Merge cards waiting on you").TabIndex(0).AccessExpanded(open).FocusHandle(a.pillFocus).Key("merge-pill").
		OnClick(func() { a.setPillOpen(!a.pillOpen.Peek(), false) }).
		Child(ui.Icon(icons.GitMerge).Size(14).Color(t.Success)).
		Child(ui.Text(label).FontSize(12).Medium().NoWrap()).
		Child(a.pillDots(ctx, cards))
	// The pill anchors the list: a press on it is inside the list, so the pill's own click closes it.
	anchor := ui.Div().Absolute().Top(space3).Right(space4).ZIndex(2).Child(pill)
	if !open {
		return anchor
	}
	w := min(float32(pillListWidth), a.chatAreaWidth(ctx)-2*space4)
	rows := ui.Div().Column()
	for _, c := range cards {
		rows = rows.Child(a.pillRow(ctx, c.th, w))
	}
	_, h := ctx.WindowSize()
	head := ui.Div().Row().Align(ui.Center).Gap(6).PaddingLeft(space3).PaddingRight(space1).Height(ui.Px(36)).Shrink(0).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).
		Child(ui.Text("Merge proposed").FontSize(12).Semibold().TextColor(t.Success)).
		Child(ui.Text(fmt.Sprint(len(cards))).FontSize(12).TextColor(t.TextSubtle)).
		Child(ui.Div().Grow(1)).
		Child(ui.IconButton(icons.X).Ghost().Small().Label("Close").Tooltip("Close").Key("merge-pill-close").
			OnClick(func() { a.setPillOpen(false, true) }))
	list := ui.Div().Overlay(ui.BelowEnd).OverlayGap(6).Column().Width(ui.Px(w)).MaxHeight(ui.Px(max(200, min(440, h-200)))).
		Radius(radiusL).Background(t.Surface).Border(1, t.BorderStrong).Shadow(ui.ShadowLg).ClipContent().
		Role(access.RoleDialog).Label("Merge cards").Key("merge-pill-list").
		// A press elsewhere closes it, as does Escape, which the window gives the topmost overlay. A menu
		// opened from a row is inside it.
		OnClickOutside(func() { a.setPillOpen(false, false) }).
		OnKeyDown(func(e *ui.KeyEvent) {
			if e.Key == input.KeyEscape {
				e.StopPropagation()
				a.setPillOpen(false, true)
			}
		}).
		Enter(ui.Fade().Slide(0, -4), ui.Animation{Duration: 120 * time.Millisecond}).
		Child(head).
		Child(ui.Div().Column().MinHeight(ui.Px(0)).Shrink(1).ScrollY().Key("merge-pill-rows").Child(rows))
	return anchor.Child(list)
}

// pillDots are a dot for each card in its state's color, then "+N" past pillDots.
func (a *App) pillDots(ctx *ui.Context, cards []openCard) ui.Element {
	t := ctx.Theme()
	row := ui.Div().Row().Align(ui.Center).Gap(3).Shrink(0).AccessHidden()
	for i, c := range cards {
		if i == pillDots {
			row = row.Child(ui.Text(fmt.Sprintf("+%d", len(cards)-pillDots)).FontSize(11).Medium().TextColor(t.TextMuted).MarginLeft(2))
			break
		}
		row = row.Child(dot(cardStateOf(ctx, a, c.th).color, 7))
	}
	return row
}

// pillRow is one card in the list, every action showing: where it stands, its title (which opens the
// thread), its pull request with its checks, the landing button, Review changes, and "…" for Request
// changes, Show card in chat, Done and Discard. A list narrower than pillOneLine gives the title a line
// of its own and the rest a second line, which wraps the actions under the checks when even that is
// too narrow, so the title is never squeezed to nothing.
func (a *App) pillRow(ctx *ui.Context, th core.ThreadView, w float32) ui.Element {
	t := ctx.Theme()
	id := th.ID
	st := cardStateOf(ctx, a, th)
	mark := ui.Div().Size(ui.Px(14), ui.Px(14)).Center().Shrink(0).Tooltip(st.tip).
		Role(access.RoleStaticText).Label(st.text).
		Child(ui.Icon(st.icon).Size(14).Color(st.color))
	where := ui.Div().Row().Align(ui.Center).Gap(4).Shrink(0)
	if pr := a.pillPRs(ctx, th); pr != nil {
		where = where.Child(pr)
	}
	where = where.Child(ui.Div().Tooltip(st.tip).Child(ui.Text(st.text).FontSize(12).TextColor(st.color).NoWrap()))
	oneLine := w >= pillOneLine
	actions := ui.Div().Row().Align(ui.Center).Gap(6).Shrink(0).
		Child(a.landElement(ctx, a.cardLand(th), oneLine)).
		Child(ui.IconButton(icons.FileDiff).Ghost().Small().Label("Review changes").Tooltip("Review changes").
			OnClick(func() {
				a.setPillOpen(false, false)
				a.openThread(id, "changes")
			})).
		Child(ui.DropdownMenu(ui.IconButton(icons.Ellipsis).Ghost().Small().Label("More").Tooltip("More").Key("merge-pill-more-"+id),
			func() []ui.MenuItem {
				return a.cardMenu(th, true, ui.MenuItem{Label: "Show card in chat", Icon: icons.LocateFixed, OnClick: func() {
					a.setPillOpen(false, false)
					a.showCard(id)
				}})
			}))
	title := ui.Div().MinWidth(ui.Px(0)).Shrink(1).Grow(1).Cursor(ui.CursorPointer).Key("merge-pill-title-" + id).
		Role(access.RoleButton).Label("Open " + th.Title).TabIndex(0).Tooltip(th.Title).
		Hover(ui.Style{TextColor: t.Link}).
		OnClick(func() {
			a.setPillOpen(false, false)
			a.openThread(id, "conversation")
		}).
		Child(ui.Text(th.Title).FontSize(13).Medium().Truncate())
	row := ui.Div().BorderEdges(ui.Edges{Bottom: 1}, t.Border).Key("merge-pill-row-" + id).
		Role(access.RoleGroup).Label(th.Title)
	if oneLine {
		return row.Row().Align(ui.Center).Gap(6).Height(ui.Px(40)).PaddingLeft(space3).PaddingRight(space1).
			Child(mark).Child(title).Child(where).Child(actions)
	}
	return row.Column().Gap(2).PaddingLeft(space3).PaddingRight(space1).PaddingY(6).
		Child(ui.Div().Row().Align(ui.Center).Gap(6).MinHeight(ui.Px(22)).MinWidth(ui.Px(0)).Child(mark).Child(title)).
		Child(ui.Div().Row().Wrap().Align(ui.Center).Justify(ui.SpaceBetween).Gap(4).PaddingLeft(20).MinWidth(ui.Px(0)).
			Child(where).Child(actions))
}

// cardState is where a card stands, in a few words: its checks once a pull request is open, or what it
// waits on before that.
type cardState struct {
	icon  *icons.Icon
	color ui.Color
	text  string // "Checks passed", "1 of 6 failed", "6/7 passed", "No PR yet"
	tip   string
}

func cardStateOf(ctx *ui.Context, a *App, th core.ThreadView) cardState {
	t := ctx.Theme()
	if cs, ok := mergeChecks(th); ok {
		icon, c := checksLook(ctx, cs.checks)
		text := cs.text
		switch {
		case cs.checks == forge.ChecksFailing && cs.counted:
			text = fmt.Sprintf("%d of %d failed", cs.n.Failed, cs.n.Total)
		case cs.checks == forge.ChecksPending && cs.counted:
			text = fmt.Sprintf("%d/%d passed", cs.n.Passed, cs.n.Total)
		}
		return cardState{icon: icon, color: c, text: text, tip: cs.text + ". " + cs.tip}
	}
	switch {
	case th.Resolving:
		return cardState{icons.Timer, t.Warning, "Resolving conflicts", "The thread is merging in newer work from " + th.BaseBranch}
	case core.Active(th.Status) || th.Status == core.StatusQueued:
		return cardState{icons.CircleDashed, t.TextMuted, "Still working", landBlockedReason(th, a.now())}
	case th.CanMarkReady:
		return cardState{icons.GitPullRequestDraft, t.TextMuted, "Draft", "Its pull request is a draft"}
	case th.CanOpenPR:
		noun := nounOf(th)
		return cardState{icons.GitPullRequestArrow, t.TextMuted, "No " + noun + " yet", "Open a " + noun + " to run its checks"}
	}
	return cardState{icons.CircleCheck, t.Success, "Ready", "Ready to land"}
}

// nounOf is "PR" or "MR" for the thread's forge, or its first repository's that has one.
func nounOf(th core.ThreadView) string {
	for _, p := range th.Parts {
		if p.Forge != forge.None {
			return p.Forge.Noun()
		}
	}
	return th.Forge.Noun()
}

// pillPRs opens the thread's pull request ("#61"), or with several, a menu of them ("3 PRs"). nil
// before one is open.
func (a *App) pillPRs(ctx *ui.Context, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	type link struct{ name, url string }
	var links []link
	for _, p := range th.Parts {
		if p.PR != nil && p.PR.State == forge.StateOpen {
			name := fmt.Sprintf("#%d", p.PR.Number)
			if th.MultiRepo() {
				name = p.Name + " " + name
			}
			links = append(links, link{name, p.PR.URL})
		}
	}
	if len(links) == 0 && th.PR != nil && th.PR.State == forge.StateOpen {
		links = append(links, link{fmt.Sprintf("#%d", th.PR.Number), th.PR.URL})
	}
	switch len(links) {
	case 0:
		return nil
	case 1:
		url, tip := links[0].url, "Open "+links[0].name+" in your browser"
		info := ui.Hex(pal(t).info)
		return ui.Div().Row().Align(ui.Center).Gap(3).PaddingX(6).Height(ui.Px(22)).Radius(radiusS).Shrink(0).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).Key("merge-pill-pr-" + th.ID).
			Role(access.RoleLink).Label(tip).Tooltip(tip).TabIndex(0).
			OnClick(func() { _ = openURL(ctx, url) }).
			Child(ui.Text(links[0].name).FontSize(12).Medium().TextColor(info).NoWrap()).
			Child(ui.Icon(icons.ArrowUpRight).Size(12).Color(info))
	}
	trigger := ui.Button(fmt.Sprintf("%d %ss", len(links), nounOf(th))).IconEnd(icons.ChevronDown).Ghost().Small().
		Key("merge-pill-prs-" + th.ID).Tooltip("Open one in your browser")
	return ui.DropdownMenu(trigger, func() []ui.MenuItem {
		var items []ui.MenuItem
		for _, l := range links {
			url := l.url
			items = append(items, ui.MenuItem{Label: l.name, Icon: icons.ExternalLink, OnClick: func() { _ = openURL(ctx, url) }})
		}
		return items
	})
}

// openURL opens a pull request's page in the browser; tests replace it to see what was opened.
var openURL = func(ctx *ui.Context, url string) error { return ctx.OpenURL(url) }

// showCard scrolls Solo's chat to the thread's merge card, where its summary is.
func (a *App) showCard(id string) {
	c := a.rows["orch"]
	if c == nil {
		return
	}
	for i, r := range c.rows {
		if r.item != nil && r.item.Kind == core.ItemProposal && r.item.Ref == id {
			a.listHandle("orch").ScrollToItem(i)
			return
		}
	}
}
