package gui

import (
	"context"
	"fmt"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

// landSpec is a thread's main landing action, described rather than drawn: Open PR, Ready for review,
// Merge (its pull request, or locally without a forge, or one repository of several), a note that
// auto-merge will do it, or a disabled button saying why it can't land yet. The thread's header, its
// merge card in Solo's chat and the merge pill above that chat all draw it with landElement, so they
// offer the same action, run it the same way and show the same busy state.
type landSpec struct {
	label string // "Open PR", "Merge all", "Merge api #142", …
	// short is label for a narrow place: one repository's merge by its pull request's number alone,
	// "Merge #142" (partMergeShort), for a place that names the repository beside it.
	short    string
	tip      string
	icon     *icons.Icon
	key      string // the button's key, "" for none
	run      func() // the button's action; nil for none (a disabled button, or a split button's inert half)
	primary  bool
	disabled bool
	loading  bool
	status   bool // a note rather than a button: "Auto-merge on"
	// menu is a split button's menu: drafts, one repository at a time. nil for a plain button.
	menu func() []ui.MenuItem
}

// landElement draws a landing action as a small button, split when it has a menu; short draws it with
// its short label.
func (a *App) landElement(ctx *ui.Context, s landSpec, short bool) ui.Element {
	t := ctx.Theme()
	label := s.label
	if short && s.short != "" {
		label = s.short
	}
	if s.status {
		return ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).Height(ui.Px(28)).Shrink(0).
			Tooltip(s.tip).Role(access.RoleStaticText).Label(label).
			Child(ui.Icon(s.icon).Size(13).Color(t.TextMuted)).
			Child(ui.Text(label).FontSize(12).Medium().TextColor(t.TextMuted).NoWrap())
	}
	if s.menu != nil {
		run := s.run
		if run == nil {
			run = func() {}
		}
		b := ui.SplitButton(label, run, s.menu).Icon(s.icon).Small().Key(s.key)
		if s.tip != "" {
			b = b.Tooltip(s.tip)
		}
		if s.primary {
			b = b.Primary()
		} else {
			b = b.Secondary()
		}
		return b.Disabled(s.disabled).Loading(s.loading)
	}
	b := ui.Button(label).Icon(s.icon).Small()
	if s.key != "" {
		b = b.Key(s.key)
	}
	if s.tip != "" {
		b = b.Tooltip(s.tip)
	}
	if s.run != nil {
		b = b.OnClick(s.run)
	}
	if s.primary {
		b = b.Primary()
	} else {
		b = b.Secondary()
	}
	return b.Disabled(s.disabled).Loading(s.loading)
}

// landAction is what a thread's header and its merge card offer to land it: its action when it can
// land, or while it is busy with changes to land (working, or waiting on background work) the disabled
// button of landBlocked. nil with nothing to land. short is landElement's: the header asks for it, the
// card doesn't.
func (a *App) landAction(ctx *ui.Context, th core.ThreadView, short bool) ui.Element {
	s, ok := a.landActionSpec(th)
	if !ok {
		return nil
	}
	return a.landElement(ctx, s, short)
}

// cardLand is the landing action a merge card offers: landAction's, or when there is none, the
// disabled button saying why.
func (a *App) cardLand(th core.ThreadView) landSpec {
	if s, ok := a.landActionSpec(th); ok {
		return s
	}
	return a.blockedSpec(th)
}

// landActionSpec is landAction's action; false with nothing to land.
func (a *App) landActionSpec(th core.ThreadView) (landSpec, bool) {
	if th.Resolving {
		return a.controlSpec(th) // it merges by itself once the conflicts are resolved
	}
	busy := core.Active(th.Status) || th.Status == core.StatusQueued
	if busy && th.Scratch {
		return landSpec{}, false // nothing it is meant to land, so nothing to wait for
	}
	if busy && th.IsWorker && !th.InPlace && (th.Changed > 0 || len(openPRs(th)) > 0) {
		return a.blockedSpec(th), true
	}
	if !th.CanOpenPR && !th.CanMerge && !th.CanMarkReady && len(openPRs(th)) == 0 {
		return landSpec{}, false
	}
	return a.controlSpec(th)
}

