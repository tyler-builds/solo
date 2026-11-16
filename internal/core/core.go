//go:build !js

// Package core is Solo's engine, independent of the UI: settings, repositories, and threads (the
// orchestrator and its workers) with their Claude sessions, transcripts, questions, worktrees and merges.
//
// The UI and the orchestrator's tools both drive it through Core's methods, and observe it through
// Subscribe. Methods are safe to call from any goroutine.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/tyler-builds/solo/internal/forge"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/store"
)

// Config configures a Core.
type Config struct {
	DataDir string
	Agent   agent.Command
	Log     *slog.Logger
	// PRPoll is how often open pull requests are checked; 0 means every 30 seconds.
	PRPoll time.Duration
}

// ToolHandler serves the app tools a session calls (the orchestrator's). It returns the text the model
// reads, and whether it is an error.
type ToolHandler func(ctx context.Context, threadID, name string, args []byte) (string, bool)

// Core is the engine.
type Core struct {
	cfg   Config
	log   *slog.Logger
	db    *store.Store
	agent *agent.Manager

	mu        sync.Mutex
	settings  Settings
	threads   map[string]*thread
	bySession map[string]*thread
	jobs      map[string]chan agent.Event // one-off sessions (the PR text agent): their turn ends, by session
	repos     []string
	plain     []string // folders searched that aren't repositories (git.ScanAll)
	subs      map[int]func(Change)
	nextSub   int
	tools     ToolHandler
	orchSpec  func() agent.OpenParams
	recap     func(ThreadView) string
	soloBusy  func() bool // whether thread updates are on their way to Solo (SetSoloBusy)
	// soloContext takes the hidden context for Solo's next message from the user (SetSoloContext).
	soloContext func() (string, func())
	rateLimits  map[string]agent.Event // the latest event of each kind of limit (usage.go)
	closed      bool
	forges      map[string]forge.Kind // by repository
	landing     map[string]bool       // threads with a landing action under way (see land)
	stop        chan struct{}         // closed by Close
	pollNow     chan struct{}         // asks the pull request watcher to look now
	fetched     map[string]time.Time  // when each "repo\x00branch" was last fetched to look for landed work
	lastSweep   time.Time             // when the PR watcher last looked for work landed outside Solo
	// resume are the threads that were working when Solo last closed, for ResumeInterrupted.
	resume []resumeInfo
	todos  []Todo // the user's to-do list, oldest first (todos.go)
}

// resumeInfo is a thread that was working when Solo closed, and how far it had got.
type resumeInfo struct {
	id      string
	stopped []string // what the restart stopped: "command: …", "subagent: …"
}

// DefaultDataDir is where Solo keeps its data: ~/.local/share/solo on Linux, %LOCALAPPDATA%\Solo on
// Windows, ~/Library/Application Support/Solo on macOS.
func DefaultDataDir() string {
	if d := os.Getenv("SOLO_DATA_DIR"); d != "" {
		return d
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "Solo")
		}
	case "darwin":
		if d, err := os.UserConfigDir(); err == nil {
			return filepath.Join(d, "Solo")
		}
	default:
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			return filepath.Join(d, "solo")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "solo")
}

// New opens the data directory and loads the saved threads. Sessions start lazily, when first used.
func New(cfg Config) (*Core, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "solo.db"))
	if err != nil {
		return nil, err
	}
	c := &Core{
		cfg:       cfg,
		log:       cfg.Log,
		db:        db,
		settings:  DefaultSettings(),
		threads:   map[string]*thread{},
		bySession: map[string]*thread{},
		jobs:      map[string]chan agent.Event{},
		subs:      map[int]func(Change){},
		forges:    map[string]forge.Kind{},
		landing:   map[string]bool{},
		stop:      make(chan struct{}),
		pollNow:   make(chan struct{}, 1),
		fetched:   map[string]time.Time{},
	}
	if _, err := db.GetSetting("settings", &c.settings); err != nil {
		cfg.Log.Warn("core: settings unreadable; using defaults", "err", err)
	}
	c.settings.normalize()
	c.loadTodos()
	rows, err := db.Threads()
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, row := range rows {
		t := &thread{row: *row, parts: loadParts(*row)}
		// A thread that was working when the app stopped lost its process. It shows as stopped until
		// ResumeInterrupted carries it on, and the steps it had running end.
		switch t.row.Status {
		case StatusStarting, StatusRunning, StatusBackground:
			// Work it ran in the background stopped with Claude too: the resume note names it.
			stopped := c.settleItems(t.row.ID, false)
			c.resume = append(c.resume, resumeInfo{id: t.row.ID, stopped: stopped})
			t.row.Status = StatusInterrupted
			t.row.Error = "Solo was closed while this thread was working."
			_ = db.SaveThread(&t.row)
		case StatusWaiting:
			// It stays waiting: its question can still be answered, which carries it on (resolveAsk).
			c.settleItems(t.row.ID, true)
			c.keepQuestion(t)
		case StatusInterrupted:
			c.settleItems(t.row.ID, false) // stopped before this was done on load
		}
		if t.row.ID == OrchestratorID {
			c.loadQueueLocked(t) // messages to Solo still waiting when it closed; ResumeInterrupted sends them
		}
		c.threads[t.row.ID] = t
		c.bySession[t.row.SessionID] = t
	}
	c.agent = agent.NewManager(cfg.Agent, agent.Callbacks{
		OnEvent:    c.onEvent,
		OnToolCall: c.onToolCall,
		OnAsk:      c.onAsk,
	}, cfg.Log)
	c.repos, c.plain = git.ScanAll(c.settings.Folders, c.settings.ScanDepth, c.worktreesDir())
	for id, t := range c.threads {
		if t.row.Kind == store.KindWorker && t.row.WorktreePath != "" && !Finished(t.row.Status) {
			go c.countChanges(id)
		}
	}
	// A todo is done once its threads' work lands.
	c.Subscribe(func(ch Change) {
		if ch.Kind == ChangeThread {
			c.checkTodos()
		}
	})
	c.checkTodos()
	go c.watchPRs()
	return c, nil
}

