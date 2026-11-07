package gui

import (
	"context"
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

// settingsDialog edits the folders to search for repositories, the models, and the worker limit.
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
		maxConc := ui.Local(ctx, "max", initial.MaxConcurrent)
		showCosts := ui.Local(ctx, "show-costs", initial.ShowCosts)

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
		field := func(label, help string, control ui.Element) ui.Element {
			return ui.Div().Column().Gap(space1 + 2).
				Child(subhead(label)).
				Child(control).
				Child(caption(ctx, help))
		}
		body := ui.Div().Column().Gap(space5).Width(ui.Full).
			Child(field("Repository folders",
				"Solo finds the git repositories in these folders (up to three levels down) and tells the orchestrator about them.",
				ui.Div().Column().Gap(space2).
					Child(list).
					Child(ui.Div().Row().Gap(space2).
						Child(ui.TextInput(newFolder).Placeholder("Add a folder, such as ~/projects").Label("Folder to add").Small().Width(ui.Full).
							Key("new-folder").OnSubmit(func(string) { add() })).
						Child(ui.Button("Add").Icon(icons.Plus).Secondary().Small().OnClick(add))))).
			Child(ui.Div().Row().Gap(space4).
				Child(ui.Div().Grow(1).Basis(ui.Px(0)).Child(field("Orchestrator model", "The manager you talk to.",
					ui.Select(orchModel, modelOptions...).Width(ui.Full)))).
				Child(ui.Div().Grow(1).Basis(ui.Px(0)).Child(field("Worker model", "The default for new threads.",
					ui.Select(workerModel, modelOptions...).Width(ui.Full))))).
			Child(field("Workers at once", "More threads wait in a queue until one finishes.",
				ui.NumberInput(maxConc, 1, 16, 1).Width(ui.Px(120)))).
			Child(field("Costs", "Show what each thread has cost so far, as Claude estimates it.",
				ui.Switch("Show costs", showCosts)))

		save := ui.Button("Save").Primary().Key("settings-save").OnClick(func() {
			f, om, wm, mc, sc := folders.Peek(), orchModel.Peek(), workerModel.Peek(), maxConc.Peek(), showCosts.Peek()
			a.settings.Set(false)
			a.run("Saving settings", func(context.Context) error {
				return a.b.UpdateSettings(func(s *core.Settings) {
					s.Folders, s.OrchestratorModel, s.WorkerModel, s.MaxConcurrent, s.ShowCosts = f, om, wm, mc, sc
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
