package gui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// statusOrder sorts the rail: what needs the user first, then work in progress, then results.
var statusOrder = map[string]int{
	core.StatusWaiting: 0, core.StatusRunning: 1, core.StatusStarting: 2, core.StatusIdle: 3,
	core.StatusFailed: 4, core.StatusQueued: 5, core.StatusInterrupted: 6, core.StatusMerged: 7, core.StatusDiscarded: 8,
}

// rail lists the worker threads, filtered by the sidebar's repository.
func (a *App) rail(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	filter := a.repoFilter()
	var open, closed []core.ThreadView
	running, waiting := 0, 0
	for _, th := range a.threads.Get() {
		if !th.IsWorker || (filter != "" && th.RepoPath != filter) {
			continue
		}
		if core.Finished(th.Status) {
			closed = append(closed, th)
			continue
		}
		open = append(open, th)
		switch th.Status {
		case core.StatusRunning, core.StatusStarting:
			running++
		case core.StatusWaiting:
			waiting++
		}
	}
	slices.SortStableFunc(open, func(x, y core.ThreadView) int {
		if d := statusOrder[x.Status] - statusOrder[y.Status]; d != 0 {
			return d
		}
		return y.UpdatedAt.Compare(x.UpdatedAt)
	})
	slices.SortFunc(closed, func(x, y core.ThreadView) int { return y.UpdatedAt.Compare(x.UpdatedAt) })

	var summary []string
	if running > 0 {
		summary = append(summary, fmt.Sprintf("%d working", running))
	}
	if waiting > 0 {
		summary = append(summary, fmt.Sprintf("%d need you", waiting))
	}
	if len(summary) == 0 {
		summary = append(summary, fmt.Sprintf("%d open", len(open)))
	}
	headTitle := "Threads"
	if filter != "" {
		headTitle = filepath.Base(filter)
	}
	head := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space4).Height(ui.Px(52)).Shrink(0).
		Child(ui.Text(headTitle).FontSize(14).Semibold().Truncate().Grow(1).MinWidth(ui.Px(0))).
		Child(caption(ctx, strings.Join(summary, " · ")))

	list := ui.Div().Column().Gap(space2).PaddingX(space3).PaddingBottom(space3)
	if len(open) == 0 {
		list = list.Child(ui.Div().Column().Align(ui.Center).Gap(space2).PaddingY(space8).PaddingX(space4).
			Child(ui.Icon(icons.Layers).Size(22).Color(t.TextSubtle)).
			Child(caption(ctx, "No open threads. Ask the orchestrator for something and its workers show up here.").Align(ui.Center)))
	}
	for _, th := range open {
		list = list.Child(a.threadCard(ctx, th))
	}
	if len(closed) > 0 {
		list = list.Child(ui.Component(func(ctx *ui.Context) ui.Element {
			show := ui.Local(ctx, "show-closed", false)
			col := ui.Div().Column().Gap(space2).MarginTop(space2).
				Child(ui.Div().Row().Align(ui.Center).Gap(space1).Cursor(ui.CursorPointer).PaddingX(space1).
					Role(access.RoleButton).Label("Closed threads").TabIndex(0).
					OnClick(func() { show.Update(func(b bool) bool { return !b }) }).
					Child(ui.Icon(map[bool]*icons.Icon{true: icons.ChevronDown, false: icons.ChevronRight}[show.Get()]).Size(13).Color(t.TextSubtle)).
					Child(subtle(ctx, fmt.Sprintf("Closed (%d)", len(closed)))))
			if show.Get() {
				for _, th := range closed {
					col = col.Child(a.threadCard(ctx, th))
				}
			}
			return col
		}))
	}
	return ui.Div().Column().Width(ui.Px(railWidth)).Shrink(0).Height(ui.Full).Background(t.Panel).
		Child(head).
		Child(ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).ScrollY().Child(list))
}

