package gui

import (
	"strings"
	"testing"
	"time"
)

// onMachine moves a backend's times to a machine whose clock is skew ahead of this one's (behind, when
// negative): the times are stamped by its clock, and the backend tells its time (api.Clock), as a web
// page's connection to the machine does.
func onMachine(f *fakeBackend, skew time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.threads {
		v := &f.threads[i]
		v.CreatedAt, v.UpdatedAt = v.CreatedAt.Add(skew), v.UpdatedAt.Add(skew)
		if !v.TurnStarted.IsZero() {
			v.TurnStarted = v.TurnStarted.Add(skew)
		}
		for j := range v.Background {
			v.Background[j].Started = v.Background[j].Started.Add(skew)
		}
	}
	f.machineNow = func() time.Time { return time.Now().Add(skew) }
}

// A thread's running times are measured by the machine's clock, which stamped them: right in the rail,
// above the composer and in the palette whether this device's clock is behind the machine's or ahead
// of it, and never negative.
func TestRunningTimesUseTheMachinesClock(t *testing.T) {
	for _, skew := range []time.Duration{10 * time.Minute, -10 * time.Minute} {
		f := demoBackend()
		bg := waitingOnBackground(f) // its work began 4m 12s ago
		onMachine(f, skew)
		a, u := newUI(t, f)

		const oauth = "a1f3c9d2-oauth" // its turn began 2m 14s ago
		if row := u.Get("card-" + oauth).Text(); !strings.Contains(row, "2m 1") || strings.Contains(row, "-") {
			t.Errorf("skew %v: the working row reads %q, want 2m 1x", skew, row)
		}
		if row := u.Get("card-" + bg.ID).Text(); !strings.Contains(row, "4m 1") || strings.Contains(row, "-") {
			t.Errorf("skew %v: the background row reads %q, want 4m 1x", skew, row)
		}
		a.openThread(oauth, "conversation")
		u.Frame()
		if line := u.Get("working-line").Text(); !strings.Contains(line, "Working · 2m 1") {
			t.Errorf("skew %v: the working line reads %q", skew, line)
		}
		a.openThread(bg.ID, "conversation")
		u.Frame()
		if line := u.Get("background-line").Text(); !strings.Contains(line, "Waiting on background work · 4m 1") {
			t.Errorf("skew %v: the background line reads %q", skew, line)
		}
		var oauthDetail string
		for _, it := range a.paletteItems("") {
			if it.Key == oauth {
				oauthDetail = it.Detail
			}
		}
		if !strings.HasSuffix(oauthDetail, " · 2m 14s") && !strings.HasSuffix(oauthDetail, " · 2m 15s") {
			t.Errorf("skew %v: the palette says %q", skew, oauthDetail)
		}
	}
}

// elapsed and shortAgo measure against the time they are given, and a start after it, which a right
// clock never gives, reads as none rather than a negative time.
func TestElapsedNeverNegative(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for since, want := range map[time.Duration]string{
		-10 * time.Minute: "0s", -time.Second: "0s", 0: "0s", 45 * time.Second: "45s",
		2*time.Minute + 14*time.Second: "2m 14s", time.Hour + 5*time.Minute: "1h 05m",
	} {
		if got := elapsed(now.Add(-since), now); got != want {
			t.Errorf("elapsed %v before now = %q, want %q", since, got, want)
		}
	}
	if got := shortAgo(now.Add(3*time.Minute), now); got != "now" {
		t.Errorf("shortAgo of a time ahead = %q, want now", got)
	}
	if got := shortAgo(now.Add(-3*time.Hour), now); got != "3h" {
		t.Errorf("shortAgo 3h back = %q", got)
	}
}
