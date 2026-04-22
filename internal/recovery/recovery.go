// Package recovery scans git repos for aupr-authored stashes left over
// from an interrupted checkout-mode protocol (SIGKILL, crash, power loss).
//
// aupr writes stashes with a known message prefix:
//
//	"aupr: auto-stash <original>-><target>"
//
// If the process dies between stash-push and stash-pop, that stash is
// orphaned. This package finds those orphans so the user can recover
// their work.
package recovery

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/ionrock/aupr/internal/discovery"
	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/notify"
	"github.com/ionrock/aupr/internal/state"
)

// StashPrefix is the substring aupr writes into every managed stash.
const StashPrefix = "aupr: auto-stash"

// Stash is one orphaned stash entry.
type Stash struct {
	RepoPath string
	Ref      string // "stash@{N}"
	Message  string
}

// Scanner coordinates listing stashes, deduplicating against the state
// DB, and notifying on first sight.
type Scanner struct {
	Runner   execx.Runner
	Logger   *slog.Logger
	Store    state.Store
	Notifier notify.Notifier
}

// Scan walks the given repos, enumerates aupr-authored stashes, and
// notifies for any newly-seen ones. Stashes that disappeared since the
// last scan are forgotten from the tracking table.
func (s *Scanner) Scan(ctx context.Context, repos []discovery.Repo) ([]Stash, error) {
	var all []Stash
	for _, repo := range repos {
		if ctx.Err() != nil {
			return all, ctx.Err()
		}
		stashes, err := s.listStashes(ctx, repo.Path)
		if err != nil {
			s.log().Debug("stash list failed; skipping", "repo", repo.Path, "err", err)
			continue
		}

		keep := make([]string, 0, len(stashes))
		for _, st := range stashes {
			keep = append(keep, st.Ref)
			first, err := s.Store.SeenRecoveryStash(ctx, repo.Path, st.Ref, st.Message)
			if err != nil {
				s.log().Warn("recovery store error", "err", err)
				continue
			}
			all = append(all, st)
			if first && s.Notifier != nil {
				_ = s.Notifier.Notify(ctx, notify.Event{
					Kind:    "recovery-stash",
					Repo:    repo.NWO,
					Summary: "aupr stash left behind: " + st.Ref,
					Detail:  "cd " + repo.Path + " && git stash pop " + st.Ref,
					URL:     "", // no URL for stash events
				})
			}
		}
		// Drop tracking rows for stashes that no longer exist (user popped or dropped them).
		if err := s.Store.ForgetRecoveryStashes(ctx, repo.Path, keep); err != nil {
			s.log().Warn("forget recovery stashes", "err", err)
		}
	}
	return all, nil
}

// stashLine matches lines from `git stash list` like:
//
//	stash@{0}: On branch: aupr: auto-stash main->eric/foo
var stashLine = regexp.MustCompile(`^(stash@\{[0-9]+\}):\s*(.*)$`)

func (s *Scanner) listStashes(ctx context.Context, repoPath string) ([]Stash, error) {
	res, err := s.Runner.RunIn(ctx, repoPath, "git", "stash", "list")
	if err != nil {
		return nil, err
	}
	var out []Stash
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := stashLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if !strings.Contains(m[2], StashPrefix) {
			continue
		}
		out = append(out, Stash{
			RepoPath: repoPath,
			Ref:      m[1],
			Message:  m[2],
		})
	}
	return out, nil
}

func (s *Scanner) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
