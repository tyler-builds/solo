//go:build !js

package core

import (
	"cmp"
	"context"
	"errors"
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

// prAgentSystem is what the PR text agent is told, after Claude Code's own instructions; %s is the
// answer's format.
const prAgentSystem = `You are writing the text of a pull request for Solo, for a change already committed on this
worktree's branch. Solo pushes the branch and opens the pull request with your text; you only write it.
- Use the skills the user names, fully: load each with the Skill tool and follow its instructions, running
  the scripts and commands it gives. You may read files and run read-only commands (git log, git diff, …).
- Leave the worktree as you found it: don't commit, push, switch or reset branches, or create, change or
  delete files in it. Put any scratch files in the system's temporary folder.
- Don't open, edit or comment on pull requests (no gh pr create, glab mr create, …).
- When you are done, end your reply with the answer in this format, with nothing after it:
%s`

const (
	prAnswerFormat      = "TITLE: <one-line title>\nDESCRIPTION:\n<the description, in Markdown>"
	prAnswerFormatTitle = "TITLE: <one-line title>"
)

// prAgentTools are the built-in tools the PR text agent has: what a skill needs to load its files and
// run its scripts. Claude Code runs Bash with a POSIX shell, Git Bash on Windows, never PowerShell.
var prAgentTools = []string{"Skill", "Bash", "Read", "Glob", "Grep"}

// prAgentTimeout is how long the PR text agent may take: skills can run scripts and read the change.
const prAgentTimeout = 5 * time.Minute

// prText writes a pull request's title and description following Settings' rules and skills; without
// a description when Settings says so. With skills it runs a Claude Code agent in the branch's worktree,
// which loads them as Claude Code does and can run their scripts; without, it asks the PR text model
// once, with no tools. If either fails, the title is the worker's one commit's subject (or the thread's
// title) and the description its summary.
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
	var text string
	var err error
	if len(pp.skills) > 0 {
		text, err = c.prAgentText(ctx, v, p, s, pp)
	} else {
		pctx, cancel := context.WithTimeout(ctx, 75*time.Second)
		text, err = c.agent.Complete(pctx, agent.CompleteParams{Model: s.PRModel, Effort: s.PREffort, System: pp.system,
			Prompt: pp.prompt, ClaudePath: agent.FindClaude(s.ClaudePath).Path, TimeoutMs: 70_000})
		cancel()
	}
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

// prAgentText runs the PR text agent: a one-off Claude Code session in the branch's worktree, with the
// chosen skills and the tools they need. It must leave the worktree as it was; if it doesn't, the
// worktree is put back.
func (c *Core) prAgentText(ctx context.Context, v ThreadView, p PartView, s Settings, pp prRequest) (string, error) {
	names := make([]string, len(pp.skills))
	for i, sk := range pp.skills {
		names[i] = sk.name
	}
	c.addNotice(v.ID, "info", fmt.Sprintf("Writing the PR text%s with %s. This can take a few minutes.", inRepo(v, p), skillList(names)))
	before := readTree(ctx, p.Worktree)
	text, err := c.runJob(ctx, agent.OpenParams{
		Cwd:            p.Worktree,
		Model:          s.PRModel,
		Effort:         s.PREffort,
		SystemAppend:   pp.system,
		PermissionMode: agent.PermissionBypass,
		Tools:          prAgentTools,
		Skills:         names,
		ToolPolicy:     agent.ToolPolicyAllow,
		ClaudePath:     agent.FindClaude(s.ClaudePath).Path,
		Ephemeral:      true,
	}, pp.prompt, prAgentTimeout)
	c.keepTree(v, p, before)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		err = fmt.Errorf("it took longer than %s", prAgentTimeout)
	}
	return text, err
}

