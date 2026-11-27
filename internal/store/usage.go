//go:build !js

package store

import (
	"context"
	"time"
)

// usageSchema keeps the history the Usage page draws: each reading of a usage limit, and what each
// thread's turns cost. Both outlive the threads they came from, so a deleted thread still counts.
const usageSchema = `
CREATE TABLE IF NOT EXISTS usage_readings (
	at        INTEGER NOT NULL,
	kind      TEXT NOT NULL,
	used      REAL NOT NULL,
	resets_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS usage_readings_at ON usage_readings(at);
CREATE TABLE IF NOT EXISTS usage_turns (
	at        INTEGER NOT NULL,
	thread_id TEXT NOT NULL,
	title     TEXT NOT NULL DEFAULT '',
	cost_usd  REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS usage_turns_at ON usage_turns(at);
`

// UsageKeep is how long usage history is kept.
const UsageKeep = 35 * 24 * time.Hour

// UsageReading is one reading of a usage limit: how much of it was used, and when the window it
// measures resets.
type UsageReading struct {
	At       time.Time
	Kind     string  // five_hour, seven_day, …
	Used     float64 // a fraction, as Claude Code reports it; can pass 1
	ResetsAt time.Time
}

// UsageTurn is what one turn of a thread cost, at API prices.
type UsageTurn struct {
	At       time.Time
	ThreadID string
	Title    string // the thread's title when the turn ended
	CostUSD  float64
}

// AddUsageReading records a reading, unless it says the same as the last one of its kind.
func (s *Store) AddUsageReading(r UsageReading) error {
	var used float64
	var resets int64
	err := s.db.QueryRow(`SELECT used, resets_at FROM usage_readings WHERE kind = ? ORDER BY at DESC LIMIT 1`, r.Kind).
		Scan(&used, &resets)
	if err == nil && used == r.Used && resets == unixMilli(r.ResetsAt) {
		return nil
	}
	_, err = s.db.Exec(`INSERT INTO usage_readings(at, kind, used, resets_at) VALUES(?,?,?,?)`,
		r.At.UnixMilli(), r.Kind, r.Used, unixMilli(r.ResetsAt))
	return err
}

// UsageReadings returns the readings taken since a time, oldest first.
func (s *Store) UsageReadings(ctx context.Context, since time.Time) ([]UsageReading, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, kind, used, resets_at FROM usage_readings WHERE at >= ? ORDER BY at`,
		since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageReading
	for rows.Next() {
		var r UsageReading
		var at, resets int64
		if err := rows.Scan(&at, &r.Kind, &r.Used, &resets); err != nil {
			return nil, err
		}
		r.At = time.UnixMilli(at)
		if resets > 0 {
			r.ResetsAt = time.UnixMilli(resets)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddUsageTurn records what a turn cost.
func (s *Store) AddUsageTurn(u UsageTurn) error {
	_, err := s.db.Exec(`INSERT INTO usage_turns(at, thread_id, title, cost_usd) VALUES(?,?,?,?)`,
		u.At.UnixMilli(), u.ThreadID, u.Title, u.CostUSD)
	return err
}

// UsageTurns returns the turns that ended since a time, oldest first.
func (s *Store) UsageTurns(ctx context.Context, since time.Time) ([]UsageTurn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, thread_id, title, cost_usd FROM usage_turns WHERE at >= ? ORDER BY at`,
		since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageTurn
	for rows.Next() {
		var u UsageTurn
		var at int64
		if err := rows.Scan(&at, &u.ThreadID, &u.Title, &u.CostUSD); err != nil {
			return nil, err
		}
		u.At = time.UnixMilli(at)
		out = append(out, u)
	}
	return out, rows.Err()
}

// PruneUsage removes the usage history from before a time.
func (s *Store) PruneUsage(before time.Time) error {
	if _, err := s.db.Exec(`DELETE FROM usage_readings WHERE at < ?`, before.UnixMilli()); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM usage_turns WHERE at < ?`, before.UnixMilli())
	return err
}
