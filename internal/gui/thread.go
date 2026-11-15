package gui

import (
	"context"
	"fmt"
	"github.com/tyler-builds/solo/internal/forge"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/codeedit/highlight"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/launch"
)

func (a *App) merge(id string) {
	a.runBusy(id, "Merge", func(ctx context.Context) error {
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

// openPR opens a pull request for a thread, as a draft when draft is set.
func (a *App) openPR(id string, draft bool) {
	a.runBusy(id, "Opening the pull request", func(ctx context.Context) error {
		pr, err := a.b.OpenPR(ctx, id, draft)
		if err != nil {
			return err
		}
		what := "Opened"
		if draft {
			what = "Opened draft"
		}
		a.notify(ui.ToastSuccess, fmt.Sprintf("%s %s #%d", what, pr.Kind.Noun(), pr.Number), pr.URL)
		return nil
	})
}

// mergeLocal merges a thread's branch in the local checkout, skipping the pull request.
func (a *App) mergeLocal(id string) {
	a.runBusy(id, "Merge", func(ctx context.Context) error {
		_, err := a.b.MergeLocal(ctx, id)
		return err
	})
}

// landControl is a thread's main landing action: Open PR, Merge (its pull request, or locally without
// a forge), or a note that auto-merge will do it. While the action runs, the button shows it is busy.
// nil when there is nothing to land. short is mergeControlMulti's.
func (a *App) landControl(ctx *ui.Context, th core.ThreadView, short bool) ui.Element {
	if th.Resolving {
		return ui.Button("Merge").Icon(icons.GitMerge).Primary().Small().Loading(true).Key("land-" + th.ID).
			Tooltip("Resolving conflicts: newer work landed in " + th.BaseBranch + " since this started. " +
				"The thread is merging it in, then Solo merges.")
	}
	e := a.landButton(ctx, th, short)
	if th.Experiment {
		// The user doesn't mean to land an experiment, but may change their mind: the button stays,
		// without the emphasis that asks for it.
		switch b := e.(type) {
		case *ui.ButtonElement:
			e = b.Secondary()
		case *ui.SplitButtonElement:
			e = b.Secondary()
		}
	}
	if a.busy.Get()[th.ID] {
		switch b := e.(type) {
		case *ui.ButtonElement:
			return b.Loading(true)
		case *ui.SplitButtonElement:
			return b.Loading(true)
		}
	}
	return e
}

// landAction is what a thread's header and its merge card offer to land it: landControl when it can
// land, or while it is busy with changes to land (working, or waiting on background work) the
// disabled button of landBlocked. nil with nothing to land. short is mergeControlMulti's: the header
// asks for it, the card doesn't.
func (a *App) landAction(ctx *ui.Context, th core.ThreadView, short bool) ui.Element {
	if th.Resolving {
		return a.landControl(ctx, th, short) // it merges by itself once the conflicts are resolved
	}
	busy := core.Active(th.Status) || th.Status == core.StatusQueued
	if busy && th.Experiment {
		return nil // nothing it is meant to land, so nothing to wait for
	}
	if busy && th.IsWorker && !th.InPlace && (th.Changed > 0 || len(openPRs(th)) > 0) {
		return a.landBlocked(th)
	}
	if !th.CanOpenPR && !th.CanMerge && !th.CanMarkReady && len(openPRs(th)) == 0 {
		return nil
	}
	return a.landControl(ctx, th, short)
}

// landBlocked is the landing button of a thread that can't land now, disabled, its tooltip saying why:
// Open PR, or Merge, as it will be once it can.
func (a *App) landBlocked(th core.ThreadView) ui.Element {
	label, icon := "Merge", icons.GitMerge
	if th.Forge != forge.None && len(openPRs(th)) == 0 {
		label, icon = "Open "+th.Forge.Noun(), icons.GitPullRequestArrow
	}
	return ui.Button(label).Icon(icon).Secondary().Small().Disabled(true).Tooltip(landBlockedReason(th, a.now())).
		Key("land-blocked-" + th.ID)
}

// landBlockedReason says why a thread can't land now (App.now).
func landBlockedReason(th core.ThreadView, now time.Time) string {
	switch {
	case th.Status == core.StatusBackground:
		what := "Background work is still running"
		if l := bgLine(th); l != "" {
			what += ": " + l
			if s := bgSince(th); !s.IsZero() {
				what += " (" + elapsed(s, now) + ")"
			}
		}
		return what + ". The thread carries on when it finishes; land it after that."
	case th.Status == core.StatusWaiting:
		return "The thread is waiting for your answer. Land it once it has finished."
	case core.Active(th.Status) || th.Status == core.StatusQueued:
		return "The thread is still working. Land it once it has finished."
	case th.Status == core.StatusMerged:
		return "Already merged."
	case th.NoChanges || th.Changed == 0:
		return "It changed no files, so there is nothing to land."
	case th.Changed < 0:
		return "Counting its changes…"
	}
	return "Nothing to land right now."
}

// landButton is landControl's button, without the busy state.
func (a *App) landButton(ctx *ui.Context, th core.ThreadView, short bool) ui.Element {
	t := ctx.Theme()
	id := th.ID
	if th.MultiRepo() {
		return a.landControlMulti(ctx, th, short)
	}
	switch {
	case th.CanOpenPR:
		noun := th.Forge.Noun()
		return ui.SplitButton("Open "+noun, func() { a.openPR(id, false) }, func() []ui.MenuItem {
			return []ui.MenuItem{{Label: "Open as draft", Icon: icons.GitPullRequestDraft, OnClick: func() { a.openPR(id, true) }}}
		}).Icon(icons.GitPullRequestArrow).Primary().Small().Key("open-pr-" + id)
	case th.CanMarkReady:
		return ui.Button("Ready for review").Icon(icons.GitPullRequest).Primary().Small().
			Tooltip(fmt.Sprintf("Mark %s #%d ready for review", th.PR.Kind.Noun(), th.PR.Number)).
			OnClick(func() {
				a.runBusy(id, "Marking it ready", func(ctx context.Context) error { return a.b.MarkReady(ctx, id) })
			})
	case th.PR != nil && th.PR.State == forge.StateOpen:
		if th.CanMerge {
			return ui.Button("Merge").Icon(icons.GitMerge).Primary().Small().
				Tooltip(fmt.Sprintf("Merge %s #%d into %s", th.PR.Kind.Noun(), th.PR.Number, th.BaseBranch)).OnClick(func() { a.merge(id) })
		}
		if th.PR.AutoMerge {
			return ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).Height(ui.Px(28)).Shrink(0).
				Tooltip("It merges once its required checks pass").
				Child(ui.Icon(icons.Timer).Size(13).Color(t.TextMuted)).
				Child(ui.Text("Auto-merge on").FontSize(12).Medium().TextColor(t.TextMuted).NoWrap())
		}
		return ui.Button("Merge").Icon(icons.GitMerge).Secondary().Small().Disabled(true).
			Tooltip("Can't merge yet: " + th.PR.Blocked)
	case th.CanMerge:
		if th.Forge == forge.None {
			// No remote to open a pull request on: it merges straight into the local branch.
			return ui.Button("Merge locally").Icon(icons.GitMerge).Primary().Small().
				Tooltip("No remote, so no pull request: merges " + branchLabel(th) + " into your local " + th.BaseBranch).
				OnClick(func() { a.merge(id) })
		}
		return ui.Button("Merge").Icon(icons.GitMerge).Primary().Small().
			Tooltip("Merge " + th.Branch + " into " + th.BaseBranch).OnClick(func() { a.merge(id) })
	}
	return nil
}

// openPRFor opens pull requests for one of a thread's repositories ("" for all).
func (a *App) openPRFor(id, repo string, draft bool) {
	a.runBusy(id, "Opening pull requests", func(ctx context.Context) error {
		prs, err := a.b.OpenPRFor(ctx, id, repo, draft)
		if err != nil {
			return err
		}
		what := "Opened"
		if draft {
			what = "Opened draft"
		}
		if len(prs) == 1 {
			a.notify(ui.ToastSuccess, fmt.Sprintf("%s %s #%d", what, prs[0].Kind.Noun(), prs[0].Number), prs[0].URL)
		} else {
			a.notify(ui.ToastSuccess, fmt.Sprintf("%s %d %ss", what, len(prs), prs[0].Kind.Noun()), "")
		}
		return nil
	})
}

// mergeRepo merges one of a thread's repositories ("" for every one that can merge now).
func (a *App) mergeRepo(id, repo string) {
	a.runBusy(id, "Merge", func(ctx context.Context) error {
		_, err := a.b.MergeRepo(ctx, id, repo)
		return err
	})
}

// landControlMulti is landControl for a thread in several repositories: a split button that acts on
// all of them, with each repository on its own in its menu. Repositories with nothing to land don't
// count: with one pull request (or one branch to merge locally), its button names that one.
func (a *App) landControlMulti(ctx *ui.Context, th core.ThreadView, short bool) ui.Element {
	id := th.ID
	var canOpen, drafts []core.PartView
	for _, p := range th.Parts {
		if p.CanOpenPR {
			canOpen = append(canOpen, p)
		}
		if p.PR != nil && p.PR.State == forge.StateOpen && p.PR.Draft {
			drafts = append(drafts, p)
		}
	}
	switch {
	case len(canOpen) > 0:
		noun := canOpen[0].Forge.Noun()
		return ui.SplitButton("Open "+plural(len(canOpen), noun), func() { a.openPRFor(id, "", false) }, func() []ui.MenuItem {
			items := []ui.MenuItem{{Label: "Open " + plural(len(canOpen), noun) + " as drafts", Icon: icons.GitPullRequestDraft,
				OnClick: func() { a.openPRFor(id, "", true) }}, ui.MenuSeparator()}
			for _, p := range canOpen {
				repo := p.Repo
				items = append(items, ui.MenuItem{Label: noun + " for " + p.Name + " only", Icon: icons.GitPullRequestArrow,
					OnClick: func() { a.openPRFor(id, repo, false) }})
			}
			return items
		}).Icon(icons.GitPullRequestArrow).Primary().Small().Key("open-pr-" + id)
	case len(drafts) > 0:
		return ui.Button("Ready for review").Icon(icons.GitPullRequest).Primary().Small().
			Tooltip("Mark " + plural(len(drafts), "draft") + " ready for review").
			OnClick(func() {
				a.runBusy(id, "Marking them ready", func(ctx context.Context) error { return a.b.MarkReady(ctx, id) })
			})
	}
	return a.mergeControlMulti(ctx, th, short)
}

// mergeTargets are what a multi-repository thread has to merge: each open pull request, and each
// repository without a remote that has commits to merge locally. ready are those that can merge now.
func mergeTargets(th core.ThreadView) (targets, ready []core.PartView) {
	for _, p := range th.Parts {
		if (p.PR != nil && p.PR.State == forge.StateOpen) || p.CanMerge {
			targets = append(targets, p)
			if p.CanMerge {
				ready = append(ready, p)
			}
		}
	}
	return targets, ready
}

// partMergeLabel names merging one repository of a thread: "Merge api #142", or "Merge db locally"
// for one without a remote.
func partMergeLabel(p core.PartView) string {
	if p.PR != nil && p.PR.State == forge.StateOpen {
		return fmt.Sprintf("Merge %s #%d", p.Name, p.PR.Number)
	}
	return "Merge " + p.Name + " locally"
}

// partMergeShort is partMergeLabel without the repository's name, "Merge #142": for the thread's
// header, whose chips name each repository beside its pull request's number.
func partMergeShort(p core.PartView) string {
	if p.PR != nil && p.PR.State == forge.StateOpen {
		return fmt.Sprintf("Merge #%d", p.PR.Number)
	}
	return partMergeLabel(p)
}

// partMergeTip says what merging one repository of a thread does.
func partMergeTip(p core.PartView) string {
	if p.PR != nil && p.PR.State == forge.StateOpen {
		return fmt.Sprintf("Merge %s's %s #%d into %s", p.Name, p.PR.Kind.Noun(), p.PR.Number, p.BaseBranch)
	}
	return "No remote, so no pull request: merges " + p.Branch + " into your local " + p.BaseBranch
}

// mergeControlMulti is the merge button of a thread in several repositories: named for the one
// repository it merges, or "Merge all" when two or more have something to merge. nil with nothing.
// short names that one repository by its pull request's number alone (partMergeShort); the menu always
// names it in full.
func (a *App) mergeControlMulti(ctx *ui.Context, th core.ThreadView, short bool) ui.Element {
	t := ctx.Theme()
	id := th.ID
	name := partMergeLabel
	if short {
		name = partMergeShort
	}
	targets, ready := mergeTargets(th)
	autoAll := len(targets) > 0
	for _, p := range targets {
		autoAll = autoAll && p.PR != nil && p.PR.AutoMerge && !p.CanMerge
	}
	autoMerge := func(tip string) ui.Element {
		return ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).Height(ui.Px(28)).Shrink(0).Tooltip(tip).
			Child(ui.Icon(icons.Timer).Size(13).Color(t.TextMuted)).
			Child(ui.Text("Auto-merge on").FontSize(12).Medium().TextColor(t.TextMuted).NoWrap())
	}
	switch len(targets) {
	case 0:
		return nil
	case 1:
		p := targets[0]
		repo := p.Repo
		switch {
		case p.CanMerge:
			return ui.Button(name(p)).Icon(icons.GitMerge).Primary().Small().Tooltip(partMergeTip(p)).
				OnClick(func() { a.mergeRepo(id, repo) }).Key("merge-" + id)
		case autoAll:
			return autoMerge("It merges once its required checks pass")
		}
		tip := "Can't merge yet"
		if p.PR.Blocked != "" {
			tip += ": " + p.PR.Blocked
		}
		return ui.Button(name(p)).Icon(icons.GitMerge).Secondary().Small().Disabled(true).Tooltip(tip).
			Key("merge-" + id)
	}
	menu := func() []ui.MenuItem {
		items := []ui.MenuItem{{Label: "Merge all", Icon: icons.GitMerge, Disabled: len(ready) < len(targets),
			OnClick: func() { a.mergeRepo(id, "") }}, ui.MenuSeparator()}
		for _, p := range targets {
			repo, label := p.Repo, partMergeLabel(p)
			if !p.CanMerge && p.PR.Blocked != "" {
				label += " · " + p.PR.Blocked
			}
			items = append(items, ui.MenuItem{Label: label, Icon: icons.GitMerge,
				Disabled: !p.CanMerge, OnClick: func() { a.mergeRepo(id, repo) }})
		}
		return items
	}
	switch {
	case len(ready) == len(targets):
		return ui.SplitButton("Merge all", func() { a.mergeRepo(id, "") }, menu).Icon(icons.GitMerge).Primary().Small().
			Tooltip("Merge each repository into its base branch").Key("merge-" + id)
	case len(ready) == 1:
		repo := ready[0].Repo
		return ui.SplitButton(name(ready[0]), func() { a.mergeRepo(id, repo) }, menu).
			Icon(icons.GitMerge).Primary().Small().Tooltip(partMergeTip(ready[0])).Key("merge-" + id)
	case len(ready) > 1:
		return ui.SplitButton(fmt.Sprintf("Merge %d ready", len(ready)), func() { a.mergeRepo(id, "") }, menu).
			Icon(icons.GitMerge).Primary().Small().Key("merge-" + id)
	case autoAll:
		return autoMerge("Each merges once its required checks pass")
	}
	return ui.SplitButton("Merge", func() {}, menu).Icon(icons.GitMerge).Secondary().Small().Key("merge-" + id)
}

// plural is n of a noun: "1 draft", "3 drafts".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// repoChips name a multi-repository thread's repositories, each with its pull request's state; a chip
// with a pull request opens it. They are for a row that wraps.
func (a *App) repoChips(ctx *ui.Context, th core.ThreadView) []ui.Element {
	t := ctx.Theme()
	var chips []ui.Element
	for _, p := range th.Parts {
		chip := ui.Div().Row().Align(ui.Center).Gap(5).PaddingX(space2).Height(ui.Px(20)).Radius(10).
			Border(1, t.Border).Shrink(0)
		tip := p.Branch + " → " + p.BaseBranch
		if p.Changed == 0 {
			tip = "No changes in " + p.Name
		}
		if pr := p.PR; pr != nil {
			c := ui.Hex(pal(t).info)
			switch {
			case pr.State == forge.StateMerged:
				c, tip = ui.Hex(pal(t).info), "Merged"
			case pr.State == forge.StateClosed:
				c, tip = t.TextSubtle, "Closed"
			case pr.Checks == forge.ChecksFailing:
				c, tip = t.Danger, "Checks failed: "+strings.Join(checkNamesOf(pr.Failed), ", ")
			case pr.Mergeable:
				c, tip = t.Success, "Ready to merge"
			case pr.Checks == forge.ChecksPending || pr.Checks == forge.ChecksWaiting:
				c, tip = t.Warning, "Waiting for checks"
			case pr.Blocked != "":
				tip = "Can't merge yet: " + pr.Blocked
			}
			url := pr.URL
			chip = chip.Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
				Role(access.RoleLink).Label(fmt.Sprintf("%s %s #%d", p.Name, pr.Kind.Noun(), pr.Number)).
				OnClick(func() { _ = ctx.OpenURL(url) }).
				Child(dot(c, 7)).
				Child(ui.Text(p.Name).FontSize(11).Medium().NoWrap()).
				Child(ui.Text(fmt.Sprintf("#%d", pr.Number)).FontSize(11).Medium().TextColor(ui.Hex(pal(t).info)).NoWrap())
		} else {
			color := t.TextMuted
			if p.Changed == 0 {
				color = t.TextSubtle
			}
			chip = chip.Child(ui.Text(p.Name).FontSize(11).Medium().TextColor(color).NoWrap())
		}
		chips = append(chips, chip.Tooltip(tip))
	}
	return chips
}

func checkNamesOf(cs []forge.Check) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// headerChecksMinWidth is the narrowest thread panel whose header spells out its checks; a narrower one
// shows only their mark, with the counts in its tooltip.
const headerChecksMinWidth = 520

// headerChecks says in a thread's header where its pull requests' checks stand, counted, in the merge
// card's colors: "3 passed · 2 running", "4 passed · 1 failed". A forge without counts (GitLab's one
// pipeline) gets the plain words. It follows the checks as Solo polls them. nil without an open pull
// request, with no checks on it, and once the thread has landed.
func (a *App) headerChecks(ctx *ui.Context, th core.ThreadView) ui.Element {
	switch th.Status {
	case core.StatusMerged, core.StatusDiscarded, core.StatusArchived:
		return nil
	}
	prs := openPRs(th)
	if len(prs) == 0 && th.PR != nil && th.PR.State == forge.StateOpen {
		prs = []*forge.PR{th.PR}
	}
	if !slices.ContainsFunc(prs, func(pr *forge.PR) bool { return pr.Checks != forge.ChecksNone }) {
		return nil
	}
	cs := checksOf(prs)
	text := cs.tally()
	if text == "" {
		text = cs.text
	}
	icon, c := checksLook(ctx, cs.checks)
	box := ui.Div().Row().Align(ui.Center).Gap(4).PaddingX(space1).Shrink(0).Key("thread-checks-" + th.ID).
		Child(ui.Icon(icon).Size(13).Color(c))
	if a.panelWidth(ctx) < headerChecksMinWidth {
		return box.Tooltip(text + ". " + cs.tip)
	}
	return box.Tooltip(cs.tip).Child(ui.Text(text).FontSize(12).TextColor(c).NoWrap())
}

// prLink opens a thread's pull request in the browser.
func (a *App) prLink(ctx *ui.Context, th core.ThreadView) ui.Element {
	if th.PR == nil || th.MultiRepo() {
		return nil
	}
	url := th.PR.URL
	return ui.Button(fmt.Sprintf("#%d", th.PR.Number)).Icon(icons.ExternalLink).Ghost().Small().
		Tooltip("Open the " + th.PR.Kind.Noun() + " in your browser").OnClick(func() { _ = ctx.OpenURL(url) })
}

// changeData is a worker's changed files and diff.
type changeData struct {
	files []git.FileChange
	diff  string
}

// changes loads a worker's changes, again whenever the thread changes.
func (a *App) changes(ctx *ui.Context, th core.ThreadView) ui.QueryResult[changeData] {
	key := fmt.Sprintf("changes:%s:%s:%d", th.ID, th.Status, th.UpdatedAt.UnixMilli())
	return ui.Query(ctx, key, func(c context.Context) (changeData, error) {
		if !th.HasWorktree {
			return changeData{}, nil
		}
		files, err := a.b.Changes(c, th.ID)
		if err != nil {
			return changeData{}, err
		}
		diff, err := a.b.Diff(c, th.ID)
		return changeData{files, diff}, err
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
	res := a.changes(ctx, th)
	added, removed := 0, 0
	for _, f := range res.Data.files {
		added += f.Added
		removed += f.Removed
	}
	if th.InTurn || len(th.Background) > 0 {
		tick := ui.Local(ctx, "tick", 0)
		tick.Get()
		ctx.After(time.Second, func() { tick.Update(func(n int) int { return n + 1 }) })
	}

	// The change size sits at the end of the tab row, beside Changes.
	stat := ui.Div().Row().Gap(6).Shrink(0).
		Child(ui.Text(fmt.Sprintf("+%d", added)).FontSize(12).Medium().TextColor(t.Success).NoWrap()).
		Child(ui.Text(fmt.Sprintf("−%d", removed)).FontSize(12).Medium().TextColor(t.Danger).NoWrap())
	// The header is two rows. The title shares the first with the small controls (tools): the checks, the
	// folder's buttons, More and Close. The wide ones that land the thread (land) are in the second,
	// beside where the thread is, so a long button never squeezes the title.
	// A compact window has no room for the folder's buttons beside the title: they are in More there.
	compact := isCompact(ctx)
	tools := ui.Div().Row().Align(ui.Center).Gap(space1).Shrink(0).Child(a.headerChecks(ctx, th))
	if th.Dir != "" && launch.Available && !compact {
		dir := th.Dir
		tools = tools.
			Child(ui.IconButton(icons.SquareCode).Ghost().Small().Label("Open in editor").Tooltip("Open in editor").
				OnClick(func() { a.openEditor(dir) })).
			Child(ui.IconButton(icons.SquareTerminal).Ghost().Small().Label("Open terminal").Tooltip("Open terminal").
				OnClick(func() { a.openTerminal(dir) }))
	}
	var land []ui.Element
	if pr := a.prLink(ctx, th); pr != nil {
		land = append(land, pr)
	}
	l := a.landAction(ctx, th, true)
	if l != nil {
		land = append(land, l)
	}
	// An experiment's landing button is a quiet one, so Done stays beside it.
	if th.CanDone && !asksToLand(th) && (l == nil || th.Experiment) {
		land = append(land, ui.Button("Done").Icon(icons.Check).Primary().Small().
			Tooltip("Take it out of your list. Settings › History can restore it.").
			OnClick(func() { a.markDone(th) }))
	}
	if compact {
		// One wrapping row there, under the window's bar: the landing controls come before More.
		tools = tools.Children(land...)
	}
	if th.IsWorker || th.CanDiscard || th.CanInterrupt || th.CanMergeLocal || th.Unlanded || th.Dir != "" {
		tools = tools.Child(ui.DropdownMenu(
			ui.IconButton(icons.Ellipsis).Ghost().Small().Label("More actions").Tooltip("More"),
			func() []ui.MenuItem {
				var items []ui.MenuItem
				if th.CanInterrupt {
					label := "Stop"
					if !th.InTurn {
						label = "Stop background work"
					}
					items = append(items, ui.MenuItem{Label: label, Icon: icons.Square, OnClick: func() {
						a.run("Stopping", func(ctx context.Context) error { return a.b.Interrupt(ctx, id) })
					}})
				}
				if th.CanMergeLocal && th.Forge != forge.None {
					items = append(items, ui.MenuItem{Label: "Merge locally instead", Icon: icons.GitMerge, OnClick: func() { a.mergeLocal(id) }})
				}
				if th.CanDone && asksToLand(th) {
					items = append(items, ui.MenuItem{Label: "Mark done…", Icon: icons.Check, OnClick: func() { a.markDone(th) }})
				} else if th.CanDone && (th.CanOpenPR || th.CanMerge || th.PR != nil) {
					items = append(items, ui.MenuItem{Label: "Done", Icon: icons.Check, OnClick: func() { a.markDone(th) }})
				}
				if th.IsWorker && !th.InPlace {
					items = append(items, a.experimentItem(th))
				}
				if th.Dir != "" {
					dir := th.Dir
					if launch.Available && compact {
						items = append(items,
							ui.MenuItem{Label: "Open in editor", Icon: icons.SquareCode, OnClick: func() { a.openEditor(dir) }},
							ui.MenuItem{Label: "Open terminal", Icon: icons.SquareTerminal, OnClick: func() { a.openTerminal(dir) }})
					}
					items = append(items, ui.MenuItem{Label: "Copy folder path", Icon: icons.Copy, OnClick: func() { a.copyText("the folder path", dir) }})
				}
				if th.CanDiscard {
					items = append(items, ui.MenuItem{Label: "Discard thread…", Icon: icons.Trash, OnClick: func() { a.confirmDiscard(th) }})
				}
				return items
			}))
	}
	if !compact {
		tools = tools.Child(ui.IconButton(icons.X).Ghost().Small().Label("Close thread").Tooltip("Close").
			OnClick(a.closeThread))
	}

	// The mark carries the status, so the row under the title is just where the thread is, then what
	// lands it. It is one row that wraps: the landing controls end its last line, at the right, so they
	// take a line of their own only when the repositories leave them no room.
	meta := ui.Div().Row().Wrap().Align(ui.Center).Gap(space2).MinWidth(ui.Px(0))
	if th.MultiRepo() {
		meta = meta.Gap(space1 + 2).Children(a.repoChips(ctx, th)...)
	} else {
		repo := ui.Div().Child(ui.Text(filepath.Base(th.RepoPath)).FontSize(12).Medium().TextColor(t.TextMuted).NoWrap())
		if th.Branch != "" {
			repo = repo.Tooltip(branchLabel(th) + " → " + th.BaseBranch)
		}
		meta = meta.Child(repo)
		if th.InPlace {
			meta = meta.Child(ui.Div().Tooltip("This folder isn't in git, so the thread edits it directly: no separate copy, diff or merge. Settings › General can put it in git.").
				Child(subtle(ctx, "not in git")))
		}
	}
	if th.Experiment && !th.InPlace {
		meta = meta.Child(experimentLabel(ctx))
	}
	if a.cfg.Get().ShowCosts && th.CostUSD > 0 {
		meta = meta.Child(subtle(ctx, fmt.Sprintf("$%.2f", th.CostUSD)))
	}
	var header *ui.DivElement
	if compact {
		// The window's bar has the title (compactBar); here is where the thread is, and what can be done
		// with it.
		header = ui.Div().Row().Wrap().Align(ui.Center).Gap(space2).PaddingX(space4).PaddingY(space2).Shrink(0).
			Child(ui.Div().Grow(1).MinWidth(ui.Px(0)).Child(meta)).
			Child(tools)
	} else {
		if len(land) > 0 {
			meta = meta.Child(ui.Div().Row().Align(ui.Center).Justify(ui.End).Gap(space1).Grow(1).Children(land...))
		}
		header = ui.Div().Column().Gap(space1).PaddingLeft(space4).PaddingRight(space2).PaddingY(space3).Shrink(0).
			Child(ui.Div().Row().Align(ui.Center).Gap(space2).MinWidth(ui.Px(0)).
				Child(statusMark(ctx, shownStatus(th)).Tooltip(statusLabel(th))).
				Child(ui.Div().Grow(1).MinWidth(ui.Px(0)).Tooltip(th.Title).ContextMenu(func() []ui.MenuItem { return a.folderMenu(th) }).
					Child(ui.Text(th.Title).FontSize(14).Semibold().Truncate())).
				Child(tools)).
			Child(ui.Div().PaddingLeft(space5 + 2).Child(meta))
	}
	changesLabel := "Changes"
	if n := len(res.Data.files); n > 0 {
		changesLabel = fmt.Sprintf("Changes · %d", n)
	}
	tabList := []ui.Tab[string]{{Value: "conversation", Label: "Conversation"}}
	if !th.InPlace { // nothing to diff against outside git
		tabList = append(tabList, ui.Tab[string]{Value: "changes", Label: changesLabel, Disabled: !th.HasWorktree})
	}
	tabs := ui.Div().Row().Align(ui.Center).PaddingX(space4).Shrink(0).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).
		Child(ui.Tabs(a.openTab, tabList...))
	if added+removed > 0 {
		// The tab list draws a line under itself and now ends where the change size starts: the same
		// line under the change size carries it to the row's end.
		tabs = tabs.Child(ui.Div().Row().Align(ui.Center).Justify(ui.End).Grow(1).AlignSelf(ui.Stretch).PaddingLeft(space3).
			BorderEdges(ui.Edges{Bottom: 1}, t.Border).Child(stat))
	}

	var content ui.Element
	if tab == "changes" && th.HasWorktree {
		content = a.changesView(ctx, th, res)
	} else {
		items := a.itemsOf(id).Get()
		content = ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).
			Child(a.transcript(ctx, th, items, "thread-"+id)).
			Child(ui.Div().Column().Gap(space2).PaddingX(space4).PaddingBottom(space4).Shrink(0).
				Child(a.workingLine(ctx, th)).
				Child(a.composerFor(ctx, th)))
	}
	return ui.Div().Column().Size(ui.Full, ui.Full).Key("thread-panel-" + id).
		Child(header).
		Child(tabs).
		Child(ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).Child(content))
}

