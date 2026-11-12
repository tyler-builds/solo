package gui

import (
	"slices"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/store"
)

const mergeThreadID = "m1m2m3m4-merge"

// mergePart is one repository of mergeThread: changed files, and its pull request or none.
func mergePart(name string, changed int, k forge.Kind, pr *forge.PR) core.PartView {
	p := core.PartView{Part: core.Part{Repo: "/home/dev/code/" + name, Branch: "solo/m1m2m3m4", BaseBranch: "main", PR: pr},
		Name: name, Changed: changed, Forge: k}
	prOpen := pr != nil && pr.State == forge.StateOpen
	p.CanOpenPR = changed != 0 && !prOpen && k != forge.None
	p.CanMergeLocal = changed != 0 && !prOpen
	p.CanMerge = changed != 0 && ((prOpen && pr.Mergeable) || (!prOpen && k == forge.None))
	return p
}

func openPR(name string, n int, mergeable bool) *forge.PR {
	return &forge.PR{Kind: forge.GitHub, Number: n, URL: "https://github.com/acme/" + name + "/pull/1", State: forge.StateOpen,
		Checks: forge.ChecksPassing, Mergeable: mergeable}
}

// mergeUI shows a settled worker in the given repositories, its panel open and a proposal to merge it
// in Solo's chat.
func mergeUI(t *testing.T, parts ...core.PartView) (*fakeBackend, *uitest.UI) {
	t.Helper()
	f := demoBackend()
	now := time.Now()
	th := core.ThreadView{Thread: store.Thread{ID: mergeThreadID, Kind: store.KindWorker, Title: "Keep composer focus",
		Status: core.StatusIdle, RepoPath: parts[0].Repo, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute)},
		IsWorker: true, HasWorktree: true, Parts: parts}
	for _, p := range parts {
		th.CanOpenPR = th.CanOpenPR || p.CanOpenPR
		th.CanMerge = th.CanMerge || p.CanMerge
		th.CanMergeLocal = th.CanMergeLocal || p.CanMergeLocal
		th.Changed += p.Changed
	}
	f.threads = append(f.threads, th)
	f.add(core.OrchestratorID, core.ItemProposal, core.ProposalData{ThreadID: mergeThreadID, Title: th.Title, Summary: "Ready to land."})
	a, u := newUI(t, f)
	a.openThread(mergeThreadID, "conversation")
	u.WaitFor("the panel to slide in", func() bool { return a.panelAnim.Peek() == 1 })
	u.Settle()
	return f, u
}

// buttons counts the buttons named name: one in the thread's header and one on the merge card.
func buttons(u *uitest.UI, name string) int {
	n := 0
	u.Tree().Walk(func(nd *access.Node) bool {
		if nd.Role == access.RoleButton && nd.Name == name {
			n++
		}
		return true
	})
	return n
}

