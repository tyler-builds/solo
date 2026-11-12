package gui

import (
	"cmp"
	"context"
	"fmt"
	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/launch"
	"maps"
	"os"
	"strconv"

	"github.com/tyler-builds/jamb/dialog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// modelPicker shows a thread's model and effort in its message box, and changes them.
func (a *App) modelPicker(ctx *ui.Context, v core.ThreadView) ui.Element {
	t := ctx.Theme()
	cfg := a.cfg.Get()
	// Shares Settings' check of Claude Code, which lists the models.
	claude := ui.Query(ctx, "claude:"+strings.TrimSpace(cfg.ClaudePath), func(c context.Context) (core.ClaudeStatus, error) {
		return a.b.CheckClaude(c, strings.TrimSpace(cfg.ClaudePath)), nil
	}, ui.QueryStaleTime(10*time.Minute))
	models := claude.Data.Models
	model := v.Model
	if model == "" {
		model = cfg.WorkerModel
	}
	label := model
	for _, o := range modelChoices(models, model) {
		if o.Value == model {
			label = o.Label
		}
	}
	effortLabel := "Default effort"
	if v.Effort != "" {
		effortLabel = strings.ToUpper(v.Effort[:1]) + v.Effort[1:]
	} else if cfg.WorkerEffort != "" {
		effortLabel = strings.ToUpper(cfg.WorkerEffort[:1]) + cfg.WorkerEffort[1:]
	}
	id, effort := v.ID, v.Effort
	set := func(m, e string) {
		a.run("Changing the model", func(ctx context.Context) error { return a.b.SetThreadModel(ctx, id, m, e) })
	}
	trigger := ui.Div().Row().Align(ui.Center).Gap(4).PaddingX(space2).Height(ui.Px(26)).Radius(radiusS).
		Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
		Role(access.RoleButton).Label("Model and effort").Tooltip("Model and effort for this thread").
		Child(ui.Text(label + " · " + effortLabel).FontSize(12).TextColor(t.TextMuted).NoWrap()).
		Child(ui.Icon(icons.ChevronDown).Size(12).Color(t.TextSubtle))
	return ui.DropdownMenu(trigger, func() []ui.MenuItem {
		items := []ui.MenuItem{ui.MenuSection("Model")}
		for _, o := range modelChoices(models, model) {
			m := o.Value
			items = append(items, ui.MenuItem{Label: o.Label, Check: ui.MenuCheckRadio, Checked: m == model,
				OnClick: func() { set(m, effort) }})
		}
		items = append(items, ui.MenuSeparator(), ui.MenuSection("Effort"))
		for _, o := range effortChoices(models, model, effort) {
			e := o.Value
			l := o.Label
			if e == "" {
				l = "Default (Settings)"
			}
			items = append(items, ui.MenuItem{Label: l, Check: ui.MenuCheckRadio, Checked: e == effort,
				OnClick: func() { set(model, e) }})
		}
		return items
	}).Key("model-" + v.ID)
}

// placeholderFor says what Solo uses when a command is left empty.
func placeholderFor(detected, what string) string {
	if detected == "" {
		return "None found; enter " + what + " command"
	}
	return "Detected: " + detected
}

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

// repoRulesEditor edits the branch naming rules that replace the global ones for a repository: a row
// per repository, and a menu to add one.
func (a *App) repoRulesEditor(ctx *ui.Context, rules *ui.Signal[map[string]string]) ui.Element {
	t := ctx.Theme()
	cur := rules.Get()
	var rows []ui.Element
	for _, repo := range slices.Sorted(maps.Keys(cur)) {
		r := repo
		name := filepath.Base(r)
		rows = append(rows, ui.Div().Column().Gap(space2).PaddingLeft(space4).PaddingRight(space3).PaddingY(space3).
			Child(ui.Div().Row().Align(ui.Center).Gap(space2).
				Child(ui.Icon(icons.FolderGit2).Size(15).Color(t.TextMuted)).
				Child(ui.Text(name).FontSize(13).Medium().Grow(1).Truncate()).
				Child(ui.IconButton(icons.X).Ghost().Small().Label("Remove the rules for "+name).Tooltip("Use the rules above again").OnClick(func() {
					next := maps.Clone(rules.Peek())
					delete(next, r)
					rules.Set(next)
				}))).
			Child(ui.Div().PaddingRight(space1).Child(ui.Component(func(ctx *ui.Context) ui.Element {
				text := ui.Local(ctx, "rules", cur[r])
				return ui.TextArea(text).Placeholder("Rules for " + name).Label("Branch naming rules for " + name).
					MinRows(1).MaxHeight(ui.Px(120)).Width(ui.Full).
					OnChange(func(v string) {
						next := maps.Clone(rules.Peek())
						next[r] = v
						rules.Set(next)
					})
			}).Key("rules-"+r))))
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
	if len(rows) == 0 {
		rows = append(rows, ui.Div().PaddingX(space4).PaddingY(space3).
			Child(caption(ctx, "None yet.")))
	}
	if len(add) > 0 {
		rows = append(rows, ui.Div().Row().PaddingX(space3).PaddingY(space2).
			Child(ui.DropdownMenu(ui.Button("Add a repository").Icon(icons.Plus).Ghost().Small(),
				func() []ui.MenuItem { return add })))
	}
	return settingsSection(ctx, "Per-repository rules", "These replace the rules above for that repository.", nil, rows...)
}

// settingsTabs are Settings' pages, listed down its left side as t3code does.
var settingsTabs = []struct {
	value, label string
	icon         *icons.Icon
}{
	{"general", "General", icons.Settings2},
	{"repos", "Repositories", icons.FolderGit2},
	{"models", "Models", icons.Bot},
	{"branches", "Branches", icons.GitBranch},
	{"prs", "Pull requests", icons.GitPullRequest},
	{"history", "History", icons.Archive},
}

// settingsNav is Settings' list of pages.
func settingsNav(ctx *ui.Context, tab *ui.Signal[string]) ui.Element {
	t := ctx.Theme()
	nav := ui.Div().Column().Gap(2).Width(ui.Px(184)).Shrink(0).Role(access.RoleTabList).Label("Settings pages")
	compact := isCompact(ctx)
	if compact {
		// A row across the top, which scrolls sideways when the pages don't fit.
		nav = ui.Div().Row().Gap(2).Shrink(0).ScrollX().Role(access.RoleTabList).Label("Settings pages")
	}
	for _, p := range settingsTabs {
		value := p.value
		on := tab.Get() == value
		item := ui.Div().Row().Align(ui.Center).Gap(space2 + 2).PaddingX(space3).Height(ui.Px(34)).Radius(radiusS).
			Cursor(ui.CursorPointer).Role(access.RoleTab).Label(p.label).AccessSelected(on).Key("settings-tab-" + value).
			OnClick(func() { tab.Set(value) })
		if compact {
			item = item.Shrink(0)
		}
		icon, text := t.TextMuted, t.TextMuted
		if on {
			item = item.Background(t.SurfaceActive)
			icon, text = t.Text, t.Text
		} else {
			item = item.Hover(ui.Style{Background: t.SurfaceHover})
		}
		nav = nav.Child(item.
			Child(ui.Icon(p.icon).Size(15).Color(icon)).
			Child(ui.Text(p.label).FontSize(13).Medium().TextColor(text).NoWrap()))
	}
	return nav
}

// settingsPage is a page of Settings: its name, an optional line about it, and its sections.
func settingsPage(ctx *ui.Context, title, help string, sections ...ui.Element) ui.Element {
	t := ctx.Theme()
	head := ui.Div().Column().Gap(space1).PaddingX(1).
		Child(ui.Text(title).FontSize(20).FontWeight(ui.FontSemibold).TextColor(t.Text))
	if help != "" {
		head = head.Child(caption(ctx, help))
	}
	page := ui.Div().Column().Gap(space6).Child(head)
	for _, s := range sections {
		if s != nil {
			page = page.Child(s)
		}
	}
	return page
}

// settingsSection is a titled group of settings: rows in a rounded card, divided by rules (t3code's
// SettingsSection). The title is a small label lined up with the rows' names, over help when there is
// some; side is shown at its right.
// Nil rows are left out.
func settingsSection(ctx *ui.Context, title, help string, side ui.Element, rows ...ui.Element) ui.Element {
	t := ctx.Theme()
	label := ui.Div().Column().Gap(space1 + 2).Grow(1).MinWidth(ui.Px(0)).
		Child(ui.Text(strings.ToUpper(title)).FontSize(11).FontWeight(ui.FontSemibold).TextColor(t.TextSubtle))
	if help != "" {
		label = label.Child(caption(ctx, help))
	}
	head := ui.Div().Row().Align(ui.End).Gap(space2).PaddingLeft(space4 + 1).PaddingRight(space4 + 1).Child(label)
	if side != nil {
		head = head.Child(side)
	}
	card := ui.Div().Column().Radius(radiusL).Border(1, t.Border).Background(t.Background)
	n := 0
	for _, r := range rows {
		if r == nil {
			continue
		}
		cell := ui.Div().Column().Child(r)
		if n > 0 {
			cell = cell.BorderEdges(ui.Edges{Top: 1}, t.Border)
		}
		card = card.Child(cell)
		n++
	}
	return ui.Div().Column().Gap(space2).Child(head).Child(card)
}

// settingLabel is a setting's name over what it does.
func settingLabel(ctx *ui.Context, title, help string) *ui.DivElement {
	col := ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).Child(subhead(title))
	if help != "" {
		col = col.Child(caption(ctx, help))
	}
	return col
}

// settingRow is one setting: its name and what it does on the left, its control on the right.
func settingRow(ctx *ui.Context, title, help string, control ui.Element) ui.Element {
	if isCompact(ctx) {
		// A narrow window keeps a small control, such as a switch, beside the name, and puts a wide one
		// under it.
		return ui.Div().Row().Wrap().Align(ui.Center).Gap(space3).PaddingX(space4).PaddingY(space3).MinHeight(ui.Px(60)).
			Child(settingLabel(ctx, title, help).Basis(ui.Px(180))).
			Child(ui.Div().Row().Wrap().Align(ui.Center).Gap(space2).MaxWidth(ui.Full).Child(control))
	}
	return ui.Div().Row().Align(ui.Center).Gap(space6).PaddingX(space4).PaddingY(space3).MinHeight(ui.Px(60)).
		Child(settingLabel(ctx, title, help)).
		Child(ui.Div().Row().Align(ui.Center).Gap(space2).Shrink(0).Child(control))
}

// settingBlock is a setting whose control needs the full width, such as a text box, under its name.
func settingBlock(ctx *ui.Context, title, help string, control ui.Element) ui.Element {
	return ui.Div().Column().Gap(space2 + 2).PaddingX(space4).PaddingY(space3).
		Child(settingLabel(ctx, title, help)).
		Child(control)
}

// Widths of the model and effort pickers, shared by their column labels.
const (
	modelPickW  = 150
	effortPickW = 110
)

// pickerHeads labels the model and effort columns over a section's pickers.
func pickerHeads(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	if isCompact(ctx) {
		return nil // the pickers sit under each name there, not in columns
	}
	head := func(s string) ui.Element {
		return ui.Text(strings.ToUpper(s)).FontSize(11).FontWeight(ui.FontSemibold).TextColor(t.TextSubtle)
	}
	return ui.Div().Row().Gap(space2).
		Child(ui.Div().Width(ui.Px(modelPickW)).Child(head("Model"))).
		Child(ui.Div().Width(ui.Px(effortPickW)).Child(head("Effort")))
}

// settingsSaver saves Settings a moment after the last change, so typing doesn't save on every key.
type settingsSaver struct {
	timer *time.Timer
	last  core.Settings // what was last saved
	apply func(*core.Settings)
}

// settingsDialog edits Solo's settings, a page per area. Changes save as they're made, as in t3code.
func (a *App) settingsDialog(ctx *ui.Context) ui.Element {
	if !a.settings.Get() {
		return nil
	}
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		initial := a.b.Settings()
		folders := ui.Local(ctx, "folders", initial.Folders)
		newFolder := ui.Local(ctx, "new-folder", "")
		tab := ui.Local(ctx, "tab", cmp.Or(a.settingsTab, "general"))
		orchModel := ui.Local(ctx, "orch-model", initial.OrchestratorModel)
		workerModel := ui.Local(ctx, "worker-model", initial.WorkerModel)
		namingModel := ui.Local(ctx, "naming-model", initial.NamingModel)
		orchEffort := ui.Local(ctx, "orch-effort", initial.OrchestratorEffort)
		soloSession := ui.Local(ctx, "solo-session", fmt.Sprint(initial.SoloSessionTokens))
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
		editorCmd := ui.Local(ctx, "editor-cmd", initial.EditorCommand)
		terminalCmd := ui.Local(ctx, "terminal-cmd", initial.TerminalCommand)
		notifications := ui.Local(ctx, "notifications", !initial.NoNotifications)
		resume := ui.Local(ctx, "resume", !initial.NoResumeAfterRestart)
		detected := ui.Local(ctx, "detected", [2]string{launch.Detect("editor"), launch.Detect("terminal")}).Peek()
		showCosts := ui.Local(ctx, "show-costs", initial.ShowCosts)
		claudePath := ui.Local(ctx, "claude-path", initial.ClaudePath)
		checked := ui.Local(ctx, "claude-checked", strings.TrimSpace(initial.ClaudePath))
		saved := ui.Local(ctx, "saved", "") // "", "saving" or "saved"

		// Save what changed, a moment after the last change.
		apply := func(s *core.Settings) {
			s.Folders, s.ShowCosts, s.ClaudePath = folders.Get(), showCosts.Get(), strings.TrimSpace(claudePath.Get())
			s.EditorCommand, s.TerminalCommand = strings.TrimSpace(editorCmd.Get()), strings.TrimSpace(terminalCmd.Get())
			s.NoNotifications = !notifications.Get()
			s.NoResumeAfterRestart = !resume.Get()
			s.OrchestratorModel, s.WorkerModel, s.NamingModel = orchModel.Get(), workerModel.Get(), namingModel.Get()
			s.OrchestratorEffort, s.WorkerEffort, s.NamingEffort = orchEffort.Get(), workerEffort.Get(), namingEffort.Get()
			s.SoloSessionTokens, _ = strconv.Atoi(soloSession.Get())
			s.BranchRules, s.RepoBranchRules = strings.TrimSpace(branchRules.Get()), repoRules.Get()
			s.AutoMerge, s.MergeMethod, s.OnChecksFail = autoMerge.Get(), mergeMethod.Get(), onFail.Get()
			s.PRModel, s.PREffort, s.PRRules, s.SkipPRDescription = prModel.Get(), prEffort.Get(), strings.TrimSpace(prRules.Get()), !prDescribe.Get()
		}
		saver := ui.Local(ctx, "saver", &settingsSaver{last: initial}).Peek()
		want := initial
		apply(&want)
		saveNow := func() {
			if saver.timer != nil {
				saver.timer.Stop()
				saver.timer = nil
			}
			if saver.apply == nil {
				return
			}
			do := saver.apply
			saver.apply = nil
			a.run("Saving settings", func(context.Context) error {
				err := a.b.UpdateSettings(do)
				a.onUI(func() {
					if err == nil && saver.apply == nil {
						saved.Set("saved")
					}
				})
				return err
			})
		}
		if !reflect.DeepEqual(want, saver.last) {
			saver.last = want
			snap := want
			saver.apply = func(s *core.Settings) {
				s.Folders, s.ShowCosts, s.ClaudePath = snap.Folders, snap.ShowCosts, snap.ClaudePath
				s.EditorCommand, s.TerminalCommand = snap.EditorCommand, snap.TerminalCommand
				s.NoNotifications, s.NoResumeAfterRestart = snap.NoNotifications, snap.NoResumeAfterRestart
				s.OrchestratorModel, s.WorkerModel, s.NamingModel = snap.OrchestratorModel, snap.WorkerModel, snap.NamingModel
				s.OrchestratorEffort, s.WorkerEffort, s.NamingEffort = snap.OrchestratorEffort, snap.WorkerEffort, snap.NamingEffort
				s.SoloSessionTokens = snap.SoloSessionTokens
				s.BranchRules, s.RepoBranchRules = snap.BranchRules, snap.RepoBranchRules
				s.AutoMerge, s.MergeMethod, s.OnChecksFail = snap.AutoMerge, snap.MergeMethod, snap.OnChecksFail
				s.PRModel, s.PREffort, s.PRRules, s.SkipPRDescription = snap.PRModel, snap.PREffort, snap.PRRules, snap.SkipPRDescription
			}
			if saver.timer != nil {
				saver.timer.Stop()
			}
			saver.timer = time.AfterFunc(600*time.Millisecond, func() { a.onUI(saveNow) })
			if saved.Peek() != "saving" {
				a.onUI(func() { saved.Set("saving") })
			}
		}
		closeDialog := func() {
			saveNow()
			a.settings.Set(false)
		}

		addPath := func(f string) {
			f = tildePath(strings.TrimSpace(f))
			if f == "" || slices.Contains(folders.Peek(), f) {
				return
			}
			folders.Set(append(slices.Clone(folders.Peek()), f))
		}
		add := func() {
			addPath(newFolder.Peek())
			newFolder.Set("")
		}
		pick := func() {
			home, _ := os.UserHomeDir()
			ctx.Window().PickFolder(dialog.FileOptions{Title: "Add a repository folder", Folder: home}, func(r dialog.FileResult) {
				switch {
				case r.Err != nil:
					a.notify(ui.ToastError, "Couldn't open the folder picker", "Type the folder's path instead. "+r.Err.Error())
				case !r.Canceled:
					for _, p := range r.Paths {
						addPath(p)
					}
				}
			})
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
		var claudeRow ui.Element
		if st := claude.Data; claude.Loading || claude.Fetching {
			claudeRow = ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space4).MinHeight(ui.Px(60)).
				Child(ui.ProgressRing().Size(16)).
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
			about := ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
				Child(ui.Text(line).FontSize(13).Medium().TextColor(t.Text).Truncate()).
				Child(caption(ctx, where).TruncateHead())
			row := ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space4).PaddingY(space3).MinHeight(ui.Px(60)).
				Child(ui.Icon(icon).Size(16).Color(color))
			if isCompact(ctx) {
				// The button goes under the lines there, which need the row's width.
				row = row.Wrap()
				about = about.Basis(ui.Px(180))
			}
			claudeRow = row.Child(about).
				Child(ui.Button("Check again").Icon(icons.RefreshCw).Secondary().Small().OnClick(check))
		}
		input := func(in *ui.TextInputElement) *ui.TextInputElement { return in.Small().Width(ui.Px(280)) }
		appRow := func(name, what, found, example string, cmd *ui.Signal[string]) ui.Element {
			help, hint := what+" {path} marks where the folder goes.", "e.g. "+example
			if found != "" {
				prog, _, _ := strings.Cut(found, " ")
				help, hint = what+" Leave empty to use "+filepath.Base(prog)+".", filepath.Base(prog)+" … {path}"
			}
			return settingRow(ctx, name, help,
				input(ui.TextInput(cmd).Placeholder(hint).Label(name+" command")))
		}

		general := settingsPage(ctx, "General", "How Solo runs Claude Code, and the apps it opens threads in.",
			settingsSection(ctx, "Claude Code", "", nil,
				claudeRow,
				settingRow(ctx, "Path to claude", "Leave empty to find it on PATH.",
					input(ui.TextInput(claudePath).Placeholder("Find automatically").Label("Path to claude").Key("claude-path").
						OnSubmit(func(string) { check() })))),
			settingsSection(ctx, "Behavior", "", nil,
				settingRow(ctx, "Notifications", "Alerts when a thread needs you, fails or finishes, while Solo is in the background.",
					ui.Switch("Notifications", notifications)),
				settingRow(ctx, "Carry on after a restart", "Threads that were working when Solo closed pick up where they left off.",
					ui.Switch("Carry on after a restart", resume)),
				settingRow(ctx, "Show costs", "What each thread has cost so far, as Claude estimates it.",
					ui.Switch("Show costs", showCosts))),
			settingsSection(ctx, "Apps", "", nil,
				appRow("Editor", "Opens a thread's folder.", detected[0], "code {path}", editorCmd),
				appRow("Terminal", "Opens a terminal in a thread's folder.", detected[1], "ptyxis --working-directory {path}", terminalCmd)))

		// Repositories: the folders to search, what the search found, and folders outside git.
		var folderRows []ui.Element
		for _, f := range folders.Get() {
			folder := f
			folderRows = append(folderRows, ui.Div().Row().Align(ui.Center).Gap(space3).PaddingLeft(space4).PaddingRight(space3).Height(ui.Px(48)).
				Child(ui.Icon(icons.Folder).Size(15).Color(t.TextMuted)).
				Child(ui.Text(folder).FontSize(13).Medium().TruncateHead().Grow(1).MinWidth(ui.Px(0))).
				Child(ui.IconButton(icons.X).Ghost().Small().Label("Remove "+folder).Tooltip("Stop searching here").OnClick(func() {
					folders.Set(slices.DeleteFunc(slices.Clone(folders.Peek()), func(s string) bool { return s == folder }))
				})))
		}
		if len(folderRows) == 0 {
			folderRows = append(folderRows, ui.Div().PaddingX(space4).PaddingY(space3).Child(caption(ctx, "No folders yet. Add the folder that holds your projects.")))
		}
		folderRows = append(folderRows, ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space4).PaddingY(space3).
			Child(ui.TextInput(newFolder).Placeholder("Add a folder path, such as ~/projects").Label("Folder to add").Small().
				Grow(1).Key("new-folder").OnSubmit(func(string) { add() })).
			Child(ui.Button("Add").Secondary().Small().Disabled(strings.TrimSpace(newFolder.Get()) == "").OnClick(add)).
			Child(ui.Button("Choose…").Icon(icons.FolderOpen).Secondary().Small().Label("Choose a folder").OnClick(pick)))
		var found ui.Element
		if repos := a.repos.Get(); len(repos) > 0 {
			parent := filepath.Dir(repos[0])
			for _, r := range repos {
				if filepath.Dir(r) != parent {
					parent = ""
				}
			}
			var rows []ui.Element
			for _, r := range repos {
				row := ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space4).Height(ui.Px(42)).
					Child(ui.Icon(icons.FolderGit2).Size(15).Color(t.TextMuted)).
					Child(ui.Text(filepath.Base(r)).FontSize(13).Medium())
				if parent == "" {
					row = row.Child(caption(ctx, tildePath(filepath.Dir(r))).TruncateHead().Grow(1).MinWidth(ui.Px(0)))
				}
				rows = append(rows, row)
			}
			title := fmt.Sprintf("Found %d repositories", len(repos))
			var side ui.Element
			if parent != "" {
				side = caption(ctx, "in "+tildePath(parent)).TruncateHead()
			}
			found = settingsSection(ctx, title, "", side, rows...)
		}
		var plain ui.Element
		if p := a.plain.Get(); len(p) > 0 {
			plain = a.plainFolders(ctx, p)
		}
		reposTab := settingsPage(ctx, "Repositories", "Solo looks for git repositories up to three folders deep in these folders.",
			settingsSection(ctx, "Folders to search", "", nil, folderRows...),
			found,
			plain)

		models := claude.Data.Models
		pickers := func(name string, model, effort *ui.Signal[string]) ui.Element {
			return ui.Div().Row().Gap(space2).
				Child(ui.Select(model, modelChoices(models, model.Get())...).Width(ui.Px(modelPickW)).Label(name + " model")).
				Child(ui.Select(effort, effortChoices(models, model.Get(), effort.Get())...).Width(ui.Px(effortPickW)).Label(name + " effort"))
		}
		// A helper's pickers stand alone, without the column labels of the Models page.
		helperPickers := func(name string, model, effort *ui.Signal[string]) ui.Element {
			efforts := effortChoices(models, model.Get(), effort.Get())
			for i := range efforts {
				efforts[i].Label += " effort"
			}
			return ui.Div().Row().Gap(space2).
				Child(ui.Select(model, modelChoices(models, model.Get())...).Width(ui.Px(modelPickW)).Label(name + " model")).
				Child(ui.Select(effort, efforts...).Width(ui.Px(effortPickW + 30)).Label(name + " effort"))
		}
		modelsTab := settingsPage(ctx, "Models", "Effort is how long a model thinks before it acts; Default leaves it to Claude. Changes apply to new sessions.",
			settingsSection(ctx, "Agents", "", pickerHeads(ctx),
				settingRow(ctx, "Solo", "The lead agent you talk to. It plans, follows the threads and reviews their work.", pickers("Solo", orchModel, orchEffort)),
				settingRow(ctx, "Threads", "The agents that do the work, one per task. Solo can pick another model when a task calls for it.", pickers("Threads", workerModel, workerEffort))),
			settingsSection(ctx, "Long conversations", "", nil,
				settingRow(ctx, "Fresh session for Solo after", "Solo moves to a new Claude session at this much context, with a recap and its memory; your chat stays whole. Longer keeps more detail, but every message uses more of your plan.",
					ui.Select(soloSession,
						ui.Option[string]{Value: "60000", Label: "60k tokens"},
						ui.Option[string]{Value: "0", Label: "120k tokens"},
						ui.Option[string]{Value: "250000", Label: "250k tokens"},
						ui.Option[string]{Value: "-1", Label: "Never"}).Width(ui.Px(modelPickW)).Label("Fresh session after"))),
			settingsSection(ctx, "Helpers", "", nil,
				settingRow(ctx, "Branch names and pull request text", "Small models that apply your rules, each set on its page.",
					ui.Div().Row().Gap(space2).
						Child(ui.Button("Branches").IconEnd(icons.ArrowRight).Ghost().Small().OnClick(func() { tab.Set("branches") })).
						Child(ui.Button("Pull requests").IconEnd(icons.ArrowRight).Ghost().Small().OnClick(func() { tab.Set("prs") })))))

		branchesTab := settingsPage(ctx, "Branches", "How Solo names the branch each thread works on.",
			settingsSection(ctx, "Naming", "", nil,
				settingBlock(ctx, "How to name branches", "In your words: a prefix, a ticket format, a length. Leave empty to name branches solo/<title>.",
					ui.TextArea(branchRules).Placeholder("For example: feature/<ticket>-<short-summary>, lowercase, hyphens, under 50 characters. Use fix/ for bug fixes.").
						Label("Branch naming rules").MinRows(1).MaxHeight(ui.Px(140)).Width(ui.Full).Key("branch-rules")),
				settingRow(ctx, "Model", "Applies the rules. A small, fast model is enough.", helperPickers("Branch names", namingModel, namingEffort))),
			a.repoRulesEditor(ctx, repoRules))

		prsTab := settingsPage(ctx, "Pull requests", "Solo pushes a thread's branch and opens a pull request (GitHub, with gh) or merge request (GitLab, with glab). Repositories without a remote merge locally.",
			settingsSection(ctx, "Title and description", "", nil,
				settingRow(ctx, "Write a description", "Off: pull requests get a title only.", ui.Switch("Write a description", prDescribe)),
				settingBlock(ctx, "How to write titles and descriptions", "A model applies this to the thread's commits, changes and summary.",
					ui.TextArea(prRules).Placeholder("For example: Conventional Commit title with the ticket, like feat(auth): OW-123 refresh tokens. Description: Summary, Changes, Testing.").
						Label("Pull request text rules").MinRows(1).MaxHeight(ui.Px(140)).Width(ui.Full).Key("pr-rules")),
				settingRow(ctx, "Model", "Writes the title and description.", helperPickers("Pull request text", prModel, prEffort))),
			settingsSection(ctx, "Merging", "", nil,
				settingRow(ctx, "Auto-merge", "Merge once required checks and reviews pass, with the forge's auto-merge.", ui.Switch("Auto-merge", autoMerge)),
				settingRow(ctx, "Merge method", "How the branch lands on its base.", ui.Select(mergeMethod, ui.Option[string]{Value: "squash", Label: "Squash"},
					ui.Option[string]{Value: "merge", Label: "Merge commit"},
					ui.Option[string]{Value: "rebase", Label: "Rebase"}).Width(ui.Px(180)).Label("Merge method")),
				settingRow(ctx, "When checks fail", "Solo can send the failure to the thread to fix.", ui.Select(onFail,
					ui.Option[string]{Value: core.ChecksFix, Label: "Solo fixes it"},
					ui.Option[string]{Value: core.ChecksNotify, Label: "Just notify me"}).Width(ui.Px(180)).Label("When checks fail"))))

		var page ui.Element
		switch tab.Get() {
		case "repos":
			page = reposTab
		case "models":
			page = modelsTab
		case "branches", "git":
			page = branchesTab
		case "prs":
			page = prsTab
		case "history":
			page = settingsPage(ctx, "History", "Restoring a thread puts it back in your list with its worktree, so you can keep talking to it.",
				a.historyList(ctx))
		default:
			page = general
		}
		// One height for every page, so the dialog doesn't move between them: the tallest page's, or what
		// the window allows beside the title and footer. A longer page scrolls.
		winW, winH := ctx.WindowSize()
		bodyH := min(max(winH-220, 360), 660)
		body := ui.Div().Row().Width(ui.Full).Height(ui.Px(bodyH)).Gap(space6).
			Child(settingsNav(ctx, tab)).
			Child(ui.Div().Column().Grow(1).MinWidth(ui.Px(0)).ScrollY().PaddingRight(space4).PaddingBottom(space6).
				Key("settings-page-" + tab.Get()).
				Child(page))
		width := ui.Px(900)
		if isCompact(ctx) {
			// A narrow window gives Settings nearly all of itself: the pages' names across the top, and the
			// page under them.
			width = ui.Px(max(winW-2*space2, 280))
			body = ui.Div().Column().Width(ui.Full).Height(ui.Px(max(winH-200, 240))).Gap(space3).
				Child(settingsNav(ctx, tab)).
				Child(ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).ScrollY().PaddingBottom(space4).
					Key("settings-page-" + tab.Get()).
					Child(page))
		}

		status := "Changes save as you make them"
		switch saved.Get() {
		case "saving":
			status = "Saving…"
		case "saved":
			status = "Saved"
		}
		doneFocus := ui.Local(ctx, "done-focus", ui.NewFocusHandle()).Peek()
		done := ui.Button("Done").Secondary().Key("settings-done").FocusHandle(doneFocus).OnClick(closeDialog)
		return ui.Modal("Settings", body).Width(width).MaxWidth(width).CloseButton(true).InitialFocus(doneFocus).
			Footer(ui.Div().Row().Align(ui.Center).Gap(space1+2).Grow(1).
				Child(ui.Icon(icons.Check).Size(13).Color(t.TextSubtle)).
				Child(caption(ctx, status)), done).
			Default(done).
			OnClose(closeDialog).Key("settings")
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
	if len(done) == 0 {
		return settingsSection(ctx, "Done and discarded", "", nil,
			ui.Div().PaddingX(space4).PaddingY(space3).Child(caption(ctx, "Nothing yet. Threads you mark done show up here.")))
	}
	slices.SortFunc(done, func(x, y core.ThreadView) int { return y.DoneAt.Compare(x.DoneAt) })
	rows := make([]ui.Element, 0, len(done))
	for _, th := range done {
		id := th.ID
		var mark ui.Element = statusMark(ctx, shownStatus(th))
		if th.Status == core.StatusDiscarded {
			mark = ui.Div().Tooltip("Discarded").Child(ui.Icon(icons.Trash).Size(15).Color(t.TextSubtle))
		}
		rows = append(rows, ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space4).PaddingY(space3).
			Child(mark).
			Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
				Child(ui.Text(th.Title).FontSize(13).Medium().TextColor(t.Text).Truncate()).
				Child(caption(ctx, repoLabel(th)+" · "+statusLabel(th)+" "+doneAgo(th.DoneAt)).Truncate())).
			Child(ui.Div().Shrink(0).Child(ui.Button("Restore").Icon(icons.RotateCcw).Secondary().Small().
				Tooltip("Back to your list").OnClick(func() { a.restore(id) }))))
	}
	return settingsSection(ctx, "Done and discarded", "", nil, rows...)
}

