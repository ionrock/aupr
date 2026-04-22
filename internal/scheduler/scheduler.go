// Package scheduler orchestrates one tick of the aupr loop.
//
// The tick:
//  1. Walk roots to find repos.
//  2. Enumerate open authored PRs via gh.
//  3. For each PR: enrich, fetch feedback, classify.
//  4. For non-skip PRs: plan workspace.
//  5. For AUTO PRs: act (acquire workspace → invoke agent → land) —
//     gated behind --dry-run (which stops before any mutation).
//  6. Render the decision table.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ionrock/aupr/internal/agent"
	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/digest"
	"github.com/ionrock/aupr/internal/discovery"
	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/feedback"
	"github.com/ionrock/aupr/internal/land"
	"github.com/ionrock/aupr/internal/notify"
	"github.com/ionrock/aupr/internal/policy"
	"github.com/ionrock/aupr/internal/recovery"
	"github.com/ionrock/aupr/internal/state"
	"github.com/ionrock/aupr/internal/worktree"
)

// Options tunes a single run.
type Options struct {
	DryRun bool
	Output io.Writer // defaults to os.Stdout

	// OnlyPR, if non-zero, restricts this tick to a single PR. Used by
	// `aupr test <pr>`.
	OnlyRepo string
	OnlyPR   int

	// Interactive makes worktree-mode=checkout use stdin prompting. For
	// `aupr run` (daemon) this is false and checkout-mode is declined.
	Interactive bool
}

// Scheduler is the top-level coordinator.
type Scheduler struct {
	cfg      *config.Config
	logger   *slog.Logger
	runner   execx.Runner
	state    state.Store
	notifier notify.Notifier
	agents   *agent.Registry
}

// New returns a Scheduler ready to RunOnce. The caller owns state.Close().
func New(cfg *config.Config, logger *slog.Logger, st state.Store) *Scheduler {
	runner := &execx.OS{Logger: logger}
	return &Scheduler{
		cfg:      cfg,
		logger:   logger,
		runner:   runner,
		state:    st,
		notifier: notify.FromConfig(cfg.Notify, logger, runner),
		agents: &agent.Registry{
			Runner:        runner,
			Logger:        logger,
			CommandConfig: cfg.Agent.Command,
		},
	}
}