// workingLine is pinned above a working thread's composer: how long it has worked, and its step. A
// thread waiting on background work says what it waits on, for how long.
func (a *App) workingLine(ctx *ui.Context, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	now := a.now()
	if !th.InTurn && th.Status == core.StatusBackground {
		text := "Waiting on background work"
		if s := bgSince(th); !s.IsZero() {
			text += " · " + elapsed(s, now)
		}
		what := bgLine(th)
		if what == "" {
			what = "Reading the results"
		}
		tip := "Its turn ended, but work it started in the background still runs. It carries on when that finishes."
		for _, b := range th.Background {
			tip += "\n• " + b.Description + " (" + b.Kind() + ", " + elapsed(b.Started, now) + ")"
		}
		return ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space3).Height(ui.Px(30)).Radius(radiusM).
			Background(t.Surface).Border(1, t.Border).Tooltip(tip).Key("background-line").
			Child(statusMark(ctx, core.StatusBackground)).
			Child(ui.Text(text).FontSize(12).Semibold().TextColor(t.Text).NoWrap()).
			Child(ui.Text(what).FontSize(12).TextColor(t.TextMuted).Truncate().Grow(1).MinWidth(ui.Px(0)))
	}
	if !th.InTurn {
		return ui.Div()
	}
	if th.Status == core.StatusWaiting {
		return ui.Div() // its question card asks in the conversation
	}
	text := "Working"
	if !th.TurnStarted.IsZero() {
		text += " · " + elapsed(th.TurnStarted, now)
	}
	step := "Thinking"
	if th.Step != nil {
		step = a.stepLabel(*th.Step)
	} else if th.Retry != "" {
		step = th.Retry
	}
	return ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space3).Height(ui.Px(30)).Radius(radiusM).
		Background(t.Surface).Border(1, t.Border).Key("working-line").
		Child(statusMark(ctx, core.StatusRunning)).
		Child(ui.Text(text).FontSize(12).Semibold().TextColor(t.Text).NoWrap()).
		Child(ui.Div().Grow(1).MinWidth(ui.Px(0)).
			Play("shimmer", []ui.Keyframe{{At: 0.5, Motion: ui.Fade().Opacity(0.55)}},
				ui.Animation{Duration: 1600 * time.Millisecond, Repeat: true}).
			Child(ui.Text(step).FontSize(12).TextColor(t.TextMuted).Truncate()))
}

