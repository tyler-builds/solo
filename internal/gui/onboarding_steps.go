package gui

// The setup's steps after Welcome and Claude Code (onboarding.go): Models, Repositories, Pull requests
// and Preferences. Each offers the choices Settings offers, from the same lists, and saves them as
// Settings does.

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/launch"
)

// ---- Models ----

// modelRole is one of the two agents whose model and effort the Models step sets: Solo, or its threads.
type modelRole struct {
	key, name, does string
	icon            ui.Element
	model, effort   string
	recommended     string // the default model
	set             func(s *core.Settings, model, effort string)
}

// setupModels picks the model and effort for Solo and for its threads, from the models Claude Code
// offers (the list Settings › Models offers).
func (a *App) setupModels(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	l := setupLayoutOf(ctx)
	cfg := a.cfg.Get()
	q := a.claudeQuery(ctx, strings.TrimSpace(cfg.ClaudePath))
	models := q.Data.Models
	def := core.DefaultSettings()
	solo := modelRole{key: "solo", name: "Solo", icon: brandMark(22),
		does:  "Plans the work, follows the threads and reviews what they make. A capable model here plans and reviews better.",
		model: cmp.Or(cfg.OrchestratorModel, def.OrchestratorModel), effort: cfg.OrchestratorEffort, recommended: def.OrchestratorModel,
		set: func(s *core.Settings, m, e string) { s.OrchestratorModel, s.OrchestratorEffort = m, e }}
	threads := modelRole{key: "threads", name: "Threads", icon: ui.Icon(icons.GitBranch).Size(20).Color(t.TextMuted),
		does:  "Write the code, often several at once, and use most of your plan. Faster models do more for less; capable ones take on harder tasks.",
		model: cmp.Or(cfg.WorkerModel, def.WorkerModel), effort: cfg.WorkerEffort, recommended: def.WorkerModel,
		set: func(s *core.Settings, m, e string) { s.WorkerModel, s.WorkerEffort = m, e }}
	var note ui.Element = bodyText(ctx, "Effort is how long a model thinks before it acts: higher is more careful, but slower and uses more, and Default leaves it to Claude. Solo can give a thread another model when a task calls for it, and Settings › Models has these choices too.").
		MaxWidth(ui.Px(setupProseWidth))
	if q.Loading {
		note = checking(ctx, "Asking Claude Code which models you have…")
	}
	return ui.Div().Column().Gap(space8).
		Child(setupHead(ctx, "Models",
			"Choose the Claude model and effort for Solo and for its threads. More capable models handle harder work, but they are slower and use more of your plan.")).
		Child(setupPairView(l, a.modelChooser(ctx, l, models, solo), a.modelChooser(ctx, l, models, threads))).
		Child(note)
}

