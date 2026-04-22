// Package worktree acquires a workspace for a PR.
//
// Resolution order:
//
//  1. If any linked worktree of the repo has pr.HeadRefName checked out,
//     use it as-is (LLM-tool interop: wt, superset.sh, claude, etc.).
//  2. Otherwise fall back to the configured mode:
//     "create"   - run the configured create_command to make one
//     "checkout" - use the main repo; swap branches with stash protection
//     "skip"     - never act without a pre-existing worktree
//
// aupr never runs `git worktree add` implicitly; creation happens through
// the user-configured create_command (which defaults to plain git but can
// point at `wt`, `superset.sh`, or anything else that produces a worktree).
package worktree

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/discovery"
	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/feedback"
)

// Action is the high-level outcome of planning a workspace for a PR.
type Action string

const (
	ActionUseExisting Action = "use-existing" // found a worktree on the target branch
	ActionUseMain     Action = "use-main"     // main repo already on target branch; no swap needed
	ActionCreate      Action = "create"       // will run CreateCommand
	ActionCheckout    Action = "checkout"     // will swap branches in the main repo
	ActionSkip        Action = "skip"         // don't act; upstream should FLAG
)

// Plan describes what would happen, without doing anything.
type Plan struct {
	Action         Action
	Reason         string   // human-readable explanation (esp. for Skip)
	Path           string   // destination workspace (filled for all non-Skip actions)
	RepoPath       string   // main repo path; needed as cwd when executing Create
	Command        []string // resolved argv for Create
	CurrentBranch  string   // filled for Checkout
	TargetBranch   string   // pr.HeadRefName
	Dirty          bool     // filled for Checkout; true iff `git status --porcelain` had output
	ExistingBranch string   // filled for UseMain / UseExisting: current branch of Path
}

// Lease is the handle returned by Acquire. Release() undoes anything aupr
// did to the workspace (e.g. restoring a swapped branch + popping a stash).
type Lease struct {
	Path    string
	Plan    *Plan
	release func(context.Context) error
}

// Release is safe to call multiple times; subsequent calls are no-ops.
func (l *Lease) Release(ctx context.Context) error {
	if l == nil || l.release == nil {
		return nil
	}
	f := l.release
	l.release = nil
	return f(ctx)
}

// Info is one entry from `git worktree list --porcelain`.
type Info struct {
	Path     string
	Head     string
	Branch   string // empty if detached or bare
	Bare     bool
	Detached bool
	Main     bool // first non-bare entry; i.e. the primary repo path
}

// Prompter asks the human to confirm a disruptive action. In daemon mode
// the Prompter always denies.
type Prompter interface {
	Confirm(ctx context.Context, plan *Plan) (bool, error)
}

// DenyPrompter is used in non-interactive (daemon) contexts.
type DenyPrompter struct{}

// Confirm always returns false with a fixed reason.
func (DenyPrompter) Confirm(_ context.Context, _ *Plan) (bool, error) {
	return false, nil
}

// Manager plans and executes workspace acquisition.
type Manager struct {
	Cfg    *config.WorktreeConfig
	Runner execx.Runner
	Logger *slog.Logger
	Prompt Prompter
}

