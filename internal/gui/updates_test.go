package gui

import (
	"testing"

	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/store"
)

func TestParseUpdates(t *testing.T) {
	_, th := Themes()
	info := ui.Hex(pal(th).info)
	a := &App{threads: ui.NewSignal([]core.ThreadView{{Thread: store.Thread{ID: "abcd1234-full-id"}}})}
	const head = `Thread abcd1234 "Fix \"login\" bug" (web)`
	for _, c := range []struct {
		name, line, what string
		icon             *icons.Icon
		color            ui.Color
	}{
		{"finished", head + " finished its turn. Summary: Fixed the **retry**. Tests pass.", "finished · Fixed the retry", icons.CircleCheck, th.Success},
		{"background", head + ` ended its turn but isn't finished: it waits on work it runs in the background (command "Run benchmarks", running 4m), and carries on by itself when that ends. Don't review it or propose landing yet; you hear when its next turn finishes. Summary so far: Waiting.`, "waiting on background work · Run benchmarks", icons.Hourglass, th.TextMuted},
		{"resolved", head + " resolved the conflicts its merge had; Solo is merging it now, so don't propose it again.", "resolved its merge conflicts", icons.GitMerge, info},
		{"failed", head + " failed: exit status 1", "failed · exit status 1", icons.CircleAlert, th.Danger},
		{"question", head + " is asking a question (see get_thread).", "has a question", icons.MessageCircleQuestionMark, th.Warning},
		{"interrupted", head + " was interrupted.", "was stopped", icons.CircleStop, th.TextSubtle},
		{"merged locally", head + " is merged into main.", "merged into main", icons.GitMerge, info},
		{"merged, old wording", head + " was merged into main by the user.", "merged into main", icons.GitMerge, info},
		{"PR merged", head + ": PR #3 merged into main.", "PR #3 merged into main", icons.GitMerge, info},
		{"MR merged", head + ": MR #12 merged into develop.", "MR #12 merged into develop", icons.GitMerge, info},
		{"every repo landed", head + ": every repository's change has landed.", "merged in every repository", icons.GitMerge, info},
		{"discarded", head + " was discarded by the user.", "was discarded", icons.Trash, th.TextSubtle},
		{"PR opened", head + ": the user opened PR #3 (https://github.com/o/web/pull/3).", "opened PR #3", icons.GitPullRequest, info},
		{"PR opened, auto-merge", head + ": the user opened PR #3 (https://github.com/o/web/pull/3). Auto-merge is on, so it merges once its required checks pass.", "opened PR #3 · auto-merge on", icons.GitPullRequest, info},
		{"PR opened, multi-repo", head + ": the user opened the api PR #4 (https://github.com/o/api/pull/4).", "opened api PR #4", icons.GitPullRequest, info},
		{"checks passed", head + ": checks passed on PR #3 (https://github.com/o/web/pull/3).", "checks passed on PR #3", icons.CircleCheck, th.Success},
		{"checks failed", head + ": checks failed on PR #3 (https://github.com/o/web/pull/3): lint (https://github.com/o/web/runs/1), test. The user handles failed checks themselves: tell them, and don't ask the thread to fix it.", "checks failed on PR #3 · lint, test", icons.CircleAlert, th.Danger},
		{"checks failed, fix", head + ": checks failed on MR #5 (https://gitlab.com/o/web/-/merge_requests/5): see the checks. Have the thread fix them with message_thread (it can read the failures with gh pr checks or glab ci view); Solo pushes its new commits to the MR.", "checks failed on MR #5 · see the checks", icons.CircleAlert, th.Danger},
		{"PR closed", head + ": PR #3 (https://github.com/o/web/pull/3) was closed without merging.", "PR #3 closed without merging", icons.GitPullRequestClosed, th.TextSubtle},
		{"listed", "- " + head + " is merged into main.", "merged into main", icons.GitMerge, info},
	} {
		us := a.parseUpdates(th, c.line)
		if len(us) != 1 {
			t.Errorf("%s: parsed %d updates, want 1", c.name, len(us))
			continue
		}
		u := us[0]
		if u.what != c.what || u.icon != c.icon || u.color != c.color {
			t.Errorf("%s: got what %q, icon %p, color %v; want %q, %p, %v", c.name, u.what, u.icon, u.color, c.what, c.icon, c.color)
		}
		if u.threadID != "abcd1234-full-id" || u.name != `Fix "login" bug` || u.repo != "web" {
			t.Errorf("%s: got thread %q, name %q, repo %q", c.name, u.threadID, u.name, u.repo)
		}
	}
	if us := a.parseUpdates(th, "Some other text\n"+head+" was interrupted.\n"); len(us) != 1 {
		t.Errorf("mixed text: parsed %d updates, want 1", len(us))
	}
}