// modelChooser is one agent's model, as cards to pick from (Claude Code's own models, with its words
// for each; the rest in a list under them), and its effort.
func (a *App) modelChooser(ctx *ui.Context, l setupLayout, models []agent.ModelInfo, r modelRole) ui.Element {
	t := ctx.Theme()
	choices := modelChoices(models, r.model)
	about := map[string]string{}
	for _, m := range models {
		about[m.Value] = m.Description
	}
	// The cards are the models Claude Code names by family (opus, sonnet…), which follow its newest
	// versions; the pinned versions wait in the list under them, with the rest.
	var cards []ui.Option[string]
	for _, c := range choices {
		if !strings.HasPrefix(c.Value, "claude-") || c.Value == r.model {
			cards = append(cards, c)
		}
	}
	set := func(m, e string) { a.saveSettings(func(s *core.Settings) { r.set(s, m, e) }) }
	list := ui.Div().Column().Gap(space2).Role(access.RoleGroup).Label(r.name + " model")
	for _, c := range cards {
		value, on := c.Value, c.Value == r.model
		ring := ui.Div().Center().Size(ui.Px(18), ui.Px(18)).Radius(9).Border(1.5, t.BorderStrong).Shrink(0)
		if on {
			ring = ui.Div().Center().Size(ui.Px(18), ui.Px(18)).Radius(9).Border(1.5, t.Text).Shrink(0).
				Child(ui.Div().Size(ui.Px(8), ui.Px(8)).Radius(4).Background(t.Text))
		}
		// The model's name, and Claude Code's words for it beside it (under it, in a compact window).
		name := ui.Div().Row().Align(ui.Center).Gap(space2).Shrink(0).
			Child(ui.Text(c.Label).FontSize(15).Medium().TextColor(t.Text).NoWrap()).
			Child(ui.If(value == r.recommended, chip(ctx, "Recommended")))
		text := ui.Div().Row().Align(ui.Center).Gap(space3).Grow(1).MinWidth(ui.Px(0)).Child(name)
		if l.compact {
			text = ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).Child(name)
		}
		if d := about[value]; d != "" {
			desc := bodyText(ctx, d)
			if !l.compact {
				desc = desc.Truncate().Grow(1).MinWidth(ui.Px(0))
			}
			text = text.Child(desc)
		}
		card := ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space4).PaddingY(space2).MinHeight(ui.Px(44)).
			Radius(radiusL+2).Border(1, t.Border).Background(t.Background).Cursor(ui.CursorPointer).
			Role(access.RoleRadioButton).Label(c.Label).AccessChecked(on).TabIndex(0).Key("model-" + r.key + "-" + value).
			OnClick(func() { set(value, r.effort) }).
			Child(ring).Child(text)
		if on {
			card = card.Border(1, t.TextSubtle).Background(t.SurfaceActive)
		} else {
			card = card.Hover(ui.Style{Background: t.SurfaceHover, BorderColor: t.BorderStrong})
		}
		list = list.Child(card)
	}
	// Every model Claude Code offers, pinned versions too, in a list beside the cards' label.
	var more ui.Element
	if len(cards) < len(choices) {
		more = ui.DropdownMenu(ui.Button("All versions").IconEnd(icons.ChevronDown).Ghost().Small().Key("model-more-"+r.key).
			Tooltip("Every model Claude Code offers, older versions too"),
			func() []ui.MenuItem {
				items := make([]ui.MenuItem, 0, len(choices))
				for _, c := range choices {
					m := c.Value
					items = append(items, ui.MenuItem{Label: c.Label, Check: ui.MenuCheckRadio, Checked: m == r.model,
						OnClick: func() { set(m, r.effort) }})
				}
				return items
			})
	}
	efforts := effortChoices(models, r.model, r.effort)
	effort := ui.Local(ctx, "effort-"+r.key, r.effort)
	if effort.Peek() != r.effort {
		effort.Set(r.effort)
	}
	onEffort := func(e string) {
		if e != r.effort {
			set(r.model, e)
		}
	}
	var effortControl ui.Element = ui.ToggleGroup(effort, efforts...).Label(r.name + " effort").Key("effort-" + r.key).OnChange(onEffort)
	if l.compact || l.w < 1000 && l.two { // six options need more room than a narrow column has
		effortControl = ui.Select(effort, efforts...).Width(ui.Px(200)).Label(r.name + " effort").Key("effort-" + r.key).OnChange(onEffort)
	}
	return setupPanel(ctx,
		ui.Div().Column().Gap(space2).
			Child(ui.Div().Row().Align(ui.Center).Gap(space3).
				Child(r.icon).
				Child(ui.Text(r.name).FontSize(18).Semibold().TextColor(t.Text))).
			Child(bodyText(ctx, r.does)),
		ui.Div().Column().Gap(space3).
			Child(ui.Div().Row().Wrap().Align(ui.Center).Gap(space2).MinHeight(ui.Px(28)).
				Child(sectionLabel(ctx, "Model")).Child(ui.Div().Grow(1)).Child(more)).
			Child(list),
		ui.Div().Column().Gap(space3).
			Child(sectionLabel(ctx, "Effort")).
			Child(ui.Div().Row().Child(effortControl))).
		Key("models-" + r.key)
}

