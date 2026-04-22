// Package state persists aupr's operational memory: cursors, attempts,
// agent sessions, and the user-maintained skip list.
//
// The Store interface is the canonical API; Memory is used in tests,
// SQLite is used in production.
package state

import (
	"context"
	"time"
)

// Store is the persistence contract. All methods must be safe for
// concurrent use.
type Store interface {
	// Cursor
	LastSeen(ctx context.Context, repo string, prNumber int) (string, error)
	RecordSeen(ctx context.Context, repo string, prNumber int, eventID string) error

	// Attempts (for audit + circuit breaker)
	RecordAttempt(ctx context.Context, a Attempt) error
	RecentAttempts(ctx context.Context, repo string, prNumber int, limit int) ([]Attempt, error)

	// Agent sessions (for resume)
	SaveSession(ctx context.Context, s Session) error
	LoadSession(ctx context.Context, repo string, prNumber int, agent string) (Session, bool, error)

	// Skip list
	IsSkipped(ctx context.Context, repo string, prNumber int) (bool, string, error)
	Skip(ctx context.Context, repo string, prNumber int, reason string) error
	Unskip(ctx context.Context, repo string, prNumber int) error
	ListSkipped(ctx context.Context) ([]Skip, error)

	Close() error
}

// Attempt records one act-on-feedback invocation.
type Attempt struct {
	Repo       string
	PRNumber   int
	EventID    string
	StartedAt  time.Time
	FinishedAt time.Time
	Agent      string
	Outcome    string // "success" | "error" | "dry-run" | "declined" | "skipped"
	Summary    string
	CommitSHA  string
	Error      string
}

// Session is an agent session ID we can resume.
type Session struct {
	Repo       string
	PRNumber   int
	Agent      string
	SessionID  string
	LastUsedAt time.Time
}

// Skip is one entry in the skip list.
type Skip struct {
	Repo     string
	PRNumber int
	Reason   string
	AddedAt  time.Time
}
