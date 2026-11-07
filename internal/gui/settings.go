package gui

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

var modelOptions = []ui.Option[string]{
	{Value: "opus", Label: "Opus"},
	{Value: "sonnet", Label: "Sonnet"},
	{Value: "haiku", Label: "Haiku"},
	{Value: "fable", Label: "Fable"},
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
		orchModel := ui.Local(ctx, "orch-model", initial.OrchestratorModel)
		workerModel := ui.Local(ctx, "worker-model", initial.WorkerModel)
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

		body := ui.Div().Column().Gap(space5).Width(ui.Full).
			Child(field("Claude Code", "Solo runs your installed Claude Code and uses its sign-in. A changed path applies to sessions started after you save.", claudeBox)).
			Child(field("Repository folders",
				"Solo looks for git repositories up to three folders deep.",
				ui.Div().Column().Gap(space2).
					Child(list).
					Child(ui.Div().Row().Gap(space2).
						Child(ui.TextInput(newFolder).Placeholder("Add a folder, such as ~/projects").Label("Folder to add").Small().Width(ui.Full).
							Key("new-folder").OnSubmit(func(string) { add() })).
						Child(ui.Button("Add").Icon(icons.Plus).Secondary().Small().OnClick(add))))).
			Child(ui.Div().Row().Gap(space4).
				Child(ui.Div().Grow(1).Basis(ui.Px(0)).Child(field("Solo's model", "The one you talk to.",
					ui.Select(orchModel, modelOptions...).Width(ui.Full)))).
				Child(ui.Div().Grow(1).Basis(ui.Px(0)).Child(field("Thread model", "The default for new threads.",
					ui.Select(workerModel, modelOptions...).Width(ui.Full))))).
			Child(ui.Div().Row().Align(ui.Center).Gap(space4).
				Child(ui.Div().Column().Gap(space1).Grow(1).MinWidth(ui.Px(0)).
					Child(subhead("Costs")).
					Child(caption(ctx, "What each thread has cost so far, as Claude estimates it."))).
				Child(ui.Switch("Show", showCosts)))

		save := ui.Button("Save").Primary().Key("settings-save").OnClick(func() {
			f, om, wm, sc, cp := folders.Peek(), orchModel.Peek(), workerModel.Peek(), showCosts.Peek(), strings.TrimSpace(claudePath.Peek())
			a.settings.Set(false)
			a.run("Saving settings", func(context.Context) error {
				return a.b.UpdateSettings(func(s *core.Settings) {
					s.Folders, s.OrchestratorModel, s.WorkerModel, s.ShowCosts, s.ClaudePath = f, om, wm, sc, cp
				})
			})
		})
		return ui.Modal("Settings", body).Width(ui.Px(600)).MaxWidth(ui.Px(600)).
			Footer(ui.Button("Cancel").Secondary().OnClick(func() { a.settings.Set(false) }), save).
			Default(save).
			OnClose(func() { a.settings.Set(false) }).Key("settings")
	})
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
