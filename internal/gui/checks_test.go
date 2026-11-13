package gui

import (
	"slices"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/store"
)

const (
	checksRunningID = "k1k2k3k4-running"
	checksPassedID  = "k5k6k7k8-passed"
	checksFailedID  = "k9k0k1k2-failed"
)

// checksPR is an open GitHub pull request whose checks stand at counts.
func checksPR(n int, counts forge.CheckCounts) *forge.PR {
	pr := &forge.PR{Kind: forge.GitHub, Number: n, URL: "https://github.com/acme/api/pull/1", State: forge.StateOpen,
		Counts: counts}
	switch {
	case counts.Failed > 0:
		pr.Checks, pr.Blocked = forge.ChecksFailing, "checks failed"
		pr.Failed = []forge.Check{{Name: "test (api)"}}
	case counts.Pending > 0:
		pr.Checks, pr.Blocked = forge.ChecksPending, "waiting for checks"
	default:
		pr.Checks, pr.Mergeable = forge.ChecksPassing, true
	}
	return pr
}

// checksThread is a settled worker in one repository with its pull request open, as core shows it:
// the pull request on its one part and on the thread.
func checksThread(id, title string, pr *forge.PR) core.ThreadView {
	now := time.Now()
	repo := "/home/dev/code/api"
	part := core.PartView{Part: core.Part{Repo: repo, Branch: "solo/" + id[:8], BaseBranch: "main", PR: pr},
		Name: "api", Changed: 3, Forge: forge.GitHub, CanMerge: pr.Mergeable}
	return core.ThreadView{Thread: store.Thread{ID: id, Kind: store.KindWorker, Title: title, Status: core.StatusIdle,
		RepoPath: repo, Branch: part.Branch, BaseBranch: "main", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute)},
		IsWorker: true, HasWorktree: true, Changed: 3, Forge: forge.GitHub, PR: pr, CanMerge: pr.Mergeable,
		Parts: []core.PartView{part}}
}

// checksUI shows the demo threads and three with pull requests open: checks running, passed, and
// failed.
func checksUI(t *testing.T) (*App, *fakeBackend, *uitest.UI) {
	t.Helper()
	f := demoBackend()
	f.threads = append(f.threads,
		checksThread(checksRunningID, "Cache the audit log query", checksPR(201, forge.CheckCounts{Total: 5, Passed: 3, Pending: 2})),
		checksThread(checksPassedID, "Index the audit log by user", checksPR(202, forge.CheckCounts{Total: 5, Passed: 5})),
		checksThread(checksFailedID, "Drop the legacy audit table", checksPR(203, forge.CheckCounts{Total: 5, Passed: 4, Failed: 1})))
	a, u := newUI(t, f)
	return a, f, u
}

// setChecks moves a thread's pull request to the counts, as a poll of the forge would, and pushes the
// threads to the UI.
func setChecks(a *App, f *fakeBackend, u *uitest.UI, id string, counts forge.CheckCounts) {
	f.mu.Lock()
	for i, th := range f.threads {
		if th.ID == id {
			pr := checksPR(th.PR.Number, counts)
			th.PR, th.CanMerge = pr, pr.Mergeable
			th.Parts = slices.Clone(th.Parts)
			th.Parts[0].PR, th.Parts[0].CanMerge = pr, pr.Mergeable
			f.threads[i] = th
		}
	}
	f.mu.Unlock()
	a.setThreads(f.Threads())
	u.Settle()
}

// railGroupOf is the title of the rail group whose header is the nearest above the thread's row.
func railGroupOf(t *testing.T, u *uitest.UI, id string) string {
	t.Helper()
	card := u.Get("card-" + id).Bounds()
	titles := map[string]bool{}
	for _, g := range railGroups {
		titles[g.title] = true
	}
	group, best := "", float32(-1)
	u.Tree().Walk(func(n *access.Node) bool {
		b := n.Bounds
		if titles[n.Name] && b.X >= card.X && b.X < card.X+card.W && b.Y < card.Y && b.Y > best {
			group, best = n.Name, b.Y
		}
		return true
	})
	return group
}

