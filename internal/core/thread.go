package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/store"
)

// Thread statuses.
const (
	StatusQueued      = "queued"      // created; about to start
	StatusStarting    = "starting"    // creating its worktree and session
	StatusRunning     = "running"     // a turn is in progress
	StatusWaiting     = "waiting"     // asked the user a question
	StatusIdle        = "idle"        // its last turn finished; ready for review or more instructions
	StatusFailed      = "failed"      // its last turn, or its start, failed
	StatusInterrupted = "interrupted" // stopped by the user, or by Solo closing
	StatusMerged      = "merged"      // its branch was merged and its worktree removed
	StatusDiscarded   = "discarded"   // its worktree and branch were deleted
	StatusArchived    = "archived"    // marked done with no changes; its worktree and branch were removed
)

// Active reports whether a thread with the status is starting or working.
func Active(status string) bool {
	return status == StatusStarting || status == StatusRunning || status == StatusWaiting
}

// Finished reports whether a thread with the status is closed for good.
func Finished(status string) bool {
	return status == StatusMerged || status == StatusDiscarded || status == StatusArchived
}

// thread is a thread's state. Guarded by Core.mu.
type thread struct {
	row     store.Thread
	opened  bool // its session is registered with the agent manager
	inTurn  bool
	live    []liveKey // streaming blocks, in order
	liveTxt map[liveKey]*LiveBlock
	model   string // the model Claude reports
	// step is the tool call in progress, and turnStart when the current turn began.
	step      *ToolData
	turnStart time.Time
	retry     string // the latest API retry, while it lasts
	asks      map[string]chan agent.AskResult
	askQs     map[string][]agent.Question
	// recovered is when the thread last moved to a new session after losing its conversation.
	recovered time.Time
	tasks     map[string]int64 // task id → item id
	tools     map[string]int64 // tool_use id → item id
	// changed is how many files its branch changes from where it started; -1 until counted.
	changed int
}

type liveKey struct {
	message string
	index   int
}

// ThreadView is a snapshot of a thread.
type ThreadView struct {
	store.Thread
	InTurn       bool
	TurnStarted  time.Time // when the running turn began
	Step         *ToolData // the tool call in progress, if any
	ActualModel  string
	Retry        string
	PendingAsk   bool
	Live         []LiveBlock
	IsWorker     bool
	HasWorktree  bool
	CanMerge     bool
	CanDiscard   bool
	CanInterrupt bool
	// CanDone: the user may mark it done, which takes it out of the rail into History. Only threads with
	// nothing left to merge: merged, or finished without changes.
	CanDone bool
	// Changed is how many files a worker's branch changes from where it started; -1 until counted.
	Changed int
	// NoChanges is a worker whose last turn finished with nothing to merge.
	NoChanges bool
}

func (t *thread) view() ThreadView {
	v := ThreadView{
		Thread:      t.row,
		InTurn:      t.inTurn,
		TurnStarted: t.turnStart,
		Step:        t.step,
		ActualModel: t.model,
		Retry:       t.retry,
		PendingAsk:  len(t.asks) > 0,
		IsWorker:    t.row.Kind == store.KindWorker,
		HasWorktree: t.row.WorktreePath != "",
		Changed:     t.changed,
	}
	for _, k := range t.live {
		v.Live = append(v.Live, *t.liveTxt[k])
	}
	v.CanInterrupt = t.inTurn
	settled := v.IsWorker && v.HasWorktree && !Finished(t.row.Status) && !Active(t.row.Status) && t.row.Status != StatusQueued
	v.CanMerge = settled && t.changed != 0
	v.NoChanges = settled && t.changed == 0 && t.row.Status == StatusIdle
	v.CanDone = v.IsWorker && t.row.DoneAt.IsZero() && !t.inTurn &&
		(t.row.Status == StatusMerged || (settled && t.changed == 0))
	v.CanDiscard = v.IsWorker && !Finished(t.row.Status)
	return v
}

// Threads returns every thread, oldest first.
func (c *Core) Threads() []ThreadView {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ThreadView, 0, len(c.threads))
	for _, t := range c.threads {
		out = append(out, t.view())
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
	return t.view(), true
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
	Repo   string // path or name of a known repository
	Title  string
	Prompt string
	Model  string   // empty: Settings.WorkerModel
	Base   string   // branch to start from; empty: the repository's current branch
	Images []string // image files to send with the prompt
}

