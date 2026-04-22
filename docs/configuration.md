# Configuration

aupr is configured by a single TOML file. The daemon works with zero
configuration — `Defaults()` in `internal/config/config.go` is the source
of truth and every field here has a baked-in default.

## File location

Resolution order, first match wins:

1. `--config <path>` (global flag)
2. `$AUPR_CONFIG`
3. `~/.config/aupr/config.toml`
4. Built-in defaults (used if no file exists and no path was forced)

If `--config` or `$AUPR_CONFIG` points at a missing file, aupr exits with
an error. A missing default-location file is silently tolerated.

## Commands

```
aupr config path          # show the path that would be loaded
aupr config show          # print the effective merged config as TOML
aupr config init          # write Defaults() to the resolved path if missing
aupr config edit          # create-if-missing then open in $EDITOR
```

## Schema

```toml
[daemon]
tick_minutes         = 15                                   # loop period
roots                = ["~/Dagster", "~/Projects"]          # where to look for git repos
github_user          = "ionrock"                            # --author filter for gh search
bounded_concurrency  = 2                                    # worker goroutines (M3+)
log_path             = "~/.local/state/aupr/aupr.log"
state_path           = "~/.local/state/aupr/state.db"      # sqlite cursor DB (M3+)

[worktree]
reuse_policy  = "per_pr"        # per_pr | per_repo_pool | ephemeral
base_path     = "~/.workset"    # matches `wt`'s default
branch_prefix = "eric/"         # matches Eric's convention

[agent]
default                 = "claude-code"  # claude-code | codex | opencode
session_reuse_policy    = "per_pr"       # per_pr | fresh | per_repo
max_turns_per_feedback  = 15
dry_run                 = false          # equivalent to --dry-run (merged via OR)

[policy]
auto_address_types    = ["typo", "style", "rename", "add-test", "flaky-ci"]
flag_but_dont_act     = ["architectural", "revert", "security-touching"]
skip                  = ["draft", "approved", "dependabot"]
max_feedback_age_days = 14

[notify]
slack_enabled        = false
slack_channel        = ""
macos_notifications  = false
summary_cadence      = "daily"     # never | per_action | daily

# Per-repo overrides, keyed by "owner/name".
[repos."dagster-io/internal"]
agent               = "codex"
bounded_concurrency = 1
quality_gates       = ["just check"]
skip                = false        # or true to blacklist a repo

[repos."ionrock/workset"]
agent = "claude-code"
quality_gates = ["cask eval", "emacs -batch -l ert -l test/workset-test.el -f ert-run-tests-batch-and-exit"]
```

## Adding a new config field

1. Add the field to the appropriate struct in `internal/config/config.go`
   with a `toml:"..."` tag.
2. Set its default value in `Defaults()`.
3. If it has a closed set of valid values, add a case to `validate()`.
4. If it's a path-like field, add a call in `Load()` to pass it through
   `expandHome`.
5. If it should be overridable by env var or flag, wire that in
   `internal/cli/cli.go` (follow `--config` / `AUPR_CONFIG` as the
   template).
6. Add a round-trip test: ensure `WriteTOML(Defaults())` contains the new
   key, and that `Load()` on a file with the key returns the value.

`config show` will render the new field automatically; that's your
sanity check.

## `--dry-run` merge rule

The effective dry-run flag is the logical OR of three sources:

```
effective = CLI --dry-run  ||  $AUPR_DRY_RUN  ||  cfg.Agent.DryRun
```

You can't turn dry-run off via the flag if the config has it enabled. That
is deliberate — it means "paranoid in config, paranoid at runtime."

## Precedence summary

```
built-in Defaults()
  └─▶ overlaid by ~/.config/aupr/config.toml (or $AUPR_CONFIG / --config)
        └─▶ per-repo [repos."owner/name"] overrides (agent, concurrency, gates)
              └─▶ runtime flags (--dry-run, --verbose) applied on top
```
