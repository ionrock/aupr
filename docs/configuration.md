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
mode           = "create"                                        # create | checkout | skip
path_template  = "~/.workset/{repo}/{branch}"                    # where new worktrees go
create_command = ["git", "worktree", "add", "{path}", "{branch}"] # argv array, tokens substituted
# remove_command = ["git", "worktree", "remove", "--force", "{path}"]  # optional, unused today

# See "Worktree handling" below for the full contract, token list, and
# examples of plugging in alternative tools (wt, superset.sh, etc.).

[agent]
default                 = "claude-code"  # claude-code | codex | opencode | command
session_reuse_policy    = "per_pr"       # per_pr | fresh | per_repo
max_turns_per_feedback  = 15
dry_run                 = false          # equivalent to --dry-run (merged via OR)

[agent.command]
# Pluggable backend. Tokens: {workspace}, {repo}, {nwo}, {branch},
# {pr_number}, {pr_title}, {pr_url}, {session_id}, {max_turns}, {prompt}, {prompt_file}
argv             = ["claude", "-p", "--output-format=json", "--max-turns={max_turns}", "{prompt}"]
prompt_delivery  = "arg"            # arg | file
session_id_from  = "json:session_id" # json:<dotted.path> | line:<prefix> | ""
summary_from     = "line:SUMMARY:"   # same forms

[policy]
auto_address_types    = ["typo", "style", "rename", "add-test", "flaky-ci"]
flag_but_dont_act     = ["architectural", "revert", "security-touching"]
skip                  = ["draft", "approved", "dependabot"]
max_feedback_age_days = 14

[notify]
slack_enabled        = false
slack_webhook_url    = ""          # https://hooks.slack.com/services/...
slack_channel        = ""          # informational only; webhook URL determines routing
macos_notifications  = false
summary_cadence      = "daily"     # never | per_action | daily  (reserved for M5+)

# Per-repo overrides, keyed by "owner/name".
[repos."dagster-io/internal"]
agent               = "command"
bounded_concurrency = 1
quality_gates       = ["just check"]
skip                = false        # or true to blacklist a repo
# Per-repo 'command' backend overrides the global argv wholesale:
[repos."dagster-io/internal".agent_command]
argv            = ["internal-ai-wrap", "--pr={pr_number}", "--prompt-file={prompt_file}"]
prompt_delivery = "file"
session_id_from = "json:id"
summary_from    = "line:RESULT:"

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

## Agent backends

`[agent] default` picks which backend the scheduler invokes. Options:

| Backend | Status | Notes |
|---|---|---|
| `claude-code` | ✅ | Wraps `claude -p --output-format=json`, knows the JSON schema, parses `session_id` / `result` fields. |
| `command` | ✅ | Pluggable. Runs any argv you configure with token substitution. Use this for custom wrappers, team tooling, or alternative LLM CLIs. |
| `codex` | stub | Returns `ErrNotImplemented`; wiring deferred. |
| `opencode` | stub | Same. |

### The `command` backend in detail

Mirrors the `worktree.create_command` design: argv array, token
substitution, cwd = `{workspace}`, exit 0 means success.

**Tokens** (available in every argv element):

| Token | Value |
|---|---|
| `{workspace}` | absolute workspace path |
| `{repo}` | repo name (e.g. `internal`) |
| `{nwo}` | `owner/name` |
| `{branch}` | PR head branch |
| `{pr_number}` / `{pr_title}` / `{pr_url}` | PR metadata |
| `{session_id}` | previous session id from state DB; `""` if fresh |
| `{max_turns}` | decimal from `[agent] max_turns_per_feedback` |
| `{prompt}` | the rendered prompt (valid when `prompt_delivery="arg"`) |
| `{prompt_file}` | temp-file path (valid when `prompt_delivery="file"`) |

**`prompt_delivery`**:
- `"arg"` (default): substitute `{prompt}` into argv directly. Simple;
  fine for prompts under a few hundred KB. If the token is absent from
  argv, the prompt is discarded — your command needs to look elsewhere.
- `"file"`: write the prompt to a temp file, substitute `{prompt_file}`
  with its path. aupr removes the file after the command returns. Use
  this when your backend expects a `--prompt-file` flag or similar.

