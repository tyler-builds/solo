package gui

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

// Threads of mergePillBackend, one in each state a merge card can be in.
const (
	pillCSV      = "c1c2c3c4-csv"      // its PR's checks failed: Merge, disabled
	pillOAuth    = "a1f3c9d2-oauth"    // its PR's checks passed: Merge
	pillPaginate = "b1b2b3b4-paginate" // checks running, auto-merge on
	pillLanding  = "e5e6e7e8-landing"  // three repositories, three PRs, one ready: Merge db #31
	pillLogging  = "l0l0l0l0-logging"  // no remote: Merge locally
	pillRetire   = "r1r1r1r1-retire"   // a draft PR: Ready for review
	pillCache    = "s1s1s1s1-cicache"  // still working: Open PR, disabled
)

// pillThree and pillMany are the cards of the shots and tests, oldest first: three in mixed states
// (a multi-repository thread with no merge requests yet, failed checks, passed checks), and one in
// every state.
var (
	pillThree = []string{longReposID, pillCSV, pillOAuth}
	pillMany  = []string{longReposID, pillCSV, pillOAuth, formsID, pillPaginate, pillLanding, pillLogging, pillRetire, pillCache}
)

// mergePillBackend is the demo afternoon with a live merge card for each of cards, in that order. With
// moved, the conversation has moved on since, so the cards in it are out of view. With no cards, the
// settings thread's card is there, but its work has merged.
func mergePillBackend(cards []string, moved bool) *fakeBackend {
	f := demoBackend()
	now := time.Now()
	f.threads = append(f.threads, longReposThread(longReposID, longRepoNames))
	for i := range f.threads {
		th := &f.threads[i]
		switch th.ID {
		case pillOAuth:
			th.Status, th.InTurn, th.Step, th.Live, th.CanInterrupt = core.StatusIdle, false, nil, nil, false
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
	logging := extra(pillLogging, "Tidy the logging setup", "infra")
	logging.Forge, logging.CanMerge = forge.None, true
	retire := extra(pillRetire, "Retire the v1 endpoints", "api")
	retire.Forge, retire.CanMarkReady = forge.GitHub, true
	retire.PR = &forge.PR{Kind: forge.GitHub, Number: 150, URL: "https://github.com/acme/api/pull/150", State: forge.StateOpen,
		Draft: true, Checks: forge.ChecksPassing, Counts: forge.CheckCounts{Total: 7, Passed: 7}}
	cache := extra(pillCache, "Speed up the CI cache", "infra")
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
	if moved && cards != nil {
		for i := 0; i < 6; i++ {
			f.add(o, core.ItemUser, core.UserData{Text: fmt.Sprintf("How is the rate limiter coming along? (%d)", i+1)})
			f.add(o, core.ItemAssistant, core.TextData{Text: "Still working: it chose Redis for the counters, as you said, and is writing the test for a burst of uploads from one user across two instances. The 429 response now carries `Retry-After`, and the middleware skips health checks. I'll tell you when it finishes."})
		}
	}
	return f
}

// setThread changes one of f's threads.
func setThread(f *fakeBackend, id string, change func(th *core.ThreadView)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.threads {
		if f.threads[i].ID == id {
			change(&f.threads[i])
		}
	}
}

// pillUI shows f's app in a window w wide (1440 for 0).
func pillUI(t *testing.T, f *fakeBackend, w float32, setup func(a *App)) (*App, *uitest.UI) {
	t.Helper()
	if w == 0 {
		w = 1440
	}
	_, dark := Themes()
	a := New(f)
	if setup != nil {
		setup(a)
	}
	u := uitest.New(t, a.Root, uitest.Size(w, 900), uitest.Theme(dark), uitest.Bindings(Bindings()...))
	t.Cleanup(u.Close)
	started(a, u)
	u.Settle()
	return a, u
}

// openPill opens the merge pill's list.
func openPill(t *testing.T, u *uitest.UI) {
	t.Helper()
	u.Click("merge-pill")
	u.Settle()
	if !u.Has("merge-pill-list") {
		t.Fatal("clicking the pill doesn't open its list")
	}
}

// The pill lists the threads whose merge card can still be acted on: not merged, discarded, marked done
// (before landing or after) or scratch; each once, by its newest card, the newest first.
func TestOpenCardsRule(t *testing.T) {
	f := demoBackend()
	o := core.OrchestratorID
	f.items[o] = nil
	base := f.threads[3] // the settings thread, settled
	add := func(id string, change func(th *core.ThreadView)) {
		th := base
		th.ID, th.Title = id, "Thread "+id
		if change != nil {
			change(&th)
		}
		f.threads = append(f.threads, th)
	}
	add("live-old", nil)
	add("live-new", nil)
	add("merged", func(th *core.ThreadView) { th.Status = core.StatusMerged })
	add("discarded", func(th *core.ThreadView) { th.Status = core.StatusDiscarded })
	add("archived", func(th *core.ThreadView) { th.Status = core.StatusArchived; th.DoneAt = time.Now() })
	add("done", func(th *core.ThreadView) { th.DoneAt = time.Now() })
	add("scratch", func(th *core.ThreadView) { th.Scratch = true })
	add("working", func(th *core.ThreadView) { th.Status, th.InTurn = core.StatusRunning, true })
	for _, id := range []string{"live-old", "merged", "discarded", "archived", "done", "scratch", "working", "live-new", "gone", "live-old"} {
		f.add(o, core.ItemProposal, core.ProposalData{ThreadID: id, Summary: "Ready."})
	}
	f.add(o, core.ItemAssistant, core.TextData{Text: "Both are ready."})
	a := New(f)
	var got []string
	for _, c := range a.openCards(f.items[o]) {
		got = append(got, c.th.ID)
	}
	if want := []string{"live-old", "live-new", "working"}; !slices.Equal(got, want) {
		t.Fatalf("open cards %q, want %q", got, want)
	}
	if got := a.openCards(nil); len(got) != 0 {
		t.Fatalf("open cards of an empty chat: %v", got)
	}
}

// With no card to act on, there is no pill; with cards, it counts them, and its list has a row each,
// the newest card first.
func TestMergePillCountsAndLists(t *testing.T) {
	_, u := pillUI(t, mergePillBackend(nil, false), 0, nil)
	if u.Has("merge-pill") {
		t.Fatal("a pill with no open merge card")
	}
	_, u = pillUI(t, mergePillBackend(pillThree, true), 0, nil)
	if name := u.Get("merge-pill").Name(); name != "3 to land: 3 merge cards" {
		t.Fatalf("the pill is named %q", name)
	}
	if u.Has("merge-pill-list") {
		t.Fatal("the pill starts open")
	}
	openPill(t, u)
	if !u.Get("merge-pill").Expanded() {
		t.Fatal("the open pill isn't marked expanded")
	}
	var ys []float32
	for _, id := range []string{pillOAuth, pillCSV, longReposID} {
		ys = append(ys, u.Get("merge-pill-row-"+id).Bounds().Y)
	}
	if !slices.IsSorted(ys) {
		t.Fatalf("rows at %v, want the newest card first", ys)
	}
}

// The list closes from its close button and Escape, which put focus back on the pill, from a press
// outside it and from the pill itself; a menu opened from a row doesn't close it.
func TestMergePillCloses(t *testing.T) {
	f := mergePillBackend(pillThree, true)
	a, u := pillUI(t, f, 0, nil)
	closed := func(how string) {
		t.Helper()
		u.Settle()
		if u.Has("merge-pill-list") || a.pillOpen.Peek() {
			t.Fatalf("%s doesn't close the list", how)
		}
	}
	openPill(t, u)
	u.Click("merge-pill-close")
	closed("the close button")
	if !u.Get("merge-pill").Focused() {
		t.Fatal("closing from the close button leaves focus off the pill")
	}

	openPill(t, u)
	u.Get("merge-pill-title-" + pillCSV).Focus()
	u.Press("Escape")
	closed("Escape inside the list")
	if !u.Get("merge-pill").Focused() {
		t.Fatal("Escape leaves focus off the pill")
	}

	openPill(t, u)
	u.Press("Escape")
	closed("Escape on the pill")

	openPill(t, u)
	u.Click("orch-draft-area")
	closed("a press outside")

	openPill(t, u)
	u.Click("merge-pill")
	closed("the pill")

	openPill(t, u)
	u.Click("merge-pill-more-" + pillCSV)
	u.Settle()
	if !u.HasRole(access.RoleMenuItem, "Discard thread…") || !u.Has("merge-pill-list") {
		t.Fatal("the row's menu didn't open over the open list")
	}
	u.Press("Escape")
	u.Settle()
	if !u.Has("merge-pill-list") {
		t.Fatal("Escape in the row's menu closed the list as well")
	}
}

// From the keyboard: Enter on the pill opens it, Tab reaches the rows' controls.
func TestMergePillKeyboard(t *testing.T) {
	_, u := pillUI(t, mergePillBackend(pillThree, true), 0, nil)
	u.Get("merge-pill").Focus()
	u.Press("Enter")
	u.Settle()
	if !u.Has("merge-pill-list") {
		t.Fatal("Enter on the pill doesn't open it")
	}
	for i := 0; i < 12; i++ {
		u.Press("Tab")
		if f := u.Focused(); f != nil && f.Key() == "merge-pill-title-"+pillOAuth {
			return
		}
	}
	t.Fatal("Tab never reaches the first row's title")
}

// pillEffect is what an action did: what the backend was asked, the dialog it opened, the thread it
// opened, the message it started, the page it opened.
func pillEffect(a *App, f *fakeBackend, urls *[]string) string {
	f.mu.Lock()
	var done []string
	for _, th := range f.threads {
		if !th.DoneAt.IsZero() && time.Since(th.DoneAt) < time.Minute {
			done = append(done, th.ID)
		}
	}
	s := fmt.Sprintf("opened=%v drafts=%q for=%q merged=%v repos=%q ready=%v done=%v", f.opened, f.drafts, f.openedFor,
		f.merged, f.mergedRepos, f.readied, done)
	f.mu.Unlock()
	confirm := ""
	if c := a.confirm.Peek(); c != nil {
		confirm = c.title
	}
	open := a.openID.Peek()
	if open != "" {
		open += "/" + a.openTab.Peek()
	}
	return s + fmt.Sprintf(" confirm=%q open=%q draft=%q urls=%q", confirm, open, a.orchDraft.Peek(), *urls)
}

// Each action on a row of the pill does what the same action does on the thread's merge card in the
// chat: the same request to the backend, the same dialog, the same thread opened, the same states.
func TestMergePillActionsMatchTheCard(t *testing.T) {
	type surface struct {
		u    *uitest.UI
		root *uitest.Element // the pill's row, or the card
		pill bool
	}
	pick := func(s surface, pill, card string) string {
		if s.pill {
			return pill
		}
		return card
	}
	// menu opens a split button's menu and picks an item.
	menu := func(s surface, button, item string) string {
		s.root.ByRole(access.RoleGroup, button).ByRole(access.RoleButton, "More options").Click()
		s.u.Settle()
		el := s.u.ByRole(access.RoleMenuItem, item)
		if el.Disabled() {
			return "disabled " + item
		}
		el.Click()
		return ""
	}
	// more picks an item from the row's or the card's "…" menu.
	more := func(s surface, item string) {
		s.root.ByRole(access.RoleButton, "More").Click()
		s.u.Settle()
		s.u.ByRole(access.RoleMenuItem, item).Click()
	}
	click := func(s surface, name string) string {
		s.root.ByRole(access.RoleButton, name).Click()
		return ""
	}
	state := func(s surface, name string) string {
		b := s.root.ByRole(access.RoleButton, name)
		return fmt.Sprintf("%s disabled=%v busy=%v", name, b.Disabled(), b.States().Has(access.StateBusy))
	}
	cases := []struct {
		name, id string
		setup    func(a *App, f *fakeBackend)
		do       func(s surface) string
		want     string // in the effect
	}{
		{"open PR", formsID, nil, func(s surface) string { return click(s, "Open PR") }, "opened=[" + formsID + "]"},
		{"open PR as draft", formsID, nil, func(s surface) string { return menu(s, "Open PR", "Open as draft") }, `drafts=["` + formsID + ` "]`},
		{"open every MR", longReposID, nil, func(s surface) string { return click(s, "Open 3 MRs") }, `for=["` + longReposID + ` "]`},
		{"open every MR as drafts", longReposID, nil, func(s surface) string { return menu(s, "Open 3 MRs", "Open 3 MRs as drafts") },
			`drafts=["` + longReposID + ` "]`},
		{"open one repository's MR", longReposID, nil,
			func(s surface) string { return menu(s, "Open 3 MRs", "MR for inventory-service-public-api only") },
			`for=["` + longReposID + ` /home/dev/code/inventory-service-public-api"]`},
		{"ready for review", pillRetire, nil, func(s surface) string { return click(s, pick(s, "Ready", "Ready for review")) },
			"ready=[" + pillRetire + "]"},
		{"merge", pillOAuth, nil, func(s surface) string { return click(s, "Merge") }, "merged=[" + pillOAuth + "]"},
		{"merge locally", pillLogging, nil, func(s surface) string { return click(s, "Merge locally") }, "merged=[" + pillLogging + "]"},
		{"merge one repository", pillLanding, nil, func(s surface) string { return click(s, pick(s, "Merge #31", "Merge db #31")) },
			`repos=["` + pillLanding + ` /home/dev/code/db"]`},
		{"merge all waits", pillLanding, nil, func(s surface) string { return menu(s, pick(s, "Merge #31", "Merge db #31"), "Merge all") },
			"disabled Merge all"},
		{"merge blocked", pillCSV, nil, func(s surface) string { return state(s, "Merge") }, "Merge disabled=true busy=false"},
		{"working", pillCache, nil, func(s surface) string { return state(s, "Open PR") }, "Open PR disabled=true busy=false"},
		{"auto-merge", pillPaginate, nil, func(s surface) string {
			return fmt.Sprintf("auto=%v", s.root.HasRole(access.RoleStaticText, "Auto-merge on"))
		}, "auto=true"},
		{"busy", pillOAuth, func(a *App, f *fakeBackend) { a.busy.Set(map[string]bool{pillOAuth: true}) },
			func(s surface) string { return state(s, "Merge") }, "Merge disabled=false busy=true"},
		{"resolving", pillOAuth, func(a *App, f *fakeBackend) {
			setThread(f, pillOAuth, func(th *core.ThreadView) { th.Resolving = true })
			a.setThreads(f.Threads())
		}, func(s surface) string { return state(s, "Merge") }, "Merge disabled=false busy=true"},
		{"pull request link", pillOAuth, nil, func(s surface) string {
			if s.pill {
				s.root.ByRole(access.RoleLink, "Open #61 in your browser").Click()
			} else {
				s.root.ByRole(access.RoleButton, "#61").Click()
			}
			return ""
		}, `urls=["https://github.com/acme/web/pull/61"]`},
		{"title", pillCSV, nil, func(s surface) string { return click(s, "Open Add CSV export to reports") }, `open="` + pillCSV + `/conversation"`},
		{"review changes", pillCSV, nil, func(s surface) string { return click(s, "Review changes") }, `open="` + pillCSV + `/changes"`},
		{"request changes", pillCSV, nil, func(s surface) string {
			if s.pill {
				more(s, "Request changes")
				return ""
			}
			return click(s, "Request changes")
		}, `draft="Changes for “Add CSV export to reports”: "`},
		{"done", formsID, func(a *App, f *fakeBackend) {
			setThread(f, formsID, func(th *core.ThreadView) { th.CanDone = true })
			a.setThreads(f.Threads())
		}, func(s surface) string { more(s, "Done"); return "" }, "done=[" + formsID + "]"},
		{"mark done without landing", formsID, func(a *App, f *fakeBackend) {
			setThread(f, formsID, func(th *core.ThreadView) { th.CanDone, th.Unlanded = true, true })
			a.setThreads(f.Threads())
		}, func(s surface) string { more(s, "Mark done…"); return "" }, `confirm="Mark it done without landing?"`},
		{"discard", pillCSV, nil, func(s surface) string { more(s, "Discard thread…"); return "" }, `confirm="Discard this thread?"`},
	}
	defer func(f func(*ui.Context, string) error) { openURL = f }(openURL)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var effects [2]string
			for i, pill := range []bool{true, false} {
				var urls []string
				openURL = func(_ *ui.Context, url string) error { urls = append(urls, url); return nil }
				f := mergePillBackend([]string{c.id}, false)
				a, u := pillUI(t, f, 0, nil)
				if c.setup != nil {
					c.setup(a, f)
					u.Settle()
				}
				s := surface{u: u, pill: pill}
				if pill {
					openPill(t, u)
					s.root = u.Get("merge-pill-row-" + c.id)
				} else {
					s.root = u.Get("proposal-" + c.id)
				}
				before := pillEffect(a, f, &urls)
				extra := c.do(s)
				got := extra
				deadline := time.Now().Add(5 * time.Second)
				for {
					u.Settle()
					if e := pillEffect(a, f, &urls); extra != "" || e != before || time.Now().After(deadline) {
						got = strings.TrimSpace(extra + " " + e)
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if !strings.Contains(got, c.want) {
					t.Fatalf("on the %s: got %s, want %s", map[bool]string{true: "pill", false: "card"}[pill], got, c.want)
				}
				effects[i] = got
				u.Close()
			}
			if effects[0] != effects[1] {
				t.Fatalf("the pill and the card differ:\npill: %s\ncard: %s", effects[0], effects[1])
			}
		})
	}
}

// Show card in chat scrolls the conversation back to the thread's card, and closes the list.
func TestMergePillShowsTheCard(t *testing.T) {
	f := mergePillBackend([]string{pillCSV}, true)
	for i := 0; i < 8; i++ {
		f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: fmt.Sprintf("And now? (%d)", i)})
		f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{Text: "Still at it. " + strings.Repeat("The tests take a while. ", 12)})
	}
	_, u := pillUI(t, f, 0, nil)
	if u.Has("proposal-" + pillCSV) {
		t.Fatal("the card is still built: the chat hasn't moved past it")
	}
	openPill(t, u)
	u.Click("merge-pill-more-" + pillCSV)
	u.Settle()
	u.ByRole(access.RoleMenuItem, "Show card in chat").Click()
	u.WaitFor("the card to scroll into view", func() bool {
		u.Advance(100 * time.Millisecond)
		return u.Has("proposal-" + pillCSV)
	})
	if u.Has("merge-pill-list") {
		t.Fatal("the list stays open over the card")
	}
}

