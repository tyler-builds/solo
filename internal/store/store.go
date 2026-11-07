// Package store keeps Solo's durable state in SQLite: settings, threads, and the transcript items
// shown for each thread. Claude's own transcripts (for resume) live in Claude's directory, not here.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS threads (
	id             TEXT PRIMARY KEY,
	kind           TEXT NOT NULL,
	session_id     TEXT NOT NULL,
	has_transcript INTEGER NOT NULL DEFAULT 0,
	title          TEXT NOT NULL,
	repo_path      TEXT NOT NULL DEFAULT '',
	worktree_path  TEXT NOT NULL DEFAULT '',
	branch         TEXT NOT NULL DEFAULT '',
	base_branch    TEXT NOT NULL DEFAULT '',
	base_commit    TEXT NOT NULL DEFAULT '',
	model          TEXT NOT NULL DEFAULT '',
	status         TEXT NOT NULL,
	summary        TEXT NOT NULL DEFAULT '',
	error          TEXT NOT NULL DEFAULT '',
	prompt         TEXT NOT NULL DEFAULT '',
	prompt_images  TEXT NOT NULL DEFAULT '',
	done_at        INTEGER NOT NULL DEFAULT 0,
	pr             TEXT NOT NULL DEFAULT '',
	cost_usd       REAL NOT NULL DEFAULT 0,
	context_tokens INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS items (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	thread_id  TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	kind       TEXT NOT NULL,
	turn_uuid  TEXT NOT NULL DEFAULT '',
	ref        TEXT NOT NULL DEFAULT '',
	data       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS items_thread ON items(thread_id, id);
CREATE INDEX IF NOT EXISTS items_ref ON items(thread_id, ref);
`

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// One connection: writes are small and frequent, and this rules out SQLITE_BUSY between our own
	// connections.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	// Columns added after the first release. SQLite has no ADD COLUMN IF NOT EXISTS.
	for _, col := range []string{`prompt_images TEXT NOT NULL DEFAULT ''`, `done_at INTEGER NOT NULL DEFAULT 0`, `pr TEXT NOT NULL DEFAULT ''`} {
		if _, err := db.Exec(`ALTER TABLE threads ADD COLUMN ` + col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("store: migrate: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// GetSetting decodes the setting into v. It reports false if the setting is unset.
func (s *Store) GetSetting(key string, v any) (bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

// SetSetting stores v as JSON.
func (s *Store) SetSetting(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(b))
	return err
}

// Thread kinds.
const (
	KindOrchestrator = "orchestrator"
	KindWorker       = "worker"
)

// Thread is one row of threads.
type Thread struct {
	ID            string
	Kind          string
	SessionID     string
	HasTranscript bool
	Title         string
	RepoPath      string
	WorktreePath  string
	Branch        string
	BaseBranch    string
	BaseCommit    string
	Model         string
	Status        string
	Summary       string
	Error         string
	Prompt        string
	PromptImages  []string  // image files sent with the prompt
	DoneAt        time.Time // when the user marked it done (it leaves the rail for History); zero while open
	PR            string    // its pull request, as JSON (see forge.PR); "" when none
	CostUSD       float64
	ContextTokens int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const threadCols = `id, kind, session_id, has_transcript, title, repo_path, worktree_path, branch, base_branch,
	base_commit, model, status, summary, error, prompt, prompt_images, done_at, pr, cost_usd, context_tokens, created_at, updated_at`

// SaveThread inserts or replaces the thread.
func (s *Store) SaveThread(t *Thread) error {
	_, err := s.db.Exec(`INSERT INTO threads(`+threadCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, session_id=excluded.session_id,
		has_transcript=excluded.has_transcript, title=excluded.title, repo_path=excluded.repo_path,
		worktree_path=excluded.worktree_path, branch=excluded.branch, base_branch=excluded.base_branch,
		base_commit=excluded.base_commit, model=excluded.model, status=excluded.status, summary=excluded.summary,
		error=excluded.error, prompt=excluded.prompt, prompt_images=excluded.prompt_images, done_at=excluded.done_at, pr=excluded.pr, cost_usd=excluded.cost_usd,
		context_tokens=excluded.context_tokens, updated_at=excluded.updated_at`,
		t.ID, t.Kind, t.SessionID, t.HasTranscript, t.Title, t.RepoPath, t.WorktreePath, t.Branch, t.BaseBranch,
		t.BaseCommit, t.Model, t.Status, t.Summary, t.Error, t.Prompt, joinPaths(t.PromptImages), unixMilli(t.DoneAt), t.PR, t.CostUSD, t.ContextTokens,
		t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli())
	return err
}

