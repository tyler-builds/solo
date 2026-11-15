//go:build !js

package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tyler-builds/solo/internal/forge"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/store"
)

// thread is a thread's state. Guarded by Core.mu, except sess.
type thread struct {
	// sess serializes the thread's use of its Claude session: opening and sending to it, and closing or
	// replacing it. Take it before c.mu, never while holding c.mu.
	sess    sync.Mutex
	row     store.Thread
	opened  bool // its session is registered with the agent manager
	inTurn  bool
	live    []liveKey // streaming blocks, in order
	liveTxt map[liveKey]*LiveBlock
	model   string // the model Claude reports
	// step is the tool call in progress, and turnStart when the current turn began.
	step      *ToolData
	turnStart time.Time
	// thought is the running turn's newest thinking, streaming or finished.
	thought string
	retry   string // the latest API retry, while it lasts
	asks    map[string]chan agent.AskResult
	askQs   map[string][]agent.Question
	// staleAsks are questions asked before Solo last closed (tool use id → item id): nothing waits on
	// them any more, so an answer goes to the thread as a message instead.
	staleAsks map[string]int64
	// recovered is when the thread last moved to a new session after losing its conversation.
	recovered time.Time
	tasks     map[string]int64 // task id → item id
	tools     map[string]int64 // tool_use id → item id
	// bg is the work Claude runs in the background, oldest first (agent.EventBackground).
	bg []BackgroundTask
	// bgEnd is how each background task ended (completed, failed, stopped), until it leaves bg.
	bgEnd map[string]string
	// bgWake: background work finished by itself between turns, so Claude is about to start a turn to
	// read its result; the thread stays in the background state until then, or bgWakeWait passes.
	// bgWakeGen tells a stale wait's timer from the current one's.
	bgWake    bool
	bgWakeGen int
	// bgStopped: the user stopped the background work, so the thread settles as stopped, not ready.
	bgStopped bool
	// parts are the repositories it works in; the first is the one most of the app shows.
	parts []*part
	// reopen asks for the session to restart at the end of the turn, to take a new effort level.
	reopen bool
	// resolving is set while the worker resolves merge conflicts it was handed; its branch merges when
	// the turn ends. resolveTries counts the hand-offs, so a merge that keeps conflicting stops.
	resolving    bool
	resolveTries int
	// prHints are pull request addresses the thread printed opening them itself (gh pr create), not yet
	// linked to a part; prScanned is set once its saved transcript was searched for them, after a load.
	prHints   []string
	prScanned bool
	// repost: new commits reached its open pull request since its merge card was posted, so the card
	// is posted anew at the end of Solo's conversation (repostCards).
	repost bool
}

