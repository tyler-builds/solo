package core

import (
	"slices"
	"strings"

	"github.com/tyler-builds/solo/internal/agent"
)

// rateLimitOrder is the order the kinds of limit show in: the five-hour window first, then the weekly
// ones. Kinds not listed follow, by name.
var rateLimitOrder = []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet",
	"seven_day_overage_included", "overage"}

// setRateLimit keeps a rate limit event as the latest of its kind. An event that doesn't say how much of
// the limit is used keeps the last known utilization.
func (c *Core) setRateLimit(e agent.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rateLimits == nil {
		c.rateLimits = map[string]agent.Event{}
	}
	if prev, ok := c.rateLimits[e.RateLimitType]; ok && e.Utilization == nil {
		e.Utilization = prev.Utilization
	}
	c.rateLimits[e.RateLimitType] = e
}

// RateLimits returns the latest rate limit event of each kind of limit Claude has reported, in
// rateLimitOrder.
func (c *Core) RateLimits() []agent.Event {
	c.mu.Lock()
	out := make([]agent.Event, 0, len(c.rateLimits))
	for _, e := range c.rateLimits {
		out = append(out, e)
	}
	c.mu.Unlock()
	rank := func(kind string) int {
		if i := slices.Index(rateLimitOrder, kind); i >= 0 {
			return i
		}
		return len(rateLimitOrder)
	}
	slices.SortFunc(out, func(a, b agent.Event) int {
		if d := rank(a.RateLimitType) - rank(b.RateLimitType); d != 0 {
			return d
		}
		return strings.Compare(a.RateLimitType, b.RateLimitType)
	})
	return out
}

// UsedFraction is how much of a limit an event says is used, from 0 to 1, and whether it says. Claude
// Code reports a fraction; a value above 1 is taken as a percent.
func UsedFraction(e agent.Event) (float64, bool) {
	if e.Utilization == nil {
		return 0, false
	}
	u := *e.Utilization
	if u > 1 {
		u /= 100
	}
	return min(max(u, 0), 1), true
}
