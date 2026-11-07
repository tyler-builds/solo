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

// railGroups orders the rail: what needs the user first, then work in progress, then results.
var railGroups = []struct {
	title    string
	statuses []string
}{
	{"Needs you", []string{core.StatusWaiting}},
	{"Failed", []string{core.StatusFailed}},
	{"Ready for review", []string{core.StatusIdle}},
	{"Working", []string{core.StatusRunning, core.StatusStarting}},
	{"Queued", []string{core.StatusQueued}},
	{"Stopped", []string{core.StatusInterrupted}},
}

// rail lists the worker threads, grouped by state and filtered by the sidebar's repository.
func (a *App) rail(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	filter := a.repoFilter()
	var open, closed []core.ThreadView
	running := false
	for _, th := range a.threads.Get() {
		if !th.IsWorker || (filter != "" && th.RepoPath != filter) {
			continue
		}
		if core.Finished(th.Status) {
			closed = append(closed, th)
			continue
		}
		open = append(open, th)
		running = running || th.InTurn
	}
	// Elapsed times tick while anything works.
	if running {
		tick := ui.Local(ctx, "tick", 0)
		tick.Get()
		ctx.After(time.Second, func() { tick.Update(func(n int) int { return n + 1 }) })
	}

	headTitle := "Threads"
	if filter != "" {
		headTitle = filepath.Base(filter)
	}
	need := 0
	for _, th := range open {
		if needsUser(th) {
			need++
		}
	}
	head := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space4).Height(ui.Px(44)).Shrink(0).
		Child(ui.Text(headTitle).FontSize(14).Semibold().Truncate().Grow(1).MinWidth(ui.Px(0)))
	if need > 0 {
		head = head.Child(ui.Text(fmt.Sprintf("%d need you", need)).FontSize(12).Medium().TextColor(t.Warning))
	}
	head = head.Child(subtle(ctx, fmt.Sprintf("%d open", len(open))))

	list := ui.Div().Column().Gap(space1).PaddingX(space2).PaddingBottom(space3)
	if len(open) == 0 {
		list = list.Child(ui.Div().Column().Align(ui.Center).Gap(space2).PaddingY(space8).PaddingX(space4).
			Child(ui.Icon(icons.Layers).Size(22).Color(t.TextSubtle)).
			Child(caption(ctx, "No open threads. Ask the orchestrator for something and its workers show up here.").Align(ui.Center)))
	}
	for _, g := range railGroups {
		var group []core.ThreadView
		for _, th := range open {
			if slices.Contains(g.statuses, th.Status) {
				group = append(group, th)
			}
		}
		if len(group) == 0 {
			continue
		}
		slices.SortStableFunc(group, func(x, y core.ThreadView) int { return y.UpdatedAt.Compare(x.UpdatedAt) })
		list = list.Child(groupHeader(ctx, g.title, len(group), g.statuses[0]))
		for _, th := range group {
			list = list.Child(a.threadRow(ctx, th))
		}
	}
	if len(closed) > 0 {
		slices.SortFunc(closed, func(x, y core.ThreadView) int { return y.UpdatedAt.Compare(x.UpdatedAt) })
		list = list.Child(ui.Component(func(ctx *ui.Context) ui.Element {
			show := ui.Local(ctx, "show-closed", false)
			col := ui.Div().Column().Gap(space1).
				Child(ui.Div().Row().Align(ui.Center).Gap(space1).Cursor(ui.CursorPointer).
					PaddingX(space2).PaddingTop(space4).PaddingBottom(space1).
					Role(access.RoleButton).Label("Closed threads").TabIndex(0).
					OnClick(func() { show.Update(func(b bool) bool { return !b }) }).
					Child(ui.Text(fmt.Sprintf("Closed · %d", len(closed))).FontSize(11).Medium().TextColor(ctx.Theme().TextSubtle)).
					Child(ui.Icon(map[bool]*icons.Icon{true: icons.ChevronDown, false: icons.ChevronRight}[show.Get()]).Size(12).Color(ctx.Theme().TextSubtle)))
			if show.Get() {
				for _, th := range closed {
					col = col.Child(a.threadRow(ctx, th))
				}
			}
			return col
		}))
	}
	return ui.Div().Column().Size(ui.Full, ui.Full).Background(t.Panel).
		Child(head).
		Child(ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).ScrollY().Child(list))
}