// ---- Repositories ----

// expandHome undoes tildePath.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return filepath.Clean(p)
}

// inFolder lists the paths that are folder or inside it.
func inFolder(paths []string, folder string) []string {
	dir := expandHome(folder)
	var out []string
	for _, p := range paths {
		if p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) {
			out = append(out, p)
		}
	}
	return out
}

// addFolders adds folders to the ones searched, as Settings › Repositories does, and shows what the
// search finds as soon as it is done.
func (a *App) addFolders(paths []string) {
	var add []string
	for _, p := range paths {
		if p = tildePath(strings.TrimSpace(p)); p != "" && !slices.Contains(a.cfg.Peek().Folders, p) {
			add = append(add, p)
		}
	}
	if len(add) == 0 {
		return
	}
	a.setupAdding.Set(append(slices.Clone(a.setupAdding.Peek()), add...))
	a.run("Adding the folder", func(context.Context) error {
		err := a.b.UpdateSettings(func(s *core.Settings) {
			for _, p := range add {
				s.Folders = withFolder(s.Folders, p)
			}
		})
		a.onUI(func() {
			a.setupAdding.Set(slices.DeleteFunc(slices.Clone(a.setupAdding.Peek()), func(p string) bool { return slices.Contains(add, p) }))
			a.syncFolders()
		})
		return err
	})
}

// removeFolder stops searching a folder.
func (a *App) removeFolder(folder string) {
	a.run("Removing the folder", func(context.Context) error {
		err := a.b.UpdateSettings(func(s *core.Settings) {
			s.Folders = slices.DeleteFunc(slices.Clone(s.Folders), func(f string) bool { return f == folder })
		})
		a.onUI(a.syncFolders)
		return err
	})
}

// syncFolders reads the folders and what was found in them now, rather than at the next refresh.
func (a *App) syncFolders() {
	a.cfg.Set(a.b.Settings())
	a.repos.Set(a.b.RepoPaths())
	a.plain.Set(a.b.PlainFolders())
}