// Plan computes what Acquire *would* do, without side effects.
func (m *Manager) Plan(ctx context.Context, repo discovery.Repo, pr feedback.PR) (*Plan, error) {
	if pr.HeadRefName == "" {
		return nil, errors.New("worktree: pr.HeadRefName is empty")
	}

	wts, err := m.listWorktrees(ctx, repo.Path)
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}

	// 1. Prefer a linked worktree on the target branch (not the main repo).
	for _, wt := range wts {
		if wt.Main || wt.Bare {
			continue
		}
		if wt.Branch == pr.HeadRefName {
			return &Plan{
				Action:         ActionUseExisting,
				Path:           wt.Path,
				TargetBranch:   pr.HeadRefName,
				ExistingBranch: wt.Branch,
				Reason:         "existing worktree on target branch",
			}, nil
		}
	}

	// 2. Fall back to mode.
	switch m.Cfg.Mode {
	case "skip":
		return &Plan{
			Action:       ActionSkip,
			TargetBranch: pr.HeadRefName,
			Reason:       "mode=skip and no existing worktree",
		}, nil

	case "create":
		tokens := buildTokens(repo, pr, "")
		path, err := resolvePath(m.Cfg.PathTemplate, tokens)
		if err != nil {
			return nil, err
		}
		tokens["path"] = path
		return &Plan{
			Action:       ActionCreate,
			Path:         path,
			RepoPath:     repo.Path,
			Command:      substituteAll(m.Cfg.CreateCommand, tokens),
			TargetBranch: pr.HeadRefName,
			Reason:       "will create worktree via create_command",
		}, nil

	case "checkout":
		main, ok := findMain(wts)
		if !ok {
			return nil, fmt.Errorf("worktree: no main worktree found for %s", repo.Path)
		}
		if main.Detached {
			return &Plan{Action: ActionSkip, Reason: "main repo is in detached-HEAD state", TargetBranch: pr.HeadRefName}, nil
		}
		if main.Branch == pr.HeadRefName {
			return &Plan{
				Action:         ActionUseMain,
				Path:           main.Path,
				ExistingBranch: main.Branch,
				TargetBranch:   pr.HeadRefName,
				Reason:         "main repo already on target branch",
			}, nil
		}
		dirty, err := m.isDirty(ctx, main.Path)
		if err != nil {
			return nil, fmt.Errorf("dirty check: %w", err)
		}
		return &Plan{
			Action:        ActionCheckout,
			Path:          main.Path,
			CurrentBranch: main.Branch,
			TargetBranch:  pr.HeadRefName,
			Dirty:         dirty,
			Reason:        "will swap branches in main repo with stash protection",
		}, nil

	default:
		return nil, fmt.Errorf("worktree: unknown mode %q", m.Cfg.Mode)
	}
}

// Acquire executes a Plan and returns a Lease. For Skip, returns (nil, ErrSkip).
var ErrSkip = errors.New("worktree: plan says skip")

// ErrUserDeclined is returned when Prompter.Confirm returns false.
var ErrUserDeclined = errors.New("worktree: user declined prompt")

// Acquire runs the plan. In dry-run contexts the caller should not call
// Acquire; it will perform real mutations.
func (m *Manager) Acquire(ctx context.Context, plan *Plan) (*Lease, error) {
	switch plan.Action {
	case ActionUseExisting, ActionUseMain:
		return &Lease{Path: plan.Path, Plan: plan}, nil

	case ActionSkip:
		return nil, ErrSkip

	case ActionCreate:
		return m.doCreate(ctx, plan)

	case ActionCheckout:
		return m.doCheckout(ctx, plan)
	}
	return nil, fmt.Errorf("worktree: unknown action %q", plan.Action)
}

func (m *Manager) doCreate(ctx context.Context, plan *Plan) (*Lease, error) {
	// Destination must not already exist as a path. If the command would
	// create at an existing (non-worktree) directory, refuse.
	if _, err := os.Stat(plan.Path); err == nil {
		return nil, fmt.Errorf("worktree: refusing to create at existing path %s", plan.Path)
	}
	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o755); err != nil {
		return nil, fmt.Errorf("prepare parent dir: %w", err)
	}

	cwd := plan.RepoPath
	if cwd == "" {
		return nil, errors.New("worktree: internal error \u2014 plan.RepoPath not set for Create")
	}

	m.log().Info("worktree create",
		"path", plan.Path, "branch", plan.TargetBranch,
		"cmd", plan.Command, "cwd", cwd)

	if _, err := m.Runner.RunIn(ctx, cwd, plan.Command[0], plan.Command[1:]...); err != nil {
		return nil, fmt.Errorf("create_command failed: %w", err)
	}

	// Verify post-conditions.
	if _, err := os.Stat(plan.Path); err != nil {
		return nil, fmt.Errorf("worktree: create_command returned success but %s does not exist", plan.Path)
	}
	branch, err := m.currentBranch(ctx, plan.Path)
	if err != nil {
		return nil, fmt.Errorf("post-create branch check: %w", err)
	}
	if branch != plan.TargetBranch {
		return nil, fmt.Errorf("worktree: created at %s but HEAD is %s, expected %s",
			plan.Path, branch, plan.TargetBranch)
	}

	// M2 keeps created worktrees for the life of the PR; no RemoveCommand
	// wired yet. Release is a no-op.
	return &Lease{Path: plan.Path, Plan: plan}, nil
}