// CreateWorker creates a worker thread and starts it.
func (c *Core) CreateWorker(ctx context.Context, spec WorkerSpec) (ThreadView, error) {
	repo, err := c.ResolveRepo(spec.Repo)
	if err != nil {
		return ThreadView{}, err
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
	t := &thread{changed: -1, row: store.Thread{
		ID:           id,
		Kind:         store.KindWorker,
		SessionID:    agent.NewUUID(),
		Title:        title,
		RepoPath:     repo,
		Branch:       "solo/" + slug,
		BaseBranch:   spec.Base,
		Model:        spec.Model,
		Status:       StatusQueued,
		Prompt:       spec.Prompt,
		PromptImages: images,
		CreatedAt:    ts,
		UpdatedAt:    ts,
	}}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ThreadView{}, errClosed
	}
	if t.row.Model == "" {
		t.row.Model = c.settings.WorkerModel
	}
	if err := c.db.SaveThread(&t.row); err != nil {
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
	c.mu.Unlock()

	if row.WorktreePath == "" {
		// Start from the latest commit of the default branch (or the one Solo named), fetched now.
		base, err := git.LatestBase(ctx, row.RepoPath, row.BaseBranch)
		if err != nil {
			fail(err)
			return
		}
		if base.FetchErr != nil {
			c.addNotice(id, "warning", fmt.Sprintf("Couldn't fetch %s from origin, so this starts from the last copy fetched.", base.Branch))
		}
		branch := c.nameBranch(ctx, row)
		wt, err := git.AddWorktree(ctx, row.RepoPath, c.threadWorktreePath(row), branch, base.Ref)
		if err != nil {
			fail(err)
			return
		}
		c.mu.Lock()
		t.row.WorktreePath, t.row.Branch, t.row.BaseBranch, t.row.BaseCommit = wt.Path, wt.Branch, base.Branch, wt.BaseCommit
		t.row.UpdatedAt = now()
		err = c.db.SaveThread(&t.row)
		c.mu.Unlock()
		if err != nil {
			fail(err)
			return
		}
		c.addNotice(id, "info", fmt.Sprintf("Working on branch %s from %s.", wt.Branch, base.Ref))
	}
	// Workers are started by the orchestrator, which wrote the prompt.
	if err := c.send(ctx, id, row.Prompt, row.PromptImages, false, FromOrchestrator); err != nil {
		fail(err)
	}
}

// openParams builds the session parameters for a thread. Call without c.mu held: the orchestrator's
// spec reads core state.
func (c *Core) openParams(row store.Thread) agent.OpenParams {
	c.mu.Lock()
	spec, claudePath, effort := c.orchSpec, c.settings.ClaudePath, c.settings.WorkerEffort
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
		Cwd:            row.WorktreePath,
		Model:          row.Model,
		Effort:         effort,
		Title:          row.Title,
		SystemAppend:   workerInstructions(row),
		PermissionMode: agent.PermissionBypass,
		ToolPolicy:     agent.ToolPolicyAllow,
		ClaudePath:     agent.FindClaude(claudePath).Path,
	}
}

func workerInstructions(r store.Thread) string {
	return fmt.Sprintf(`You are a thread in Solo. Solo is the lead agent the user talks to; it gave you this task.
You work alone in a git worktree of %s on branch %s, created from %s. Other workers may be changing
other branches of the same repository at the same time; stay on your branch and in this worktree.

- Do the task you were given completely, verifying your work (build, tests) where the project allows.
- You own the whole task. When parts of it are independent and large, run them in parallel with
  subagents (the Agent tool), and always when Solo or the user asks you to.
- When you are done, commit your changes on this branch with a Conventional Commit message. Don't push,
  merge, rebase onto other branches, or switch branches; the user reviews and merges your branch.
- End with a short summary of what you changed and anything left undone, for the user and Solo.
- If you need a decision only the user can make, ask with AskUserQuestion.`, r.RepoPath, r.Branch, r.BaseBranch)
}

// Send sends the user's message, with any images (see ImportImage), to a thread, starting its session
// if needed.
func (c *Core) Send(ctx context.Context, threadID, text string, images ...string) error {
	return c.send(ctx, threadID, text, images, false, FromUser)
}

