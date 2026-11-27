package store

import (
	"context"
	"testing"
	"time"
)

// A reading that says what the last one of its kind said isn't kept again; one that differs in either
// what is used or when it resets is.
func TestUsageReadings(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.UnixMilli(1_800_000_000_000)
	reset := t0.Add(5 * time.Hour)
	for i, r := range []UsageReading{
		{At: t0, Kind: "five_hour", Used: 0.1, ResetsAt: reset},
		{At: t0.Add(time.Minute), Kind: "five_hour", Used: 0.1, ResetsAt: reset}, // the same: dropped
		{At: t0.Add(2 * time.Minute), Kind: "seven_day", Used: 0.1, ResetsAt: reset},
		{At: t0.Add(3 * time.Minute), Kind: "five_hour", Used: 0.2, ResetsAt: reset},
		{At: t0.Add(4 * time.Minute), Kind: "five_hour", Used: 0.2, ResetsAt: reset.Add(5 * time.Hour)},
	} {
		if err := s.AddUsageReading(r); err != nil {
			t.Fatal(i, err)
		}
	}
	got, err := s.UsageReadings(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"five_hour 0.1", "seven_day 0.1", "five_hour 0.2", "five_hour 0.2"}
	if len(got) != len(want) {
		t.Fatalf("got %d readings, want %d: %+v", len(got), len(want), got)
	}
	for i, r := range got {
		if g := r.Kind + " " + formatUsed(r.Used); g != want[i] {
			t.Errorf("reading %d = %s, want %s", i, g, want[i])
		}
	}
	if !got[0].ResetsAt.Equal(reset) || !got[0].At.Equal(t0) {
		t.Errorf("first reading = %+v", got[0])
	}
	if later, _ := s.UsageReadings(ctx, t0.Add(150*time.Second)); len(later) != 2 {
		t.Errorf("since 2.5m: %d readings, want 2", len(later))
	}
}

func formatUsed(f float64) string {
	return map[float64]string{0.1: "0.1", 0.2: "0.2"}[f]
}

// Turns are kept with the thread's title, and pruning drops the history from before a time.
func TestUsageTurnsAndPrune(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.UnixMilli(1_800_000_000_000)
	for _, u := range []UsageTurn{
		{At: t0, ThreadID: "a", Title: "Old", CostUSD: 1},
		{At: t0.Add(time.Hour), ThreadID: "a", Title: "Renamed", CostUSD: 0.5},
		{At: t0.Add(2 * time.Hour), ThreadID: "b", Title: "B", CostUSD: 0.25},
	} {
		if err := s.AddUsageTurn(u); err != nil {
			t.Fatal(err)
		}
	}
	s.AddUsageReading(UsageReading{At: t0, Kind: "five_hour", Used: 0.1})
	s.AddUsageReading(UsageReading{At: t0.Add(2 * time.Hour), Kind: "five_hour", Used: 0.2})
	got, err := s.UsageTurns(ctx, time.Time{})
	if err != nil || len(got) != 3 || got[1].Title != "Renamed" || got[2].CostUSD != 0.25 {
		t.Fatalf("turns = %+v, %v", got, err)
	}
	if err := s.PruneUsage(t0.Add(30 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UsageTurns(ctx, time.Time{}); len(got) != 2 {
		t.Errorf("after pruning: %d turns, want 2", len(got))
	}
	if got, _ := s.UsageReadings(ctx, time.Time{}); len(got) != 1 {
		t.Errorf("after pruning: %d readings, want 1", len(got))
	}
}