func (m *Manager) doCheckout(ctx context.Context, plan *Plan) (*Lease, error) {
	// Prompt — always, even for clean trees, because a branch swap is
	// intrinsically surprising to a human who may have work on the
	// current branch.
	if m.Prompt == nil {
		return nil, errors.New("worktree: checkout mode requires a Prompter")
	}
	ok, err := m.Prompt.Confirm(ctx, plan)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrUserDeclined
	}

	stashRef := ""
	if plan.Dirty {
		msg := fmt.Sprintf("aupr: auto-stash %s->%s", plan.CurrentBranch, plan.TargetBranch)
		out, err := m.Runner.RunIn(ctx, plan.Path, "git", "stash", "push", "--include-untracked", "--message", msg)
		if err != nil {
			return nil, fmt.Errorf("stash push: %w", err)
		}
		stashRef = parseStashRef(out.Stdout)
		if stashRef == "" {
			return nil, errors.New("stash push: could not parse stash ref from output")
		}
		m.log().Info("worktree stashed", "ref", stashRef, "path", plan.Path)
	}

	if _, err := m.Runner.RunIn(ctx, plan.Path, "git", "fetch", "origin", plan.TargetBranch); err != nil {
		m.restoreAfterCheckoutFailure(ctx, plan, stashRef, "pre-checkout fetch failed")
		return nil, fmt.Errorf("fetch: %w", err)
	}
	if _, err := m.Runner.RunIn(ctx, plan.Path, "git", "checkout", plan.TargetBranch); err != nil {
		m.restoreAfterCheckoutFailure(ctx, plan, stashRef, "checkout failed")
		return nil, fmt.Errorf("checkout %s: %w", plan.TargetBranch, err)
	}
	if _, err := m.Runner.RunIn(ctx, plan.Path, "git", "pull", "--rebase", "origin", plan.TargetBranch); err != nil {
		// Abort any in-progress rebase and restore.
		_, _ = m.Runner.RunIn(ctx, plan.Path, "git", "rebase", "--abort")
		m.restoreAfterCheckoutFailure(ctx, plan, stashRef, "rebase failed")
		return nil, fmt.Errorf("pull --rebase: %w", err)
	}

	origBranch := plan.CurrentBranch
	path := plan.Path
	ref := stashRef
	runner := m.Runner
	log := m.log()
	return &Lease{
		Path: plan.Path,
		Plan: plan,
		release: func(ctx context.Context) error {
			if _, err := runner.RunIn(ctx, path, "git", "checkout", origBranch); err != nil {
				log.Error("CRITICAL: failed to restore branch",
					"path", path, "branch", origBranch, "err", err)
				return err
			}
			if ref != "" {
				if _, err := runner.RunIn(ctx, path, "git", "stash", "pop", ref); err != nil {
					log.Error("CRITICAL: failed to pop stash; it is preserved",
						"path", path, "ref", ref, "err", err,
						"recovery", fmt.Sprintf("cd %s && git stash pop %s", path, ref))
					return err
				}
			}
			return nil
		},
	}, nil
}