func waitMergedRepo(t *testing.T, f *fakeBackend, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := slices.Clone(f.mergedRepos)
		f.mu.Unlock()
		if slices.Contains(got, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("merged %q, want %q", f.mergedRepos, want)
}

// A thread in two repositories where only one changed and has a pull request names that pull
// request, and merges only it; it never says "Merge all". Its merge card names the repository; its
// header, whose chips name it, gives the number alone.
func TestMergeOnePRNamesIt(t *testing.T) {
	f, u := mergeUI(t, mergePart("solo", 3, forge.GitHub, openPR("solo", 5, true)), mergePart("jamb", 0, forge.GitHub, nil))
	if card, header := buttons(u, "Merge solo #5"), buttons(u, "Merge #5"); card != 1 || header != 1 {
		t.Fatalf("%d buttons say Merge solo #5 and %d Merge #5, want 1 (card) and 1 (header)", card, header)
	}
	if buttons(u, "Merge all") > 0 || u.HasRole(access.RoleGroup, "Merge all") {
		t.Fatal(`one pull request says "Merge all"`)
	}
	u.ByRole(access.RoleButton, "Merge solo #5").Click()
	waitMergedRepo(t, f, mergeThreadID+" /home/dev/code/solo")
}

// One pull request that can't merge yet is still named, disabled, and says why.
func TestMergeOneBlockedPRNamesIt(t *testing.T) {
	pr := openPR("solo", 5, false)
	pr.Blocked = "checks failed"
	_, u := mergeUI(t, mergePart("solo", 3, forge.GitHub, pr), mergePart("jamb", 0, forge.GitHub, nil))
	for _, name := range []string{"Merge solo #5", "Merge #5"} { // the card's, the header's
		if b := u.ByRole(access.RoleButton, name); !b.Disabled() {
			t.Fatalf("a blocked pull request's button %q is enabled", name)
		}
	}
	if buttons(u, "Merge all") > 0 || buttons(u, "Merge") > 0 {
		t.Fatal("a blocked pull request isn't named")
	}
}

// Two pull requests ready to merge are "Merge all", with each one on its own in the menu.
func TestMergeTwoPRsIsMergeAll(t *testing.T) {
	f, u := mergeUI(t, mergePart("solo", 3, forge.GitHub, openPR("solo", 5, true)),
		mergePart("jamb", 2, forge.GitHub, openPR("jamb", 9, true)), mergePart("docs", 0, forge.GitHub, nil))
	if n := buttons(u, "Merge all"); n != 2 {
		t.Fatalf("%d buttons say Merge all, want 2 (header and card)", n)
	}
	u.ByRole(access.RoleGroup, "Merge all").ByRole(access.RoleButton, "More options").Click()
	u.Settle()
	for _, item := range []string{"Merge all", "Merge solo #5", "Merge jamb #9"} {
		if !u.HasRole(access.RoleMenuItem, item) {
			t.Fatalf("the menu has no %q", item)
		}
	}
	if u.HasRole(access.RoleMenuItem, "Merge docs locally") {
		t.Fatal("the menu offers a repository with nothing to merge")
	}
	u.ByRole(access.RoleMenuItem, "Merge all").Click()
	waitMergedRepo(t, f, mergeThreadID+" ")
}

// Of two pull requests, with one ready, the button names the ready one; the menu's "Merge all" waits
// for both.
func TestMergeOneOfTwoReadyNamesIt(t *testing.T) {
	_, u := mergeUI(t, mergePart("solo", 3, forge.GitHub, openPR("solo", 5, true)),
		mergePart("jamb", 2, forge.GitHub, &forge.PR{Kind: forge.GitHub, Number: 9, State: forge.StateOpen, Blocked: "waiting for checks"}))
	if card, header := buttons(u, "Merge solo #5"), buttons(u, "Merge #5"); card != 1 || header != 1 {
		t.Fatalf("%d buttons say Merge solo #5 and %d Merge #5, want 1 (card) and 1 (header)", card, header)
	}
	u.ByRole(access.RoleGroup, "Merge solo #5").ByRole(access.RoleButton, "More options").Click()
	u.Settle()
	if !u.ByRole(access.RoleMenuItem, "Merge all").Disabled() {
		t.Fatal(`"Merge all" is enabled with one pull request blocked`)
	}
	if !u.ByRole(access.RoleMenuItem, "Merge jamb #9 · waiting for checks").Disabled() {
		t.Fatal("the blocked pull request is enabled")
	}
}

// Without a remote, only the repositories with commits count: one is named and merged locally, two
// are "Merge all".
func TestMergeLocalCountsReposWithCommits(t *testing.T) {
	f, u := mergeUI(t, mergePart("solo", 3, forge.None, nil), mergePart("jamb", 0, forge.None, nil))
	if n := buttons(u, "Merge solo locally"); n != 2 {
		t.Fatalf("%d buttons say Merge solo locally, want 2", n)
	}
	if buttons(u, "Merge all") > 0 {
		t.Fatal(`one branch to merge says "Merge all"`)
	}
	u.ByRole(access.RoleButton, "Merge solo locally").Click()
	waitMergedRepo(t, f, mergeThreadID+" /home/dev/code/solo")

	_, u = mergeUI(t, mergePart("solo", 3, forge.None, nil), mergePart("jamb", 1, forge.None, nil))
	if n := buttons(u, "Merge all"); n != 2 {
		t.Fatalf("%d buttons say Merge all, want 2", n)
	}
}