// RunOnce performs a single discovery+decision+act cycle.
func (s *Scheduler) RunOnce(ctx context.Context, opts Options) error {
	if opts.Output == nil {
		opts.Output = os.Stdout
	}
	paused, pauseReason, _ := s.state.IsPaused(ctx)
	s.logger.Info("aupr tick: starting",
		"dry_run", opts.DryRun, "only_pr", opts.OnlyPR, "paused", paused)
	if opts.DryRun {
		fmt.Fprintln(opts.Output, "[dry-run] no mutations will be performed")
	}
	if paused {
		fmt.Fprintf(opts.Output, "[paused] act-loop suspended (%s); polling continues\n", pauseReason)
	}

	// Daily digest: run before discovery so errors there don't hide the digest.
	if err := s.maybeSendDigest(ctx); err != nil {
		s.logger.Warn("digest send failed", "err", err)
	}

	// 1. Discovery.
	w := &discovery.Walker{Runner: s.runner, Logger: s.logger}
	repos, err := w.Walk(ctx, s.cfg.Daemon.Roots)
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	s.logger.Info("discovered repos", "count", len(repos))

	allowed := make(map[string]struct{}, len(repos))
	repoByNWO := make(map[string]discovery.Repo, len(repos))
	for _, r := range repos {
		allowed[r.NWO] = struct{}{}
		repoByNWO[r.NWO] = r
	}

	// Recovery scan: look for orphaned aupr stashes from any interrupted
	// checkout-mode protocol.
	scanner := &recovery.Scanner{Runner: s.runner, Logger: s.logger, Store: s.state, Notifier: s.notifier}
	if _, err := scanner.Scan(ctx, repos); err != nil {
		s.logger.Warn("recovery scan", "err", err)
	}

	// 2. Enumerate PRs via gh.
	client := &feedback.Client{Runner: s.runner, User: s.cfg.Daemon.GithubUser}
	prs, err := client.ListAuthoredOpenPRs(ctx, allowed)
	if err != nil {
		return fmt.Errorf("list prs: %w", err)
	}
	s.logger.Info("found open authored PRs", "count", len(prs))

	// Filter for OnlyPR.
	if opts.OnlyPR > 0 {
		filtered := prs[:0]
		for _, pr := range prs {
			if pr.Number == opts.OnlyPR && (opts.OnlyRepo == "" || pr.Repo == opts.OnlyRepo) {
				filtered = append(filtered, pr)
			}
		}
		prs = filtered
		if len(prs) == 0 {
			return fmt.Errorf("no matching PR for repo=%q number=%d", opts.OnlyRepo, opts.OnlyPR)
		}
	}

	// 3. Enrich + classify + plan + (maybe) act.
	engine := &policy.Engine{Cfg: s.cfg, User: s.cfg.Daemon.GithubUser}
	var prompter worktree.Prompter = worktree.DenyPrompter{}
	if opts.Interactive {
		prompter = worktree.StdinPrompter{}
	}
	wtMgr := &worktree.Manager{Cfg: &s.cfg.Worktree, Runner: s.runner, Logger: s.logger, Prompt: prompter}

	type row struct {
		decision   policy.Decision
		plan       *worktree.Plan
		planErr    error
		actOutcome string // filled when action was attempted
	}
	var rows []row
	for i := range prs {
		if err := ctx.Err(); err != nil {
			return err
		}
		pr := &prs[i]

		// Config-level skip list and persistent skip list.
		if skipped, reason, _ := s.state.IsSkipped(ctx, pr.Repo, pr.Number); skipped {
			rows = append(rows, row{decision: policy.Decision{
				PR: *pr, Action: policy.ActSkip, Reason: "persistent skip: " + reason,
			}})
			continue
		}

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
		cursor, _ := s.state.LastSeen(ctx, pr.Repo, pr.Number)
		d := engine.Classify(*pr, events, cursor)

		r := row{decision: d}
		if d.Action != policy.ActSkip {
			repo, ok := repoByNWO[pr.Repo]
			if !ok {
				r.planErr = errors.New("repo not in configured roots")
			} else if pr.HeadRefName == "" {
				r.planErr = errors.New("empty HeadRefName")
			} else {
				plan, err := wtMgr.Plan(ctx, repo, *pr)
				if err != nil {
					s.logger.Warn("worktree plan failed", "repo", pr.Repo, "pr", pr.Number, "err", err)
					r.planErr = err
				} else {
					r.plan = plan
				}
			}
		}

		// Action: only AUTO, only when we have a viable plan, and only
		// when not paused. The one-shot interactive path is `aupr test`,
		// which sets OnlyPR and uses dry-run by default; `aupr run` sets
		// !DryRun to actually push.
		if d.Action == policy.ActAuto && r.plan != nil && r.planErr == nil {
			if paused {
				r.actOutcome = "paused"
			} else {
				outcome := s.act(ctx, repoByNWO[pr.Repo], *pr, d, r.plan, wtMgr, opts)
				r.actOutcome = outcome
			}
		}

		rows = append(rows, r)
	}

	// 4. Render.
	decisions := make([]policy.Decision, len(rows))
	plans := make([]*worktree.Plan, len(rows))
	planErrs := make([]error, len(rows))
	outcomes := make([]string, len(rows))
	for i, r := range rows {
		decisions[i] = r.decision
		plans[i] = r.plan
		planErrs[i] = r.planErr
		outcomes[i] = r.actOutcome
	}
	renderTable(opts.Output, decisions, plans, planErrs, outcomes)
	s.logger.Info("aupr tick: done", "decisions", len(decisions))
	return nil
}