// panelWidth estimates the thread panel's width from the window and the split's size.
func (a *App) panelWidth(ctx *ui.Context) float32 {
	w, _ := ctx.WindowSize()
	if isCompact(ctx) {
		return w // the thread fills a compact window
	}
	if l := a.panelSize.Get(); l.Unit == ui.Px(0).Unit {
		return l.Value
	}
	// The split's length is the window less the rail; the pane's padding takes space2 of it.
	return (w-railW(ctx)-space2)*a.panelGoal/100 - space2
}

// splitMinWidth is the narrowest panel that shows a diff side by side.
const splitMinWidth = 640

// widenFor reports whether the thread panel can be at least px wide, and widens it if it can.
// Asking without room changes nothing.
func (a *App) widenFor(ctx *ui.Context, px float32) bool {
	if a.panelWidth(ctx) >= px {
		return true
	}
	if isCompact(ctx) {
		return false
	}
	w, _ := ctx.WindowSize()
	length := w - railW(ctx) - space2
	pct := (px + space2) / length * 100
	if pct > 75 || length-px < 340 {
		return false
	}
	// Only report here; the caller widens on click (see segmented), so a render doesn't resize.
	return true
}

// widenNow widens the panel to px.
func (a *App) widenNow(ctx *ui.Context, px float32) {
	w, _ := ctx.WindowSize()
	pct := (px + space2) / (w - railW(ctx) - space2) * 100
	a.panelGoal = pct
	a.panelSize.Set(ui.Percent(pct))
}

