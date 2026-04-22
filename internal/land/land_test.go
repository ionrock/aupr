package land

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/feedback"
)

type call struct {
	dir  string
	name string
	args []string
}

type fakeRunner struct {
	calls   []call
	stdouts map[string]string // keyed by "dir|name args"
	errs    map[string]error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (*execx.Result, error) {
	return f.RunIn(ctx, "", name, args...)
}
func (f *fakeRunner) RunIn(_ context.Context, dir, name string, args ...string) (*execx.Result, error) {
	key := dir + "|" + name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call{dir, name, args})
	if e, ok := f.errs[key]; ok {
		return &execx.Result{}, e
	}
	return &execx.Result{Stdout: f.stdouts[key]}, nil
}

func TestDetectQualityGates(t *testing.T) {
	dir := t.TempDir()
	if got := DetectQualityGates(dir); got != nil {
		t.Errorf("empty dir: want nil, got %v", got)
	}
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x"), 0o644)
	if got := DetectQualityGates(dir); len(got) != 1 || !strings.Contains(got[0], "go test") {
		t.Errorf("go.mod: got %v", got)
	}
}

func TestLandNoNewCommits(t *testing.T) {
	fr := &fakeRunner{stdouts: map[string]string{
		"/wk|git rev-parse HEAD":            "abc\n",
		"/wk|git rev-parse origin/eric/foo": "abc\n",
	}}
	l := &Lander{Runner: fr}
	pr := feedback.PR{Repo: "x/y", Number: 1, HeadRefName: "eric/foo"}
	_, err := l.Land(context.Background(), "/wk", pr, Options{DryRun: true}, "noop")
	if err == nil || !strings.Contains(err.Error(), "no new commits") {
		t.Fatalf("want no-new-commits error, got %v", err)
	}
}

func TestLandHappyPathDryRun(t *testing.T) {
	fr := &fakeRunner{stdouts: map[string]string{
		"/wk|git rev-parse HEAD":            "new1234\n",
		"/wk|git rev-parse origin/eric/foo": "old0000\n",
	}}
	l := &Lander{Runner: fr}
	pr := feedback.PR{Repo: "x/y", Number: 1, HeadRefName: "eric/foo"}
	res, err := l.Land(context.Background(), "/wk", pr, Options{DryRun: true, CommentOnPR: true}, "did stuff")
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed {
		t.Errorf("dry-run should not set Pushed")
	}
	// Only read-only git commands should have been issued.
	for _, c := range fr.calls {
		joined := c.name + " " + strings.Join(c.args, " ")
		if strings.HasPrefix(joined, "git push") || strings.HasPrefix(joined, "git pull") {
			t.Errorf("dry-run issued mutating command: %s", joined)
		}
		if c.name == "gh" {
			t.Errorf("dry-run issued gh: %v", c.args)
		}
	}
}

func TestLandPushFailsSurfacesError(t *testing.T) {
	fr := &fakeRunner{
		stdouts: map[string]string{
			"/wk|git rev-parse HEAD":            "new1234\n",
			"/wk|git rev-parse origin/eric/foo": "old0000\n",
		},
		errs: map[string]error{
			"/wk|git push origin eric/foo": errors.New("server rejected"),
		},
	}
	l := &Lander{Runner: fr}
	pr := feedback.PR{Repo: "x/y", Number: 1, HeadRefName: "eric/foo"}
	if _, err := l.Land(context.Background(), "/wk", pr, Options{}, "summary"); err == nil {
		t.Fatal("expected push error")
	}
}
