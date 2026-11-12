//go:build !js

package core

import (
	"cmp"
	"context"
	"fmt"
	"github.com/tyler-builds/solo/internal/agent"
	"path/filepath"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/git"
)

// uncommittedPrefix starts the message of the commit Solo makes of a worker's uncommitted changes.
const uncommittedPrefix = "chore: uncommitted work from "

// forgeFor finds where a repository's pull requests go, once per repository.
func (c *Core) forgeFor(repo string) forge.Kind {
	c.mu.Lock()
	k, ok := c.forges[repo]
	c.mu.Unlock()
	if ok {
		return k
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	override, _ := git.Run(ctx, repo, "config", "--get", "solo.forge")
	k = forge.Detect(override, git.RemoteURL(ctx, repo))
	c.mu.Lock()
	c.forges[repo] = k
	c.mu.Unlock()
	return k
}

// OpenPR pushes a worker's branches and opens a pull request (or merge request) in each repository
// with changes and none open yet, against the branch it started from; draft opens them as drafts.
// With AutoMerge on, ready pull requests also get the forge's auto-merge (a draft gets it when marked
// ready). It returns the first pull request opened.
func (c *Core) OpenPR(ctx context.Context, threadID string, draft bool) (forge.PR, error) {
	prs, err := c.OpenPRFor(ctx, threadID, "", draft)
	if err != nil {
		return forge.PR{}, err
	}
	return prs[0], nil
}

// OpenPRFor is OpenPR for one of the worker's repositories (by path or folder name; "" for all).
// Pull requests a thread opens in several repositories link to each other.
func (c *Core) OpenPRFor(ctx context.Context, threadID, repo string, draft bool) ([]forge.PR, error) {
	release, err := c.land(threadID)
	if err != nil {
		return nil, err
	}
	defer release()
	v, ok := c.Thread(threadID)
	if !ok {
		return nil, fmt.Errorf("no thread %s", threadID)
	}
	var todo []PartView
	for _, p := range v.Parts {
		if p.CanOpenPR && (repo == "" || p.Repo == repo || p.Name == repo) {
			todo = append(todo, p)
		}
	}
	if len(todo) == 0 {
		return nil, fmt.Errorf("thread %q has nothing to open a pull request for", v.Title)
	}
	var opened []forge.PR
	bodies := map[string]string{}
	for _, p := range todo {
		pr, body, err := c.openOne(ctx, v, p, draft)
		if err != nil {
			if len(opened) > 0 {
				break // keep what opened; the rest can be retried
			}
			return nil, err
		}
		opened = append(opened, pr)
		bodies[pr.URL] = body
	}
	if v.MultiRepo() {
		c.linkPRs(ctx, threadID, bodies)
	}
	c.pokePRs()
	return opened, nil
}

func (c *Core) openOne(ctx context.Context, v ThreadView, p PartView, draft bool) (forge.PR, string, error) {
	client, err := forge.New(p.Forge)
	if err != nil {
		return forge.PR{}, "", err
	}
	if _, err := git.CommitAll(ctx, p.Worktree, uncommittedPrefix+v.Title); err != nil {
		return forge.PR{}, "", fmt.Errorf("committing %s's changes: %w", p.Name, err)
	}
	if err := git.Push(ctx, p.Worktree, p.Branch); err != nil {
		return forge.PR{}, "", fmt.Errorf("pushing %s to %s: %w", p.Branch, p.Name, err)
	}
	title, body := c.prText(ctx, v, p)
	pr, err := client.Open(ctx, p.Worktree, p.BaseBranch, p.Branch, title, body, draft)
	if err != nil {
		c.addNotice(v.ID, "error", fmt.Sprintf("Couldn't open the %s in %s: %v", p.Forge.Noun(), p.Name, err))
		return forge.PR{}, "", err
	}
	what := "Opened"
	if draft {
		what = "Opened draft"
	}
	c.addNotice(v.ID, "info", fmt.Sprintf("%s %s #%d against %s%s.", what, p.Forge.Noun(), pr.Number, p.BaseBranch, inRepo(v, p)))
	if !draft {
		pr = c.autoMerge(ctx, v.ID, client, p.Worktree, pr)
	}
	c.applyPR(v.ID, p.Repo, pr, PROpened)
	return pr, body, nil
}

// inRepo is " in api" for a multi-repository thread, else "".
func inRepo(v ThreadView, p PartView) string {
	if v.MultiRepo() {
		return " in " + p.Name
	}
	return ""
}

// linkPRs adds to each newly opened pull request's description a list of the thread's pull requests
// in its other repositories.
func (c *Core) linkPRs(ctx context.Context, threadID string, bodies map[string]string) {
	v, ok := c.Thread(threadID)
	if !ok {
		return
	}
	var all []PartView
	for _, p := range v.Parts {
		if p.PR != nil && p.PR.State != forge.StateClosed {
			all = append(all, p)
		}
	}
	if len(all) < 2 {
		return
	}
	for _, p := range all {
		body, ok := bodies[p.PR.URL]
		if !ok {
			continue
		}
		var b strings.Builder
		if strings.TrimSpace(body) != "" {
			b.WriteString(strings.TrimSpace(body) + "\n\n")
		}
		b.WriteString("Part of a change across repositories:\n")
		for _, o := range all {
			if o.Repo != p.Repo {
				fmt.Fprintf(&b, "- %s: %s\n", o.Name, o.PR.URL)
			}
		}
		client, err := forge.New(p.PR.Kind)
		if err != nil {
			continue
		}
		if err := client.EditBody(ctx, p.Worktree, *p.PR, b.String()); err != nil {
			c.log.Warn("core: linking pull requests", "url", p.PR.URL, "err", err)
		}
	}
}

// MarkReady marks a worker's draft pull requests ready for review, and turns on auto-merge when
// Settings asks.
func (c *Core) MarkReady(ctx context.Context, threadID string) error {
	release, err := c.land(threadID)
	if err != nil {
		return err
	}
	defer release()
	v, ok := c.Thread(threadID)
	if !ok {
		return fmt.Errorf("no thread %s", threadID)
	}
	n := 0
	for _, p := range v.Parts {
		if !p.CanMarkReady {
			continue
		}
		client, err := forge.New(p.PR.Kind)
		if err != nil {
			return err
		}
		if err := client.Ready(ctx, p.Worktree, *p.PR); err != nil {
			return err
		}
		pr := *p.PR
		pr.Draft = false
		c.addNotice(threadID, "info", fmt.Sprintf("%s #%d is ready for review%s.", pr.Kind.Noun(), pr.Number, inRepo(v, p)))
		pr = c.autoMerge(ctx, threadID, client, p.Worktree, pr)
		c.applyPR(threadID, p.Repo, pr, "")
		n++
	}
	if n == 0 {
		return fmt.Errorf("thread %q has no draft to mark ready", v.Title)
	}
	c.pokePRs()
	return nil
}

// autoMerge turns on the forge's auto-merge when Settings asks for it.
func (c *Core) autoMerge(ctx context.Context, threadID string, client *forge.Client, dir string, pr forge.PR) forge.PR {
	s := c.Settings()
	if !s.AutoMerge || pr.AutoMerge {
		return pr
	}
	if err := client.EnableAutoMerge(ctx, dir, pr, s.MergeMethod); err != nil {
		c.addNotice(threadID, "warning", fmt.Sprintf("Couldn't turn on auto-merge for #%d (%v). Merge it when its checks pass.", pr.Number, err))
		return pr
	}
	pr.AutoMerge = true
	c.addNotice(threadID, "info", fmt.Sprintf("Auto-merge is on for #%d: it merges once its required checks pass.", pr.Number))
	return pr
}

const prSystem = `You write pull request titles and descriptions for a code change. Follow the user's rules exactly.
Reply in this format and nothing else:
TITLE: <one-line title>
DESCRIPTION:
<the description, in Markdown>`

const prSystemTitle = `You write pull request titles for a code change. Follow the user's rules exactly.
Reply in this format and nothing else:
TITLE: <one-line title>`

// prText writes a pull request's title and description with the PR text model, following Settings'
// rules; without a description when Settings says so. If the model fails, the title is the worker's
// one commit's subject (or the thread's title) and the description its summary.
func (c *Core) prText(ctx context.Context, v ThreadView, p PartView) (string, string) {
	s := c.Settings()
	var subjects []string // the worker's own commits, not Solo's commit of what it left uncommitted
	if log, err := git.Run(ctx, p.Worktree, "log", "--format=%s", p.BaseCommit+"..HEAD"); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
			if l != "" && !strings.HasPrefix(l, uncommittedPrefix) {
				subjects = append(subjects, l)
			}
		}
	}
	title := v.Title
	if len(subjects) == 1 {
		title = subjects[0]
	}
	body := strings.TrimSpace(v.Summary)
	if body == "" {
		body = clipText(strings.TrimSpace(v.Prompt), 2000)
	}
	if s.SkipPRDescription {
		body = ""
	}

	rules := prRules(s)
	var files []string
	if fc, err := git.Changes(ctx, p.Worktree, p.BaseCommit); err == nil {
		for i, f := range fc {
			if i == 60 {
				files = append(files, fmt.Sprintf("… and %d more", len(fc)-60))
				break
			}
			files = append(files, fmt.Sprintf("%s %s (+%d −%d)", f.Status, f.Path, f.Added, f.Removed))
		}
	}
	scope := ""
	if v.MultiRepo() {
		scope = fmt.Sprintf("\nThis pull request is the %s part of a change across %s; describe this repository's part.", p.Name, repoNames(partRepos(v.Parts)))
	}
	prompt := fmt.Sprintf("Write the pull request for this change.\n\n%s\n\nRepository: %s%s\nBranch: %s into %s\nTask title: %s\nTask:\n%s\n\nWhat the agent that made the change reported:\n%s\n\nCommits:\n%s\n\nFiles changed:\n%s",
		rules, p.Name, scope, p.Branch, p.BaseBranch, v.Title, clipText(v.Prompt, 1500),
		clipText(strings.TrimSpace(v.Summary), 3000), strings.Join(subjects, "\n"), strings.Join(files, "\n"))
	system := prSystem
	if s.SkipPRDescription {
		system = prSystemTitle
	}
	pctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	text, err := c.agent.Complete(pctx, agent.CompleteParams{Model: s.PRModel, Effort: s.PREffort, System: system,
		Prompt: prompt, ClaudePath: agent.FindClaude(s.ClaudePath).Path, TimeoutMs: 70_000})
	if err != nil {
		c.addNotice(v.ID, "warning", "Couldn't write the PR text ("+err.Error()+"), so it uses the thread's summary.")
		return title, body
	}
	t, d := parsePRText(text)
	if t == "" {
		c.addNotice(v.ID, "warning", "The PR text model didn't give a title, so it uses the thread's summary.")
		return title, body
	}
	if s.SkipPRDescription {
		d = ""
	} else if d == "" {
		d = body
	}
	return t, d
}

