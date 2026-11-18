package gui

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

// TestLandPanelShots renders the prototype panels of open merge cards (landpanel.go), each direction in
// each state, for design review:
//
//	SOLO_SHOTS=/tmp/shots go test ./internal/gui -run TestLandPanelShots
//
// SOLO_SHOTS_ONLY narrows them by name prefix ("shelf-", "pill-three", …).
func TestLandPanelShots(t *testing.T) {
	out := os.Getenv("SOLO_SHOTS")
	if out == "" {
		t.Skip("set SOLO_SHOTS to an output directory")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	defer func(k landPanelStyle) { landPanelKind = k }(landPanelKind)
	light, dark := Themes()

	three := []string{longReposID, "c1c2c3c4-csv", "a1f3c9d2-oauth"}
	many := []string{longReposID, "c1c2c3c4-csv", "a1f3c9d2-oauth", formsID, "b1b2b3b4-paginate",
		"e5e6e7e8-landing", "l0l0l0l0-logging", "r1r1r1r1-retire", "s1s1s1s1-cicache"}
	type step struct{ do, key string } // "click" or "hover" an element, by key, once the window is up
	type shot struct {
		name  string
		cards []string // the threads with a live merge card, oldest first; nil: none (forms merged)
		w     float32
		setup func(a *App)
		steps []step
	}
	var shots []shot
	shots = append(shots, shot{name: "off-three", cards: three})
	for _, d := range []landPanelStyle{landShelf, landPill, landStack} {
		p := string(d) + "-"
		shots = append(shots,
			shot{name: p + "one", cards: []string{formsID}},
			shot{name: p + "three", cards: three},
			shot{name: p + "many", cards: many},
			shot{name: p + "none"},
			shot{name: p + "narrow", cards: three, w: 1000},
			shot{name: p + "compact", cards: three, w: 680},
			shot{name: p + "split", cards: three, setup: func(a *App) { a.openThread(formsID, "conversation") }},
		)
		switch d {
		case landShelf:
			shots = append(shots,
				shot{name: p + "three-menu", cards: three, steps: []step{{"click", "shelf-more-c1c2c3c4-csv"}}},
				shot{name: p + "narrow-open", cards: three, w: 1000, setup: func(a *App) { a.setLandOpen(true) }},
				shot{name: p + "compact-open", cards: three, w: 680, setup: func(a *App) { a.setLandOpen(true) }},
			)
		case landPill:
			open := func(a *App) { a.setLandOpen(true) }
			shots = append(shots,
				shot{name: p + "one-open", cards: []string{formsID}, setup: open},
				shot{name: p + "three-open", cards: three, setup: open},
				shot{name: p + "three-hover", cards: three, setup: open, steps: []step{{"hover", "pill-row-c1c2c3c4-csv"}}},
				shot{name: p + "three-menu", cards: three, setup: open,
					steps: []step{{"hover", "pill-row-" + longReposID}, {"click", "pill-more-" + longReposID}}},
				shot{name: p + "three-actions", cards: three, setup: func(a *App) { a.setLandOpen(true); landHoverAll = true }},
				shot{name: p + "many-open", cards: many, setup: open},
				shot{name: p + "narrow-open", cards: three, w: 1000, setup: open},
				shot{name: p + "compact-open", cards: three, w: 680, setup: open},
			)
		case landStack:
			shots = append(shots,
				shot{name: p + "three-menu", cards: three, steps: []step{{"click", "stack-more-c1c2c3c4-csv"}}},
				shot{name: p + "three-folded", cards: three, setup: func(a *App) { a.setLandOpen(false) }},
				shot{name: p + "many-more", cards: many, setup: func(a *App) { a.landMore.Set(true) }},
				shot{name: p + "narrow-open", cards: three, w: 1000, setup: func(a *App) { a.setLandOpen(true) }},
				shot{name: p + "compact-open", cards: three, w: 680, setup: func(a *App) { a.setLandOpen(true) }},
			)
		}
	}
	for _, theme := range []struct {
		name string
		t    *ui.Theme
	}{{"dark", dark}, {"light", light}} {
		for _, sh := range shots {
			if only := os.Getenv("SOLO_SHOTS_ONLY"); only != "" && !strings.HasPrefix(sh.name, only) {
				continue
			}
			if theme.name == "light" && !strings.HasSuffix(sh.name, "-three") && !strings.HasSuffix(sh.name, "-three-open") {
				continue
			}
			landPanelKind, landHoverAll = "", false
			if d, _, ok := strings.Cut(sh.name, "-"); ok && d != "off" {
				landPanelKind = landPanelStyle(d)
			}
			f := landBackend(sh.cards)
			a := New(f)
			if sh.setup != nil {
				sh.setup(a)
			}
			w := sh.w
			if w == 0 {
				w = 1440
			}
			u := uitest.New(t, a.Root, uitest.Size(w, 900), uitest.Theme(theme.t), uitest.Bindings(Bindings()...))
			for i := 0; i < 10; i++ {
				u.Advance(100 * time.Millisecond)
				time.Sleep(20 * time.Millisecond)
			}
			for _, s := range sh.steps {
				switch s.do {
				case "click":
					u.Click(s.key)
				case "hover":
					u.Get(s.key).Hover()
				}
				for i := 0; i < 4; i++ {
					u.Advance(100 * time.Millisecond)
				}
			}
			u.Settle()
			file, err := os.Create(filepath.Join(out, theme.name+"-"+sh.name+".png"))
			if err != nil {
				t.Fatal(err)
			}
			png.Encode(file, u.Screenshot())
			file.Close()
			u.Close()
		}
	}
}

// landBackend is the demo afternoon with a live merge card for each of cards, in that order, and a
// conversation that has moved on since, so the cards in it have scrolled out of view. With no cards,
// the settings thread's card is there but its work has merged.
func landBackend(cards []string) *fakeBackend {
	f := demoBackend()
	now := time.Now()
	f.threads = append(f.threads, longReposThread(longReposID, longRepoNames))
	for i := range f.threads {
		th := &f.threads[i]
		switch th.ID {
		case "a1f3c9d2-oauth": // its PR open, checks passed
			th.Status, th.InTurn, th.Step, th.Live = core.StatusIdle, false, nil, nil
			th.Forge, th.CanMerge, th.Changed = forge.GitHub, true, 4
			th.PR = &forge.PR{Kind: forge.GitHub, Number: 61, URL: "https://github.com/acme/web/pull/61", State: forge.StateOpen,
				Checks: forge.ChecksPassing, Counts: forge.CheckCounts{Total: 6, Passed: 6}, Mergeable: true}
		case formsID:
			th.Changed = 6
			if cards == nil {
				th.Status, th.CanOpenPR, th.CanMerge, th.CanMergeLocal = core.StatusMerged, false, false, false
			}
		}
	}
	extra := func(id, title, repo string) core.ThreadView {
		th := f.threads[3] // the settings thread, settled
		th.ID, th.Title, th.RepoPath, th.Branch = id, title, "/home/dev/code/"+repo, "solo/"+id[:8]
		th.CreatedAt, th.UpdatedAt = now.Add(-2*time.Hour), now.Add(-20*time.Minute)
		th.CanOpenPR, th.CanMerge, th.CanMergeLocal, th.Changed, th.PR = false, false, false, 5, nil
		return th
	}
	logging := extra("l0l0l0l0-logging", "Tidy the logging setup", "infra")
	logging.Forge, logging.CanMerge = forge.None, true
	retire := extra("r1r1r1r1-retire", "Retire the v1 endpoints", "api")
	retire.Forge, retire.CanMarkReady = forge.GitHub, true
	retire.PR = &forge.PR{Kind: forge.GitHub, Number: 150, URL: "https://github.com/acme/api/pull/150", State: forge.StateOpen,
		Draft: true, Checks: forge.ChecksPassing, Counts: forge.CheckCounts{Total: 7, Passed: 7}}
	cache := extra("s1s1s1s1-cicache", "Speed up the CI cache", "infra")
	cache.Forge, cache.Status, cache.InTurn = forge.GitHub, core.StatusRunning, true
	cache.TurnStarted = now.Add(-3 * time.Minute)
	f.threads = append(f.threads, logging, retire, cache)
	// One part per single-repository thread, its pull request on it, as core shows them.
	for i := range f.threads {
		th := &f.threads[i]
		if !th.IsWorker || len(th.Parts) > 0 || th.InPlace {
			continue
		}
		prOpen := th.PR != nil && th.PR.State == forge.StateOpen
		th.Parts = []core.PartView{{Part: core.Part{Repo: th.RepoPath, Branch: th.Branch, BaseBranch: th.BaseBranch, PR: th.PR},
			Name: filepath.Base(th.RepoPath), Changed: th.Changed, Forge: th.Forge, CanOpenPR: th.CanOpenPR,
			CanMerge: th.CanMerge, CanMergeLocal: th.CanMergeLocal && !prOpen}}
	}
	// No question waiting: the conversation gets the window.
	f.pending = nil
	for i := range f.threads {
		if th := &f.threads[i]; th.ID == "b7e21a90-limit" {
			th.Status, th.PendingAsk, th.TurnStarted = core.StatusRunning, false, now.Add(-90*time.Second)
		}
	}

	o := core.OrchestratorID
	var kept []core.Item
	for _, it := range f.items[o] {
		if it.Kind != core.ItemProposal || cards == nil {
			kept = append(kept, it)
		}
	}
	f.items[o] = kept
	for _, id := range cards {
		f.add(o, core.ItemProposal, core.ProposalData{ThreadID: id, Summary: "Ready to land."})
	}
	if cards == nil {
		return f
	}
	for i := 0; i < 6; i++ {
		f.add(o, core.ItemUser, core.UserData{Text: fmt.Sprintf("How is the rate limiter coming along? (%d)", i+1)})
		f.add(o, core.ItemAssistant, core.TextData{Text: "Still working: it chose Redis for the counters, as you said, and is writing the test for a burst of uploads from one user across two instances. The 429 response now carries `Retry-After`, and the middleware skips health checks. I'll tell you when it finishes."})
	}
	return f
}