// DescribeBackground names background work in a line: `command "Run benchmarks", running 4m`; several
// are joined with "; ".
func DescribeBackground(bg []BackgroundTask) string {
	var parts []string
	for _, b := range bg {
		s := b.Kind()
		if b.Description != "" {
			s += fmt.Sprintf(" %q", b.Description)
		}
		if !b.Started.IsZero() {
			s += ", running " + roughDuration(time.Since(b.Started))
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

// roughDuration is a duration to the second under a minute, else to the minute: "40s", "4m", "1h12m".
// A negative one, which a right clock never gives, is "0s".
func roughDuration(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

type liveKey struct {
	message string
	index   int
}

func (t *thread) view() ThreadView {
	v := ThreadView{
		Thread:      t.row,
		InTurn:      t.inTurn,
		TurnStarted: t.turnStart,
		Step:        t.step,
		Thought:     t.thought,
		ActualModel: t.model,
		Retry:       t.retry,
		PendingAsk:  len(t.asks)+len(t.staleAsks) > 0,
		Resolving:   t.resolving,
		IsWorker:    t.row.Kind == store.KindWorker,
	}
	for _, k := range t.live {
		v.Live = append(v.Live, *t.liveTxt[k])
	}
	v.Background = slices.Clone(t.bg)
	v.CanInterrupt = t.inTurn || len(t.bg) > 0
	for _, p := range t.parts {
		v.HasWorktree = v.HasWorktree || p.Worktree != ""
	}
	settled := v.IsWorker && v.HasWorktree && !Finished(t.row.Status) && !Active(t.row.Status) && t.row.Status != StatusQueued
	allLanded := len(t.parts) > 0 // every part with changes has a merged pull request
	for _, p := range t.parts {
		pv := PartView{Part: p.Part, Name: filepath.Base(p.Repo), Changed: p.changed, Forge: p.forge}
		if p.PR != nil {
			pr := *p.PR
			pv.PR = &pr
		}
		prOpen := p.prOpen()
		hasWork := settled && p.changed != 0
		pv.CanMergeLocal = hasWork && !prOpen
		pv.CanOpenPR = hasWork && !prOpen && p.forge != forge.None
		pv.CanMarkReady = prOpen && p.PR.Draft && !t.inTurn && len(t.bg) == 0
		// Merge: the open pull request when it can merge now, or locally when there's no forge.
		pv.CanMerge = hasWork && ((prOpen && p.PR.Mergeable) || (!prOpen && p.forge == forge.None))
		v.CanMergeLocal = v.CanMergeLocal || pv.CanMergeLocal
		v.CanOpenPR = v.CanOpenPR || pv.CanOpenPR
		v.CanMarkReady = v.CanMarkReady || pv.CanMarkReady
		v.CanMerge = v.CanMerge || pv.CanMerge
		switch {
		case p.changed < 0 || v.Changed < 0:
			v.Changed = -1
		default:
			v.Changed += p.changed
		}
		if p.changed != 0 && (p.PR == nil || p.PR.State != forge.StateMerged) {
			allLanded = false
		}
		v.Parts = append(v.Parts, pv)
	}
	if len(t.parts) == 0 {
		v.Changed = -1
	}
	if len(t.parts) == 1 {
		v.Forge, v.PR = v.Parts[0].Forge, v.Parts[0].PR
	}
	v.NoChanges = settled && v.Changed == 0 && t.row.Status == StatusIdle
	v.CanDone = v.IsWorker && t.row.DoneAt.IsZero() && !t.inTurn && len(t.bg) == 0 && !Active(t.row.Status) && t.row.Status != StatusQueued
	landed := t.row.Status == StatusMerged || (settled && v.Changed == 0) || (allLanded && v.Changed > 0)
	v.Unlanded = v.CanDone && v.HasWorktree && !landed && !Finished(t.row.Status)
	if t.inPlace() {
		v.InPlace, v.Unlanded = true, false
	}
	v.CanDiscard = v.IsWorker && !Finished(t.row.Status)
	return v
}

// Threads returns every thread, oldest first.
func (c *Core) Threads() []ThreadView {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ThreadView, 0, len(c.threads))
	for _, t := range c.threads {
		out = append(out, c.viewLocked(t))
	}
	slices.SortFunc(out, func(a, b ThreadView) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// Thread returns one thread.
func (c *Core) Thread(id string) (ThreadView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.threads[id]
	if t == nil {
		return ThreadView{}, false
	}
	return c.viewLocked(t), true
}

// viewLocked is a thread's view with what only Core knows. Call with c.mu held.
func (c *Core) viewLocked(t *thread) ThreadView {
	v := t.view()
	if v.InPlace {
		v.Dir = v.Parts[0].Repo
	}
	if v.HasWorktree {
		v.Dir = v.Parts[0].Worktree
		if len(t.parts) > 1 {
			v.Dir = c.threadDir(t.row)
		}
	}
	return v
}

// Items returns up to limit of the thread's newest transcript items (all when limit <= 0).
func (c *Core) Items(ctx context.Context, threadID string, limit int) ([]Item, error) {
	rows, err := c.db.Items(ctx, threadID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Item, len(rows))
	for i, r := range rows {
		out[i] = Item{ID: r.ID, ThreadID: r.ThreadID, Kind: r.Kind, TurnUUID: r.TurnUUID, Ref: r.Ref, Data: r.Data, CreatedAt: r.CreatedAt}
	}
	return out, nil
}

// WorkerSpec describes a new worker thread.
type WorkerSpec struct {
	Repo   string   // path or name of a known repository
	Repos  []string // more repositories, for a task that spans them (Repo, if set, comes first)
	Title  string
	Prompt string
	Model  string   // empty: Settings.WorkerModel
	Effort string   // empty: Settings.WorkerEffort
	Base   string   // branch to start from; empty: the repository's current branch
	Images []string // image files to send with the prompt
}

// CreateWorker creates a worker thread and starts it.
func (c *Core) CreateWorker(ctx context.Context, spec WorkerSpec) (ThreadView, error) {
	var repos []string
	inPlace := "" // a folder outside git the thread works in directly
	for _, name := range append([]string{spec.Repo}, spec.Repos...) {
		if strings.TrimSpace(name) == "" {
			continue
		}
		repo, err := c.ResolveRepo(name)
		if err != nil {
			dir, ok := c.ResolvePlain(name)
			if !ok {
				return ThreadView{}, err
			}
			repo, inPlace = dir, dir
		}
		if !slices.Contains(repos, repo) {
			repos = append(repos, repo)
		}
	}
	if len(repos) == 0 {
		return ThreadView{}, errors.New("a worker needs a repository")
	}
	if inPlace != "" && len(repos) > 1 {
		return ThreadView{}, fmt.Errorf("%s isn't in git, so a thread can only work in it alone; put it in git (init_repo) to include it with other repositories", filepath.Base(inPlace))
	}
	if strings.TrimSpace(spec.Prompt) == "" {
		return ThreadView{}, errors.New("a worker needs a prompt")
	}
	images, err := c.ImportImages(spec.Images)
	if err != nil {
		return ThreadView{}, err
	}
	title := strings.TrimSpace(spec.Title)
	if title == "" {
		title = firstLine(spec.Prompt, 60)
	}
	id := agent.NewUUID()
	slug := git.Slug(title) + "-" + id[:6]
	ts := now()
	t := &thread{row: store.Thread{
		ID:           id,
		Kind:         store.KindWorker,
		SessionID:    agent.NewUUID(),
		Title:        title,
		Model:        spec.Model,
		Effort:       spec.Effort,
		Status:       StatusQueued,
		Prompt:       spec.Prompt,
		PromptImages: images,
		CreatedAt:    ts,
		UpdatedAt:    ts,
	}}
	for _, r := range repos {
		p := Part{Repo: r, Branch: "solo/" + slug, BaseBranch: spec.Base}
		if r == inPlace {
			p = Part{Repo: r, InPlace: true}
		}
		t.parts = append(t.parts, &part{Part: p, changed: -1})
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ThreadView{}, errClosed
	}
	if t.row.Model == "" {
		t.row.Model = c.settings.WorkerModel
	}
	if err := c.saveLocked(t); err != nil {
		c.mu.Unlock()
		return ThreadView{}, err
	}
	c.threads[id] = t
	c.bySession[t.row.SessionID] = t
	v := t.view()
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeThread, ThreadID: id, Status: StatusQueued})
	c.schedule()
	if v2, ok := c.Thread(id); ok {
		v = v2
	}
	return v, nil
}

// schedule starts every queued worker. There is no cap on how many run at once.
func (c *Core) schedule() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	var start []*thread
	var changes []Change
	for _, t := range c.threads {
		if t.row.Kind == store.KindWorker && t.row.Status == StatusQueued {
			changes = append(changes, c.setStatusLocked(t, StatusStarting, ""))
			start = append(start, t)
		}
	}
	c.mu.Unlock()
	c.notify(changes...)
	for _, t := range start {
		go c.startWorker(t.row.ID)
	}
}

// startWorker creates the worker's worktree, opens its session, and sends its prompt.
func (c *Core) startWorker(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fail := func(err error) {
		c.mu.Lock()
		t := c.threads[id]
		var ch Change
		if t != nil {
			ch = c.setStatusLocked(t, StatusFailed, err.Error())
		}
		c.mu.Unlock()
		c.addNotice(id, "error", "Could not start: "+err.Error())
		c.notify(ch)
		c.schedule()
	}

	c.mu.Lock()
	t := c.threads[id]
	if t == nil || t.row.Status != StatusStarting {
		c.mu.Unlock()
		return
	}
	row := t.row
	repos := t.repos()
	started := len(t.parts) > 0 && t.parts[0].Worktree != ""
	inPlace := t.inPlace()
	parts := make([]Part, len(t.parts))
	for i, p := range t.parts {
		parts[i] = p.Part
	}
	c.mu.Unlock()

	if inPlace && !row.HasTranscript {
		c.addNotice(id, "info", fmt.Sprintf("Working directly in %s. It isn't in git, so there's no separate copy, diff or merge.", tildeDir(repos[0])))
	}
	if !started && !inPlace {
		// One branch name for every repository, from the latest commit of each one's default branch (or
		// the one Solo named), fetched now.
		branch := c.nameBranch(ctx, row, repos)
		var made []git.Worktree
		undo := func() {
			for i, wt := range made {
				_ = git.RemoveWorktree(ctx, repos[i], wt.Path, wt.Branch, true)
			}
		}
		for i := range parts {
			base, err := git.LatestBase(ctx, repos[i], parts[i].BaseBranch)
			if err != nil {
				undo()
				fail(fmt.Errorf("%s: %w", filepath.Base(repos[i]), err))
				return
			}
			if base.FetchErr != nil {
				c.addNotice(id, "warning", fmt.Sprintf("Couldn't fetch %s of %s from origin, so it starts from the last copy fetched.", base.Branch, filepath.Base(repos[i])))
			}
			wt, err := git.AddWorktree(ctx, repos[i], c.partWorktreePath(row, repos, i), branch, base.Ref)
			if err != nil {
				undo()
				fail(fmt.Errorf("%s: %w", filepath.Base(repos[i]), err))
				return
			}
			made = append(made, wt)
			parts[i].Worktree, parts[i].Branch, parts[i].BaseBranch, parts[i].BaseCommit = wt.Path, wt.Branch, base.Branch, wt.BaseCommit
			where := base.Ref
			if len(repos) > 1 {
				where += " in " + filepath.Base(repos[i])
			}
			c.addNotice(id, "info", fmt.Sprintf("Working on branch %s from %s.", wt.Branch, where))
		}
		c.mu.Lock()
		for i, p := range t.parts {
			p.Part = parts[i]
		}
		t.row.UpdatedAt = now()
		err := c.saveLocked(t)
		row = t.row
		c.mu.Unlock()
		if err != nil {
			fail(err)
			return
		}
	}
	// Workers are started by the orchestrator, which wrote the prompt.
	if err := c.send(ctx, id, row.Prompt, row.PromptImages, false, FromOrchestrator, ""); err != nil {
		fail(err)
	}
}

// openParams builds the session parameters for a thread. Call without c.mu held: the orchestrator's
// spec reads core state.
func (c *Core) openParams(row store.Thread) agent.OpenParams {
	c.mu.Lock()
	spec, claudePath, effort, commitRules := c.orchSpec, c.settings.ClaudePath, c.settings.WorkerEffort, c.settings.CommitRules
	c.mu.Unlock()
	if row.Kind == store.KindOrchestrator && spec != nil {
		p := spec()
		p.SessionID, p.Resume = row.SessionID, row.HasTranscript
		if row.Model != "" {
			p.Model = row.Model
		}
		return p
	}
	return agent.OpenParams{
		SessionID:      row.SessionID,
		Resume:         row.HasTranscript,
		Cwd:            c.sessionDir(row),
		Model:          row.Model,
		Effort:         cmp.Or(row.Effort, effort),
		Title:          row.Title,
		SystemAppend:   workerInstructions(row, commitRules),
		PermissionMode: agent.PermissionBypass,
		ToolPolicy:     agent.ToolPolicyAllow,
		ClaudePath:     agent.FindClaude(claudePath).Path,
	}
}

// workerInstructions are what a thread is told about working in Solo, after Claude Code's own. With
// commitRules (Settings.CommitRules), its commits follow them rather than Conventional Commits.
func workerInstructions(r store.Thread, commitRules string) string {
	ps := loadParts(r)
	where := fmt.Sprintf(`You work alone in a git worktree of %s on branch %s, created from %s. Other workers may be changing
other branches of the same repository at the same time; stay on your branch and in this worktree.`, r.RepoPath, r.Branch, r.BaseBranch)
	if len(ps) == 1 && ps[0].InPlace {
		return `You are a thread in Solo. Solo is the lead agent the user talks to; it gave you this task.
You work directly in ` + ps[0].Repo + `, which isn't a git repository: your edits change the user's own
files at once, with no branch or copy to review first, and other threads may work in the same folder.

- Do the task you were given completely, verifying your work (build, tests) where the project allows.
- You own the whole task. When parts of it are independent and large, run them in parallel with
  subagents (the Agent tool), and always when Solo or the user asks you to.
- Change only what the task needs, and be careful with deleting or overwriting files: there is no undo.
  Don't run git init; the user decides whether this folder goes in git.
- End with a short summary of what you changed (each file) and anything left undone, for the user and
  Solo.
- If you need a decision only the user can make, ask with AskUserQuestion.`
	}
	if len(ps) > 1 {
		var b strings.Builder
		b.WriteString("This task spans several repositories. Your working directory holds a git worktree of each, all on\nbranch " + r.Branch + ":\n")
		repos := make([]string, len(ps))
		for i, p := range ps {
			repos[i] = p.Repo
		}
		for i, dir := range partDirs(repos) {
			fmt.Fprintf(&b, "- %s/: %s, created from %s\n", dir, ps[i].Repo, ps[i].BaseBranch)
		}
		b.WriteString(`Other workers may be changing other branches of these repositories at the same time; stay on your
branch and in these worktrees. Change only the repositories the task needs; commit in each one you change.`)
		where = b.String()
	}
	commit, rules := "with a Conventional Commit message", ""
	if r := strings.TrimSpace(commitRules); r != "" {
		commit = "with a message that follows the commit message rules below"
		rules = `

Commit message rules, from the user. They apply to every commit you make in this thread (including
merge commits and fixes made later), and override any other commit message convention, such as the
repository's or your own defaults:
` + r
	}
	return `You are a thread in Solo. Solo is the lead agent the user talks to; it gave you this task.
` + where + `

- Do the task you were given completely, verifying your work (build, tests) where the project allows.
- You own the whole task. When parts of it are independent and large, run them in parallel with
  subagents (the Agent tool), and always when Solo or the user asks you to.
- When you are done, commit your changes on this branch ` + commit + `. Don't push,
  merge, rebase onto other branches, or switch branches; the user reviews and merges your branch.
- End with a short summary of what you changed and anything left undone, for the user and Solo.
- If you need a decision only the user can make, ask with AskUserQuestion.` + rules
}

// Send sends the user's message, with any images (see ImportImage), to a thread, starting its session
// if needed. Sent while the thread is working, it steers: Claude stops what it is doing and reads it.
func (c *Core) Send(ctx context.Context, threadID, text string, images ...string) error {
	return c.send(ctx, threadID, text, images, false, FromUser, "")
}

// SendWithContext sends the user's message as Send does, with hidden context before it: Claude reads
// both, and the transcript shows only the message.
func (c *Core) SendWithContext(ctx context.Context, threadID, hidden, text string, images ...string) error {
	return c.send(ctx, threadID, text, images, false, FromUser, hidden)
}

// SendFromOrchestrator sends the orchestrator's message to a worker.
func (c *Core) SendFromOrchestrator(ctx context.Context, threadID, text string, images ...string) error {
	return c.send(ctx, threadID, text, images, false, FromOrchestrator, "")
}

// SendSynthetic sends text the app composed (for example a thread update to the orchestrator).
func (c *Core) SendSynthetic(ctx context.Context, threadID, text string) error {
	return c.send(ctx, threadID, text, nil, true, FromSolo, "")
}

// SendSyntheticWithContext sends app-composed text with hidden context before it, which Claude reads
// and the transcript leaves out.
func (c *Core) SendSyntheticWithContext(ctx context.Context, threadID, hidden, text string) error {
	return c.send(ctx, threadID, text, nil, true, FromSolo, hidden)
}

// send records text as the message and sends it to Claude, after hidden when that isn't empty.
func (c *Core) send(ctx context.Context, threadID, text string, images []string, synthetic bool, from, hidden string) error {
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return errors.New("empty message")
	}
	blocks, err := c.loadImages(images)
	if err != nil {
		return err
	}
	// Solo only sees the pixels; it needs the files' paths to hand the images on to a thread.
	sent := text
	if threadID == OrchestratorID && len(images) > 0 {
		sent = strings.TrimSpace(text + "\n\n[Attached images, saved at: " + strings.Join(images, ", ") +
			". To give them to a thread, pass these paths in images when you call spawn_thread or message_thread.]")
	}
	if hidden != "" {
		sent = hidden + "\n\n---\n\n" + sent
	}
	c.mu.Lock()
	t := c.threads[threadID]
	if t == nil {
		c.mu.Unlock()
		return fmt.Errorf("no thread %s", threadID)
	}
	if Finished(t.row.Status) {
		c.mu.Unlock()
		return fmt.Errorf("thread %q is %s", t.row.Title, t.row.Status)
	}
	if t.row.Kind == store.KindWorker && t.row.WorktreePath == "" && !t.inPlace() {
		c.mu.Unlock()
		return fmt.Errorf("thread %q has not started yet", t.row.Title)
	}
	// The user's message to a working thread steers it, as in t3code; a question it is waiting on is
	// answered in its own card, so a message then waits for the turn like the others.
	steer := from == FromUser && !synthetic && t.inTurn && len(t.asks) == 0
	// Record the message and the running state before Claude can answer, so a fast reply never lands
	// ahead of the message or leaves the thread marked running after its turn ended.
	uuid := agent.NewUUID()
	data, _ := json.Marshal(UserData{Text: text, Synthetic: synthetic, From: from, Images: images, Steer: steer})
	if err := c.db.AddItem(&store.Item{ThreadID: threadID, Kind: ItemUser, TurnUUID: uuid, Data: data, CreatedAt: now()}); err != nil {
		c.mu.Unlock()
		return err
	}
	prev := t.row.Status
	if len(t.staleAsks) > 0 {
		// A question from before Solo restarted: a message instead of an answer moves past it.
		for key, itemID := range t.staleAsks {
			c.updateItemLocked(itemID, func(raw json.RawMessage) any {
				var d QuestionData
				_ = json.Unmarshal(raw, &d)
				d.Cancelled = true
				return d
			})
			delete(t.askQs, key)
		}
		t.staleAsks = nil
		if prev == StatusWaiting {
			prev = StatusIdle
		}
	}
	var ch Change
	if prev != StatusRunning && prev != StatusWaiting {
		ch = c.setStatusLocked(t, StatusRunning, "")
	}
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeItems, ThreadID: threadID}, ch)

	fail := func(err error) error {
		c.mu.Lock()
		var back Change
		if t.row.Status == StatusRunning && !t.inTurn {
			back = c.setStatusLocked(t, prev, t.row.Error)
		}
		c.mu.Unlock()
		c.notify(back)
		c.addNotice(threadID, "error", "The message was not delivered: "+err.Error())
		return err
	}
	if err := c.deliver(ctx, t, agent.SendParams{UUID: uuid, Text: sent, Images: blocks, Synthetic: synthetic, Steer: steer}); err != nil {
		return fail(err)
	}
	return nil
}