// Rules for pull request text when Settings has none.
const (
	defaultPRTitleRules       = "A short, specific title in the imperative mood."
	defaultPRDescriptionRules = "A brief description: what changed and why, then a short list of the main changes."
)

// prRules is the rules part of the PR text model's prompt: Settings' rules for the title and, unless
// pull requests get a title only, for the description, or the defaults where it has none. The same
// rules for both (as moved from the single setting there was before) are given once.
func prRules(s Settings) string {
	title := cmp.Or(strings.TrimSpace(s.PRTitleRules), defaultPRTitleRules)
	if s.SkipPRDescription {
		return "Rules for the title:\n" + title
	}
	desc := cmp.Or(strings.TrimSpace(s.PRDescriptionRules), defaultPRDescriptionRules)
	if title == desc {
		return "Rules for the title and description:\n" + title
	}
	return "Rules for the title:\n" + title + "\n\nRules for the description:\n" + desc
}

// parsePRText reads "TITLE: …" and "DESCRIPTION:" from the PR text model's answer.
func parsePRText(s string) (title, desc string) {
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "`"))
	head, rest, _ := strings.Cut(s, "\n")
	t, ok := strings.CutPrefix(strings.TrimSpace(head), "TITLE:")
	if !ok {
		return "", ""
	}
	title = strings.Trim(strings.TrimSpace(t), "\"'`")
	if len(title) > 200 {
		title = title[:200]
	}
	rest = strings.TrimSpace(rest)
	if d, ok := strings.CutPrefix(rest, "DESCRIPTION:"); ok {
		desc = strings.TrimSpace(d)
	}
	return title, desc
}

