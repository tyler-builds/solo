package gui

// Prototype, for design review: a panel inset into the top right of Solo's chat that keeps every open
// merge card one glance away, however far the conversation has scrolled past it. Three directions,
// chosen by SOLO_MERGE_PANEL (or landPanelKind, which the shots set):
//
//   - shelf: a column docked at the right of the chat, which makes room for it; always open, a small
//     card per thread with labeled landing buttons. In a narrow chat it becomes a bar above the
//     conversation that folds open.
//   - pill: a pill floating at the top right with a count and a dot per card; a click opens a list of
//     one-line rows whose actions show on hover or focus.
//   - stack: a floating list of one-line rows, open by default, every action visible as an icon;
//     it folds into a round badge, and starts folded in a narrow chat.
//
// Unset, there is no panel. Once a direction is chosen, the others go.

import (
	"context"
	"fmt"
	"os"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

type landPanelStyle string

const (
	landShelf landPanelStyle = "shelf"
	landPill  landPanelStyle = "pill"
	landStack landPanelStyle = "stack"
)

// landPanelKind is the direction shown; "" for none.
var landPanelKind = landPanelStyle(os.Getenv("SOLO_MERGE_PANEL"))

const (
	shelfWidth = 320 // the docked shelf, its gap to the chat included
	// shelfMinChat is the narrowest chat the shelf docks beside; narrower, it is a bar above the chat.
	shelfMinChat = 600
	pillListW    = 420
	stackW       = 520
	stackMinChat = 760 // a narrower chat starts with the stack folded
	stackRows    = 4   // rows the stack shows before "N more"
)

// openCard is a thread whose merge card in Solo's chat can still be acted on.
type openCard struct {
	th      core.ThreadView
	summary string
}

// openCards are the threads with a live merge card in Solo's chat, the newest card first. A card is
// live while its thread's work hasn't landed, been discarded or been marked done: the same threads
// whose inline card still shows its buttons rather than a status pill (proposalCard), less those marked
// done before they landed (archived), whose inline card still shows buttons though nothing is left to land.
func (a *App) openCards() []openCard {
	items := a.itemsOf(core.OrchestratorID).Get()
	seen := map[string]bool{}
	var out []openCard
	for i := len(items) - 1; i >= 0; i-- {
		it := &items[i]
		if it.Kind != core.ItemProposal || seen[it.Ref] {
			continue
		}
		seen[it.Ref] = true
		th, ok := a.thread(it.Ref)
		if !ok || !cardLive(th) {
			continue
		}
		var d core.ProposalData
		it.Decode(&d)
		out = append(out, openCard{th: th, summary: d.Summary})
	}
	return out
}

// cardLive reports whether a thread's merge card can still be acted on.
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

// landSpec is a thread's landing action as its merge card offers it (landAction, landButton,
// landControlMulti, mergeControlMulti), described rather than drawn, so a compact card can draw it as
// a labeled button, an icon, or a menu item.
type landSpec struct {
	label   string // "Open PR", "Merge all", "Merge api #142", …
	short   string // label without a repository's name, for a narrow row: "Merge #142"
	tip     string
	icon    *icons.Icon
	run     func() // nil: disabled, its tip saying why
	primary bool
	status  bool // a note rather than a button: "Auto-merge on"
	loading bool
	more    []ui.MenuItem // the split button's menu: drafts, one repository at a time
}

// landOf is th's landing action, as its merge card shows it.
func (a *App) landOf(th core.ThreadView) landSpec {
	if th.Resolving {
		return landSpec{label: "Merge", short: "Merge", icon: icons.GitMerge, primary: true, loading: true,
			tip: "Resolving conflicts: newer work landed in " + th.BaseBranch + " since this started. The thread is merging it in, then Solo merges."}
	}
	blocked := func() landSpec {
		label, icon := "Merge", icons.GitMerge
		if th.Forge != forge.None && len(openPRs(th)) == 0 {
			label, icon = "Open "+th.Forge.Noun(), icons.GitPullRequestArrow
		}
		return landSpec{label: label, short: label, icon: icon, tip: landBlockedReason(th, a.now())}
	}
	busy := core.Active(th.Status) || th.Status == core.StatusQueued
	if busy && (th.Scratch || (th.IsWorker && !th.InPlace && (th.Changed > 0 || len(openPRs(th)) > 0))) {
		return blocked()
	}
	if !th.CanOpenPR && !th.CanMerge && !th.CanMarkReady && len(openPRs(th)) == 0 {
		return blocked()
	}
	var s *landSpec
	if th.MultiRepo() {
		s = a.landMultiOf(th)
	} else {
		s = a.landSingleOf(th)
	}
	if s == nil {
		return blocked()
	}
	if th.Scratch {
		s.primary = false
	}
	if a.busy.Get()[th.ID] {
		s.loading = true
	}
	if s.short == "" {
		s.short = s.label
	}
	return *s
}

func (a *App) landSingleOf(th core.ThreadView) *landSpec {
	id := th.ID
	switch {
	case th.CanOpenPR:
		noun := th.Forge.Noun()
		return &landSpec{label: "Open " + noun, icon: icons.GitPullRequestArrow, primary: true,
			tip: "Push the branch and open a " + noun, run: func() { a.openPR(id, false) },
			more: []ui.MenuItem{{Label: "Open as draft", Icon: icons.GitPullRequestDraft, OnClick: func() { a.openPR(id, true) }}}}
	case th.CanMarkReady:
		return &landSpec{label: "Ready for review", short: "Ready", icon: icons.GitPullRequest, primary: true,
			tip: fmt.Sprintf("Mark %s #%d ready for review", th.PR.Kind.Noun(), th.PR.Number), run: func() { a.markReady(id) }}
	case th.PR != nil && th.PR.State == forge.StateOpen:
		if th.CanMerge {
			return &landSpec{label: "Merge", icon: icons.GitMerge, primary: true, run: func() { a.merge(id) },
				tip: fmt.Sprintf("Merge %s #%d into %s", th.PR.Kind.Noun(), th.PR.Number, th.BaseBranch)}
		}
		if th.PR.AutoMerge {
			return &landSpec{label: "Auto-merge on", short: "Auto-merge", icon: icons.Timer, status: true,
				tip: "It merges once its required checks pass"}
		}
		return &landSpec{label: "Merge", icon: icons.GitMerge, tip: "Can't merge yet: " + th.PR.Blocked}
	case th.CanMerge:
		if th.Forge == forge.None {
			return &landSpec{label: "Merge locally", icon: icons.GitMerge, primary: true, run: func() { a.merge(id) },
				tip: "No remote, so no pull request: merges " + branchLabel(th) + " into your local " + th.BaseBranch}
		}
		return &landSpec{label: "Merge", icon: icons.GitMerge, primary: true, run: func() { a.merge(id) },
			tip: "Merge " + th.Branch + " into " + th.BaseBranch}
	}
	return nil
}

func (a *App) landMultiOf(th core.ThreadView) *landSpec {
	id := th.ID
	var canOpen, drafts []core.PartView
	for _, p := range th.Parts {
		if p.CanOpenPR {
			canOpen = append(canOpen, p)
		}
		if p.PR != nil && p.PR.State == forge.StateOpen && p.PR.Draft {
			drafts = append(drafts, p)
		}
	}
	switch {
	case len(canOpen) > 0:
		noun := canOpen[0].Forge.Noun()
		more := []ui.MenuItem{{Label: "Open " + plural(len(canOpen), noun) + " as drafts", Icon: icons.GitPullRequestDraft,
			OnClick: func() { a.openPRFor(id, "", true) }}, ui.MenuSeparator()}
		for _, p := range canOpen {
			repo := p.Repo
			more = append(more, ui.MenuItem{Label: noun + " for " + p.Name + " only", Icon: icons.GitPullRequestArrow,
				OnClick: func() { a.openPRFor(id, repo, false) }})
		}
		return &landSpec{label: "Open " + plural(len(canOpen), noun), icon: icons.GitPullRequestArrow, primary: true,
			tip: "Push each branch and open a " + noun + " in each repository", run: func() { a.openPRFor(id, "", false) }, more: more}
	case len(drafts) > 0:
		return &landSpec{label: "Ready for review", short: "Ready", icon: icons.GitPullRequest, primary: true,
			tip: "Mark " + plural(len(drafts), "draft") + " ready for review", run: func() { a.markReady(id) }}
	}
	targets, ready := mergeTargets(th)
	autoAll := len(targets) > 0
	for _, p := range targets {
		autoAll = autoAll && p.PR != nil && p.PR.AutoMerge && !p.CanMerge
	}
	switch len(targets) {
	case 0:
		return nil
	case 1:
		p := targets[0]
		repo := p.Repo
		switch {
		case p.CanMerge:
			return &landSpec{label: partMergeLabel(p), short: partMergeShort(p), icon: icons.GitMerge, primary: true,
				tip: partMergeTip(p), run: func() { a.mergeRepo(id, repo) }}
		case autoAll:
			return &landSpec{label: "Auto-merge on", short: "Auto-merge", icon: icons.Timer, status: true, tip: "It merges once its required checks pass"}
		}
		tip := "Can't merge yet"
		if p.PR.Blocked != "" {
			tip += ": " + p.PR.Blocked
		}
		return &landSpec{label: partMergeLabel(p), short: partMergeShort(p), icon: icons.GitMerge, tip: tip}
	}
	more := []ui.MenuItem{{Label: "Merge all", Icon: icons.GitMerge, Disabled: len(ready) < len(targets),
		OnClick: func() { a.mergeRepo(id, "") }}, ui.MenuSeparator()}
	for _, p := range targets {
		repo, label := p.Repo, partMergeLabel(p)
		if !p.CanMerge && p.PR.Blocked != "" {
			label += " · " + p.PR.Blocked
		}
		more = append(more, ui.MenuItem{Label: label, Icon: icons.GitMerge, Disabled: !p.CanMerge,
			OnClick: func() { a.mergeRepo(id, repo) }})
	}
	switch {
	case len(ready) == len(targets):
		return &landSpec{label: "Merge all", icon: icons.GitMerge, primary: true, tip: "Merge each repository into its base branch",
			run: func() { a.mergeRepo(id, "") }, more: more}
	case len(ready) == 1:
		repo := ready[0].Repo
		return &landSpec{label: partMergeLabel(ready[0]), short: partMergeShort(ready[0]), icon: icons.GitMerge, primary: true,
			tip: partMergeTip(ready[0]), run: func() { a.mergeRepo(id, repo) }, more: more}
	case len(ready) > 1:
		return &landSpec{label: fmt.Sprintf("Merge %d ready", len(ready)), icon: icons.GitMerge, primary: true,
			run: func() { a.mergeRepo(id, "") }, more: more}
	case autoAll:
		return &landSpec{label: "Auto-merge on", short: "Auto-merge", icon: icons.Timer, status: true, tip: "Each merges once its required checks pass"}
	}
	return &landSpec{label: "Merge", icon: icons.GitMerge, tip: "None can merge yet", more: more}
}

// markReady marks a thread's draft pull requests ready for review.
func (a *App) markReady(id string) {
	a.runBusy(id, "Marking it ready", func(c context.Context) error { return a.b.MarkReady(c, id) })
}

// landButtonOf draws a landing action as the merge card does: a small labeled button, split when it has
// a menu. short uses the label without the repository's name.
func landButtonOf(ctx *ui.Context, s landSpec, key string, short bool) ui.Element {
	t := ctx.Theme()
	label := s.label
	if short {
		label = s.short
	}
	if s.status {
		return ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).Height(ui.Px(28)).Shrink(0).Tooltip(s.tip).
			Child(ui.Icon(s.icon).Size(13).Color(t.TextMuted)).
			Child(ui.Text(label).FontSize(12).Medium().TextColor(t.TextMuted).NoWrap())
	}
	if len(s.more) > 0 {
		run := s.run
		if run == nil {
			run = func() {}
		}
		more := s.more
		b := ui.SplitButton(label, run, func() []ui.MenuItem { return more }).Icon(s.icon).Small().Key(key).Tooltip(s.tip).Loading(s.loading)
		if s.primary && s.run != nil {
			return b.Primary()
		}
		return b.Secondary()
	}
	b := ui.Button(label).Icon(s.icon).Small().Key(key).Tooltip(s.tip).Disabled(s.run == nil).Loading(s.loading)
	if s.run != nil {
		b = b.OnClick(s.run)
	}
	if s.primary && s.run != nil {
		return b.Primary()
	}
	return b.Secondary()
}

