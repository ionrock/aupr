// Package cli wires up the aupr urfave/cli command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/daemon"
	"github.com/ionrock/aupr/internal/digest"
	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/inspect"
	"github.com/ionrock/aupr/internal/logging"
	"github.com/ionrock/aupr/internal/scheduler"
	"github.com/ionrock/aupr/internal/state"
	"github.com/urfave/cli/v3"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// NewApp builds the root aupr *cli.Command.
func NewApp() *cli.Command {
	return &cli.Command{
		Name:    "aupr",
		Usage:   "PR feedback daemon",
		Version: Version,
		Description: "aupr polls your open PRs and spawns AI coding sessions " +
			"to address reviewer feedback.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Usage:   "path to config.toml (default: ~/.config/aupr/config.toml)",
				Sources: cli.EnvVars("AUPR_CONFIG"),
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "verbose logging",
			},
			&cli.BoolFlag{
				Name:    "dry-run",
				Aliases: []string{"n"},
				Usage:   "do not perform mutations: no git writes, no pushes, no comment replies, no agent invocations",
				Sources: cli.EnvVars("AUPR_DRY_RUN"),
			},
		},
		Commands: []*cli.Command{
			cmdRun(),
			cmdOnce(),
			cmdTest(),
			cmdInspect(),
			cmdStatus(),
			cmdDigest(),
			cmdRecovery(),
			cmdSkip(),
			cmdUnskip(),
			cmdConfig(),
			cmdLogs(),
			cmdPause(),
			cmdResume(),
		},
	}
}

// loadConfigAndState opens both and returns a cleanup closer for state.
func loadConfigAndState(c *cli.Command) (*config.Config, state.Store, func() error, error) {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load config: %w", err)
	}
	st, err := state.OpenSQLite(cfg.Daemon.StatePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open state: %w", err)
	}
	return cfg, st, st.Close, nil
}

func cmdRun() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "start the daemon in the foreground (ticks every [daemon] tick_minutes)",
		Description: "Combine with --dry-run to stand the daemon up locally " +
			"without any side effects.",
		Action: func(ctx context.Context, c *cli.Command) error {
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			cfg, st, closeState, err := loadConfigAndState(c)
			if err != nil {
				return err
			}
			defer closeState()
			logger := logging.New(c.Bool("verbose"))
			sch := scheduler.New(cfg, logger, st)
			return daemon.Run(ctx, sch, cfg.Daemon.TickMinutes, scheduler.Options{
				DryRun:      c.Bool("dry-run") || cfg.Agent.DryRun,
				Interactive: false, // daemon mode: never prompt
			}, logger)
		},
	}
}

func cmdOnce() *cli.Command {
	return &cli.Command{
		Name:  "once",
		Usage: "run a single tick and exit",
		Action: func(ctx context.Context, c *cli.Command) error {
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			cfg, st, closeState, err := loadConfigAndState(c)
			if err != nil {
				return err
			}
			defer closeState()
			logger := logging.New(c.Bool("verbose"))
			sch := scheduler.New(cfg, logger, st)
			return sch.RunOnce(ctx, scheduler.Options{
				DryRun:      c.Bool("dry-run") || cfg.Agent.DryRun,
				Interactive: true,
			})
		},
	}
}

func cmdTest() *cli.Command {
	return &cli.Command{
		Name:      "test",
		Usage:     "preview the action aupr would take for a specific PR",
		ArgsUsage: "<repo> <pr-number>",
		Description: "Runs the full pipeline against a single PR in dry-run " +
			"unless --dry-run=false is passed. Useful for calibrating " +
			"policy or validating configuration on a real PR without " +
			"touching others.",
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 2 {
				return fmt.Errorf("test requires: <owner/repo> <pr-number>")
			}
			repo := c.Args().Get(0)
			prNum, err := strconv.Atoi(c.Args().Get(1))
			if err != nil || prNum <= 0 {
				return fmt.Errorf("invalid PR number: %s", c.Args().Get(1))
			}
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			cfg, st, closeState, err := loadConfigAndState(c)
			if err != nil {
				return err
			}
			defer closeState()
			logger := logging.New(c.Bool("verbose"))
			sch := scheduler.New(cfg, logger, st)
			// test is always dry-run by default; --dry-run=false to actually act.
			dryRun := true
			if c.IsSet("dry-run") {
				dryRun = c.Bool("dry-run")
			}
			return sch.RunOnce(ctx, scheduler.Options{
				DryRun:      dryRun || cfg.Agent.DryRun,
				Interactive: true,
				OnlyRepo:    repo,
				OnlyPR:      prNum,
			})
		},
	}
}

