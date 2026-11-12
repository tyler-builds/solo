//go:build !js

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Full-text search (SQLite FTS5, BM25 ranking, Porter stemming) over Solo's memories and its archive:
// what the user and the agents said, and each thread's title, task and summary.
const searchSchema = `
CREATE TABLE IF NOT EXISTS memories (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	text          TEXT NOT NULL,
	keywords      TEXT NOT NULL DEFAULT '',
	repo          TEXT NOT NULL DEFAULT '',
	superseded_by INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(text, keywords, repo,
	content='memories', content_rowid='id', tokenize='porter unicode61');
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
	INSERT INTO memories_fts(rowid, text, keywords, repo) VALUES (new.id, new.text, new.keywords, new.repo);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
	INSERT INTO memories_fts(memories_fts, rowid, text, keywords, repo) VALUES ('delete', old.id, old.text, old.keywords, old.repo);
END;
CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE OF text, keywords, repo ON memories BEGIN
	INSERT INTO memories_fts(memories_fts, rowid, text, keywords, repo) VALUES ('delete', old.id, old.text, old.keywords, old.repo);
	INSERT INTO memories_fts(rowid, text, keywords, repo) VALUES (new.id, new.text, new.keywords, new.repo);
END;

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(text, thread_id UNINDEXED, tokenize='porter unicode61');
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON items
	WHEN new.kind IN ('user', 'assistant') AND coalesce(json_extract(new.data, '$.synthetic'), 0) = 0
	AND trim(coalesce(json_extract(new.data, '$.text'), '')) NOT IN ('', '(noted)') BEGIN
	INSERT INTO messages_fts(rowid, text, thread_id) VALUES (new.id, json_extract(new.data, '$.text'), new.thread_id);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON items BEGIN
	DELETE FROM messages_fts WHERE rowid = old.id;
END;

CREATE VIRTUAL TABLE IF NOT EXISTS threads_fts USING fts5(title, prompt, summary, thread_id UNINDEXED,
	tokenize='porter unicode61');
`

// searchVersion is bumped when the search tables need rebuilding from the rows they index.
const searchVersion = 1

// initSearch creates the search tables and fills them from rows written before they existed.
func (s *Store) initSearch() error {
	if _, err := s.db.Exec(searchSchema); err != nil {
		return err
	}
	var v int
	if _, err := s.GetSetting("search_version", &v); err != nil {
		return err
	}
	if v >= searchVersion {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM messages_fts;
		INSERT INTO messages_fts(rowid, text, thread_id)
			SELECT id, json_extract(data, '$.text'), thread_id FROM items
			WHERE kind IN ('user', 'assistant') AND coalesce(json_extract(data, '$.synthetic'), 0) = 0
			AND trim(coalesce(json_extract(data, '$.text'), '')) NOT IN ('', '(noted)');
		DELETE FROM threads_fts;
		INSERT INTO threads_fts(rowid, title, prompt, summary, thread_id)
			SELECT rowid, title, prompt, summary, id FROM threads;
		INSERT INTO memories_fts(memories_fts) VALUES ('rebuild');`); err != nil {
		return err
	}
	return s.SetSetting("search_version", searchVersion)
}

// indexThread keeps a thread's search entry in step with its row.
func (s *Store) indexThread(t *Thread) error {
	_, err := s.db.Exec(`DELETE FROM threads_fts WHERE thread_id = ?;
		INSERT INTO threads_fts(rowid, title, prompt, summary, thread_id)
			SELECT rowid, title, prompt, summary, id FROM threads WHERE id = ?`, t.ID, t.ID)
	return err
}

// Memory is one thing Solo remembers: a fact, decision or preference that outlasts a conversation.
type Memory struct {
	ID       int64
	Text     string
	Keywords string // other words it might be searched by
	Repo     string // the repository it is about; "" for all
	// SupersededBy is the memory that replaced it, or 0. A replaced memory is kept but no longer shown.
	SupersededBy int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const memoryCols = `id, text, keywords, repo, superseded_by, created_at, updated_at`

func scanMemories(rows *sql.Rows) ([]Memory, error) {
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		var created, updated int64
		if err := rows.Scan(&m.ID, &m.Text, &m.Keywords, &m.Repo, &m.SupersededBy, &created, &updated); err != nil {
			return nil, err
		}
		m.CreatedAt, m.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddMemory saves a memory and sets its ID. When it replaces others, they are marked superseded.
func (s *Store) AddMemory(m *Memory, replaces ...int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	res, err := tx.Exec(`INSERT INTO memories(text, keywords, repo, created_at, updated_at) VALUES(?,?,?,?,?)`,
		m.Text, m.Keywords, m.Repo, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, old := range replaces {
		r, err := tx.Exec(`UPDATE memories SET superseded_by = ?, updated_at = ? WHERE id = ? AND superseded_by = 0`, id, now.UnixMilli(), old)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return fmt.Errorf("no current memory #%d to replace", old)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.ID, m.CreatedAt, m.UpdatedAt = id, now, now
	return nil
}

// DeleteMemory removes a memory for good.
func (s *Store) DeleteMemory(id int64) error {
	res, err := s.db.Exec(`DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no memory #%d", id)
	}
	return nil
}

