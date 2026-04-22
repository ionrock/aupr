package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

// SQLite is a persistent Store backed by a single sqlite file.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens (creating if needed) the sqlite database at path. The
// parent directory is created if missing. Schema is self-applied.
func OpenSQLite(path string) (*SQLite, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	s := &SQLite{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS pr_cursor (
    repo TEXT NOT NULL,
    pr_number INTEGER NOT NULL,
    last_seen_event_id TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (repo, pr_number)
);

CREATE TABLE IF NOT EXISTS attempts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo TEXT NOT NULL,
    pr_number INTEGER NOT NULL,
    event_id TEXT NOT NULL,
    started_at INTEGER NOT NULL,
    finished_at INTEGER NOT NULL,
    agent TEXT NOT NULL,
    outcome TEXT NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    commit_sha TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_attempts_pr ON attempts(repo, pr_number, id DESC);

CREATE TABLE IF NOT EXISTS agent_sessions (
    repo TEXT NOT NULL,
    pr_number INTEGER NOT NULL,
    agent TEXT NOT NULL,
    session_id TEXT NOT NULL,
    last_used_at INTEGER NOT NULL,
    PRIMARY KEY (repo, pr_number, agent)
);

CREATE TABLE IF NOT EXISTS pr_skip (
    repo TEXT NOT NULL,
    pr_number INTEGER NOT NULL,
    reason TEXT NOT NULL,
    added_at INTEGER NOT NULL,
    PRIMARY KEY (repo, pr_number)
);

CREATE TABLE IF NOT EXISTS daemon_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
`

func (s *SQLite) migrate() error {
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// LastSeen implements Store.
func (s *SQLite) LastSeen(ctx context.Context, repo string, prNumber int) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT last_seen_event_id FROM pr_cursor WHERE repo=? AND pr_number=?`,
		repo, prNumber).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// RecordSeen implements Store.
func (s *SQLite) RecordSeen(ctx context.Context, repo string, prNumber int, eventID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pr_cursor (repo, pr_number, last_seen_event_id, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(repo, pr_number) DO UPDATE SET
			last_seen_event_id = excluded.last_seen_event_id,
			updated_at = excluded.updated_at`,
		repo, prNumber, eventID, time.Now().Unix())
	return err
}

// AllCursors implements Store.
func (s *SQLite) AllCursors(ctx context.Context) ([]Cursor, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repo, pr_number, last_seen_event_id, updated_at FROM pr_cursor ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cursor
	for rows.Next() {
		var c Cursor
		var updated int64
		if err := rows.Scan(&c.Repo, &c.PRNumber, &c.LastEventID, &updated); err != nil {
			return nil, err
		}
		c.UpdatedAt = time.Unix(updated, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordAttempt implements Store.
func (s *SQLite) RecordAttempt(ctx context.Context, a Attempt) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO attempts (repo, pr_number, event_id, started_at, finished_at, agent, outcome, summary, commit_sha, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Repo, a.PRNumber, a.EventID, a.StartedAt.Unix(), a.FinishedAt.Unix(),
		a.Agent, a.Outcome, a.Summary, a.CommitSHA, a.Error)
	return err
}

// RecentAttempts implements Store.
func (s *SQLite) RecentAttempts(ctx context.Context, repo string, prNumber int, limit int) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT repo, pr_number, event_id, started_at, finished_at, agent, outcome, summary, commit_sha, error
		FROM attempts WHERE repo=? AND pr_number=?
		ORDER BY id DESC LIMIT ?`,
		repo, prNumber, limit)
	if err != nil {
		return nil, err
	}
	return scanAttempts(rows)
}

// AllRecentAttempts implements Store.
func (s *SQLite) AllRecentAttempts(ctx context.Context, limit int) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT repo, pr_number, event_id, started_at, finished_at, agent, outcome, summary, commit_sha, error
		FROM attempts ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanAttempts(rows)
}

func scanAttempts(rows *sql.Rows) ([]Attempt, error) {
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var started, finished int64
		if err := rows.Scan(&a.Repo, &a.PRNumber, &a.EventID, &started, &finished,
			&a.Agent, &a.Outcome, &a.Summary, &a.CommitSHA, &a.Error); err != nil {
			return nil, err
		}
		a.StartedAt = time.Unix(started, 0)
		a.FinishedAt = time.Unix(finished, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

// SaveSession implements Store.
func (s *SQLite) SaveSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_sessions (repo, pr_number, agent, session_id, last_used_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo, pr_number, agent) DO UPDATE SET
			session_id = excluded.session_id,
			last_used_at = excluded.last_used_at`,
		sess.Repo, sess.PRNumber, sess.Agent, sess.SessionID, sess.LastUsedAt.Unix())
	return err
}

// LoadSession implements Store.
func (s *SQLite) LoadSession(ctx context.Context, repo string, prNumber int, agent string) (Session, bool, error) {
	var sess Session
	var last int64
	err := s.db.QueryRowContext(ctx, `
		SELECT repo, pr_number, agent, session_id, last_used_at FROM agent_sessions
		WHERE repo=? AND pr_number=? AND agent=?`,
		repo, prNumber, agent).Scan(&sess.Repo, &sess.PRNumber, &sess.Agent, &sess.SessionID, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	sess.LastUsedAt = time.Unix(last, 0)
	return sess, true, nil
}

// IsSkipped implements Store.
func (s *SQLite) IsSkipped(ctx context.Context, repo string, prNumber int) (bool, string, error) {
	var reason string
	err := s.db.QueryRowContext(ctx,
		`SELECT reason FROM pr_skip WHERE repo=? AND pr_number=?`,
		repo, prNumber).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, reason, nil
}

// Skip implements Store.
func (s *SQLite) Skip(ctx context.Context, repo string, prNumber int, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pr_skip (repo, pr_number, reason, added_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(repo, pr_number) DO UPDATE SET reason = excluded.reason`,
		repo, prNumber, reason, time.Now().Unix())
	return err
}

// Unskip implements Store.
func (s *SQLite) Unskip(ctx context.Context, repo string, prNumber int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pr_skip WHERE repo=? AND pr_number=?`, repo, prNumber)
	return err
}

// ListSkipped implements Store.
func (s *SQLite) ListSkipped(ctx context.Context) ([]Skip, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repo, pr_number, reason, added_at FROM pr_skip ORDER BY added_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skip
	for rows.Next() {
		var s Skip
		var added int64
		if err := rows.Scan(&s.Repo, &s.PRNumber, &s.Reason, &added); err != nil {
			return nil, err
		}
		s.AddedAt = time.Unix(added, 0)
		out = append(out, s)
	}
	return out, rows.Err()
}

// IsPaused implements Store.
func (s *SQLite) IsPaused(ctx context.Context) (bool, string, error) {
	var reason string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM daemon_settings WHERE key='pause_reason'`).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return reason != "", reason, nil
}

// Pause implements Store.
func (s *SQLite) Pause(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "manual"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO daemon_settings (key, value, updated_at) VALUES ('pause_reason', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		reason, time.Now().Unix())
	return err
}

// Unpause implements Store.
func (s *SQLite) Unpause(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM daemon_settings WHERE key='pause_reason'`)
	return err
}

// Close implements Store.
func (s *SQLite) Close() error { return s.db.Close() }