// The list follows the cards while it is open: a pull request opened turns Open PR into Merge, a new
// card adds a row, a merged thread's row goes, and with none left the pill goes.
func TestMergePillFollowsTheCards(t *testing.T) {
	f := mergePillBackend([]string{formsID}, true)
	a, u := pillUI(t, f, 0, nil)
	openPill(t, u)
	row := func() *uitest.Element { return u.Get("merge-pill-row-" + formsID) }
	if !row().HasRole(access.RoleGroup, "Open PR") {
		t.Fatal("the row doesn't offer Open PR")
	}
	setThread(f, formsID, func(th *core.ThreadView) {
		pr := &forge.PR{Kind: forge.GitHub, Number: 7, URL: "https://github.com/acme/web/pull/7", State: forge.StateOpen,
			Checks: forge.ChecksPassing, Counts: forge.CheckCounts{Total: 3, Passed: 3}, Mergeable: true}
		th.PR, th.CanOpenPR, th.CanMerge = pr, false, true
		th.Parts = slices.Clone(th.Parts)
		th.Parts[0].PR, th.Parts[0].CanOpenPR, th.Parts[0].CanMerge = pr, false, true
	})
	a.setThreads(f.Threads())
	u.Settle()
	if !row().HasRole(access.RoleButton, "Merge") || !row().HasRole(access.RoleLink, "Open #7 in your browser") {
		t.Fatal("the row didn't follow its pull request opening")
	}

	f.add(core.OrchestratorID, core.ItemProposal, core.ProposalData{ThreadID: pillOAuth, Summary: "Ready."})
	items, _ := f.Items(context.Background(), core.OrchestratorID, 500)
	a.itemsOf(core.OrchestratorID).Set(items)
	u.Settle()
	if !u.Has("merge-pill-row-"+pillOAuth) || u.Get("merge-pill").Name() != "2 to land: 2 merge cards" {
		t.Fatal("a new card didn't join the open list")
	}

	setThread(f, formsID, func(th *core.ThreadView) { th.Status, th.CanMerge = core.StatusMerged, false })
	a.setThreads(f.Threads())
	u.Settle()
	if u.Has("merge-pill-row-"+formsID) || !u.Has("merge-pill-list") {
		t.Fatal("a merged thread's row stayed, or the list closed")
	}
	setThread(f, pillOAuth, func(th *core.ThreadView) { th.Status, th.DoneAt = core.StatusMerged, time.Now() })
	a.setThreads(f.Threads())
	u.Settle()
	if u.Has("merge-pill") {
		t.Fatal("the pill stayed with no card left")
	}
}

