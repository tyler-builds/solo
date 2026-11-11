package gui

import (
	"fmt"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/store"
)

// busyBackend is a heavy day: the demo afternoon plus many more threads across six repositories in
// every state, several open questions, a long to-do list, and a long chat with Solo full of cards.
// All names, repositories and people are made up.
func busyBackend() *fakeBackend {
	f := demoBackend()
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	const root = "/home/dev/code/"
	f.repos = append(f.repos, root+"mobile", root+"billing")
	worker := func(id, title, repo, status string, updated time.Duration, cost float64) core.ThreadView {
		v := core.ThreadView{Thread: store.Thread{ID: id, Kind: store.KindWorker, Title: title, RepoPath: root + repo,
			Branch: "solo/" + id[:8], BaseBranch: "main", BaseCommit: "abc123", WorktreePath: "/tmp/wt/" + id,
			Status: status, Model: "sonnet", CostUSD: cost, CreatedAt: ago(updated + time.Hour), UpdatedAt: ago(updated)},
			IsWorker: true, HasWorktree: true, Dir: "/tmp/wt/" + id, ActualModel: "claude-sonnet-5", Changed: 3}
		v.CanDiscard = true
		v.CanMerge = status == core.StatusIdle || status == core.StatusFailed || status == core.StatusInterrupted
		v.Forge = forge.GitHub
		return v
	}
	running := func(v core.ThreadView, started time.Duration, step core.ToolData, live string) core.ThreadView {
		v.InTurn, v.CanInterrupt, v.TurnStarted = true, true, ago(started)
		step.Done = false
		v.Step = &step
		v.Live = []core.LiveBlock{{Text: live}}
		return v
	}
	waiting := func(v core.ThreadView) core.ThreadView {
		v.PendingAsk, v.InTurn, v.CanMerge = true, true, false
		return v
	}
	pr := func(v core.ThreadView, n int, checks string, failed ...string) core.ThreadView {
		v.CanMerge, v.CanMergeLocal, v.CanOpenPR = checks == forge.ChecksPassing, false, false
		p := &forge.PR{Kind: forge.GitHub, Number: n, URL: fmt.Sprintf("https://github.com/acme/%s/pull/%d", repoLabel(v), n),
			State: forge.StateOpen, Checks: checks, Mergeable: checks == forge.ChecksPassing}
		for _, name := range failed {
			p.Failed = append(p.Failed, forge.Check{Name: name})
		}
		switch checks {
		case forge.ChecksFailing:
			p.Blocked = "checks failed"
			p.Counts = forge.CheckCounts{Total: 5 + len(failed), Passed: 5, Failed: len(failed)}
		case forge.ChecksPending:
			p.Blocked = "waiting for checks"
			p.Counts = forge.CheckCounts{Total: 7, Passed: 6, Pending: 1}
		case forge.ChecksPassing:
			p.Counts = forge.CheckCounts{Total: 7, Passed: 7}
		}
		v.PR = p
		return v
	}

	push := running(worker("a2a2a2a2-push", "Add push notification opt-in screen", "mobile", core.StatusRunning, 10*time.Second, 0.74),
		4*time.Minute+31*time.Second, tool("p9", "Bash", map[string]any{"command": "pnpm --filter mobile test notifications"}, ""),
		"The opt-in screen asks once after the second session. Running the notification tests now.")
	invoice := running(worker("a3a3a3a3-invoice", "Round invoice totals per line item", "billing", core.StatusRunning, 5*time.Second, 0.51),
		58*time.Second, tool("i9", "Edit", map[string]any{"file_path": "/wt/internal/invoice/totals.go"}, ""),
		"Rounding each line before summing changes 3 fixtures; updating them to the corrected cents.")
	tf := running(worker("a4a4a4a4-tf", "Split the Terraform state by environment", "infra", core.StatusRunning, 20*time.Second, 1.12),
		9*time.Minute+5*time.Second, tool("t9", "Bash", map[string]any{"command": "terraform plan -out staging.plan"}, ""),
		"Moving staging resources into their own state; the plan should show zero changes.")
	dark := waiting(worker("b2b2b2b2-darkmode", "Dark mode for the docs site", "docs", core.StatusWaiting, 4*time.Minute, 0.44))
	retry := waiting(worker("b3b3b3b3-retry", "Retry failed webhook deliveries", "billing", core.StatusWaiting, 7*time.Minute, 0.62))
	webhooks := pr(worker("c5c5c5c5-hooks", "Sign outgoing webhooks with HMAC", "billing", core.StatusIdle, 18*time.Minute, 0.93), 214, forge.ChecksPassing)
	locale := pr(worker("c6c6c6c6-locale", "Load translations lazily", "web", core.StatusIdle, 22*time.Minute, 0.58), 91, forge.ChecksPending)
	locale.PR.AutoMerge = true
	deeplink := pr(worker("c7c7c7c7-deeplink", "Handle expired magic-link deep links", "mobile", core.StatusIdle, 31*time.Minute, 0.67), 57,
		forge.ChecksFailing, "ios-build", "e2e (android)")
	sitemap := worker("c8c8c8c8-sitemap", "Generate a sitemap for the docs", "docs", core.StatusIdle, 14*time.Minute, 0.29)
	sitemap.CanOpenPR, sitemap.CanMergeLocal, sitemap.CanMerge = true, true, false
	cache := worker("d2d2d2d2-cache", "Cache tenant lookups in the API", "api", core.StatusInterrupted, 35*time.Minute, 0.4)
	cache.CanMerge = false
	alarms := worker("d3d3d3d3-alarms", "Add CPU alarms for the job runners", "infra", core.StatusFailed, 9*time.Minute, 0.36)
	alarms.Error = "terraform validate: Reference to undeclared resource aws_sns_topic.oncall in alarms.tf line 14."
	alarms.CanMerge = false
	csp := worker("e2e2e2e2-csp", "Tighten the docs Content-Security-Policy", "docs", core.StatusMerged, 50*time.Minute, 0.21)
	csp.CanMerge, csp.CanDiscard, csp.CanDone = false, false, true
	csp.PR = &forge.PR{Kind: forge.GitHub, Number: 40, URL: "https://github.com/acme/docs/pull/40", State: forge.StateMerged, Checks: forge.ChecksPassing}
	ledger := worker("e3e3e3e3-ledger", "Backfill ledger entries for March", "billing", core.StatusMerged, 70*time.Minute, 0.88)
	ledger.CanMerge, ledger.CanDiscard, ledger.CanDone = false, false, true
	queued := worker("e4e4e4e4-icons", "Replace PNG icons with SVG", "mobile", core.StatusQueued, 2*time.Second, 0)
	queued.HasWorktree, queued.WorktreePath, queued.Branch = false, "", ""
	f.threads = append(f.threads, push, invoice, tf, dark, retry, webhooks, locale, deeplink, sitemap, cache, alarms, csp, ledger, queued)

	f.pending[dark.ID] = []core.PendingQuestion{{ToolUseID: "toolu_q2", Questions: []agent.Question{{
		Question: "Should dark mode follow the system setting, or default to light with a toggle?", Header: "Theme",
		Options: []agent.QuestionOption{
			{Label: "Follow the system", Description: "Uses prefers-color-scheme; the toggle overrides it."},
			{Label: "Light with a toggle", Description: "Matches the marketing site; dark only when chosen."},
		}}}}}
	f.pending[retry.ID] = []core.PendingQuestion{{ToolUseID: "toolu_q3", Questions: []agent.Question{{
		Question: "How many times should a failed webhook be retried before it's marked dead?", Header: "Retries",
		Options: []agent.QuestionOption{
			{Label: "5 over an hour", Description: "Exponential backoff from 1 minute."},
			{Label: "8 over a day", Description: "Matches what most payment providers do."},
			{Label: "Until the endpoint is disabled", Description: "Never give up on its own."},
		}}}}}

	f.todos = append(f.todos[:4:4],
		core.Todo{ID: "t6", Text: "Ship push notification opt-in before the mobile release", Threads: []string{push.ID}},
		core.Todo{ID: "t7", Text: "Webhook retries and signing for billing", Threads: []string{retry.ID, webhooks.ID}},
		core.Todo{ID: "t8", Text: "Ask Priya about the invoice rounding rule"},
		core.Todo{ID: "t9", Text: "Dark mode for the docs site", Threads: []string{dark.ID}},
		f.todos[4],
		core.Todo{ID: "t10", Text: "Tighten the docs CSP", Threads: []string{csp.ID}, DoneAt: ago(50 * time.Minute), DoneBy: core.TodoByThread},
	)

	o := core.OrchestratorID
	upd := func(lines ...string) {
		text := "[thread update from Solo, not from the user]"
		for _, l := range lines {
			text += "\n- " + l
		}
		f.add(o, core.ItemUser, core.UserData{Synthetic: true, Text: text})
	}
	f.add(o, core.ItemUser, core.UserData{Text: "Next: sign billing's outgoing webhooks, retry the failed ones, and get the docs site a dark mode. Also split the Terraform state per environment."})
	f.add(o, core.ItemThinking, core.TextData{Text: "Four tasks, three repos. Webhook signing and retries touch the same sender, so they can't share a branch safely; keep them separate."})
	f.add(o, core.ItemTool, tool("b1", "Grep", map[string]any{"pattern": "func Deliver", "path": root + "billing"}, "internal/webhook/send.go:31"))
	f.add(o, core.ItemTool, tool("b2", "mcp__solo__spawn_thread", map[string]any{"repo": "billing", "title": "Sign outgoing webhooks with HMAC"}, "Started thread c5c5c5c5"))
	f.add(o, core.ItemTool, tool("b3", "mcp__solo__spawn_thread", map[string]any{"repo": "billing", "title": "Retry failed webhook deliveries"}, "Started thread b3b3b3b3"))
	f.add(o, core.ItemTool, tool("b4", "mcp__solo__spawn_thread", map[string]any{"repo": "docs", "title": "Dark mode for the docs site"}, "Started thread b2b2b2b2"))
	f.add(o, core.ItemTool, tool("b5", "mcp__solo__spawn_thread", map[string]any{"repo": "infra", "title": "Split the Terraform state by environment"}, "Started thread a4a4a4a4"))
	f.add(o, core.ItemAssistant, core.TextData{Text: "Started four threads: **webhook signing** and **retries** in `billing` (separate branches, since both touch `send.go`), **dark mode** in `docs`, and the **Terraform split** in `infra`."})
	upd(`Thread c7c7c7c7 "Handle expired magic-link deep links" (mobile): checks failed on PR #57 (https://github.com/acme/mobile/pull/57): ios-build, e2e (android). The user handles failed checks themselves: tell them, and don't ask the thread to fix it.`,
		`Thread d3d3d3d3 "Add CPU alarms for the job runners" (infra) failed: terraform validate: Reference to undeclared resource aws_sns_topic.oncall.`)
	f.add(o, core.ItemAssistant, core.TextData{Text: "Two problems:\n\n- **Deep links** (mobile): PR #57 failed `ios-build` and `e2e (android)`. The iOS failure is a signing timeout on the runner, not the code; re-running it is likely enough.\n- **CPU alarms** (infra): `alarms.tf` points at an SNS topic that only exists in the prod state. It needs the topic moved to the shared module first."})
	f.add(o, core.ItemUser, core.UserData{Text: "Rerun the iOS build. For the alarms, wait until the Terraform split lands."})
	f.add(o, core.ItemTool, tool("b7", "mcp__solo__message_thread", map[string]any{"id": "c7c7c7c7", "text": "Re-run the ios-build check"}, "Sent"))
	upd(`Thread b2b2b2b2 "Dark mode for the docs site" (docs) is asking a question (see get_thread).`,
		`Thread b3b3b3b3 "Retry failed webhook deliveries" (billing) is asking a question (see get_thread).`,
		`Thread c6c6c6c6 "Load translations lazily" (web): the user opened PR #91 (https://github.com/acme/web/pull/91). Auto-merge is on, so it merges once its required checks pass.`)
	f.add(o, core.ItemAssistant, core.TextData{Text: "Three threads are waiting on you: the **docs theme default**, the **webhook retry schedule**, and the **upload rate limiter's store**. Their questions are pinned below."})
	upd(`Thread e2e2e2e2 "Tighten the docs Content-Security-Policy" (docs): the user opened PR #40 (https://github.com/acme/docs/pull/40). Auto-merge is on, so it merges once its required checks pass.`,
		`Thread c5c5c5c5 "Sign outgoing webhooks with HMAC" (billing) finished its turn. Summary: Signs every delivery with an HMAC-SHA256 header; 96 tests pass.`)
	f.add(o, core.ItemTool, tool("b6", "mcp__solo__get_diff", map[string]any{"id": "c5c5c5c5"}, "diff --git …"))
	f.add(o, core.ItemProposal, core.ProposalData{ThreadID: webhooks.ID, Title: webhooks.Title,
		Summary: "Adds an `X-Acme-Signature` header to each delivery and a verification helper in the SDK docs. Checks passed on PR #214."})
	upd(`Thread e2e2e2e2 "Tighten the docs Content-Security-Policy" (docs): PR #40 merged into main.`,
		`Thread e3e3e3e3 "Backfill ledger entries for March" (billing) is merged into main.`)
	f.add(o, core.ItemAssistant, core.TextData{Text: "The CSP change and the March ledger backfill both landed. I ticked **Tighten the docs CSP** off the list."})
	// The OAuth thread a little further along: a failed run, a fix, and a note from the user.
	oa := "a1f3c9d2-oauth"
	last := f.items[oa][len(f.items[oa])-1]
	f.items[oa] = f.items[oa][:len(f.items[oa])-1]
	failed := tool("o6", "Bash", map[string]any{"command": "pnpm test src/auth"}, "FAIL src/auth/refresh.test.ts > retries once after a refresh\nexpected 1 call to refresh, got 3")
	failed.IsError = true
	f.add(oa, core.ItemTool, failed)
	f.add(oa, core.ItemThinking, core.TextData{Text: "The mock answers 401 every time, so the retry loops. The retried request must not retry again."})
	f.add(oa, core.ItemTool, tool("o7", "Edit", map[string]any{"file_path": "/wt/src/auth/refresh.ts"}, "Updated"))
	f.add(oa, core.ItemAssistant, core.TextData{Text: "One test failed: the retried request could retry again, so a token that stays invalid looped. The retry is now marked on the request and happens at most once."})
	f.add(oa, core.ItemUser, core.UserData{Text: "Also make sure a 403 doesn't trigger a refresh."})
	f.add(oa, core.ItemAssistant, core.TextData{Text: "Right: a 403 means the token is valid but lacks the scope, so refreshing can't help. I'll limit the retry to 401 and add a test for 403."})
	f.add(oa, core.ItemTool, tool("o8", "Edit", map[string]any{"file_path": "/wt/src/auth/refresh.test.ts"}, "Updated"))
	f.nextID++
	last.ID = f.nextID
	f.items[oa] = append(f.items[oa], last)

	// The rate limiter's own conversation, up to its open question.
	l := "b7e21a90-limit"
	f.add(l, core.ItemNotice, core.NoticeData{Level: "info", Text: "Working on branch solo/b7e21a90 from main."})
	f.add(l, core.ItemUser, core.UserData{From: core.FromOrchestrator, Text: "In api, rate limit POST /api/upload to 10 uploads per minute per user. Return 429 with Retry-After. Add tests."})
	f.add(l, core.ItemTool, tool("l1", "Grep", map[string]any{"pattern": "upload", "path": "internal/http"}, "internal/http/routes.go:57"))
	f.add(l, core.ItemTool, tool("l2", "Read", map[string]any{"file_path": "/wt/internal/http/routes.go"}, "…"))
	f.add(l, core.ItemTool, tool("l3", "Read", map[string]any{"file_path": "/wt/internal/session/redis.go"}, "…"))
	f.add(l, core.ItemAssistant, core.TextData{Text: "Uploads go through `routes.go:57`. The API already has a Redis client for sessions (`internal/session/redis.go`), so a shared limiter is cheap to add. Before I write it, one decision:"})
	f.add(l, core.ItemQuestion, core.QuestionData{ToolUseID: "toolu_q1", Questions: f.pending[l][0].Questions})
	return f
}
