package gui

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/store"
)

// fakeBackend serves canned data for screenshots and UI tests.
type fakeBackend struct {
	mu       sync.Mutex
	threads  []core.ThreadView
	items    map[string][]core.Item
	pending  map[string][]core.PendingQuestion
	changes  map[string][]git.FileChange
	diffs    map[string]string
	settings core.Settings
	repos    []string
	sent     []string
	merged   []string
	nextID   int64
}

func (f *fakeBackend) Threads() []core.ThreadView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.ThreadView(nil), f.threads...)
}

func (f *fakeBackend) Thread(id string) (core.ThreadView, bool) {
	for _, t := range f.Threads() {
		if t.ID == id {
			return t, true
		}
	}
	return core.ThreadView{}, false
}

func (f *fakeBackend) Items(_ context.Context, id string, _ int) ([]core.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Item(nil), f.items[id]...), nil
}

func (f *fakeBackend) PendingQuestions(id string) []core.PendingQuestion { return f.pending[id] }
func (f *fakeBackend) Subscribe(func(core.Change)) func()                { return func() {} }

func (f *fakeBackend) SendToOrchestrator(_ context.Context, text string) error {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) Send(_ context.Context, _, text string) error {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) Interrupt(context.Context, string) error        { return nil }
func (f *fakeBackend) Answer(string, string, map[string]string) error { return nil }
func (f *fakeBackend) Dismiss(string, string) error                   { return nil }
func (f *fakeBackend) Changes(_ context.Context, id string) ([]git.FileChange, error) {
	return f.changes[id], nil
}
func (f *fakeBackend) Diff(_ context.Context, id string) (string, error) { return f.diffs[id], nil }
func (f *fakeBackend) Merge(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	f.merged = append(f.merged, id)
	f.mu.Unlock()
	return "", errors.New("fake")
}
func (f *fakeBackend) Discard(context.Context, string) error { return nil }
func (f *fakeBackend) Settings() core.Settings               { return f.settings }
func (f *fakeBackend) UpdateSettings(fn func(*core.Settings)) error {
	fn(&f.settings)
	return nil
}
func (f *fakeBackend) RepoPaths() []string    { return f.repos }
func (f *fakeBackend) RateLimit() agent.Event { return agent.Event{} }

func (f *fakeBackend) add(thread, kind string, data any) {
	raw, _ := json.Marshal(data)
	f.nextID++
	ref := ""
	switch d := data.(type) {
	case core.ToolData:
		ref = d.ID
	case core.ProposalData:
		ref = d.ThreadID
	}
	f.items[thread] = append(f.items[thread], core.Item{ID: f.nextID, ThreadID: thread, Kind: kind, Ref: ref, Data: raw,
		CreatedAt: time.Now()})
}

func tool(id, name string, input any, result string) core.ToolData {
	raw, _ := json.Marshal(input)
	return core.ToolData{ID: id, Name: name, Input: raw, Result: result, Done: true}
}

