// Package sample is a stand-in for Solo with generated threads and transcripts. The web build's sample
// page runs the UI over it, and it is what the UI is measured against: its sizes follow a busy real
// database (dozens of threads, most of them done; transcripts of a few hundred items and a few hundred
// kilobytes), and one thread streams text as a working agent does.
//
// It builds everywhere, a web page included, and touches nothing on the machine.
package sample

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/store"
)

// Options sizes the sample.
type Options struct {
	Threads int // worker threads, most of them done; 0 means 60
	Items   int // transcript items of Solo's conversation and of each open thread; 0 means 400
	// Stream makes one thread stream text, a chunk every StreamEvery (33 ms when zero), and finish a
	// message every few seconds.
	Stream      bool
	StreamEvery time.Duration
}

// Backend serves the sample. It implements gui.Backend.
type Backend struct {
	mu       sync.Mutex
	threads  []core.ThreadView
	items    map[string][]core.Item
	todos    []core.Todo
	settings core.Settings
	subs     map[int]func(core.Change)
	nextSub  int
	nextItem int64
	repos    []string
}

// StreamingID is the thread that streams when Options.Stream is set.
const StreamingID = "a1f3c9d2-stream"

var repos = []string{"/home/dev/code/web", "/home/dev/code/api", "/home/dev/code/docs", "/home/dev/code/infra"}

// New makes a sample of the given size.
func New(o Options) *Backend {
	if o.Threads <= 0 {
		o.Threads = 60
	}
	if o.Items <= 0 {
		o.Items = 400
	}
	b := &Backend{items: map[string][]core.Item{}, subs: map[int]func(core.Change){}, repos: repos,
		settings: core.DefaultSettings()}
	b.settings.Folders = []string{"/home/dev/code"}
	now := time.Now()
	b.threads = append(b.threads, core.ThreadView{Thread: store.Thread{ID: core.OrchestratorID,
		Kind: store.KindOrchestrator, Title: "Solo", Status: core.StatusIdle, Model: "opus",
		CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now}})
	b.fill(core.OrchestratorID, o.Items)

	// A few open threads in each state the rail shows, and the rest done, as a database has after a week.
	open := []struct{ id, title, status string }{
		{StreamingID, "Fix OAuth token refresh", core.StatusRunning},
		{"b7e21a90-limit", "Rate limit /api/upload", core.StatusIdle},
		{"c3d4e5f6-forms", "Move settings page to new form components", core.StatusIdle},
		{"d9a8b7c6-flaky", "Fix flaky billing worker test", core.StatusFailed},
		{"e1e2e3e4-readme", "Update install steps in README", core.StatusIdle},
		{"f1f2f3f4-export", "Add audit log export", core.StatusIdle},
	}
	for i, t := range open {
		v := worker(t.id, t.title, repos[i%len(repos)], t.status, now.Add(-time.Duration(i+1)*7*time.Minute))
		switch t.status {
		case core.StatusRunning:
			v.InTurn, v.CanInterrupt, v.TurnStarted = true, true, now.Add(-2*time.Minute)
			v.Live = []core.LiveBlock{{Text: "Reading the refresh path."}}
		case core.StatusFailed:
			v.Error = "Tests failed: 2 of 148 in billing/worker_test.go; the retry timer races with the test clock."
		default:
			v.Summary = "Finished the change and ran the tests.\n\n- 6 files changed; all 214 tests pass."
			v.Forge, v.CanOpenPR, v.CanMergeLocal, v.CanDone = forge.GitHub, true, true, true
			v.Changed = 6
		}
		b.threads = append(b.threads, v)
		b.fill(t.id, o.Items)
	}
	for i := len(open); i < o.Threads; i++ {
		id := fmt.Sprintf("%08x-done", 0x9e3779b1*uint32(i+1))
		v := worker(id, doneTitles[i%len(doneTitles)]+fmt.Sprintf(" (%d)", i), repos[i%len(repos)], core.StatusMerged,
			now.Add(-time.Duration(i)*3*time.Hour))
		v.DoneAt = v.UpdatedAt
		v.HasWorktree, v.WorktreePath, v.Dir, v.CanMerge, v.CanDiscard = false, "", "", false, false
		v.Summary = "Merged. " + lorem(220, i)
		v.Prompt = lorem(1800, i+7) // prompts average about two kilobytes
		b.threads = append(b.threads, v)
	}
	b.todos = []core.Todo{
		{ID: "t1", Text: "Fix OAuth token refresh", Threads: []string{StreamingID}, CreatedAt: now.Add(-time.Hour)},
		{ID: "t2", Text: "Rate limit uploads in api", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "t3", Text: "Look into the slow dashboard query", CreatedAt: now.Add(-3 * time.Hour)},
	}
	if o.Stream {
		every := o.StreamEvery
		if every <= 0 {
			every = 33 * time.Millisecond
		}
		go b.stream(every)
	}
	return b
}

