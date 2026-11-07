package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/store"
)

// Thread statuses.
const (
	StatusQueued      = "queued"      // waiting for a free slot (Settings.MaxConcurrent)
	StatusStarting    = "starting"    // creating its worktree and session
	StatusRunning     = "running"     // a turn is in progress
	StatusWaiting     = "waiting"     // asked the user a question
	StatusIdle        = "idle"        // its last turn finished; ready for review or more instructions
	StatusFailed      = "failed"      // its last turn, or its start, failed
	StatusInterrupted = "interrupted" // stopped by the user, or by Solo closing
	StatusMerged      = "merged"      // its branch was merged and its worktree removed
	StatusDiscarded   = "discarded"   // its worktree and branch were deleted
)

// Active reports whether a thread with the status takes up a worker slot.
func Active(status string) bool {
	return status == StatusStarting || status == StatusRunning || status == StatusWaiting
}

// Finished reports whether a thread with the status is closed for good.
func Finished(status string) bool { return status == StatusMerged || status == StatusDiscarded }

// thread is a thread's state. Guarded by Core.mu.
type thread struct {
	row     store.Thread
	opened  bool // its session is registered with the agent manager
	inTurn  bool
	live    []liveKey // streaming blocks, in order
	liveTxt map[liveKey]*LiveBlock
	model   string // the model Claude reports
	retry   string // the latest API retry, while it lasts
	asks    map[string]chan agent.AskResult
	tasks   map[string]int64 // task id → item id
	tools   map[string]int64 // tool_use id → item id
}

type liveKey struct {
	message string
	index   int
}

// ThreadView is a snapshot of a thread.
type ThreadView struct {
	store.Thread
	InTurn       bool
	ActualModel  string
	Retry        string
	PendingAsk   bool
	Live         []LiveBlock
	IsWorker     bool
	HasWorktree  bool
	CanMerge     bool
	CanDiscard   bool
	CanInterrupt bool
}

func (t *thread) view() ThreadView {
	v := ThreadView{
		Thread:      t.row,
		InTurn:      t.inTurn,
		ActualModel: t.model,
		Retry:       t.retry,
		PendingAsk:  len(t.asks) > 0,
		IsWorker:    t.row.Kind == store.KindWorker,
		HasWorktree: t.row.WorktreePath != "",
	}
	for _, k := range t.live {
		v.Live = append(v.Live, *t.liveTxt[k])
	}
	v.CanInterrupt = t.inTurn
	v.CanMerge = v.IsWorker && v.HasWorktree && !Finished(t.row.Status) && !Active(t.row.Status) && t.row.Status != StatusQueued
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
	Model  string // empty: Settings.WorkerModel
	Base   string // branch to start from; empty: the repository's current branch
}

