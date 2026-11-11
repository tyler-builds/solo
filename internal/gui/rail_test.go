package gui

import (
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/store"
)

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

// A working row shows its thread's title, not its repositories: long repository names would crowd
// the title out of a thread across several of them. A long title is cut short to fit the row.
func TestWorkingRowShowsTitleNotRepos(t *testing.T) {
	f := demoBackend()
	now := time.Now()
	names := []string{"inventory_service_core", "inventory-service-public-api", "inventory-service-admin-web"}
	parts := make([]core.PartView, len(names))
	for i, n := range names {
		parts[i] = core.PartView{Part: core.Part{Repo: "/home/dev/code/" + n, Branch: "solo/w1w2w3w4", BaseBranch: "main"}, Name: n}
	}
	multi := core.ThreadView{Thread: store.Thread{ID: "w1w2w3w4-multi", Kind: store.KindWorker,
		Title: "Add a warehouse filter to the stock level report", Status: core.StatusRunning, RepoPath: parts[0].Repo,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute)},
		IsWorker: true, HasWorktree: true, InTurn: true, TurnStarted: now.Add(-3 * time.Minute), Parts: parts}
	f.threads = append(f.threads, multi)
	_, u := newUI(t, f)

	for _, id := range []string{multi.ID, "a1f3c9d2-oauth"} {
		row := u.Get("card-" + id)
		text := row.Text()
		for _, r := range append(names, "web") {
			if strings.Contains(text, r) {
				t.Errorf("working row %s shows repository %q: %q", id, r, text)
			}
		}
		rb, tb := row.Bounds(), u.Get("working-title-"+id).Bounds()
		if tb.W < rb.W/2 {
			t.Errorf("working row %s gives its title %v of %v, want most of it", id, tb.W, rb.W)
		}
		if tb.X+tb.W > rb.X+rb.W {
			t.Errorf("working row %s title %v overflows the row %v", id, tb, rb)
		}
	}
	if !strings.Contains(u.Get("card-"+multi.ID).Text(), multi.Title) {
		t.Error("the multi-repo working row doesn't show its title")
	}
}