// setupRepos adds the folders that hold the user's projects, with the system's folder picker or from
// the usual places Solo found, and shows at once what it found in each: the ways to add on one side,
// what was found on the other.
func (a *App) setupRepos(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	l := setupLayoutOf(ctx)
	folders := a.cfg.Get().Folders
	repos, plain := a.repos.Get(), a.plain.Get()
	adding := a.setupAdding.Get()
	typed := ui.Local(ctx, "typed", "")

	addTyped := func() {
		a.addFolders([]string{typed.Peek()})
		typed.Set("")
	}
	choose := ui.Button("Choose a folder…").Icon(icons.FolderOpen).Secondary().Key("repos-choose").
		OnClick(func() { a.chooseFolders(ctx, a.addFolders) })
	if !l.compact {
		choose = choose.Large()
	}
	adders := ui.Div().Column().Gap(space3).
		Child(ui.Div().Row().Child(choose)).
		Child(ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(ui.TextInput(typed).Placeholder("or type a path, such as ~/projects").Label("Folder to add").
				Grow(1).MinWidth(ui.Px(0)).Key("repos-path").OnSubmit(func(string) { addTyped() })).
			Child(ui.Button("Add").Secondary().Key("repos-path-add").
				Disabled(strings.TrimSpace(typed.Get()) == "").OnClick(addTyped))).
		Child(bodyText(ctx, "A folder that isn't in git works too: its threads edit it in place, without a branch of their own."))

	// What was found, over the folders searched.
	shown := append(slices.Clone(folders), adding...)
	var rows []ui.Element
	for _, f := range shown {
		folder := f
		pending := slices.Contains(adding, f) && !slices.Contains(folders, f)
		found, outside := inFolder(repos, f), inFolder(plain, f)
		var detail ui.Element
		switch {
		case pending:
			detail = ui.Div().Row().Align(ui.Center).Gap(space2).
				Child(ui.ProgressRing().Size(12)).Child(caption(ctx, "Looking for repositories…"))
		case len(found) > 0:
			line := count(len(found), "repository", "repositories") // the line over the folders names them
			if len(outside) > 0 {
				line += " · " + count(len(outside), "folder", "folders") + " not in git"
			}
			detail = caption(ctx, line)
		case len(outside) == 1 && outside[0] == expandHome(f):
			detail = caption(ctx, "Not in git. Threads work in it directly.")
		case len(outside) > 0:
			detail = caption(ctx, "No repositories; "+count(len(outside), "folder", "folders")+" not in git: "+nameList(outside, 4))
		default:
			detail = ui.Text("Nothing found here. Pick the folder that holds your projects.").FontSize(12).TextColor(t.Warning)
		}
		icon := icons.FolderGit2
		if !pending && len(found) == 0 {
			icon = icons.Folder
		}
		row := ui.Div().Row().Align(ui.Center).Gap(space3).PaddingLeft(space4).PaddingRight(space2).PaddingY(space3).
			Key("folder-" + folder).
			Child(ui.Icon(icon).Size(18).Color(t.TextMuted)).
			Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
				Child(ui.Text(folder).FontSize(14).Medium().TextColor(t.Text).TruncateHead()).
				Child(detail))
		if !pending {
			row = row.Child(ui.IconButton(icons.X).Ghost().Small().Label("Remove " + folder).Tooltip("Stop searching here").
				OnClick(func() { a.removeFolder(folder) }))
		}
		rows = append(rows, row)
	}
	var found ui.Element
	if len(rows) > 0 {
		var all []string
		for _, f := range folders {
			all = append(all, inFolder(repos, f)...)
		}
		var head ui.Element
		if len(all) > 0 {
			head = ui.Div().Row().Align(ui.Start).Gap(space3).Key("repos-found").
				Child(ui.Div().PaddingTop(1).Child(ui.Icon(icons.CircleCheck).Size(20).Color(t.Success))).
				Child(ui.Text("Found " + count(len(all), "repository", "repositories") + ": " + nameList(all, 6)).
					FontSize(16).Semibold().TextColor(t.Text).Grow(1).MinWidth(ui.Px(0)))
		}
		found = ui.Div().Column().Gap(space4).Child(head).Child(setupCard(ctx, rows...))
	} else if l.two {
		// Before anything is added, the side that shows what was found says what will be there.
		found = ui.Div().Column().Center().Gap(space3).Padding(space8).MinHeight(ui.Px(220)).
			Radius(radius2X).Border(1, t.Border).Background(t.Background).
			Child(ui.Icon(icons.FolderSearch).Size(28).Color(t.TextSubtle)).
			Child(bodyText(ctx, "The folders you add show here, with the repositories Solo finds in them.").Align(ui.Center).MaxWidth(ui.Px(320)))
	}

	// Folders where projects usually are, found to hold repositories: the user adds them, or not.
	var suggestions ui.Element
	if sb := a.setupBackend(); sb != nil {
		q := ui.Query(ctx, "setup-suggest", func(c context.Context) ([]core.FolderSuggestion, error) {
			return sb.SuggestFolders(c), nil
		})
		var sugg []ui.Element
		for _, s := range q.Data {
			p := tildePath(s.Path)
			if slices.Contains(shown, p) {
				continue
			}
			sugg = append(sugg, ui.Div().Row().Align(ui.Center).Gap(space3).PaddingLeft(space4).PaddingRight(space3).PaddingY(space3).
				Key("suggest-"+p).
				Child(ui.Icon(icons.FolderSearch).Size(18).Color(t.TextMuted)).
				Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
					Child(ui.Text(p).FontSize(14).Medium().TextColor(t.Text).TruncateHead()).
					Child(caption(ctx, count(len(s.Repos), "repository", "repositories")+": "+nameList(s.Repos, 5)))).
				Child(ui.Button("Add").Icon(icons.Plus).Secondary().Small().Label("Add "+p).Key("suggest-add-"+p).
					OnClick(func() { a.addFolders([]string{p}) })))
		}
		if len(sugg) > 0 {
			suggestions = ui.Div().Column().Gap(space3).
				Child(sectionLabel(ctx, "Found on this computer")).
				Child(setupCard(ctx, sugg...))
		}
	}
	var later ui.Element
	if len(shown) == 0 {
		later = bodyText(ctx, "You can also add folders later, in Settings › Repositories.")
	}
	left := ui.Div().Column().Gap(space8).
		Child(setupHead(ctx, "Your repositories",
			"Add the folders that hold your projects. Solo finds the git repositories in them, up to three folders deep.")).
		Child(adders)
	if !l.two {
		return left.Child(found).Child(suggestions).Child(later)
	}
	return setupSplitView(l, left.Child(suggestions).Child(later), found)
}