// openChecksThread opens a thread's panel and waits for it to slide in.
func openChecksThread(a *App, u *uitest.UI, id string) {
	a.openThread(id, "conversation")
	u.WaitFor("the panel to slide in", func() bool { return a.panelAnim.Peek() == 1 })
	u.Settle()
}

// A thread whose pull request's checks still run waits in its own group, just before In review; once
// they pass it moves to In review, and once one fails, to Failed.
func TestWaitingForChecksGroup(t *testing.T) {
	a, f, u := checksUI(t)
	a.railQuery.Set("audit") // keeps every group of the rail on screen
	u.Settle()
	for id, want := range map[string]string{
		checksRunningID: "Waiting for checks",
		checksPassedID:  "In review",
		checksFailedID:  "Failed",
	} {
		if got := railGroupOf(t, u, id); got != want {
			t.Errorf("%s is under %q, want %q", id, got, want)
		}
	}
	titles := make([]string, len(railGroups))
	for i, g := range railGroups {
		titles[i] = g.title
	}
	if w, r := slices.Index(titles, "Waiting for checks"), slices.Index(titles, "In review"); w < 0 || w != r-1 {
		t.Errorf("Waiting for checks is group %d and In review %d, want it just before", w, r)
	}
	if y, r := u.Get("card-"+checksRunningID).Bounds().Y, u.Get("card-"+checksPassedID).Bounds().Y; y >= r {
		t.Errorf("the thread waiting for checks (y %v) sits below the one in review (y %v)", y, r)
	}

	// The checks finish: passed, it moves to In review.
	setChecks(a, f, u, checksRunningID, forge.CheckCounts{Total: 5, Passed: 5})
	if got := railGroupOf(t, u, checksRunningID); got != "In review" {
		t.Errorf("with its checks passed, the thread is under %q, want In review", got)
	}
	// Run again and one fails: it moves to Failed.
	setChecks(a, f, u, checksRunningID, forge.CheckCounts{Total: 5, Passed: 3, Pending: 2})
	if got := railGroupOf(t, u, checksRunningID); got != "Waiting for checks" {
		t.Errorf("with its checks running again, the thread is under %q, want Waiting for checks", got)
	}
	setChecks(a, f, u, checksRunningID, forge.CheckCounts{Total: 5, Passed: 4, Failed: 1})
	if got := railGroupOf(t, u, checksRunningID); got != "Failed" {
		t.Errorf("with a check failed, the thread is under %q, want Failed", got)
	}
}

// The open thread's header counts its pull request's checks, and follows them as they change.
func TestThreadHeaderCountsChecks(t *testing.T) {
	a, f, u := checksUI(t)
	for _, c := range []struct{ id, want string }{
		{checksRunningID, "3 passed · 2 running"},
		{checksPassedID, "5 passed"},
		{checksFailedID, "4 passed · 1 failed"},
	} {
		openChecksThread(a, u, c.id)
		if got := u.Get("thread-checks-" + c.id).Text(); got != c.want {
			t.Errorf("%s header checks %q, want %q", c.id, got, c.want)
		}
	}

	openChecksThread(a, u, checksRunningID)
	key := "thread-checks-" + checksRunningID
	setChecks(a, f, u, checksRunningID, forge.CheckCounts{Total: 5, Passed: 4, Pending: 1})
	if got := u.Get(key).Text(); got != "4 passed · 1 running" {
		t.Errorf("after one more check passed, the header says %q", got)
	}
	setChecks(a, f, u, checksRunningID, forge.CheckCounts{Total: 5, Passed: 4, Failed: 1})
	if got := u.Get(key).Text(); got != "4 passed · 1 failed" {
		t.Errorf("after the last check failed, the header says %q", got)
	}
}