// confirmDialog asks before a destructive action.
func (a *App) confirmDialog(ctx *ui.Context) ui.Element {
	c := a.confirm.Get()
	if c == nil {
		return nil
	}
	do := ui.Button(c.action).Danger()
	if c.safe {
		do = ui.Button(c.action).Primary()
	}
	do = do.OnClick(func() {
		a.confirm.Set(nil)
		c.run()
	})
	winW, _ := ctx.WindowSize()
	return ui.Modal(c.title, ui.Div().Width(ui.Full).Child(muted(ctx, c.body))).Width(ui.Px(min(460, winW-2*space2))).
		Footer(ui.Button("Cancel").Secondary().OnClick(func() { a.confirm.Set(nil) }), do).
		OnClose(func() { a.confirm.Set(nil) }).Key("confirm")
}

// doneAgo says when a thread was marked done, after its status: "Merged 20h ago".
func doneAgo(t time.Time) string {
	if s := shortAgo(t); s != "now" {
		return s + " ago"
	}
	return "just now"
}

// tildePath writes a path in the home folder as ~/…, the way folders are shown.
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || p == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return p
}

// plainFolders lists the folders Solo found that aren't in git, each with a button that puts it in
// git so threads get their own copy and a diff.
func (a *App) plainFolders(ctx *ui.Context, plain []string) ui.Element {
	t := ctx.Theme()
	busy := ui.Local(ctx, "init-busy", "")
	rows := make([]ui.Element, 0, len(plain))
	parent := filepath.Dir(plain[0])
	for _, p := range plain {
		if filepath.Dir(p) != parent {
			parent = ""
		}
	}
	help := "Threads work in these directly, with no copy of their own or diff. Put in git adds a .gitignore if there's none and commits what's there."
	if parent != "" {
		help = "Folders in " + tildePath(parent) + " without git. " + help
	}
	for _, p := range plain {
		dir := p
		name := filepath.Base(dir)
		btn := ui.Button("Put in git").Icon(icons.GitBranchPlus).Secondary().Small().Label("Put " + name + " in git").
			Tooltip("Runs git init in " + tildePath(dir))
		if busy.Get() == dir {
			btn = btn.Loading(true)
		} else if busy.Get() != "" {
			btn = btn.Disabled(true)
		} else {
			btn = btn.OnClick(func() {
				busy.Set(dir)
				go func() {
					_, err := a.b.InitRepo(context.Background(), dir)
					a.onUI(func() {
						busy.Set("")
						if err != nil {
							a.notify(ui.ToastError, "Couldn't put "+name+" in git", err.Error())
							return
						}
						a.notify(ui.ToastSuccess, name+" is in git now", "Threads can work in it.")
						a.plain.Set(a.b.PlainFolders())
						a.repos.Set(a.b.RepoPaths())
					})
				}()
			})
		}
		row := ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space4).Height(ui.Px(48)).
			Child(ui.Icon(icons.FolderX).Size(15).Color(t.TextMuted)).
			Child(ui.Text(name).FontSize(13).Medium())
		if parent == "" {
			row = row.Child(caption(ctx, tildePath(filepath.Dir(dir))).TruncateHead())
		}
		rows = append(rows, row.Child(ui.Div().Grow(1)).Child(btn))
	}
	return settingsSection(ctx, "Not in git", help, nil, rows...)
}