// ---- Pull requests ----

// forgeName is a forge's name.
func forgeName(k forge.Kind) string {
	return map[forge.Kind]string{forge.GitHub: "GitHub", forge.GitLab: "GitLab"}[k]
}

// mergeMeaning says what each merge method does to the base branch.
var mergeMeaning = map[string]string{
	"squash": "Each thread's work lands as one commit on the base branch.",
	"merge":  "The thread's commits land as they are, joined by a merge commit.",
	"rebase": "The thread's commits land one by one on top of the base branch.",
}

// setupPRs shows, for each forge the repositories push to, whether its CLI is ready to open pull
// requests, and the one command to run when it isn't; and, beside it, how pull requests land. It is
// optional: without a forge, work merges locally.
func (a *App) setupPRs(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	l := setupLayoutOf(ctx)
	repos := a.repos.Get()
	head := setupHead(ctx, "Pull requests",
		"Optional. Solo opens pull requests with GitHub's gh or GitLab's glab, signed in as you. Without them, work merges on this machine.")
	sb := a.setupBackend()
	if len(repos) == 0 || sb == nil {
		return ui.Div().Column().Gap(space8).Child(head).Child(setupPanel(ctx,
			statusLine(ctx, icons.Info, t.TextMuted, "No repositories yet",
				bodyText(ctx, "Once you add repositories, this shows what they need. Settings › General can run the setup again."))).
			MaxWidth(ui.Px(setupProseWidth)))
	}
	q := ui.Query(ctx, "setup-forges:"+strings.Join(repos, "\n"), func(c context.Context) ([]core.ForgeStatus, error) {
		return sb.CheckForges(c), nil
	})
	busy := q.Loading || q.Fetching
	problem := slices.ContainsFunc(q.Data, func(f core.ForgeStatus) bool { return !f.Ready() })
	a.recheckOnFocus(ctx, !busy && problem, q.Refetch)
	if q.Loading {
		return ui.Div().Column().Gap(space8).Child(head).
			Child(setupPanel(ctx, checking(ctx, "Checking where your repositories push…")).MaxWidth(ui.Px(setupProseWidth)))
	}
	var rows []ui.Element
	var remote []string
	for _, f := range q.Data {
		remote = append(remote, f.Repos...)
		name := forgeName(f.Kind)
		var status ui.Element
		switch {
		case f.Ready():
			line := f.CLI + " is signed in"
			if f.Account != "" {
				line += " as " + f.Account
			}
			status = statusLine(ctx, icons.CircleCheck, t.Success, line)
		case f.Path == "":
			var cmd, then ui.Element
			if f.Install != "" {
				cmd = commandBox(ctx, f.Install)
				then = caption(ctx, "Then sign in with "+f.Login+". Or see "+f.InstallURL+".")
			} else {
				then = caption(ctx, "Install it from "+f.InstallURL+", then sign in with "+f.Login+".")
			}
			status = statusLine(ctx, icons.CircleAlert, t.Warning, f.CLI+" isn't installed",
				bodyText(ctx, "Solo needs it to open "+map[forge.Kind]string{forge.GitHub: "pull requests", forge.GitLab: "merge requests"}[f.Kind]+" on "+name+"."), cmd, then)
		case f.Err != "":
			status = statusLine(ctx, icons.CircleAlert, t.Danger, "Couldn't check "+f.CLI, bodyText(ctx, f.Err))
		default:
			status = statusLine(ctx, icons.CircleAlert, t.Warning, f.CLI+" isn't signed in",
				bodyText(ctx, "Run this in a terminal and follow its steps."), commandBox(ctx, f.Login))
		}
		rows = append(rows, ui.Div().Column().Gap(space4).Padding(space5).Key("forge-"+string(f.Kind)).
			Child(ui.Div().Row().Wrap().Align(ui.Center).Gap(space2).
				Child(ui.Icon(icons.GitPullRequest).Size(16).Color(t.TextMuted)).
				Child(ui.Text(name).FontSize(15).Semibold().TextColor(t.Text)).
				Child(caption(ctx, count(len(f.Repos), "repository", "repositories")+": "+nameList(f.Repos, 4)).MinWidth(ui.Px(0)))).
			Child(status))
	}
	local := slices.DeleteFunc(slices.Clone(repos), func(r string) bool { return slices.Contains(remote, r) })
	if len(local) > 0 {
		what := count(len(local), "repository has", "repositories have")
		rows = append(rows, ui.Div().Padding(space5).Key("forge-local").Child(
			statusLine(ctx, icons.GitMerge, t.TextMuted, "Merge locally",
				bodyText(ctx, what+" no GitHub or GitLab remote: "+nameList(local, 4)+". Their work merges on this machine, with nothing to set up."))))
	}
	tools := ui.Div().Column().Gap(space4).Child(setupCard(ctx, rows...))
	if problem {
		tools = tools.Child(ui.Div().Row().Wrap().Align(ui.Center).Gap(space3).
			Child(ui.Button("Check again").Icon(icons.RefreshCw).Secondary().Key("forges-again").Loading(busy).OnClick(q.Refetch)).
			Child(caption(ctx, "Solo checks again when you come back to this window.")))
	}
	left := ui.Div().Column().Gap(space8).Child(head)
	if len(q.Data) == 0 {
		return left.Child(tools.MaxWidth(ui.Px(setupProseWidth + 120)))
	}
	if !l.two {
		return left.Child(tools).Child(a.landingChoices(ctx))
	}
	return setupSplitView(l, left.Child(tools), a.landingChoices(ctx))
}

