package state

import (
	"context"
	"sync"
	"time"
)

// Memory is an in-process Store used in tests.
type Memory struct {
	mu          sync.Mutex
	cursors     map[string]Cursor
	attempts    []Attempt
	sessions    map[string]Session
	skipped     map[string]Skip
	paused      bool
	pauseReason string
}

// NewMemory returns an empty Memory store.
func NewMemory() *Memory {
	return &Memory{
		cursors:  map[string]Cursor{},
		sessions: map[string]Session{},
		skipped:  map[string]Skip{},
	}
}

func prKey(repo string, n int) string { return repo + "#" + itoa(n) }
func sessionKey(repo string, n int, agent string) string {
	return repo + "#" + itoa(n) + "#" + agent
}

// LastSeen implements Store.
func (m *Memory) LastSeen(_ context.Context, repo string, prNumber int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursors[prKey(repo, prNumber)].LastEventID, nil
}

// RecordSeen implements Store.
func (m *Memory) RecordSeen(_ context.Context, repo string, prNumber int, eventID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursors[prKey(repo, prNumber)] = Cursor{
		Repo: repo, PRNumber: prNumber, LastEventID: eventID, UpdatedAt: time.Now(),
	}
	return nil
}

// AllCursors implements Store.
func (m *Memory) AllCursors(_ context.Context) ([]Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Cursor, 0, len(m.cursors))
	for _, c := range m.cursors {
		out = append(out, c)
	}
	return out, nil
}

// AllRecentAttempts implements Store.
func (m *Memory) AllRecentAttempts(_ context.Context, limit int) ([]Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Attempt
	for i := len(m.attempts) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.attempts[i])
	}
	return out, nil
}

// IsPaused implements Store.
func (m *Memory) IsPaused(_ context.Context) (bool, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paused, m.pauseReason, nil
}

// Pause implements Store.
func (m *Memory) Pause(_ context.Context, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paused, m.pauseReason = true, reason
	return nil
}

// Unpause implements Store.
func (m *Memory) Unpause(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paused, m.pauseReason = false, ""
	return nil
}

// RecordAttempt implements Store.
func (m *Memory) RecordAttempt(_ context.Context, a Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts = append(m.attempts, a)
	return nil
}

// RecentAttempts implements Store. Returns newest-first.
func (m *Memory) RecentAttempts(_ context.Context, repo string, prNumber int, limit int) ([]Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Attempt
	for i := len(m.attempts) - 1; i >= 0 && len(out) < limit; i-- {
		a := m.attempts[i]
		if a.Repo == repo && a.PRNumber == prNumber {
			out = append(out, a)
		}
	}
	return out, nil
}

// SaveSession implements Store.
func (m *Memory) SaveSession(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sessionKey(s.Repo, s.PRNumber, s.Agent)] = s
	return nil
}

// LoadSession implements Store.
func (m *Memory) LoadSession(_ context.Context, repo string, prNumber int, agent string) (Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionKey(repo, prNumber, agent)]
	return s, ok, nil
}

// IsSkipped implements Store.
func (m *Memory) IsSkipped(_ context.Context, repo string, prNumber int) (bool, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.skipped[prKey(repo, prNumber)]
	return ok, s.Reason, nil
}

// Skip implements Store.
func (m *Memory) Skip(_ context.Context, repo string, prNumber int, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skipped[prKey(repo, prNumber)] = Skip{Repo: repo, PRNumber: prNumber, Reason: reason, AddedAt: time.Now()}
	return nil
}

// Unskip implements Store.
func (m *Memory) Unskip(_ context.Context, repo string, prNumber int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.skipped, prKey(repo, prNumber))
	return nil
}

// ListSkipped implements Store.
func (m *Memory) ListSkipped(_ context.Context) ([]Skip, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Skip, 0, len(m.skipped))
	for _, s := range m.skipped {
		out = append(out, s)
	}
	return out, nil
}

// Close implements Store.
func (m *Memory) Close() error { return nil }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
