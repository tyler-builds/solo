//go:build !js

package core

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/agent"

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
	settleChecks(&pr, p.Worktree) // as applyPR records it
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
// rules and skills; without a description when Settings says so. If the model fails, the title is the
// worker's one commit's subject (or the thread's title) and the description its summary.
func (c *Core) prText(ctx context.Context, v ThreadView, p PartView) (string, string) {
	s := c.Settings()
	pp := prPrompt(ctx, v, p, s)
	if len(pp.missing) > 0 {
		c.addNotice(v.ID, "warning", fmt.Sprintf("The PR text skips %s%s, which Solo couldn't find. Settings › Pull requests lists the skills there are.",
			skillList(pp.missing), inRepo(v, p)))
	}
	title := v.Title
	if len(pp.subjects) == 1 {
		title = pp.subjects[0]
	}
	body := strings.TrimSpace(v.Summary)
	if body == "" {
		body = clipText(strings.TrimSpace(v.Prompt), 2000)
	}
	if s.SkipPRDescription {
		body = ""
	}
	pctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	text, err := c.agent.Complete(pctx, agent.CompleteParams{Model: s.PRModel, Effort: s.PREffort, System: pp.system,
		Prompt: pp.prompt, ClaudePath: agent.FindClaude(s.ClaudePath).Path, TimeoutMs: 70_000})
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

// prRequest is what the PR text model is asked for one repository's part of a thread.
type prRequest struct {
	system, prompt string
	subjects       []string // the worker's own commits' subjects
	missing        []string // skills Settings names that weren't found
}

// prPrompt builds the PR text model's request. The model runs without tools, so the chosen skills'
// instructions go in the prompt, read from the branch's worktree for a project's skills.
func prPrompt(ctx context.Context, v ThreadView, p PartView, s Settings) prRequest {
	var r prRequest
	// The worker's own commits, not Solo's commit of what it left uncommitted.
	if log, err := git.Run(ctx, p.Worktree, "log", "--format=%s", p.BaseCommit+"..HEAD"); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
			if l != "" && !strings.HasPrefix(l, uncommittedPrefix) {
				r.subjects = append(r.subjects, l)
			}
		}
	}
	rules := prRules(s)
	skills, missing := skillRules(cmp.Or(p.Worktree, p.Repo), s.PRSkills)
	r.missing = missing
	rules += prSkillRules(skills)
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
	r.prompt = fmt.Sprintf("Write the pull request for this change.\n\n%s\n\nRepository: %s%s\nBranch: %s into %s\nTask title: %s\nTask:\n%s\n\nWhat the agent that made the change reported:\n%s\n\nCommits:\n%s\n\nFiles changed:\n%s",
		rules, p.Name, scope, p.Branch, p.BaseBranch, v.Title, clipText(v.Prompt, 1500),
		clipText(strings.TrimSpace(v.Summary), 3000), strings.Join(r.subjects, "\n"), strings.Join(files, "\n"))
	r.system = prSystem
	if s.SkipPRDescription {
		r.system = prSystemTitle
	}
	return r
}

// prSkillRules is the part of the PR text model's prompt that gives the chosen skills' instructions,
// or "" with none.
func prSkillRules(skills []namedText) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAlso follow these skills' instructions where they say how to write the pull request. You can't run tools or open\nfiles, so leave out any steps that need them.")
	for _, sk := range skills {
		fmt.Fprintf(&b, "\n\n<skill name=%q>\n%s\n</skill>", sk.name, sk.text)
	}
	return b.String()
}