func cmdInspect() *cli.Command {
	return &cli.Command{
		Name:      "inspect",
		Usage:     "run the agent on any PR, show the diff, never push",
		ArgsUsage: "<repo> <pr-number>",
		Description: "Read-only iteration tool. Fetches the PR head, acquires " +
			"a workspace, invokes the agent for real, and prints the diff. " +
			"Works on any PR (not just yours). Never writes state, never " +
			"pushes, never comments. Leaves the workspace for manual " +
			"inspection.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "reset", Usage: "git reset --hard to PR head before invoking agent"},
			&cli.BoolFlag{Name: "force-classify", Usage: "suppress the warning when classification is FLAG or SKIP"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 2 {
				return fmt.Errorf("inspect requires: <owner/repo> <pr-number>")
			}
			repo := c.Args().Get(0)
			prNum, err := strconv.Atoi(c.Args().Get(1))
			if err != nil || prNum <= 0 {
				return fmt.Errorf("invalid PR number: %s", c.Args().Get(1))
			}
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			cfg, err := config.Load(c.String("config"))
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			logger := logging.New(c.Bool("verbose"))
			runner := &execx.OS{Logger: logger}
			insp := &inspect.Inspector{Cfg: cfg, Runner: runner, Logger: logger}
			_, err = insp.Run(ctx, inspect.Options{
				Repo:          repo,
				PRNumber:      prNum,
				Reset:         c.Bool("reset"),
				ForceClassify: c.Bool("force-classify"),
				User:          cfg.Daemon.GithubUser,
				Output:        c.Writer,
			})
			return err
		},
	}
}

func cmdStatus() *cli.Command {
	return &cli.Command{
		Name:      "status",
		Usage:     "show daemon status, cursors, recent attempts, and skip list",
		ArgsUsage: "[repo pr-number]",
		Action: func(ctx context.Context, c *cli.Command) error {
			_, st, closeState, err := loadConfigAndState(c)
			if err != nil {
				return err
			}
			defer closeState()

			out := c.Writer

			// Detail mode: `aupr status owner/repo pr`
			if c.Args().Len() == 2 {
				return statusDetail(ctx, c, st, c.Args().Get(0), c.Args().Get(1))
			}

			// Summary mode.
			paused, reason, _ := st.IsPaused(ctx)
			if paused {
				fmt.Fprintf(out, "Daemon state: PAUSED (%s)\n\n", reason)
			} else {
				fmt.Fprintln(out, "Daemon state: running")
				fmt.Fprintln(out)
			}

			cursors, _ := st.AllCursors(ctx)
			fmt.Fprintf(out, "Cursors (%d PR(s) tracked):\n", len(cursors))
			if len(cursors) == 0 {
				fmt.Fprintln(out, "  (none yet — aupr hasn't recorded any PRs)")
			} else {
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "  REPO\tPR\tLAST EVENT\tUPDATED")
				for _, cur := range cursors {
					fmt.Fprintf(tw, "  %s\t#%d\t%s\t%s\n",
						cur.Repo, cur.PRNumber, truncateStr(cur.LastEventID, 24),
						cur.UpdatedAt.Format("2006-01-02 15:04"))
				}
				tw.Flush()
			}
			fmt.Fprintln(out)

			attempts, _ := st.AllRecentAttempts(ctx, 10)
			fmt.Fprintf(out, "Recent attempts (%d):\n", len(attempts))
			if len(attempts) == 0 {
				fmt.Fprintln(out, "  (none)")
			} else {
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "  WHEN\tREPO\tPR\tOUTCOME\tAGENT\tSUMMARY")
				for _, a := range attempts {
					sum := a.Summary
					if sum == "" && a.Error != "" {
						sum = "err: " + a.Error
					}
					fmt.Fprintf(tw, "  %s\t%s\t#%d\t%s\t%s\t%s\n",
						age(a.FinishedAt), a.Repo, a.PRNumber, a.Outcome, a.Agent,
						truncateStr(sum, 70))
				}
				tw.Flush()
			}
			fmt.Fprintln(out)

			skips, _ := st.ListSkipped(ctx)
			fmt.Fprintf(out, "Skip list (%d):\n", len(skips))
			if len(skips) == 0 {
				fmt.Fprintln(out, "  (none)")
			} else {
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "  REPO\tPR\tADDED\tREASON")
				for _, s := range skips {
					fmt.Fprintf(tw, "  %s\t#%d\t%s\t%s\n",
						s.Repo, s.PRNumber, s.AddedAt.Format("2006-01-02"), s.Reason)
				}
				tw.Flush()
			}
			return nil
		},
	}
}