// landIconOf draws a landing action as an icon button, its label in its tooltip. The split button's
// menu moves to the card's "…" menu.
func landIconOf(ctx *ui.Context, s landSpec, key string) ui.Element {
	t := ctx.Theme()
	if s.status {
		return ui.Div().Size(ui.Px(28), ui.Px(28)).Center().Shrink(0).Tooltip(s.label + ": " + s.tip).
			Role(access.RoleStaticText).Label(s.label).
			Child(ui.Icon(s.icon).Size(14).Color(t.TextMuted))
	}
	tip := s.label
	if s.tip != "" {
		tip += ". " + s.tip
	}
	b := ui.IconButton(s.icon).Small().Label(s.label).Tooltip(tip).Key(key).Disabled(s.run == nil).Loading(s.loading)
	if s.run != nil {
		b = b.OnClick(s.run)
	}
	if s.primary && s.run != nil {
		return b.Primary()
	}
	return b.Ghost()
}

// cardState is where a card stands, in a few words: its checks once a pull request is open, or what it
// waits on before that.
type cardState struct {
	icon  *icons.Icon
	color ui.Color
	text  string // short: "1 of 6 failed"
	tip   string
}

func stateOf(ctx *ui.Context, a *App, th core.ThreadView) cardState {
	t := ctx.Theme()
	if cs, ok := mergeChecks(th); ok {
		icon, c := checksLook(ctx, cs.checks)
		text := cs.text
		switch cs.checks {
		case forge.ChecksFailing:
			if cs.counted {
				text = fmt.Sprintf("%d of %d failed", cs.n.Failed, cs.n.Total)
			}
		case forge.ChecksPending:
			if cs.counted {
				text = fmt.Sprintf("%d/%d passed", cs.n.Passed, cs.n.Total)
			}
		}
		return cardState{icon: icon, color: c, text: text, tip: cs.text + ". " + cs.tip}
	}
	busy := core.Active(th.Status) || th.Status == core.StatusQueued
	switch {
	case th.Resolving:
		return cardState{icons.Timer, t.Warning, "Resolving conflicts", "The thread is merging in newer work from " + th.BaseBranch}
	case busy:
		return cardState{icons.CircleDashed, t.TextMuted, "Still working", landBlockedReason(th, a.now())}
	case th.CanMarkReady:
		return cardState{icons.GitPullRequestDraft, t.TextMuted, "Draft", "Its pull request is a draft"}
	case th.CanOpenPR:
		noun := nounOf(th)
		return cardState{icons.GitPullRequestArrow, t.TextMuted, "No " + noun + " yet", "Open a " + noun + " to run its checks"}
	}
	return cardState{icons.CircleCheck, t.Success, "Ready", "Ready to land"}
}