// Close stops every session and closes the database.
func (c *Core) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.stop)
	c.mu.Unlock()
	c.agent.Shutdown()
	c.db.Close()
}

// Agent returns the session manager, for probing Claude.
func (c *Core) Agent() *agent.Manager { return c.agent }

// Subscribe calls f on every change, on the goroutine that made it; f must not block. It returns a
// function that unsubscribes.
func (c *Core) Subscribe(f func(Change)) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextSub
	c.nextSub++
	c.subs[id] = f
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

// notify calls subscribers. Call without c.mu held.
func (c *Core) notify(changes ...Change) {
	c.mu.Lock()
	subs := make([]func(Change), 0, len(c.subs))
	for _, f := range c.subs {
		subs = append(subs, f)
	}
	c.mu.Unlock()
	for _, ch := range changes {
		if ch == (Change{}) {
			continue // a change that wasn't made
		}
		for _, f := range subs {
			f(ch)
		}
	}
}

// SetToolHandler sets the handler for app tools (see ToolHandler).
func (c *Core) SetToolHandler(h ToolHandler) {
	c.mu.Lock()
	c.tools = h
	c.mu.Unlock()
}

// Settings returns the current settings.
func (c *Core) Settings() Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.settings
	s.Folders = slices.Clone(s.Folders)
	return s
}

// UpdateSettings changes the settings and saves them. Changing the folders rescans for repositories.
func (c *Core) UpdateSettings(f func(*Settings)) error {
	c.mu.Lock()
	s := c.settings
	s.Folders = slices.Clone(s.Folders)
	s.RepoBranchRules = maps.Clone(s.RepoBranchRules)
	f(&s)
	s.normalize()
	if s.ScanDepth < 1 {
		s.ScanDepth = 1
	}
	if err := c.db.SetSetting("settings", s); err != nil {
		c.mu.Unlock()
		return err
	}
	rescan := !slices.Equal(s.Folders, c.settings.Folders) || s.ScanDepth != c.settings.ScanDepth
	c.settings = s
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeSettings})
	if rescan {
		c.RescanRepos()
	}
	c.schedule()
	return nil
}

// RescanRepos searches the configured folders for repositories again.
func (c *Core) RescanRepos() []string {
	s := c.Settings()
	repos, plain := git.ScanAll(s.Folders, s.ScanDepth, c.worktreesDir())
	c.mu.Lock()
	c.repos, c.plain = repos, plain
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeRepos})
	return repos
}

// RepoPaths returns the known repositories.
func (c *Core) RepoPaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.repos)
}

// PlainFolders returns the folders Solo searched that aren't git repositories, such as a project never
// put in git. Threads can't work in one until InitRepo makes it a repository.
func (c *Core) PlainFolders() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.plain)
}

// ResolvePlain finds a plain folder by path or name, as ResolveRepo finds a repository.
func (c *Core) ResolvePlain(nameOrPath string) (string, bool) {
	clean := filepath.Clean(git.ExpandHome(nameOrPath))
	var match string
	n := 0
	for _, p := range c.PlainFolders() {
		if p == clean {
			return p, true
		}
		if filepath.Base(p) == nameOrPath {
			match, n = p, n+1
		}
	}
	return match, n == 1
}

// InitRepo puts a plain folder in git (git.Init) so threads can work in it, then searches again so it
// shows up as a repository.
func (c *Core) InitRepo(ctx context.Context, nameOrPath string) (string, error) {
	dir, ok := c.ResolvePlain(nameOrPath)
	if !ok {
		return "", fmt.Errorf("%q isn't one of the folders Solo found outside git", nameOrPath)
	}
	if err := git.Init(ctx, dir); err != nil {
		return "", err
	}
	c.log.Info("core: put a folder in git", "dir", dir)
	c.RescanRepos()
	return dir, nil
}

// Repos reads the state of every known repository.
func (c *Core) Repos(ctx context.Context) []git.RepoInfo {
	return git.InfoAll(ctx, c.RepoPaths())
}

// ResolveRepo finds a known repository by path or by name. A name must be unambiguous.
func (c *Core) ResolveRepo(nameOrPath string) (string, error) {
	repos := c.RepoPaths()
	clean := filepath.Clean(nameOrPath)
	for _, r := range repos {
		if r == clean {
			return r, nil
		}
	}
	var matches []string
	for _, r := range repos {
		if filepath.Base(r) == nameOrPath {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if filepath.IsAbs(clean) && git.IsRepo(clean) {
			return clean, nil
		}
		if p, ok := c.ResolvePlain(nameOrPath); ok {
			return "", fmt.Errorf("%s isn't a git repository yet; ask the user whether to put it in git (init_repo)", p)
		}
		return "", fmt.Errorf("no repository named %q; known repositories are listed by list_repos", nameOrPath)
	default:
		return "", fmt.Errorf("%q matches several repositories (%v); use the full path", nameOrPath, matches)
	}
}

func (c *Core) worktreesDir() string { return filepath.Join(c.cfg.DataDir, "worktrees") }

// worktreePath is where a worker's worktree goes: under the data directory, grouped by repository.
func (c *Core) worktreePath(repo, slug string) string {
	h := sha256.Sum256([]byte(repo))
	return filepath.Join(c.worktreesDir(), filepath.Base(repo)+"-"+hex.EncodeToString(h[:4]), slug)
}

// errClosed is returned after Close.
var errClosed = errors.New("core: closed")

func now() time.Time { return time.Now() }
