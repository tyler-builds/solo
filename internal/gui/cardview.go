package gui

// The card view: a board card opened over the board. It shows what the user last asked and the
// lead's review of what came of it (or, while the agents work, what they are doing), with a box to
// reply to the lead. The builder's transcript, the lead's notes and the diff are a tab away, for when
// the user wants to dig in.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/launch"
)

// openCard shows a board card over the board.
func (a *App) openCard(id string) {
	a.cardTab.Set("outcome")
	a.cardOpen.Set(id)
}

func (a *App) closeCard() { a.cardOpen.Set("") }

// isCard reports whether a thread is a board card's builder, which opens as a card.
func isCard(th core.ThreadView) bool { return th.Card != nil && th.Card.Lead != "" }

// cardPhaseLine says what a card's agents are doing, for a card at work.
func cardPhaseLine(th core.ThreadView) string {
	if th.Card == nil {
		return ""
	}
	switch th.Card.Phase {
	case core.CardPlanning:
		return "Solo is reading the code and writing the brief"
	case core.CardReviewing:
		return "Solo is reviewing the work"
	case core.CardAnswering:
		return "Solo is answering the builder's question"
	case core.CardReplying:
		return "Solo is reading your reply"
	case core.CardBuilding:
		if th.Card.Rounds > 0 {
			return fmt.Sprintf("Fixing what Solo found (round %d)", th.Card.Rounds)
		}
	}
	return ""
}

// reviewLines are a card's review in a few plain lines, for the board: at most three.
func reviewLines(th core.ThreadView) []string {
	if th.Card == nil || th.Card.Review == "" {
		return nil
	}
	var out []string
	needs := ""
	for _, l := range strings.Split(stripMarkdown(th.Card.Review), "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-•*"))
		switch {
		case l == "":
		case strings.HasPrefix(l, "Needs you:"):
			needs = l // what the user must decide always shows
		default:
			out = append(out, l)
		}
	}
	keep := 3
	if needs != "" {
		keep = 2
	}
	if len(out) > keep {
		out = out[:keep]
	}
	if needs != "" {
		out = append(out, needs)
	}
	return out
}

// cardOverlay is the open card, over the board.
func (a *App) cardOverlay(ctx *ui.Context) ui.Element {
	id := a.cardOpen.Get()
	if id == "" {
		return nil
	}
	th, ok := a.thread(id)
	if !ok || th.Card == nil {
		return nil
	}
	t := ctx.Theme()
	winW, winH := ctx.WindowSize()
	w := min(920, winW-2*space4)
	h := min(winH*0.86, 900) - 120 // the modal's title and padding take the rest
	ticking(ctx, th.InTurn || th.Card.CardWorking())

	col := boardColumn(th, a.busy.Get()[th.ID])
	head := ui.Div().Row().Wrap().Align(ui.Center).Gap(space2).Shrink(0)
	head = head.Child(statusMark(ctx, shownStatus(th)))
	if reason, c := cardReason(ctx, th, col); reason != "" {
		head = head.Child(reasonChip(reason, c))
	} else if line := cardPhaseLine(th); line != "" || th.Card.CardWorking() {
		head = head.Child(reasonChip("Working", workingColor(t)))
	}
	meta := []string{repoLabel(th)}
	if th.Changed > 0 {
		meta = append(meta, plural(th.Changed, "file"))
	}
	if prs := cardPRs(th); len(prs) == 1 {
		meta = append(meta, fmt.Sprintf("%s #%d", prs[0].Kind.Noun(), prs[0].Number))
	}
	head = head.Child(ui.Text(strings.Join(meta, " · ")).FontSize(12).TextColor(t.TextMuted).NoWrap()).
		Child(ui.Div().Grow(1))
	if !th.Card.CardWorking() {
		if land := a.landAction(ctx, th, true); land != nil {
			head = head.Child(land)
		}
		if th.CanDone && !asksToLand(th) {
			head = head.Child(ui.Button("Done").Icon(icons.Check).Small().Secondary().Key("card-view-done").
				OnClick(func() { a.closeCard(); a.markDone(th) }))
		}
	}
	head = head.Child(ui.DropdownMenu(
		ui.IconButton(icons.Ellipsis).Ghost().Small().Label("More actions").Tooltip("More"),
		func() []ui.MenuItem { return a.cardViewMenu(th) }))

	tab := a.cardTab.Get()
	tabs := []ui.Tab[string]{{Value: "outcome", Label: "Outcome"}, {Value: "history", Label: "History"}}
	if th.HasWorktree {
		label := "Changes"
		if th.Changed > 0 {
			label = fmt.Sprintf("Changes · %d", th.Changed)
		}
		tabs = append(tabs, ui.Tab[string]{Value: "changes", Label: label})
	}
	tabRow := ui.Div().Row().Shrink(0).BorderEdges(ui.Edges{Bottom: 1}, t.Border).Child(ui.Tabs(a.cardTab, tabs...))

	var content ui.Element
	switch tab {
	case "history":
		content = a.cardHistory(ctx, th)
	case "changes":
		content = a.changesView(ctx, th, a.changes(ctx, th))
	default:
		content = ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).ScrollY().PaddingY(space3).Child(a.cardOutcome(ctx, th))
	}
	reply := ui.Div().Shrink(0).PaddingTop(space2).Child(a.cardReplyBox(ctx, th))
	body := ui.Div().Column().Gap(space2).Width(ui.Full).Height(ui.Px(h)).
		Child(head).Child(tabRow).
		Child(ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).Child(content)).
		Child(reply)
	return ui.Modal(th.Title, body).Width(ui.Px(w)).MaxWidth(ui.Px(w)).CloseButton(true).CloseOnBackdrop().
		OnClose(a.closeCard).Key("card-view")
}