// nounOf is "PR" or "MR" for the thread's forge.
func nounOf(th core.ThreadView) string {
	for _, p := range th.Parts {
		if p.Forge != forge.None && p.Forge != "" {
			return p.Forge.Noun()
		}
	}
	return th.Forge.Noun()
}

// prLinkOf opens the thread's pull request ("#61"), or with several, a menu of them ("3 PRs").
// nil before one is open.
func prLinkOf(ctx *ui.Context, th core.ThreadView, key string) ui.Element {
	t := ctx.Theme()
	type link struct{ name, url string }
	var links []link
	if th.MultiRepo() {
		for _, p := range th.Parts {
			if p.PR != nil && p.PR.State == forge.StateOpen {
				links = append(links, link{fmt.Sprintf("%s #%d", p.Name, p.PR.Number), p.PR.URL})
			}
		}
	} else if th.PR != nil && th.PR.State == forge.StateOpen {
		links = append(links, link{fmt.Sprintf("#%d", th.PR.Number), th.PR.URL})
	}
	switch len(links) {
	case 0:
		return nil
	case 1:
		url := links[0].url
		return ui.Div().Row().Align(ui.Center).Gap(3).PaddingX(6).Height(ui.Px(22)).Radius(radiusS).Shrink(0).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).Key(key).
			Role(access.RoleLink).Label("Open " + links[0].name + " in your browser").Tooltip("Open " + links[0].name + " in your browser").
			OnClick(func() { _ = ctx.OpenURL(url) }).
			Child(ui.Text(links[0].name).FontSize(12).Medium().TextColor(ui.Hex(pal(t).info)).NoWrap()).
			Child(ui.Icon(icons.ArrowUpRight).Size(12).Color(ui.Hex(pal(t).info)))
	}
	trigger := ui.Button(fmt.Sprintf("%d %ss", len(links), nounOf(th))).IconEnd(icons.ChevronDown).Ghost().Small().
		Key(key).Tooltip("Open one in your browser")
	return ui.DropdownMenu(trigger, func() []ui.MenuItem {
		var items []ui.MenuItem
		for _, l := range links {
			url := l.url
			items = append(items, ui.MenuItem{Label: l.name, Icon: icons.ExternalLink, OnClick: func() { _ = ctx.OpenURL(url) }})
		}
		return items
	})
}