// runJob sends prompt to a new one-off session and returns its answer, closing the session after. It
// gives up after timeout.
func (c *Core) runJob(ctx context.Context, op agent.OpenParams, prompt string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	op.SessionID = agent.NewUUID()
	ended := make(chan agent.Event, 4)
	c.mu.Lock()
	c.jobs[op.SessionID] = ended
	c.mu.Unlock()
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := c.agent.Close(cctx, op.SessionID); err != nil {
			c.log.Warn("core: closing a one-off session", "err", err)
		}
		cancel()
		c.mu.Lock()
		delete(c.jobs, op.SessionID)
		c.mu.Unlock()
	}()
	if err := c.agent.Open(ctx, op); err != nil {
		return "", err
	}
	if _, err := c.agent.Send(ctx, op.SessionID, prompt, false); err != nil {
		return "", err
	}
	for {
		select {
		case e := <-ended:
			switch {
			case e.Kind == agent.EventExited:
				return "", errors.New(cmp.Or(e.Error, "Claude stopped before it answered"))
			case e.Steered:
			case e.Status != agent.TurnCompleted:
				return "", errors.New(cmp.Or(e.Error, "Claude's turn ended "+e.Status))
			default:
				return e.Result, nil
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// treeState is what a worktree has checked out, and its uncommitted changes.
type treeState struct {
	ok                   bool
	branch, head, status string
}

func readTree(ctx context.Context, dir string) treeState {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	branch, _ := git.Run(ctx, dir, "symbolic-ref", "-q", "HEAD") // "" when detached
	head, err := git.Run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return treeState{}
	}
	status, err := git.Run(ctx, dir, "status", "--porcelain", "-uall")
	if err != nil {
		return treeState{}
	}
	return treeState{true, strings.TrimSpace(branch), strings.TrimSpace(head), status}
}

// keepTree puts a worktree back as it was before the PR text agent ran (before), if the agent changed
// it, and says so.
func (c *Core) keepTree(v ThreadView, p PartView, before treeState) {
	if !before.ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	after := readTree(ctx, p.Worktree)
	if !after.ok || after == before {
		return
	}
	if before.status != "" {
		c.addNotice(v.ID, "warning", fmt.Sprintf("The PR text agent changed the worktree%s, and Solo couldn't put it back on its own because it had uncommitted changes. Check it before you go on.", inRepo(v, p)))
		return
	}
	var err error
	if before.branch != "" && after.branch != before.branch {
		_, err = git.Run(ctx, p.Worktree, "checkout", "-q", "-f", strings.TrimPrefix(before.branch, "refs/heads/"))
	}
	if err == nil {
		_, err = git.Run(ctx, p.Worktree, "reset", "-q", "--hard", before.head)
	}
	if err == nil {
		_, err = git.Run(ctx, p.Worktree, "clean", "-fdq") // not -x: ignored files (builds, dependencies) stay
	}
	if err != nil {
		c.addNotice(v.ID, "warning", fmt.Sprintf("The PR text agent changed the worktree%s, and Solo couldn't put it back (%v). Check it before you go on.", inRepo(v, p), err))
		return
	}
	c.addNotice(v.ID, "warning", fmt.Sprintf("The PR text agent changed the worktree%s, so Solo put it back as it was.", inRepo(v, p)))
}

// prRequest is what the PR text model (or agent) is asked for one repository's part of a thread.
type prRequest struct {
	system, prompt string
	subjects       []string    // the worker's own commits' subjects
	skills         []namedText // the chosen skills found, with their SKILL.md paths: the agent writes it
	missing        []string    // skills Settings names that weren't found
}

// prPrompt builds the PR text request. Without skills it is for the PR text model, which runs without
// tools; with skills, for the PR text agent, which loads them itself, from the branch's worktree for a
// project's skills.
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
	r.skills, r.missing = skillPaths(cmp.Or(p.Worktree, p.Repo), s.PRSkills)
	var rules string
	if len(r.skills) > 0 {
		rules = prAgentRules(s, r.skills)
	} else {
		rules = prRules(s)
	}
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
	switch {
	case len(r.skills) > 0 && s.SkipPRDescription:
		r.system = fmt.Sprintf(prAgentSystem, prAnswerFormatTitle)
	case len(r.skills) > 0:
		r.system = fmt.Sprintf(prAgentSystem, prAnswerFormat)
	case s.SkipPRDescription:
		r.system = prSystemTitle
	default:
		r.system = prSystem
	}
	return r
}

// prAgentRules is the rules part of the PR text agent's prompt: the skills to use and Settings' own
// rules. Where Settings has none, the skills alone say how to write it; the default rules would
// override their format.
func prAgentRules(s Settings, skills []namedText) string {
	var b strings.Builder
	names := make([]string, len(skills))
	for i, sk := range skills {
		names[i] = sk.name
	}
	fmt.Fprintf(&b, "Write it with %s: load each with the Skill tool and follow its instructions fully, including running the scripts and commands it gives. ", skillList(names))
	b.WriteString("If the Skill tool can't load one, read its SKILL.md instead; paths in a skill are relative to its folder:")
	for _, sk := range skills {
		fmt.Fprintf(&b, "\n- /%s: %s", sk.name, sk.text)
	}
	title := strings.TrimSpace(s.PRTitleRules)
	desc := strings.TrimSpace(s.PRDescriptionRules)
	if s.SkipPRDescription {
		desc = ""
		b.WriteString("\n\nWrite a title only, with no description.")
	}
	switch {
	case title != "" && title == desc:
		b.WriteString("\n\nAlso follow these rules for the title and description:\n" + title)
	default:
		if title != "" {
			b.WriteString("\n\nAlso follow these rules for the title:\n" + title)
		}
		if desc != "" {
			b.WriteString("\n\nAlso follow these rules for the description:\n" + desc)
		}
	}
	if title == "" {
		b.WriteString("\n\nIf the skills don't say how to write the title, make it " + strings.ToLower(defaultPRTitleRules[:1]) + defaultPRTitleRules[1:])
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

// parsePRText reads "TITLE: …" and "DESCRIPTION:" from the PR text model's answer. An agent may say
// what it did first, so the answer starts at the last line that begins with "TITLE:"; a code fence
// around the answer is dropped.
func parsePRText(s string) (title, desc string) {
	s = strings.TrimSpace(s)
	i := strings.LastIndex("\n"+s, "\nTITLE:")
	if i < 0 {
		return "", ""
	}
	lead := strings.TrimSpace(s[:i])
	s = strings.TrimSpace(s[i:])
	if strings.HasPrefix(lead[strings.LastIndex(lead, "\n")+1:], "```") {
		s = strings.TrimSpace(strings.TrimSuffix(s, "```"))
	}
	head, rest, _ := strings.Cut(s, "\n")
	t, _ := strings.CutPrefix(strings.TrimSpace(head), "TITLE:")
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