// act runs the full acquire→invoke→land pipeline for one PR.
// Returns a short string describing the outcome for the table.
func (s *Scheduler) act(
	ctx context.Context,
	repo discovery.Repo,
	pr feedback.PR,
	d policy.Decision,
	plan *worktree.Plan,
	wtMgr *worktree.Manager,
	opts Options,
) string {
	// Circuit breaker: if the last 3 attempts on this PR all failed, skip.
	recent, _ := s.state.RecentAttempts(ctx, pr.Repo, pr.Number, 3)
	if len(recent) >= 3 && allFailed(recent) {
		s.logger.Warn("circuit breaker: 3 consecutive failures, auto-skipping",
			"repo", pr.Repo, "pr", pr.Number)
		_ = s.state.Skip(ctx, pr.Repo, pr.Number, "circuit breaker: 3 consecutive failures")
		_ = s.notifier.Notify(ctx, notify.Event{
			Kind: "circuit-breaker", Repo: pr.Repo, PR: pr.Number, URL: pr.URL,
			Summary: "auto-skipped after 3 consecutive failures",
		})
		return "circuit-break"
	}

	// Pick agent. Per-repo override may swap the agent AND/OR the
	// command-backend argv for the "command" agent.
	agentName := s.cfg.Agent.Default
	commandCfg := s.cfg.Agent.Command
	if ov, ok := s.cfg.Repos[pr.Repo]; ok {
		if ov.Agent != "" {
			agentName = ov.Agent
		}
		if len(ov.AgentCommand.Argv) > 0 {
			// Per-repo command overrides the global one wholesale.
			commandCfg = ov.AgentCommand
		}
	}
	// Give the registry the right command config for this repo's invocation.
	s.agents.CommandConfig = commandCfg
	ag, err := s.agents.Get(agentName)
	if err != nil {
		s.logger.Error("agent registry", "err", err)
		return "agent-err"
	}

	lastEventID := d.NewEvents[len(d.NewEvents)-1].ID
	started := time.Now()
	attempt := state.Attempt{
		Repo: pr.Repo, PRNumber: pr.Number, EventID: lastEventID,
		StartedAt: started, Agent: agentName,
	}

	// Acquire workspace.
	lease, err := wtMgr.Acquire(ctx, plan)
	if err != nil {
		attempt.Outcome, attempt.Error = "error", "acquire: "+err.Error()
		attempt.FinishedAt = time.Now()
		_ = s.state.RecordAttempt(ctx, attempt)
		if errors.Is(err, worktree.ErrSkip) || errors.Is(err, worktree.ErrUserDeclined) {
			return "wt-" + shortErr(err)
		}
		s.logger.Warn("acquire failed", "repo", pr.Repo, "pr", pr.Number, "err", err)
		return "wt-err"
	}
	defer func() {
		if rerr := lease.Release(ctx); rerr != nil {
			s.logger.Error("lease release failed", "err", rerr)
		}
	}()

	// Load any persisted session for this PR+agent.
	var sessionID string
	if sess, ok, _ := s.state.LoadSession(ctx, pr.Repo, pr.Number, agentName); ok {
		sessionID = sess.SessionID
	}

	// Build AUTO-only classifications. An agent shouldn't see FLAG events.
	var auto []policy.EventClass
	for _, c := range d.Classifications {
		if c.Action == policy.ActAuto {
			auto = append(auto, c)
		}
	}
	if len(auto) == 0 {
		attempt.Outcome = "skipped"
		attempt.Summary = "no AUTO events in classification"
		attempt.FinishedAt = time.Now()
		_ = s.state.RecordAttempt(ctx, attempt)
		return "no-auto"
	}

	req := agent.Request{
		Workspace: lease.Path, PR: pr, Classifications: auto,
		SessionID: sessionID, MaxTurns: s.cfg.Agent.MaxTurnsPerFeedback,
		DryRun: opts.DryRun,
	}
	resp, err := ag.Invoke(ctx, req)
	if err != nil {
		attempt.Outcome, attempt.Error = "error", "agent: "+err.Error()
		attempt.FinishedAt = time.Now()
		_ = s.state.RecordAttempt(ctx, attempt)
		s.logger.Warn("agent invoke failed", "repo", pr.Repo, "pr", pr.Number, "err", err)
		return "agent-err"
	}
	if resp.SessionID != "" {
		_ = s.state.SaveSession(ctx, state.Session{
			Repo: pr.Repo, PRNumber: pr.Number, Agent: agentName,
			SessionID: resp.SessionID, LastUsedAt: time.Now(),
		})
	}

	// Land.
	gates := []string(nil)
	if ov, ok := s.cfg.Repos[pr.Repo]; ok {
		gates = ov.QualityGates
	}
	lander := &land.Lander{Runner: s.runner, Logger: s.logger}
	lres, lerr := lander.Land(ctx, lease.Path, pr, land.Options{
		QualityGates: gates,
		DryRun:       opts.DryRun,
		CommentOnPR:  !opts.DryRun,
	}, resp.Summary)
	if lerr != nil {
		attempt.Outcome, attempt.Error = "error", "land: "+lerr.Error()
		attempt.FinishedAt = time.Now()
		_ = s.state.RecordAttempt(ctx, attempt)
		s.logger.Warn("land failed", "repo", pr.Repo, "pr", pr.Number, "err", lerr)
		return "land-err"
	}

	attempt.Outcome = "success"
	if opts.DryRun {
		attempt.Outcome = "dry-run"
	}
	attempt.Summary = resp.Summary
	attempt.CommitSHA = lres.CommitSHA
	attempt.InputTokens = resp.InputTokens
	attempt.OutputTokens = resp.OutputTokens
	attempt.CostUSD = resp.CostUSD
	attempt.FinishedAt = time.Now()
	_ = s.state.RecordAttempt(ctx, attempt)

	// Only advance the cursor on real (non-dry-run) success.
	if !opts.DryRun {
		_ = s.state.RecordSeen(ctx, pr.Repo, pr.Number, lastEventID)
	}

	// summary_cadence gates success notifications: per_action fires
	// every time, daily/never suppresses (digest picks them up).
	if s.cfg.Notify.SummaryCadence == "per_action" || s.cfg.Notify.SummaryCadence == "" {
		_ = s.notifier.Notify(ctx, notify.Event{
			Kind: "acted", Repo: pr.Repo, PR: pr.Number, URL: pr.URL,
			Summary: resp.Summary, Detail: lres.CommitSHA,
		})
	}
	if opts.DryRun {
		return "dry-run-ok"
	}
	return "acted"
}