// Threads returns every thread, oldest first.
func (s *Store) Threads() ([]*Thread, error) {
	rows, err := s.db.Query(`SELECT ` + threadCols + ` FROM threads ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Thread
	for rows.Next() {
		var t Thread
		var created, updated int64
		var images string
		var done int64
		if err := rows.Scan(&t.ID, &t.Kind, &t.SessionID, &t.HasTranscript, &t.Title, &t.RepoPath, &t.WorktreePath,
			&t.Branch, &t.BaseBranch, &t.BaseCommit, &t.Model, &t.Status, &t.Summary, &t.Error, &t.Prompt, &images, &done, &t.PR,
			&t.CostUSD, &t.ContextTokens, &created, &updated); err != nil {
			return nil, err
		}
		t.PromptImages = splitPaths(images)
		if done != 0 {
			t.DoneAt = time.UnixMilli(done)
		}
		t.CreatedAt, t.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// PathReferenced reports whether a thread other than exceptThread mentions path in a message or in
// the images of its prompt.
func (s *Store) PathReferenced(path, exceptThread string) (bool, error) {
	enc, _ := json.Marshal(path) // as it appears inside item data
	var n int
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM items WHERE thread_id != ? AND instr(data, ?) > 0) +
		(SELECT COUNT(*) FROM threads WHERE id != ? AND instr(prompt_images, ?) > 0)`,
		exceptThread, string(enc), exceptThread, path).Scan(&n)
	return n > 0, err
}

// DeleteThread removes the thread and its items.
func (s *Store) DeleteThread(id string) error {
	_, err := s.db.Exec(`DELETE FROM threads WHERE id = ?`, id)
	return err
}

// Item is one transcript entry. Data is the kind-specific payload as JSON.
type Item struct {
	ID        int64
	ThreadID  string
	Kind      string
	TurnUUID  string
	Ref       string // an identifier to find the item again, such as a tool_use id
	Data      json.RawMessage
	CreatedAt time.Time
}

// AddItem appends an item and sets its ID.
func (s *Store) AddItem(it *Item) error {
	res, err := s.db.Exec(`INSERT INTO items(thread_id, kind, turn_uuid, ref, data, created_at) VALUES(?,?,?,?,?,?)`,
		it.ThreadID, it.Kind, it.TurnUUID, it.Ref, string(it.Data), it.CreatedAt.UnixMilli())
	if err != nil {
		return err
	}
	it.ID, err = res.LastInsertId()
	return err
}

// UpdateItemData replaces an item's data.
func (s *Store) UpdateItemData(id int64, data json.RawMessage) error {
	_, err := s.db.Exec(`UPDATE items SET data = ? WHERE id = ?`, string(data), id)
	return err
}

// Items returns up to limit of the thread's newest items, in order. limit <= 0 returns all.
func (s *Store) Items(ctx context.Context, threadID string, limit int) ([]*Item, error) {
	q := `SELECT id, thread_id, kind, turn_uuid, ref, data, created_at FROM items WHERE thread_id = ? ORDER BY id`
	args := []any{threadID}
	if limit > 0 {
		q = `SELECT * FROM (SELECT id, thread_id, kind, turn_uuid, ref, data, created_at FROM items WHERE thread_id = ?
			ORDER BY id DESC LIMIT ?) ORDER BY id`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		var it Item
		var data string
		var created int64
		if err := rows.Scan(&it.ID, &it.ThreadID, &it.Kind, &it.TurnUUID, &it.Ref, &data, &created); err != nil {
			return nil, err
		}
		it.Data, it.CreatedAt = json.RawMessage(data), time.UnixMilli(created)
		out = append(out, &it)
	}
	return out, rows.Err()
}

// ItemByID returns one item.
func (s *Store) ItemByID(id int64) (*Item, error) {
	var it Item
	var data string
	var created int64
	err := s.db.QueryRow(`SELECT id, thread_id, kind, turn_uuid, ref, data, created_at FROM items WHERE id = ?`, id).
		Scan(&it.ID, &it.ThreadID, &it.Kind, &it.TurnUUID, &it.Ref, &data, &created)
	if err != nil {
		return nil, err
	}
	it.Data, it.CreatedAt = json.RawMessage(data), time.UnixMilli(created)
	return &it, nil
}

// Paths are stored one per line; a path can't hold a newline that matters here.
func joinPaths(p []string) string { return strings.Join(p, "\n") }

func splitPaths(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// unixMilli is t in milliseconds, with the zero time as 0.
func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
