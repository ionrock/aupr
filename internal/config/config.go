// Package config loads and validates aupr's TOML configuration.
//
// Resolution order (first match wins for the path, then fields merge over
// the baked-in defaults):
//  1. --config flag
//  2. $AUPR_CONFIG environment variable
//  3. ~/.config/aupr/config.toml
//  4. bundled defaults (used if no file exists)
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level daemon configuration.
type Config struct {
	Daemon   DaemonConfig            `toml:"daemon"`
	Worktree WorktreeConfig          `toml:"worktree"`
	Agent    AgentConfig             `toml:"agent"`
	Policy   PolicyConfig            `toml:"policy"`
	Notify   NotifyConfig            `toml:"notify"`
	Repos    map[string]RepoOverride `toml:"repos"`
}

// DaemonConfig drives the main loop.
type DaemonConfig struct {
	TickMinutes        int      `toml:"tick_minutes"`
	Roots              []string `toml:"roots"`
	GithubUser         string   `toml:"github_user"`
	BoundedConcurrency int      `toml:"bounded_concurrency"`
	LogPath            string   `toml:"log_path"`
	StatePath          string   `toml:"state_path"`
}

// WorktreeConfig controls how aupr gets a workspace for a PR.
//
// Resolution order is always:
//  1. Use an existing worktree whose checked-out branch matches pr.HeadRefName.
//  2. Otherwise fall back to `Mode`:
//     "create"   - run CreateCommand to materialize a new worktree
//     "checkout" - use the main repo; swap branches with stash protection
//     "skip"     - never act without a pre-existing worktree
type WorktreeConfig struct {
	Mode          string   `toml:"mode"`
	PathTemplate  string   `toml:"path_template"`
	CreateCommand []string `toml:"create_command"`
	RemoveCommand []string `toml:"remove_command"`
}

// AgentConfig controls which coding agent is invoked and how its session is reused.
type AgentConfig struct {
	Default             string             `toml:"default"`
	SessionReusePolicy  string             `toml:"session_reuse_policy"`
	MaxTurnsPerFeedback int                `toml:"max_turns_per_feedback"`
	DryRun              bool               `toml:"dry_run"`
	Command             AgentCommandConfig `toml:"command"`
}

// AgentCommandConfig configures the generic "command" agent backend.
// The command must produce exit 0 on success. Output parsing (for
// session_id and summary) is optional; if unset, the agent records
// success with an empty session and uses the last non-empty stdout
// line as a summary.
//
// Tokens available in both argv elements:
//
//	{workspace}   absolute workspace path
//	{repo}        repo name (e.g. "internal")
//	{nwo}         owner/name
//	{branch}      PR head branch
//	{pr_number}   PR number
//	{pr_title}    PR title
//	{pr_url}      PR URL
//	{session_id}  previous session ID (empty string if fresh)
//	{max_turns}   decimal
//	{prompt}      the rendered prompt (only when prompt_delivery="arg")
//	{prompt_file} temp-file path (only when prompt_delivery="file")
type AgentCommandConfig struct {
	Argv           []string `toml:"argv"`
	PromptDelivery string   `toml:"prompt_delivery"` // "arg" | "file"
	SessionIDFrom  string   `toml:"session_id_from"` // "json:field" | "line:prefix" | ""
	SummaryFrom    string   `toml:"summary_from"`
}

// PolicyConfig defines what the daemon will and won't act on.
type PolicyConfig struct {
	AutoAddressTypes   []string `toml:"auto_address_types"`
	FlagButDontAct     []string `toml:"flag_but_dont_act"`
	Skip               []string `toml:"skip"`
	MaxFeedbackAgeDays int      `toml:"max_feedback_age_days"`
}

// NotifyConfig controls notification sinks.
type NotifyConfig struct {
	SlackEnabled       bool   `toml:"slack_enabled"`
	SlackChannel       string `toml:"slack_channel"`
	SlackWebhookURL    string `toml:"slack_webhook_url"`
	MacOSNotifications bool   `toml:"macos_notifications"`
	SummaryCadence     string `toml:"summary_cadence"`
}

// RepoOverride is per-repo config keyed by "owner/name".
type RepoOverride struct {
	Agent              string             `toml:"agent"`
	BoundedConcurrency int                `toml:"bounded_concurrency"`
	QualityGates       []string           `toml:"quality_gates"`
	Skip               bool               `toml:"skip"`
	AgentCommand       AgentCommandConfig `toml:"agent_command"`
}

