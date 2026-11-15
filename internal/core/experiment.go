//go:build !js

package core

import (
	"context"
	"fmt"

	"github.com/tyler-builds/solo/internal/store"
)

// SetExperiment turns a worker's experiment mode on or off for the user (store.Thread.Experiment). An
// experiment is a thread the user started only to try or test something, with no intention of landing
// it: Solo proposes no merge for it, the app doesn't count it as needing the user for unlanded changes,
// and it reads as finished when its turn ends. It works as any thread does otherwise: its worktree,
// branch and commits, and what Done and Discard do with them, are unchanged, and the user can still
// land it. Turning it on removes the thread's merge card from Solo's conversation.
func (c *Core) SetExperiment(_ context.Context, threadID string, on bool) error {
	_, err := c.setExperiment(threadID, on, FromUser)
	return err
}

// SetExperimentFromOrchestrator is SetExperiment for Solo's tools, which say what the thread is now.
func (c *Core) SetExperimentFromOrchestrator(threadID string, on bool) (ThreadView, error) {
	return c.setExperiment(threadID, on, FromOrchestrator)
}

func (c *Core) setExperiment(threadID string, on bool, from string) (ThreadView, error) {
	c.mu.Lock()
	t := c.threads[threadID]
	if t == nil || t.row.Kind != store.KindWorker {
		c.mu.Unlock()
		return ThreadView{}, fmt.Errorf("no worker thread %s", threadID)
	}
	if t.row.Experiment == on {
		v := c.viewLocked(t)
		c.mu.Unlock()
		return v, nil
	}
	t.row.Experiment = on
	err := c.saveLocked(t)
	removed := false
	if err == nil && on {
		removed, err = c.removeProposalsLocked(threadID)
	}
	v := c.viewLocked(t)
	c.mu.Unlock()
	changes := []Change{{Kind: ChangeThread, ThreadID: threadID, PrevStatus: v.Status, Status: v.Status,
		ExperimentSet: true, ExperimentBy: from}}
	if removed {
		changes = append(changes, Change{Kind: ChangeItems, ThreadID: OrchestratorID})
	}
	c.notify(changes...)
	return v, err
}

// removeProposalsLocked deletes the thread's merge cards from Solo's conversation, reporting whether
// there were any.
func (c *Core) removeProposalsLocked(threadID string) (bool, error) {
	old, err := c.db.ItemsByRef(context.Background(), OrchestratorID, ItemProposal, threadID)
	if err != nil || len(old) == 0 {
		return false, err
	}
	ids := make([]int64, len(old))
	for i, it := range old {
		ids[i] = it.ID
	}
	return true, c.db.DeleteItems(ids...)
}

// experimentError is why AddProposal refuses an experiment thread.
func experimentError(v ThreadView) error {
	return fmt.Errorf("thread %q is an experiment: the user started it to try or test something and doesn't mean to land it, "+
		"so there is nothing to propose. Report what it found or did instead", v.Title)
}