var doneTitles = []string{"Paginate the audit log", "Add CSV export to reports", "Bump Go to 1.27", "Swap the sidebar icons",
	"Find where uploads are logged", "Tidy the billing worker", "Add tenant usage report", "Cache the dashboard query"}

func worker(id, title, repo, status string, updated time.Time) core.ThreadView {
	v := core.ThreadView{Thread: store.Thread{ID: id, Kind: store.KindWorker, Title: title, RepoPath: repo,
		Branch: "solo/" + id[:8], BaseBranch: "main", BaseCommit: "abc123", WorktreePath: "/tmp/wt/" + id,
		Status: status, Model: "sonnet", CostUSD: 0.42, CreatedAt: updated.Add(-time.Hour), UpdatedAt: updated,
		Prompt: "In " + repo + ": " + title + ". " + lorem(600, len(id))},
		IsWorker: true, HasWorktree: true, Dir: "/tmp/wt/" + id, ActualModel: "claude-sonnet-5", Changed: -1}
	v.CanDiscard = true
	v.CanMerge = status == core.StatusIdle || status == core.StatusFailed
	return v
}

// fill gives a thread a transcript of n items: turns of a message, a few tool calls with their output,
// and a reply, sized like real ones (about half a kilobyte an item).
func (b *Backend) fill(thread string, n int) {
	at := time.Now().Add(-time.Duration(n) * time.Minute)
	for i := 0; len(b.items[thread]) < n; i++ {
		b.add(thread, core.ItemUser, "", core.UserData{Text: "Next: " + lorem(140+i%160, i)}, at)
		for j := 0; j < 3+i%3 && len(b.items[thread]) < n; j++ {
			id := fmt.Sprintf("%s-%d-%d", thread[:4], i, j)
			in, _ := json.Marshal(map[string]any{"command": "pnpm test src/auth --filter " + lorem(40, i+j)})
			name := []string{"Bash", "Read", "Edit", "Grep"}[(i+j)%4]
			b.add(thread, core.ItemTool, id, core.ToolData{ID: id, Name: name, Input: in, Result: lorem(300+(i*j)%900, i*7+j), Done: true}, at)
		}
		reply := lorem(300+i%500, i+3)
		if i%4 == 0 {
			reply += "\n\n```ts\nif (res.status === 401 && !req.retried) {\n  await session.refresh();\n  return fetch(withAuth({ ...req, retried: true }));\n}\n```\n\n" + lorem(160, i)
		}
		b.add(thread, core.ItemAssistant, "", core.TextData{Text: reply}, at)
		b.add(thread, core.ItemTurn, "", core.TurnData{Status: "completed", DurationMs: 42000}, at)
		at = at.Add(time.Minute)
	}
}

func (b *Backend) add(thread, kind, ref string, data any, at time.Time) {
	raw, _ := json.Marshal(data)
	b.nextItem++
	b.items[thread] = append(b.items[thread], core.Item{ID: b.nextItem, ThreadID: thread, Kind: kind, Ref: ref, Data: raw, CreatedAt: at})
}

var words = strings.Fields("the refresh path retries once when the access token expires mid request and the " +
	"worker then runs the auth tests to confirm nothing else depends on the old behavior of the interceptor " +
	"which gave up on a 401 because the session resolved after the request had already failed in production")

// lorem is n bytes or so of plain prose, different for each seed.
func lorem(n, seed int) string {
	var s strings.Builder
	for i := seed; s.Len() < n; i++ {
		if s.Len() > 0 {
			s.WriteByte(' ')
		}
		s.WriteString(words[(i*7+seed)%len(words)])
	}
	return s.String()
}

