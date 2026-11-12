package gui

import (
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/agent"
)

func fraction(f float64) *float64 { return &f }

// demoLimits are usage limits as Claude Code reports them: one near its limit, two well under.
func demoLimits() []agent.Event {
	soon := float64(time.Now().Add(2*time.Hour + 14*time.Minute).Unix())
	week := float64(time.Now().Add(4 * 24 * time.Hour).Unix())
	return []agent.Event{
		{Kind: agent.EventRateLimit, RateLimitType: "five_hour", Status: "allowed_warning", Utilization: fraction(0.84), ResetsAt: soon},
		{Kind: agent.EventRateLimit, RateLimitType: "seven_day", Status: "allowed", Utilization: fraction(0.18), ResetsAt: week},
		{Kind: agent.EventRateLimit, RateLimitType: "seven_day_opus", Status: "allowed", Utilization: fraction(0.05), ResetsAt: week},
	}
}

// Each known limit gets its own bar at the foot of the rail, and no notice.
func TestUsageBars(t *testing.T) {
	f := demoBackend()
	f.rate = demoLimits()
	_, u := newUI(t, f)
	for _, k := range []string{"usage-five_hour", "usage-seven_day", "usage-seven_day_opus"} {
		if !u.Has(k) {
			t.Errorf("no bar %s", k)
		}
	}
	if u.Has("usage-notice") {
		t.Error("a notice beside the bars")
	}
}

// With no utilization known, a warning still shows, as a notice in the same spot.
func TestUsageNoticeWithoutUtilization(t *testing.T) {
	f := demoBackend()
	f.rate = []agent.Event{{Kind: agent.EventRateLimit, RateLimitType: "five_hour", Status: "rejected"}}
	_, u := newUI(t, f)
	if !u.Has("usage-notice") {
		t.Fatal("no notice")
	}
	if u.Has("usage-five_hour") {
		t.Error("a bar with nothing known")
	}
}

// Nothing shows while every limit is fine and nothing says how much is used.
func TestUsageQuietWhenAllowed(t *testing.T) {
	f := demoBackend()
	f.rate = []agent.Event{{Kind: agent.EventRateLimit, RateLimitType: "five_hour", Status: "allowed"}}
	_, u := newUI(t, f)
	if u.Has("usage-notice") || u.Has("usage-five_hour") {
		t.Error("usage shown")
	}
}

func TestUsageTip(t *testing.T) {
	now := time.Date(2026, 11, 11, 13, 0, 0, 0, time.Local)
	e := agent.Event{RateLimitType: "seven_day_opus", Status: "allowed_warning",
		ResetsAt: float64(now.Add(90 * time.Minute).Unix())}
	got := usageTip(e, 0.72, true, now)
	want := "Weekly Opus limit: 72% used\nNear the limit\nResets at 2:30 PM (in 1h 30m)"
	if got != want {
		t.Errorf("usageTip = %q, want %q", got, want)
	}
	if r := resetLine(float64(now.Add(3*24*time.Hour).Unix()), now); !strings.HasPrefix(r, "Resets Sat Nov 14") {
		t.Errorf("resetLine = %q", r)
	}
}

// The server marks a limit allowed_warning once usage passes any of its warning thresholds, 25% among
// them. A quarter of the weekly limit used is not near it: the bar keeps the accent color and the tooltip
// says nothing about being close. From the share Claude Code warns at, the server's warning shows.
func TestUsageWarnsByAmountUsed(t *testing.T) {
	_, th := Themes()
	now := time.Date(2026, 11, 11, 13, 0, 0, 0, time.Local)
	early := agent.Event{RateLimitType: "seven_day", Status: "allowed_warning", Utilization: fraction(0.25)}
	if c := usageColor(th, early, 0.25); c != th.Accent {
		t.Errorf("25%% used, warned: color %v, want the accent", c)
	}
	if tip := usageTip(early, 0.25, true, now); tip != "Weekly limit: 25% used" {
		t.Errorf("25%% used, warned: tip %q", tip)
	}

	cases := []struct {
		status string
		used   float64
		want   ui.Color
	}{
		{"allowed", 0.25, th.Accent},
		{"allowed_warning", 0.5, th.Accent},
		{"allowed_warning", 0.7, th.Warning},
		{"allowed", 0.75, th.Accent},
		{"allowed", 0.8, th.Warning},
		{"rejected", 0.25, th.Danger},
		{"rejected", 1, th.Danger},
	}
	for _, c := range cases {
		e := agent.Event{RateLimitType: "seven_day", Status: c.status}
		if got := usageColor(th, e, c.used); got != c.want {
			t.Errorf("%s at %v: color %v, want %v", c.status, c.used, got, c.want)
		}
	}
}

// A weekly limit a quarter used, which the server marks allowed_warning, shows a bar and no notice.
func TestUsageBarQuarterUsedWeekly(t *testing.T) {
	f := demoBackend()
	f.rate = []agent.Event{
		{Kind: agent.EventRateLimit, RateLimitType: "five_hour", Status: "allowed", Utilization: fraction(0.15)},
		{Kind: agent.EventRateLimit, RateLimitType: "seven_day", Status: "allowed_warning", Utilization: fraction(0.25)},
	}
	_, u := newUI(t, f)
	if !u.Has("usage-seven_day") {
		t.Fatal("no weekly bar")
	}
	if u.Has("usage-notice") {
		t.Error("a notice for a limit a quarter used")
	}
}