// deliver sends p to the thread's current session, opening it first if needed. It holds t.sess, so the
// session can't be closed or replaced between the open and the send. A session the agent manager has
// lost is opened again (resuming it), and the message sent once more.
func (c *Core) deliver(ctx context.Context, t *thread, p agent.SendParams) error {
	t.sess.Lock()
	defer t.sess.Unlock()
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		needOpen, row := !t.opened, t.row
		c.mu.Unlock()
		params := c.openParams(row)
		if needOpen {
			if err := c.agent.Open(ctx, params); err != nil {
				return err
			}
			c.mu.Lock()
			t.opened = true
			c.mu.Unlock()
		}
		p.SessionID = params.SessionID
		err := c.agent.SendUUID(ctx, p)
		if err == nil || attempt > 0 || !errors.Is(err, agent.ErrUnknownSession) {
			return err
		}
		c.log.Warn("core: reopening a session the agent lost", "thread", row.ID, "session", row.SessionID)
		c.mu.Lock()
		t.opened = false
		c.mu.Unlock()
	}
}

// SetThreadModel changes a thread's model and effort ("" effort: the default in Settings). A new model
// applies at once, even mid-turn; a new effort needs the session to restart, which happens now when
// the thread is between turns, or at the end of the turn (it resumes, keeping its context).
func (c *Core) SetThreadModel(ctx context.Context, threadID, model, effort string) error {
	c.mu.Lock()
	t := c.threads[threadID]
	if t == nil {
		c.mu.Unlock()
		return fmt.Errorf("no thread %s", threadID)
	}
	if model == "" {
		model = t.row.Model
	}
	modelChanged, effortChanged := model != t.row.Model, effort != t.row.Effort
	if !modelChanged && !effortChanged {
		c.mu.Unlock()
		return nil
	}
	t.row.Model, t.row.Effort = model, effort
	t.row.UpdatedAt = now()
	err := c.saveLocked(t)
	opened, sid := t.opened, t.row.SessionID
	// Background work dies with the session, so a session with any also waits for its turn to end
	// with nothing left running.
	inTurn := t.inTurn || len(t.bg) > 0
	if opened && inTurn && effortChanged {
		t.reopen = true
	}
	status := t.row.Status
	c.mu.Unlock()
	if err != nil {
		return err
	}
	switch {
	case opened && !inTurn:
		c.closeSession(ctx, threadID) // the next message reopens it with the new settings
	case opened && modelChanged:
		if err := c.agent.SetModel(ctx, sid, model); err != nil {
			c.log.Warn("core: changing the model", "thread", threadID, "err", err)
		}
	}
	c.notify(Change{Kind: ChangeThread, ThreadID: threadID, PrevStatus: status, Status: status})
	return nil
}

