// Package cli wires up the aupr urfave/cli command tree.
package cli

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/dagster-io/aupr/internal/config"
	"github.com/dagster-io/aupr/internal/logging"
	"github.com/dagster-io/aupr/internal/scheduler"
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
			cmdStatus(),
			cmdPause(),
			cmdResume(),
			cmdSkip(),
			cmdUnskip(),
			cmdConfig(),
			cmdLogs(),
			cmdTest(),
		},
	}
}

func cmdRun() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "start the daemon in the foreground",
		Description: "Long-running loop. Combine with --dry-run to stand the " +
			"daemon up locally without any side effects.",
		Action: func(_ context.Context, c *cli.Command) error {
			_ = c.Bool("dry-run") // wired; honored when M3 implements the loop
			return fmt.Errorf("run: not implemented in M1 (use `aupr once`)")
		},
	}
}

func cmdOnce() *cli.Command {
	return &cli.Command{
		Name:  "once",
		Usage: "run a single tick of the discovery+decision loop and exit",
		Description: "M1: read-only scout. Walks configured roots, enumerates " +
			"open PRs, and prints a decision table. Pair with --dry-run (global) " +
			"to guarantee no mutations once those code paths exist.",
		Action: func(ctx context.Context, c *cli.Command) error {
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			cfg, err := config.Load(c.String("config"))
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			logger := logging.New(c.Bool("verbose"))
			s := scheduler.New(cfg, logger)
			return s.RunOnce(ctx, scheduler.Options{DryRun: c.Bool("dry-run") || cfg.Agent.DryRun})
		},
	}
}

func cmdStatus() *cli.Command {
	return &cli.Command{
		Name:      "status",
		Usage:     "show daemon status or a single PR",
		ArgsUsage: "[pr]",
		Action: func(_ context.Context, _ *cli.Command) error {
			return fmt.Errorf("status: not implemented yet")
		},
	}
}

func cmdPause() *cli.Command {
	return &cli.Command{
		Name:  "pause",
		Usage: "stop acting (keep polling)",
		Action: func(_ context.Context, _ *cli.Command) error {
			return fmt.Errorf("pause: not implemented yet")
		},
	}
}

func cmdResume() *cli.Command {
	return &cli.Command{
		Name:  "resume",
		Usage: "resume acting after pause",
		Action: func(_ context.Context, _ *cli.Command) error {
			return fmt.Errorf("resume: not implemented yet")
		},
	}
}

func cmdSkip() *cli.Command {
	return &cli.Command{
		Name:      "skip",
		Usage:     "never act on this PR",
		ArgsUsage: "<pr-url-or-ref>",
		Action: func(_ context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return fmt.Errorf("skip requires exactly one argument")
			}
			return fmt.Errorf("skip: not implemented yet")
		},
	}
}

func cmdUnskip() *cli.Command {
	return &cli.Command{
		Name:      "unskip",
		Usage:     "remove a PR from the skip list",
		ArgsUsage: "<pr-url-or-ref>",
		Action: func(_ context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return fmt.Errorf("unskip requires exactly one argument")
			}
			return fmt.Errorf("unskip: not implemented yet")
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
			&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "follow the log"},
		},
		Action: func(_ context.Context, _ *cli.Command) error {
			return fmt.Errorf("logs: not implemented yet")
		},
	}
}

func cmdTest() *cli.Command {
	return &cli.Command{
		Name:      "test",
		Usage:     "preview the action the daemon would take for a specific PR",
		ArgsUsage: "<pr-url-or-ref>",
		Action: func(_ context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return fmt.Errorf("test requires exactly one argument")
			}
			return fmt.Errorf("test: not implemented yet")
		},
	}
}