// landingChoices are how pull requests land: the merge method, auto-merge, and what happens when
// checks fail, as Settings › Pull requests sets them.
func (a *App) landingChoices(ctx *ui.Context) ui.Element {
	cfg := a.cfg.Get()
	method := ui.Local(ctx, "merge-method", cfg.MergeMethod)
	auto := ui.Local(ctx, "auto-merge", cfg.AutoMerge)
	onFail := ui.Local(ctx, "on-fail", cfg.OnChecksFail)
	failHelp := "Solo sends the failure to the thread, which fixes it and pushes again."
	if onFail.Get() == core.ChecksNotify {
		failHelp = "You hear about it, and the thread waits for you."
	}
	return setupPanel(ctx,
		ui.Div().Column().Gap(space2).
			Child(ui.Text("When work is ready to land").FontSize(18).Semibold().TextColor(ctx.Theme().Text)).
			Child(bodyText(ctx, "How a pull request lands once you open it. Settings › Pull requests has these too, with rules for titles and descriptions.")),
		ui.Div().Column().Gap(space3).
			Child(sectionLabel(ctx, "Merge method")).
			Child(ui.Div().Row().Child(ui.ToggleGroup(method, mergeMethods...).Label("Merge method").Key("merge-method").
				OnChange(func(v string) { a.saveSettings(func(s *core.Settings) { s.MergeMethod = v }) }))).
			Child(bodyText(ctx, mergeMeaning[method.Get()])),
		setupRow(ctx, "Auto-merge", "Merge on its own once required checks and reviews pass, with the forge's auto-merge.",
			ui.Switch("Auto-merge", auto).Key("auto-merge").
				OnChange(func(on bool) { a.saveSettings(func(s *core.Settings) { s.AutoMerge = on }) })),
		ui.Div().Column().Gap(space3).
			Child(sectionLabel(ctx, "When checks fail")).
			Child(ui.Div().Row().Child(ui.ToggleGroup(onFail, onChecksFail...).Label("When checks fail").Key("on-fail").
				OnChange(func(v string) { a.saveSettings(func(s *core.Settings) { s.OnChecksFail = v }) }))).
			Child(bodyText(ctx, failHelp))).
		Key("landing")
}

