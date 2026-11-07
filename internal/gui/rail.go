package gui

import (
	"fmt"
	"github.com/tyler-builds/solo/internal/forge"
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
	{"Needs answer", []string{core.StatusWaiting}},
	{"Failed", []string{core.StatusFailed, statusChecksFailed}},
	{"Ready", []string{core.StatusIdle}},
	{"In review", []string{statusInReview}},
	{"No changes", []string{statusNoChanges}},
	{"Merged", []string{core.StatusMerged}},
	{"Stopped", []string{core.StatusInterrupted}},
	// Last, and pinned to the bottom of the rail: work in progress needs nothing from the user.
	{"Working", []string{core.StatusRunning, core.StatusStarting, core.StatusQueued}},
}

// inRail reports whether a thread shows in the rail: a worker the user hasn't marked done.
func inRail(th core.ThreadView) bool { return th.IsWorker && th.DoneAt.IsZero() }

// rail lists the worker threads, grouped by state and filtered by the sidebar's repository.
func (a *App) rail(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	filter := a.repoFilter()
	var open []core.ThreadView
	running := false
	for _, th := range a.threads.Get() {
		if !inRail(th) || (filter != "" && th.RepoPath != filter) {
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

	need := 0
	for _, th := range open {
		if needsUser(th) {
			need++
		}
	}
	head := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space4).Height(ui.Px(44)).Shrink(0).
		Child(ui.Text("Threads").FontSize(14).Semibold().NoWrap()).
		Child(ui.Div().Column().Justify(ui.Center).Shrink(1).MinWidth(ui.Px(0)).Child(a.repoMenu(ctx))).
		Child(ui.Div().Grow(1))
	if need > 0 {
		label := fmt.Sprintf("%d need you", need)
		if need == 1 {
			label = "1 needs you"
		}
		head = head.Child(ui.Text(label).FontSize(12).Medium().TextColor(t.Warning).NoWrap())
	}

	list := ui.Div().Column().Gap(space1).PaddingX(space2).PaddingBottom(space3)
	if len(open) == 0 {
		list = list.Child(ui.Div().Column().Align(ui.Center).Gap(space2).PaddingY(space8).PaddingX(space4).
			Child(ui.Icon(icons.Layers).Size(22).Color(t.TextSubtle)).
			Child(caption(ctx, "No open threads. Ask Solo for something and its threads show up here.").Align(ui.Center)))
	}
	var working ui.Element
	for gi, g := range railGroups {
		var group []core.ThreadView
		for _, th := range open {
			if slices.Contains(g.statuses, shownStatus(th)) {
				group = append(group, th)
			}
		}
		if len(group) == 0 {
			continue
		}
		slices.SortStableFunc(group, func(x, y core.ThreadView) int { return y.UpdatedAt.Compare(x.UpdatedAt) })
		if gi == len(railGroups)-1 {
			col := ui.Div().Column().Gap(space1).PaddingX(space2).PaddingBottom(space2).Opacity(0.72).
				Hover(ui.Style{Opacity: 1}).Transition(150 * time.Millisecond).
				Child(groupHeader(ctx, g.title, len(group), g.statuses[0]))
			for _, th := range group {
				col = col.Child(a.threadRow(ctx, th))
			}
			working = ui.Div().Column().Shrink(0).MaxHeight(ui.Percent(45)).ScrollY().
				BorderEdges(ui.Edges{Top: 1}, t.Border).Child(col)
			continue
		}
		list = list.Child(groupHeader(ctx, g.title, len(group), g.statuses[0]))
		for _, th := range group {
			list = list.Child(a.threadRow(ctx, th))
		}
	}
	rail := ui.Div().Column().Size(ui.Full, ui.Full).Background(t.Panel).
		Child(head).
		Child(ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).ScrollY().Child(list))
	if working != nil {
		rail = rail.Child(working)
	}
	return rail
}

