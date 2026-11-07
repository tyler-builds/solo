package gui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/git"
)

func (a *App) merge(id string) {
	a.run("Merge", func(ctx context.Context) error {
		commit, err := a.b.Merge(ctx, id)
		if err != nil {
			return err
		}
		th, _ := a.b.Thread(id)
		if len(commit) > 8 {
			commit = commit[:8]
		}
		a.notify(ui.ToastSuccess, "Merged", fmt.Sprintf("%s is in %s (%s).", th.Title, th.BaseBranch, commit))
		return nil
	})
}

// threadPanel shows the open thread beside the orchestrator: its conversation or its changes, and its
// actions.
func (a *App) threadPanel(ctx *ui.Context) ui.Element {
	id := a.openID.Get()
	th, ok := a.thread(id)
	if !ok {
		return ui.Div()
	}
	t := ctx.Theme()
	tab := a.openTab.Get()

	actions := ui.Div().Row().Align(ui.Center).Gap(space1).Shrink(0)
	if th.CanDiscard {
		actions = actions.Child(ui.IconButton(icons.Trash).Ghost().Small().Label("Discard thread").Tooltip("Discard…").OnClick(func() {
			a.confirm.Set(&confirmation{
				title:  "Discard this thread?",
				body:   fmt.Sprintf("This stops %q and deletes its worktree and branch %s, with any work not merged.", th.Title, th.Branch),
				action: "Discard",
				run: func() {
					a.closeThread()
					a.run("Discard", func(ctx context.Context) error { return a.b.Discard(ctx, id) })
				},
			})
		}))
	}
	if th.CanMerge {
		actions = actions.Child(ui.Button("Merge").Icon(icons.GitMerge).Primary().Small().
			Tooltip("Merge " + th.Branch + " into " + th.BaseBranch).OnClick(func() { a.merge(id) }))
	}
	actions = actions.Child(ui.IconButton(icons.X).Ghost().Small().Label("Close thread").Tooltip("Close").
		OnClick(a.closeThread))
	meta := ui.Div().Row().Align(ui.Center).Gap(space2).MinWidth(ui.Px(0)).
		Child(ui.Text(statusLabel(th)).FontSize(12).Semibold().TextColor(statusColor(ctx, th.Status)).NoWrap()).
		Child(ui.Text(filepath.Base(th.RepoPath)).FontSize(12).Medium().TextColor(t.TextMuted).NoWrap())
	if th.Branch != "" {
		meta = meta.Child(ui.Div().Row().Align(ui.Center).Gap(space1).MinWidth(ui.Px(0)).
			Child(ui.Icon(icons.GitBranch).Size(12).Color(t.TextSubtle)).
			Child(ui.Text(branchLabel(th) + " → " + th.BaseBranch).FontSize(12).TextColor(t.TextSubtle).TruncateHead().MinWidth(ui.Px(0))))
	}
	if a.cfg.Get().ShowCosts && th.CostUSD > 0 {
		meta = meta.Child(subtle(ctx, fmt.Sprintf("$%.2f", th.CostUSD)))
	}
	header := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingLeft(space4).PaddingRight(space2).Height(ui.Px(60)).Shrink(0).
		Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
			Child(ui.Div().Row().Align(ui.Center).Gap(space2).MinWidth(ui.Px(0)).
				Child(dot(statusColor(ctx, th.Status), 8)).
				Child(ui.Text(th.Title).FontSize(14).Semibold().Truncate().MinWidth(ui.Px(0)))).
			Child(ui.Div().PaddingLeft(space4).Child(meta))).
		Child(actions)
	tabs := ui.Div().Row().Align(ui.Center).PaddingX(space4).Shrink(0).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).
		Child(ui.Tabs(a.openTab,
			ui.Tab[string]{Value: "conversation", Label: "Conversation"},
			ui.Tab[string]{Value: "changes", Label: "Changes", Disabled: !th.HasWorktree},
		))

	var content ui.Element
	if tab == "changes" && th.HasWorktree {
		content = ui.Component(func(ctx *ui.Context) ui.Element { return a.changesView(ctx, th) })
	} else {
		items := a.itemsOf(id).Get()
		content = ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).
			Child(a.transcript(ctx, th, items, "thread-"+id)).
			Child(ui.Div().Column().PaddingX(space4).PaddingBottom(space4).Shrink(0).
				Child(a.composerFor(ctx, th)))
	}
	return ui.Div().Column().Size(ui.Full, ui.Full).Key("thread-panel-" + id).
		Child(header).
		Child(tabs).
		Child(ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).Child(content))
}

func (a *App) composerFor(ctx *ui.Context, th core.ThreadView) ui.Element {
	if core.Finished(th.Status) {
		return caption(ctx, "This thread is "+strings.ToLower(statusLabel(th))+".")
	}
	if th.Status == core.StatusQueued {
		return caption(ctx, "This thread starts when a worker slot is free.")
	}
	id := th.ID
	return a.composer(ctx, th, "draft-"+id, "Message this worker…", func(text string) error {
		return a.b.Send(context.Background(), id, text)
	})
}

