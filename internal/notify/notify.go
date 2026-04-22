// Package notify emits user-facing notifications.
//
// M3 ships only a log-based notifier. Slack / macOS / webhook transports
// will plug into the same interface in M4 without changing callers.
package notify

import (
	"context"
	"log/slog"
)

// Event is one notification payload.
type Event struct {
	Kind    string // "acted" | "flagged" | "error" | "skip" | "circuit-breaker"
	Repo    string
	PR      int
	Summary string
	URL     string
	Detail  string
}

// Notifier is the minimal sink interface.
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

// Log writes events to slog at INFO. Always available, always cheap.
type Log struct {
	Logger *slog.Logger
}

// Notify implements Notifier.
func (l *Log) Notify(_ context.Context, ev Event) error {
	log := l.Logger
	if log == nil {
		log = slog.Default()
	}
	log.Info("notify",
		"kind", ev.Kind,
		"repo", ev.Repo,
		"pr", ev.PR,
		"summary", ev.Summary,
		"url", ev.URL,
		"detail", ev.Detail)
	return nil
}