func TestChecksOf(t *testing.T) {
	pr := func(checks string, c forge.CheckCounts, failed ...string) *forge.PR {
		p := &forge.PR{Kind: forge.GitHub, Number: 7, State: forge.StateOpen, Checks: checks, Counts: c}
		for _, n := range failed {
			p.Failed = append(p.Failed, forge.Check{Name: n})
		}
		return p
	}
	cases := []struct {
		name   string
		prs    []*forge.PR
		checks string
		text   string
	}{
		{"running", []*forge.PR{pr(forge.ChecksPending, forge.CheckCounts{Total: 7, Passed: 6, Pending: 1})},
			forge.ChecksPending, "Checks: 6/7 passed · 1 running"},
		{"just started", []*forge.PR{pr(forge.ChecksPending, forge.CheckCounts{Total: 4, Pending: 4})},
			forge.ChecksPending, "Checks: 4 running"},
		{"failed while others run", []*forge.PR{pr(forge.ChecksFailing, forge.CheckCounts{Total: 7, Passed: 4, Failed: 1, Pending: 2}, "lint")},
			forge.ChecksFailing, "Checks failed: 1 of 7 · 2 running"},
		{"failed", []*forge.PR{pr(forge.ChecksFailing, forge.CheckCounts{Total: 7, Passed: 5, Failed: 2}, "lint", "test")},
			forge.ChecksFailing, "Checks failed: 2 of 7"},
		{"passed", []*forge.PR{pr(forge.ChecksPassing, forge.CheckCounts{Total: 7, Passed: 7})},
			forge.ChecksPassing, "Checks passed"},
		{"no counts (GitLab)", []*forge.PR{pr(forge.ChecksPending, forge.CheckCounts{})},
			forge.ChecksPending, "Checks running"},
		{"just opened, none reported yet", []*forge.PR{pr(forge.ChecksWaiting, forge.CheckCounts{})},
			forge.ChecksWaiting, "Waiting for checks"},
		{"waiting in one repository, passed in another", []*forge.PR{
			pr(forge.ChecksPassing, forge.CheckCounts{Total: 3, Passed: 3}), pr(forge.ChecksWaiting, forge.CheckCounts{})},
			forge.ChecksWaiting, "Waiting for checks"},
		{"waiting in one repository, running in another", []*forge.PR{
			pr(forge.ChecksWaiting, forge.CheckCounts{}), pr(forge.ChecksPending, forge.CheckCounts{Total: 2, Pending: 2})},
			forge.ChecksPending, "Checks: 2 running"},
		{"none to run", []*forge.PR{pr(forge.ChecksNone, forge.CheckCounts{})},
			forge.ChecksPassing, "Checks passed"},
		{"summed across repositories", []*forge.PR{
			pr(forge.ChecksPassing, forge.CheckCounts{Total: 3, Passed: 3}),
			pr(forge.ChecksPending, forge.CheckCounts{Total: 5, Passed: 3, Pending: 2})},
			forge.ChecksPending, "Checks: 6/8 passed · 2 running"},
	}
	for _, c := range cases {
		got := checksOf(c.prs)
		if got.checks != c.checks || got.text != c.text {
			t.Errorf("%s: got %s %q, want %s %q", c.name, got.checks, got.text, c.checks, c.text)
		}
	}
}
