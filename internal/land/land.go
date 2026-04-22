// Package land implements "landing the plane": after the agent makes
// its changes, aupr validates, pulls, pushes, and posts a reply on the
// originating comment thread.
package land

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/feedback"
)

// Options tunes Land.
type Options struct {
	QualityGates []string // argv-lines (one string per gate, parsed on whitespace)
	DryRun       bool     // if true, logs what would run and skips mutations
	// CommentOnPR: if true, post a reply comment after a successful push.
	CommentOnPR bool
}

// Result summarizes what landing accomplished.
type Result struct {
	Pushed       bool
	CommitSHA    string
	GatesRan     []string
	Commented    bool
	CommentURL   string
	QualityError string
}

// Lander executes the landing sequence.
type Lander struct {
	Runner execx.Runner
	Logger *slog.Logger
}

// Land runs the full sequence on the given workspace for the given PR.
// It returns an error on any failure. The workspace is NOT cleaned up on
// failure — the caller (scheduler) decides whether to release the lease.
func (l *Lander) Land(ctx context.Context, workspace string, pr feedback.PR, opts Options, summary string) (*Result, error) {
	res := &Result{}

	// 1. Quality gates — best-effort; we log output but don't hard-fail
	//    in M3 unless the gate's exit code is non-zero.
	gates := opts.QualityGates
	if len(gates) == 0 {
		gates = DetectQualityGates(workspace)
	}
	for _, gate := range gates {
		parts := strings.Fields(gate)
		if len(parts) == 0 {
			continue
		}
		res.GatesRan = append(res.GatesRan, gate)
		l.log().Info("land: quality gate", "cmd", gate, "workspace", workspace, "dry_run", opts.DryRun)
		if opts.DryRun {
			continue
		}
		if _, err := l.Runner.RunIn(ctx, workspace, parts[0], parts[1:]...); err != nil {
			res.QualityError = err.Error()
			return res, fmt.Errorf("quality gate %q failed: %w", gate, err)
		}
	}

	// 2. Ensure there's a new commit to push.
	headBefore, err := l.headSHA(ctx, workspace)
	if err != nil {
		return res, fmt.Errorf("read head: %w", err)
	}
	remoteHead, err := l.remoteHeadSHA(ctx, workspace, pr.HeadRefName)
	if err != nil {
		l.log().Warn("land: could not read remote head, continuing", "err", err)
	}
	if headBefore == remoteHead {
		return res, fmt.Errorf("land: no new commits to push (head %s matches origin/%s)",
			short(headBefore), pr.HeadRefName)
	}

	// 3. Pull --rebase to integrate any upstream advances.
	l.log().Info("land: pull --rebase", "branch", pr.HeadRefName, "workspace", workspace, "dry_run", opts.DryRun)
	if !opts.DryRun {
		if _, err := l.Runner.RunIn(ctx, workspace, "git", "pull", "--rebase", "origin", pr.HeadRefName); err != nil {
			// abort in-progress rebase if one is hanging
			_, _ = l.Runner.RunIn(ctx, workspace, "git", "rebase", "--abort")
			return res, fmt.Errorf("pull --rebase: %w", err)
		}
	}

	// 4. Push.
	l.log().Info("land: push", "branch", pr.HeadRefName, "workspace", workspace, "dry_run", opts.DryRun)
	if !opts.DryRun {
		if _, err := l.Runner.RunIn(ctx, workspace, "git", "push", "origin", pr.HeadRefName); err != nil {
			return res, fmt.Errorf("push: %w", err)
		}
	}

	// 5. Record the pushed SHA.
	headAfter, err := l.headSHA(ctx, workspace)
	if err == nil {
		res.CommitSHA = headAfter
	}
	res.Pushed = !opts.DryRun

	// 6. Optional: reply on the PR.
	if opts.CommentOnPR {
		body := fmt.Sprintf("aupr addressed the above in `%s`: %s", short(res.CommitSHA), summary)
		l.log().Info("land: comment on PR", "repo", pr.Repo, "pr", pr.Number, "dry_run", opts.DryRun)
		if !opts.DryRun {
			if _, err := l.Runner.Run(ctx, "gh", "pr", "comment",
				fmt.Sprint(pr.Number), "--repo", pr.Repo, "--body", body); err != nil {
				l.log().Warn("land: PR comment failed (non-critical)", "err", err)
			} else {
				res.Commented = true
			}
		}
	}
	return res, nil
}

func (l *Lander) headSHA(ctx context.Context, workspace string) (string, error) {
	res, err := l.Runner.RunIn(ctx, workspace, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (l *Lander) remoteHeadSHA(ctx context.Context, workspace, branch string) (string, error) {
	res, err := l.Runner.RunIn(ctx, workspace, "git", "rev-parse", "origin/"+branch)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (l *Lander) log() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

// DetectQualityGates inspects the workspace for common build-system
// signals and returns a sensible default command list. Returns nil if
// nothing obvious is present; callers can override via Options.
func DetectQualityGates(workspace string) []string {
	// Order matters: justfile beats Makefile beats package.json.
	if exists(filepath.Join(workspace, "justfile")) || exists(filepath.Join(workspace, ".justfile")) {
		return []string{"just check"}
	}
	if exists(filepath.Join(workspace, "Makefile")) {
		return []string{"make test"}
	}
	if exists(filepath.Join(workspace, "pyproject.toml")) {
		return []string{"uv run pytest -x"}
	}
	if exists(filepath.Join(workspace, "go.mod")) {
		return []string{"go test ./..."}
	}
	if exists(filepath.Join(workspace, "package.json")) {
		return []string{"npm test"}
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