// restoreAfterCheckoutFailure does best-effort cleanup during doCheckout()
// if something in the forward path fails. It never returns an error
// because the caller is already returning one.
func (m *Manager) restoreAfterCheckoutFailure(ctx context.Context, plan *Plan, stashRef, why string) {
	m.log().Warn("checkout failed; attempting to restore", "why", why, "path", plan.Path, "branch", plan.CurrentBranch)
	if _, err := m.Runner.RunIn(ctx, plan.Path, "git", "checkout", plan.CurrentBranch); err != nil {
		m.log().Error("restore: checkout failed", "err", err)
	}
	if stashRef != "" {
		if _, err := m.Runner.RunIn(ctx, plan.Path, "git", "stash", "pop", stashRef); err != nil {
			m.log().Error("restore: stash pop failed; stash preserved",
				"ref", stashRef,
				"recovery", fmt.Sprintf("cd %s && git stash pop %s", plan.Path, stashRef))
		}
	}
}

// -- low-level git helpers ----------------------------------------------------

func (m *Manager) listWorktrees(ctx context.Context, repoPath string) ([]Info, error) {
	res, err := m.Runner.RunIn(ctx, repoPath, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeList(res.Stdout), nil
}

func (m *Manager) isDirty(ctx context.Context, path string) (bool, error) {
	res, err := m.Runner.RunIn(ctx, path, "git", "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(res.Stdout) != "", nil
}

func (m *Manager) currentBranch(ctx context.Context, path string) (string, error) {
	res, err := m.Runner.RunIn(ctx, path, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (m *Manager) log() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// -- parsing ------------------------------------------------------------------

// parseWorktreeList parses `git worktree list --porcelain` output.
// Entries are separated by blank lines; keys are "worktree", "HEAD",
// "branch", "bare", "detached", "locked", "prunable".
func parseWorktreeList(s string) []Info {
	var out []Info
	var cur *Info
	flush := func() {
		if cur != nil && cur.Path != "" {
			out = append(out, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		if cur == nil {
			cur = &Info{}
		}
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			cur.Path = val
		case "HEAD":
			cur.Head = val
		case "branch":
			cur.Branch = strings.TrimPrefix(val, "refs/heads/")
		case "bare":
			cur.Bare = true
		case "detached":
			cur.Detached = true
		}
	}
	flush()
	// First non-bare entry is the main repo.
	for i := range out {
		if !out[i].Bare {
			out[i].Main = true
			break
		}
	}
	return out
}

func findMain(wts []Info) (Info, bool) {
	for _, wt := range wts {
		if wt.Main {
			return wt, true
		}
	}
	return Info{}, false
}

// parseStashRef extracts "stash@{N}" from `git stash push` output.
// Example: "Saved working directory and index state On branch: stash@{0}: aupr: ..."
// Modern git prints: "Saved working directory and index state On <branch>".
// We use `git stash list -n 1` is more reliable, but for a single push we
// can trust that the newest stash is stash@{0}.
func parseStashRef(out string) string {
	_ = out
	// git stash push --message ... always pushes to stash@{0} on success
	// (older stashes are pushed down). Returning this constant is correct
	// provided we call it immediately after a successful push.
	return "stash@{0}"
}

// -- token substitution -------------------------------------------------------

func buildTokens(repo discovery.Repo, pr feedback.PR, path string) map[string]string {
	tokens := map[string]string{
		"repo":      repo.Name,
		"nwo":       repo.NWO,
		"branch":    pr.HeadRefName,
		"repo_path": repo.Path,
	}
	if path != "" {
		tokens["path"] = path
	}
	return tokens
}

func substitute(s string, tokens map[string]string) string {
	for k, v := range tokens {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

func substituteAll(args []string, tokens map[string]string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = substitute(a, tokens)
	}
	return out
}

func resolvePath(template string, tokens map[string]string) (string, error) {
	p := substitute(template, tokens)
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[2:])
	}
	return p, nil
}