// Interrupt stops a thread's running turn, and any work it runs in the background.
func (c *Core) Interrupt(ctx context.Context, threadID string) error {
	c.mu.Lock()
	t := c.threads[threadID]
	if t == nil {
		c.mu.Unlock()
		return fmt.Errorf("no thread %s", threadID)
	}
	sid, queued, inTurn := t.row.SessionID, t.row.Status == StatusQueued, t.inTurn
	var ch Change
	if queued {
		ch = c.setStatusLocked(t, StatusInterrupted, "")
	}
	// Stop means everything: the work it runs in the background too.
	var bg []string
	for _, b := range t.bg {
		bg = append(bg, b.ID)
	}
	if len(bg) > 0 {
		t.bgStopped = true
	}
	c.mu.Unlock()
	if queued {
		c.notify(ch)
		return nil
	}
	var err error
	if inTurn || len(bg) == 0 {
		err = c.agent.Interrupt(ctx, sid)
	}
	for _, id := range bg {
		if e := c.agent.StopTask(ctx, sid, id); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// Answer records the user's answer to a pending question. answers is keyed by question text.
func (c *Core) Answer(threadID, toolUseID string, answers map[string]string) error {
	return c.resolveAsk(threadID, toolUseID, agent.AskResult{Answers: answers}, FromUser)
}

// AnswerFromOrchestrator answers a worker's pending question for the user, as Answer does.
func (c *Core) AnswerFromOrchestrator(threadID, toolUseID string, answers map[string]string) error {
	return c.resolveAsk(threadID, toolUseID, agent.AskResult{Answers: answers}, FromOrchestrator)
}

// Dismiss declines a pending question for the user.
func (c *Core) Dismiss(threadID, toolUseID string) error {
	return c.resolveAsk(threadID, toolUseID, agent.AskResult{Cancelled: true}, FromUser)
}

// resolveAsk gives a pending question its result, and tells subscribers (ChangeAnswer).
func (c *Core) resolveAsk(threadID, toolUseID string, res agent.AskResult, from string) error {
	c.mu.Lock()
	t := c.threads[threadID]
	if t == nil {
		c.mu.Unlock()
		return fmt.Errorf("no thread %s", threadID)
	}
	qs := t.askQs[toolUseID]
	if ch := t.asks[toolUseID]; ch != nil {
		delete(t.asks, toolUseID)
		delete(t.askQs, toolUseID)
		ch <- res
	} else if itemID, ok := t.staleAsks[toolUseID]; ok {
		if err := c.answerStaleLocked(t, toolUseID, itemID, res); err != nil {
			c.mu.Unlock()
			return err
		}
	} else {
		c.mu.Unlock()
		return errors.New("that question is no longer waiting for an answer")
	}
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeAnswer, ThreadID: threadID,
		Answer: &Answered{Questions: qs, Answers: res.Answers, Skipped: res.Cancelled, From: from}})
	return nil
}