// Memories returns the current memories (not superseded), newest first.
func (s *Store) Memories(ctx context.Context) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+memoryCols+` FROM memories WHERE superseded_by = 0 ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	return scanMemories(rows)
}

// MessageHit is a message found in the archive, with a short snippet around what matched.
type MessageHit struct {
	ItemID    int64
	ThreadID  string
	Kind      string
	Snippet   string
	CreatedAt time.Time
}

// ThreadHit is a thread found by its title, task or summary.
type ThreadHit struct {
	ThreadID string
	Snippet  string
}

// ErrNoTerms means a query had no words worth searching for.
var ErrNoTerms = errors.New("store: nothing to search for")

// MatchQuery turns free text into an FTS5 query: its words, quoted so punctuation can't break the
// syntax, any of which may match (BM25 ranks documents with more, rarer matches first). Common words
// are dropped.
func MatchQuery(text string) (string, error) {
	var terms []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	}) {
		if seen[w] || stopWords[w] || (len(w) < 2 && !unicode.IsDigit(rune(w[0]))) {
			continue
		}
		seen[w] = true
		terms = append(terms, `"`+w+`"`)
	}
	if len(terms) == 0 {
		return "", ErrNoTerms
	}
	return strings.Join(terms, " OR "), nil
}

// SearchMemories ranks current memories against the query, best first. Keywords count less than the
// text, and the repository name a little.
func (s *Store) SearchMemories(ctx context.Context, query string, limit int) ([]Memory, error) {
	q, err := MatchQuery(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.text, m.keywords, m.repo, m.superseded_by, m.created_at, m.updated_at
		FROM memories_fts JOIN memories m ON m.id = memories_fts.rowid
		WHERE memories_fts MATCH ? AND m.superseded_by = 0
		ORDER BY bm25(memories_fts, 1.0, 0.7, 0.4) LIMIT ?`, q, limit)
	if err != nil {
		return nil, err
	}
	return scanMemories(rows)
}

// SearchMessages ranks one thread's archived messages against the query, best first.
func (s *Store) SearchMessages(ctx context.Context, threadID, query string, limit int) ([]MessageHit, error) {
	q, err := MatchQuery(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.thread_id, i.kind, snippet(messages_fts, 0, '', '', '…', 24), i.created_at
		FROM messages_fts JOIN items i ON i.id = messages_fts.rowid
		WHERE messages_fts MATCH ? AND messages_fts.thread_id = ?
		ORDER BY bm25(messages_fts) LIMIT ?`, q, threadID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageHit
	for rows.Next() {
		var h MessageHit
		var created int64
		if err := rows.Scan(&h.ItemID, &h.ThreadID, &h.Kind, &h.Snippet, &created); err != nil {
			return nil, err
		}
		h.CreatedAt = time.UnixMilli(created)
		out = append(out, h)
	}
	return out, rows.Err()
}

// SearchThreads ranks worker threads by their title, task and summary against the query, best first.
func (s *Store) SearchThreads(ctx context.Context, query string, limit int) ([]ThreadHit, error) {
	q, err := MatchQuery(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT thread_id, snippet(threads_fts, -1, '', '', '…', 20)
		FROM threads_fts WHERE threads_fts MATCH ?
		AND thread_id IN (SELECT id FROM threads WHERE kind = 'worker') ORDER BY bm25(threads_fts, 2.0, 1.0, 1.0) LIMIT ?`, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThreadHit
	for rows.Next() {
		var h ThreadHit
		if err := rows.Scan(&h.ThreadID, &h.Snippet); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// stopWords are words too common to search by.
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be but by can could did do does for from had has have
		he her his how i if in into is it its just me my of on or our she so than that the their them then there
		these they this to too us was we were what when where which who why will with would you your yours
		about again also any been being both each few more most other some such only own same very s t don
		should now ll re ve d m o y am let lets get got make made like want wants please thing things one`) {
		stopWords[w] = true
	}
}
