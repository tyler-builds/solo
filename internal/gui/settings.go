package gui

import (
	"context"
	"fmt"
	"github.com/tyler-builds/solo/internal/agent"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// fallbackModels are offered until Claude Code reports its own list.
var fallbackModels = []ui.Option[string]{
	{Value: "opus", Label: "Opus"},
	{Value: "sonnet", Label: "Sonnet"},
	{Value: "haiku", Label: "Haiku"},
	{Value: "fable", Label: "Fable"},
}

// modelChoices are the models Claude Code offers, keeping the current choice even when it's not
// listed.
func modelChoices(models []agent.ModelInfo, current string) []ui.Option[string] {
	var out []ui.Option[string]
	for _, m := range models {
		if m.Value == "default" {
			continue // the account default changes under the user; name a model instead
		}
		label := m.DisplayName
		if label == "" {
			label = m.Value
		}
		out = append(out, ui.Option[string]{Value: m.Value, Label: label})
	}
	if len(out) == 0 {
		out = slices.Clone(fallbackModels)
	}
	if current != "" && !slices.ContainsFunc(out, func(o ui.Option[string]) bool { return o.Value == current }) {
		out = append(out, ui.Option[string]{Value: current, Label: current})
	}
	return out
}

// effortChoices are Default plus the effort levels the model takes.
func effortChoices(models []agent.ModelInfo, model, current string) []ui.Option[string] {
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	for _, m := range models {
		if m.Value == model && len(m.EffortLevels) > 0 {
			levels = m.EffortLevels
		}
	}
	out := []ui.Option[string]{{Value: "", Label: "Default"}}
	for _, l := range levels {
		out = append(out, ui.Option[string]{Value: l, Label: strings.ToUpper(l[:1]) + l[1:]})
	}
	if current != "" && !slices.Contains(levels, current) {
		out = append(out, ui.Option[string]{Value: current, Label: current})
	}
	return out
}

// repoRulesEditor edits the branch naming rules that override the global ones for a repository.
func (a *App) repoRulesEditor(ctx *ui.Context, rules *ui.Signal[map[string]string]) ui.Element {
	t := ctx.Theme()
	cur := rules.Get()
	col := ui.Div().Column().Gap(space2)
	repos := slices.Sorted(maps.Keys(cur))
	for _, repo := range repos {
		r := repo
		col = col.Child(ui.Div().Column().Gap(space1).Padding(space3).Radius(radiusM).Border(1, t.Border).Background(t.Background).
			Child(ui.Div().Row().Align(ui.Center).Gap(space2).
				Child(ui.Icon(icons.FolderGit2).Size(13).Color(t.TextMuted)).
				Child(ui.Text(filepath.Base(r)).FontSize(13).Medium().Grow(1)).
				Child(ui.IconButton(icons.X).Ghost().Small().Label("Remove the rules for " + filepath.Base(r)).OnClick(func() {
					next := maps.Clone(rules.Peek())
					delete(next, r)
					rules.Set(next)
				}))).
			Child(ui.Component(func(ctx *ui.Context) ui.Element {
				text := ui.Local(ctx, "rules", cur[r])
				return ui.TextArea(text).Placeholder("Rules for " + filepath.Base(r)).Label("Branch naming rules for " + filepath.Base(r)).
					MinRows(2).MaxHeight(ui.Px(120)).Width(ui.Full).Borderless().
					OnChange(func(v string) {
						next := maps.Clone(rules.Peek())
						next[r] = v
						rules.Set(next)
					})
			}).Key("rules-" + r)))
	}
	var add []ui.MenuItem
	for _, repo := range a.repos.Get() {
		if _, ok := cur[repo]; ok {
			continue
		}
		r := repo
		add = append(add, ui.MenuItem{Label: filepath.Base(r), Icon: icons.FolderGit2, OnClick: func() {
			next := maps.Clone(rules.Peek())
			if next == nil {
				next = map[string]string{}
			}
			next[r] = ""
			rules.Set(next)
		}})
	}
	if len(add) > 0 {
		col = col.Child(ui.Div().Row().Child(ui.DropdownMenu(ui.Button("Rules for a repository").Icon(icons.Plus).Secondary().Small(),
			func() []ui.MenuItem { return add })))
	}
	return col
}

// settingsDialog edits the Claude Code path, the folders to search for repositories, the models, and
// whether costs show.
func (a *App) settingsDialog(ctx *ui.Context) ui.Element {
	if !a.settings.Get() {
		return nil
	}
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		initial := a.b.Settings()
		folders := ui.Local(ctx, "folders", initial.Folders)
		newFolder := ui.Local(ctx, "new-folder", "")
		tab := ui.Local(ctx, "tab", a.settingsTab)
		orchModel := ui.Local(ctx, "orch-model", initial.OrchestratorModel)
		workerModel := ui.Local(ctx, "worker-model", initial.WorkerModel)
		namingModel := ui.Local(ctx, "naming-model", initial.NamingModel)
		orchEffort := ui.Local(ctx, "orch-effort", initial.OrchestratorEffort)
		workerEffort := ui.Local(ctx, "worker-effort", initial.WorkerEffort)
		namingEffort := ui.Local(ctx, "naming-effort", initial.NamingEffort)
		branchRules := ui.Local(ctx, "branch-rules", initial.BranchRules)
		repoRules := ui.Local(ctx, "repo-rules", maps.Clone(initial.RepoBranchRules))
		autoMerge := ui.Local(ctx, "auto-merge", initial.AutoMerge)
		mergeMethod := ui.Local(ctx, "merge-method", initial.MergeMethod)
		onFail := ui.Local(ctx, "on-fail", initial.OnChecksFail)
		prModel := ui.Local(ctx, "pr-model", initial.PRModel)
		prEffort := ui.Local(ctx, "pr-effort", initial.PREffort)
		prRules := ui.Local(ctx, "pr-rules", initial.PRRules)
		prDescribe := ui.Local(ctx, "pr-describe", !initial.SkipPRDescription)
		showCosts := ui.Local(ctx, "show-costs", initial.ShowCosts)
		claudePath := ui.Local(ctx, "claude-path", initial.ClaudePath)
		checked := ui.Local(ctx, "claude-checked", strings.TrimSpace(initial.ClaudePath))

		add := func() {
			f := strings.TrimSpace(newFolder.Peek())
			if f == "" || slices.Contains(folders.Peek(), f) {
				return
			}
			folders.Set(append(slices.Clone(folders.Peek()), f))
			newFolder.Set("")
		}
		list := ui.Div().Column().Gap(space1)
		for _, f := range folders.Get() {
			folder := f
			list = list.Child(ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space3).Height(ui.Px(34)).
				Radius(radiusM).Border(1, t.Border).Background(t.Background).
				Child(ui.Icon(icons.Folder).Size(14).Color(t.TextMuted)).
				Child(ui.Text(folder).FontSize(13).TruncateHead().Grow(1).MinWidth(ui.Px(0))).
				Child(ui.IconButton(icons.X).Ghost().Small().Label("Remove " + folder).OnClick(func() {
					folders.Set(slices.DeleteFunc(slices.Clone(folders.Peek()), func(s string) bool { return s == folder }))
				})))
		}
		if len(folders.Get()) == 0 {
			list = list.Child(caption(ctx, "No folders yet."))
		}
		// What the last scan found, so the user can check it.
		if found := a.repos.Get(); len(found) > 0 {
			names := make([]string, 0, len(found))
			for _, r := range found {
				names = append(names, filepath.Base(r))
			}
			list = list.Child(ui.Div().Row().Gap(space1 + 2).PaddingTop(space1).
				Child(ui.Icon(icons.FolderGit2).Size(13).Color(t.TextSubtle)).
				Child(caption(ctx, fmt.Sprintf("Found %d repositories: %s", len(found), strings.Join(names, ", "))).Grow(1)))
		}
		field := func(label, help string, control ui.Element) ui.Element {
			return ui.Div().Column().Gap(space1 + 2).
				Child(subhead(label)).
				Child(control).
				Child(caption(ctx, help))
		}
		// Claude Code: what Solo found, and a path to use instead.
		claude := ui.Query(ctx, "claude:"+checked.Get(), func(c context.Context) (core.ClaudeStatus, error) {
			return a.b.CheckClaude(c, checked.Peek()), nil
		})
		check := func() {
			p := strings.TrimSpace(claudePath.Peek())
			if p == checked.Peek() {
				claude.Refetch()
				return
			}
			checked.Set(p)
		}
		var status ui.Element
		if st := claude.Data; claude.Loading || claude.Fetching {
			status = ui.Div().Row().Align(ui.Center).Gap(space2).
				Child(ui.ProgressRing().Size(14)).
				Child(muted(ctx, "Checking Claude Code…"))
		} else {
			icon, color, line := icons.CircleCheck, t.Success, "Claude Code "+st.Version
			if st.Account != "" {
				line += " · " + st.Account
			}
			if st.Err != "" {
				icon, color, line = icons.CircleAlert, t.Danger, st.Err
			}
			where := "Not found"
			if st.Path != "" {
				where = st.Path + " · " + map[string]string{"setting": "from the path below", "PATH": "found on PATH",
					"default location": "found in its default location", "bundled": "shipped with Solo"}[st.Source]
			}
			status = ui.Div().Row().Align(ui.Start).Gap(space2).
				Child(ui.Div().PaddingTop(2).Child(ui.Icon(icon).Size(14).Color(color))).
				Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
					Child(ui.Text(line).FontSize(13).TextColor(t.Text)).
					Child(caption(ctx, where).TruncateHead()))
		}
		claudeBox := ui.Div().Column().Gap(space2).
			Child(ui.Div().Padding(space3).Radius(radiusM).Border(1, t.Border).Background(t.Background).Child(status)).
			Child(ui.Div().Row().Gap(space2).
				Child(ui.TextInput(claudePath).Placeholder("Path to claude (leave empty to detect it)").Label("Path to claude").
					Small().Width(ui.Full).Key("claude-path").OnSubmit(func(string) { check() })).
				Child(ui.Button("Check").Icon(icons.RefreshCw).Secondary().Small().OnClick(check)))

		general := ui.Div().Column().Gap(space5).Width(ui.Full).
			Child(field("Claude Code", "Solo runs your installed Claude Code and uses its sign-in. A changed path applies to sessions started after you save.", claudeBox)).
			Child(field("Repository folders",
				"Solo looks for git repositories up to three folders deep.",
				ui.Div().Column().Gap(space2).
					Child(list).
					Child(ui.Div().Row().Gap(space2).
						Child(ui.TextInput(newFolder).Placeholder("Add a folder, such as ~/projects").Label("Folder to add").Small().Width(ui.Full).
							Key("new-folder").OnSubmit(func(string) { add() })).
						Child(ui.Button("Add").Icon(icons.Plus).Secondary().Small().OnClick(add))))).
			Child(ui.Div().Row().Align(ui.Center).Gap(space4).
				Child(ui.Div().Column().Gap(space1).Grow(1).MinWidth(ui.Px(0)).
					Child(subhead("Costs")).
					Child(caption(ctx, "What each thread has cost so far, as Claude estimates it."))).
				Child(ui.Switch("Show", showCosts)))

		models := claude.Data.Models
		feature := func(name, help string, model, effort *ui.Signal[string]) ui.Element {
			return ui.Div().Column().Gap(space1 + 2).
				Child(subhead(name)).
				Child(ui.Div().Row().Gap(space3).
					Child(ui.Div().Grow(2).Basis(ui.Px(0)).Child(ui.Select(model, modelChoices(models, model.Get())...).Width(ui.Full).Label(name + " model"))).
					Child(ui.Div().Grow(1).Basis(ui.Px(0)).Child(ui.Select(effort, effortChoices(models, model.Get(), effort.Get())...).Width(ui.Full).Label(name + " effort")))).
				Child(caption(ctx, help))
		}
		modelsTab := ui.Div().Column().Gap(space5).Width(ui.Full).
			Child(feature("Solo", "The lead agent you talk to. It plans, follows the threads and reviews their work.", orchModel, orchEffort)).
			Child(feature("Threads", "The agents that do the work, one per task. Solo can pick another model for a thread when the task calls for it.", workerModel, workerEffort)).
			Child(feature("Branch names", "Names each thread's branch by your rules (Git tab). Only used when you have rules.", namingModel, namingEffort)).
			Child(feature("Pull request text", "Writes each pull request's title and description, by your rules (Git tab).", prModel, prEffort)).
			Child(caption(ctx, "Effort is how long a model thinks before it acts. Default leaves it to Claude. Changes apply to sessions started after you save."))

		gitTab := ui.Div().Column().Gap(space5).Width(ui.Full).PaddingRight(space3). // clear of the scrollbar
												Child(field("Branch names",
				"Rules for naming a thread's branch, in your words: a prefix, a ticket format, a length. A small model applies them. Leave empty for solo/<title>.",
				ui.TextArea(branchRules).Placeholder("For example: feature/<ticket>-<short-summary>, lowercase, hyphens, under 50 characters. Use fix/ for bug fixes.").
					Label("Branch naming rules").MinRows(3).MaxHeight(ui.Px(140)).Width(ui.Full).Key("branch-rules"))).
			Child(a.repoRulesEditor(ctx, repoRules)).
			Child(ui.Div().Column().Gap(space1 + 2).
				Child(subhead("Pull request text")).
				Child(caption(ctx, "How to write a pull request's title and description. A model applies them to the thread's commits, changes and summary.")).
				Child(ui.Div().Column().Gap(space3).PaddingTop(space2).
					Child(ui.TextArea(prRules).Placeholder("For example: Conventional Commit title with the ticket, like feat(auth): OW-123 refresh tokens. Description: Summary, Changes, Testing.").
						Label("Pull request text rules").MinRows(3).MaxHeight(ui.Px(140)).Width(ui.Full).Key("pr-rules")).
					Child(ui.Div().Row().Align(ui.Center).Gap(space4).
						Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
							Child(ui.Text("Write a description").FontSize(13)).
							Child(caption(ctx, "Off: pull requests get a title only."))).
						Child(ui.Switch("Write a description", prDescribe))))).
			Child(ui.Div().Column().Gap(space1 + 2).
				Child(subhead("Pull requests")).
				Child(caption(ctx, "Solo pushes a thread's branch and opens a pull request (GitHub, with gh) or merge request (GitLab, with glab) against the branch it started from. Repositories without a remote merge locally.")).
				Child(ui.Div().Column().Gap(space3).PaddingTop(space2).
					Child(ui.Div().Row().Align(ui.Center).Gap(space4).
						Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
							Child(ui.Text("Auto-merge").FontSize(13)).
							Child(caption(ctx, "Turn on the forge's auto-merge when the pull request opens, so it merges once required checks and reviews pass."))).
						Child(ui.Switch("Auto-merge", autoMerge))).
					Child(ui.Div().Row().Gap(space4).
						Child(ui.Div().Grow(1).Basis(ui.Px(0)).Child(field("Merge method", "How the pull request merges.",
							ui.Select(mergeMethod, ui.Option[string]{Value: "squash", Label: "Squash"},
								ui.Option[string]{Value: "merge", Label: "Merge commit"},
								ui.Option[string]{Value: "rebase", Label: "Rebase"}).Width(ui.Full)))).
						Child(ui.Div().Grow(1).Basis(ui.Px(0)).Child(field("When checks fail", "Solo can send the failure to the thread to fix.",
							ui.Select(onFail, ui.Option[string]{Value: core.ChecksFix, Label: "Solo fixes it"},
								ui.Option[string]{Value: core.ChecksNotify, Label: "Just notify me"}).Width(ui.Full)))))))

		var body ui.Element
		switch tab.Get() {
		case "models":
			body = modelsTab
		case "git":
			body = gitTab
		case "history":
			body = ui.Div().Column().Gap(space3).Width(ui.Full).
				Child(caption(ctx, "Threads you marked done or discarded. Restoring one puts it back in your list with its worktree, so you can keep talking to it.")).
				Child(a.historyList(ctx))
		default:
			body = general
		}
		body = ui.Div().Column().Gap(space4).Width(ui.Full).
			Child(ui.Div().Row().BorderEdges(ui.Edges{Bottom: 1}, t.Border).
				Child(ui.Tabs(tab,
					ui.Tab[string]{Value: "general", Label: "General"},
					ui.Tab[string]{Value: "models", Label: "Models"},
					ui.Tab[string]{Value: "git", Label: "Git"},
					ui.Tab[string]{Value: "history", Label: "History"}))).
			Child(body)

		save := ui.Button("Save").Primary().Key("settings-save").OnClick(func() {
			apply := func(s *core.Settings) {
				s.Folders, s.ShowCosts, s.ClaudePath = folders.Peek(), showCosts.Peek(), strings.TrimSpace(claudePath.Peek())
				s.OrchestratorModel, s.WorkerModel, s.NamingModel = orchModel.Peek(), workerModel.Peek(), namingModel.Peek()
				s.OrchestratorEffort, s.WorkerEffort, s.NamingEffort = orchEffort.Peek(), workerEffort.Peek(), namingEffort.Peek()
				s.BranchRules, s.RepoBranchRules = strings.TrimSpace(branchRules.Peek()), repoRules.Peek()
				s.AutoMerge, s.MergeMethod, s.OnChecksFail = autoMerge.Peek(), mergeMethod.Peek(), onFail.Peek()
				s.PRModel, s.PREffort, s.PRRules, s.SkipPRDescription = prModel.Peek(), prEffort.Peek(), strings.TrimSpace(prRules.Peek()), !prDescribe.Peek()
			}
			a.settings.Set(false)
			a.run("Saving settings", func(context.Context) error { return a.b.UpdateSettings(apply) })
		})
		return ui.Modal("Settings", body).Width(ui.Px(600)).MaxWidth(ui.Px(600)).
			Footer(ui.Button("Cancel").Secondary().OnClick(func() { a.settings.Set(false) }), save).
			Default(save).
			OnClose(func() { a.settings.Set(false) }).Key("settings")
	})
}