// SendFromOrchestrator sends the orchestrator's message to a worker.
func (c *Core) SendFromOrchestrator(ctx context.Context, threadID, text string, images ...string) error {
	return c.send(ctx, threadID, text, images, false, FromOrchestrator)
}

// SendSynthetic sends text the app composed (for example a thread update to the orchestrator).
func (c *Core) SendSynthetic(ctx context.Context, threadID, text string) error {
	return c.send(ctx, threadID, text, nil, true, FromSolo)
}

func (c *Core) send(ctx context.Context, threadID, text string, images []string, synthetic bool, from string) error {
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
	if t.row.Kind == store.KindWorker && t.row.WorktreePath == "" {
		c.mu.Unlock()
		return fmt.Errorf("thread %q has not started yet", t.row.Title)
	}
	needOpen := !t.opened
	row := t.row
	// Record the message and the running state before Claude can answer, so a fast reply never lands
	// ahead of the message or leaves the thread marked running after its turn ended.
	uuid := agent.NewUUID()
	data, _ := json.Marshal(UserData{Text: text, Synthetic: synthetic, From: from, Images: images})
	if err := c.db.AddItem(&store.Item{ThreadID: threadID, Kind: ItemUser, TurnUUID: uuid, Data: data, CreatedAt: now()}); err != nil {
		c.mu.Unlock()
		return err
	}
	prev := t.row.Status
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
	params := c.openParams(row)
	if needOpen {
		if err := c.agent.Open(ctx, params); err != nil {
			return fail(err)
		}
		c.mu.Lock()
		t.opened = true
		c.mu.Unlock()
	}
	if err := c.agent.SendUUID(ctx, params.SessionID, uuid, sent, blocks, synthetic); err != nil {
		return fail(err)
	}
	return nil
}

// Interrupt stops a thread's running turn.
func (c *Core) Interrupt(ctx context.Context, threadID string) error {
	c.mu.Lock()
	t := c.threads[threadID]
	if t == nil {
		c.mu.Unlock()
		return fmt.Errorf("no thread %s", threadID)
	}
	sid, queued := t.row.SessionID, t.row.Status == StatusQueued
	var ch Change
	if queued {
		ch = c.setStatusLocked(t, StatusInterrupted, "")
	}
	c.mu.Unlock()
	if queued {
		c.notify(ch)
		return nil
	}
	return c.agent.Interrupt(ctx, sid)
}

// Answer answers a pending question. answers is keyed by question text.
func (c *Core) Answer(threadID, toolUseID string, answers map[string]string) error {
	return c.resolveAsk(threadID, toolUseID, agent.AskResult{Answers: answers})
}

// Dismiss declines a pending question.
func (c *Core) Dismiss(threadID, toolUseID string) error {
	return c.resolveAsk(threadID, toolUseID, agent.AskResult{Cancelled: true})
}

func (c *Core) resolveAsk(threadID, toolUseID string, res agent.AskResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.threads[threadID]
	if t == nil {
		return fmt.Errorf("no thread %s", threadID)
	}
	ch := t.asks[toolUseID]
	if ch == nil {
		return errors.New("that question is no longer waiting for an answer")
	}
	delete(t.asks, toolUseID)
	delete(t.askQs, toolUseID)
	ch <- res
	return nil
}

// Changes lists the files a worker changed.
func (c *Core) Changes(ctx context.Context, threadID string) ([]git.FileChange, error) {
	t, ok := c.Thread(threadID)
	if !ok || t.WorktreePath == "" || t.BaseCommit == "" {
		return nil, errors.New("this thread has no worktree")
	}
	return git.Changes(ctx, t.WorktreePath, t.BaseCommit)
}

// Diff returns a worker's diff against where it started.
func (c *Core) Diff(ctx context.Context, threadID string) (string, error) {
	t, ok := c.Thread(threadID)
	if !ok || t.WorktreePath == "" || t.BaseCommit == "" {
		return "", errors.New("this thread has no worktree")
	}
	return git.Diff(ctx, t.WorktreePath, t.BaseCommit)
}