// demoBackend is a busy afternoon: an orchestrator conversation and workers in every state.
func demoBackend() *fakeBackend {
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	repos := []string{"/home/dev/code/web", "/home/dev/code/api", "/home/dev/code/docs", "/home/dev/code/infra"}
	worker := func(id, title, repo, status string, updated time.Duration, cost float64) core.ThreadView {
		v := core.ThreadView{Thread: store.Thread{ID: id, Kind: store.KindWorker, Title: title, RepoPath: repo,
			Branch: "solo/" + id[:8], BaseBranch: "main", BaseCommit: "abc123", WorktreePath: "/tmp/wt/" + id,
			Status: status, Model: "sonnet", CostUSD: cost, CreatedAt: ago(updated + time.Hour), UpdatedAt: ago(updated)},
			IsWorker: true, HasWorktree: true, ActualModel: "claude-sonnet-5"}
		v.CanDiscard = true
		v.CanMerge = status == core.StatusIdle || status == core.StatusFailed || status == core.StatusInterrupted
		return v
	}
	oauth := worker("a1f3c9d2-oauth", "Fix OAuth token refresh", repos[0], core.StatusRunning, 40*time.Second, 0.42)
	oauth.InTurn, oauth.CanInterrupt = true, true
	oauth.TurnStarted = ago(2*time.Minute + 14*time.Second)
	step := tool("o9", "Bash", map[string]any{"command": "pnpm test src/auth"}, "")
	step.Done = false
	oauth.Step = &step
	oauth.Live = []core.LiveBlock{{Text: "The refresh path now retries once on a 401. Running the auth test suite to confirm nothing else depends on the old behavior."}}
	limiter := worker("b7e21a90-limit", "Rate limit /api/upload", repos[1], core.StatusWaiting, 2*time.Minute, 0.31)
	limiter.PendingAsk, limiter.InTurn = true, true
	forms := worker("c3d4e5f6-forms", "Move settings page to new form components", repos[0], core.StatusIdle, 6*time.Minute, 1.08)
	forms.Summary = "Moved the settings page to the new `Form` components and deleted the old `LegacyField` wrappers.\n\n- 6 files changed; all 214 web tests pass."
	flaky := worker("d9a8b7c6-flaky", "Fix flaky billing worker test", repos[1], core.StatusFailed, 11*time.Minute, 0.57)
	flaky.Error = "Tests failed: 2 of 148 in billing/worker_test.go; the retry timer races with the test clock."
	readme := worker("e1e2e3e4-readme", "Update install steps in README", repos[2], core.StatusQueued, 30*time.Second, 0)
	readme.HasWorktree, readme.WorktreePath, readme.Branch = false, "", ""
	bump := worker("f0f0f0f0-gobump", "Bump Go to 1.27", repos[3], core.StatusMerged, 3*time.Hour, 0.12)
	bump.CanMerge, bump.CanDiscard = false, false
	orch := core.ThreadView{Thread: store.Thread{ID: core.OrchestratorID, Kind: store.KindOrchestrator, Title: "Orchestrator",
		Status: core.StatusIdle, ContextTokens: 61000, CostUSD: 2.14, CreatedAt: ago(5 * time.Hour), UpdatedAt: ago(time.Minute)},
		ActualModel: "claude-opus-5-5"}

	f := &fakeBackend{
		threads:  []core.ThreadView{orch, oauth, limiter, forms, flaky, readme, bump},
		items:    map[string][]core.Item{},
		repos:    repos,
		settings: core.Settings{Folders: []string{"~/code"}, OrchestratorModel: "opus", WorkerModel: "sonnet", ScanDepth: 3},
		pending: map[string][]core.PendingQuestion{limiter.ID: {{ToolUseID: "toolu_q1", Questions: []agent.Question{{
			Question: "Which store should back the upload rate limiter?", Header: "Limiter",
			Options: []agent.QuestionOption{
				{Label: "Redis", Description: "Shared across instances; the API already uses Redis for sessions."},
				{Label: "In-memory", Description: "Simplest, but limits are per instance."},
			}}}}}},
		changes: map[string][]git.FileChange{forms.ID: {
			// These match demoDiff for the two files it shows.
			{Path: "src/settings/SettingsPage.tsx", Status: "M", Added: 9, Removed: 10},
			{Path: "src/settings/ProfileSection.tsx", Status: "M", Added: 22, Removed: 31},
			{Path: "src/settings/NotificationsSection.tsx", Status: "M", Added: 19, Removed: 27},
			{Path: "src/components/form/Toggle.tsx", Status: "A", Added: 11, Removed: 0},
			{Path: "src/components/legacy/LegacyField.tsx", Status: "D", Added: 0, Removed: 64},
			{Path: "src/settings/settings.test.tsx", Status: "M", Added: 12, Removed: 4},
		}},
		diffs: map[string]string{forms.ID: demoDiff},
	}

	o := core.OrchestratorID
	f.add(o, core.ItemUser, core.UserData{Text: "Three things today: the OAuth refresh bug in web, rate limiting on uploads in api, and finish moving the settings page to the new form components."})
	f.add(o, core.ItemThinking, core.TextData{Text: "Three independent tasks in two repos. I'll check the auth module first so the worker prompt is specific."})
	f.add(o, core.ItemTool, tool("t1", "Grep", map[string]any{"pattern": "refreshToken", "path": "/home/dev/code/web/src"}, "src/auth/session.ts:88\nsrc/auth/refresh.ts:12"))
	f.add(o, core.ItemTool, tool("t2", "Read", map[string]any{"file_path": "/home/dev/code/web/src/auth/refresh.ts"}, "…"))
	f.add(o, core.ItemTool, tool("t3", "mcp__solo__spawn_thread", map[string]any{"repo": "web", "title": "Fix OAuth token refresh"}, "Started thread a1f3c9d2"))
	f.add(o, core.ItemTool, tool("t4", "mcp__solo__spawn_thread", map[string]any{"repo": "api", "title": "Rate limit /api/upload"}, "Started thread b7e21a90"))
	f.add(o, core.ItemTool, tool("t5", "mcp__solo__spawn_thread", map[string]any{"repo": "web", "title": "Move settings page to new form components"}, "Started thread c3d4e5f6"))
	f.add(o, core.ItemAssistant, core.TextData{Text: "I started three threads:\n\n- **Fix OAuth token refresh** (web): `refresh.ts` drops the retry when the access token expires mid-request. The worker will add a single retry on 401 and a test.\n- **Rate limit /api/upload** (api): 10 uploads per minute per user, returning 429 with `Retry-After`.\n- **Move settings page to new form components** (web): finish the migration and remove `LegacyField`.\n\nI'll report as each one finishes."})
	f.add(o, core.ItemUser, core.UserData{Synthetic: true, Text: "[thread update from Solo, not from the user]\n- Thread c3d4e5f6 \"Move settings page to new form components\" (web) finished its turn. Summary: Moved the settings page to the new Form components; 214 tests pass.\n- Thread b7e21a90 \"Rate limit /api/upload\" (api) is asking a question (see get_thread)."})
	f.add(o, core.ItemTool, tool("t6", "mcp__solo__get_diff", map[string]any{"id": "c3d4e5f6"}, "diff --git …"))
	f.add(o, core.ItemProposal, core.ProposalData{ThreadID: forms.ID, Title: forms.Title,
		Summary: "Replaces the settings page's fields with the new Form components and deletes LegacyField. The diff is contained to settings and the web tests pass."})
	f.add(o, core.ItemAssistant, core.TextData{Text: "The settings migration is done and reviewed; it's ready to merge.\n\nThe rate limiter thread needs a decision: **Redis or in-memory** for the counters. Redis works across instances, and the API already runs it for sessions. Answer below and I'll pass it on."})

	w := forms.ID
	f.add(w, core.ItemNotice, core.NoticeData{Level: "info", Text: "Working on branch solo/c3d4e5f6 from main."})
	f.add(w, core.ItemUser, core.UserData{From: core.FromOrchestrator, Text: "Finish moving the settings page (src/settings) to the new Form components in src/components/form. Remove LegacyField when nothing uses it. Run the web tests."})
	f.add(w, core.ItemTool, tool("w1", "Glob", map[string]any{"pattern": "src/settings/**/*.tsx"}, "5 files"))
	f.add(w, core.ItemTool, tool("w2", "Read", map[string]any{"file_path": "/wt/src/settings/SettingsPage.tsx"}, "…"))
	f.add(w, core.ItemThinking, core.TextData{Text: "Three sections still use LegacyField. The new Form has no Toggle yet, so I'll add one that matches Checkbox."})
	f.add(w, core.ItemTool, tool("w3", "Write", map[string]any{"file_path": "/wt/src/components/form/Toggle.tsx"}, "File created"))
	f.add(w, core.ItemTool, tool("w4", "Edit", map[string]any{"file_path": "/wt/src/settings/SettingsPage.tsx"}, "Updated"))
	f.add(w, core.ItemTool, tool("w5", "Edit", map[string]any{"file_path": "/wt/src/settings/ProfileSection.tsx"}, "Updated"))
	f.add(w, core.ItemTool, tool("w6", "Bash", map[string]any{"command": "pnpm test --filter web"}, "Test Files  38 passed (38)\nTests  214 passed (214)"))
	f.add(w, core.ItemTool, tool("w7", "Bash", map[string]any{"command": "git commit -am \"refactor(settings): use Form components\""}, "[solo/c3d4e5f6 9e1b2c4] refactor(settings): use Form components"))
	f.add(w, core.ItemAssistant, core.TextData{Text: forms.Summary})
	o2 := oauth.ID
	f.add(o2, core.ItemNotice, core.NoticeData{Level: "info", Text: "Working on branch solo/a1f3c9d2 from main."})
	f.add(o2, core.ItemUser, core.UserData{From: core.FromOrchestrator, Text: "In web, the OAuth access token isn't refreshed when it expires mid-request (src/auth/refresh.ts). Retry once on a 401 after refreshing, add a test, and run the auth tests."})
	f.add(o2, core.ItemTool, tool("o1", "Read", map[string]any{"file_path": "/wt/src/auth/refresh.ts"}, "…"))
	f.add(o2, core.ItemTool, tool("o2", "Grep", map[string]any{"pattern": "refreshToken\\("}, "3 matches"))
	f.add(o2, core.ItemTool, tool("o3", "Read", map[string]any{"file_path": "/wt/src/auth/session.ts"}, "…"))
	f.add(o2, core.ItemAssistant, core.TextData{Text: "The interceptor gives up on a 401 because `refresh()` resolves after the request has already failed. I'll await the refresh and retry the request once:\n\n```ts\nif (res.status === 401 && !req.retried) {\n  await session.refresh();\n  return fetch(withAuth({ ...req, retried: true }));\n}\n```"})
	f.add(o2, core.ItemTool, tool("o4", "Edit", map[string]any{"file_path": "/wt/src/auth/refresh.ts"}, "Updated"))
	f.add(o2, core.ItemTool, tool("o5", "Write", map[string]any{"file_path": "/wt/src/auth/refresh.test.ts"}, "File created"))
	running := tool("o9", "Bash", map[string]any{"command": "pnpm test src/auth"}, "")
	running.Done = false
	f.add(o2, core.ItemTool, running)
	return f
}