// historyList lists the threads the user marked done, newest first, each with Restore.
func (a *App) historyList(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	var done []core.ThreadView
	for _, th := range a.threads.Get() {
		if th.IsWorker && !th.DoneAt.IsZero() {
			done = append(done, th)
		}
	}
	box := ui.Div().Column().Radius(radiusM).Border(1, t.Border).Background(t.Background).MaxHeight(ui.Px(208)).ScrollY()
	if len(done) == 0 {
		return box.Padding(space3).Child(caption(ctx, "Nothing yet. Threads you mark done show up here."))
	}
	slices.SortFunc(done, func(x, y core.ThreadView) int { return y.DoneAt.Compare(x.DoneAt) })
	for i, th := range done {
		id := th.ID
		row := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space3).PaddingY(space2).
			Child(statusMark(ctx, shownStatus(th))).
			Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
				Child(ui.Text(th.Title).FontSize(13).Truncate()).
				Child(caption(ctx, repoLabel(th)+" · "+statusLabel(th)+" · "+doneAgo(th.DoneAt)).Truncate())).
			Child(ui.Div().Shrink(0).Child(ui.Button("Restore").Icon(icons.RotateCcw).Secondary().Small().
				Tooltip("Back to your list").OnClick(func() { a.restore(id) })))
		if i > 0 {
			row = row.BorderEdges(ui.Edges{Top: 1}, t.Border)
		}
		box = box.Child(row)
	}
	return box
}

// confirmDialog asks before a destructive action.
func (a *App) confirmDialog(ctx *ui.Context) ui.Element {
	c := a.confirm.Get()
	if c == nil {
		return nil
	}
	do := ui.Button(c.action).Danger().OnClick(func() {
		a.confirm.Set(nil)
		c.run()
	})
	return ui.Modal(c.title, ui.Div().Width(ui.Full).Child(muted(ctx, c.body))).Width(ui.Px(460)).
		Footer(ui.Button("Cancel").Secondary().OnClick(func() { a.confirm.Set(nil) }), do).
		OnClose(func() { a.confirm.Set(nil) }).Key("confirm")
}

func doneAgo(t time.Time) string {
	if s := shortAgo(t); s != "now" {
		return "done " + s + " ago"
	}
	return "done just now"
}