// Changes lists the files a worker changed. With several repositories, each path starts with its
// repository's folder ("api/internal/x.go").
func (c *Core) Changes(ctx context.Context, threadID string) ([]git.FileChange, error) {
	v, ok := c.Thread(threadID)
	if !ok || !v.HasWorktree {
		return nil, errors.New("this thread has no worktree")
	}
	var out []git.FileChange
	dirs := partDirs(partRepos(v.Parts))
	for i, p := range v.Parts {
		if p.Worktree == "" || p.BaseCommit == "" {
			continue
		}
		fc, err := git.Changes(ctx, p.Worktree, p.BaseCommit)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
		if v.MultiRepo() {
			for j := range fc {
				fc[j].Path = dirs[i] + "/" + fc[j].Path
			}
		}
		out = append(out, fc...)
	}
	return out, nil
}

// Diff returns a worker's diff against where it started, every repository's in one, with paths as
// Changes gives them.
func (c *Core) Diff(ctx context.Context, threadID string) (string, error) {
	v, ok := c.Thread(threadID)
	if !ok || !v.HasWorktree {
		return "", errors.New("this thread has no worktree")
	}
	var b strings.Builder
	dirs := partDirs(partRepos(v.Parts))
	for i, p := range v.Parts {
		if p.Worktree == "" || p.BaseCommit == "" {
			continue
		}
		prefix := ""
		if v.MultiRepo() {
			prefix = dirs[i]
		}
		d, err := git.DiffPrefixed(ctx, p.Worktree, p.BaseCommit, prefix)
		if err != nil {
			return "", fmt.Errorf("%s: %w", p.Name, err)
		}
		if b.Len() > 0 && d != "" && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
		b.WriteString(d)
	}
	return b.String(), nil
}

func partRepos(ps []PartView) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Repo
	}
	return out
}

// Merge lands every repository of a worker that can land now: its pull request when it can merge,
// or, for a repository without a forge, its branch merged locally. It returns the last local merge
// commit, if any.
func (c *Core) Merge(ctx context.Context, threadID string) (string, error) {
	return c.MergeRepo(ctx, threadID, "")
}

// land marks a landing action as under way on a thread, and returns what ends it. It fails with ErrBusy
// when the thread already has one.
func (c *Core) land(threadID string) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.landing[threadID] {
		return nil, ErrBusy
	}
	c.landing[threadID] = true
	return func() {
		c.mu.Lock()
		delete(c.landing, threadID)
		c.mu.Unlock()
	}, nil
}

// MergeRepo is Merge for one of the worker's repositories (by path or folder name; "" for all).
func (c *Core) MergeRepo(ctx context.Context, threadID, repo string) (string, error) {
	release, err := c.land(threadID)
	if err != nil {
		return "", err
	}
	defer release()
	v, ok := c.Thread(threadID)
	if !ok {
		return "", fmt.Errorf("no thread %s", threadID)
	}
	var todo []PartView
	for _, p := range v.Parts {
		if repo != "" && p.Repo != repo && p.Name != repo {
			continue
		}
		if p.CanMerge {
			todo = append(todo, p)
		}
	}
	if len(todo) == 0 {
		for _, p := range v.Parts {
			if (repo == "" || p.Repo == repo || p.Name == repo) && p.Forge != forge.None && p.PR == nil && p.Changed != 0 {
				return "", fmt.Errorf("open a %s for %q first, or merge it locally", p.Forge.Noun(), v.Title)
			}
		}
		return "", fmt.Errorf("thread %q has nothing that can merge now", v.Title)
	}
	var commit string
	var local []PartView
	for _, p := range todo {
		if p.PR != nil && p.PR.State == forge.StateOpen {
			if err := c.mergePR(ctx, v, p); err != nil {
				return "", err
			}
			continue
		}
		local = append(local, p)
	}
	if len(local) > 0 {
		var err error
		if commit, err = c.mergeLocalParts(ctx, v, local); err != nil {
			return "", err
		}
	}
	return commit, nil
}

// MergeLocal merges each of a worker's branches into its base branch in the repository's own
// checkout, skipping pull requests.
func (c *Core) MergeLocal(ctx context.Context, threadID string) (string, error) {
	release, err := c.land(threadID)
	if err != nil {
		return "", err
	}
	defer release()
	v, ok := c.Thread(threadID)
	if !ok {
		return "", fmt.Errorf("no thread %s", threadID)
	}
	if !v.CanMergeLocal {
		return "", fmt.Errorf("thread %q can't be merged while it is %s", v.Title, v.Status)
	}
	var todo []PartView
	for _, p := range v.Parts {
		if p.CanMergeLocal {
			todo = append(todo, p)
		}
	}
	return c.mergeLocalParts(ctx, v, todo)
}