// blockedSpec is the landing button of a thread that can't land now: Open PR, or Merge, as it will be
// once it can, disabled, its tooltip saying why.
func (a *App) blockedSpec(th core.ThreadView) landSpec {
	label, icon := "Merge", icons.GitMerge
	if th.Forge != forge.None && len(openPRs(th)) == 0 {
		label, icon = "Open "+th.Forge.Noun(), icons.GitPullRequestArrow
	}
	return landSpec{label: label, icon: icon, disabled: true, tip: landBlockedReason(th, a.now()), key: "land-blocked-" + th.ID}
}

// controlSpec is a thread's main landing action with its busy state: while the action runs, the button
// shows it is busy. A scratch thread's is a quiet one. false when there is nothing to land.
func (a *App) controlSpec(th core.ThreadView) (landSpec, bool) {
	if th.Resolving {
		return landSpec{label: "Merge", icon: icons.GitMerge, primary: true, loading: true, key: "land-" + th.ID,
			tip: "Resolving conflicts: newer work landed in " + th.BaseBranch + " since this started. " +
				"The thread is merging it in, then Solo merges."}, true
	}
	s, ok := a.buttonSpec(th)
	if !ok {
		return s, false
	}
	if th.Scratch {
		// The user doesn't mean to land a scratch thread, but may change their mind: the button stays,
		// without the emphasis that asks for it.
		s.primary = false
	}
	if a.busy.Get()[th.ID] && !s.status {
		s.loading = true
	}
	return s, true
}

// buttonSpec is controlSpec's action, without the busy state.
func (a *App) buttonSpec(th core.ThreadView) (landSpec, bool) {
	id := th.ID
	if th.MultiRepo() {
		return a.multiSpec(th)
	}
	switch {
	case th.CanOpenPR:
		noun := th.Forge.Noun()
		return landSpec{label: "Open " + noun, icon: icons.GitPullRequestArrow, primary: true, key: "open-pr-" + id,
			run: func() { a.openPR(id, false) },
			menu: func() []ui.MenuItem {
				return []ui.MenuItem{{Label: "Open as draft", Icon: icons.GitPullRequestDraft, OnClick: func() { a.openPR(id, true) }}}
			}}, true
	case th.CanMarkReady:
		return landSpec{label: "Ready for review", short: "Ready", icon: icons.GitPullRequest, primary: true,
			tip: fmt.Sprintf("Mark %s #%d ready for review", th.PR.Kind.Noun(), th.PR.Number),
			run: func() { a.markReady(id, "Marking it ready") }}, true
	case th.PR != nil && th.PR.State == forge.StateOpen:
		if th.CanMerge {
			return landSpec{label: "Merge", icon: icons.GitMerge, primary: true,
				tip: fmt.Sprintf("Merge %s #%d into %s", th.PR.Kind.Noun(), th.PR.Number, th.BaseBranch),
				run: func() { a.merge(id) }}, true
		}
		if th.PR.AutoMerge {
			return landSpec{label: "Auto-merge on", icon: icons.Timer, status: true, tip: "It merges once its required checks pass"}, true
		}
		return landSpec{label: "Merge", icon: icons.GitMerge, disabled: true, tip: "Can't merge yet: " + th.PR.Blocked}, true
	case th.CanMerge:
		if th.Forge == forge.None {
			// No remote to open a pull request on: it merges straight into the local branch.
			return landSpec{label: "Merge locally", icon: icons.GitMerge, primary: true,
				tip: "No remote, so no pull request: merges " + branchLabel(th) + " into your local " + th.BaseBranch,
				run: func() { a.merge(id) }}, true
		}
		return landSpec{label: "Merge", icon: icons.GitMerge, primary: true, tip: "Merge " + th.Branch + " into " + th.BaseBranch,
			run: func() { a.merge(id) }}, true
	}
	return landSpec{}, false
}

// markReady marks a thread's draft pull requests ready for review.
func (a *App) markReady(id, what string) {
	a.runBusy(id, what, func(ctx context.Context) error { return a.b.MarkReady(ctx, id) })
}

