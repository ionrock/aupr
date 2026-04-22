// Package scheduler orchestrates one tick of the aupr loop.
//
// M1 responsibility: walk roots → enumerate authored open PRs → fetch events →
// classify → print a decision table. No mutations.
package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/discovery"
	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/feedback"
	"github.com/ionrock/aupr/internal/policy"
	"github.com/ionrock/aupr/internal/state"
)

// Options tunes a single run.
type Options struct {
	DryRun bool
	Output io.Writer // defaults to os.Stdout
}

// Scheduler is the top-level coordinator.
type Scheduler struct {
	cfg    *config.Config
	logger *slog.Logger
	runner execx.Runner
	state  state.Store
}

// New returns a Scheduler ready to RunOnce.
func New(cfg *config.Config, logger *slog.Logger) *Scheduler {
	runner := &execx.OS{Logger: logger}
	return &Scheduler{
		cfg:    cfg,
		logger: logger,
		runner: runner,
		state:  state.NewMemory(),
	}
}

// RunOnce performs a single discovery+decision cycle and prints the table.
func (s *Scheduler) RunOnce(ctx context.Context, opts Options) error {
	if opts.Output == nil {
		opts.Output = os.Stdout
	}
	s.logger.Info("aupr tick: starting", "dry_run", opts.DryRun)
	if opts.DryRun {
		fmt.Fprintln(opts.Output, "[dry-run] no mutations will be performed")
	}

	// 1. Discovery.
	w := &discovery.Walker{Runner: s.runner, Logger: s.logger}
	repos, err := w.Walk(ctx, s.cfg.Daemon.Roots)
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	s.logger.Info("discovered repos", "count", len(repos))

	allowed := make(map[string]struct{}, len(repos))
	for _, r := range repos {
		allowed[r.NWO] = struct{}{}
	}

	// 2. Enumerate PRs via gh.
	client := &feedback.Client{Runner: s.runner, User: s.cfg.Daemon.GithubUser}
	prs, err := client.ListAuthoredOpenPRs(ctx, allowed)
	if err != nil {
		return fmt.Errorf("list prs: %w", err)
	}
	s.logger.Info("found open authored PRs", "count", len(prs))

	// 3. Enrich + classify.
	engine := &policy.Engine{Cfg: s.cfg, User: s.cfg.Daemon.GithubUser}
	var decisions []policy.Decision
	for i := range prs {
		if err := ctx.Err(); err != nil {
			return err
		}
		pr := &prs[i]
		if err := client.EnrichPR(ctx, pr); err != nil {
			s.logger.Warn("enrich pr failed", "repo", pr.Repo, "pr", pr.Number, "err", err)
			continue
		}
		events, err := client.FetchEvents(ctx, *pr)
		if err != nil {
			s.logger.Warn("fetch events failed", "repo", pr.Repo, "pr", pr.Number, "err", err)
			continue
		}
		sort.Slice(events, func(i, j int) bool { return events[i].CreatedAt.Before(events[j].CreatedAt) })
		cursor, _ := s.state.LastSeen(pr.Repo, pr.Number)
		d := engine.Classify(*pr, events, cursor)
		decisions = append(decisions, d)
	}

	// 4. Render.
	renderTable(opts.Output, decisions)
	s.logger.Info("aupr tick: done", "decisions", len(decisions))
	return nil
}

func renderTable(w io.Writer, decisions []policy.Decision) {
	if len(decisions) == 0 {
		fmt.Fprintln(w, "no authored open PRs found")
		return
	}
	// Stable sort: SKIP at the bottom, AUTO on top, then by repo/number.
	sort.Slice(decisions, func(i, j int) bool {
		ri, rj := rankAction(decisions[i].Action), rankAction(decisions[j].Action)
		if ri != rj {
			return ri < rj
		}
		if decisions[i].PR.Repo != decisions[j].PR.Repo {
			return decisions[i].PR.Repo < decisions[j].PR.Repo
		}
		return decisions[i].PR.Number < decisions[j].PR.Number
	})

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tREPO\tPR\tNEW\tTITLE\tREASON")
	for _, d := range decisions {
		title := truncate(d.PR.Title, 50)
		reason := truncate(d.Reason, 80)
		fmt.Fprintf(tw, "%s\t%s\t#%d\t%d\t%s\t%s\n",
			d.Action, d.PR.Repo, d.PR.Number, len(d.NewEvents), title, reason)
	}
	_ = tw.Flush()

	// Summary.
	var auto, flag, skip int
	for _, d := range decisions {
		switch d.Action {
		case policy.ActAuto:
			auto++
		case policy.ActFlag:
			flag++
		case policy.ActSkip:
			skip++
		}
	}
	fmt.Fprintf(w, "\n%d PR(s): %d AUTO, %d FLAG, %d SKIP\n", len(decisions), auto, flag, skip)
}

func rankAction(a policy.Action) int {
	switch a {
	case policy.ActAuto:
		return 0
	case policy.ActFlag:
		return 1
	case policy.ActSkip:
		return 2
	}
	return 3
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
