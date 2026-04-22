package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func testStores(t *testing.T) []Store {
	t.Helper()
	mem := NewMemory()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	sq, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	return []Store{mem, sq}
}

func TestCursorRoundTrip(t *testing.T) {
	for _, s := range testStores(t) {
		ctx := context.Background()
		if got, err := s.LastSeen(ctx, "r", 1); err != nil || got != "" {
			t.Fatalf("empty LastSeen: %q / %v", got, err)
		}
		if err := s.RecordSeen(ctx, "r", 1, "evt1"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.LastSeen(ctx, "r", 1); got != "evt1" {
			t.Errorf("got %q", got)
		}
		// overwrite
		_ = s.RecordSeen(ctx, "r", 1, "evt2")
		if got, _ := s.LastSeen(ctx, "r", 1); got != "evt2" {
			t.Errorf("overwrite: got %q", got)
		}
	}
}

func TestAttemptsAndCircuitBreaker(t *testing.T) {
	for _, s := range testStores(t) {
		ctx := context.Background()
		now := time.Now()
		for i := 0; i < 5; i++ {
			outcome := "error"
			if i == 4 {
				outcome = "success"
			}
			if err := s.RecordAttempt(ctx, Attempt{
				Repo: "r", PRNumber: 1, EventID: "e" + itoa(i),
				StartedAt: now, FinishedAt: now, Agent: "claude-code",
				Outcome: outcome,
			}); err != nil {
				t.Fatal(err)
			}
		}
		recent, err := s.RecentAttempts(ctx, "r", 1, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(recent) != 3 {
			t.Fatalf("want 3, got %d", len(recent))
		}
		// Newest first: the most recent is the success; the two before are errors.
		if recent[0].Outcome != "success" {
			t.Errorf("newest should be success, got %s", recent[0].Outcome)
		}
	}
}

func TestSkipList(t *testing.T) {
	for _, s := range testStores(t) {
		ctx := context.Background()
		if ok, _, _ := s.IsSkipped(ctx, "r", 1); ok {
			t.Fatal("should start empty")
		}
		_ = s.Skip(ctx, "r", 1, "noisy")
		ok, reason, _ := s.IsSkipped(ctx, "r", 1)
		if !ok || reason != "noisy" {
			t.Errorf("want skipped noisy, got %v %q", ok, reason)
		}
		_ = s.Unskip(ctx, "r", 1)
		if ok, _, _ := s.IsSkipped(ctx, "r", 1); ok {
			t.Errorf("unskip did not remove")
		}
	}
}

func TestSessions(t *testing.T) {
	for _, s := range testStores(t) {
		ctx := context.Background()
		if _, ok, _ := s.LoadSession(ctx, "r", 1, "claude-code"); ok {
			t.Fatal("should start empty")
		}
		_ = s.SaveSession(ctx, Session{
			Repo: "r", PRNumber: 1, Agent: "claude-code",
			SessionID: "sess-abc", LastUsedAt: time.Now(),
		})
		sess, ok, _ := s.LoadSession(ctx, "r", 1, "claude-code")
		if !ok || sess.SessionID != "sess-abc" {
			t.Errorf("want sess-abc, got %+v ok=%v", sess, ok)
		}
	}
}
