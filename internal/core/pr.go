package core

import (
	"context"
	"encoding/json"
	"fmt"
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

// OpenPR pushes a worker's branch and opens a pull request (or merge request) against the branch it
// started from. With AutoMerge on, it also turns on the forge's auto-merge.
func (c *Core) OpenPR(ctx context.Context, threadID string) (forge.PR, error) {
	v, ok := c.Thread(threadID)
	if !ok {
		return forge.PR{}, fmt.Errorf("no thread %s", threadID)
	}
	if !v.CanOpenPR {
		return forge.PR{}, fmt.Errorf("thread %q has nothing to open a pull request for", v.Title)
	}
	client, err := forge.New(v.Forge)
	if err != nil {
		return forge.PR{}, err
	}
	if _, err := git.CommitAll(ctx, v.WorktreePath, uncommittedPrefix+v.Title); err != nil {
		return forge.PR{}, fmt.Errorf("committing the worktree's changes: %w", err)
	}
	if err := git.Push(ctx, v.WorktreePath, v.Branch); err != nil {
		return forge.PR{}, fmt.Errorf("pushing %s: %w", v.Branch, err)
	}
	title, body := c.prText(ctx, v)
	pr, err := client.Open(ctx, v.WorktreePath, v.BaseBranch, v.Branch, title, body)
	if err != nil {
		c.addNotice(threadID, "error", fmt.Sprintf("Couldn't open the %s: %v", v.Forge.Noun(), err))
		return forge.PR{}, err
	}
	noun := v.Forge.Noun()
	c.addNotice(threadID, "info", fmt.Sprintf("Opened %s #%d against %s.", noun, pr.Number, v.BaseBranch))
	if s := c.Settings(); s.AutoMerge && !pr.AutoMerge {
		if err := client.EnableAutoMerge(ctx, v.WorktreePath, pr, s.MergeMethod); err != nil {
			c.addNotice(threadID, "warning", fmt.Sprintf("Couldn't turn on auto-merge (%v). Merge it when its checks pass.", err))
		} else {
			pr.AutoMerge = true
			c.addNotice(threadID, "info", "Auto-merge is on: it merges once its required checks pass.")
		}
	}
	c.applyPR(threadID, pr, PROpened)
	c.pokePRs()
	return pr, nil
}

// prText is a pull request's title and description: the one commit's subject (or the thread's title)
// and the thread's summary.
func (c *Core) prText(ctx context.Context, v ThreadView) (string, string) {
	title := v.Title
	if log, err := git.Run(ctx, v.WorktreePath, "log", "--format=%s", v.BaseCommit+"..HEAD"); err == nil {
		var subjects []string // the worker's own commits, not Solo's commit of what it left uncommitted
		for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
			if l != "" && !strings.HasPrefix(l, uncommittedPrefix) {
				subjects = append(subjects, l)
			}
		}
		if len(subjects) == 1 {
			title = subjects[0]
		}
	}
	body := strings.TrimSpace(v.Summary)
	if body == "" {
		body = clipText(strings.TrimSpace(v.Prompt), 2000)
	}
	return title, body
}

// mergePR merges an open pull request through the forge.
func (c *Core) mergePR(ctx context.Context, v ThreadView) error {
	client, err := forge.New(v.PR.Kind)
	if err != nil {
		return err
	}
	if err := client.Merge(ctx, v.WorktreePath, *v.PR, c.Settings().MergeMethod); err != nil {
		c.addNotice(v.ID, "error", fmt.Sprintf("Merging %s #%d failed: %v", v.PR.Kind.Noun(), v.PR.Number, err))
		return err
	}
	c.checkPR(ctx, v.ID)
	return nil
}