// maybeSendDigest fires a digest to the notifier iff cadence="daily"
// and >= 24h have passed since the last digest (tracked in
// daemon_settings).
func (s *Scheduler) maybeSendDigest(ctx context.Context) error {
	if s.cfg.Notify.SummaryCadence != "daily" {
		return nil
	}
	now := time.Now()
	lastS, _, _ := s.state.GetSetting(ctx, "last_digest_at")
	last := int64(0)
	if lastS != "" {
		last, _ = strconv.ParseInt(lastS, 10, 64)
	}
	if last != 0 && now.Unix()-last < 24*60*60 {
		return nil
	}
	since := now.Add(-24 * time.Hour)
	attempts, _ := s.state.AttemptsSince(ctx, since)
	skips, _ := s.state.ListSkipped(ctx)
	stashes, _ := s.state.ListRecoveryStashes(ctx)
	summary := digest.Build(since, now, attempts, skips, stashes)
	if summary.Empty() {
		_ = s.state.SetSetting(ctx, "last_digest_at", strconv.FormatInt(now.Unix(), 10))
		return nil
	}
	body := summary.Format()
	s.logger.Info("sending daily digest", "attempts", len(attempts), "errors", summary.ErrorCount)
	_ = s.notifier.Notify(ctx, notify.Event{
		Kind: "digest", Summary: "daily digest", Detail: body,
	})
	return s.state.SetSetting(ctx, "last_digest_at", strconv.FormatInt(now.Unix(), 10))
}

func allFailed(attempts []state.Attempt) bool {
	for _, a := range attempts {
		if a.Outcome == "success" {
			return false
		}
	}
	return true
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, ":"); i > 0 {
		return s[:i]
	}
	if len(s) > 20 {
		return s[:20]
	}
	return s
}

// --- rendering ---------------------------------------------------------------

func renderTable(
	w io.Writer,
	decisions []policy.Decision,
	plans []*worktree.Plan,
	planErrs []error,
	outcomes []string,
) {
	if len(decisions) == 0 {
		fmt.Fprintln(w, "no authored open PRs found")
		return
	}
	type pair struct {
		d       policy.Decision
		p       *worktree.Plan
		planErr error
		outcome string
	}
	pairs := make([]pair, len(decisions))
	for i := range decisions {
		pairs[i] = pair{d: decisions[i], p: plans[i], planErr: planErrs[i], outcome: outcomes[i]}
	}
	sort.Slice(pairs, func(i, j int) bool {
		ri, rj := rankAction(pairs[i].d.Action), rankAction(pairs[j].d.Action)
		if ri != rj {
			return ri < rj
		}
		if pairs[i].d.PR.Repo != pairs[j].d.PR.Repo {
			return pairs[i].d.PR.Repo < pairs[j].d.PR.Repo
		}
		return pairs[i].d.PR.Number < pairs[j].d.PR.Number
	})

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tREPO\tPR\tNEW\tTITLE\tWORKSPACE\tOUTCOME\tREASON")
	for _, p := range pairs {
		d := p.d
		title := truncate(d.PR.Title, 50)
		reason := truncate(d.Reason, 70)
		ws := summarizePlan(p.p, p.planErr, d.Action == policy.ActSkip)
		outcome := p.outcome
		if outcome == "" {
			outcome = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t#%d\t%d\t%s\t%s\t%s\t%s\n",
			d.Action, d.PR.Repo, d.PR.Number, len(d.NewEvents), title, ws, outcome, reason)
	}
	_ = tw.Flush()

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

func summarizePlan(p *worktree.Plan, err error, skipped bool) string {
	if skipped {
		return "—"
	}
	if err != nil {
		return "ERR:" + truncate(err.Error(), 30)
	}
	if p == nil {
		return "?"
	}
	switch p.Action {
	case worktree.ActionUseExisting:
		return "existing " + truncate(p.Path, 40)
	case worktree.ActionUseMain:
		return "main (on target)"
	case worktree.ActionCreate:
		return "create " + truncate(p.Path, 40)
	case worktree.ActionCheckout:
		if p.Dirty {
			return "checkout (dirty; would stash)"
		}
		return "checkout"
	case worktree.ActionSkip:
		return "skip: " + truncate(p.Reason, 30)
	}
	return string(p.Action)
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
