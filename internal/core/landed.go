//go:build !js

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/git"
)

// A thread can land its work without Solo's merge or PR buttons: it opens a pull request itself
// (gh pr create), the user opens one for a branch it pushed, or its changes reach the base branch some
// other way (a squash merge on the forge, a cherry-pick). Solo finds these when a turn ends, on load,
// and every landedSweep while a thread waits, and then treats them as its own: a found pull request is
// followed like one Solo opened, and work already in the base branch closes the thread as merged,
// unless that branch is itself an open pull request's: the thread then follows that one.

// createsPR matches a shell command that opens a pull or merge request.
var createsPR = regexp.MustCompile(`\b(gh\s+pr\s+create|glab\s+mr\s+create)\b`)

// prHintsOf returns the pull request addresses a Bash call printed opening them.
func prHintsOf(d ToolData) []string {
	if d.Name != "Bash" || d.IsError || d.Result == "" {
		return nil
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(d.Input, &in) != nil || !createsPR.MatchString(in.Command) {
		return nil
	}
	return forge.FindURLs(d.Result)
}

// notePRHintsLocked keeps the pull requests a finished tool call opened, to link at the turn's end.
// Call with c.mu held.
func (t *thread) notePRHintsLocked(d ToolData) {
	for _, u := range prHintsOf(d) {
		if !slices.Contains(t.prHints, u) {
			t.prHints = append(t.prHints, u)
		}
	}
}

// transcriptPRs searches a thread's saved transcript for pull requests it opened itself: those from
// before Solo last started, or from before Solo looked for them.
func (c *Core) transcriptPRs(ctx context.Context, id string) []string {
	items, err := c.db.Items(ctx, id, 0)
	if err != nil {
		return nil
	}
	var out []string
	for _, it := range items {
		if it.Kind != ItemTool {
			continue
		}
		var d ToolData
		if json.Unmarshal(it.Data, &d) != nil {
			continue
		}
		for _, u := range prHintsOf(d) {
			if !slices.Contains(out, u) {
				out = append(out, u)
			}
		}
	}
	return out
}

// prGrace allows for clock skew between Solo and the forge when telling a pull request the thread
// opened from an older one on a branch of the same name.
const prGrace = 2 * time.Minute

// findPRs links pull requests opened outside Solo to the parts that have none (or only a closed one):
// first those the thread printed opening (gh pr create), then any from a branch it pushed. A found
// pull request that already merged lands its part the usual way (applyPR).
func (c *Core) findPRs(ctx context.Context, id string) {
	c.mu.Lock()
	t := c.threads[id]
	if t == nil {
		c.mu.Unlock()
		return
	}
	hints := slices.Clone(t.prHints)
	scan := !t.prScanned
	t.prScanned = true
	c.mu.Unlock()
	if scan {
		for _, u := range c.transcriptPRs(ctx, id) {
			if !slices.Contains(hints, u) {
				hints = append(hints, u)
			}
		}
	}
	v, ok := c.Thread(id)
	if !ok || !v.IsWorker || Finished(v.Status) {
		return
	}
	var used []string // hints linked, or that belong to no part of the thread
	for _, p := range v.Parts {
		if p.InPlace || p.Worktree == "" || p.Forge == forge.None || (p.PR != nil && p.PR.State != forge.StateClosed) {
			continue
		}
		client, err := forge.New(p.Forge)
		if err != nil {
			continue
		}
		remote := git.RemoteURL(ctx, p.Repo)
		var found *forge.PR
		for _, u := range hints {
			// Its address says whose it is, unless origin's doesn't (a local path); then its branch must
			// be one of this repository's.
			known := forge.KnownRemote(remote)
			if slices.Contains(used, u) || (known && !forge.SameRepo(u, remote)) || (p.PR != nil && p.PR.URL == u) {
				continue
			}
			vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			pr, err := client.View(vctx, p.Worktree, forge.PR{Kind: p.Forge, URL: u, Number: forge.Number(u)})
			cancel()
			if err != nil {
				c.log.Warn("core: reading a pull request the thread opened", "url", u, "err", err)
				continue
			}
			used = append(used, u) // linked below, or not this repository's
			if !known && (pr.Head == "" || !git.BranchExists(ctx, p.Repo, pr.Head)) {
				continue
			}
			if pr.State != forge.StateClosed {
				found = &pr
				break
			}
		}
		if found == nil {
			found = c.prByHead(ctx, client, v, p)
		}
		if found == nil {
			continue
		}
		c.addNotice(id, "info", fmt.Sprintf("Found %s #%d%s, opened outside Solo; Solo follows it now.", found.Kind.Noun(), found.Number, inRepo(v, p)))
		event := PRFound
		if found.State == forge.StateMerged {
			event = "" // applyPR says it merged, and lands the part
		}
		c.applyPR(id, p.Repo, *found, event)
	}
	// Keep the hints not linked yet that belong to one of its repositories, to try again.
	var remotes []string
	for _, p := range v.Parts {
		remotes = append(remotes, git.RemoteURL(ctx, p.Repo))
	}
	ours := func(u string) bool {
		return slices.ContainsFunc(remotes, func(r string) bool { return !forge.KnownRemote(r) || forge.SameRepo(u, r) })
	}
	c.mu.Lock()
	if t := c.threads[id]; t != nil {
		keep := slices.DeleteFunc(slices.Concat(t.prHints, hints), func(u string) bool {
			return slices.Contains(used, u) || !ours(u) ||
				slices.ContainsFunc(t.parts, func(p *part) bool { return p.PR != nil && p.PR.URL == u })
		})
		slices.Sort(keep)
		t.prHints = slices.Compact(keep)
	}
	c.mu.Unlock()
}

// prByHead finds an open or merged pull request from a branch the thread pushed: its own branch, or
// the one its worktree is on now when it made another. Only one opened since the thread started.
func (c *Core) prByHead(ctx context.Context, client *forge.Client, v ThreadView, p PartView) *forge.PR {
	heads := []string{p.Branch}
	if cur := git.CurrentBranch(ctx, p.Worktree); cur != "" && cur != p.Branch && cur != p.BaseBranch {
		heads = append(heads, cur)
	}
	for _, head := range heads {
		if !git.Pushed(ctx, p.Worktree, head) {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		prs, err := client.ByHead(lctx, p.Worktree, head)
		cancel()
		if err != nil {
			c.log.Warn("core: looking for a pull request", "branch", head, "err", err)
			continue
		}
		var pick *forge.Listed
		for i, pr := range prs {
			if pr.State == forge.StateClosed || pr.Created.Before(v.CreatedAt.Add(-prGrace)) {
				continue
			}
			if p.PR != nil && p.PR.URL == pr.URL {
				continue // the closed one it has
			}
			if pick == nil || (pr.State == forge.StateOpen && pick.State != forge.StateOpen) {
				pick = &prs[i]
			}
		}
		if pick == nil {
			continue
		}
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		pr, err := client.View(vctx, p.Worktree, pick.PR)
		cancel()
		if err != nil {
			pr = pick.PR
		}
		return &pr
	}
	return nil
}

// fetchEvery is how often Solo fetches a base branch to see whether a thread's work reached it.
const fetchEvery = 2 * time.Minute

// fetchBase updates origin's copy of a base branch, at most every fetchEvery.
func (c *Core) fetchBase(ctx context.Context, repo, branch string) {
	if git.RemoteURL(ctx, repo) == "" {
		return
	}
	key := repo + "\x00" + branch
	c.mu.Lock()
	last := c.fetched[key]
	fresh := time.Since(last) < fetchEvery
	if !fresh {
		c.fetched[key] = time.Now()
	}
	c.mu.Unlock()
	if fresh {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := git.Fetch(fctx, repo, branch); err != nil {
		c.log.Warn("core: fetching to look for landed work", "repo", repo, "branch", branch, "err", err)
	}
}

// basePR finds the open pull request that a part's base branch is the branch of, once the part's work
// is in origin's copy of that branch: a thread started from a pull request's branch to add to it. Its
// work is then in a pull request still to merge, not landed. Not for the default branch, which every
// other thread starts from. ok is false when the forge couldn't say, to look again later.
func (c *Core) basePR(ctx context.Context, p PartView) (pr *forge.PR, ok bool) {
	if p.Forge == forge.None || p.BaseBranch == git.DefaultBranch(ctx, p.Repo) || !git.InRemoteBase(ctx, p.Worktree, p.BaseBranch) {
		return nil, true
	}
	client, err := forge.New(p.Forge)
	if err != nil {
		return nil, true // without its CLI no pull request can be followed
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	prs, err := client.ByHead(lctx, p.Worktree, p.BaseBranch)
	cancel()
	if err != nil {
		c.log.Warn("core: looking for the base branch's pull request", "branch", p.BaseBranch, "err", err)
		return nil, false
	}
	for _, l := range prs {
		if l.State != forge.StateOpen {
			continue
		}
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		found, err := client.View(vctx, p.Worktree, l.PR)
		cancel()
		if err != nil {
			c.log.Warn("core: reading the base branch's pull request", "url", l.URL, "err", err)
			return nil, false
		}
		if found.State != forge.StateOpen {
			return nil, true // it merged or closed just now
		}
		return &found, true
	}
	return nil, true
}

// findLanded closes the parts whose changes are already in their base branch, by ancestry or by
// content (a squash merge): their change count drops to 0, and once nothing is left to land the
// thread is merged, as with Solo's own merge. A part with an open or merged pull request is left to
// it, and so is one whose base branch is an open pull request's (basePR): that pull request becomes
// the part's, and the thread is merged when it merges.
func (c *Core) findLanded(ctx context.Context, id string) {
	v, ok := c.Thread(id)
	if !ok || !v.IsWorker || v.InTurn || Finished(v.Status) || Active(v.Status) || v.Status == StatusQueued || v.Resolving {
		return
	}
	var landed []PartView
	for _, p := range v.Parts {
		if p.InPlace || p.Worktree == "" || p.Changed <= 0 || (p.PR != nil && p.PR.State != forge.StateClosed) {
			continue
		}
		c.fetchBase(ctx, p.Repo, p.BaseBranch)
		if !git.InBase(ctx, p.Worktree, p.BaseBranch) {
			continue
		}
		pr, ok := c.basePR(ctx, p)
		switch {
		case !ok:
		case pr != nil:
			c.addNotice(id, "info", fmt.Sprintf("Its changes are in %s, the branch of open %s #%d%s. Solo follows it now; the thread is merged when it merges.",
				p.BaseBranch, pr.Kind.Noun(), pr.Number, inRepo(v, p)))
			c.applyPR(id, p.Repo, *pr, PRFound)
		default:
			landed = append(landed, p)
		}
	}
	if len(landed) == 0 {
		return
	}
	c.mu.Lock()
	if t := c.threads[id]; t != nil {
		for _, lp := range landed {
			if p := t.part(lp.Repo); p != nil {
				p.changed = 0
			}
		}
	}
	c.mu.Unlock()
	for _, p := range landed {
		where := p.BaseBranch
		if v.MultiRepo() {
			where += " in " + p.Name
		}
		c.addNotice(id, "info", "Already in "+where+": its changes landed outside Solo (committed or merged there directly, or squash-merged).")
	}
	c.notify(Change{Kind: ChangeThread, ThreadID: id, PrevStatus: v.Status, Status: v.Status})
	c.finishIfLanded(id)
}

// landedSweep is how often the PR watcher looks for work landed outside Solo in threads that wait
// with changes and no open pull request.
const landedSweep = 5 * time.Minute

// sweepLanded runs findPRs and findLanded (through countChanges) for each settled worker with changes
// and no open pull request, at most every landedSweep.
func (c *Core) sweepLanded() {
	c.mu.Lock()
	if time.Since(c.lastSweep) < landedSweep {
		c.mu.Unlock()
		return
	}
	c.lastSweep = time.Now()
	var ids []string
	for id, t := range c.threads {
		v := t.view()
		if !v.IsWorker || !v.HasWorktree || v.InTurn || Finished(v.Status) || Active(v.Status) || v.Status == StatusQueued || v.Changed <= 0 {
			continue
		}
		if slices.ContainsFunc(v.Parts, func(p PartView) bool { return p.PR != nil && p.PR.State == forge.StateOpen }) {
			continue // its pull request is followed already
		}
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.countChanges(id)
	}
}
