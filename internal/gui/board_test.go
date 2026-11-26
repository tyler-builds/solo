package gui

import (
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/store"
)

// boardDemo turns the board on over the demo threads and gives them briefs, as the summary model
// would write them.
func boardDemo(a *App, f *fakeBackend) {
	f.settings.Board = true
	at := time.Now().Add(-time.Minute)
	briefs := map[string]core.Brief{
		"a1f3c9d2-oauth": {Turn: "- Found refresh tokens expire after one retry\n- Made the refresh path retry once on 401", At: at},
		"b7e21a90-limit": {Turn: "- Added a token bucket to the upload handler", Overall: "Uploads are limited; the limit itself is still open.", At: at},
		"c3d4e5f6-forms": {Turn: "- Moved the settings page to the new form components\n- Removed the old field wrappers; web tests pass",
			Overall: "The settings page runs on the new components and is ready to merge.", Need: core.BriefNeedReview, At: at},
		"d9a8b7c6-flaky": {Turn: "- Traced the flake to the retry timer racing the test clock\n- Two billing tests still fail",
			Overall: "The cause is known; the fix needs a fake clock in the worker.", Need: core.BriefNeedBlocked, At: at},
		"a0b1c2d3-audit": {Turn: "- Uploads are logged by the request logger in the upload handler\n- No file names are written anywhere",
			Overall: "Answered: nothing to change.", Need: core.BriefNeedReview, At: at},
		"b1b2b3b4-paginate": {Turn: "- Added cursor pagination to the audit log endpoint", Overall: "PR open; checks almost done.", Need: core.BriefNeedReview, At: at},
		"c1c2c3c4-csv":      {Turn: "- Added CSV export to the reports page", Overall: "One web test fails in CI.", Need: core.BriefNeedReview, At: at},
	}
	for i := range f.threads {
		th := &f.threads[i]
		if b, ok := briefs[th.ID]; ok {
			th.Brief = &b
		}
		if th.ID == "a1f3c9d2-oauth" {
			th.Now = "Running the auth test suite"
		}
	}
	// Board cards, with a lead: one planning, one building, and reviews waiting on the user.
	cards := map[string]core.CardState{
		"a1f3c9d2-oauth":  {Lead: "lead-oauth", Phase: core.CardBuilding, Ask: "OAuth tokens aren't refreshed when they expire mid-request. Fix it.", AskAt: at.Add(-5 * time.Minute)},
		"e1e2e3e4-readme": {Lead: "lead-readme", Phase: core.CardPlanning, Ask: "Update the install steps in the README for the new installer.", AskAt: at},
		"c3d4e5f6-forms": {Lead: "lead-forms", Phase: core.CardReview, Ask: "Move the settings page to the new form components, and delete the old field wrappers once nothing uses them.",
			AskAt: at.Add(-20 * time.Minute), Need: core.BriefNeedReview, ReviewAt: at,
			Review: "Settings now runs on the new `Form` components; the old `LegacyField` wrappers are gone.\n\n" +
				"- ✓ 6 files changed; all 214 web tests pass, and I checked the page renders the same\n" +
				"- ✓ Validation messages keep their old wording\n" +
				"- ⚠ The \"Danger zone\" section still uses an inline style; harmless, but worth a follow-up"},
		"d9a8b7c6-flaky": {Lead: "lead-flaky", Phase: core.CardReview, Ask: "The billing worker test is flaky. Make it reliable.", AskAt: at.Add(-30 * time.Minute),
			Need: core.BriefNeedInput, ReviewAt: at, Rounds: 2,
			Review: "The flake is the retry timer racing the test clock.\n\n- ✓ Reproduced it: fails about 1 run in 20\n- ⚠ Two fix attempts moved the race rather than removing it\n\nNeeds you: inject a fake clock into the worker (small API change), or drop the retry test?"},
	}
	for i := range f.threads {
		th := &f.threads[i]
		if cs, ok := cards[th.ID]; ok {
			th.Card = &cs
		}
	}
	a.cfg.Set(f.Settings())
	a.setThreads(f.Threads())
}

func worker(id, status string) core.ThreadView {
	return core.ThreadView{Thread: store.Thread{ID: id, Kind: store.KindWorker, Status: status}, IsWorker: true}
}