// Defaults returns the baked-in default config.
func Defaults() *Config {
	return &Config{
		Daemon: DaemonConfig{
			TickMinutes:        15,
			Roots:              []string{"~/Dagster", "~/Projects"},
			GithubUser:         "ionrock",
			BoundedConcurrency: 2,
			LogPath:            "~/.local/state/aupr/aupr.log",
			StatePath:          "~/.local/state/aupr/state.db",
		},
		Worktree: WorktreeConfig{
			Mode:          "create",
			PathTemplate:  "~/.workset/{repo}/{branch}",
			CreateCommand: []string{"git", "worktree", "add", "{path}", "{branch}"},
			RemoveCommand: nil,
		},
		Agent: AgentConfig{
			Default:             "claude-code",
			SessionReusePolicy:  "per_pr",
			MaxTurnsPerFeedback: 15,
			DryRun:              false,
			Command: AgentCommandConfig{
				Argv:           []string{"claude", "-p", "--output-format=json", "--max-turns={max_turns}", "{prompt}"},
				PromptDelivery: "arg",
				SessionIDFrom:  "json:session_id",
				SummaryFrom:    "line:SUMMARY:",
			},
		},
		Policy: PolicyConfig{
			AutoAddressTypes:   []string{"typo", "style", "rename", "add-test", "flaky-ci"},
			FlagButDontAct:     []string{"architectural", "revert", "security-touching"},
			Skip:               []string{"draft", "approved", "dependabot"},
			MaxFeedbackAgeDays: 14,
		},
		Notify: NotifyConfig{
			SlackEnabled:       false,
			SlackChannel:       "",
			MacOSNotifications: false,
			SummaryCadence:     "daily",
		},
		Repos: map[string]RepoOverride{},
	}
}

// ResolvePath returns the path aupr will load from.
func ResolvePath(override string) (string, error) {
	if override != "" {
		return expandHome(override)
	}
	if env := os.Getenv("AUPR_CONFIG"); env != "" {
		return expandHome(env)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "aupr", "config.toml"), nil
}

// Load returns the effective config. If the resolved path is missing, defaults
// are returned and the caller can proceed; explicit --config pointing at a
// non-existent file is an error.
func Load(override string) (*Config, error) {
	path, err := ResolvePath(override)
	if err != nil {
		return nil, err
	}
	cfg := Defaults()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if _, err := toml.Decode(string(data), cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		if override != "" {
			return nil, fmt.Errorf("config file %s does not exist", path)
		}
		// fall through with defaults
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	// Expand ~ in path-like fields so downstream code can treat them as absolute.
	cfg.Daemon.Roots = expandAll(cfg.Daemon.Roots)
	cfg.Daemon.LogPath, _ = expandHome(cfg.Daemon.LogPath)
	cfg.Daemon.StatePath, _ = expandHome(cfg.Daemon.StatePath)
	return cfg, nil
}

// InitDefault writes the default config to disk if no file exists.
// Returns the path and whether a file was written.
func InitDefault(override string) (string, bool, error) {
	path, err := ResolvePath(override)
	if err != nil {
		return "", false, err
	}
	if _, err := os.Stat(path); err == nil {
		return path, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, err
	}
	f, err := os.Create(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	if err := WriteTOML(f, Defaults()); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// Edit opens the config file in $EDITOR, creating defaults first if missing.
func Edit(override string) error {
	path, _, err := InitDefault(override)
	if err != nil {
		return err
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// WriteTOML encodes cfg as TOML to w.
func WriteTOML(w io.Writer, cfg *Config) error {
	enc := toml.NewEncoder(w)
	enc.Indent = "  "
	return enc.Encode(cfg)
}

func validate(cfg *Config) error {
	if cfg.Daemon.TickMinutes <= 0 {
		return errors.New("daemon.tick_minutes must be > 0")
	}
	if cfg.Daemon.BoundedConcurrency <= 0 {
		return errors.New("daemon.bounded_concurrency must be > 0")
	}
	if len(cfg.Daemon.Roots) == 0 {
		return errors.New("daemon.roots must not be empty")
	}
	switch cfg.Worktree.Mode {
	case "create", "checkout", "skip":
	default:
		return fmt.Errorf("worktree.mode: invalid value %q (want create|checkout|skip)", cfg.Worktree.Mode)
	}
	if cfg.Worktree.Mode == "create" {
		if cfg.Worktree.PathTemplate == "" {
			return errors.New("worktree.path_template must not be empty when mode=create")
		}
		if len(cfg.Worktree.CreateCommand) == 0 {
			return errors.New("worktree.create_command must not be empty when mode=create")
		}
	}
	switch cfg.Agent.SessionReusePolicy {
	case "per_pr", "fresh", "per_repo":
	default:
		return fmt.Errorf("agent.session_reuse_policy: invalid value %q", cfg.Agent.SessionReusePolicy)
	}
	switch cfg.Agent.Default {
	case "claude-code", "codex", "opencode", "command":
	default:
		return fmt.Errorf("agent.default: invalid value %q", cfg.Agent.Default)
	}
	if cfg.Agent.Default == "command" && len(cfg.Agent.Command.Argv) == 0 {
		return errors.New("agent.command.argv must not be empty when default=command")
	}
	if cfg.Agent.Command.PromptDelivery != "" {
		switch cfg.Agent.Command.PromptDelivery {
		case "arg", "file":
		default:
			return fmt.Errorf("agent.command.prompt_delivery: invalid value %q (want arg|file)", cfg.Agent.Command.PromptDelivery)
		}
	}
	return nil
}

func expandHome(p string) (string, error) {
	if p == "" {
		return p, nil
	}
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p, err
	}
	if p == "~" {
		return home, nil
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

func expandAll(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		ex, err := expandHome(p)
		if err != nil {
			out[i] = p
		} else {
			out[i] = ex
		}
	}
	return out
}
