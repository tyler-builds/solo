package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/store"
)

// OrchestratorID is the ID of the one orchestrator thread.
const OrchestratorID = "orchestrator"

// ItemProposal is a merge proposal the orchestrator made, shown as a card with a Merge button. Its
// data is ProposalData; Ref is the worker's thread ID. A thread has at most one: proposing it again,
// or its checks passing after they failed, removes the card and posts it anew at the end of the
// conversation, so the card the user can act on is always the latest one.
const ItemProposal = "proposal"

// ProposalData is a merge proposal.
type ProposalData struct {
	ThreadID string `json:"threadId"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	// ChecksFailed is set while the thread's pull request checks have failed since the card was
	// posted; when they pass again, the card is posted anew.
	ChecksFailed bool `json:"checksFailed,omitempty"`
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

// AddProposal records a merge proposal in the orchestrator's transcript, in place of any earlier one
// for the thread.
func (c *Core) AddProposal(workerID, summary string) error {
	v, ok := c.Thread(workerID)
	if !ok {
		return fmt.Errorf("no thread %s", workerID)
	}
	if v.NoChanges || (v.Changed == 0 && !Finished(v.Status)) {
		return fmt.Errorf("thread %q changed no files; there is nothing to merge", v.Title)
	}
	if v.Status == StatusBackground {
		return fmt.Errorf("thread %q is still waiting on work it runs in the background (%s); it carries on when that finishes, so propose landing after its next turn ends", v.Title, DescribeBackground(v.Background))
	}
	if !v.CanMerge && !v.CanOpenPR {
		return fmt.Errorf("thread %q can't be merged while it is %s", v.Title, v.Status)
	}
	failing := false
	for _, p := range v.Parts {
		failing = failing || (p.PR != nil && p.PR.State == forge.StateOpen && p.PR.Checks == forge.ChecksFailing)
	}
	c.mu.Lock()
	err := c.postProposalLocked(ProposalData{ThreadID: workerID, Title: v.Title, Summary: summary, ChecksFailed: failing})
	c.mu.Unlock()
	if err != nil {
		return err
	}
	c.notify(Change{Kind: ChangeItems, ThreadID: OrchestratorID})
	return nil
}

// postProposalLocked removes the thread's merge proposals and appends d as its one card.
func (c *Core) postProposalLocked(d ProposalData) error {
	old, err := c.db.ItemsByRef(context.Background(), OrchestratorID, ItemProposal, d.ThreadID)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(d)
	if err := c.db.AddItem(&store.Item{ThreadID: OrchestratorID, Kind: ItemProposal, Ref: d.ThreadID, Data: data, CreatedAt: now()}); err != nil {
		return err
	}
	ids := make([]int64, len(old))
	for i, it := range old {
		ids[i] = it.ID
	}
	return c.db.DeleteItems(ids...)
}

// proposalChecks keeps the thread's merge proposal current with its pull requests' checks: it notes
// on the card when they fail, and when they all pass again, posts the card anew at the end of the
// conversation, where the user sees it, in place of the one the conversation has scrolled past.
func (c *Core) proposalChecks(threadID, event string) {
	if event != PRChecksFailed && event != PRChecksPassed {
		return
	}
	c.mu.Lock()
	changed, err := c.proposalChecksLocked(threadID, event)
	c.mu.Unlock()
	if err != nil {
		c.log.Error("core: updating a merge proposal", "thread", threadID, "err", err)
	}
	if changed {
		c.notify(Change{Kind: ChangeItems, ThreadID: OrchestratorID})
	}
}

func (c *Core) proposalChecksLocked(threadID, event string) (bool, error) {
	t := c.threads[threadID]
	if t == nil {
		return false, nil
	}
	items, err := c.db.ItemsByRef(context.Background(), OrchestratorID, ItemProposal, threadID)
	if err != nil || len(items) == 0 {
		return false, err
	}
	last := items[len(items)-1]
	var d ProposalData
	if err := json.Unmarshal(last.Data, &d); err != nil {
		return false, err
	}
	if event == PRChecksFailed {
		if d.ChecksFailed {
			return false, nil
		}
		d.ChecksFailed = true
		data, _ := json.Marshal(d)
		return false, c.db.UpdateItemData(last.ID, data)
	}
	if !d.ChecksFailed {
		return false, nil // never failed: the card is as it was posted
	}
	for _, p := range t.parts {
		if p.prOpen() && p.PR.Checks == forge.ChecksFailing {
			return false, nil // another repository's checks still fail
		}
	}
	d.ChecksFailed = false
	d.Title = t.row.Title
	return true, c.postProposalLocked(d)
}

// ResetSession starts the thread on a new Claude session the next time it is used. The transcript
// shown in Solo is kept.
func (c *Core) ResetSession(ctx context.Context, threadID string) error {
	return c.resetSession(ctx, threadID, "Started a new conversation with Claude.", false)
}

// RotateSession moves the thread to a fresh Claude session, as ResetSession does, without a notice: the
// user sees one conversation throughout. It doesn't rotate while a message is on its way to Claude.
func (c *Core) RotateSession(ctx context.Context, threadID string) error {
	return c.resetSession(ctx, threadID, "", true)
}

// resetSession closes the thread's session and moves it to a new session ID. It holds the thread's
// session lock throughout, so a message being sent meanwhile goes wholly to the old session (before)
// or the new one (after), never opens the old session again after it closed, and never leaves the
// new one marked open when it isn't.
func (c *Core) resetSession(ctx context.Context, threadID, notice string, quiet bool) error {
	c.mu.Lock()
	t := c.threads[threadID]
	c.mu.Unlock()
	if t == nil {
		return fmt.Errorf("no thread %s", threadID)
	}
	t.sess.Lock()
	defer t.sess.Unlock()
	c.mu.Lock()
	// Running outside a turn: a message was just sent, and its turn hasn't started yet.
	if t.inTurn || (quiet && t.row.Status == StatusRunning) {
		c.mu.Unlock()
		return errors.New("wait for the current turn to finish, or interrupt it")
	}
	c.mu.Unlock()
	c.closeSessionHeld(ctx, t)
	c.mu.Lock()
	delete(c.bySession, t.row.SessionID)
	t.row.SessionID = agent.NewUUID()
	t.row.HasTranscript = false
	t.row.ContextTokens = 0
	t.row.CostBase = t.row.CostUSD // the thread's cost runs on across sessions
	t.opened = false
	c.bySession[t.row.SessionID] = t
	var ch Change
	if t.row.Status != StatusRunning { // running: a message waits to go to the new session
		ch = c.setStatusLocked(t, StatusIdle, "")
	}
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
