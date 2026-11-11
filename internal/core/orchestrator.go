package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/store"
)

// OrchestratorID is the ID of the one orchestrator thread.
const OrchestratorID = "orchestrator"

// ItemProposal is a merge proposal the orchestrator made, shown as a card with a Merge button. Its
// data is ProposalData; Ref is the worker's thread ID.
const ItemProposal = "proposal"

// ProposalData is a merge proposal.
type ProposalData struct {
	ThreadID string `json:"threadId"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
}

// SetOrchestratorSpec sets how the orchestrator's session is opened. Core fills in the session ID and
// resume flag.
func (c *Core) SetOrchestratorSpec(f func() agent.OpenParams) {
	c.mu.Lock()
	c.orchSpec = f
	c.mu.Unlock()
}

// SetRecap sets how a thread whose saved conversation was lost is caught up: the returned text starts
// the fresh session's first message. When f returns "", the default restates a worker's task and last
// summary.
func (c *Core) SetRecap(f func(t ThreadView) string) {
	c.mu.Lock()
	c.recap = f
	c.mu.Unlock()
}

// EnsureOrchestrator creates the orchestrator thread if it doesn't exist, and returns it.
func (c *Core) EnsureOrchestrator() (ThreadView, error) {
	c.mu.Lock()
	if t := c.threads[OrchestratorID]; t != nil {
		v := t.view()
		c.mu.Unlock()
		return v, nil
	}
	ts := now()
	t := &thread{row: store.Thread{
		ID:        OrchestratorID,
		Kind:      store.KindOrchestrator,
		SessionID: agent.NewUUID(),
		Title:     "Orchestrator",
		Status:    StatusIdle,
		CreatedAt: ts,
		UpdatedAt: ts,
	}}
	if err := c.db.SaveThread(&t.row); err != nil {
		c.mu.Unlock()
		return ThreadView{}, err
	}
	c.threads[t.row.ID] = t
	c.bySession[t.row.SessionID] = t
	v := t.view()
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeThread, ThreadID: OrchestratorID, Status: StatusIdle})
	return v, nil
}

// FindThread finds a thread by ID or by a unique prefix of at least 4 characters of its ID.
func (c *Core) FindThread(idOrPrefix string) (ThreadView, error) {
	idOrPrefix = strings.TrimSpace(idOrPrefix)
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.threads[idOrPrefix]; t != nil {
		return t.view(), nil
	}
	if len(idOrPrefix) < 4 {
		return ThreadView{}, fmt.Errorf("no thread %q", idOrPrefix)
	}
	var found []*thread
	for id, t := range c.threads {
		if strings.HasPrefix(id, idOrPrefix) {
			found = append(found, t)
		}
	}
	switch len(found) {
	case 1:
		return found[0].view(), nil
	case 0:
		return ThreadView{}, fmt.Errorf("no thread %q", idOrPrefix)
	default:
		return ThreadView{}, fmt.Errorf("%q matches %d threads; use more of the ID", idOrPrefix, len(found))
	}
}

// PendingQuestion is a question a thread is waiting on.
type PendingQuestion struct {
	ToolUseID string
	Questions []agent.Question
}

// PendingQuestions returns the questions a thread is waiting on.
func (c *Core) PendingQuestions(threadID string) []PendingQuestion {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.threads[threadID]
	if t == nil {
		return nil
	}
	var out []PendingQuestion
	for id, qs := range t.askQs {
		out = append(out, PendingQuestion{ToolUseID: id, Questions: qs})
	}
	slices.SortFunc(out, func(a, b PendingQuestion) int { return strings.Compare(a.ToolUseID, b.ToolUseID) })
	return out
}

// AddProposal records a merge proposal in the orchestrator's transcript.
func (c *Core) AddProposal(workerID, summary string) error {
	v, ok := c.Thread(workerID)
	if !ok {
		return fmt.Errorf("no thread %s", workerID)
	}
	if v.NoChanges || (v.Changed == 0 && !Finished(v.Status)) {
		return fmt.Errorf("thread %q changed no files; there is nothing to merge", v.Title)
	}
	if !v.CanMerge && !v.CanOpenPR {
		return fmt.Errorf("thread %q can't be merged while it is %s", v.Title, v.Status)
	}
	data, _ := json.Marshal(ProposalData{ThreadID: workerID, Title: v.Title, Summary: summary})
	c.mu.Lock()
	err := c.db.AddItem(&store.Item{ThreadID: OrchestratorID, Kind: ItemProposal, Ref: workerID, Data: data, CreatedAt: now()})
	c.mu.Unlock()
	if err != nil {
		return err
	}
	c.notify(Change{Kind: ChangeItems, ThreadID: OrchestratorID})
	return nil
}

// ResetSession starts the thread on a new Claude session the next time it is used. The transcript
// shown in Solo is kept.
func (c *Core) ResetSession(ctx context.Context, threadID string) error {
	return c.resetSession(ctx, threadID, "Started a new conversation with Claude.")
}

// RotateSession moves the thread to a fresh Claude session, as ResetSession does, without a notice: the
// user sees one conversation throughout.
func (c *Core) RotateSession(ctx context.Context, threadID string) error {
	return c.resetSession(ctx, threadID, "")
}

func (c *Core) resetSession(ctx context.Context, threadID, notice string) error {
	c.mu.Lock()
	t := c.threads[threadID]
	if t == nil {
		c.mu.Unlock()
		return fmt.Errorf("no thread %s", threadID)
	}
	if t.inTurn {
		c.mu.Unlock()
		return errors.New("wait for the current turn to finish, or interrupt it")
	}
	c.mu.Unlock()
	c.closeSession(ctx, threadID)
	c.mu.Lock()
	delete(c.bySession, t.row.SessionID)
	t.row.SessionID = agent.NewUUID()
	t.row.HasTranscript = false
	t.row.ContextTokens = 0
	t.row.CostBase = t.row.CostUSD // the thread's cost runs on across sessions
	c.bySession[t.row.SessionID] = t
	ch := c.setStatusLocked(t, StatusIdle, "")
	c.mu.Unlock()
	if notice != "" {
		c.addNotice(threadID, "info", notice)
	}
	c.notify(ch)
	return nil
}

// isLostSession reports whether a turn failed because Claude's saved conversation is gone.
func isLostSession(errText string) bool {
	return strings.Contains(errText, "No conversation found with session ID")
}

// recoverLostSession moves a thread whose saved conversation is gone to a new session, and resends the
// message that failed, prefixed with a recap.
func (c *Core) recoverLostSession(threadID, failedUUID string) {
	ctx := context.Background()
	items, err := c.db.Items(ctx, threadID, 50)
	if err != nil {
		return
	}
	var text string
	for _, it := range items {
		if it.Kind == ItemUser && it.TurnUUID == failedUUID {
			var d UserData
			_ = json.Unmarshal(it.Data, &d)
			text = d.Text
		}
	}
	if err := c.ResetSession(ctx, threadID); err != nil {
		c.log.Warn("core: resetting lost session", "err", err)
		return
	}
	v, _ := c.Thread(threadID)
	c.mu.Lock()
	recap := c.recap
	c.mu.Unlock()
	var r string
	if recap != nil {
		r = recap(v)
	}
	if r == "" {
		r = defaultRecap(v)
	}
	if text == "" {
		return
	}
	if err := c.SendSyntheticWithContext(ctx, threadID, r, text); err != nil {
		c.addNotice(threadID, "error", "Could not restart the conversation: "+err.Error())
	}
}

func defaultRecap(v ThreadView) string {
	if v.Kind != store.KindWorker {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Solo: your earlier conversation for this thread was lost, so this is a fresh session in the same worktree.]\n")
	fmt.Fprintf(&b, "Your original task:\n%s\n", v.Prompt)
	if v.Summary != "" {
		fmt.Fprintf(&b, "\nYour last summary:\n%s\n", v.Summary)
	}
	b.WriteString("\nCheck `git status` and `git log` to see what you already did.")
	return b.String()
}