// The pill is in Solo's chat only: beside an open thread it stays in Solo's chat, and a compact window
// showing a thread has none.
func TestMergePillOnlyInSoloChat(t *testing.T) {
	f := mergePillBackend(pillThree, true)
	a, u := pillUI(t, f, 0, nil)
	a.openThread(formsID, "conversation")
	u.WaitFor("the panel to slide in", func() bool { return a.panelAnim.Peek() == 1 })
	u.Settle()
	if !u.Has("merge-pill") {
		t.Fatal("the pill went when a thread opened beside Solo's chat")
	}
	edge := u.Get("draft-"+formsID+"-row").Bounds().X - space6 // the thread's panel, by its composer
	if b := u.Get("merge-pill").Bounds(); b.X+b.W > edge {
		t.Fatalf("the pill (%v) is over the thread's panel at %v", b, edge)
	}
	openPill(t, u)
	if b := u.Get("merge-pill-list").Bounds(); b.X < 0 || b.X+b.W > edge {
		t.Fatalf("the open list (%v) leaves Solo's chat", b)
	}

	a, u = pillUI(t, mergePillBackend(pillThree, true), 680, nil)
	if !u.Has("merge-pill") {
		t.Fatal("a compact window's Solo chat has no pill")
	}
	a.openThread(formsID, "conversation")
	u.Settle()
	if u.Has("merge-pill") {
		t.Fatal("a compact window showing a thread has the pill")
	}
}