func statusDetail(ctx context.Context, c *cli.Command, st state.Store, repo, prStr string) error {
	n, err := strconv.Atoi(prStr)
	if err != nil {
		return fmt.Errorf("invalid pr number: %v", err)
	}
	out := c.Writer
	fmt.Fprintf(out, "%s#%d\n\n", repo, n)

	cursor, _ := st.LastSeen(ctx, repo, n)
	if cursor == "" {
		fmt.Fprintln(out, "Cursor: (none — aupr hasn't acted yet)")
	} else {
		fmt.Fprintf(out, "Cursor: %s\n", cursor)
	}

	if ok, reason, _ := st.IsSkipped(ctx, repo, n); ok {
		fmt.Fprintf(out, "Skip: yes (%s)\n", reason)
	}
	fmt.Fprintln(out)

	atts, _ := st.RecentAttempts(ctx, repo, n, 20)
	fmt.Fprintf(out, "Attempts (%d most recent):\n", len(atts))
	if len(atts) == 0 {
		fmt.Fprintln(out, "  (none)")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  FINISHED\tOUTCOME\tAGENT\tSHA\tSUMMARY/ERROR")
	for _, a := range atts {
		msg := a.Summary
		if msg == "" && a.Error != "" {
			msg = a.Error
		}
		sha := a.CommitSHA
		if len(sha) > 8 {
			sha = sha[:8]
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			a.FinishedAt.Format("2006-01-02 15:04"),
			a.Outcome, a.Agent, sha, truncateStr(msg, 80))
	}
	tw.Flush()
	return nil
}

func cmdSkip() *cli.Command {
	return &cli.Command{
		Name:      "skip",
		Usage:     "persistently skip a PR",
		ArgsUsage: "<repo> <pr-number> [reason...]",
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() < 2 {
				return fmt.Errorf("skip requires: <owner/repo> <pr-number> [reason]")
			}
			repo := c.Args().Get(0)
			n, err := strconv.Atoi(c.Args().Get(1))
			if err != nil {
				return fmt.Errorf("invalid pr number: %v", err)
			}
			reason := "manual"
			if c.Args().Len() > 2 {
				reason = strings.Join(c.Args().Slice()[2:], " ")
			}
			_, st, closeState, err := loadConfigAndState(c)
			if err != nil {
				return err
			}
			defer closeState()
			if err := st.Skip(ctx, repo, n, reason); err != nil {
				return err
			}
			fmt.Fprintf(c.Writer, "skipped %s#%d (%s)\n", repo, n, reason)
			return nil
		},
	}
}

func cmdUnskip() *cli.Command {
	return &cli.Command{
		Name:      "unskip",
		Usage:     "remove a PR from the skip list",
		ArgsUsage: "<repo> <pr-number>",
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 2 {
				return fmt.Errorf("unskip requires: <owner/repo> <pr-number>")
			}
			repo := c.Args().Get(0)
			n, err := strconv.Atoi(c.Args().Get(1))
			if err != nil {
				return fmt.Errorf("invalid pr number: %v", err)
			}
			_, st, closeState, err := loadConfigAndState(c)
			if err != nil {
				return err
			}
			defer closeState()
			if err := st.Unskip(ctx, repo, n); err != nil {
				return err
			}
			fmt.Fprintf(c.Writer, "unskipped %s#%d\n", repo, n)
			return nil
		},
	}
}

func cmdDigest() *cli.Command {
	return &cli.Command{
		Name:  "digest",
		Usage: "print a summary of recent activity (last 24h by default)",
		Flags: []cli.Flag{
			&cli.DurationFlag{Name: "since", Value: 24 * time.Hour, Usage: "time window"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			_, st, closeState, err := loadConfigAndState(c)
			if err != nil {
				return err
			}
			defer closeState()
			now := time.Now()
			since := now.Add(-c.Duration("since"))
			attempts, _ := st.AttemptsSince(ctx, since)
			skips, _ := st.ListSkipped(ctx)
			stashes, _ := st.ListRecoveryStashes(ctx)
			summary := digest.Build(since, now, attempts, skips, stashes)
			fmt.Fprint(c.Writer, summary.Format())
			return nil
		},
	}
}

func cmdRecovery() *cli.Command {
	return &cli.Command{
		Name:  "recovery",
		Usage: "list aupr-authored stashes left behind by an interrupted protocol",
		Action: func(ctx context.Context, c *cli.Command) error {
			_, st, closeState, err := loadConfigAndState(c)
			if err != nil {
				return err
			}
			defer closeState()
			stashes, err := st.ListRecoveryStashes(ctx)
			if err != nil {
				return err
			}
			out := c.Writer
			if len(stashes) == 0 {
				fmt.Fprintln(out, "No recovery stashes tracked.")
				return nil
			}
			fmt.Fprintf(out, "%d recovery stash(es) — run `git stash pop <ref>` in each to recover:\n\n", len(stashes))
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "  REPO PATH\tREF\tFIRST SEEN\tMESSAGE")
			for _, s := range stashes {
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
					s.RepoPath, s.Ref, s.FirstSeenAt.Format("2006-01-02 15:04"),
					truncateStr(s.Message, 60))
			}
			tw.Flush()
			return nil
		},
	}
}