// confirmDiscard asks before discarding a thread.
func (a *App) confirmDiscard(th core.ThreadView) {
	id := th.ID
	a.confirm.Set(&confirmation{
		title:  "Discard this thread?",
		body:   discardText(th),
		action: "Discard",
		run: func() {
			if a.openID.Peek() == id {
				a.closeThread()
			}
			a.run("Discard", func(ctx context.Context) error { return a.b.Discard(ctx, id) })
		},
	})
}

// experimentItem is the thread menu's item that turns its experiment mode on or off
// (store.Thread.Experiment).
func (a *App) experimentItem(th core.ThreadView) ui.MenuItem {
	id, on := th.ID, !th.Experiment
	label, icon, what := "Mark as experiment", icons.FlaskConical, "Marking it an experiment"
	if th.Experiment {
		label, icon, what = "Not an experiment", icons.FlaskConicalOff, "Making it a normal thread"
	}
	return ui.MenuItem{Label: label, Icon: icon, OnClick: func() {
		a.run(what, func(ctx context.Context) error { return a.b.SetExperiment(ctx, id, on) })
	}}
}

// experimentLabel marks an experiment in its header, as "not in git" marks a folder outside git.
func experimentLabel(ctx *ui.Context) ui.Element {
	return ui.Div().Key("experiment-label").
		Tooltip("An experiment: you're trying something out, not landing it, so Solo proposes no merge and it never waits on you to land it. " +
			"It still has its branch, and you can land it if you change your mind. More › Not an experiment turns this off.").
		Child(subtle(ctx, "experiment"))
}

