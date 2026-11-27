package gui

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/core"
)

// demoUsage is a week of usage history: the 5-hour window filling and resetting through working days,
// the weekly one climbing, and what the demo threads used.
func demoUsage(now time.Time) core.UsageReport {
	r := rand.New(rand.NewPCG(7, 11))
	weekReset := now.Add(4 * 24 * time.Hour).Truncate(time.Hour)
	since := weekReset.Add(-7 * 24 * time.Hour)
	var rs []core.UsageReading
	week, five := 0.0, 0.0
	var fiveReset time.Time
	for at := since; at.Before(now); at = at.Add(15 * time.Minute) {
		h := at.Local().Hour()
		if h < 9 || h >= 19 || r.Float64() < 0.4 {
			continue
		}
		if fiveReset.IsZero() || !fiveReset.After(at) {
			five, fiveReset = 0, at.Add(5*time.Hour)
		}
		d := r.Float64() * 0.05
		five = min(five+d, 1)
		week = min(week+d/10, 1)
		rs = append(rs,
			core.UsageReading{At: at, Kind: "five_hour", Used: float64(int(five*100)) / 100, ResetsAt: fiveReset},
			core.UsageReading{At: at, Kind: "seven_day", Used: float64(int(week*100)) / 100, ResetsAt: weekReset})
	}
	return core.UsageReport{Readings: rs, Since: since, Threads: []core.UsageThread{
		{ThreadID: core.OrchestratorID, Title: "Solo", Kind: core.UsageKindSolo, CostUSD: 6.2, Turns: 41},
		{ThreadID: "e5e6e7e8-landing", Title: "Add tenant usage report", Kind: core.UsageKindThread, CostUSD: 5.1, Turns: 18},
		{ThreadID: "c3d4e5f6-forms", Title: "Move settings page to new form components", Kind: core.UsageKindThread, CostUSD: 3.4, Turns: 12},
		{ThreadID: "a1f3c9d2-oauth", Title: "Fix OAuth token refresh", Kind: core.UsageKindThread, CostUSD: 2.2, Turns: 9},
		{ThreadID: "gone-1", Title: "Bump the Go toolchain", CostUSD: 0.8, Turns: 3},
		{ThreadID: "b7e21a90-limit", Title: "Rate limit /api/upload", Kind: core.UsageKindThread, CostUSD: 0.6, Turns: 4},
	}}
}

// The line is what is left: it climbs back to 100% where a window reset between two readings, and
// again at its end when the last window has reset by now. Readings of other limits don't count.
func TestLeftSeries(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	rs := []core.UsageReading{
		{At: at(0), Kind: "five_hour", Used: 0.2, ResetsAt: at(60)},
		{At: at(10), Kind: "seven_day", Used: 0.9, ResetsAt: at(9000)},
		{At: at(30), Kind: "five_hour", Used: 0.5, ResetsAt: at(60)},
		{At: at(90), Kind: "five_hour", Used: 0.1, ResetsAt: at(390)},
	}
	got := leftSeries(rs, "five_hour", at(100), 24*time.Hour)
	want := []leftPoint{{at(0), 80}, {at(30), 50}, {at(60), 50}, {at(60), 100}, {at(90), 90}, {at(100), 90}}
	if !samePoints(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	// Past the last window's reset, the line ends full.
	got = leftSeries(rs, "five_hour", at(400), 24*time.Hour)
	if n := len(got); n < 3 || got[n-1].Left != 100 || !got[n-1].At.Equal(at(400)) || !got[n-2].At.Equal(at(390)) {
		t.Errorf("after the reset: %v", got)
	}
	// Points before the span are cut, the span starting at what was left then.
	got = leftSeries(rs, "five_hour", at(100), 80*time.Minute)
	want = []leftPoint{{at(20), 80}, {at(30), 50}, {at(60), 50}, {at(60), 100}, {at(90), 90}, {at(100), 90}}
	if !samePoints(got, want) {
		t.Errorf("cut: got %v\nwant %v", got, want)
	}
	if got := leftSeries(rs, "seven_day_opus", at(100), time.Hour); got != nil {
		t.Errorf("no readings: %v", got)
	}
}

func samePoints(a, b []leftPoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].At.Equal(b[i].At) || a[i].Left-b[i].Left > 1e-9 || b[i].Left-a[i].Left > 1e-9 {
			return false
		}
	}
	return true
}

// Clicking the usage bars opens the Usage page, which lists the threads; a thread's row opens it.
func TestUsagePageOpens(t *testing.T) {
	f := demoBackend()
	f.rate = demoLimits()
	f.usage = demoUsage(time.Now())
	a, u := newUI(t, f)
	if u.Has("usage-page") {
		t.Fatal("the page is open before it is asked for")
	}
	u.Click("usage-five_hour")
	u.WaitFor("the Usage page", func() bool { return u.Has("usage-row-c3d4e5f6-forms") })
	for _, k := range []string{"usage-card-five_hour", "usage-card-seven_day", "usage-line-seven_day", "usage-row-gone-1"} {
		if !u.Has(k) {
			t.Errorf("no %s", k)
		}
	}
	u.Click("usage-row-c3d4e5f6-forms")
	u.WaitFor("the thread to open", func() bool { return a.openID.Peek() == "c3d4e5f6-forms" && !a.usageOpen.Peek() })
}

// usagePage opens the Usage page over a week of history, for TestShots.
func usagePage(a *App, f *fakeBackend) {
	f.rate = demoLimits()
	f.usage = demoUsage(time.Now())
	a.rate.Set(f.rate)
	a.openUsage()
}