// mergeLocalParts merges the parts' branches locally; when nothing is left to land, the thread is
// merged and its worktrees go.
func (c *Core) mergeLocalParts(ctx context.Context, v ThreadView, todo []PartView) (string, error) {
	var commit string
	for _, p := range todo {
		if _, err := git.CommitAll(ctx, p.Worktree, uncommittedPrefix+v.Title); err != nil {
			return "", fmt.Errorf("committing %s's changes: %w", p.Name, err)
		}
		var err error
		commit, err = git.Merge(ctx, p.Repo, p.Branch, p.BaseBranch, fmt.Sprintf("Merge %s: %s", p.Branch, v.Title))
		if err != nil {
			var me *git.MergeError
			if errors.As(err, &me) && len(me.Conflicts) > 0 && !me.Local {
				if err := c.handConflicts(ctx, v, p, me.Conflicts); err != nil {
					c.addNotice(v.ID, "error", "Merge failed in "+p.Name+": "+err.Error())
					return "", err
				}
				return "", &ResolvingError{Title: v.Title}
			}
			c.addNotice(v.ID, "error", "Merge failed in "+p.Name+": "+err.Error())
			return "", err
		}
		short := commit
		if len(short) > 8 {
			short = short[:8]
		}
		where := p.BaseBranch
		if v.MultiRepo() {
			where += " in " + p.Name
		}
		c.addNotice(v.ID, "info", fmt.Sprintf("Merged into %s (%s).", where, short))
		c.mu.Lock()
		if t := c.threads[v.ID]; t != nil {
			if tp := t.part(p.Repo); tp != nil {
				tp.changed = 0 // landed: nothing left to merge here
			}
		}
		c.mu.Unlock()
	}
	c.finishIfLanded(v.ID)
	return commit, nil
}

// maxResolveTries is how many times a merge goes back to the worker before the user is told.
const maxResolveTries = 2

// handConflicts asks the worker to resolve the conflicts between its branch and the base branch; the
// branch merges when its turn ends (see mergeResolved).
func (c *Core) handConflicts(ctx context.Context, v ThreadView, p PartView, files []string) error {
	c.mu.Lock()
	t := c.threads[v.ID]
	if t == nil {
		c.mu.Unlock()
		return fmt.Errorf("no thread %s", v.ID)
	}
	if t.resolveTries >= maxResolveTries {
		c.mu.Unlock()
		return fmt.Errorf("%s still conflicts with %s after %d tries to resolve it: %s", p.Branch, p.BaseBranch,
			maxResolveTries, strings.Join(files, ", "))
	}
	t.resolveTries++
	t.resolving = true
	ch := Change{Kind: ChangeThread, ThreadID: v.ID}
	c.mu.Unlock()
	c.notify(ch)
	where := ""
	if v.MultiRepo() {
		where = " in " + p.Name
	}
	msg := fmt.Sprintf("Merging your branch into %s%s conflicts with work that landed since you started, in: %s.\n\n"+
		"Merge %s into your branch (`git merge %s`), resolve each conflict so both changes still do what they meant to, "+
		"run the checks that cover them, and commit. Solo merges your branch as soon as you finish; don't merge it yourself.",
		p.BaseBranch, where, strings.Join(files, ", "), p.BaseBranch, p.BaseBranch)
	if err := c.SendFromOrchestrator(ctx, v.ID, msg); err != nil {
		c.mu.Lock()
		t.resolving = false
		c.mu.Unlock()
		c.notify(ch)
		return err
	}
	c.addNotice(v.ID, "info", "Merging conflicted with newer work in "+p.BaseBranch+"; resolving it, then merging.")
	return nil
}

// mergeResolved merges a worker's branch after it resolved conflicts.
func (c *Core) mergeResolved(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Whether the thread can merge goes by its count of changed files, which the turn that just ended
	// is having counted again, and which a count that ran into a merge's commit can have left at none:
	// count before merging.
	c.recount(id)
	// The merge that handed the conflicts over holds the thread until it has returned, which a worker
	// that finishes at once can beat: wait for it, rather than give the merge up as busy.
	_, err := c.MergeLocal(ctx, id)
	for errors.Is(err, ErrBusy) && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Millisecond):
		}
		_, err = c.MergeLocal(ctx, id)
	}
	var re *ResolvingError
	if errors.As(err, &re) {
		return // back to the worker, still resolving
	}
	if err != nil {
		c.log.Warn("core: merging after resolving conflicts", "thread", id, "err", err)
	}
	c.mu.Lock()
	if t := c.threads[id]; t != nil {
		t.resolving = false
	}
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeThread, ThreadID: id})
}

