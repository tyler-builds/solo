//go:build !js

package core

import (
	"context"
	"time"
)

// viewing is the thread a UI has open, and until when that holds without being said again.
type viewing struct {
	thread string
	until  time.Time // zero: until the UI says otherwise
}

// SetViewing records the thread a UI has open beside Solo's conversation, "" for none. viewer names the
// UI: the desktop window, or a paired device's session. A UI that can vanish without saying so (a
// device that stops renewing its watch is hidden or gone) passes until, when the record lapses; zero
// keeps it until it is set again.
func (c *Core) SetViewing(viewer, threadID string, until time.Time) {
	c.viewMu.Lock()
	defer c.viewMu.Unlock()
	if threadID == "" {
		delete(c.views, viewer)
		return
	}
	if c.views == nil {
		c.views = map[string]viewing{}
	}
	c.views[viewer] = viewing{thread: threadID, until: until}
}

// Viewing reports whether a UI has the thread open now: the user reads what it does there themselves.
func (c *Core) Viewing(threadID string) bool {
	c.viewMu.Lock()
	defer c.viewMu.Unlock()
	now := time.Now()
	for k, v := range c.views {
		if !v.until.IsZero() && now.After(v.until) {
			delete(c.views, k)
			continue
		}
		if v.thread == threadID {
			return true
		}
	}
	return false
}

// HasProposal reports whether Solo's conversation holds a merge card for the thread.
func (c *Core) HasProposal(threadID string) bool {
	items, err := c.db.ItemsByRef(context.Background(), OrchestratorID, ItemProposal, threadID)
	return err == nil && len(items) > 0
}