func TestBoardColumn(t *testing.T) {
	open := func(v core.ThreadView, checks string, canMerge, auto bool) core.ThreadView {
		pr := &forge.PR{State: forge.StateOpen, Checks: checks, AutoMerge: auto}
		v.PR, v.CanMerge = pr, canMerge
		v.Parts = []core.PartView{{Part: core.Part{PR: pr}}}
		return v
	}
	done := worker("d", core.StatusIdle)
	done.DoneAt = time.Now()
	cases := []struct {
		name string
		v    core.ThreadView
		busy bool
		want string
	}{
		{"queued", worker("q", core.StatusQueued), false, colTodo},
		{"starting", worker("s", core.StatusStarting), false, colTodo},
		{"running", worker("r", core.StatusRunning), false, colWorking},
		{"background", worker("b", core.StatusBackground), false, colWorking},
		{"question", worker("w", core.StatusWaiting), false, colYou},
		{"idle", worker("i", core.StatusIdle), false, colYou},
		{"failed", worker("f", core.StatusFailed), false, colYou},
		{"merging", worker("m", core.StatusIdle), true, colLanding},
		{"checks running", open(worker("c", core.StatusIdle), forge.ChecksPending, false, true), false, colLanding},
		{"checks passed, waits for Merge", open(worker("p", core.StatusIdle), forge.ChecksPassing, true, false), false, colYou},
		{"checks failed", open(worker("x", core.StatusIdle), forge.ChecksFailing, false, false), false, colYou},
		{"merged", worker("g", core.StatusMerged), false, colDone},
		{"marked done", done, false, colDone},
	}
	for _, c := range cases {
		if got := boardColumn(c.v, c.busy); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestReviewLinesKeepNeedsYou(t *testing.T) {
	v := worker("a", core.StatusIdle)
	v.Card = &core.CardState{Lead: "l", Phase: core.CardReview, Review: "Outcome.\n\n- ✓ one\n- ✓ two\n- ⚠ three\n\nNeeds you: pick A or B"}
	got := reviewLines(v)
	if len(got) != 3 || got[0] != "Outcome." || got[1] != "✓ one" || got[2] != "Needs you: pick A or B" {
		t.Fatalf("%q", got)
	}
}

func TestBriefLines(t *testing.T) {
	v := worker("a", core.StatusIdle)
	v.Brief = &core.Brief{Turn: "- one\n\n* two\n• three\n- four"}
	if got := briefLines(v); len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Fatalf("%q", got)
	}
}

func TestBoardShowsCardsAndAddsTasks(t *testing.T) {
	f := demoBackend()
	a := New(f)
	boardDemo(a, f)
	u := uitest.New(t, a.Root, uitest.Size(1440, 900))
	defer u.Close()
	u.Settle()
	for _, key := range []string{"board-col-todo", "board-col-working", "board-col-you", "board-col-landing", "board-col-done",
		"card-a1f3c9d2-oauth", "card-c3d4e5f6-forms", "card-e1e2e3e4-readme"} {
		if !u.Has(key) {
			t.Errorf("no %s", key)
		}
	}
	if u.Has("rail-filter") {
		t.Error("the rail shows beside the board")
	}

	u.Get("board-draft-area").Focus()
	u.Type("Add a dark mode toggle to Settings")
	u.Press("Enter")
	// The task is added off the UI thread (App.run).
	var created []core.WorkerSpec
	for i := 0; i < 100 && len(created) == 0; i++ {
		u.Advance(20 * time.Millisecond)
		time.Sleep(10 * time.Millisecond)
		f.mu.Lock()
		created = f.created
		f.mu.Unlock()
	}
	u.Settle()
	if len(created) != 1 || created[0].Prompt != "Add a dark mode toggle to Settings" || !created[0].FromUser || created[0].Repo != "/home/dev/code/web" {
		t.Fatalf("created %+v", created)
	}

	// A card with a lead opens over the board, with its review; one without opens its thread beside it.
	u.Click("card-c3d4e5f6-forms")
	u.Settle()
	if a.cardOpen.Peek() != "c3d4e5f6-forms" || !u.Has("card-view") || a.openID.Peek() != "" {
		t.Fatalf("card %q, thread %q", a.cardOpen.Peek(), a.openID.Peek())
	}
	a.closeCard()
	u.Settle()
	u.Click("card-b1b2b3b4-paginate")
	u.Settle()
	if a.openID.Peek() != "b1b2b3b4-paginate" || a.cardOpen.Peek() != "" {
		t.Fatalf("thread %q, card %q", a.openID.Peek(), a.cardOpen.Peek())
	}
}

func TestCardPromptSettings(t *testing.T) {
	f := demoBackend()
	a := New(f)
	a.settingsTab = "models"
	a.settings.Set(true)
	u := uitest.New(t, a.Root, uitest.Size(1440, 1400))
	defer u.Close()
	u.Settle()
	saved := func() core.Settings {
		// Settings save a moment (600ms of real time) after the last change.
		for i := 0; i < 20; i++ {
			u.Advance(100 * time.Millisecond)
			time.Sleep(60 * time.Millisecond)
		}
		u.Settle()
		return f.Settings()
	}
	if u.Has("card-reviewer-prompt-reset") {
		t.Fatal("Reset shows for the default prompt")
	}
	u.Get("card-reviewer-prompt").Focus()
	u.Press("Ctrl+End")
	u.Type(" Be blunt.")
	s := saved()
	if !strings.HasSuffix(s.CardReviewPrompt, "Be blunt.") || !strings.HasPrefix(s.CardReviewPrompt, "You are Solo, reviewing") || s.CardBriefPrompt != "" {
		t.Fatalf("saved %q / %q", s.CardBriefPrompt, s.CardReviewPrompt)
	}
	u.Get("card-reviewer-prompt-reset").Focus()
	u.Press("Enter")
	if s := saved(); s.CardReviewPrompt != "" {
		t.Fatalf("after reset %q", s.CardReviewPrompt)
	}
}