// finishIfLanded closes a thread once each of its repositories with changes has landed (merged
// locally, or its pull request merged): the session ends, the worktrees and local branches go, and
// the thread is merged.
func (c *Core) finishIfLanded(id string) {
	c.mu.Lock()
	t := c.threads[id]
	if t == nil || Finished(t.row.Status) {
		c.mu.Unlock()
		return
	}
	for _, p := range t.parts {
		merged := p.PR != nil && p.PR.State == forge.StateMerged
		if p.changed != 0 && !merged {
			c.mu.Unlock()
			return
		}
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c.closeSession(ctx, id)
	c.removeWorktrees(ctx, id, false)
	c.mu.Lock()
	var ch Change
	if t := c.threads[id]; t != nil {
		ch = c.setStatusLocked(t, StatusMerged, "")
	}
	c.mu.Unlock()
	c.notify(ch)
	c.schedule()
}

// removeWorktrees removes a worker's worktrees, and their local branches unless keepBranches.
func (c *Core) removeWorktrees(ctx context.Context, id string, keepBranches bool) {
	c.mu.Lock()
	t := c.threads[id]
	if t == nil {
		c.mu.Unlock()
		return
	}
	var ps []Part
	for _, p := range t.parts {
		ps = append(ps, p.Part)
	}
	multi := len(t.parts) > 1
	dir := c.threadDir(t.row)
	c.mu.Unlock()
	for _, p := range ps {
		if p.InPlace || (p.Worktree == "" && p.Branch == "") { // never touch a folder worked in directly
			continue
		}
		if err := git.RemoveWorktree(ctx, p.Repo, p.Worktree, p.Branch, !keepBranches); err != nil {
			c.log.Warn("core: removing a worktree", "repo", p.Repo, "err", err)
		}
	}
	if multi {
		_ = os.Remove(dir) // empty now
	}
	c.mu.Lock()
	if t := c.threads[id]; t != nil {
		for _, p := range t.parts {
			p.Worktree = ""
		}
		_ = c.saveLocked(t)
	}
	c.mu.Unlock()
}

// Discard stops a worker and deletes its worktrees and branches.
func (c *Core) Discard(ctx context.Context, threadID string) error {
	release, err := c.land(threadID)
	if err != nil {
		return err
	}
	defer release()
	v, ok := c.Thread(threadID)
	if !ok {
		return fmt.Errorf("no thread %s", threadID)
	}
	if !v.CanDiscard {
		return fmt.Errorf("thread %q is already %s", v.Title, v.Status)
	}
	c.closeSession(ctx, threadID)
	c.removeWorktrees(ctx, threadID, false)
	c.mu.Lock()
	var ch Change
	if t := c.threads[threadID]; t != nil {
		t.row.DoneAt = now() // a discarded thread leaves the rail at once
		ch = c.setStatusLocked(t, StatusDiscarded, "")
	}
	c.mu.Unlock()
	c.addNotice(threadID, "info", "Discarded.")
	c.notify(ch)
	c.schedule()
	c.removeAttachments(ctx, threadID)
	return nil
}

// Done marks a thread done: it leaves the rail for History, merged or not. Its session ends and its
// worktrees go. A thread whose work landed (or that had none) loses its branches too; one with work
// that hasn't landed (ThreadView.Unlanded) keeps them, its uncommitted edits committed, so nothing is
// lost and Restore brings it back. Discard is what throws work away.
func (c *Core) Done(ctx context.Context, threadID string) error {
	release, err := c.land(threadID)
	if err != nil {
		return err
	}
	defer release()
	v, ok := c.Thread(threadID)
	if !ok {
		return fmt.Errorf("no thread %s", threadID)
	}
	if !v.CanDone {
		if !v.DoneAt.IsZero() {
			return fmt.Errorf("thread %q is already done", v.Title)
		}
		return fmt.Errorf("thread %q can't be marked done while it is %s; stop it first", v.Title, v.Status)
	}
	if v.Unlanded {
		for _, p := range v.Parts {
			if err := keepWork(ctx, v, p); err != nil {
				return fmt.Errorf("keeping %s's work before marking it done: %w", p.Name, err)
			}
		}
	}
	if v.HasWorktree || v.InPlace {
		c.closeSession(ctx, threadID)
		c.removeWorktrees(ctx, threadID, v.Unlanded)
	}
	if v.Unlanded {
		var branches []string
		for _, p := range v.Parts {
			if p.Branch != "" && !p.InPlace {
				branches = append(branches, p.Branch)
			}
		}
		c.addNotice(threadID, "info", fmt.Sprintf("Marked done without landing. Its branch %s is kept; Settings › History can restore it.",
			strings.Join(branches, ", ")))
	}
	c.mu.Lock()
	t := c.threads[threadID]
	if t == nil {
		c.mu.Unlock()
		return fmt.Errorf("no thread %s", threadID)
	}
	var ch Change
	t.row.DoneAt = now()
	if Finished(t.row.Status) {
		t.row.UpdatedAt = now()
		err := c.saveLocked(t)
		c.mu.Unlock()
		if err != nil {
			return err
		}
		ch = Change{Kind: ChangeThread, ThreadID: threadID, PrevStatus: t.row.Status, Status: t.row.Status}
	} else {
		ch = c.setStatusLocked(t, StatusArchived, "")
		c.mu.Unlock()
	}
	c.notify(ch)
	return nil
}

// keepWork commits what a part's worktree left uncommitted, onto its branch, so removing the worktree
// loses nothing. A detached HEAD is put back on the part's branch first, so its commits stay reachable.
func keepWork(ctx context.Context, v ThreadView, p PartView) error {
	if p.InPlace || p.Worktree == "" || p.Branch == "" {
		return nil
	}
	if _, err := os.Stat(p.Worktree); err != nil {
		return nil // already gone
	}
	if git.CurrentBranch(ctx, p.Worktree) == "" {
		if _, err := git.Run(ctx, p.Worktree, "checkout", "-q", "-B", p.Branch); err != nil {
			return err
		}
	}
	_, err := git.CommitAll(ctx, p.Worktree, uncommittedPrefix+v.Title)
	return err
}

// Restore brings a done thread back to the rail. Its worktrees are recreated at the same paths, so its
// Claude session resumes: on its branch if that still exists, or on a new branch of the same name
// from the current head of the branch it started from.
func (c *Core) Restore(ctx context.Context, threadID string) error {
	release, err := c.land(threadID)
	if err != nil {
		return err
	}
	defer release()
	v, ok := c.Thread(threadID)
	if !ok {
		return fmt.Errorf("no thread %s", threadID)
	}
	if !v.IsWorker || v.DoneAt.IsZero() {
		return fmt.Errorf("thread %q isn't in History", v.Title)
	}
	repos := partRepos(v.Parts)
	made := make([]git.Worktree, len(v.Parts))
	for i, p := range v.Parts {
		if _, err := os.Stat(p.Repo); err != nil {
			return fmt.Errorf("%s is gone: %w", p.Repo, err)
		}
		if p.InPlace {
			continue // it works in the folder itself
		}
		path := p.Worktree
		if path == "" {
			path = c.partWorktreePath(v.Thread, repos, i)
		}
		made[i] = git.Worktree{Path: path}
		if _, err := os.Stat(path); err == nil {
			continue
		}
		base, err := git.LatestBase(ctx, p.Repo, p.BaseBranch)
		if err != nil {
			return err
		}
		if made[i], err = git.RestoreWorktree(ctx, p.Repo, path, p.Branch, base.Ref); err != nil {
			return fmt.Errorf("recreating %s's worktree: %w", p.Name, err)
		}
	}
	c.mu.Lock()
	t := c.threads[threadID]
	if t == nil {
		c.mu.Unlock()
		return fmt.Errorf("no thread %s", threadID)
	}
	for i, p := range t.parts {
		if p.InPlace {
			continue
		}
		p.Worktree, p.changed = made[i].Path, -1
		if made[i].BaseCommit != "" {
			p.BaseCommit = made[i].BaseCommit
		}
		if p.PR != nil && p.PR.State != forge.StateOpen {
			p.PR = nil // a new branch gets a new pull request
		}
	}
	t.row.DoneAt, t.row.Error = time.Time{}, ""
	t.opened = false
	ch := c.setStatusLocked(t, StatusIdle, "")
	c.mu.Unlock()
	c.addNotice(threadID, "info", "Restored from History.")
	c.notify(ch)
	go c.countChanges(threadID)
	return nil
}

const namingSystem = "You name git branches. Reply with only the branch name: no quotes, no explanation, no code formatting."

// nameBranch names a new worker's branch. With branch rules in Settings, the naming model follows
// them; otherwise, or when its answer isn't a usable name, the branch is solo/<title>-<id>.
func (c *Core) nameBranch(ctx context.Context, row store.Thread, repos []string) string {
	fallback := "solo/" + git.Slug(row.Title) + "-" + row.ID[:6]
	s := c.Settings()
	rules := s.BranchRulesFor(repos[0])
	if rules == "" {
		return fallback
	}
	prompt := fmt.Sprintf("Name the git branch for this task, following the rules.\n\nRules:\n%s\n\nRepository: %s\nTask title: %s\nTask:\n%s",
		rules, repoNames(repos), row.Title, clipText(row.Prompt, 1500))
	nctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	text, err := c.agent.Complete(nctx, agent.CompleteParams{Model: s.NamingModel, Effort: s.NamingEffort,
		System: namingSystem, Prompt: prompt, ClaudePath: agent.FindClaude(s.ClaudePath).Path, TimeoutMs: 40_000})
	if err != nil {
		c.addNotice(row.ID, "warning", "Couldn't name the branch by your rules ("+err.Error()+"), so it's "+fallback+".")
		return fallback
	}
	name := cleanBranchName(text)
	if !git.ValidBranchName(ctx, repos[0], name) {
		c.addNotice(row.ID, "warning", fmt.Sprintf("The suggested branch name %q isn't valid, so it's %s.", firstLine(text, 80), fallback))
		return fallback
	}
	taken := func(n string) bool { // in any of the repositories, since they share the name
		return slices.ContainsFunc(repos, func(r string) bool { return git.BranchExists(ctx, r, n) })
	}
	for i, n := 2, name; ; i++ {
		if !taken(n) {
			return n
		}
		n = fmt.Sprintf("%s-%d", name, i)
	}
}

func repoNames(repos []string) string {
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = filepath.Base(r)
	}
	return strings.Join(names, ", ")
}

