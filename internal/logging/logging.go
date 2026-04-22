// Package logging wraps slog with aupr defaults.
package logging

import (
	"log/slog"
	"os"
)

// New returns a JSON slog.Logger writing to stderr.
func New(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return slog.New(h)
}