// mergePR merges one repository's open pull request through the forge.
func (c *Core) mergePR(ctx context.Context, v ThreadView, p PartView) error {
	client, err := forge.New(p.PR.Kind)
	if err != nil {
		return err
	}
	if err := client.Merge(ctx, p.Worktree, *p.PR, c.Settings().MergeMethod); err != nil {
		c.addNotice(v.ID, "error", fmt.Sprintf("Merging %s #%d failed: %v", p.PR.Kind.Noun(), p.PR.Number, err))
		return err
	}
	c.checkPR(ctx, v.ID, p.Repo)
	return nil
}

// pushPR pushes new commits on one repository's branch to its open pull request. A pull request the
// thread opened from another branch is pushed to only while its worktree is on that branch.
func (c *Core) pushPR(ctx context.Context, v ThreadView, p PartView) {
	head := p.Branch
	if p.PR.Head != "" && p.PR.Head != p.Branch {
		if git.CurrentBranch(ctx, p.Worktree) != p.PR.Head {
			return
		}
		head = p.PR.Head
	}
	ahead, err := git.Run(ctx, p.Worktree, "rev-list", "--count", head+"@{upstream}.."+head)
	if err == nil && strings.TrimSpace(ahead) == "0" {
		return
	}
	if err := git.Push(ctx, p.Worktree, head); err != nil {
		c.addNotice(v.ID, "error", fmt.Sprintf("Couldn't push to %s #%d: %v", p.PR.Kind.Noun(), p.PR.Number, err))
		return
	}
	c.addNotice(v.ID, "info", fmt.Sprintf("Pushed new commits to %s #%d%s.", p.PR.Kind.Noun(), p.PR.Number, inRepo(v, p)))
	c.pokePRs()
}