// needsUser reports whether a thread waits on the user: a question, a failure, or work to review.
func needsUser(th core.ThreadView) bool {
	return th.Status == core.StatusWaiting || th.Status == core.StatusFailed || (th.Status == core.StatusIdle && th.CanMerge)
}

func groupHeader(ctx *ui.Context, title string, n int, status string) ui.Element {
	t := ctx.Theme()
	c := t.TextSubtle
	if status == core.StatusWaiting {
		c = t.Warning
	}
	return ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).PaddingTop(space3).PaddingBottom(space1).
		Child(ui.Text(title).FontSize(11).Medium().TextColor(c)).
		Child(ui.Text(fmt.Sprint(n)).FontSize(11).TextColor(t.TextSubtle))
}

// threadRow is one thread in the rail: its mark, title and time, and one line of what it is doing.
func (a *App) threadRow(ctx *ui.Context, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	selected := a.openID.Get() == th.ID
	when, whenColor := shortAgo(th.UpdatedAt), t.TextSubtle
	if th.InTurn && !th.TurnStarted.IsZero() {
		when, whenColor = elapsed(th.TurnStarted), ui.Hex(pal(t).brand)
	}
	// The mark and the group carry the state; the line stays neutral.
	line, lineColor := a.activityLine(th), t.TextMuted
	repo := filepath.Base(th.RepoPath)
	row := ui.Div().Column().Gap(3).PaddingX(space2 + 2).PaddingY(space2).Radius(radiusM).
		Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
		Role(access.RoleButton).Label(th.Title).TabIndex(0).Key("card-" + th.ID).
		OnClick(func() {
			if a.openID.Peek() == th.ID {
				a.closeThread()
			} else {
				a.openThread(th.ID, "conversation")
			}
		}).
		Child(ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(statusMark(ctx, th.Status)).
			Child(ui.Text(th.Title).FontSize(13).Medium().Truncate().Grow(1).MinWidth(ui.Px(0))).
			Child(ui.Text(when).FontSize(11).TextColor(whenColor).NoWrap())).
		Child(ui.Div().Row().Align(ui.Center).Gap(6).PaddingLeft(space5 + 2).MinWidth(ui.Px(0)).
			Child(ui.Text(repo).FontSize(12).TextColor(t.TextSubtle).NoWrap()).
			Child(ui.Text("·").FontSize(12).TextColor(t.TextSubtle)).
			Child(ui.Text(line).FontSize(12).TextColor(lineColor).Truncate().Grow(1).MinWidth(ui.Px(0))))
	if selected {
		row = row.Background(t.SurfaceActive)
	}
	return row
}

func branchLabel(th core.ThreadView) string {
	if th.Branch == "" {
		return ""
	}
	return strings.TrimPrefix(th.Branch, "solo/")
}

// activityLine says what a thread is doing or did, in a line.
func (a *App) activityLine(th core.ThreadView) string {
	switch th.Status {
	case core.StatusQueued:
		return "Waiting for a free worker slot"
	case core.StatusStarting:
		return "Creating its worktree"
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
		if th.Step != nil {
			return a.stepLabel(*th.Step)
		}
		for i := len(th.Live) - 1; i >= 0; i-- {
			if !th.Live[i].Thinking {
				if l := lastLine(th.Live[i].Text); l != "" {
					return l
				}
			}
		}
		return "Thinking"
	case core.StatusInterrupted:
		return "Stopped"
	}
	if th.Summary != "" {
		return firstLine(stripMarkdown(th.Summary))
	}
	return firstLine(th.Prompt)
}

// stepLabel says what a tool call in progress is doing: "Running pnpm test".
func (a *App) stepLabel(d core.ToolData) string {
	_, verb, detail := a.describeTool(d)
	if p, ok := presentTense[verb]; ok {
		verb = p
	}
	if detail == "" {
		return verb
	}
	return verb + " " + detail
}

var presentTense = map[string]string{
	"Read": "Reading", "Edited": "Editing", "Wrote": "Writing", "Ran": "Running", "Searched": "Searching",
	"Found files": "Finding files", "Fetched": "Fetching", "Searched the web": "Searching the web",
	"Updated the plan": "Planning", "Started thread": "Starting thread", "Messaged": "Messaging",
	"Checked": "Checking", "Read transcript": "Reading", "Reviewed changes": "Reviewing changes of",
	"Listed threads": "Listing threads", "Listed repositories": "Listing repositories",
	"Stopped": "Stopping", "Answered": "Answering", "Proposed merge": "Proposing a merge of",
	"Loaded tools": "Loading tools", "Subagent": "Running subagent",
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
