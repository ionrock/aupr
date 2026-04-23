// Package inspect runs the aupr pipeline against any PR in read-only
// mode: fetch the PR head, acquire a workspace, invoke the agent, and
// show the diff. It never writes state, never pushes, never comments.
//
// This is the iteration-loop tool: you use it to see what the agent
// produces for a given set of reviewer comments, tweak the prompt or
// the command backend, and repeat until you like the output. When
// you're satisfied, enable the normal `aupr test --dry-run=false` or
// daemon path to actually land changes.
package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/ionrock/aupr/internal/agent"
	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/discovery"
	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/feedback"
	"github.com/ionrock/aupr/internal/policy"
	"github.com/ionrock/aupr/internal/worktree"
)

// Options tunes one inspection run.
type Options struct {
	Repo          string // "owner/name"
	PRNumber      int
	Reset         bool   // git reset --hard to PR head before running agent
	ForceClassify bool   // run even if classification is FLAG/SKIP, suppress warning
	User          string // logged-in user (for policy classification display only)
	Output        io.Writer
}

// Inspector runs the pipeline.
type Inspector struct {
	Cfg    *config.Config
	Runner execx.Runner
	Logger *slog.Logger
}

// Run executes the inspection. Returns the workspace path on success
// so CLI callers can print it.
func (i *Inspector) Run(ctx context.Context, opts Options) (workspacePath string, err error) {
	if opts.Output == nil {
		return "", errors.New("inspect: Options.Output must not be nil")
	}
	out := opts.Output
	logger := i.log()

	fmt.Fprintf(out, "aupr inspect %s#%d\n\n", opts.Repo, opts.PRNumber)

	// 1. Locate the repo on disk.
	walker := &discovery.Walker{Runner: i.Runner, Logger: logger}
	repos, err := walker.Walk(ctx, i.Cfg.Daemon.Roots)
	if err != nil {
		return "", fmt.Errorf("discovery: %w", err)
	}
	var repo discovery.Repo
	var found bool
	for _, r := range repos {
		if r.NWO == opts.Repo {
			repo, found = r, true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("repo %q not found under configured roots (%v); clone it or add its parent to [daemon] roots",
			opts.Repo, i.Cfg.Daemon.Roots)
	}
	fmt.Fprintf(out, "Repo path:  %s\n", repo.Path)

	// 2. Fetch PR metadata (no author filter — this is the key difference from `aupr test`).
	client := &feedback.Client{Runner: i.Runner, User: opts.User}
	pr, err := fetchPR(ctx, i.Runner, opts.Repo, opts.PRNumber)
	if err != nil {
		return "", fmt.Errorf("fetch PR: %w", err)
	}
	fmt.Fprintf(out, "PR:         #%d — %s\n", pr.Number, pr.Title)
	fmt.Fprintf(out, "Author:     %s (you are %s)\n", pr.Author, opts.User)
	fmt.Fprintf(out, "Branch:     %s\n\n", pr.HeadRefName)

	// 3. Fetch events + classify (for display).
	events, err := client.FetchEvents(ctx, pr)
	if err != nil {
		return "", fmt.Errorf("fetch events: %w", err)
	}
	sort.Slice(events, func(a, b int) bool { return events[a].CreatedAt.Before(events[b].CreatedAt) })
	engine := &policy.Engine{Cfg: i.Cfg, User: opts.User}
	decision := engine.Classify(pr, events, "")
	renderClassification(out, decision)

	// 4. Decide whether to proceed given classification.
	if decision.Action != policy.ActAuto {
		if !opts.ForceClassify {
			fmt.Fprintf(out, "\n⚠  aupr would normally %s this PR (not act on it).\n", decision.Action)
			fmt.Fprint(out, "   Proceeding anyway because `aupr inspect` is read-only.\n\n")
		}
	}

	// Filter to AUTO-classified events only for the agent, matching the
	// scheduler's behavior. Fall back to all events if nothing classified
	// AUTO so iteration works on FLAG PRs too.
	var autoClassifications []policy.EventClass
	for _, c := range decision.Classifications {
		if c.Action == policy.ActAuto {
			autoClassifications = append(autoClassifications, c)
		}
	}
	if len(autoClassifications) == 0 {
		autoClassifications = decision.Classifications
		if len(autoClassifications) == 0 {
			return "", errors.New("no actionable events to show the agent — nothing new since last acted, or PR has no reviewer comments")
		}
		fmt.Fprintln(out, "   (no AUTO events; showing the agent all classified events for iteration)")
	}

	// 5. Fetch the PR head into a known local ref.
	inspectBranch := fmt.Sprintf("aupr-inspect/%s/%d", sanitize(opts.Repo), opts.PRNumber)
	pullRef := fmt.Sprintf("pull/%d/head", opts.PRNumber)
	fmt.Fprintf(out, "Fetching:   origin %s → refs/heads/%s\n", pullRef, inspectBranch)
	if _, err := i.Runner.RunIn(ctx, repo.Path, "git", "fetch", "origin",
		fmt.Sprintf("+%s:refs/heads/%s", pullRef, inspectBranch)); err != nil {
		return "", fmt.Errorf("git fetch: %w", err)
	}

	// 6. Plan + acquire workspace.
	// Substitute pr.HeadRefName with our local inspect branch so the
	// worktree manager creates a dedicated workspace and doesn't
	// collide with the author's own checkout.
	prForWorkspace := pr
	prForWorkspace.HeadRefName = inspectBranch
	wtMgr := &worktree.Manager{
		Cfg:    &i.Cfg.Worktree,
		Runner: i.Runner,
		Logger: logger,
		Prompt: worktree.DenyPrompter{}, // never prompt; inspect never swaps branches
	}
	plan, err := wtMgr.Plan(ctx, repo, prForWorkspace)
	if err != nil {
		return "", fmt.Errorf("workspace plan: %w", err)
	}
	fmt.Fprintf(out, "Workspace:  %s (%s)\n", plan.Path, plan.Action)
	if plan.Action == worktree.ActionCheckout || plan.Action == worktree.ActionSkip {
		return "", fmt.Errorf("inspect refuses workspace action=%s (set [worktree] mode=create)", plan.Action)
	}

	lease, err := wtMgr.Acquire(ctx, plan)
	if err != nil {
		return "", fmt.Errorf("workspace acquire: %w", err)
	}
	workspacePath = lease.Path
	// Do NOT call lease.Release() — inspect leaves the workspace intact
	// for the user to iterate. Release would un-create a fresh worktree.
	_ = lease

	// 7. Record pre-state.
	preSHA, err := revParse(ctx, i.Runner, workspacePath, "HEAD")
	if err != nil {
		return workspacePath, fmt.Errorf("pre-state HEAD: %w", err)
	}
	fmt.Fprintf(out, "Pre-run HEAD: %s\n\n", short(preSHA))

	if opts.Reset {
		fmt.Fprintln(out, "Resetting workspace to PR head (--reset)…")
		if _, err := i.Runner.RunIn(ctx, workspacePath, "git", "reset", "--hard", inspectBranch); err != nil {
			return workspacePath, fmt.Errorf("git reset: %w", err)
		}
		preSHA, _ = revParse(ctx, i.Runner, workspacePath, "HEAD")
	}

	// 8. Invoke the agent for real.
	agentName := i.Cfg.Agent.Default
	commandCfg := i.Cfg.Agent.Command
	if ov, ok := i.Cfg.Repos[pr.Repo]; ok {
		if ov.Agent != "" {
			agentName = ov.Agent
		}
		if len(ov.AgentCommand.Argv) > 0 {
			commandCfg = ov.AgentCommand
		}
	}
	reg := &agent.Registry{Runner: i.Runner, Logger: logger, CommandConfig: commandCfg}
	ag, err := reg.Get(agentName)
	if err != nil {
		return workspacePath, fmt.Errorf("agent: %w", err)
	}
	fmt.Fprintf(out, "Invoking agent: %s (max_turns=%d)…\n", ag.Name(), i.Cfg.Agent.MaxTurnsPerFeedback)

	start := time.Now()
	resp, err := ag.Invoke(ctx, agent.Request{
		Workspace:       workspacePath,
		PR:              pr,
		Classifications: autoClassifications,
		SessionID:       "",
		MaxTurns:        i.Cfg.Agent.MaxTurnsPerFeedback,
		DryRun:          false, // inspect wants the edits
	})
	dur := time.Since(start)
	if err != nil {
		fmt.Fprintf(out, "\nAgent FAILED after %s: %v\n\n", dur.Round(time.Second), err)
		fmt.Fprintf(out, "Workspace left at: %s\n", workspacePath)
		fmt.Fprintln(out, "Inspect partial state with `cd <workspace> && git status && git diff`.")
		return workspacePath, err
	}
	fmt.Fprintf(out, "Agent returned after %s.\n", dur.Round(time.Second))
	if resp.Summary != "" {
		fmt.Fprintf(out, "  Summary: %s\n", resp.Summary)
	}
	if resp.InputTokens+resp.OutputTokens > 0 {
		fmt.Fprintf(out, "  Tokens:  %d in / %d out", resp.InputTokens, resp.OutputTokens)
		if resp.CostUSD > 0 {
			fmt.Fprintf(out, "   Cost: $%.4f", resp.CostUSD)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out)

	// 9. Show what changed.
	if err := i.renderChanges(ctx, out, workspacePath, preSHA); err != nil {
		return workspacePath, fmt.Errorf("render changes: %w", err)
	}

	// 10. Next-step hint.
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Inspection complete. The workspace is preserved for iteration:")
	fmt.Fprintf(out, "  cd %s\n", workspacePath)
	fmt.Fprintln(out, "Re-run with --reset to start over from the PR head.")
	fmt.Fprintln(out, "Remove the worktree when done: git worktree remove", workspacePath)
	return workspacePath, nil
}

func (i *Inspector) renderChanges(ctx context.Context, out io.Writer, workspace, preSHA string) error {
	postSHA, err := revParse(ctx, i.Runner, workspace, "HEAD")
	if err != nil {
		return err
	}

	if postSHA != preSHA {
		fmt.Fprintln(out, "=== New commits ===")
		logOut, _ := i.Runner.RunIn(ctx, workspace, "git", "log", "--oneline", preSHA+".."+postSHA)
		if strings.TrimSpace(logOut.Stdout) == "" {
			fmt.Fprintln(out, "(none)")
		} else {
			fmt.Fprint(out, logOut.Stdout)
		}
		fmt.Fprintln(out)

		fmt.Fprintln(out, "=== Diff of new commits ===")
		diffOut, _ := i.Runner.RunIn(ctx, workspace, "git", "diff", preSHA+".."+postSHA)
		if strings.TrimSpace(diffOut.Stdout) == "" {
			fmt.Fprintln(out, "(empty)")
		} else {
			fmt.Fprint(out, diffOut.Stdout)
		}
		fmt.Fprintln(out)
	} else {
		fmt.Fprintln(out, "=== No new commits ===")
	}

	// Uncommitted (working tree + index).
	statusOut, _ := i.Runner.RunIn(ctx, workspace, "git", "status", "--porcelain")
	if strings.TrimSpace(statusOut.Stdout) != "" {
		fmt.Fprintln(out, "=== Uncommitted changes ===")
		fmt.Fprint(out, statusOut.Stdout)
		fmt.Fprintln(out)
		diffOut, _ := i.Runner.RunIn(ctx, workspace, "git", "diff", "HEAD")
		if strings.TrimSpace(diffOut.Stdout) != "" {
			fmt.Fprintln(out, "=== Diff of uncommitted changes ===")
			fmt.Fprint(out, diffOut.Stdout)
		}
	} else if postSHA == preSHA {
		fmt.Fprintln(out, "The agent did not change any files.")
	}
	return nil
}

func (i *Inspector) log() *slog.Logger {
	if i.Logger != nil {
		return i.Logger
	}
	return slog.Default()
}

// fetchPR pulls one PR's metadata, bypassing the authored-by-@me filter.
// Used only by inspect.
func fetchPR(ctx context.Context, runner execx.Runner, repo string, number int) (feedback.PR, error) {
	res, err := runner.Run(ctx, "gh", "pr", "view", fmt.Sprint(number),
		"--repo", repo,
		"--json", "number,title,url,headRefName,baseRefName,isDraft,mergeable,reviewDecision,createdAt,updatedAt,author")
	if err != nil {
		return feedback.PR{}, err
	}
	var v struct {
		Number         int       `json:"number"`
		Title          string    `json:"title"`
		URL            string    `json:"url"`
		HeadRefName    string    `json:"headRefName"`
		BaseRefName    string    `json:"baseRefName"`
		IsDraft        bool      `json:"isDraft"`
		Mergeable      string    `json:"mergeable"`
		ReviewDecision string    `json:"reviewDecision"`
		CreatedAt      time.Time `json:"createdAt"`
		UpdatedAt      time.Time `json:"updatedAt"`
		Author         struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &v); err != nil {
		return feedback.PR{}, fmt.Errorf("parse pr view: %w", err)
	}
	return feedback.PR{
		Repo: repo, Number: v.Number, Title: v.Title, URL: v.URL,
		HeadRefName: v.HeadRefName, BaseRefName: v.BaseRefName,
		IsDraft: v.IsDraft, Mergeable: v.Mergeable,
		ReviewDecision: v.ReviewDecision,
		CreatedAt:      v.CreatedAt, UpdatedAt: v.UpdatedAt,
		Author: v.Author.Login,
	}, nil
}

func revParse(ctx context.Context, runner execx.Runner, workspace, ref string) (string, error) {
	res, err := runner.RunIn(ctx, workspace, "git", "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

func renderClassification(out io.Writer, d policy.Decision) {
	fmt.Fprintln(out, "=== Classification ===")
	fmt.Fprintf(out, "Action: %s\n", d.Action)
	fmt.Fprintf(out, "Reason: %s\n", d.Reason)
	fmt.Fprintf(out, "Events: %d new\n", len(d.NewEvents))
	for i, c := range d.Classifications {
		fmt.Fprintf(out, "  [%d] %s (%s): %s — %s\n",
			i+1, c.Action, c.Reason, c.Event.Author, oneline(c.Event.Body))
	}
}

func oneline(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > 100 {
		s = s[:97] + "…"
	}
	return s
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func sanitize(s string) string {
	// Git ref components can't contain ".." or ":" or consecutive slashes
	// (well, some can — but owner/repo is safe).
	return strings.ReplaceAll(s, ":", "-")
}