// changesView lists a worker's changed files and shows the diff.
func (a *App) changesView(ctx *ui.Context, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	type data struct {
		files []git.FileChange
		diff  string
	}
	// Reload when the thread's transcript or status changes: the worker may have edited files.
	key := fmt.Sprintf("changes:%s:%s:%d", th.ID, th.Status, th.UpdatedAt.UnixMilli())
	res := ui.Query(ctx, key, func(c context.Context) (data, error) {
		files, err := a.b.Changes(c, th.ID)
		if err != nil {
			return data{}, err
		}
		diff, err := a.b.Diff(c, th.ID)
		return data{files, diff}, err
	})
	switch {
	case res.Loading && len(res.Data.files) == 0:
		return ui.Div().Column().Center().Grow(1).Child(ui.ProgressRing().Label("Loading changes"))
	case res.Err != nil:
		return ui.Div().Column().Center().Grow(1).Padding(space6).
			Child(ui.Empty("Could not read the changes").Icon(icons.CircleAlert).Description(res.Err.Error()))
	case len(res.Data.files) == 0:
		return ui.Div().Column().Center().Grow(1).Padding(space6).
			Child(ui.Empty("No changes yet").Icon(icons.FileCode).Description("Files the worker changes show up here."))
	}
	added, removed := 0, 0
	files := ui.Div().Column().Gap(2).Padding(space3).BorderEdges(ui.Edges{Bottom: 1}, t.Border).Shrink(0).
		MaxHeight(ui.Px(180)).ScrollY()
	for _, f := range res.Data.files {
		added += f.Added
		removed += f.Removed
		files = files.Child(ui.Div().Row().Align(ui.Center).Gap(space2).Height(ui.Px(24)).
			Child(ui.Text(fileStatus(f.Status)).Mono().FontSize(12).Semibold().TextColor(fileStatusColor(ctx, f.Status)).Width(ui.Px(14))).
			Child(ui.Text(f.Path).Mono().FontSize(12).TruncateHead().Grow(1).MinWidth(ui.Px(0))).
			Child(ui.Text(fmt.Sprintf("+%d", f.Added)).Mono().FontSize(12).TextColor(t.Success)).
			Child(ui.Text(fmt.Sprintf("−%d", f.Removed)).Mono().FontSize(12).TextColor(t.Danger)))
	}
	summary := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space3).Height(ui.Px(36)).Shrink(0).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).
		Child(subhead(fmt.Sprintf("%d files changed", len(res.Data.files)))).
		Child(ui.Text(fmt.Sprintf("+%d", added)).FontSize(12).TextColor(t.Success)).
		Child(ui.Text(fmt.Sprintf("−%d", removed)).FontSize(12).TextColor(t.Danger))
	return ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).
		Child(summary).
		Child(files).
		Child(diffView(ctx, res.Data.diff))
}

func fileStatus(s string) string {
	if s == "?" {
		return "A"
	}
	return s
}

func fileStatusColor(ctx *ui.Context, s string) ui.Color {
	t := ctx.Theme()
	switch s {
	case "A", "?":
		return t.Success
	case "D":
		return t.Danger
	}
	return ui.Hex(pal(t).warning)
}

// diffView shows a unified diff, one line per row, colored by kind.
func diffView(ctx *ui.Context, diff string) ui.Element {
	t := ctx.Theme()
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	addBg, delBg := t.Success.Alpha(0.10), t.Danger.Alpha(0.10)
	return ui.UniformList(len(lines), func(i int) ui.Element {
		l := lines[i]
		row := ui.Div().Row().Align(ui.Center).PaddingX(space3).Height(ui.Px(20))
		txt := ui.Text(strings.ReplaceAll(l, "\t", "    ")).Mono().FontSize(12).NoWrap()
		switch {
		case strings.HasPrefix(l, "diff --git"):
			row = row.Background(t.SurfaceActive)
			txt = ui.Text(strings.TrimPrefix(l, "diff --git ")).Mono().FontSize(12).Semibold().NoWrap()
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "index "),
			strings.HasPrefix(l, "new file"), strings.HasPrefix(l, "deleted file"):
			txt = txt.TextColor(t.TextSubtle)
		case strings.HasPrefix(l, "@@"):
			txt = txt.TextColor(ui.Hex(pal(t).info))
		case strings.HasPrefix(l, "+"):
			row = row.Background(addBg)
			txt = txt.TextColor(t.Text)
		case strings.HasPrefix(l, "-"):
			row = row.Background(delBg)
			txt = txt.TextColor(t.Text)
		default:
			txt = txt.TextColor(t.TextMuted)
		}
		return row.Child(txt)
	}).Style(ui.Style{Grow: 1}).Label("Diff")
}
