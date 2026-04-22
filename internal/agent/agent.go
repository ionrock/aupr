// Package agent invokes an AI coding agent (claude-code, codex, opencode)
// inside a workspace to address a batch of reviewer feedback events.
//
// M3 ships claude-code; other agents return ErrNotImplemented so the
// registration surface is in place for M4/M5 without blocking iteration.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/feedback"
	"github.com/ionrock/aupr/internal/policy"
)

// Request is what the scheduler hands to an Agent.
type Request struct {
	Workspace       string // absolute path on disk
	PR              feedback.PR
	Classifications []policy.EventClass // only AUTO-classified events get passed
	SessionID       string              // "" for fresh; else resume target
	MaxTurns        int
	DryRun          bool // if true, return a Response without invoking anything
}

// Response is what the Agent returned.
type Response struct {
	SessionID    string
	Summary      string
	FilesTouched []string
	NewCommit    string // sha of the agent's commit, if any; empty if none
	Output       string // raw agent stdout (for audit; may be large)

	// Cost tracking (0 if not reported by the backend).
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
}

// Agent is the contract for a coding-agent backend.
type Agent interface {
	Name() string
	Invoke(ctx context.Context, req Request) (*Response, error)
}

// ErrNotImplemented is returned by agents whose backend isn't wired yet.
var ErrNotImplemented = errors.New("agent: backend not implemented")

// Registry resolves an agent name to an Agent instance.
//
// The "command" backend is driven by the CommandConfig field; callers
// (typically the scheduler) may point CommandConfig at a per-repo
// override before calling Get("command").
type Registry struct {
	Runner        execx.Runner
	Logger        *slog.Logger
	CommandConfig config.AgentCommandConfig
}

// Get returns an Agent for the named backend.
func (r *Registry) Get(name string) (Agent, error) {
	switch name {
	case "claude-code":
		return &ClaudeCode{Runner: r.Runner, Logger: r.Logger}, nil
	case "command":
		return &Command{Cfg: r.CommandConfig, Runner: r.Runner, Logger: r.Logger}, nil
	case "codex":
		return &stub{name: "codex"}, nil
	case "opencode":
		return &stub{name: "opencode"}, nil
	}
	return nil, fmt.Errorf("agent: unknown backend %q", name)
}

type stub struct{ name string }

func (s *stub) Name() string { return s.name }
func (s *stub) Invoke(_ context.Context, _ Request) (*Response, error) {
	return nil, fmt.Errorf("%s: %w", s.name, ErrNotImplemented)
}