// pushPR pushes new commits on a thread's branch to its open pull request.
func (c *Core) pushPR(ctx context.Context, v ThreadView) {
	ahead, err := git.Run(ctx, v.WorktreePath, "rev-list", "--count", "@{upstream}..HEAD")
	if err == nil && strings.TrimSpace(ahead) == "0" {
		return
	}
	if err := git.Push(ctx, v.WorktreePath, v.Branch); err != nil {
		c.addNotice(v.ID, "error", fmt.Sprintf("Couldn't push to the %s: %v", v.PR.Kind.Noun(), err))
		return
	}
	c.addNotice(v.ID, "info", fmt.Sprintf("Pushed new commits to %s #%d.", v.PR.Kind.Noun(), v.PR.Number))
	c.pokePRs()
}

// pokePRs asks the watcher to check pull requests now.
func (c *Core) pokePRs() {
	select {
	case c.pollNow <- struct{}{}:
	default:
	}
}

// watchPRs follows open pull requests until Close.
func (c *Core) watchPRs() {
	every := c.cfg.PRPoll
	if every <= 0 {
		every = 30 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-tick.C:
		case <-c.pollNow:
		}
		c.CheckPRs(context.Background())
	}
}

// CheckPRs reads the state of every open pull request.
func (c *Core) CheckPRs(ctx context.Context) {
	c.mu.Lock()
	var ids []string
	for id, t := range c.threads {
		if t.pr != nil && t.pr.State == forge.StateOpen {
			ids = append(ids, id)
		}
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.checkPR(ctx, id)
	}
}

func (c *Core) checkPR(ctx context.Context, id string) {
	v, ok := c.Thread(id)
	if !ok || v.PR == nil {
		return
	}
	client, err := forge.New(v.PR.Kind)
	if err != nil {
		return
	}
	dir := v.WorktreePath
	if dir == "" {
		dir = v.RepoPath
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pr, err := client.View(vctx, dir, *v.PR)
	if err != nil {
		c.log.Warn("core: reading a pull request", "url", v.PR.URL, "err", err)
		return
	}
	c.applyPR(id, pr, "")
}

// applyPR records a pull request's new state and acts on what changed: it says so in the thread,
// tells Solo (through Change.PREvent), and finishes the thread when the pull request merged.
func (c *Core) applyPR(id string, pr forge.PR, event string) {
	c.mu.Lock()
	t := c.threads[id]
	if t == nil {
		c.mu.Unlock()
		return
	}
	var old forge.PR
	if t.pr != nil {
		old = *t.pr
	}
	if event == "" && prSame(old, pr) {
		c.mu.Unlock()
		return
	}
	t.pr = &pr
	data, _ := json.Marshal(pr)
	t.row.PR = string(data)
	t.row.UpdatedAt = now()
	_ = c.db.SaveThread(&t.row)
	status := t.row.Status
	c.mu.Unlock()

	noun := fmt.Sprintf("%s #%d", pr.Kind.Noun(), pr.Number)
	if event == "" {
		switch {
		case pr.State == forge.StateMerged && old.State != forge.StateMerged:
			event = PRMerged
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
	if event == PRMerged {
		c.finishMerged(id, pr)
		return
	}
	c.notify(Change{Kind: ChangeThread, ThreadID: id, PrevStatus: status, Status: status, PREvent: event})
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

// finishMerged closes a thread whose pull request merged on the forge: it removes the worktree and
// the local branch.
func (c *Core) finishMerged(id string, pr forge.PR) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	v, ok := c.Thread(id)
	if !ok || Finished(v.Status) {
		return
	}
	c.closeSession(ctx, id)
	if v.WorktreePath != "" {
		if err := git.RemoveWorktree(ctx, v.RepoPath, v.WorktreePath, v.Branch, true); err != nil {
			c.log.Warn("core: removing a merged worktree", "err", err)
		}
	}
	c.mu.Lock()
	var ch Change
	if t := c.threads[id]; t != nil {
		t.row.WorktreePath = ""
		ch = c.setStatusLocked(t, StatusMerged, "")
		ch.PREvent = PRMerged
	}
	c.mu.Unlock()
	c.addNotice(id, "info", fmt.Sprintf("%s #%d merged into %s.", pr.Kind.Noun(), pr.Number, v.BaseBranch))
	c.notify(ch)
}
