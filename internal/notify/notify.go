// Package notify emits user-facing notifications.
//
// Multi sinks: Log (always on) + optional Slack (incoming webhook) +
// optional macOS Notification Center (via osascript). All plug into
// the same Notifier interface so the scheduler doesn't care how many
// sinks are active.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/execx"
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

// FromConfig returns a Notifier that fans out to every configured sink.
// Log is always included. Slack is included iff slack_enabled AND a
// webhook URL is set. macOS is included iff macos_notifications.
func FromConfig(cfg config.NotifyConfig, logger *slog.Logger, runner execx.Runner) Notifier {
	if logger == nil {
		logger = slog.Default()
	}
	fan := Fan{}
	fan = append(fan, &Log{Logger: logger})
	if cfg.SlackEnabled {
		if cfg.SlackWebhookURL == "" {
			logger.Warn("slack enabled but slack_webhook_url is empty; skipping slack sink")
		} else {
			fan = append(fan, &Slack{
				WebhookURL: cfg.SlackWebhookURL,
				Channel:    cfg.SlackChannel,
				Client:     &http.Client{Timeout: 10 * time.Second},
				Logger:     logger,
			})
		}
	}
	if cfg.MacOSNotifications {
		fan = append(fan, &MacOS{Runner: runner, Logger: logger})
	}
	return fan
}

// Fan dispatches to every sink, best-effort. Errors from individual
// sinks are logged but not returned so one broken sink can't block
// another.
type Fan []Notifier

// Notify implements Notifier.
func (f Fan) Notify(ctx context.Context, ev Event) error {
	for _, n := range f {
		if err := n.Notify(ctx, ev); err != nil {
			slog.Default().Warn("notifier sink failed", "err", err)
		}
	}
	return nil
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

// Slack posts to an Incoming Webhook URL. Keeps payloads small; Slack
// rate-limits unknown clients generously but still finite.
type Slack struct {
	WebhookURL string
	Channel    string // optional; displayed in text only. The webhook URL determines destination.
	Client     *http.Client
	Logger     *slog.Logger
}

type slackPayload struct {
	Text string `json:"text"`
}

// Notify implements Notifier. Only routes events that matter — we skip
// routine "acted" successes to avoid notification fatigue; those show
// up in `aupr status` anyway.
func (s *Slack) Notify(ctx context.Context, ev Event) error {
	// Filter to high-signal events.
	switch ev.Kind {
	case "error", "circuit-breaker", "flagged":
		// route
	case "acted":
		// route — but users can silence these by filtering on their
		// Slack side if desired. Acted events are the whole point.
	default:
		return nil
	}

	body := s.format(ev)
	payload, err := json.Marshal(slackPayload{Text: body})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("slack post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack post: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *Slack) format(ev Event) string {
	prefix := map[string]string{
		"acted":           ":white_check_mark:",
		"error":           ":warning:",
		"circuit-breaker": ":no_entry:",
		"flagged":         ":triangular_flag_on_post:",
	}[ev.Kind]
	if prefix == "" {
		prefix = "•"
	}
	head := fmt.Sprintf("%s aupr %s — %s#%d", prefix, ev.Kind, ev.Repo, ev.PR)
	if ev.URL != "" {
		head = fmt.Sprintf("%s (<%s|view>)", head, ev.URL)
	}
	if ev.Summary != "" {
		head += "\n  " + ev.Summary
	}
	if ev.Detail != "" {
		head += "\n  `" + ev.Detail + "`"
	}
	return head
}

// MacOS posts via `osascript -e 'display notification ...'`. Subject to
// macOS notification permissions for the invoking process.
type MacOS struct {
	Runner execx.Runner
	Logger *slog.Logger
}

// Notify implements Notifier.
func (m *MacOS) Notify(ctx context.Context, ev Event) error {
	// Only errors / circuit-breakers by default — successes flood.
	switch ev.Kind {
	case "error", "circuit-breaker", "flagged":
	default:
		return nil
	}
	title := fmt.Sprintf("aupr %s", ev.Kind)
	subtitle := fmt.Sprintf("%s#%d", ev.Repo, ev.PR)
	body := ev.Summary
	if body == "" {
		body = ev.Detail
	}
	// osascript wants AppleScript string syntax; escape quotes and backslashes.
	script := fmt.Sprintf(
		`display notification %q with title %q subtitle %q`,
		body, title, subtitle,
	)
	if _, err := m.Runner.Run(ctx, "osascript", "-e", script); err != nil {
		return fmt.Errorf("osascript: %w", err)
	}
	return nil
}