// requestChanges starts a message to Solo asking for changes to the thread.
func (a *App) requestChanges(title string) {
	a.orchDraft.Set("Changes for “" + title + "”: ")
	a.orchFocus.Focus()
}

// showCard scrolls Solo's chat to the thread's merge card, where its summary is.
func (a *App) showCard(id string) {
	if c := a.rows["orch"]; c != nil {
		for i, r := range c.rows {
			if r.item != nil && r.item.Kind == core.ItemProposal && r.item.Ref == id {
				a.listHandle("orch").ScrollToItem(i)
				return
			}
		}
	}
}

// menuParts are what a compact card's "…" menu holds besides what it shows.
type menuParts struct {
	land            *landSpec // its split menu, and the action itself when the card shows no button for it
	review, request bool      // the card has no button of its own for these
	prs             bool      // nor a link to its pull requests
}

// cardMenu is a compact card's "…" menu: what the card has no room to show, then the merge card's own
// menu (Done, Discard).
func (a *App) cardMenu(ctx *ui.Context, th core.ThreadView, m menuParts) []ui.MenuItem {
	id, title := th.ID, th.Title
	var items []ui.MenuItem
	if s := m.land; s != nil {
		items = append(items, s.more...)
		if len(s.more) > 0 {
			items = append(items, ui.MenuSeparator())
		}
	}
	if m.prs {
		for _, p := range th.Parts {
			if p.PR != nil && p.PR.State == forge.StateOpen {
				url := p.PR.URL
				name := fmt.Sprintf("#%d", p.PR.Number)
				if th.MultiRepo() {
					name = p.Name + " " + name
				}
				items = append(items, ui.MenuItem{Label: "Open " + name + " in browser", Icon: icons.ExternalLink, OnClick: func() { _ = ctx.OpenURL(url) }})
			}
		}
	}
	if m.review {
		items = append(items, ui.MenuItem{Label: "Review changes", Icon: icons.FileDiff, OnClick: func() { a.openThread(id, "changes") }})
	}
	if m.request {
		items = append(items, ui.MenuItem{Label: "Request changes", Icon: icons.MessageSquare, OnClick: func() { a.requestChanges(title) }})
	}
	items = append(items, ui.MenuItem{Label: "Show card in chat", Icon: icons.LocateFixed, OnClick: func() { a.showCard(id) }})
	items = append(items, ui.MenuSeparator())
	if th.CanDone {
		label := "Done"
		if asksToLand(th) {
			label = "Mark done…"
		}
		items = append(items, ui.MenuItem{Label: label, Icon: icons.Check, OnClick: func() { a.markDone(th) }})
	}
	return append(items, ui.MenuItem{Label: "Discard thread…", Icon: icons.Trash, OnClick: func() { a.confirmDiscard(th) }})
}

