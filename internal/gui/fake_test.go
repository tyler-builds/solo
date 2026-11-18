package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/store"
)

// fakeBackend serves canned data for screenshots and UI tests.
type fakeBackend struct {
	mu          sync.Mutex
	threads     []core.ThreadView
	rate        []agent.Event // usage limits, for RateLimits
	items       map[string][]core.Item
	pending     map[string][]core.PendingQuestion
	changes     map[string][]git.FileChange
	diffs       map[string]string
	settings    core.Settings
	repos       []string
	plain       []string
	inited      []string
	sent        []string
	sentImgs    [][]string // the images sent with each message in sent
	merged      []string
	mergedRepos []string // "id repo" for each MergeRepo, repo "" for all
	opened      []string
	drafts      []string // "id repo" for each pull request opened as a draft, repo "" for all
	openedFor   []string // "id repo" for each OpenPRFor, repo "" for all
	readied     []string // MarkReady
	nextID      int64
	todos       []core.Todo
	skills      []core.Skill   // for "/" and Settings; fakeSkills when nil
	paths       []core.PathHit // a thread's files, for "@"; fakePaths when nil
	pathQueries []string       // the queries SearchPaths was asked, in order
	imageReads  []string       // the images ReadImage was asked for, in order
	// machineNow is the time on the machine, for a backend on another one (api.Clock); nil for this
	// process's clock.
	machineNow func() time.Time
}

func (f *fakeBackend) Now() time.Time {
	if f.machineNow != nil {
		return f.machineNow()
	}
	return time.Now()
}

// fakeSkills and fakePaths are what the composer's lists show unless a test sets others.
var (
	fakeSkills = []core.Skill{
		{Name: "review", Description: "Review the current branch for bugs, risky changes and missing tests.", Source: "Personal"},
		{Name: "release-notes", Description: "Write release notes from the pull requests merged since the last tag.", Command: true, Source: "Project"},
		{Name: "pr-style", Description: "How our pull request titles and descriptions read.", Source: "Personal"},
		{Name: "frontend-design:frontend-design", Description: "Create distinctive, production-grade frontend interfaces.", Source: "Plugin frontend-design"},
		{Name: "anthropic-skills:pdf", Description: "Read, fill and make PDF files.", Source: "Organization"},
	}
	fakePaths = []core.PathHit{{Path: "internal", Dir: true}, {Path: "internal/auth", Dir: true},
		{Path: "internal/auth/token.go"}, {Path: "internal/auth/token_test.go"}, {Path: "internal/auth/refresh.go"},
		{Path: "cmd", Dir: true}, {Path: "cmd/api/main.go"}, {Path: "README.md"}, {Path: "go.mod"}}
)