**`session_id_from` / `summary_from`** parse the command's stdout.
Forms:

- `"json:foo.bar"` — parse stdout (or any NDJSON line, last-first) as
  JSON and descend the dotted path. Strings, numbers, and bools are
  stringified.
- `"line:PREFIX"` — first line whose trimmed form starts with `PREFIX`.
  The prefix is stripped; the rest is trimmed and returned.
- `""` (or omitted) — skip; session id falls back to whatever the caller
  passed in; summary falls back to the last non-empty stdout line.

**Per-repo override** uses `[repos."owner/name"].agent_command` and
replaces the entire `[agent.command]` block for that repo. Combine with
`agent = "command"` under the same repo override to force the command
backend even when `[agent] default` is something else.

**Example: invoke an internal wrapper for one monorepo**

```toml
[agent]
default = "claude-code"

[repos."dagster-io/internal"]
agent = "command"

[repos."dagster-io/internal".agent_command]
argv            = ["dagster-ai", "address", "--pr={pr_number}", "--branch={branch}", "--prompt={prompt_file}"]
prompt_delivery = "file"
session_id_from = "json:session.id"
summary_from    = "line:DONE:"
```

## Worktree handling

aupr's resolution order for acquiring a workspace is always:

1. **Existing worktree wins.** If any linked worktree of the repo has the
   PR's head branch checked out, aupr uses it as-is. This is the LLM-tool
   interop path — if superset.sh or a human already prepared a workspace
   for this branch, aupr operates there.
2. **Fallback to `mode`:**
   - `create` (default): run `create_command` to make a new worktree at
     `path_template`.
   - `checkout`: use the main repo; swap branches with stash protection.
     Always prompts interactively; the daemon always skips-and-flags.
   - `skip`: never act without a pre-existing worktree.

### Token substitution

Tokens are literal-string replacements (no templating language). Available
in both `path_template` and each element of `create_command`:

| Token | Value |
|---|---|
| `{repo}` | repo name (e.g. `internal`) |
| `{nwo}` | `owner/name` (e.g. `dagster-io/internal`) |
| `{branch}` | PR head branch name (e.g. `eric/redis-max-conns`) |
| `{repo_path}` | absolute path of the main checkout |
| `{path}` | resolved `path_template` (only inside `create_command`) |

### `create_command` contract

- **Argv array, not shell.** TOML array form. aupr never spawns `sh -c`, so
  `{branch}` with slashes or odd characters is safe.
- **Cwd is `{repo_path}`.** That matches `git worktree add`, `wt`, and
  `superset.sh` expectations.
- **Success is verified**, in order: exit 0 → `{path}` exists →
  `git -C {path} rev-parse --abbrev-ref HEAD` equals `{branch}`. Any
  failure FLAGs the PR and leaves it untouched.
- **Output is not parsed.** stdout/stderr go to the log at DEBUG; if your
  command prints a path, we don't read it — the destination is `{path}`.

### Examples

```toml
# Default
create_command = ["git", "worktree", "add", "{path}", "{branch}"]

# Use wt so its hooks fire
create_command = ["wt", "switch", "-c", "{branch}", "--no-cd", "-y"]

# Pre-seed a superset.sh LLM session
create_command = ["superset.sh", "worktree", "--branch", "{branch}", "--path", "{path}"]

# Per-repo override: different tool, different scratch disk
[repos."dagster-io/internal"]
worktree.create_command = ["dagster-wt", "new", "{branch}"]
worktree.path_template  = "/scratch/{repo}/{branch}"
```

### `mode = "checkout"` protocol

Used when you want aupr to swap branches in the main repo rather than
create a new worktree. The sequence:

1. Interactive prompt. Daemon mode skips-and-FLAGs — never silent.
2. If dirty: `git stash push --include-untracked --message "aupr: auto-stash…"`.
3. `git fetch origin {branch}`.
4. `git checkout {branch}`.
5. `git pull --rebase origin {branch}`.
6. (agent session runs)
7. On release: `git checkout <original-branch>` then `git stash pop` if stashed.

Failure at any point: aupr best-effort-restores the original branch and
**preserves the stash**, logging the exact `git stash pop stash@{N}`
command needed to recover. aupr never runs `git stash drop` automatically.

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