// stream makes the streaming thread write as an agent does: a few words every tick, and every few
// seconds the text becomes a finished message and a new one starts.
func (b *Backend) stream(every time.Duration) {
	for tick := 0; ; tick++ {
		time.Sleep(every)
		b.mu.Lock()
		var changes []core.Change
		for i := range b.threads {
			v := &b.threads[i]
			if v.ID != StreamingID {
				continue
			}
			live := v.Live[0]
			live.Text += " " + words[tick%len(words)] + " " + words[(tick*3)%len(words)]
			v.Live = []core.LiveBlock{live}
			v.UpdatedAt = time.Now()
			changes = append(changes, core.Change{Kind: core.ChangeStream, ThreadID: v.ID})
			if len(live.Text) > 1500 {
				b.add(v.ID, core.ItemAssistant, "", core.TextData{Text: live.Text}, time.Now())
				v.Live = []core.LiveBlock{{Text: "Next step."}}
				changes = append(changes, core.Change{Kind: core.ChangeItems, ThreadID: v.ID})
			}
		}
		subs := make([]func(core.Change), 0, len(b.subs))
		for _, f := range b.subs {
			subs = append(subs, f)
		}
		b.mu.Unlock()
		for _, ch := range changes {
			for _, f := range subs {
				f(ch)
			}
		}
	}
}

func (b *Backend) notify(ch core.Change) {
	b.mu.Lock()
	subs := make([]func(core.Change), 0, len(b.subs))
	for _, f := range b.subs {
		subs = append(subs, f)
	}
	b.mu.Unlock()
	for _, f := range subs {
		f(ch)
	}
}

// ---- gui.Backend ----

func (b *Backend) Threads() []core.ThreadView {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]core.ThreadView(nil), b.threads...)
}

func (b *Backend) Thread(id string) (core.ThreadView, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.threads {
		if t.ID == id {
			return t, true
		}
	}
	return core.ThreadView{}, false
}

func (b *Backend) Items(_ context.Context, id string, limit int) ([]core.Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := b.items[id]
	if limit > 0 && len(items) > limit {
		items = items[len(items)-limit:]
	}
	return append([]core.Item(nil), items...), nil
}

func (b *Backend) PendingQuestions(string) []core.PendingQuestion { return nil }