const demoDiff = `diff --git a/src/settings/SettingsPage.tsx b/src/settings/SettingsPage.tsx
index 3f2a1b0..9e1b2c4 100644
--- a/src/settings/SettingsPage.tsx
+++ b/src/settings/SettingsPage.tsx
@@ -1,18 +1,14 @@
-import { LegacyField } from "../components/legacy/LegacyField";
-import { useLegacyForm } from "../hooks/useLegacyForm";
+import { Form, Field, Toggle } from "../components/form";
 import { ProfileSection } from "./ProfileSection";
 import { NotificationsSection } from "./NotificationsSection";

 export function SettingsPage({ user }: Props) {
-  const form = useLegacyForm(user.settings);
+  return (
+    <Form initial={user.settings} onSubmit={save}>
+      <ProfileSection />
+      <NotificationsSection />
+      <Toggle name="betaFeatures" label="Try beta features" />
+    </Form>
+  );
+}
-  return (
-    <div className="settings">
-      <LegacyField form={form} name="displayName" />
-      <LegacyField form={form} name="email" type="email" />
-    </div>
-  );
-}
diff --git a/src/components/form/Toggle.tsx b/src/components/form/Toggle.tsx
new file mode 100644
--- /dev/null
+++ b/src/components/form/Toggle.tsx
@@ -0,0 +1,12 @@
+import { useField } from "./useField";
+
+export function Toggle({ name, label }: { name: string; label: string }) {
+  const field = useField<boolean>(name);
+  return (
+    <label className="toggle">
+      <input type="checkbox" checked={field.value} onChange={(e) => field.set(e.target.checked)} />
+      {label}
+    </label>
+  );
+}
`
