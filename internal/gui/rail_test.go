package gui

import "testing"

// Threads at work sit in their own group pinned above the usage bars, out of the scrolling list, with
// room between them and no line. The rail's filter applies to them too.
func TestWorkingPinnedAboveUsage(t *testing.T) {
	f := demoBackend()
	f.rate = demoLimits()
	a, u := newUI(t, f)
	if !u.Has("rail-working") {
		t.Fatal("no pinned working group")
	}
	w, bar := u.Get("rail-working").Bounds(), u.Get("usage-five_hour").Bounds()
	if gap := bar.Y - (w.Y + w.H); gap < 0 || gap > 40 {
		t.Errorf("working group ends %v above the first usage bar, want just above it", gap)
	}
	card := u.Get("card-a1f3c9d2-oauth").Bounds()
	if card.Y < w.Y || card.Y+card.H > w.Y+w.H {
		t.Errorf("working thread %v outside the pinned group %v", card, w)
	}

	a.railQuery.Set("oauth")
	u.Settle()
	if !u.Has("card-a1f3c9d2-oauth") || u.Has("card-e1e2e3e4-readme") {
		t.Error("the filter doesn't apply to the working group")
	}
	a.railQuery.Set("audit")
	u.Settle()
	if u.Has("rail-working") {
		t.Error("an empty working group shows")
	}
}
