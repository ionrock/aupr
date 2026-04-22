// Package state holds the aupr cursor DB.
//
// M1 ships an in-memory no-op so `aupr once` works without any sqlite
// dependency. M3 will swap in a real sqlite-backed store via database/sql +
// modernc.org/sqlite.
package state

// Store tracks per-PR cursors and attempts.
type Store interface {
	LastSeen(repo string, prNumber int) (string, error)
	RecordSeen(repo string, prNumber int, eventID string) error
}

// Memory is a volatile Store used in M1.
type Memory struct {
	cursors map[string]string
}

// NewMemory returns an empty Memory.
func NewMemory() *Memory { return &Memory{cursors: map[string]string{}} }

// LastSeen implements Store.
func (m *Memory) LastSeen(repo string, prNumber int) (string, error) {
	return m.cursors[key(repo, prNumber)], nil
}

// RecordSeen implements Store.
func (m *Memory) RecordSeen(repo string, prNumber int, eventID string) error {
	m.cursors[key(repo, prNumber)] = eventID
	return nil
}

func key(repo string, prNumber int) string {
	return repo + "#" + itoa(prNumber)
}

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
