package worktree

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/discovery"
	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/feedback"
)

// --- parser ----------------------------------------------------------------

func TestParseWorktreeList(t *testing.T) {
	in := `worktree /Users/eric/Dagster/internal
HEAD abcdef0
branch refs/heads/main

worktree /Users/eric/.workset/internal/eric/feature-a
HEAD 1234567
branch refs/heads/eric/feature-a

worktree /Users/eric/.workset/internal/detached-bisect
HEAD 9999999
detached
`
	got := parseWorktreeList(in)
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d: %+v", len(got), got)
	}
	if !got[0].Main {
		t.Errorf("first entry should be Main: %+v", got[0])
	}
	if got[1].Branch != "eric/feature-a" {
		t.Errorf("branch parse: got %q", got[1].Branch)
	}
	if !got[2].Detached {
		t.Errorf("detached flag not set: %+v", got[2])
	}
}

// --- substitution ----------------------------------------------------------

func TestSubstituteAll(t *testing.T) {
	tokens := map[string]string{"repo": "internal", "branch": "eric/foo", "path": "/tmp/x"}
	got := substituteAll([]string{"git", "worktree", "add", "{path}", "{branch}"}, tokens)
	want := []string{"git", "worktree", "add", "/tmp/x", "eric/foo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// --- fake runner -----------------------------------------------------------

type fakeRunner struct {
	// Responses keyed by joined argv, in the order commands are issued.
	// Key form: "cwd|cmd arg1 arg2 ...".
	calls     []string
	canned    map[string]*execx.Result
	cannedErr map[string]error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (*execx.Result, error) {
	return f.RunIn(ctx, "", name, args...)
}

func (f *fakeRunner) RunIn(_ context.Context, dir, name string, args ...string) (*execx.Result, error) {
	key := dir + "|" + name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if err, ok := f.cannedErr[key]; ok {
		return &execx.Result{}, err
	}
	if r, ok := f.canned[key]; ok {
		return r, nil
	}
	return &execx.Result{}, nil
}

func (f *fakeRunner) set(key, stdout string) {
	if f.canned == nil {
		f.canned = map[string]*execx.Result{}
	}
	f.canned[key] = &execx.Result{Stdout: stdout}
}

func (f *fakeRunner) setErr(key string, err error) {
	if f.cannedErr == nil {
		f.cannedErr = map[string]error{}
	}
	f.cannedErr[key] = err
}

func newManager(cfg *config.WorktreeConfig, r execx.Runner) *Manager {
	return &Manager{Cfg: cfg, Runner: r}
}

func testRepo() discovery.Repo {
	return discovery.Repo{
		Path:    "/repo/internal",
		Owner:   "dagster-io",
		Name:    "internal",
		NWO:     "dagster-io/internal",
		Default: "origin",
	}
}

func testPR(branch string) feedback.PR {
	return feedback.PR{Repo: "dagster-io/internal", Number: 42, HeadRefName: branch}
}

// --- Plan() branches -------------------------------------------------------

func TestPlanUseExisting(t *testing.T) {
	r := &fakeRunner{}
	r.set("/repo/internal|git worktree list --porcelain",
		"worktree /repo/internal\nHEAD aaa\nbranch refs/heads/main\n\n"+
			"worktree /wt/feature-a\nHEAD bbb\nbranch refs/heads/eric/feature-a\n")

	m := newManager(&config.WorktreeConfig{Mode: "create"}, r)
	plan, err := m.Plan(context.Background(), testRepo(), testPR("eric/feature-a"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionUseExisting || plan.Path != "/wt/feature-a" {
		t.Errorf("want UseExisting @ /wt/feature-a, got %+v", plan)
	}
}

func TestPlanCreate(t *testing.T) {
	r := &fakeRunner{}
	r.set("/repo/internal|git worktree list --porcelain",
		"worktree /repo/internal\nHEAD aaa\nbranch refs/heads/main\n")

	cfg := &config.WorktreeConfig{
		Mode:          "create",
		PathTemplate:  "/tmp/workset/{repo}/{branch}",
		CreateCommand: []string{"git", "worktree", "add", "{path}", "{branch}"},
	}
	m := newManager(cfg, r)
	plan, err := m.Plan(context.Background(), testRepo(), testPR("eric/new-branch"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionCreate {
		t.Fatalf("want Create, got %s", plan.Action)
	}
	wantPath := "/tmp/workset/internal/eric/new-branch"
	if plan.Path != wantPath {
		t.Errorf("path: got %q want %q", plan.Path, wantPath)
	}
	wantCmd := []string{"git", "worktree", "add", wantPath, "eric/new-branch"}
	if !reflect.DeepEqual(plan.Command, wantCmd) {
		t.Errorf("command: got %v want %v", plan.Command, wantCmd)
	}
	if plan.RepoPath != "/repo/internal" {
		t.Errorf("RepoPath: got %q", plan.RepoPath)
	}
}

func TestPlanSkip(t *testing.T) {
	r := &fakeRunner{}
	r.set("/repo/internal|git worktree list --porcelain",
		"worktree /repo/internal\nHEAD aaa\nbranch refs/heads/main\n")

	cfg := &config.WorktreeConfig{Mode: "skip"}
	m := newManager(cfg, r)
	plan, err := m.Plan(context.Background(), testRepo(), testPR("eric/new"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionSkip {
		t.Errorf("want Skip, got %s", plan.Action)
	}
}

func TestPlanCheckoutMainOnOther(t *testing.T) {
	r := &fakeRunner{}
	r.set("/repo/internal|git worktree list --porcelain",
		"worktree /repo/internal\nHEAD aaa\nbranch refs/heads/main\n")
	r.set("/repo/internal|git status --porcelain", " M foo.go\n")

	cfg := &config.WorktreeConfig{Mode: "checkout"}
	m := newManager(cfg, r)
	plan, err := m.Plan(context.Background(), testRepo(), testPR("eric/target"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionCheckout {
		t.Fatalf("want Checkout, got %s", plan.Action)
	}
	if plan.CurrentBranch != "main" || plan.TargetBranch != "eric/target" {
		t.Errorf("branches: %+v", plan)
	}
	if !plan.Dirty {
		t.Errorf("dirty should be true")
	}
}

func TestPlanCheckoutMainAlreadyOnTarget(t *testing.T) {
	r := &fakeRunner{}
	r.set("/repo/internal|git worktree list --porcelain",
		"worktree /repo/internal\nHEAD aaa\nbranch refs/heads/eric/target\n")

	cfg := &config.WorktreeConfig{Mode: "checkout"}
	m := newManager(cfg, r)
	plan, err := m.Plan(context.Background(), testRepo(), testPR("eric/target"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionUseMain {
		t.Fatalf("want UseMain, got %s", plan.Action)
	}
}

func TestPlanDetachedMainSkips(t *testing.T) {
	r := &fakeRunner{}
	r.set("/repo/internal|git worktree list --porcelain",
		"worktree /repo/internal\nHEAD aaa\ndetached\n")

	cfg := &config.WorktreeConfig{Mode: "checkout"}
	m := newManager(cfg, r)
	plan, err := m.Plan(context.Background(), testRepo(), testPR("eric/target"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionSkip {
		t.Fatalf("detached HEAD should Skip, got %s", plan.Action)
	}
}

// --- Checkout flow incl. prompter ------------------------------------------

type denyPrompt struct{}

func (denyPrompt) Confirm(_ context.Context, _ *Plan) (bool, error) { return false, nil }

type acceptPrompt struct{}

func (acceptPrompt) Confirm(_ context.Context, _ *Plan) (bool, error) { return true, nil }

func TestAcquireCheckoutDeclined(t *testing.T) {
	plan := &Plan{Action: ActionCheckout, Path: "/repo/internal",
		CurrentBranch: "main", TargetBranch: "eric/target", Dirty: false}
	m := &Manager{Cfg: &config.WorktreeConfig{Mode: "checkout"}, Runner: &fakeRunner{}, Prompt: denyPrompt{}}
	_, err := m.Acquire(context.Background(), plan)
	if !errors.Is(err, ErrUserDeclined) {
		t.Fatalf("want ErrUserDeclined, got %v", err)
	}
}

func TestAcquireCheckoutStashAndRestore(t *testing.T) {
	r := &fakeRunner{}
	plan := &Plan{
		Action: ActionCheckout, Path: "/repo/internal",
		CurrentBranch: "main", TargetBranch: "eric/target", Dirty: true,
	}
	m := &Manager{Cfg: &config.WorktreeConfig{Mode: "checkout"}, Runner: r, Prompt: acceptPrompt{}}
	lease, err := m.Acquire(context.Background(), plan)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Expect the forward sequence in order.
	wantPrefix := []string{
		`/repo/internal|git stash push --include-untracked --message aupr: auto-stash main->eric/target`,
		`/repo/internal|git fetch origin eric/target`,
		`/repo/internal|git checkout eric/target`,
		`/repo/internal|git pull --rebase origin eric/target`,
	}
	for i, want := range wantPrefix {
		if i >= len(r.calls) || r.calls[i] != want {
			t.Errorf("forward call %d: got %q want %q", i, safe(r.calls, i), want)
		}
	}
	forwardLen := len(r.calls)
	// Release should restore: checkout original, pop stash.
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	wantRestore := []string{
		`/repo/internal|git checkout main`,
		`/repo/internal|git stash pop stash@{0}`,
	}
	for i, want := range wantRestore {
		idx := forwardLen + i
		if idx >= len(r.calls) || r.calls[idx] != want {
			t.Errorf("restore call %d: got %q want %q", i, safe(r.calls, idx), want)
		}
	}
	// Calling Release again should be a no-op.
	if err := lease.Release(context.Background()); err != nil {
		t.Errorf("second Release should be no-op, got %v", err)
	}
	if len(r.calls) != forwardLen+len(wantRestore) {
		t.Errorf("second Release performed calls: %v", r.calls[forwardLen+len(wantRestore):])
	}
}

func TestAcquireCheckoutFetchFailsTriggersRestore(t *testing.T) {
	r := &fakeRunner{}
	r.setErr("/repo/internal|git fetch origin eric/target", errors.New("network down"))
	plan := &Plan{
		Action: ActionCheckout, Path: "/repo/internal",
		CurrentBranch: "main", TargetBranch: "eric/target", Dirty: false,
	}
	m := &Manager{Cfg: &config.WorktreeConfig{Mode: "checkout"}, Runner: r, Prompt: acceptPrompt{}}
	if _, err := m.Acquire(context.Background(), plan); err == nil {
		t.Fatal("expected error")
	}
	// Must have attempted to restore the branch (no stash to pop; Dirty was false).
	foundRestore := false
	for _, c := range r.calls {
		if c == "/repo/internal|git checkout main" {
			foundRestore = true
			break
		}
	}
	if !foundRestore {
		t.Errorf("expected restore checkout; calls were: %v", r.calls)
	}
}

func safe(s []string, i int) string {
	if i >= len(s) {
		return "<MISSING>"
	}
	return s[i]
}