// Merge merges a worker's branch into its base branch in the repository's main checkout, committing any
// uncommitted work first, then removes its worktree and branch. It returns the merge commit.
func (c *Core) Merge(ctx context.Context, threadID string) (string, error) {
	v, ok := c.Thread(threadID)
	if !ok {
		return "", fmt.Errorf("no thread %s", threadID)
	}
	if !v.CanMerge {
		return "", fmt.Errorf("thread %q can't be merged while it is %s", v.Title, v.Status)
	}
	if _, err := git.CommitAll(ctx, v.WorktreePath, "chore: uncommitted work from "+v.Title); err != nil {
		return "", fmt.Errorf("committing the worktree's changes: %w", err)
	}
	commit, err := git.Merge(ctx, v.RepoPath, v.Branch, v.BaseBranch, fmt.Sprintf("Merge %s: %s", v.Branch, v.Title))
	if err != nil {
		c.addNotice(threadID, "error", "Merge failed: "+err.Error())
		return "", err
	}
	c.closeSession(ctx, threadID)
	if err := git.RemoveWorktree(ctx, v.RepoPath, v.WorktreePath, v.Branch, true); err != nil {
		c.log.Warn("core: removing merged worktree", "err", err)
	}
	c.mu.Lock()
	var ch Change
	if t := c.threads[threadID]; t != nil {
		t.row.WorktreePath = ""
		ch = c.setStatusLocked(t, StatusMerged, "")
	}
	c.mu.Unlock()
	short := commit
	if len(short) > 8 {
		short = short[:8]
	}
	c.addNotice(threadID, "info", fmt.Sprintf("Merged into %s (%s).", v.BaseBranch, short))
	c.notify(ch)
	c.schedule()
	return commit, nil
}