// skillList names skills for a notice: "the skill /a", "the skills /a and /b", "the skills /a, /b and /c".
func skillList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = "/" + n
	}
	if len(q) == 1 {
		return "the skill " + q[0]
	}
	return "the skills " + strings.Join(q[:len(q)-1], ", ") + " and " + q[len(q)-1]
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
// thread opened from another branch is pushed to only while its worktree is on that branch; its base
// branch's pull request (basePR) gets the commits made since on top of that branch.
func (c *Core) pushPR(ctx context.Context, v ThreadView, p PartView) {
	head := p.Branch
	if p.PR.Head != "" && p.PR.Head == p.BaseBranch && p.PR.Head != p.Branch {
		if git.InRemoteBranch(ctx, p.Worktree, p.BaseBranch) {
			return
		}
		if err := git.PushTo(ctx, p.Worktree, p.BaseBranch); err != nil {
			c.addNotice(v.ID, "error", fmt.Sprintf("Couldn't push to %s #%d: %v", p.PR.Kind.Noun(), p.PR.Number, err))
			return
		}
		c.restartChecks(v.ID, p.Repo)
		c.addNotice(v.ID, "info", fmt.Sprintf("Pushed new commits to %s #%d%s.", p.PR.Kind.Noun(), p.PR.Number, inRepo(v, p)))
		c.pokePRs()
		return
	}
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
	c.restartChecks(v.ID, p.Repo)
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

// freshPRPoll is the longest the watcher waits while a pull request's checks may still be on their
// way (checksFresh), so they show up within seconds of the forge queueing them.
const freshPRPoll = 10 * time.Second

// checksGrace is how long after a pull request's head changed (it opened, or new commits were pushed)
// a repository with CI set up may take to report checks on it. Until then no checks reported means
// they aren't queued yet (ChecksWaiting); after, that none run (ChecksNone): its workflows don't run
// on pull requests, say.
const checksGrace = 2 * time.Minute

// watchPRs follows open pull requests until Close: every PRPoll while one has checks running or
// merges on its own, settledPRPoll times as long while all are settled, since each check runs gh or
// glab once per pull request, and at most every freshPRPoll while one's checks may still show up.
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
		timer.Reset(c.nextPRPoll(every))
	}
}

// nextPRPoll is how long the watcher waits before checking pull requests again, given PRPoll: at most
// freshPRPoll while an open pull request has no checks reported soon after its head changed, every
// while one has checks running, unknown checks or auto-merge on, and settledPRPoll times every while
// nothing about them is about to change on its own.
func (c *Core) nextPRPoll(every time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	settled := true
	for _, t := range c.threads {
		for _, p := range t.parts {
			if !p.prOpen() {
				continue
			}
			switch p.PR.Checks {
			case forge.ChecksWaiting, forge.ChecksNone:
				if checksFresh(*p.PR) {
					return min(every, freshPRPoll)
				}
			}
			if p.PR.AutoMerge || (p.PR.Checks != forge.ChecksPassing && p.PR.Checks != forge.ChecksFailing && p.PR.Checks != forge.ChecksNone) {
				settled = false
			}
		}
	}
	if settled {
		return every * settledPRPoll
	}
	return every
}

// checksFresh reports whether a pull request's head changed less than checksGrace ago, so the forge
// may not have reported its checks yet.
func checksFresh(pr forge.PR) bool {
	return !pr.ChecksSince.IsZero() && now().Sub(pr.ChecksSince) < checksGrace
}

// settleChecks decides what a pull request with no checks reported (ChecksWaiting) waits for: while
// its checks may still be queued (checksFresh, in a repository with CI set up in dir) it keeps
// waiting, and can't merge until they report; otherwise it has none (ChecksNone).
func settleChecks(pr *forge.PR, dir string) {
	if pr.Checks != forge.ChecksWaiting {
		return
	}
	if pr.State == forge.StateOpen && checksFresh(*pr) && hasCI(dir, pr.Kind) {
		pr.Mergeable = false
		if pr.Blocked == "" {
			pr.Blocked = "waiting for checks"
		}
		return
	}
	pr.Checks = forge.ChecksNone
}

// hasCI reports whether the checkout dir sets up CI on the forge: a GitHub Actions workflow, or a
// GitLab pipeline.
func hasCI(dir string, kind forge.Kind) bool {
	if dir == "" {
		return false
	}
	if kind == forge.GitLab {
		_, err := os.Stat(filepath.Join(dir, ".gitlab-ci.yml"))
		return err == nil
	}
	entries, err := os.ReadDir(filepath.Join(dir, ".github", "workflows"))
	if err != nil {
		return false
	}
	return slices.ContainsFunc(entries, func(e os.DirEntry) bool {
		ext := filepath.Ext(e.Name())
		return !e.IsDir() && (ext == ".yml" || ext == ".yaml")
	})
}

// restartChecks records that new commits reached a repository's open pull request: its checks start
// over, and may take a while to show up.
func (c *Core) restartChecks(id, repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.threads[id]
	if t == nil {
		return
	}
	p := t.part(repo)
	if p == nil || !p.prOpen() {
		return
	}
	pr := *p.PR // a new value: views share the old one
	pr.ChecksSince = now()
	p.PR = &pr
	_ = c.saveLocked(t)
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
	dir := p.Worktree
	if dir == "" {
		dir = p.Repo
	}
	settleChecks(&pr, dir)
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