func (a *App) moreButton(ctx *ui.Context, th core.ThreadView, m menuParts, key string) ui.Element {
	return ui.Div().Size(ui.Px(28), ui.Px(28)).Center().Shrink(0).Child(
		ui.DropdownMenu(ui.IconButton(icons.Ellipsis).Ghost().Small().Label("More").Tooltip("More").Key(key),
			func() []ui.MenuItem { return a.cardMenu(ctx, th, m) }))
}

// titleLink is the thread's title, which opens the thread.
func (a *App) titleLink(ctx *ui.Context, th core.ThreadView, size float32, key string) *ui.DivElement {
	t := ctx.Theme()
	id := th.ID
	return ui.Div().MinWidth(ui.Px(0)).Shrink(1).Cursor(ui.CursorPointer).Key(key).
		Role(access.RoleButton).Label("Open " + th.Title).TabIndex(0).Tooltip(th.Title).
		Hover(ui.Style{TextColor: t.Link}).
		OnClick(func() { a.openThread(id, "conversation") }).
		Child(ui.Text(th.Title).FontSize(size).Medium().Truncate())
}

// landPanelFor wraps Solo's chat body with the chosen direction's panel. It is the body unchanged when
// there is no direction or no open card.
func (a *App) landPanelFor(ctx *ui.Context, body ui.Element) ui.Element {
	a.landShelfW = 0
	if landPanelKind == "" {
		return body
	}
	cards := a.openCards()
	if len(cards) == 0 {
		return body
	}
	switch landPanelKind {
	case landShelf:
		return a.shelf(ctx, body, cards)
	case landPill:
		return ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).Child(body).Child(a.pill(ctx, cards))
	case landStack:
		return ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).Child(body).Child(a.stack(ctx, cards))
	}
	return body
}

// isOpen is the panel's open state, or def before the user has opened or closed it.
func (a *App) landIsOpen(def bool) bool {
	if p := a.landOpen.Get(); p != nil {
		return *p
	}
	return def
}

func (a *App) setLandOpen(on bool) { a.landOpen.Set(&on) }

// countLine is "3 to land", with a dot per card in its state's color.
func countDots(ctx *ui.Context, a *App, cards []openCard, max int) ui.Element {
	t := ctx.Theme()
	row := ui.Div().Row().Align(ui.Center).Gap(3).Shrink(0)
	for i, c := range cards {
		if i == max {
			row = row.Child(ui.Text(fmt.Sprintf("+%d", len(cards)-max)).FontSize(11).Medium().TextColor(t.TextMuted).MarginLeft(2))
			break
		}
		row = row.Child(dot(stateOf(ctx, a, c.th).color, 7))
	}
	return row
}

// ---- Direction A: the shelf ----

// shelf docks the cards in a column at the right of the chat, which narrows to make room; in a chat too
// narrow for both, they fold into a bar above the conversation.
func (a *App) shelf(ctx *ui.Context, body ui.Element, cards []openCard) ui.Element {
	t := ctx.Theme()
	area := a.chatAreaWidth(ctx)
	if area-shelfWidth < shelfMinChat {
		return a.shelfBar(ctx, body, cards)
	}
	a.landShelfW = shelfWidth
	list := ui.Div().Column().Gap(space2).PaddingBottom(space4)
	for _, c := range cards {
		list = list.Child(a.shelfCard(ctx, c, shelfWidth-space4))
	}
	head := ui.Div().Row().Align(ui.Center).Gap(6).Height(ui.Px(28)).Shrink(0).PaddingX(space1).
		Child(ui.Icon(icons.GitMerge).Size(14).Color(t.Success)).
		Child(ui.Text("To land").FontSize(12).Semibold().TextColor(t.TextMuted)).
		Child(ui.Text(fmt.Sprint(len(cards))).FontSize(12).TextColor(t.TextSubtle))
	col := ui.Div().Column().Width(ui.Px(shelfWidth)).Shrink(0).PaddingTop(space3).PaddingRight(space4).Gap(space1).
		MinHeight(ui.Px(0)).Key("land-shelf").Label("Merge cards to land").
		Child(head).
		Child(ui.Div().Column().MinHeight(ui.Px(0)).Shrink(1).ScrollY().Key("land-shelf-list").Child(list))
	return ui.Div().Row().Grow(1).MinHeight(ui.Px(0)).
		Child(ui.Div().Column().Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(0)).Child(body)).
		Child(col)
}