// Discard stops a worker and deletes its worktree and branch.
func (c *Core) Discard(ctx context.Context, threadID string) error {
	v, ok := c.Thread(threadID)
	if !ok {
		return fmt.Errorf("no thread %s", threadID)
	}
	if !v.CanDiscard {
		return fmt.Errorf("thread %q is already %s", v.Title, v.Status)
	}
	c.closeSession(ctx, threadID)
	if v.WorktreePath != "" || v.Branch != "" {
		if err := git.RemoveWorktree(ctx, v.RepoPath, v.WorktreePath, v.Branch, v.WorktreePath != ""); err != nil {
			c.log.Warn("core: discarding worktree", "err", err)
		}
	}
	c.mu.Lock()
	var ch Change
	if t := c.threads[threadID]; t != nil {
		t.row.WorktreePath = ""
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

// Done marks a thread done: it leaves the rail for History. A thread that finished without changes
// also loses its worktree and branch (Restore brings them back). Only threads with nothing to merge
// can be done; others are merged or discarded first.
func (c *Core) Done(ctx context.Context, threadID string) error {
	v, ok := c.Thread(threadID)
	if !ok {
		return fmt.Errorf("no thread %s", threadID)
	}
	if !v.CanDone {
		if v.Changed > 0 && !Finished(v.Status) {
			return fmt.Errorf("thread %q has changes; merge or discard it", v.Title)
		}
		return fmt.Errorf("thread %q can't be marked done while it is %s", v.Title, v.Status)
	}
	if v.WorktreePath != "" {
		c.closeSession(ctx, threadID)
		if err := git.RemoveWorktree(ctx, v.RepoPath, v.WorktreePath, v.Branch, true); err != nil {
			c.log.Warn("core: removing a done thread's worktree", "err", err)
		}
	}
	c.mu.Lock()
	t := c.threads[threadID]
	if t == nil {
		c.mu.Unlock()
		return fmt.Errorf("no thread %s", threadID)
	}
	var ch Change
	t.row.WorktreePath = ""
	t.row.DoneAt = now()
	if Finished(t.row.Status) {
		t.row.UpdatedAt = now()
		err := c.db.SaveThread(&t.row)
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

// Restore brings a done thread back to the rail. Its worktree is recreated at the same path, so its
// Claude session resumes: on its branch if that still exists, or on a new branch of the same name
// from the current head of the branch it started from.
func (c *Core) Restore(ctx context.Context, threadID string) error {
	v, ok := c.Thread(threadID)
	if !ok {
		return fmt.Errorf("no thread %s", threadID)
	}
	if !v.IsWorker || v.DoneAt.IsZero() {
		return fmt.Errorf("thread %q isn't in History", v.Title)
	}
	if _, err := os.Stat(v.RepoPath); err != nil {
		return fmt.Errorf("%s is gone: %w", v.RepoPath, err)
	}
	path := v.WorktreePath
	if path == "" {
		path = c.threadWorktreePath(v.Thread)
	}
	wt := git.Worktree{Path: path}
	if _, err := os.Stat(path); err != nil {
		base, err := git.LatestBase(ctx, v.RepoPath, v.BaseBranch)
		if err != nil {
			return err
		}
		if wt, err = git.RestoreWorktree(ctx, v.RepoPath, path, v.Branch, base.Ref); err != nil {
			return fmt.Errorf("recreating the worktree: %w", err)
		}
	}
	c.mu.Lock()
	t := c.threads[threadID]
	if t == nil {
		c.mu.Unlock()
		return fmt.Errorf("no thread %s", threadID)
	}
	t.row.WorktreePath, t.row.DoneAt, t.row.Error = wt.Path, time.Time{}, ""
	if wt.BaseCommit != "" {
		t.row.BaseCommit = wt.BaseCommit
	}
	t.changed, t.opened = -1, false
	ch := c.setStatusLocked(t, StatusIdle, "")
	c.mu.Unlock()
	c.addNotice(threadID, "info", "Restored from History.")
	c.notify(ch)
	go c.countChanges(threadID)
	return nil
}

// threadWorktreePath is where a worker's worktree lives. It depends only on the thread, so Restore
// finds the same path (and with it the Claude session, which is keyed by directory).
func (c *Core) threadWorktreePath(row store.Thread) string {
	return c.worktreePath(row.RepoPath, git.Slug(row.Title)+"-"+row.ID[:6])
}

const namingSystem = "You name git branches. Reply with only the branch name: no quotes, no explanation, no code formatting."

// nameBranch names a new worker's branch. With branch rules in Settings, the naming model follows
// them; otherwise, or when its answer isn't a usable name, the branch is solo/<title>-<id>.
func (c *Core) nameBranch(ctx context.Context, row store.Thread) string {
	fallback := "solo/" + git.Slug(row.Title) + "-" + row.ID[:6]
	s := c.Settings()
	rules := s.BranchRulesFor(row.RepoPath)
	if rules == "" {
		return fallback
	}
	prompt := fmt.Sprintf("Name the git branch for this task, following the rules.\n\nRules:\n%s\n\nRepository: %s\nTask title: %s\nTask:\n%s",
		rules, filepath.Base(row.RepoPath), row.Title, clipText(row.Prompt, 1500))
	nctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	text, err := c.agent.Complete(nctx, agent.CompleteParams{Model: s.NamingModel, Effort: s.NamingEffort,
		System: namingSystem, Prompt: prompt, ClaudePath: agent.FindClaude(s.ClaudePath).Path, TimeoutMs: 40_000})
	if err != nil {
		c.addNotice(row.ID, "warning", "Couldn't name the branch by your rules ("+err.Error()+"), so it's "+fallback+".")
		return fallback
	}
	name := cleanBranchName(text)
	if !git.ValidBranchName(ctx, row.RepoPath, name) {
		c.addNotice(row.ID, "warning", fmt.Sprintf("The suggested branch name %q isn't valid, so it's %s.", firstLine(text, 80), fallback))
		return fallback
	}
	for i, n := 2, name; ; i++ {
		if !git.BranchExists(ctx, row.RepoPath, n) {
			return n
		}
		n = fmt.Sprintf("%s-%d", name, i)
	}
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

// countChanges counts the files a worker's branch changes, for Done and CanMerge.
func (c *Core) countChanges(id string) {
	v, ok := c.Thread(id)
	if !ok || !v.IsWorker || v.WorktreePath == "" || v.BaseCommit == "" || Finished(v.Status) {
		return
	}
	if _, err := os.Stat(v.WorktreePath); err != nil {
		return // being removed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	files, err := git.Changes(ctx, v.WorktreePath, v.BaseCommit)
	if err != nil {
		c.log.Warn("core: counting changes", "thread", id, "err", err)
		return
	}
	c.mu.Lock()
	t := c.threads[id]
	if t == nil || t.changed == len(files) {
		c.mu.Unlock()
		return
	}
	t.changed = len(files)
	c.mu.Unlock()
	nv, _ := c.Thread(id)
	c.notify(Change{Kind: ChangeThread, ThreadID: id, PrevStatus: nv.Status, Status: nv.Status})
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
	if t == nil {
		c.mu.Unlock()
		return
	}
	sid := t.row.SessionID
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
	if err := c.db.SaveThread(&t.row); err != nil {
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