// cardOutcome is what the user last asked, and what came of it.
func (a *App) cardOutcome(ctx *ui.Context, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	cs := th.Card
	now := a.now()
	section := func(label, when string) ui.Element {
		row := ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(ui.Text(label).FontSize(11).Semibold().TextColor(t.TextSubtle).NoWrap())
		if when != "" {
			row = row.Child(ui.Text("· " + when).FontSize(11).TextColor(t.TextSubtle).NoWrap())
		}
		return row
	}
	out := ui.Div().Column().Gap(space4).PaddingX(space1)

	ask := ui.Div().Column().Gap(space2).
		Child(section("YOU ASKED", agoText(cs.AskAt, now))).
		Child(ui.Div().Padding(space3).Radius(radiusL).Background(t.Panel).
			Child(a.md(ctx, hardBreaks(cs.Ask), viewBases(th), nil)))
	var imgs, files []string
	for _, f := range cs.AskFiles {
		if core.IsFileAttachment(f) {
			files = append(files, f)
		} else {
			imgs = append(imgs, f)
		}
	}
	if len(imgs)+len(files) > 0 {
		strip := ui.Div().Row().Wrap().Gap(space2)
		all := func() []string { return imgs }
		for _, p := range imgs {
			strip = strip.Child(a.thumb(ctx, p, 64, all, nil))
		}
		for _, p := range files {
			name, _ := core.FileName(p)
			strip = strip.Child(chip(ctx, name))
		}
		ask = ask.Child(strip)
	}
	out = out.Child(ask)

	solo := ui.Div().Column().Gap(space2)
	if cs.CardWorking() {
		solo = solo.Child(section("SOLO", ""))
		line := cardPhaseLine(th)
		if line == "" {
			line = a.nowLine(th)
		}
		box := ui.Div().Column().Gap(space2).Padding(space3).Radius(radiusL).Border(1, t.Border).
			Child(ui.Div().Row().Align(ui.Center).Gap(space2).
				Child(statusMark(ctx, core.StatusRunning)).
				Child(ui.Text(line).FontSize(13).Medium().TextColor(workingColor(t)).Grow(1).MinWidth(ui.Px(0))))
		if cs.Phase == core.CardBuilding {
			for _, l := range briefLines(th) {
				box = box.Child(ui.Text("• " + l).FontSize(12).TextColor(t.TextMuted))
			}
			if th.Now != "" && line != th.Now {
				box = box.Child(caption(ctx, "Now: "+th.Now))
			}
		}
		if cs.Review != "" {
			box = box.Child(ui.Div().Column().Gap(space1).PaddingTop(space2).BorderEdges(ui.Edges{Top: 1}, t.Border).
				Child(caption(ctx, "Solo's last review")).
				Child(a.md(ctx, cs.Review, viewBases(th), nil)))
		}
		solo = solo.Child(box)
	} else {
		solo = solo.Child(section("SOLO'S REVIEW", agoText(cs.ReviewAt, now)))
		review := cs.Review
		if review == "" {
			review = "No review yet."
		}
		solo = solo.Child(ui.Div().Column().Gap(space2).Padding(space3).Radius(radiusL).Border(1, t.Border).
			Child(a.md(ctx, review, viewBases(th), nil)))
	}
	if th.Status == core.StatusWaiting && !cs.CardWorking() {
		if line := a.activityLine(th); line != "" {
			solo = solo.Child(caption(ctx, "The builder is waiting: "+strings.TrimPrefix(line, "Asks: ")+" Reply below, and Solo answers it."))
		}
	}
	return out.Child(solo)
}

