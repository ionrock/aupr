package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/execx"
)

type fakeRunner struct {
	calls atomic.Int32
	last  struct {
		name string
		args []string
	}
	mu sync.Mutex
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (*execx.Result, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.last.name = name
	f.last.args = append([]string(nil), args...)
	f.mu.Unlock()
	return &execx.Result{}, nil
}
func (f *fakeRunner) RunIn(ctx context.Context, _ string, name string, args ...string) (*execx.Result, error) {
	return f.Run(ctx, name, args...)
}

func TestSlackPostsExpectedPayload(t *testing.T) {
	var got slackPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := &Slack{WebhookURL: srv.URL}
	err := s.Notify(context.Background(), Event{
		Kind: "acted", Repo: "x/y", PR: 42, Summary: "fixed typo",
		URL: "https://example/pr/42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "x/y#42") {
		t.Errorf("payload missing repo/pr: %q", got.Text)
	}
	if !strings.Contains(got.Text, "fixed typo") {
		t.Errorf("payload missing summary: %q", got.Text)
	}
}

func TestSlackSkipsNoiseKinds(t *testing.T) {
	hits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := &Slack{WebhookURL: srv.URL}
	// kinds not in the allow-list should not hit the webhook at all
	_ = s.Notify(context.Background(), Event{Kind: "unrelated", Repo: "x/y", PR: 1})
	if n := hits.Load(); n != 0 {
		t.Errorf("unrelated kinds should not POST, got %d hits", n)
	}
}

func TestMacOSOnlyRoutesImportantKinds(t *testing.T) {
	fr := &fakeRunner{}
	m := &MacOS{Runner: fr}
	_ = m.Notify(context.Background(), Event{Kind: "acted", Repo: "x/y", PR: 1})
	if n := fr.calls.Load(); n != 0 {
		t.Errorf("'acted' should not notify macOS, got %d calls", n)
	}
	_ = m.Notify(context.Background(), Event{Kind: "error", Repo: "x/y", PR: 1, Summary: "boom"})
	if fr.calls.Load() != 1 {
		t.Errorf("expected one osascript call, got %d", fr.calls.Load())
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.last.name != "osascript" {
		t.Errorf("want osascript, got %q", fr.last.name)
	}
	joined := strings.Join(fr.last.args, " ")
	if !strings.Contains(joined, "display notification") {
		t.Errorf("missing applescript directive: %q", joined)
	}
}

func TestFanSinkErrorsDoNotBlockOthers(t *testing.T) {
	called := atomic.Int32{}
	f := Fan{
		notifierFunc(func(_ context.Context, _ Event) error { return io.ErrUnexpectedEOF }),
		notifierFunc(func(_ context.Context, _ Event) error { called.Add(1); return nil }),
	}
	_ = f.Notify(context.Background(), Event{Kind: "acted"})
	if called.Load() != 1 {
		t.Errorf("second sink should have been called even after first errored")
	}
}

type notifierFunc func(context.Context, Event) error

func (n notifierFunc) Notify(ctx context.Context, ev Event) error { return n(ctx, ev) }

func TestFromConfigFanoutShape(t *testing.T) {
	cfg := config.NotifyConfig{
		SlackEnabled:       true,
		SlackWebhookURL:    "https://example/hooks/xyz",
		MacOSNotifications: true,
	}
	n := FromConfig(cfg, nil, &fakeRunner{})
	fan, ok := n.(Fan)
	if !ok {
		t.Fatalf("want Fan, got %T", n)
	}
	if len(fan) != 3 { // Log + Slack + MacOS
		t.Errorf("want 3 sinks, got %d", len(fan))
	}
}

func TestFromConfigSlackMissingURL(t *testing.T) {
	cfg := config.NotifyConfig{SlackEnabled: true, SlackWebhookURL: ""}
	n := FromConfig(cfg, nil, &fakeRunner{})
	fan := n.(Fan)
	// Only Log should be present.
	if len(fan) != 1 {
		t.Errorf("want 1 sink (Log only), got %d", len(fan))
	}
}