// pokePRs asks the watcher to check pull requests now.
func (c *Core) pokePRs() {
	select {
	case c.pollNow <- struct{}{}:
	default:
	}
}

// settledPRPoll is how many times PRPoll the watcher waits while every open pull request is settled.
const settledPRPoll = 4

// watchPRs follows open pull requests until Close: every PRPoll while one has checks running or
// merges on its own, and settledPRPoll times as long while all are settled, since each check runs gh
// or glab once per pull request.
func (c *Core) watchPRs() {
	every := c.cfg.PRPoll
	if every <= 0 {
		every = 30 * time.Second
	}
	timer := time.NewTimer(every)
	defer timer.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-timer.C:
		case <-c.pollNow:
		}
		c.CheckPRs(context.Background())
		c.sweepLanded()
		next := every
		if c.prsSettled() {
			next *= settledPRPoll
		}
		timer.Reset(next)
	}
}

// prsSettled reports whether no open pull request has checks running, unknown checks, or auto-merge
// on: nothing about them is about to change on its own.
func (c *Core) prsSettled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.threads {
		for _, p := range t.parts {
			if p.prOpen() && (p.PR.AutoMerge || (p.PR.Checks != forge.ChecksPassing && p.PR.Checks != forge.ChecksFailing && p.PR.Checks != forge.ChecksNone)) {
				return false
			}
		}
	}
	return true
}