// discardText says what discarding a thread loses.
func discardText(th core.ThreadView) string {
	if th.InPlace {
		return fmt.Sprintf("This stops %q. Its edits stay in %s: the folder isn't in git, so there is nothing to throw away.", th.Title, filepath.Base(th.RepoPath))
	}
	return fmt.Sprintf("This stops %q and deletes its worktree and branch %s, with any work not merged.", th.Title, th.Branch)
}

func (a *App) composerFor(ctx *ui.Context, th core.ThreadView) ui.Element {
	if core.Finished(th.Status) {
		return caption(ctx, "This thread is "+strings.ToLower(statusLabel(th))+".")
	}
	if th.Status == core.StatusQueued {
		return caption(ctx, "This thread is starting.")
	}
	id := th.ID
	return a.composer(ctx, th, "draft-"+id, "thread-"+id, "Message this thread…", func(text string, images []string) error {
		return a.b.Send(context.Background(), id, text, images...)
	})
}

// changesView lists a worker's changed files over its diff, a collapsible section per file.
func (a *App) changesView(ctx *ui.Context, th core.ThreadView, res ui.QueryResult[changeData]) ui.Element {
	switch {
	case res.Loading && len(res.Data.files) == 0:
		return ui.Div().Column().Center().Grow(1).Child(ui.ProgressRing().Label("Loading changes"))
	case res.Err != nil:
		return ui.Div().Column().Center().Grow(1).Padding(space6).
			Child(ui.Empty("Could not read the changes").Icon(icons.CircleAlert).Description(res.Err.Error()))
	case len(res.Data.files) == 0:
		return ui.Div().Column().Center().Grow(1).Padding(space6).
			Child(ui.Empty("No changes yet").Icon(icons.FileCode).Description("Files the thread changes show up here."))
	}
	files := parseDiff(res.Data.diff)
	return ui.Component(func(ctx *ui.Context) ui.Element {
		// Side by side needs room; a narrow panel shows the unified diff.
		return diffView(ctx, res.Data.files, files, th.MultiRepo(), a.panelWidth(ctx) >= splitMinWidth, func() bool { return a.widenFor(ctx, splitMinWidth) }, func() { a.widenNow(ctx, splitMinWidth) })
	})
}

