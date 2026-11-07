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
	"log/slog"
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

// Settings are the user's preferences.
type Settings struct {
	// Folders are searched for git repositories, which the orchestrator then knows about.
	Folders           []string `json:"folders"`
	ScanDepth         int      `json:"scanDepth"`
	OrchestratorModel string   `json:"orchestratorModel"`
	WorkerModel       string   `json:"workerModel"`
	// MaxConcurrent caps how many workers run at once; more wait as queued.
	MaxConcurrent int    `json:"maxConcurrent"`
	ClaudePath    string `json:"claudePath,omitempty"`
}

// DefaultSettings are used until the user changes them.
func DefaultSettings() Settings {
	return Settings{ScanDepth: 3, OrchestratorModel: "opus", WorkerModel: "sonnet", MaxConcurrent: 4}
}

// Config configures a Core.
type Config struct {
	DataDir string
	Agent   agent.Command
	Log     *slog.Logger
}

// ChangeKind says what changed.
type ChangeKind int

const (
	ChangeThread   ChangeKind = iota // a thread was added, or its status or details changed
	ChangeItems                      // a thread's transcript gained or updated an item
	ChangeStream                     // a thread's streaming text changed (frequent)
	ChangeSettings                   // settings changed
	ChangeRepos                      // the repository list changed
)

// Change is one notification to subscribers.
type Change struct {
	Kind     ChangeKind
	ThreadID string
	// For ChangeThread: the status before and after, equal when something else changed.
	PrevStatus, Status string
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
	repos     []string
	subs      map[int]func(Change)
	nextSub   int
	tools     ToolHandler
	orchSpec  func() agent.OpenParams
	recap     func(ThreadView) string
	rateLimit agent.Event
	closed    bool
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
		subs:      map[int]func(Change){},
	}
	if _, err := db.GetSetting("settings", &c.settings); err != nil {
		cfg.Log.Warn("core: settings unreadable; using defaults", "err", err)
	}
	rows, err := db.Threads()
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, row := range rows {
		t := &thread{row: *row}
		// A thread that was working when the app stopped lost its process; say so.
		switch t.row.Status {
		case StatusStarting, StatusRunning, StatusWaiting:
			t.row.Status = StatusInterrupted
			t.row.Error = "Solo was closed while this thread was working."
			_ = db.SaveThread(&t.row)
		}
		c.threads[t.row.ID] = t
		c.bySession[t.row.SessionID] = t
	}
	c.agent = agent.NewManager(cfg.Agent, agent.Callbacks{
		OnEvent:    c.onEvent,
		OnToolCall: c.onToolCall,
		OnAsk:      c.onAsk,
	}, cfg.Log)
	c.repos = git.Scan(c.settings.Folders, c.settings.ScanDepth, c.worktreesDir())
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
	f(&s)
	if s.MaxConcurrent < 1 {
		s.MaxConcurrent = 1
	}
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
	repos := git.Scan(s.Folders, s.ScanDepth, c.worktreesDir())
	c.mu.Lock()
	c.repos = repos
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
		return "", fmt.Errorf("no repository named %q; known repositories are listed by list_repos", nameOrPath)
	default:
		return "", fmt.Errorf("%q matches several repositories (%v); use the full path", nameOrPath, matches)
	}
}

// RateLimit returns the latest rate limit event, if any.
func (c *Core) RateLimit() agent.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rateLimit
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