// repoMenu narrows the rail to one repository. It shows only when the threads span several, or a
// filter is on.
func (a *App) repoMenu(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	filter := a.repoFilter()
	counts := map[string]int{}
	var repos []string
	for _, th := range a.threads.Get() {
		if th.IsWorker && !core.Finished(th.Status) {
			if counts[th.RepoPath] == 0 {
				repos = append(repos, th.RepoPath)
			}
			counts[th.RepoPath]++
		}
	}
	if len(repos) < 2 && filter == "" {
		return ui.Div()
	}
	slices.SortFunc(repos, func(x, y string) int { return strings.Compare(filepath.Base(x), filepath.Base(y)) })
	label := "All repos"
	if railW(ctx) < railWidth {
		label = "All"
	}
	if filter != "" {
		label = filepath.Base(filter)
	}
	trigger := ui.Div().Row().Align(ui.Center).Gap(space1).PaddingX(space2).Height(ui.Px(24)).Radius(radiusS).
		Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).Shrink(1).MinWidth(ui.Px(0)).
		Role(access.RoleButton).Label("Filter by repository: " + label).TabIndex(0).
		Child(ui.Text(label).FontSize(12).Medium().TextColor(t.TextMuted).Truncate().MinWidth(ui.Px(0))).
		Child(ui.Icon(icons.ChevronDown).Size(12).Color(t.TextSubtle))
	return ui.DropdownMenu(trigger, func() []ui.MenuItem {
		items := []ui.MenuItem{{Label: "All repos", Icon: icons.Layers, OnClick: func() { a.navSel.Set("orchestrator") }}}
		for _, r := range repos {
			path := r
			items = append(items, ui.MenuItem{Label: fmt.Sprintf("%s  ·  %d", filepath.Base(r), counts[r]),
				Icon: icons.FolderGit2, OnClick: func() { a.navSel.Set("repo:" + path) }})
		}
		return items
	})
}

// needsUser reports whether a thread waits on the user: a question, a failure, or work to review.
func needsUser(th core.ThreadView) bool {
	if th.PR != nil && th.PR.State == forge.StateOpen {
		return th.CanMerge && !th.PR.AutoMerge // checks passed and it waits for the user's Merge
	}
	return th.Status == core.StatusWaiting || th.Status == core.StatusFailed ||
		(th.Status == core.StatusIdle && (th.CanMerge || th.CanOpenPR))
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
			Child(statusMark(ctx, shownStatus(th))).
			Child(ui.Text(th.Title).FontSize(13).Medium().Truncate().Grow(1).MinWidth(ui.Px(0))).
			Child(a.rowEnd(ctx, th, when, whenColor))).
		Child(a.rowDetail(ctx, repo, line, lineColor))
	if selected {
		row = row.Background(t.SurfaceActive)
	}
	return row
}

// rowEnd is the end of a rail row's first line: its time, or a Done button for a thread with nothing
// left to merge.
func (a *App) rowEnd(ctx *ui.Context, th core.ThreadView, when string, c ui.Color) ui.Element {
	if th.CanDone {
		return ui.Div().Height(ui.Px(16)).Row().Align(ui.Center).
			Child(ui.IconButton(icons.Check).Ghost().Small().Label("Mark “" + th.Title + "” done").
				Tooltip("Done: take it out of your list").OnClick(func() { a.markDone(th) }))
	}
	return ui.Text(when).FontSize(11).TextColor(c).NoWrap()
}

// rowDetail is a rail row's second line: the repository (unless the list shows only one) and the
// activity.
func (a *App) rowDetail(ctx *ui.Context, repo, line string, c ui.Color) ui.Element {
	t := ctx.Theme()
	l := ui.Div().Row().Align(ui.Center).Gap(6).PaddingLeft(space5 + 2).MinWidth(ui.Px(0))
	if a.repoFilter() == "" {
		l = l.Child(ui.Text(repo).FontSize(12).TextColor(t.TextSubtle).NoWrap()).
			Child(ui.Text("·").FontSize(12).TextColor(t.TextSubtle))
	}
	return l.Child(ui.Text(line).FontSize(12).TextColor(c).Truncate().Grow(1).MinWidth(ui.Px(0)))
}

func branchLabel(th core.ThreadView) string {
	if th.Branch == "" {
		return ""
	}
	return strings.TrimPrefix(th.Branch, "solo/")
}

// activityLine says what a thread is doing or did, in a line.
func (a *App) activityLine(th core.ThreadView) string {
	if s := shownStatus(th); s == statusInReview || s == statusChecksFailed {
		return prLine(th.PR)
	}
	switch th.Status {
	case core.StatusQueued:
		return "Starting"
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
	case core.StatusMerged:
		if th.BaseBranch != "" {
			return "Merged into " + th.BaseBranch
		}
		return "Merged"
	}
	if th.Summary != "" {
		return firstLine(stripMarkdown(th.Summary))
	}
	return firstLine(th.Prompt)
}

// prLine says where a pull request stands: "PR #7 · waiting for checks".
func prLine(pr *forge.PR) string {
	parts := []string{fmt.Sprintf("%s #%d", pr.Kind.Noun(), pr.Number)}
	switch {
	case pr.Draft:
		parts = append(parts, "draft")
	case pr.Checks == forge.ChecksFailing:
		parts = append(parts, "checks failed")
	case pr.Mergeable:
		parts = append(parts, "ready to merge")
	case pr.Blocked != "":
		parts = append(parts, pr.Blocked)
	}
	if pr.AutoMerge {
		parts = append(parts, "auto-merge on")
	}
	return strings.Join(parts, " · ")
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