func cmdPause() *cli.Command {
	return &cli.Command{
		Name:      "pause",
		Usage:     "stop acting (keep polling). Takes effect on the next tick.",
		ArgsUsage: "[reason...]",
		Action: func(ctx context.Context, c *cli.Command) error {
			_, st, closeState, err := loadConfigAndState(c)
			if err != nil {
				return err
			}
			defer closeState()
			reason := "manual"
			if c.Args().Len() > 0 {
				reason = strings.Join(c.Args().Slice(), " ")
			}
			if err := st.Pause(ctx, reason); err != nil {
				return err
			}
			fmt.Fprintf(c.Writer, "paused: %s\n", reason)
			return nil
		},
	}
}

func cmdResume() *cli.Command {
	return &cli.Command{
		Name:  "resume",
		Usage: "resume acting after pause",
		Action: func(ctx context.Context, c *cli.Command) error {
			_, st, closeState, err := loadConfigAndState(c)
			if err != nil {
				return err
			}
			defer closeState()
			if err := st.Unpause(ctx); err != nil {
				return err
			}
			fmt.Fprintln(c.Writer, "resumed")
			return nil
		},
	}
}

func cmdConfig() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "inspect or edit config",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "print merged effective config as TOML",
				Action: func(_ context.Context, c *cli.Command) error {
					cfg, err := config.Load(c.String("config"))
					if err != nil {
						return err
					}
					return config.WriteTOML(c.Writer, cfg)
				},
			},
			{
				Name:  "path",
				Usage: "print the config file path that would be loaded",
				Action: func(_ context.Context, c *cli.Command) error {
					p, err := config.ResolvePath(c.String("config"))
					if err != nil {
						return err
					}
					fmt.Fprintln(c.Writer, p)
					return nil
				},
			},
			{
				Name:  "init",
				Usage: "write a default config file if none exists",
				Action: func(_ context.Context, c *cli.Command) error {
					p, wrote, err := config.InitDefault(c.String("config"))
					if err != nil {
						return err
					}
					if wrote {
						fmt.Fprintf(c.Writer, "wrote default config to %s\n", p)
					} else {
						fmt.Fprintf(c.Writer, "config already exists at %s\n", p)
					}
					return nil
				},
			},
			{
				Name:  "edit",
				Usage: "open the config file in $EDITOR",
				Action: func(_ context.Context, c *cli.Command) error {
					return config.Edit(c.String("config"))
				},
			},
		},
	}
}

func cmdLogs() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "print or tail the launchd daemon log files",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "follow (tail -f)"},
			&cli.IntFlag{Name: "lines", Aliases: []string{"n"}, Value: 200, Usage: "lines to print when not following"},
			&cli.BoolFlag{Name: "err", Usage: "show stderr only (default: both streams interleaved)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			cfg, err := config.Load(c.String("config"))
			if err != nil {
				return err
			}
			// The daemon's log file lives at cfg.Daemon.LogPath. The launchd
			// plist writes aupr.{out,err}.log next to it.
			dir := directoryOf(cfg.Daemon.LogPath)
			var paths []string
			if !c.Bool("err") {
				paths = append(paths, dir+"/aupr.out.log")
			}
			paths = append(paths, dir+"/aupr.err.log")

			var existing []string
			for _, p := range paths {
				if _, err := os.Stat(p); err == nil {
					existing = append(existing, p)
				} else if !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
			if len(existing) == 0 {
				return fmt.Errorf("no aupr log files in %s (install launchd: scripts/install-launchd.sh)", dir)
			}

			args := []string{"-n", strconv.Itoa(c.Int("lines"))}
			if c.Bool("follow") {
				args = append(args, "-F")
			}
			args = append(args, existing...)
			cmd := exec.CommandContext(ctx, "tail", args...)
			cmd.Stdout = c.Writer
			cmd.Stderr = c.ErrWriter
			return cmd.Run()
		},
	}
}

func directoryOf(p string) string {
	// filepath.Dir but tolerant of trailing slashes.
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