// CreateWorker creates a worker thread. It starts at once if a slot is free, or waits as queued.
func (c *Core) CreateWorker(ctx context.Context, spec WorkerSpec) (ThreadView, error) {
	repo, err := c.ResolveRepo(spec.Repo)
	if err != nil {
		return ThreadView{}, err
	}
	if strings.TrimSpace(spec.Prompt) == "" {
		return ThreadView{}, errors.New("a worker needs a prompt")
	}
	title := strings.TrimSpace(spec.Title)
	if title == "" {
		title = firstLine(spec.Prompt, 60)
	}
	id := agent.NewUUID()
	slug := git.Slug(title) + "-" + id[:6]
	ts := now()
	t := &thread{row: store.Thread{
		ID:         id,
		Kind:       store.KindWorker,
		SessionID:  agent.NewUUID(),
		Title:      title,
		RepoPath:   repo,
		Branch:     "solo/" + slug,
		BaseBranch: spec.Base,
		Model:      spec.Model,
		Status:     StatusQueued,
		Prompt:     spec.Prompt,
		CreatedAt:  ts,
		UpdatedAt:  ts,
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

// schedule starts queued workers while slots are free, oldest first.
func (c *Core) schedule() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	active := 0
	var queued []*thread
	for _, t := range c.threads {
		if t.row.Kind != store.KindWorker {
			continue
		}
		if Active(t.row.Status) {
			active++
		} else if t.row.Status == StatusQueued {
			queued = append(queued, t)
		}
	}
	slices.SortFunc(queued, func(a, b *thread) int { return a.row.CreatedAt.Compare(b.row.CreatedAt) })
	var start []*thread
	var changes []Change
	for _, t := range queued {
		if active >= c.settings.MaxConcurrent {
			break
		}
		active++
		changes = append(changes, c.setStatusLocked(t, StatusStarting, ""))
		start = append(start, t)
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
		path := c.worktreePath(row.RepoPath, strings.TrimPrefix(row.Branch, "solo/"))
		wt, err := git.AddWorktree(ctx, row.RepoPath, path, row.Branch, row.BaseBranch)
		if err != nil {
			fail(err)
			return
		}
		c.mu.Lock()
		t.row.WorktreePath, t.row.BaseBranch, t.row.BaseCommit = wt.Path, wt.BaseBranch, wt.BaseCommit
		t.row.UpdatedAt = now()
		err = c.db.SaveThread(&t.row)
		c.mu.Unlock()
		if err != nil {
			fail(err)
			return
		}
		c.addNotice(id, "info", fmt.Sprintf("Working on branch %s from %s.", wt.Branch, wt.BaseBranch))
	}
	if err := c.send(ctx, id, row.Prompt, false); err != nil {
		fail(err)
	}
}

// openParams builds the session parameters for a thread.
func (c *Core) openParams(t *thread) agent.OpenParams {
	if t.row.Kind == store.KindOrchestrator && c.orchSpec != nil {
		p := c.orchSpec()
		p.SessionID, p.Resume = t.row.SessionID, t.row.HasTranscript
		if t.row.Model != "" {
			p.Model = t.row.Model
		}
		return p
	}
	return agent.OpenParams{
		SessionID:      t.row.SessionID,
		Resume:         t.row.HasTranscript,
		Cwd:            t.row.WorktreePath,
		Model:          t.row.Model,
		Title:          t.row.Title,
		SystemAppend:   workerInstructions(t.row),
		PermissionMode: agent.PermissionBypass,
		ToolPolicy:     agent.ToolPolicyAllow,
		ClaudePath:     agent.FindClaude(c.settings.ClaudePath),
	}
}

func workerInstructions(r store.Thread) string {
	return fmt.Sprintf(`You are a worker thread in Solo, an app where an orchestrator agent delegates coding tasks.
You work alone in a git worktree of %s on branch %s, created from %s. Other workers may be changing
other branches of the same repository at the same time; stay on your branch and in this worktree.

- Do the task you were given completely, verifying your work (build, tests) where the project allows.
- When you are done, commit your changes on this branch with a Conventional Commit message. Don't push,
  merge, rebase onto other branches, or switch branches; the user reviews and merges your branch.
- End with a short summary of what you changed and anything left undone, for the user and the orchestrator.
- If you need a decision only the user can make, ask with AskUserQuestion.`, r.RepoPath, r.Branch, r.BaseBranch)
}

// Send sends the user's message to a thread, starting its session if needed.
func (c *Core) Send(ctx context.Context, threadID, text string) error {
	return c.send(ctx, threadID, text, false)
}

// SendSynthetic sends text the app composed (for example a thread update to the orchestrator).
func (c *Core) SendSynthetic(ctx context.Context, threadID, text string) error {
	return c.send(ctx, threadID, text, true)
}

func (c *Core) send(ctx context.Context, threadID, text string, synthetic bool) error {
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
	params := c.openParams(t)
	c.mu.Unlock()

	if needOpen {
		if err := c.agent.Open(ctx, params); err != nil {
			return err
		}
		c.mu.Lock()
		t.opened = true
		c.mu.Unlock()
	}
	uuid, err := c.agent.Send(ctx, params.SessionID, text, synthetic)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(UserData{Text: text, Synthetic: synthetic})
	c.mu.Lock()
	it := &store.Item{ThreadID: threadID, Kind: ItemUser, TurnUUID: uuid, Data: data, CreatedAt: now()}
	err = c.db.AddItem(it)
	var ch Change
	if t.row.Status != StatusRunning && t.row.Status != StatusWaiting {
		ch = c.setStatusLocked(t, StatusRunning, "")
	}
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeItems, ThreadID: threadID}, ch)
	return err
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
		ch = c.setStatusLocked(t, StatusDiscarded, "")
	}
	c.mu.Unlock()
	c.addNotice(threadID, "info", "Discarded.")
	c.notify(ch)
	c.schedule()
	return nil
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