// ---- Preferences ----

// setupPrefs sets the rest of what is worth setting on day one: notifications, costs, and the apps a
// thread's folder opens in. Everything else is left to Settings.
func (a *App) setupPrefs(ctx *ui.Context) ui.Element {
	l := setupLayoutOf(ctx)
	cfg := a.cfg.Get()
	notify := ui.Local(ctx, "notify", !cfg.NoNotifications)
	costs := ui.Local(ctx, "costs", cfg.ShowCosts)
	editor := ui.Local(ctx, "editor", cfg.EditorCommand)
	terminal := ui.Local(ctx, "terminal", cfg.TerminalCommand)
	detected := ui.Local(ctx, "detected", [2]string{launch.Detect("editor"), launch.Detect("terminal")}).Peek()

	behavior := setupPanel(ctx,
		ui.Text("While you work").FontSize(18).Semibold().TextColor(ctx.Theme().Text),
		setupRow(ctx, "Notifications", "An alert when a thread needs you, fails or finishes, while Solo is in the background.",
			ui.Switch("Notifications", notify).Key("pref-notify").
				OnChange(func(on bool) { a.saveSettings(func(s *core.Settings) { s.NoNotifications = !on }) })),
		setupRow(ctx, "Show costs", "What each thread has cost so far, as Claude estimates it, in the title bar and on each thread.",
			ui.Switch("Show costs", costs).Key("pref-costs").
				OnChange(func(on bool) { a.saveSettings(func(s *core.Settings) { s.ShowCosts = on }) })))
	app := func(name, key, what, found, example string, cmd *ui.Signal[string], set func(*core.Settings, string)) ui.Element {
		help, hint := appCommandHelp(what, found, example)
		return ui.Div().Column().Gap(space2).
			Child(ui.Text(name).FontSize(15).Medium().TextColor(ctx.Theme().Text)).
			Child(bodyText(ctx, help)).
			Child(ui.TextInput(cmd).Placeholder(hint).Label(name + " command").Width(ui.Full).Key(key).
				OnChange(func(v string) {
					v = strings.TrimSpace(v)
					a.saveLater(key, func(s *core.Settings) { set(s, v) })
				}))
	}
	var apps ui.Element
	if launch.Available {
		apps = setupPanel(ctx,
			ui.Text("Opening a thread's folder").FontSize(18).Semibold().TextColor(ctx.Theme().Text),
			app("Editor", "pref-editor", "Opens a thread's folder.", detected[0], "code {path}", editor,
				func(s *core.Settings, v string) { s.EditorCommand = v }),
			app("Terminal", "pref-terminal", "Opens a terminal in a thread's folder.", detected[1], "ptyxis --working-directory {path}", terminal,
				func(s *core.Settings, v string) { s.TerminalCommand = v }))
	}
	return ui.Div().Column().Gap(space8).
		Child(setupHead(ctx, "Preferences", "A few things to set now. You can change any of them later in Settings.")).
		Child(setupPairView(l, behavior, apps)).
		Child(bodyText(ctx, "Branch names, commit and pull request rules, and the rest are in Settings, whenever you want them.").
			MaxWidth(ui.Px(setupProseWidth)))
}