// multiSpec is buttonSpec for a thread in several repositories: a split button that acts on all of
// them, with each repository on its own in its menu. Repositories with nothing to land don't count:
// with one pull request (or one branch to merge locally), its button names that one.
func (a *App) multiSpec(th core.ThreadView) (landSpec, bool) {
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
		return landSpec{label: "Open " + plural(len(canOpen), noun), icon: icons.GitPullRequestArrow, primary: true, key: "open-pr-" + id,
			run: func() { a.openPRFor(id, "", false) },
			menu: func() []ui.MenuItem {
				items := []ui.MenuItem{{Label: "Open " + plural(len(canOpen), noun) + " as drafts", Icon: icons.GitPullRequestDraft,
					OnClick: func() { a.openPRFor(id, "", true) }}, ui.MenuSeparator()}
				for _, p := range canOpen {
					repo := p.Repo
					items = append(items, ui.MenuItem{Label: noun + " for " + p.Name + " only", Icon: icons.GitPullRequestArrow,
						OnClick: func() { a.openPRFor(id, repo, false) }})
				}
				return items
			}}, true
	case len(drafts) > 0:
		return landSpec{label: "Ready for review", short: "Ready", icon: icons.GitPullRequest, primary: true,
			tip: "Mark " + plural(len(drafts), "draft") + " ready for review",
			run: func() { a.markReady(id, "Marking them ready") }}, true
	}
	return a.mergeSpecMulti(th)
}

// mergeSpecMulti is the merge button of a thread in several repositories: named for the one repository
// it merges ("Merge api #142", short "Merge #142"), or "Merge all" when two or more have something to
// merge. false with nothing. The menu always names each repository in full.
func (a *App) mergeSpecMulti(th core.ThreadView) (landSpec, bool) {
	id := th.ID
	targets, ready := mergeTargets(th)
	autoAll := len(targets) > 0
	for _, p := range targets {
		autoAll = autoAll && p.PR != nil && p.PR.AutoMerge && !p.CanMerge
	}
	switch len(targets) {
	case 0:
		return landSpec{}, false
	case 1:
		p := targets[0]
		repo := p.Repo
		switch {
		case p.CanMerge:
			return landSpec{label: partMergeLabel(p), short: partMergeShort(p), icon: icons.GitMerge, primary: true,
				tip: partMergeTip(p), key: "merge-" + id, run: func() { a.mergeRepo(id, repo) }}, true
		case autoAll:
			return landSpec{label: "Auto-merge on", icon: icons.Timer, status: true, tip: "It merges once its required checks pass"}, true
		}
		tip := "Can't merge yet"
		if p.PR.Blocked != "" {
			tip += ": " + p.PR.Blocked
		}
		return landSpec{label: partMergeLabel(p), short: partMergeShort(p), icon: icons.GitMerge, disabled: true,
			tip: tip, key: "merge-" + id}, true
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
		return landSpec{label: "Merge all", icon: icons.GitMerge, primary: true, key: "merge-" + id,
			tip: "Merge each repository into its base branch", run: func() { a.mergeRepo(id, "") }, menu: menu}, true
	case len(ready) == 1:
		repo := ready[0].Repo
		return landSpec{label: partMergeLabel(ready[0]), short: partMergeShort(ready[0]), icon: icons.GitMerge, primary: true,
			key: "merge-" + id, tip: partMergeTip(ready[0]), run: func() { a.mergeRepo(id, repo) }, menu: menu}, true
	case len(ready) > 1:
		return landSpec{label: fmt.Sprintf("Merge %d ready", len(ready)), icon: icons.GitMerge, primary: true, key: "merge-" + id,
			run: func() { a.mergeRepo(id, "") }, menu: menu}, true
	case autoAll:
		return landSpec{label: "Auto-merge on", icon: icons.Timer, status: true, tip: "Each merges once its required checks pass"}, true
	}
	return landSpec{label: "Merge", icon: icons.GitMerge, key: "merge-" + id, menu: menu}, true
}
