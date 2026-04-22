// Package cli wires up the aupr urfave/cli command tree.
package cli

import (
	"context"
	"fmt"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/daemon"
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
			cmdStatus(),
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

func cmdStatus() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "show cursors, recent attempts, and skip list",
		Action: func(ctx context.Context, c *cli.Command) error {
			_, st, closeState, err := loadConfigAndState(c)
			if err != nil {
				return err
			}
			defer closeState()

			out := c.Writer
			fmt.Fprintln(out, "Skip list:")
			skips, _ := st.ListSkipped(ctx)
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

func cmdPause() *cli.Command {
	return &cli.Command{
		Name:  "pause",
		Usage: "stop acting (keep polling)",
		Action: func(_ context.Context, _ *cli.Command) error {
			return fmt.Errorf("pause: not implemented (runtime pause control is M4)")
		},
	}
}

func cmdResume() *cli.Command {
	return &cli.Command{
		Name:  "resume",
		Usage: "resume acting after pause",
		Action: func(_ context.Context, _ *cli.Command) error {
			return fmt.Errorf("resume: not implemented (runtime pause control is M4)")
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
		Usage: "print (or tail) the daemon log",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}},
		},
		Action: func(_ context.Context, _ *cli.Command) error {
			return fmt.Errorf("logs: not implemented (use `tail -f ~/.local/state/aupr/aupr.log`)")
		},
	}
}