// diffFile is one file of a unified diff.
type diffFile struct {
	path     string
	lines    []diffLine
	colors   map[int][]span // line index → syntax spans
	add, del int
}

type diffLine struct {
	kind     byte // ' ', '+', '-', '@'
	old, new int  // line numbers; 0 when not on that side
	text     string
	// wordStart and wordEnd mark the part of a changed line that differs from its counterpart.
	wordStart, wordEnd int
}

type span struct {
	start, end int
	capture    string
}

// parseDiff splits a unified diff into files, with line numbers, and colors their code.
func parseDiff(diff string) []*diffFile {
	var files []*diffFile
	var f *diffFile
	oldN, newN := 0, 0
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			f = &diffFile{}
			if i := strings.LastIndex(l, " b/"); i >= 0 {
				f.path = l[i+3:]
			}
			files = append(files, f)
			continue
		case f == nil:
			continue
		case strings.HasPrefix(l, "+++ "):
			if p := strings.TrimPrefix(l, "+++ "); p != "/dev/null" {
				f.path = strings.TrimPrefix(p, "b/")
			}
			continue
		case strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "index "), strings.HasPrefix(l, "similarity"),
			strings.HasPrefix(l, "rename "), strings.HasPrefix(l, "old mode"), strings.HasPrefix(l, "new mode"),
			strings.HasPrefix(l, "new file"), strings.HasPrefix(l, "deleted file"), strings.HasPrefix(l, "Binary files"):
			continue
		case strings.HasPrefix(l, "@@"):
			parts := strings.Fields(l) // @@ -a,b +c,d @@ context
			if len(parts) >= 3 {
				oldN, newN = hunkStart(parts[1]), hunkStart(parts[2])
			}
			f.lines = append(f.lines, diffLine{kind: '@', text: l})
			continue
		}
		if l == "" || strings.HasPrefix(l, `\ `) {
			continue
		}
		text := strings.ReplaceAll(l[1:], "\t", "    ")
		switch l[0] {
		case '+':
			f.lines = append(f.lines, diffLine{kind: '+', new: newN, text: text})
			f.add++
			newN++
		case '-':
			f.lines = append(f.lines, diffLine{kind: '-', old: oldN, text: text})
			f.del++
			oldN++
		default:
			f.lines = append(f.lines, diffLine{kind: ' ', old: oldN, new: newN, text: text})
			oldN++
			newN++
		}
	}
	for _, f := range files {
		f.pairWords()
		f.colorize()
	}
	return files
}

// pairWords marks, in each removed line and the added line that replaces it, the part that changed:
// the text between their common prefix and suffix.
func (f *diffFile) pairWords() {
	for i := 0; i < len(f.lines); {
		if f.lines[i].kind != '-' {
			i++
			continue
		}
		dels := i
		for i < len(f.lines) && f.lines[i].kind == '-' {
			i++
		}
		adds := i
		for i < len(f.lines) && f.lines[i].kind == '+' {
			i++
		}
		for k := 0; dels+k < adds && adds+k < i; k++ {
			d, a := &f.lines[dels+k], &f.lines[adds+k]
			p := commonPrefix(d.text, a.text)
			q := commonSuffix(d.text[p:], a.text[p:])
			// Only mark a real edit inside the line, not a line rewritten from scratch.
			if p+q == 0 {
				continue
			}
			d.wordStart, d.wordEnd = p, len(d.text)-q
			a.wordStart, a.wordEnd = p, len(a.text)-q
		}
	}
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	for n > 0 && n < len(a) && !utf8.RuneStart(a[n]) {
		n--
	}
	return n
}

func commonSuffix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	for n > 0 && n < len(a) && !utf8.RuneStart(a[len(a)-n]) {
		n--
	}
	return n
}

func hunkStart(s string) int {
	s = strings.TrimLeft(s, "-+")
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	n, _ := strconv.Atoi(s)
	return n
}