// cardHistory is the builder's transcript, or the lead's notes.
func (a *App) cardHistory(ctx *ui.Context, th core.ThreadView) ui.Element {
	whose := ui.Local(ctx, "card-history-"+th.ID, "builder")
	id := th.ID
	v := th
	leadID := map[string]string{"briefer": th.Card.Lead, "reviewer": th.Card.Reviewer}[whose.Get()]
	if lead, ok := a.thread(leadID); ok && leadID != "" {
		id, v = lead.ID, lead
	}
	opts := []ui.Option[string]{{Value: "builder", Label: "Builder"}, {Value: "briefer", Label: "Briefer"}}
	if th.Card.Reviewer != "" {
		opts = append(opts, ui.Option[string]{Value: "reviewer", Label: "Reviewer"})
	}
	pick := ui.Div().Row().PaddingY(space2).Shrink(0).Child(ui.ToggleGroup(whose, opts...).
		Small().Label("Whose history").Key("card-history-pick"))
	items := a.itemsOf(id).Get()
	return ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).
		Child(pick).
		Child(ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).Child(a.transcript(v, items, "card-"+id)))
}

// cardReplyBox is the box that replies to the card's lead.
func (a *App) cardReplyBox(ctx *ui.Context, th core.ThreadView) ui.Element {
	if core.Finished(th.Status) || !th.DoneAt.IsZero() {
		return caption(ctx, "This card is "+strings.ToLower(statusLabel(th))+".")
	}
	if th.Status == core.StatusQueued || th.Status == core.StatusStarting {
		return caption(ctx, "This card is starting.")
	}
	id := th.ID
	return a.composer(ctx, th, "card-draft-"+id, "card-"+id, "Reply to Solo…", func(text string, images []string) error {
		return a.b.Send(context.Background(), id, text, images...)
	})
}

// cardViewMenu is the card's More menu.
func (a *App) cardViewMenu(th core.ThreadView) []ui.MenuItem {
	id := th.ID
	var items []ui.MenuItem
	if th.CanInterrupt {
		items = append(items, ui.MenuItem{Label: "Stop the builder", Icon: icons.Square, OnClick: func() {
			a.run("Stopping", func(ctx context.Context) error { return a.b.Interrupt(ctx, id) })
		}})
	}
	for _, lid := range []string{th.Card.Lead, th.Card.Reviewer} {
		if lead, ok := a.thread(lid); ok && lid != "" && lead.CanInterrupt {
			items = append(items, ui.MenuItem{Label: "Stop Solo", Icon: icons.Square, OnClick: func() {
				a.run("Stopping", func(ctx context.Context) error { return a.b.Interrupt(ctx, lid) })
			}})
		}
	}
	items = append(items, ui.MenuItem{Label: "Open as a thread", Icon: icons.PanelRight, OnClick: func() {
		a.closeCard()
		a.openThread(id, "conversation")
	}})
	if th.Dir != "" {
		dir := th.Dir
		if launch.Available {
			items = append(items,
				ui.MenuItem{Label: "Open in editor", Icon: icons.SquareCode, OnClick: func() { a.openEditor(dir) }},
				ui.MenuItem{Label: "Open terminal", Icon: icons.SquareTerminal, OnClick: func() { a.openTerminal(dir) }})
		}
		items = append(items, ui.MenuItem{Label: "Copy folder path", Icon: icons.Copy, OnClick: func() { a.copyText("the folder path", dir) }})
	}
	if th.CanDone && asksToLand(th) {
		items = append(items, ui.MenuItem{Label: "Mark done…", Icon: icons.Check, OnClick: func() { a.closeCard(); a.markDone(th) }})
	}
	if th.CanDiscard {
		items = append(items, ui.MenuItem{Label: "Discard card…", Icon: icons.Trash, OnClick: func() { a.closeCard(); a.confirmDiscard(th) }})
	}
	return items
}

// agoText says how long ago t was: "2m ago", or "" for a zero time.
func agoText(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	if s := shortAgo(t, now); s != "now" {
		return s + " ago"
	}
	return "just now"
}