// A thread without an open pull request, or one with no checks, has no checks in its header; a
// narrow panel keeps only their mark.
func TestThreadHeaderChecksWhenNone(t *testing.T) {
	a, f, u := checksUI(t)
	openChecksThread(a, u, "c3d4e5f6-forms") // no pull request yet
	if u.Has("thread-checks-c3d4e5f6-forms") {
		t.Error("a thread without a pull request shows checks")
	}
	openChecksThread(a, u, checksPassedID)
	setChecks(a, f, u, checksPassedID, forge.CheckCounts{})
	f.mu.Lock()
	for i := range f.threads {
		if f.threads[i].ID == checksPassedID {
			pr := *f.threads[i].PR // a new value, as core gives each snapshot its own
			pr.Checks = forge.ChecksNone
			f.threads[i].PR = &pr
			f.threads[i].Parts = slices.Clone(f.threads[i].Parts)
			f.threads[i].Parts[0].PR = &pr
		}
	}
	f.mu.Unlock()
	a.setThreads(f.Threads())
	u.Settle()
	if u.Has("thread-checks-" + checksPassedID) {
		t.Error("a pull request without checks shows checks in the header")
	}

	openChecksThread(a, u, checksRunningID)
	a.panelSize.Set(ui.Px(headerChecksMinWidth - 40))
	u.Settle()
	if got := u.Get("thread-checks-" + checksRunningID).Text(); got != "" {
		t.Errorf("a narrow panel spells out the checks: %q", got)
	}
}

// A pull request just opened, with no checks reported yet, waits for them: under Waiting for checks,
// its header and merge card say so rather than that they passed, and it can't merge. Once they're
// queued they run.
func TestWaitingForChecksJustOpened(t *testing.T) {
	a, f, u := checksUI(t)
	waiting := &forge.PR{Kind: forge.GitHub, Number: 204, URL: "https://github.com/acme/api/pull/4", State: forge.StateOpen,
		Checks: forge.ChecksWaiting, Blocked: "waiting for checks"}
	const id = "k3k4k5k6-waiting"
	f.mu.Lock()
	f.threads = append(f.threads, checksThread(id, "Trim the audit log", waiting))
	f.mu.Unlock()
	a.setThreads(f.Threads())
	a.railQuery.Set("audit")
	u.Settle()
	if got := railGroupOf(t, u, id); got != "Waiting for checks" {
		t.Errorf("a pull request waiting for its checks is under %q, want Waiting for checks", got)
	}
	th, _ := a.thread(id)
	if cs, ok := mergeChecks(th); !ok || cs.checks != forge.ChecksWaiting || cs.text != "Waiting for checks" {
		t.Errorf("merge card checks %+v, want waiting for checks", cs)
	}
	if th.CanMerge {
		t.Error("can merge before its checks started")
	}
	openChecksThread(a, u, id)
	key := "thread-checks-" + id
	if got := u.Get(key).Text(); got != "Waiting for checks" {
		t.Errorf("header checks %q, want Waiting for checks", got)
	}

	setChecks(a, f, u, id, forge.CheckCounts{Total: 3, Pending: 3})
	if got := u.Get(key).Text(); got != "3 running" {
		t.Errorf("once queued, the header says %q, want 3 running", got)
	}
}

func TestChecksTally(t *testing.T) {
	for _, c := range []struct {
		counts forge.CheckCounts
		want   string
	}{
		{forge.CheckCounts{Total: 5, Passed: 3, Pending: 2}, "3 passed · 2 running"},
		{forge.CheckCounts{Total: 4, Pending: 4}, "4 running"},
		{forge.CheckCounts{Total: 7, Passed: 4, Failed: 1, Pending: 2}, "4 passed · 1 failed · 2 running"},
		{forge.CheckCounts{Total: 5, Passed: 5}, "5 passed"},
		{forge.CheckCounts{}, ""}, // GitLab's one pipeline: no counts
	} {
		if got := checksOf([]*forge.PR{checksPR(1, c.counts)}).tally(); got != c.want {
			t.Errorf("%+v: tally %q, want %q", c.counts, got, c.want)
		}
	}
}
