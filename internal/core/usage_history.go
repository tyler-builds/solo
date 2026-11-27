//go:build !js

package core

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/store"
)

// usageWeek is how far back the Usage page looks.
const usageWeek = 7 * 24 * time.Hour

// recordReadingsLocked keeps the readings of a rate limit event for the Usage page: one for each
// window it lists, or the one its top level describes when it lists none. Call with c.mu held.
func (c *Core) recordReadingsLocked(e agent.Event) {
	add := func(kind string, used, resetsAt float64) {
		r := store.UsageReading{At: now(), Kind: kind, Used: used}
		if resetsAt > 0 {
			r.ResetsAt = time.Unix(int64(resetsAt), 0)
		}
		if err := c.db.AddUsageReading(r); err != nil {
			c.log.Warn("core: saving a usage reading", "err", err)
		}
	}
	for _, w := range e.Windows {
		add(w.RateLimitType, w.Utilization, w.ResetsAt)
	}
	if len(e.Windows) == 0 && e.Utilization != nil && e.RateLimitType != "" {
		add(e.RateLimitType, *e.Utilization, e.ResetsAt)
	}
}

// recordTurnLocked keeps what a thread's turn cost for the Usage page. A card's leads count toward
// its thread, the builder. Call with c.mu held.
func (c *Core) recordTurnLocked(t *thread, cost float64) {
	if cost <= 0 {
		return
	}
	owner := t
	if cs := t.card(); t.row.Kind == store.KindLead && cs != nil && cs.Builder != "" {
		if b := c.threads[cs.Builder]; b != nil {
			owner = b
		}
	}
	if err := c.db.AddUsageTurn(store.UsageTurn{At: now(), ThreadID: owner.row.ID, Title: owner.row.Title, CostUSD: cost}); err != nil {
		c.log.Warn("core: saving a turn's cost", "err", err)
	}
}

// Usage returns the usage history the Usage page draws: the last week's readings of each limit, and
// what each thread's turns cost since the weekly limit's window began.
func (c *Core) Usage(ctx context.Context) (UsageReport, error) {
	weekAgo := now().Add(-usageWeek)
	readings, err := c.db.UsageReadings(ctx, weekAgo)
	if err != nil {
		return UsageReport{}, err
	}
	rep := UsageReport{Since: weekAgo, Readings: make([]UsageReading, len(readings))}
	for i, r := range readings {
		rep.Readings[i] = UsageReading{At: r.At, Kind: r.Kind, Used: min(max(r.Used, 0), 1), ResetsAt: r.ResetsAt}
		if r.Kind == "seven_day" && !r.ResetsAt.IsZero() {
			rep.Since = r.ResetsAt.Add(-usageWeek)
		}
	}
	turns, err := c.db.UsageTurns(ctx, rep.Since)
	if err != nil {
		return UsageReport{}, err
	}
	byID := map[string]*UsageThread{}
	for _, u := range turns {
		ut := byID[u.ThreadID]
		if ut == nil {
			ut = &UsageThread{ThreadID: u.ThreadID}
			byID[u.ThreadID] = ut
		}
		ut.Title = u.Title // the latest, as the thread may have been renamed
		ut.CostUSD += u.CostUSD
		ut.Turns++
	}
	c.mu.Lock()
	for id, ut := range byID {
		t := c.threads[id]
		switch {
		case t == nil:
		case id == OrchestratorID:
			ut.Kind, ut.Title = UsageKindSolo, "Solo"
		case t.row.Card != "":
			ut.Kind, ut.Title = UsageKindCard, t.row.Title
		default:
			ut.Kind, ut.Title = UsageKindThread, t.row.Title
		}
		rep.Threads = append(rep.Threads, *ut)
	}
	c.mu.Unlock()
	slices.SortFunc(rep.Threads, func(a, b UsageThread) int {
		return cmp.Or(cmp.Compare(b.CostUSD, a.CostUSD), cmp.Compare(a.ThreadID, b.ThreadID))
	})
	return rep, nil
}
