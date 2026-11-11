package core

import (
	"testing"

	"github.com/tyler-builds/solo/internal/agent"
)

func used(f float64) *float64 { return &f }

func rateEvent(kind, status string, u *float64) agent.Event {
	return agent.Event{Kind: agent.EventRateLimit, Status: status, RateLimitType: kind, Utilization: u, ResetsAt: 1_900_000_000}
}

// Each kind of limit keeps its own latest event, and they come back in a fixed order.
func TestRateLimitsPerKind(t *testing.T) {
	e := newEnv(t)
	if got := e.c.RateLimits(); len(got) != 0 {
		t.Fatalf("no events yet, got %v", got)
	}
	e.c.onEvent(rateEvent("seven_day", "allowed", used(0.18)))
	e.c.onEvent(rateEvent("five_hour", "allowed", used(0.5)))
	e.c.onEvent(rateEvent("five_hour", "allowed_warning", used(0.72)))
	e.c.onEvent(rateEvent("seven_day_opus", "allowed", used(0.05)))

	got := e.c.RateLimits()
	want := []struct {
		kind, status string
		used         float64
	}{{"five_hour", "allowed_warning", 0.72}, {"seven_day", "allowed", 0.18}, {"seven_day_opus", "allowed", 0.05}}
	if len(got) != len(want) {
		t.Fatalf("got %d limits, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		u, ok := UsedFraction(got[i])
		if got[i].RateLimitType != w.kind || got[i].Status != w.status || !ok || u != w.used {
			t.Errorf("limit %d = %s %s %v, want %s %s %v", i, got[i].RateLimitType, got[i].Status, u, w.kind, w.status, w.used)
		}
	}
}

// An event that doesn't say how much is used keeps the last known utilization of its kind.
func TestRateLimitKeepsKnownUtilization(t *testing.T) {
	e := newEnv(t)
	e.c.onEvent(rateEvent("five_hour", "allowed", used(0.4)))
	e.c.onEvent(rateEvent("five_hour", "rejected", nil))
	got := e.c.RateLimits()
	if len(got) != 1 || got[0].Status != "rejected" {
		t.Fatalf("got %v", got)
	}
	if u, ok := UsedFraction(got[0]); !ok || u != 0.4 {
		t.Errorf("utilization = %v %v, want 0.4", u, ok)
	}
}

func TestUsedFraction(t *testing.T) {
	for _, c := range []struct {
		in   *float64
		want float64
		ok   bool
	}{
		{nil, 0, false},
		{used(0), 0, true},
		{used(0.72), 0.72, true},
		{used(1), 1, true},
		{used(72), 0.72, true}, // a percent
		{used(140), 1, true},
		{used(-0.1), 0, true},
	} {
		got, ok := UsedFraction(agent.Event{Utilization: c.in})
		if ok != c.ok || got != c.want {
			t.Errorf("UsedFraction(%v) = %v %v, want %v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