// cleanBranchName takes a branch name from a model's answer: its first line, without quotes or code
// marks.
func cleanBranchName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(s, "`\"' ")
	if strings.ContainsAny(s, " \t") {
		return "" // a sentence, not a name
	}
	return s
}

// countChanges counts the files each of a worker's branches changes, for Done and the landing
// actions, finds each repository's forge, pushes new commits to open pull requests, and looks for work
// that landed outside Solo (findPRs, findLanded).
func (c *Core) countChanges(id string) {
	if !c.recount(id) {
		return
	}
	// Work that landed outside Solo's merge and PR buttons (landed.go).
	lctx, lcancel := context.WithTimeout(context.Background(), time.Minute)
	defer lcancel()
	c.findPRs(lctx, id)
	c.findLanded(lctx, id)
}

// recount counts the files a worker's branches change and records the counts; false when there was
// nothing to count, or it couldn't.
func (c *Core) recount(id string) bool {
	v, ok := c.Thread(id)
	if !ok || !v.IsWorker || !v.HasWorktree || Finished(v.Status) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	counts := map[string]int{}
	kinds := map[string]forge.Kind{}
	pushed := false
	for _, p := range v.Parts {
		if p.Worktree == "" || p.BaseCommit == "" {
			continue
		}
		if _, err := os.Stat(p.Worktree); err != nil {
			return false // being removed
		}
		files, err := git.Changes(ctx, p.Worktree, p.BaseCommit)
		if err != nil {
			c.log.Warn("core: counting changes", "thread", id, "repo", p.Repo, "err", err)
			return false
		}
		counts[p.Repo], kinds[p.Repo] = len(files), c.forgeFor(p.Repo)
		if p.PR != nil && p.PR.State == forge.StateOpen && !v.InTurn && c.pushPR(ctx, v, p) {
			pushed = true
		}
	}
	if pushed {
		c.cardStale(id)
	}
	c.mu.Lock()
	t := c.threads[id]
	if t == nil {
		c.mu.Unlock()
		return false
	}
	changed := false
	for _, p := range t.parts {
		n, ok := counts[p.Repo]
		if !ok || (p.changed == n && p.forge == kinds[p.Repo]) {
			continue
		}
		p.changed, p.forge, changed = n, kinds[p.Repo], true
	}
	c.mu.Unlock()
	if changed {
		nv, _ := c.Thread(id)
		c.notify(Change{Kind: ChangeThread, ThreadID: id, PrevStatus: nv.Status, Status: nv.Status})
	}
	return true
}

// removeAttachments deletes the images of a discarded thread's messages that no other thread uses
// (Solo's messages often share them).
func (c *Core) removeAttachments(ctx context.Context, threadID string) {
	items, err := c.db.Items(ctx, threadID, 0)
	if err != nil {
		return
	}
	c.mu.Lock()
	var paths []string
	if t := c.threads[threadID]; t != nil {
		paths = append(paths, t.row.PromptImages...)
	}
	c.mu.Unlock()
	for _, it := range items {
		if it.Kind == ItemUser {
			var d UserData
			if json.Unmarshal(it.Data, &d) == nil {
				paths = append(paths, d.Images...)
			}
		}
	}
	dir := c.attachmentsDir()
	for _, p := range paths {
		if filepath.Dir(p) != dir {
			continue // not ours to delete
		}
		if used, err := c.db.PathReferenced(p, threadID); err != nil || used {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.log.Warn("core: removing attachment", "err", err)
		}
	}
}

func (c *Core) closeSession(ctx context.Context, threadID string) {
	c.mu.Lock()
	t := c.threads[threadID]
	c.mu.Unlock()
	if t == nil {
		return
	}
	t.sess.Lock()
	defer t.sess.Unlock()
	c.closeSessionHeld(ctx, t)
}

// closeSessionHeld closes the thread's session. Call with t.sess held.
func (c *Core) closeSessionHeld(ctx context.Context, t *thread) {
	c.mu.Lock()
	threadID, sid := t.row.ID, t.row.SessionID
	t.opened = false
	for id, ch := range t.asks {
		ch <- agent.AskResult{Cancelled: true}
		delete(t.asks, id)
		delete(t.askQs, id)
	}
	c.mu.Unlock()
	if err := c.agent.Close(ctx, sid); err != nil {
		c.log.Warn("core: closing session", "thread", threadID, "err", err)
	}
}

// setStatusLocked changes a thread's status and saves it. Call with c.mu held; notify the returned
// change after unlocking.
func (c *Core) setStatusLocked(t *thread, status, errText string) Change {
	prev := t.row.Status
	t.row.Status = status
	t.row.Error = errText
	t.row.UpdatedAt = now()
	if err := c.saveLocked(t); err != nil {
		c.log.Error("core: saving thread", "err", err)
	}
	return Change{Kind: ChangeThread, ThreadID: t.row.ID, PrevStatus: prev, Status: status}
}

func (c *Core) addNotice(threadID, level, text string) {
	data, _ := json.Marshal(NoticeData{Level: level, Text: text})
	c.mu.Lock()
	err := c.db.AddItem(&store.Item{ThreadID: threadID, Kind: ItemNotice, Data: data, CreatedAt: now()})
	c.mu.Unlock()
	if err != nil {
		c.log.Error("core: saving notice", "err", err)
	}
	c.notify(Change{Kind: ChangeItems, ThreadID: threadID})
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max])) + "…"
	}
	return s
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// tildeDir writes a folder in the home folder as ~/….
func tildeDir(dir string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rest, ok := strings.CutPrefix(dir, home+string(filepath.Separator)); ok {
			return "~" + string(filepath.Separator) + rest
		}
	}
	return dir
}