// colorize lexes the new side (context and added lines) and the old side (removed lines) of the file
// as its language, and keeps each line's spans.
func (f *diffFile) colorize() {
	lang := strings.TrimPrefix(filepath.Ext(f.path), ".")
	if lang == "" || !highlight.Supported(lang) {
		return
	}
	f.colors = map[int][]span{}
	for _, side := range []func(diffLine) bool{
		func(l diffLine) bool { return l.kind == ' ' || l.kind == '+' },
		func(l diffLine) bool { return l.kind == '-' },
	} {
		var b strings.Builder
		var idx, starts []int
		for i, l := range f.lines {
			if !side(l) {
				continue
			}
			idx = append(idx, i)
			starts = append(starts, b.Len())
			b.WriteString(l.text)
			b.WriteByte('\n')
		}
		if len(idx) == 0 {
			continue
		}
		spans, ok := code.HighlightCode(context.Background(), lang, b.String(), func() {})
		if !ok {
			continue
		}
		li := 0
		for _, s := range spans {
			for li+1 < len(starts) && starts[li+1] <= s.Start {
				li++
			}
			lineStart := starts[li]
			lineEnd := lineStart + len(f.lines[idx[li]].text)
			start, end := s.Start-lineStart, min(s.End, lineEnd)-lineStart
			if start < 0 || end <= start {
				continue
			}
			f.colors[idx[li]] = append(f.colors[idx[li]], span{start, end, s.Capture})
		}
	}
}

const diffRow = 22

// diffView shows the file list, then each file's diff: a header that folds it, and its lines with
// old and new line numbers, colored by change and by syntax.
func diffView(ctx *ui.Context, stats []git.FileChange, files []*diffFile, grouped, roomy bool, canWiden func() bool, widen func()) ui.Element {
	t := ctx.Theme()
	folded := ui.Local(ctx, "folded", map[string]bool{})
	h := ui.Local(ctx, "list", ui.NewListHandle()).Peek()
	fold := folded.Get()
	split := diffSplit

	type rowRef struct {
		file *diffFile
		line int // -1 for the file's header
	}
	var rows []rowRef
	headerAt := map[string]int{}
	for _, f := range files {
		headerAt[f.path] = len(rows)
		rows = append(rows, rowRef{f, -1})
		if fold[f.path] {
			continue
		}
		for i := range f.lines {
			rows = append(rows, rowRef{f, i})
		}
	}

	added, removed := 0, 0
	list := ui.Div().Column().PaddingX(space2).PaddingY(space2).BorderEdges(ui.Edges{Bottom: 1}, t.Border).Shrink(0).
		MaxHeight(ui.Px(176)).ScrollY()
	// With several repositories, each path starts with its repository's folder: group by it.
	type sum struct{ added, removed int }
	repoSums := map[string]sum{}
	for _, f := range stats {
		r, _, _ := strings.Cut(f.Path, "/")
		rs := repoSums[r]
		rs.added, rs.removed = rs.added+f.Added, rs.removed+f.Removed
		repoSums[r] = rs
	}
	lastRepo := ""
	for _, f := range stats {
		added += f.Added
		removed += f.Removed
		path := f.Path
		dir := filepath.Dir(path)
		if grouped {
			r, rest, _ := strings.Cut(path, "/")
			dir = filepath.Dir(rest)
			if r != lastRepo {
				lastRepo = r
				rs := repoSums[r]
				list = list.Child(ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space2).PaddingTop(space2).Height(ui.Px(26)).
					Child(ui.Icon(icons.FolderGit2).Size(13).Color(t.TextMuted)).
					Child(ui.Text(r).FontSize(12).Semibold().Grow(1)).
					Child(counts(ctx, rs.added, rs.removed, 12)))
			}
		}
		if dir == "." {
			dir = ""
		}
		list = list.Child(ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space2).Height(ui.Px(26)).Radius(radiusS).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label(path).TabIndex(0).
			OnClick(func() {
				if i, ok := headerAt[path]; ok {
					h.ScrollToItem(i)
				}
			}).
			Child(ui.Text(fileStatus(f.Status)).Mono().FontSize(12).Semibold().TextColor(fileStatusColor(ctx, f.Status)).Width(ui.Px(12))).
			Child(ui.Text(filepath.Base(path)).FontSize(13).Medium().NoWrap()).
			Child(ui.Text(dir).FontSize(12).TextColor(t.TextSubtle).TruncateHead().Grow(1).MinWidth(ui.Px(0))).
			Child(counts(ctx, f.Added, f.Removed, 12)))
	}

	gutter := func(n int) ui.Element {
		s := ""
		if n > 0 {
			s = strconv.Itoa(n)
		}
		return ui.Div().Row().Justify(ui.End).Width(ui.Px(40)).PaddingRight(space2).Shrink(0).
			Child(ui.Text(s).Mono().FontSize(11).TextColor(t.TextSubtle))
	}
	addBg, delBg := t.Success.Alpha(0.16), t.Danger.Alpha(0.16)
	addWord, delWord := t.Success.Alpha(0.36), t.Danger.Alpha(0.36)
	// lineText is a line's code, colored by syntax, with its changed part marked.
	lineText := func(f *diffFile, i int) *ui.TextElement {
		l := f.lines[i]
		txt := ui.Text(l.text).Mono().FontSize(12).NoWrap().TextColor(t.Text)
		for _, sp := range f.colors[i] {
			if _, st, ok := highlight.Closest(t.Syntax, sp.capture); ok && sp.end <= len(l.text) {
				txt = txt.Highlight(sp.start, sp.end, ui.Highlight{Color: st.Color})
			}
		}
		if l.wordEnd > l.wordStart {
			bg := addWord
			if l.kind == '-' {
				bg = delWord
			}
			txt = txt.Highlight(l.wordStart, l.wordEnd, ui.Highlight{Background: bg})
		}
		return txt
	}
	fileHeader := func(f *diffFile) ui.Element {
		path := f.path
		return ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space3).Height(ui.Px(diffRow)).
			Background(t.SurfaceActive).Cursor(ui.CursorPointer).
			Role(access.RoleButton).Label(path).TabIndex(0).
			OnClick(func() {
				next := map[string]bool{}
				for k, v := range folded.Peek() {
					next[k] = v
				}
				next[path] = !next[path]
				folded.Set(next)
			}).
			Child(ui.Icon(map[bool]*icons.Icon{true: icons.ChevronRight, false: icons.ChevronDown}[fold[path]]).Size(13).Color(t.TextMuted)).
			Child(ui.Text(path).Mono().FontSize(12).Semibold().TruncateHead().Grow(1).MinWidth(ui.Px(0))).
			Child(counts(ctx, f.add, f.del, 11)).
			Child(ui.Div().Size(ui.Px(20), ui.Px(20)).Center().Radius(radiusS).Cursor(ui.CursorPointer).
				Hover(ui.Style{Background: t.SurfaceHover}).
				Role(access.RoleButton).Label("Copy path").Tooltip("Copy path").TabIndex(0).
				OnClick(func() {
					ctx.SetClipboard(path)
					ctx.Toast(ui.ToastInfo, path).Title("Path copied")
				}).
				Child(ui.Icon(icons.Copy).Size(12).Color(t.TextSubtle)))
	}
	hunk := func(text string, indent float32) ui.Element {
		return ui.Div().Row().Align(ui.Center).Height(ui.Px(diffRow)).Background(t.Surface).PaddingLeft(indent).
			Child(ui.Text(text).Mono().FontSize(11).TextColor(t.TextSubtle).NoWrap())
	}

	var body ui.Element
	if !split.Get() || !roomy {
		body = ui.UniformList(len(rows), func(i int) ui.Element {
			r := rows[i]
			f := r.file
			if r.line < 0 {
				return fileHeader(f)
			}
			l := f.lines[r.line]
			if l.kind == '@' {
				return hunk(l.text, 96)
			}
			row := ui.Div().Row().Align(ui.Center).Height(ui.Px(diffRow))
			sign, signColor := " ", t.TextSubtle
			switch l.kind {
			case '+':
				row, sign, signColor = row.Background(addBg), "+", t.Success
			case '-':
				row, sign, signColor = row.Background(delBg), "−", t.Danger
			}
			return row.
				Child(gutter(l.old)).
				Child(gutter(l.new)).
				Child(ui.Div().Width(ui.Px(16)).Shrink(0).Child(ui.Text(sign).Mono().FontSize(12).TextColor(signColor))).
				Child(lineText(f, r.line))
		}).Handle(h).Style(ui.Style{Grow: 1}).Label("Diff")
	} else {
		// Side by side: removed lines on the left, added on the right, paired in order.
		type pair struct {
			file        *diffFile
			left, right int // line indexes; -1 for none; both -2 for the file header
		}
		var pairs []pair
		for _, f := range files {
			pairs = append(pairs, pair{f, -2, -2})
			if fold[f.path] {
				continue
			}
			for i := 0; i < len(f.lines); {
				switch f.lines[i].kind {
				case '@', ' ':
					pairs = append(pairs, pair{f, i, i})
					i++
				default:
					// A change block runs until the next unchanged line; its removed lines go left and
					// its added lines right, in order, however they interleave.
					var dels, adds []int
					for i < len(f.lines) && (f.lines[i].kind == '-' || f.lines[i].kind == '+') {
						if f.lines[i].kind == '-' {
							dels = append(dels, i)
						} else {
							adds = append(adds, i)
						}
						i++
					}
					for k := 0; k < max(len(dels), len(adds)); k++ {
						p := pair{f, -1, -1}
						if k < len(dels) {
							p.left = dels[k]
						}
						if k < len(adds) {
							p.right = adds[k]
						}
						pairs = append(pairs, p)
					}
				}
			}
		}
		side := func(f *diffFile, i int, left bool) ui.Element {
			box := ui.Div().Row().Align(ui.Center).Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(0)).Height(ui.Full).
				PaddingRight(space2).ClipContent()
			if i < 0 {
				return box.Background(t.Text.Alpha(0.045))
			}
			l := f.lines[i]
			n := l.new
			if left {
				n = l.old
			}
			switch {
			case l.kind == '-':
				box = box.Background(delBg)
			case l.kind == '+':
				box = box.Background(addBg)
			}
			return box.Child(gutter(n)).Child(lineText(f, i))
		}
		body = ui.UniformList(len(pairs), func(i int) ui.Element {
			p := pairs[i]
			if p.left == -2 {
				return fileHeader(p.file)
			}
			if p.left >= 0 && p.file.lines[p.left].kind == '@' {
				return hunk(p.file.lines[p.left].text, 48)
			}
			return ui.Div().Row().Height(ui.Px(diffRow)).
				Child(side(p.file, p.left, true)).
				Child(ui.Div().Width(ui.Px(1)).Height(ui.Full).Background(t.BorderStrong).Shrink(0)).
				Child(side(p.file, p.right, false))
		}).Handle(h).Style(ui.Style{Grow: 1}).Label("Diff, side by side")
	}

	summary := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space4).Height(ui.Px(36)).Shrink(0).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).
		Child(subhead(fmt.Sprintf("%d files changed", len(stats)))).
		Child(ui.Text(fmt.Sprintf("+%d", added)).FontSize(12).TextColor(t.Success)).
		Child(ui.Text(fmt.Sprintf("−%d", removed)).FontSize(12).TextColor(t.Danger)).
		Child(ui.Div().Grow(1)).
		Child(ui.If(!isCompact(ctx), segmented(ctx, split, roomy, canWiden, widen))) // a compact window has room for one column only
	return ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).
		Child(summary).
		Child(list).
		Child(body)
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
	return t.Warning
}