func (f *fakeBackend) Skills(_ context.Context, threadID string) ([]core.Skill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.skills
	if src == nil {
		src = fakeSkills
	}
	var out []core.Skill
	for _, s := range src {
		if threadID == "" && s.Command {
			continue // Settings lists skills only
		}
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeBackend) SearchPaths(_ context.Context, _ string, query string, limit int) ([]core.PathHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pathQueries = append(f.pathQueries, query)
	src := f.paths
	if src == nil {
		src = fakePaths
	}
	var out []core.PathHit
	for _, p := range src {
		if _, ok := core.MatchScore(query, p.Path); ok && (query != "" || !strings.Contains(p.Path, "/")) && len(out) < limit {
			out = append(out, p)
		}
	}
	return out, nil
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

func (f *fakeBackend) Todos(done bool) []core.Todo {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.Todo
	for _, td := range f.todos {
		if done || !td.Done() {
			out = append(out, td)
		}
	}
	return out
}

func (f *fakeBackend) CompleteTodo(id string) error {
	done := true
	_, err := f.UpdateTodo(id, core.TodoChange{Done: &done, By: core.TodoByUser})
	return err
}

func (f *fakeBackend) UpdateTodo(id string, ch core.TodoChange) (core.Todo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.todos {
		if f.todos[i].ID == id {
			if ch.Done != nil {
				f.todos[i].DoneAt = time.Time{}
				if *ch.Done {
					f.todos[i].DoneAt, f.todos[i].DoneBy = time.Now(), ch.By
				}
			}
			return f.todos[i], nil
		}
	}
	return core.Todo{}, fmt.Errorf("no todo %q", id)
}

func (f *fakeBackend) SendToOrchestrator(_ context.Context, text string, images ...string) error {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	f.sentImgs = append(f.sentImgs, images)
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) Send(_ context.Context, _, text string, images ...string) error {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	f.sentImgs = append(f.sentImgs, images)
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
func (f *fakeBackend) MergeLocal(ctx context.Context, id string) (string, error) {
	return f.Merge(ctx, id)
}
func (f *fakeBackend) MarkReady(_ context.Context, id string) error {
	f.mu.Lock()
	f.readied = append(f.readied, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) SearchMessages(_ context.Context, query string, limit int) ([]core.SearchHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.SearchHit
	for id, items := range f.items {
		for _, it := range items {
			var d core.TextData
			it.Decode(&d)
			if (it.Kind == core.ItemUser || it.Kind == core.ItemAssistant) && strings.Contains(strings.ToLower(d.Text), strings.ToLower(query)) {
				out = append(out, core.SearchHit{ThreadID: id, ItemID: it.ID, Kind: it.Kind, Snippet: d.Text, At: it.CreatedAt})
			}
		}
	}
	return out, nil
}
func (f *fakeBackend) SetThreadModel(_ context.Context, id, model, effort string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.threads {
		if f.threads[i].ID == id {
			f.threads[i].Model, f.threads[i].Effort = model, effort
		}
	}
	return nil
}
func (f *fakeBackend) SetScratch(_ context.Context, id string, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.threads {
		if f.threads[i].ID == id {
			f.threads[i].Scratch = on
		}
	}
	return nil
}
func (f *fakeBackend) OpenPRFor(ctx context.Context, id, repo string, draft bool) ([]forge.PR, error) {
	f.mu.Lock()
	f.openedFor = append(f.openedFor, id+" "+repo)
	if draft {
		f.drafts = append(f.drafts, id+" "+repo)
	}
	f.mu.Unlock()
	pr, err := f.openPR(id)
	return []forge.PR{pr}, err
}
func (f *fakeBackend) MergeRepo(ctx context.Context, id, repo string) (string, error) {
	f.mu.Lock()
	f.mergedRepos = append(f.mergedRepos, id+" "+repo)
	f.mu.Unlock()
	return f.Merge(ctx, id)
}
func (f *fakeBackend) OpenPR(_ context.Context, id string, draft bool) (forge.PR, error) {
	if draft {
		f.mu.Lock()
		f.drafts = append(f.drafts, id+" ")
		f.mu.Unlock()
	}
	return f.openPR(id)
}

func (f *fakeBackend) openPR(id string) (forge.PR, error) {
	f.mu.Lock()
	f.opened = append(f.opened, id)
	f.mu.Unlock()
	return forge.PR{Kind: forge.GitHub, Number: 1, URL: "https://github.com/acme/web/pull/1"}, nil
}
func (f *fakeBackend) Discard(context.Context, string) error { return nil }
func (f *fakeBackend) Settings() core.Settings {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings
}
func (f *fakeBackend) UpdateSettings(fn func(*core.Settings)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.settings)
	return nil
}
func (f *fakeBackend) RepoPaths() []string    { return f.repos }
func (f *fakeBackend) PlainFolders() []string { return f.plain }
func (f *fakeBackend) InitRepo(_ context.Context, folder string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.Index(f.plain, folder)
	if i < 0 {
		return "", fmt.Errorf("%q isn't a plain folder", folder)
	}
	f.plain = slices.Delete(slices.Clone(f.plain), i, i+1)
	f.repos = append(slices.Clone(f.repos), folder)
	f.inited = append(f.inited, folder)
	return folder, nil
}
func (f *fakeBackend) RateLimits() []agent.Event { return f.rate }

func (f *fakeBackend) Done(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.threads {
		if f.threads[i].ID == id {
			f.threads[i].DoneAt, f.threads[i].CanDone = time.Now(), false
		}
	}
	return nil
}

func (f *fakeBackend) Restore(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.threads {
		if f.threads[i].ID == id {
			f.threads[i].DoneAt, f.threads[i].Status = time.Time{}, core.StatusIdle
		}
	}
	return nil
}

func (f *fakeBackend) SaveImage(img image.Image) (string, error) {
	p := filepath.Join(os.TempDir(), fmt.Sprintf("solo-paste-%d.png", time.Now().UnixNano()))
	out, err := os.Create(p)
	if err != nil {
		return "", err
	}
	defer out.Close()
	return p, png.Encode(out, img)
}

func (f *fakeBackend) ImportImage(path string) (string, error) { return path, nil }

func (f *fakeBackend) SaveImageData(_ context.Context, data []byte) (string, error) {
	p := filepath.Join(os.TempDir(), fmt.Sprintf("solo-file-%d.png", time.Now().UnixNano()))
	return p, os.WriteFile(p, data, 0o600)
}

// ReadImage reads any file, but scales a preview down as the real backend does.
func (f *fakeBackend) ReadImage(_ context.Context, path string, maxSide int) ([]byte, error) {
	f.mu.Lock()
	f.imageReads = append(f.imageReads, path)
	f.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil || maxSide <= 0 {
		return data, err
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return data, nil
	}
	b := img.Bounds()
	if b.Dx() <= maxSide && b.Dy() <= maxSide {
		return data, nil
	}
	scale := float64(maxSide) / float64(max(b.Dx(), b.Dy()))
	dst := image.NewRGBA(image.Rect(0, 0, int(float64(b.Dx())*scale), int(float64(b.Dy())*scale)))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	var buf bytes.Buffer
	err = png.Encode(&buf, dst)
	return buf.Bytes(), err
}

// sampleScreenshot writes a small picture of a settings form, standing in for a pasted screenshot.
func sampleScreenshot(dir string, accent color.RGBA) string {
	img := image.NewRGBA(image.Rect(0, 0, 640, 400))
	fill := func(r image.Rectangle, c color.RGBA) { draw.Draw(img, r, &image.Uniform{c}, image.Point{}, draw.Src) }
	fill(img.Bounds(), color.RGBA{246, 246, 248, 255})
	fill(image.Rect(0, 0, 640, 44), color.RGBA{32, 33, 38, 255})
	for i := 0; i < 4; i++ {
		y := 80 + i*70
		fill(image.Rect(40, y, 260, y+14), color.RGBA{120, 122, 130, 255})
		fill(image.Rect(40, y+24, 600, y+56), color.RGBA{255, 255, 255, 255})
	}
	fill(image.Rect(40, 360, 160, 392), accent)
	fill(image.Rect(300, 230, 600, 270), color.RGBA{230, 70, 70, 255}) // the broken field
	p := filepath.Join(dir, fmt.Sprintf("shot-%d-%d-%d.png", accent.R, accent.G, accent.B))
	out, _ := os.Create(p)
	defer out.Close()
	png.Encode(out, img)
	return p
}

func (f *fakeBackend) CheckClaude(_ context.Context, override string) core.ClaudeStatus {
	if override != "" {
		return core.ClaudeStatus{Path: override, Source: "setting", Err: override + " doesn't exist"}
	}
	return core.ClaudeStatus{Path: "/home/you/.local/bin/claude", Source: "PATH", Version: "2.1.319",
		Account: "you@example.com · Max", Models: []agent.ModelInfo{
			{Value: "default", DisplayName: "Default (recommended)"},
			{Value: "opus", DisplayName: "Opus 5.5", EffortLevels: []string{"low", "medium", "high", "xhigh", "max"}},
			{Value: "sonnet", DisplayName: "Sonnet 5.5", EffortLevels: []string{"low", "medium", "high", "max"}},
			{Value: "haiku", DisplayName: "Haiku 5.5"},
		}}
}

func (f *fakeBackend) add(thread, kind string, data any) {
	raw, _ := json.Marshal(data)
	f.nextID++
	ref := ""
	switch d := data.(type) {
	case core.ToolData:
		ref = d.ID
	case core.ProposalData:
		ref = d.ThreadID
	case core.QuestionData:
		ref = d.ToolUseID
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
			IsWorker: true, HasWorktree: true, Dir: "/tmp/wt/" + id, ActualModel: "claude-sonnet-5"}
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
	forms.Forge, forms.CanOpenPR, forms.CanMerge, forms.CanMergeLocal = forge.GitHub, true, false, true
	review := worker("b1b2b3b4-paginate", "Paginate the audit log", repos[1], core.StatusIdle, 40*time.Minute, 0.66)
	review.Forge, review.CanMerge, review.CanMergeLocal = forge.GitHub, false, false
	review.PR = &forge.PR{Kind: forge.GitHub, Number: 142, URL: "https://github.com/acme/api/pull/142", State: forge.StateOpen,
		Checks: forge.ChecksPending, Counts: forge.CheckCounts{Total: 7, Passed: 6, Pending: 1}, AutoMerge: true, Blocked: "waiting for checks"}
	ci := worker("c1c2c3c4-csv", "Add CSV export to reports", repos[0], core.StatusIdle, 55*time.Minute, 0.81)
	ci.Forge, ci.CanMerge, ci.CanMergeLocal = forge.GitHub, false, false
	ci.PR = &forge.PR{Kind: forge.GitHub, Number: 88, URL: "https://github.com/acme/web/pull/88", State: forge.StateOpen,
		Checks: forge.ChecksFailing, Failed: []forge.Check{{Name: "test (web)"}}, Counts: forge.CheckCounts{Total: 6, Passed: 4, Failed: 1, Pending: 1},
		Blocked: "checks failed"}
	// A task across the client, its API and its database: before its PRs, and with them open.
	part := func(name string, changed int, pr *forge.PR) core.PartView {
		p := core.PartView{Part: core.Part{Repo: "/home/dev/code/" + name, Branch: "feat/OW-512-audit-export", BaseBranch: "main", PR: pr},
			Name: name, Changed: changed, Forge: forge.GitHub}
		p.CanOpenPR = changed > 0 && pr == nil
		p.CanMerge = pr != nil && pr.Mergeable
		return p
	}
	export := worker("d1d2d3d4-export", "Add audit log export", "/home/dev/code/client", core.StatusIdle, 12*time.Minute, 1.4)
	export.Parts = []core.PartView{part("client", 3, nil), part("api", 5, nil), part("db", 1, nil)}
	export.Forge, export.CanOpenPR, export.CanMerge, export.CanMergeLocal = "", true, false, true
	landing := worker("e5e6e7e8-landing", "Add tenant usage report", "/home/dev/code/client", core.StatusIdle, 48*time.Minute, 2.1)
	landing.Parts = []core.PartView{
		part("client", 2, &forge.PR{Kind: forge.GitHub, Number: 88, URL: "https://github.com/acme/client/pull/88", State: forge.StateOpen, Checks: forge.ChecksFailing, Failed: []forge.Check{{Name: "test (web)"}}, Counts: forge.CheckCounts{Total: 4, Passed: 3, Failed: 1}, Blocked: "checks failed"}),
		part("api", 4, &forge.PR{Kind: forge.GitHub, Number: 142, URL: "https://github.com/acme/api/pull/142", State: forge.StateOpen, Checks: forge.ChecksPending, Counts: forge.CheckCounts{Total: 5, Passed: 3, Pending: 2}, Blocked: "waiting for checks"}),
		part("db", 1, &forge.PR{Kind: forge.GitHub, Number: 31, URL: "https://github.com/acme/db/pull/31", State: forge.StateOpen, Checks: forge.ChecksPassing, Counts: forge.CheckCounts{Total: 3, Passed: 3}, Mergeable: true}),
	}
	landing.Forge, landing.PR, landing.CanOpenPR, landing.CanMerge, landing.CanMergeLocal = "", nil, false, true, false
	audit := worker("a0b1c2d3-audit", "Find where uploads are logged", repos[1], core.StatusIdle, 25*time.Minute, 0.09)
	audit.Summary = "Uploads are logged in `internal/upload/handler.go:142` through the request logger; nothing writes file names. No changes needed."
	audit.NoChanges, audit.CanMerge, audit.CanDone, audit.Changed = true, false, true, 0
	bump := worker("f0f0f0f0-gobump", "Bump Go to 1.27", repos[3], core.StatusMerged, 3*time.Hour, 0.12)
	bump.CanMerge, bump.CanDiscard, bump.CanDone = false, false, true
	old := worker("f1f1f1f1-old", "Rename the config loader", repos[0], core.StatusMerged, 26*time.Hour, 0.2)
	old.CanMerge, old.CanDiscard, old.DoneAt = false, false, ago(20*time.Hour)
	subs := worker("a7a7a7a7-subs", "Swap players in from the sideline", "/home/dev/code/vfl", core.StatusIdle, 3*time.Minute, 0.38)
	subs.WorktreePath, subs.Branch, subs.BaseBranch, subs.BaseCommit, subs.HasWorktree = "", "", "", "", false
	subs.InPlace, subs.Dir, subs.CanMerge, subs.CanDone = true, "/home/dev/code/vfl", false, true
	subs.Summary = "Players leaving the field now jog to their bench between plays and the new personnel run on.\n\n- Changed `js/engine.js` and `js/stadium.js`."
	gone := worker("f2f2f2f2-gone", "Try a WASM build", repos[3], core.StatusDiscarded, 50*time.Hour, 0.4)
	gone.CanMerge, gone.CanDiscard, gone.DoneAt = false, false, ago(48*time.Hour)
	orch := core.ThreadView{Thread: store.Thread{ID: core.OrchestratorID, Kind: store.KindOrchestrator, Title: "Orchestrator",
		Status: core.StatusIdle, ContextTokens: 61000, CostUSD: 2.14, CreatedAt: ago(5 * time.Hour), UpdatedAt: ago(time.Minute)},
		ActualModel: "claude-opus-5-5"}

	todos := []core.Todo{
		{ID: "t1", Text: "Fix OAuth token refresh", Threads: []string{oauth.ID}},
		{ID: "t2", Text: "Rate limit uploads in api", Threads: []string{limiter.ID}},
		{ID: "t3", Text: "Finish moving the settings page to the new form components", Threads: []string{forms.ID}},
		{ID: "t4", Text: "Look into the slow dashboard query"},
		{ID: "t5", Text: "Rename the config loader", Threads: []string{old.ID}, DoneAt: ago(20 * time.Hour), DoneBy: core.TodoByThread},
	}
	f := &fakeBackend{
		todos:   todos,
		threads: []core.ThreadView{orch, oauth, limiter, forms, review, ci, export, landing, audit, subs, flaky, readme, bump, old, gone},
		items:   map[string][]core.Item{},
		repos:   repos,
		plain:   []string{"/home/dev/code/vfl"},
		settings: core.Settings{Folders: []string{"/home/dev/code"}, OrchestratorModel: "opus", WorkerModel: "sonnet", ScanDepth: 3,
			NamingModel: "haiku", OrchestratorEffort: "high", MergeMethod: "squash", OnChecksFail: core.ChecksFix,
			PRModel: "sonnet", PREffort: "low"},
		pending: map[string][]core.PendingQuestion{limiter.ID: {{ToolUseID: "toolu_q1", Questions: []agent.Question{{
			Question: "Which store should back the upload rate limiter?", Header: "Limiter",
			Options: []agent.QuestionOption{
				{Label: "Redis", Description: "Shared across instances; the API already uses Redis for sessions."},
				{Label: "In-memory", Description: "Simplest, but limits are per instance."},
			}}}}}},
		changes: map[string][]git.FileChange{export.ID: {
			{Path: "client/src/audit/ExportButton.tsx", Status: "A", Added: 3, Removed: 0},
			{Path: "api/internal/audit/export.go", Status: "A", Added: 4, Removed: 0},
			{Path: "api/internal/audit/routes.go", Status: "M", Added: 1, Removed: 1},
			{Path: "db/migrations/0042_audit_export.sql", Status: "A", Added: 2, Removed: 0},
		}, forms.ID: {
			// These match demoDiff for the two files it shows.
			{Path: "src/settings/SettingsPage.tsx", Status: "M", Added: 9, Removed: 10},
			{Path: "src/settings/ProfileSection.tsx", Status: "M", Added: 22, Removed: 31},
			{Path: "src/settings/NotificationsSection.tsx", Status: "M", Added: 19, Removed: 27},
			{Path: "src/components/form/Toggle.tsx", Status: "A", Added: 11, Removed: 0},
			{Path: "src/components/legacy/LegacyField.tsx", Status: "D", Added: 0, Removed: 64},
			{Path: "src/settings/settings.test.tsx", Status: "M", Added: 12, Removed: 4},
		}},
		diffs: map[string]string{forms.ID: demoDiff, export.ID: exportDiff},
	}

	o := core.OrchestratorID
	f.add("a7a7a7a7-subs", core.ItemNotice, core.NoticeData{Level: "info", Text: "Working directly in ~/code/vfl. It isn't in git, so there's no separate copy, diff or merge."})
	f.add("a7a7a7a7-subs", core.ItemUser, core.UserData{Text: "Between plays, send the players who are leaving to their sideline and bring the new personnel on."})
	f.add("a7a7a7a7-subs", core.ItemAssistant, core.TextData{Text: subs.Summary})
	f.add(o, core.ItemUser, core.UserData{Text: "Three things today:\n**OAuth:** the refresh bug in `web`\n**Uploads:** rate limiting in `api`\n**Settings:** finish moving the page to the new form components"})
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

const exportDiff = `diff --git a/client/src/audit/ExportButton.tsx b/client/src/audit/ExportButton.tsx
new file mode 100644
--- /dev/null
+++ b/client/src/audit/ExportButton.tsx
@@ -0,0 +1,3 @@
+export function ExportButton({ onExport }: { onExport: () => void }) {
+  return <Button onClick={onExport}>Export CSV</Button>;
+}
diff --git a/api/internal/audit/export.go b/api/internal/audit/export.go
new file mode 100644
--- /dev/null
+++ b/api/internal/audit/export.go
@@ -0,0 +1,4 @@
+package audit
+
+// Export writes the tenant's audit log as CSV.
+func Export(w io.Writer, tenant string) error { return writeCSV(w, tenant) }
diff --git a/api/internal/audit/routes.go b/api/internal/audit/routes.go
--- a/api/internal/audit/routes.go
+++ b/api/internal/audit/routes.go
@@ -10,3 +10,3 @@ func Routes(r chi.Router) {
 	r.Get("/audit", list)
-	r.Get("/audit/{id}", show)
+	r.Get("/audit/export", export)
 }
diff --git a/db/migrations/0042_audit_export.sql b/db/migrations/0042_audit_export.sql
new file mode 100644
--- /dev/null
+++ b/db/migrations/0042_audit_export.sql
@@ -0,0 +1,2 @@
+CREATE INDEX audit_tenant_created ON audit_log (tenant_id, created_at);
+-- for the export's ordered scan
`