func (b *Backend) Subscribe(f func(core.Change)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSub
	b.nextSub++
	b.subs[id] = f
	return func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

func (b *Backend) Todos(done bool) []core.Todo {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []core.Todo
	for _, t := range b.todos {
		if t.Done() == done {
			out = append(out, t)
		}
	}
	return out
}

func (b *Backend) CompleteTodo(id string) error {
	b.mu.Lock()
	for i := range b.todos {
		if b.todos[i].ID == id {
			b.todos[i].DoneAt, b.todos[i].DoneBy = time.Now(), core.TodoByUser
		}
	}
	b.mu.Unlock()
	b.notify(core.Change{Kind: core.ChangeTodos})
	return nil
}

func (b *Backend) UpdateTodo(id string, ch core.TodoChange) (core.Todo, error) {
	return core.Todo{}, errSample
}

var errSample = errors.New("this is the sample page: nothing here is connected to a machine")

func (b *Backend) send(thread, text string, attachments []string) error {
	// A sample has no images to show, but it shows the files a machine stored (cmd/solo-localrelay).
	var files []core.FileRef
	for _, p := range attachments {
		if name, ok := core.FileName(p); ok {
			size := int64(-1)
			if fi, err := os.Stat(p); err == nil {
				size = fi.Size()
			}
			files = append(files, core.FileRef{Path: p, Name: name, Size: size})
		}
	}
	b.mu.Lock()
	b.add(thread, core.ItemUser, "", core.UserData{Text: text, Files: files}, time.Now())
	b.add(thread, core.ItemAssistant, "", core.TextData{Text: "This is the sample page, so nothing ran. You wrote: " + text}, time.Now())
	b.mu.Unlock()
	b.notify(core.Change{Kind: core.ChangeItems, ThreadID: thread})
	return nil
}

func (b *Backend) SendToOrchestrator(_ context.Context, text string, attachments ...string) error {
	return b.send(core.OrchestratorID, text, attachments)
}
func (b *Backend) Send(_ context.Context, id, text string, attachments ...string) error {
	return b.send(id, text, attachments)
}
func (b *Backend) SaveImage(image.Image) (string, error) { return "", errSample }
func (b *Backend) ImportImage(string) (string, error)    { return "", errSample }
func (b *Backend) SaveImageData(context.Context, []byte) (string, error) {
	return "", errSample
}
func (b *Backend) SaveFile(context.Context, string, []byte, func(int64)) (string, error) {
	return "", errSample
}
func (b *Backend) ImportFile(string) (string, error)                      { return "", errSample }
func (b *Backend) ReadImage(context.Context, string, int) ([]byte, error) { return nil, errSample }
func (b *Backend) Interrupt(context.Context, string) error                { return errSample }
func (b *Backend) Answer(string, string, map[string]string) error         { return errSample }
func (b *Backend) Dismiss(string, string) error                           { return errSample }

func (b *Backend) Changes(context.Context, string) ([]git.FileChange, error) {
	return []git.FileChange{{Path: "src/auth/refresh.ts", Status: "M", Added: 9, Removed: 10},
		{Path: "src/auth/refresh.test.ts", Status: "A", Added: 41}}, nil
}

func (b *Backend) Diff(context.Context, string) (string, error) {
	return "diff --git a/src/auth/refresh.ts b/src/auth/refresh.ts\n--- a/src/auth/refresh.ts\n+++ b/src/auth/refresh.ts\n" +
		"@@ -1,4 +1,5 @@\n import { session } from \"./session\";\n-export const retry = false;\n+export const retry = true;\n+export const attempts = 1;\n", nil
}

func (b *Backend) Merge(context.Context, string) (string, error)      { return "", errSample }
func (b *Backend) MergeLocal(context.Context, string) (string, error) { return "", errSample }
func (b *Backend) OpenPR(context.Context, string, bool) (forge.PR, error) {
	return forge.PR{}, errSample
}
func (b *Backend) MarkReady(context.Context, string) error { return errSample }
func (b *Backend) OpenPRFor(context.Context, string, string, bool) ([]forge.PR, error) {
	return nil, errSample
}
func (b *Backend) MergeRepo(context.Context, string, string) (string, error)    { return "", errSample }
func (b *Backend) SetThreadModel(context.Context, string, string, string) error { return errSample }
func (b *Backend) SetScratch(context.Context, string, bool) error               { return errSample }
func (b *Backend) CreateWorker(context.Context, core.WorkerSpec) (core.ThreadView, error) {
	return core.ThreadView{}, errSample
}
func (b *Backend) SearchMessages(context.Context, string, int) ([]core.SearchHit, error) {
	return nil, nil
}

// sampleSkills are the skills and commands the sample offers after "/".
var sampleSkills = []core.Skill{
	{Name: "review", Description: "Review the current branch for bugs and missing tests.", Source: "Personal"},
	{Name: "release-notes", Description: "Write release notes from the merged pull requests.", Command: true, Source: "Project"},
	{Name: "frontend:design", Description: "Build distinctive, production-grade interfaces.", Source: "Plugin frontend"},
}

func (b *Backend) Skills(context.Context, string) ([]core.Skill, error) {
	return slices.Clone(sampleSkills), nil
}

// samplePaths are the files a sample thread's folder holds, for "@".
var samplePaths = []core.PathHit{{Path: "cmd", Dir: true}, {Path: "cmd/api/main.go"}, {Path: "internal", Dir: true},
	{Path: "internal/auth", Dir: true}, {Path: "internal/auth/token.go"}, {Path: "internal/auth/token_test.go"},
	{Path: "README.md"}, {Path: "go.mod"}}

func (b *Backend) SearchPaths(_ context.Context, _ string, query string, limit int) ([]core.PathHit, error) {
	var out []core.PathHit
	for _, p := range samplePaths {
		if _, ok := core.MatchScore(query, p.Path); ok && len(out) < limit && (query != "" || !strings.Contains(p.Path, "/")) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (b *Backend) Discard(context.Context, string) error { return errSample }
func (b *Backend) Done(context.Context, string) error    { return errSample }
func (b *Backend) Restore(context.Context, string) error { return errSample }

func (b *Backend) Settings() core.Settings {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.settings
}

func (b *Backend) UpdateSettings(f func(*core.Settings)) error {
	b.mu.Lock()
	f(&b.settings)
	b.mu.Unlock()
	b.notify(core.Change{Kind: core.ChangeSettings})
	return nil
}

func (b *Backend) RepoPaths() []string                              { return b.repos }
func (b *Backend) PlainFolders() []string                           { return nil }
func (b *Backend) InitRepo(context.Context, string) (string, error) { return "", errSample }
func (b *Backend) RateLimits() []agent.Event                        { return nil }
func (b *Backend) CheckClaude(context.Context, string) core.ClaudeStatus {
	return core.ClaudeStatus{Path: "/home/dev/.local/bin/claude", Source: "PATH", Version: "2.1.319",
		Models: []agent.ModelInfo{{Value: "opus", DisplayName: "Opus"}, {Value: "sonnet", DisplayName: "Sonnet"}}}
}