// At a phone's width, with touch, the pill opens with a tap, rows give the title a line of its own,
// and every action still shows.
func TestMergePillPhone(t *testing.T) {
	f := mergePillBackend(pillMany, true)
	_, dark := Themes()
	a := New(f)
	a.Touch = true
	u := uitest.New(t, a.Root, uitest.Size(390, 844), uitest.Theme(dark), uitest.Bindings(Bindings()...),
		uitest.Settings(ui.SystemSettings{Dark: true, CaptionButtons: ":", CaretBlink: 530 * time.Millisecond, CaretWidth: 1}))
	t.Cleanup(u.Close)
	started(a, u)
	u.Settle()
	openPill(t, u)
	row := u.Get("merge-pill-row-" + pillCache)
	title, list := u.Get("merge-pill-title-"+pillCache).Bounds(), u.Get("merge-pill-list").Bounds()
	if title.W < list.W/2 {
		t.Fatalf("the title gets %v of a %v list", title.W, list.W)
	}
	for _, name := range []string{"Open PR", "Review changes", "More"} {
		if !row.HasRole(access.RoleButton, name) {
			t.Fatalf("the row has no %q", name)
		}
	}
	if list.X < 0 || list.X+list.W > 390 {
		t.Fatalf("the list (%v) leaves the screen", list)
	}
}
