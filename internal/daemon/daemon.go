// Package daemon runs the scheduler on a ticker until the context is
// cancelled. Designed to be launched by launchd / systemd / tmux — one
// process, one ticker, no self-forking.
package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/ionrock/aupr/internal/scheduler"
)

// Run blocks until ctx is cancelled, ticking every cfgTick minutes.
// The first tick runs immediately so you don't wait a full period to see
// whether the daemon is alive.
func Run(ctx context.Context, sch *scheduler.Scheduler, tickMinutes int, opts scheduler.Options, logger *slog.Logger) error {
	if tickMinutes <= 0 {
		tickMinutes = 15
	}
	period := time.Duration(tickMinutes) * time.Minute

	logger.Info("aupr daemon starting",
		"tick_minutes", tickMinutes, "dry_run", opts.DryRun)
	defer logger.Info("aupr daemon stopped")

	// Initial tick.
	if err := runTick(ctx, sch, opts, logger); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		logger.Error("tick failed", "err", err)
	}

	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := runTick(ctx, sch, opts, logger); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				logger.Error("tick failed", "err", err)
			}
		}
	}
}

func runTick(ctx context.Context, sch *scheduler.Scheduler, opts scheduler.Options, logger *slog.Logger) error {
	// Bound a single tick so a stuck gh call doesn't hang the daemon.
	tickCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	return sch.RunOnce(tickCtx, opts)
}