// shelfCard is one card on the shelf: the title; the repositories, where it stands and its pull
// request; the landing button and the rest of the card's actions.
func (a *App) shelfCard(ctx *ui.Context, c openCard, width float32) ui.Element {
	t := ctx.Theme()
	th := c.th
	id := th.ID
	st := stateOf(ctx, a, th)
	land := a.landOf(th)
	inner := width - 2*space3 - 2
	pr := prLinkOf(ctx, th, "shelf-pr-"+id)
	prW := float32(0)
	if pr != nil {
		prW = 64
	}
	stateW := textWidth(ctx, st.text, 12, ui.FontRegular)
	room := inner - 20 - stateW - 16 - prW
	meta := ui.Div().Row().Align(ui.Center).Gap(6).MinWidth(ui.Px(0)).PaddingLeft(20).
		Child(fitRepos(ctx, "shelf-"+id, repoNames(th), room)).
		Child(ui.Text("·").FontSize(12).TextColor(t.TextSubtle)).
		Child(ui.Text(st.text).FontSize(12).TextColor(st.color).NoWrap()).
		Child(ui.Div().Grow(1)).
		Child(pr)
	head := ui.Div().Row().Align(ui.Center).Gap(6).MinWidth(ui.Px(0)).
		Child(ui.Div().Size(ui.Px(14), ui.Px(14)).Center().Shrink(0).Tooltip(st.tip).Child(ui.Icon(st.icon).Size(14).Color(st.color))).
		Child(a.titleLink(ctx, th, 13, "shelf-title-"+id))
	actions := ui.Div().Row().Align(ui.Center).Gap(space1).
		Child(landButtonOf(ctx, land, "shelf-land-"+id, false)).
		Child(ui.Div().Grow(1)).
		Child(ui.IconButton(icons.FileDiff).Ghost().Small().Label("Review changes").Tooltip("Review changes").
			OnClick(func() { a.openThread(id, "changes") })).
		Child(ui.IconButton(icons.MessageSquare).Ghost().Small().Label("Request changes").
			Tooltip("Request changes: tell Solo what to change; it passes it on to the thread").
			OnClick(func() { a.requestChanges(th.Title) })).
		Child(a.moreButton(ctx, th, menuParts{}, "shelf-more-"+id))
	return ui.Div().Column().Gap(6).Padding(space3).Radius(radiusL).Background(t.Surface).Border(1, t.BorderStrong).
		Key("shelf-card-" + id).
		Child(head).Child(meta).Child(actions.MarginTop(2))
}

// shelfBar is the shelf in a narrow chat: a bar above the conversation that says how many cards wait
// and folds open into the same cards, two to a row when they fit, pushing the conversation down.
func (a *App) shelfBar(ctx *ui.Context, body ui.Element, cards []openCard) ui.Element {
	t := ctx.Theme()
	open := a.landIsOpen(false)
	chev := icons.ChevronDown
	if open {
		chev = icons.ChevronUp
	}
	bar := ui.Div().Row().Align(ui.Center).Gap(space2).Height(ui.Px(36)).PaddingX(space3).Radius(radiusL).
		Background(t.Surface).Border(1, t.BorderStrong).Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
		Role(access.RoleButton).Label(fmt.Sprintf("%d to land", len(cards))).TabIndex(0).AccessExpanded(open).Key("land-bar").
		OnClick(func() { a.setLandOpen(!open) }).
		Child(ui.Icon(icons.GitMerge).Size(14).Color(t.Success)).
		Child(ui.Text(fmt.Sprintf("%d to land", len(cards))).FontSize(13).Medium()).
		Child(countDots(ctx, a, cards, 8)).
		Child(ui.Div().Grow(1)).
		Child(ui.Text(map[bool]string{true: "Hide", false: "Show"}[open]).FontSize(12).TextColor(t.TextMuted)).
		Child(ui.Icon(chev).Size(14).Color(t.TextMuted))
	g := gutter(ctx)
	col := ui.Div().Column().Gap(space2).Width(ui.Full).MaxWidth(ui.Px(chatMaxWidth)).Child(bar)
	if open {
		inner := min(a.chatAreaWidth(ctx), chatMaxWidth+2*g) - 2*g
		perRow := 1
		if inner >= 2*280+space2 {
			perRow = 2
		}
		cw := (inner - float32(perRow-1)*space2) / float32(perRow)
		grid := ui.Div().Column().Gap(space2)
		for i := 0; i < len(cards); i += perRow {
			r := ui.Div().Row().Gap(space2)
			for j := i; j < i+perRow && j < len(cards); j++ {
				r = r.Child(ui.Div().Width(ui.Px(cw)).Shrink(0).Child(a.shelfCard(ctx, cards[j], cw)))
			}
			grid = grid.Child(r)
		}
		col = col.Child(ui.Div().Column().MaxHeight(ui.Px(330)).ScrollY().Key("land-bar-list").Child(grid))
	}
	return ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).
		Child(ui.Div().Column().Align(ui.Center).PaddingX(g).PaddingTop(space3).Shrink(0).Child(col)).
		Child(body)
}

// ---- Direction B: the pill ----