// threadCard is one thread in the rail.
func (a *App) threadCard(ctx *ui.Context, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	c := statusColor(ctx, th.Status)
	activity, actColor := a.activityLine(th), t.TextMuted
	switch th.Status {
	case core.StatusWaiting:
		actColor = t.Warning
	case core.StatusFailed:
		actColor = t.Danger
	}
	when := shortAgo(th.UpdatedAt)
	card := ui.Div().Column().Gap(6).Padding(space3).Radius(radiusL).Background(t.Background).
		Border(1, t.Border).Cursor(ui.CursorPointer).
		Hover(ui.Style{Background: t.SurfaceHover, BorderColor: t.BorderStrong}).
		Role(access.RoleButton).Label(th.Title).TabIndex(0).Key("card-" + th.ID).
		OnClick(func() { a.openThread(th.ID, "conversation") }).
		Child(ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(dot(c, 8)).
			Child(ui.Text(th.Title).FontSize(13).Semibold().Truncate().Grow(1).MinWidth(ui.Px(0))).
			Child(subtle(ctx, when).NoWrap())).
		Child(ui.Div().Row().Align(ui.Center).Gap(6).PaddingLeft(space4).
			Child(ui.Text(filepath.Base(th.RepoPath)).FontSize(12).Medium().TextColor(t.TextMuted).NoWrap()).
			Child(ui.Text(branchLabel(th)).FontSize(12).TextColor(t.TextSubtle).TruncateHead().MinWidth(ui.Px(0)))).
		Child(ui.Div().Row().Align(ui.Start).Gap(6).PaddingLeft(space4).
			Child(ui.Text(activity).FontSize(12).TextColor(actColor).MaxLines(2).Grow(1).MinWidth(ui.Px(0))))
	if th.Status == core.StatusWaiting {
		card = card.Border(1, t.Warning.Alpha(0.5))
	}
	if th.CostUSD > 0 || th.Status == core.StatusIdle {
		foot := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingLeft(space4)
		foot = foot.Child(ui.Text(statusLabel(th)).FontSize(11).Medium().TextColor(c))
		if th.CostUSD > 0 {
			foot = foot.Child(subtle(ctx, fmt.Sprintf("$%.2f", th.CostUSD)).FontSize(11))
		}
		card = card.Child(foot)
	}
	return card
}

func branchLabel(th core.ThreadView) string {
	if th.Branch == "" {
		return ""
	}
	return strings.TrimPrefix(th.Branch, "solo/")
}

// activityLine says what a thread is doing or did, in a line or two.
func (a *App) activityLine(th core.ThreadView) string {
	switch th.Status {
	case core.StatusQueued:
		return "Waiting for a free worker slot"
	case core.StatusStarting:
		return "Creating its worktree…"
	case core.StatusWaiting:
		if qs := a.pending.Get()[th.ID]; len(qs) > 0 && len(qs[0].Questions) > 0 {
			return "Asks: " + qs[0].Questions[0].Question
		}
		return "Needs your answer"
	case core.StatusFailed:
		return firstLine(th.Error)
	case core.StatusRunning:
		if th.Retry != "" {
			return th.Retry
		}
		for i := len(th.Live) - 1; i >= 0; i-- {
			if !th.Live[i].Thinking {
				if l := lastLine(th.Live[i].Text); l != "" {
					return l
				}
			}
		}
		return "Working…"
	case core.StatusInterrupted:
		if th.Error != "" {
			return th.Error
		}
		return "Stopped"
	}
	if th.Summary != "" {
		return firstLine(stripMarkdown(th.Summary))
	}
	return firstLine(th.Prompt)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return stripMarkdown(l)
		}
	}
	return ""
}

// stripMarkdown removes the emphasis and code marks of a one-line summary.
func stripMarkdown(s string) string {
	r := strings.NewReplacer("**", "", "`", "", "__", "", "# ", "")
	return strings.TrimLeft(r.Replace(s), "-* ")
}

// shortAgo is a compact time since t: "now", "4m", "3h", "2d".
func shortAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