// diffSplit is whether diffs show side by side; it is kept across threads.
var diffSplit = ui.NewSignal(false)

// segmented is the Unified / Split switch of the diff.
func segmented(ctx *ui.Context, split *ui.Signal[bool], roomy bool, canWiden func() bool, widen func()) ui.Element {
	t := ctx.Theme()
	shown := split.Get() && roomy // what the diff actually shows
	canSplit := roomy || canWiden()
	opt := func(label string, on bool) ui.Element {
		d := ui.Div().PaddingX(space3).Height(ui.Px(24)).Radius(radiusS - 1).Center().
			Role(access.RoleButton).Label(label).TabIndex(0).AccessSelected(shown == on)
		if on && !canSplit {
			return d.Opacity(0.45).Tooltip("The window is too narrow to compare side by side").
				Child(ui.Text(label).FontSize(12).TextColor(t.TextMuted))
		}
		d = d.Cursor(ui.CursorPointer).OnClick(func() {
			if on && !roomy {
				widen() // the panel grows to fit; the diff goes side by side on the next render
			}
			split.Set(on)
		})
		if shown == on {
			return d.Background(t.SurfaceActive).Child(ui.Text(label).FontSize(12).Medium())
		}
		return d.Hover(ui.Style{Background: t.SurfaceHover}).Child(ui.Text(label).FontSize(12).TextColor(t.TextMuted))
	}
	return ui.Div().Row().Align(ui.Center).Gap(2).Padding(2).Radius(radiusS+2).Border(1, t.Border).Background(t.Surface).
		Child(opt("Unified", false)).
		Child(opt("Split", true))
}

// counts shows a change's added and removed lines, leaving out a side that is zero.
func counts(ctx *ui.Context, added, removed int, size float32) ui.Element {
	t := ctx.Theme()
	row := ui.Div().Row().Gap(6).Shrink(0)
	if added > 0 || removed == 0 {
		row = row.Child(ui.Text(fmt.Sprintf("+%d", added)).Mono().FontSize(size).TextColor(t.Success))
	}
	if removed > 0 {
		row = row.Child(ui.Text(fmt.Sprintf("−%d", removed)).Mono().FontSize(size).TextColor(t.Danger))
	}
	return row
}