// pill floats a small pill at the top right of the chat: how many cards wait, and a dot for each in its
// state's color. A click opens them as a list of one-line rows over the chat; Escape or a click outside
// closes it.
func (a *App) pill(ctx *ui.Context, cards []openCard) ui.Element {
	t := ctx.Theme()
	open := a.landIsOpen(false)
	label := fmt.Sprintf("%d to land", len(cards))
	p := ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space3).Height(ui.Px(30)).Radius(15).Shrink(0).
		Background(t.Surface).Border(1, t.BorderStrong).Shadow(ui.ShadowMd).
		Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
		Role(access.RoleButton).Label(label).TabIndex(0).AccessExpanded(open).Key("land-pill").
		OnClick(func() { a.setLandOpen(!open) }).
		Child(ui.Icon(icons.GitMerge).Size(14).Color(t.Success)).
		Child(ui.Text(label).FontSize(12).Medium().NoWrap()).
		Child(countDots(ctx, a, cards, 6))
	wrap := ui.Div().Absolute().Top(space3).Right(space4).ZIndex(2).Column().Align(ui.End).Gap(6).Child(p)
	if !open {
		return wrap
	}
	list := ui.Div().Column().PaddingY(space1)
	for _, c := range cards {
		list = list.Child(a.pillRow(ctx, c))
	}
	_, h := ctx.WindowSize()
	head := ui.Div().Row().Align(ui.Center).Gap(6).PaddingLeft(space3).PaddingRight(space1).Height(ui.Px(36)).Shrink(0).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).
		Child(ui.Text("Merge proposed").FontSize(12).Semibold().TextColor(t.Success)).
		Child(ui.Text(fmt.Sprint(len(cards))).FontSize(12).TextColor(t.TextSubtle)).
		Child(ui.Div().Grow(1)).
		Child(caption(ctx, "Hover a row for its actions")).
		Child(ui.IconButton(icons.X).Ghost().Small().Label("Close").Tooltip("Close").OnClick(func() { a.setLandOpen(false) }))
	w := min(float32(pillListW), a.chatAreaWidth(ctx)-2*space4)
	panel := ui.Div().Column().Width(ui.Px(w)).MaxHeight(ui.Px(min(440, h-260))).Radius(radiusL).
		Background(t.Surface).Border(1, t.BorderStrong).Shadow(ui.ShadowLg).ClipContent().
		Key("land-pill-list").Label("Merge cards to land").
		OnClickOutside(func() { a.setLandOpen(false) }).
		Child(head).
		Child(ui.Div().Column().MinHeight(ui.Px(0)).Shrink(1).ScrollY().Child(list))
	return wrap.Child(panel)
}

// pillRow is one card as a line: where it stands, its title, its repositories and its checks; hovered
// or focused, the words on the right give way to the card's actions.
func (a *App) pillRow(ctx *ui.Context, c openCard) ui.Element {
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		th := c.th
		id := th.ID
		hover := ui.Local(ctx, "hover", false)
		st := stateOf(ctx, a, th)
		land := a.landOf(th)
		row := ui.Div().Row().Align(ui.Center).Gap(space2).Height(ui.Px(40)).PaddingX(space3).
			Key("pill-row-" + id).
			Hover(ui.Style{Background: t.SurfaceHover}).FocusWithin(ui.Style{Background: t.SurfaceHover}).
			OnMouseEnter(func() { hover.Set(true) }).OnMouseLeave(func() { hover.Set(false) }).
			Child(ui.Div().Size(ui.Px(14), ui.Px(14)).Center().Shrink(0).Tooltip(st.tip).Child(ui.Icon(st.icon).Size(14).Color(st.color))).
			Child(a.titleLink(ctx, th, 13, "pill-title-"+id).Grow(1))
		if hover.Get() || landHoverAll {
			pr := ui.Element(nil)
			if !th.MultiRepo() {
				pr = prIconOf(ctx, th)
			}
			return row.
				Child(landIconOf(ctx, land, "pill-land-"+id)).
				Child(pr).
				Child(ui.IconButton(icons.FileDiff).Ghost().Small().Label("Review changes").Tooltip("Review changes").
					OnClick(func() { a.openThread(id, "changes") })).
				Child(ui.IconButton(icons.MessageSquare).Ghost().Small().Label("Request changes").Tooltip("Request changes").
					OnClick(func() { a.requestChanges(th.Title) })).
				Child(a.moreButton(ctx, th, menuParts{land: &land, prs: th.MultiRepo()}, "pill-more-"+id))
		}
		names := repoNames(th)
		repos := names[0]
		if len(names) > 1 {
			repos = fmt.Sprintf("%s +%d", names[0], len(names)-1)
		}
		return row.
			Child(ui.Div().MaxWidth(ui.Px(110)).MinWidth(ui.Px(0)).Shrink(1).Child(ui.Text(repos).FontSize(12).TextColor(t.TextMuted).Truncate())).
			Child(ui.Text(st.text).FontSize(12).TextColor(st.color).NoWrap())
	})
}

// landHoverAll shows every pill row as if hovered; the shots set it to show the actions.
var landHoverAll = false

// prIconOf is the thread's one pull request as an icon button; nil without one.
func prIconOf(ctx *ui.Context, th core.ThreadView) ui.Element {
	if th.PR == nil || th.PR.State != forge.StateOpen {
		return nil
	}
	url := th.PR.URL
	name := fmt.Sprintf("%s #%d", th.PR.Kind.Noun(), th.PR.Number)
	return ui.IconButton(icons.ExternalLink).Ghost().Small().Label("Open " + name + " in your browser").
		Tooltip("Open " + name + " in your browser").OnClick(func() { _ = ctx.OpenURL(url) })
}

// ---- Direction C: the stack ----

