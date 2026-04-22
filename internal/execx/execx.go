// Package execx runs external commands with context, logging, and output capture.
//
// All subprocess invocations in aupr go through this package so we have one
// place to enforce timeouts, redact tokens from logs, and test against a fake.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// Result bundles the outputs of a command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner executes commands. Tests can substitute a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (*Result, error)
	RunIn(ctx context.Context, dir string, name string, args ...string) (*Result, error)
}

// OS is the default runner that shells out to the real OS.
type OS struct {
	Logger *slog.Logger
}

// Run executes a command in the current working directory.
func (o *OS) Run(ctx context.Context, name string, args ...string) (*Result, error) {
	return o.RunIn(ctx, "", name, args...)
}

// RunIn executes a command with the given working directory.
func (o *OS) RunIn(ctx context.Context, dir, name string, args ...string) (*Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if o.Logger != nil {
		o.Logger.Debug("exec", "cmd", name, "args", redactArgs(args), "dir", dir)
	}
	err := cmd.Run()
	res := &Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: cmd.ProcessState.ExitCode(),
	}
	if err != nil {
		// Return a rich error but keep Result so callers can inspect partial output.
		return res, fmt.Errorf("%s %s: %w (stderr=%s)",
			name, strings.Join(redactArgs(args), " "), err, trim(res.Stderr))
	}
	return res, nil
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

// redactArgs masks obvious token-shaped arguments.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "ghp_"),
			strings.HasPrefix(a, "gho_"),
			strings.HasPrefix(a, "github_pat_"),
			strings.HasPrefix(a, "xoxb-"),
			strings.HasPrefix(a, "xoxp-"):
			out[i] = "<redacted>"
		default:
			out[i] = a
		}
	}
	return out
}
