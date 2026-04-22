package recovery

import (
	"context"
	"strings"
	"testing"

	"github.com/ionrock/aupr/internal/discovery"
	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/notify"
	"github.com/ionrock/aupr/internal/state"
)

type fakeRunner struct {
	stdout map[string]string
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (*execx.Result, error) {
	return f.RunIn(ctx, "", name, args...)
}
func (f *fakeRunner) RunIn(_ context.Context, dir, name string, args ...string) (*execx.Result, error) {
	key := dir + "|" + name + " " + strings.Join(args, " ")
	return &execx.Result{Stdout: f.stdout[key]}, nil
}

type capturingNotifier struct {
	events []notify.Event
}

func (c *capturingNotifier) Notify(_ context.Context, ev notify.Event) error {
	c.events = append(c.events, ev)
	return nil
}

func TestScanFindsAuprStashesOnly(t *testing.T) {
	fr := &fakeRunner{stdout: map[string]string{
		"/repo|git stash list": `stash@{0}: On main: aupr: auto-stash main->eric/foo
stash@{1}: WIP on main: unrelated user stash
stash@{2}: On main: aupr: auto-stash main->eric/bar
`,
	}}
	n := &capturingNotifier{}
	st := state.NewMemory()
	s := &Scanner{Runner: fr, Store: st, Notifier: n}

	stashes, err := s.Scan(context.Background(), []discovery.Repo{
		{Path: "/repo", NWO: "x/y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stashes) != 2 {
		t.Fatalf("want 2 aupr stashes, got %d: %+v", len(stashes), stashes)
	}
	if len(n.events) != 2 {
		t.Errorf("want 2 notifications on first scan, got %d", len(n.events))
	}

	// Second scan with the same stashes should notify zero more.
	stashes2, _ := s.Scan(context.Background(), []discovery.Repo{{Path: "/repo", NWO: "x/y"}})
	if len(stashes2) != 2 {
		t.Errorf("second scan should still report 2, got %d", len(stashes2))
	}
	if len(n.events) != 2 {
		t.Errorf("repeat scan should not re-notify; got %d total events", len(n.events))
	}
}

func TestScanForgetsDisappearedStashes(t *testing.T) {
	fr := &fakeRunner{stdout: map[string]string{
		"/repo|git stash list": `stash@{0}: On main: aupr: auto-stash main->eric/foo
`,
	}}
	st := state.NewMemory()
	s := &Scanner{Runner: fr, Store: st}

	_, _ = s.Scan(context.Background(), []discovery.Repo{{Path: "/repo"}})
	// User popped the stash.
	fr.stdout["/repo|git stash list"] = ""
	_, _ = s.Scan(context.Background(), []discovery.Repo{{Path: "/repo"}})

	tracked, _ := st.ListRecoveryStashes(context.Background())
	if len(tracked) != 0 {
		t.Errorf("want 0 tracked after disappearance, got %d", len(tracked))
	}
}