// stack floats the cards at the top right of the chat as one-line rows with every action showing.
// Past stackRows the rest fold under "N more". It folds into a round badge, and starts folded when
// the chat is too narrow for it to leave the conversation readable.
func (a *App) stack(ctx *ui.Context, cards []openCard) ui.Element {
	t := ctx.Theme()
	area := a.chatAreaWidth(ctx)
	open := a.landIsOpen(area >= stackMinChat)
	wrap := ui.Div().Absolute().Top(space3).Right(space4).ZIndex(2).Column().Align(ui.End)
	if !open {
		label := fmt.Sprintf("%d to land", len(cards))
		const size = 36
		btn := ui.Div().Size(ui.Px(size), ui.Px(size)).Radius(size/2).Center().Shrink(0).
			Background(t.Surface).Border(1, t.BorderStrong).Shadow(ui.ShadowMd).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label(label).Tooltip(label).TabIndex(0).AccessExpanded(false).Key("land-stack-badge").
			OnClick(func() { a.setLandOpen(true) }).
			Child(ui.Icon(icons.GitMerge).Size(16).Color(t.Success)).
			Child(ui.Div().Absolute().Top(-5).Right(-5).Height(ui.Px(18)).MinWidth(ui.Px(18)).PaddingX(5).Radius(9).Center().
				Background(t.Success).AccessHidden().
				Child(ui.Text(fmt.Sprint(len(cards))).FontSize(11).Semibold().TextColor(t.Background).NoWrap()))
		return wrap.Child(btn)
	}
	w := min(float32(stackW), area-2*space4)
	more := a.landMore.Get()
	shown := cards
	if !more && len(cards) > stackRows+1 {
		shown = cards[:stackRows]
	}
	list := ui.Div().Column()
	for _, c := range shown {
		list = list.Child(a.stackRow(ctx, c, w))
	}
	_, h := ctx.WindowSize()
	head := ui.Div().Row().Align(ui.Center).Gap(6).PaddingLeft(space3).PaddingRight(space1).Height(ui.Px(32)).Shrink(0).
		Child(ui.Icon(icons.GitMerge).Size(14).Color(t.Success)).
		Child(ui.Text(fmt.Sprintf("%d to land", len(cards))).FontSize(12).Semibold().TextColor(t.TextMuted)).
		Child(ui.Div().Grow(1)).
		Child(ui.IconButton(icons.Minimize2).Ghost().Small().Label("Fold").Tooltip("Fold into a badge").Key("land-stack-fold").
			OnClick(func() { a.setLandOpen(false) }))
	panel := ui.Div().Column().Width(ui.Px(w)).Radius(radiusL).Background(t.Surface).Border(1, t.BorderStrong).
		Shadow(ui.ShadowLg).ClipContent().Key("land-stack").Label("Merge cards to land").
		Child(head).
		Child(ui.Div().Column().MaxHeight(ui.Px(min(400, h-300))).ScrollY().BorderEdges(ui.Edges{Top: 1}, t.Border).Child(list))
	if len(cards) > stackRows+1 {
		text := fmt.Sprintf("%d more", len(cards)-len(shown))
		icon := icons.ChevronDown
		if more {
			text, icon = "Show fewer", icons.ChevronUp
		}
		panel = panel.Child(ui.Div().Row().Align(ui.Center).Justify(ui.Center).Gap(4).Height(ui.Px(30)).Shrink(0).
			BorderEdges(ui.Edges{Top: 1}, t.Border).Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label(text).TabIndex(0).Key("land-stack-more").
			OnClick(func() { a.landMore.Set(!more) }).
			Child(ui.Text(text).FontSize(12).Medium().TextColor(t.TextMuted)).
			Child(ui.Icon(icon).Size(13).Color(t.TextMuted)))
	}
	return wrap.Child(panel)
}

// stackRow is one card as a line with its actions: where it stands, its title, its pull request with
// its checks, the landing button, Review changes, and "…" (Request changes and the card's menu).
func (a *App) stackRow(ctx *ui.Context, c openCard, w float32) ui.Element {
	t := ctx.Theme()
	th := c.th
	id := th.ID
	st := stateOf(ctx, a, th)
	land := a.landOf(th)
	// Where it stands, in words, beside its pull request: "#88 · 1 of 6 failed", or "No PR yet".
	where := ui.Div().Row().Align(ui.Center).Gap(4).Shrink(0).Tooltip(st.tip)
	if pr := prLinkOf(ctx, th, "stack-pr-"+id); pr != nil {
		where = where.Child(pr)
	}
	where = where.Child(ui.Text(st.text).FontSize(12).TextColor(st.color).NoWrap())
	return ui.Div().Row().Align(ui.Center).Gap(6).Height(ui.Px(40)).PaddingLeft(space3).PaddingRight(space1).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).Key("stack-row-" + id).
		Child(ui.Div().Size(ui.Px(14), ui.Px(14)).Center().Shrink(0).Tooltip(st.tip).Child(ui.Icon(st.icon).Size(14).Color(st.color))).
		Child(a.titleLink(ctx, th, 13, "stack-title-"+id).Grow(1)).
		Child(where).
		Child(landButtonOf(ctx, land, "stack-land-"+id, true)).
		Child(ui.IconButton(icons.FileDiff).Ghost().Small().Label("Review changes").Tooltip("Review changes").
			OnClick(func() { a.openThread(id, "changes") })).
		Child(a.moreButton(ctx, th, menuParts{request: true}, "stack-more-"+id))
}