// CheckPRs reads the state of every open pull request.
func (c *Core) CheckPRs(ctx context.Context) {
	type key struct{ id, repo string }
	c.mu.Lock()
	var todo []key
	for id, t := range c.threads {
		for _, p := range t.parts {
			if p.prOpen() {
				todo = append(todo, key{id, p.Repo})
			}
		}
	}
	c.mu.Unlock()
	for _, k := range todo {
		c.checkPR(ctx, k.id, k.repo)
	}
}

func (c *Core) checkPR(ctx context.Context, id, repo string) {
	v, ok := c.Thread(id)
	if !ok {
		return
	}
	for _, p := range v.Parts {
		if p.Repo != repo || p.PR == nil {
			continue
		}
		client, err := forge.New(p.PR.Kind)
		if err != nil {
			return
		}
		dir := p.Worktree
		if dir == "" {
			dir = p.Repo
		}
		vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		pr, err := client.View(vctx, dir, *p.PR)
		cancel()
		if err != nil {
			c.log.Warn("core: reading a pull request", "url", p.PR.URL, "err", err)
			return
		}
		c.applyPR(id, repo, pr, "")
		return
	}
}

// applyPR records a repository's pull request's new state and acts on what changed: it says so in the
// thread, tells Solo (through Change.PREvent), and closes the thread once every repository's pull
// request merged.
func (c *Core) applyPR(id, repo string, pr forge.PR, event string) {
	c.mu.Lock()
	t := c.threads[id]
	var p *part
	if t != nil {
		p = t.part(repo)
	}
	if p == nil {
		c.mu.Unlock()
		return
	}
	var old forge.PR
	if p.PR != nil {
		old = *p.PR
	}
	same := prSame(old, pr)
	if event == "" && same && old.Counts == pr.Counts {
		c.mu.Unlock()
		return
	}
	p.PR = &pr
	if event != "" || !same { // checks ticking over one by one is progress, not news
		t.row.UpdatedAt = now()
	}
	_ = c.saveLocked(t)
	status := t.row.Status
	multi := len(t.parts) > 1
	name := filepath.Base(p.Repo)
	c.mu.Unlock()

	noun := fmt.Sprintf("%s #%d", pr.Kind.Noun(), pr.Number)
	if multi {
		noun += " (" + name + ")"
	}
	if event == "" {
		switch {
		case pr.State == forge.StateMerged && old.State != forge.StateMerged:
			event = PRMerged
			c.addNotice(id, "info", noun+" merged.")
		case pr.State == forge.StateClosed && old.State != forge.StateClosed:
			event = PRClosed
			c.addNotice(id, "warning", noun+" was closed without merging.")
		case pr.Checks == forge.ChecksFailing && old.Checks != forge.ChecksFailing:
			event = PRChecksFailed
			c.addNotice(id, "error", "Checks failed on "+noun+": "+checkNames(pr.Failed)+".")
		case pr.Checks == forge.ChecksPassing && old.Checks != forge.ChecksPassing:
			event = PRChecksPassed
			c.addNotice(id, "info", "Checks passed on "+noun+".")
		}
	}
	c.proposalChecks(id, event)
	c.notify(Change{Kind: ChangeThread, ThreadID: id, PrevStatus: status, Status: status, PREvent: event, Repo: repo})
	if event == PRMerged {
		c.finishIfLanded(id)
	}
}

func prSame(a, b forge.PR) bool {
	return a.URL == b.URL && a.State == b.State && a.Checks == b.Checks && a.AutoMerge == b.AutoMerge &&
		a.Mergeable == b.Mergeable && a.Blocked == b.Blocked && checkNames(a.Failed) == checkNames(b.Failed)
}

func checkNames(cs []forge.Check) string {
	var names []string
	for _, c := range cs {
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		return "see the checks"
	}
	return strings.Join(names, ", ")
}
