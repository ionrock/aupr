# Operations

How to actually run aupr as a daemon and iterate on it safely.

## Layered safety model

Every mutation goes through three gates, in order. Any one of them saying
"no" stops the mutation.

1. **Dry-run flag.** `--dry-run` / `-n` / `$AUPR_DRY_RUN` / `[agent]
   dry_run = true`. Any one of these triggers dry-run; the effective value
   is their logical OR. Dry-run short-circuits before any agent invocation,
   git write, git push, or PR comment.
2. **Worktree mode.** `[worktree] mode` decides how aupr gets a workspace.
   `create` (default) never touches the main repo. `checkout` always
   prompts (in `aupr once`/`test`) or skips (in `aupr run`).
3. **Circuit breaker.** Three consecutive failed attempts on the same PR
   auto-adds it to the skip list. Manual unskip required to try again.

## The `aupr test` iteration loop

This is the path for calibrating policy and verifying config without
touching any other PR.

```bash
# Preview one PR end-to-end, dry-run (default):
aupr test dagster-io/internal 22117

# Same, with full verbose logs:
aupr -v test dagster-io/internal 22117

# Actually act on that one PR (real git push, real comment reply):
aupr --dry-run=false test dagster-io/internal 22117
```

`aupr test` is always interactive (so if `mode = "checkout"` would cause
a branch swap, you get the prompt) and always scoped to a single PR.

## The `aupr run` daemon loop

```bash
# Foreground, real pushes:
aupr run

# Foreground, safe (recommended for first few days):
aupr --dry-run run

# Background via launchd (writes a plist, defaults to --dry-run):
scripts/install-launchd.sh

# Tear it down:
scripts/uninstall-launchd.sh
```

### Pre-flight checklist before removing `--dry-run`

1. Watch 5–10 tick cycles in dry-run. Look at `~/.local/state/aupr/state.db`
   (via `sqlite3` if needed):
   ```sql
   SELECT repo, pr_number, outcome, summary, finished_at FROM attempts
   ORDER BY id DESC LIMIT 20;
   ```
2. Confirm the WORKSPACE column of `aupr --dry-run once` shows sensible
   paths for every AUTO PR.
3. Pick one low-stakes PR and run `aupr --dry-run=false test <repo> <n>`
   manually. Verify the commit on GitHub before removing `--dry-run` from
   the launchd plist.

### Editing the launchd plist

The install script writes `~/Library/LaunchAgents/io.ionrock.aupr.plist`
with `--dry-run` by default. To enable writes:

```
# Edit the plist: remove the <string>--dry-run</string> entry.
$EDITOR ~/Library/LaunchAgents/io.ionrock.aupr.plist

# Reload:
launchctl unload ~/Library/LaunchAgents/io.ionrock.aupr.plist
launchctl load   ~/Library/LaunchAgents/io.ionrock.aupr.plist
```

Or re-run `scripts/install-launchd.sh` with a modified script.

## Daily digest

When `[notify] summary_cadence = "daily"`:

- Per-event success ("acted") notifications are suppressed. Errors,
  circuit-breaker trips, and recovery-stash alerts still fire
  immediately.
- On each tick, aupr checks the `last_digest_at` setting; if >= 24h
  have passed (or it has never run), it assembles a digest of the last
  24h and sends it via the notifier (Slack/macOS/log).
- Digest content: attempt counts by outcome, per-repo activity, up to
  10 error lines, and any tracked recovery stashes.

At any time you can run `aupr digest` to see the summary on stdout
without advancing the timestamp; `aupr digest --since 72h` widens
the window.

Cadence values:

| `summary_cadence` | Successes | Errors | Daily digest |
|---|---|---|---|
| `per_action` (default) | notify each | notify each | no |
| `daily` | suppressed | notify each | yes |
| `never` | suppressed | notify each | no |

## Recovery stashes

An interrupted `mode = "checkout"` protocol (SIGKILL mid-swap, crash)
leaves a stash behind with the message `aupr: auto-stash <orig>-><target>`.

Every tick aupr scans all discovered repos for these and:

1. Records new ones in `recovery_stashes` (SQLite).
2. Notifies on first sight (Slack + macOS + log) with the exact
   `git stash pop` recovery command.
3. Forgets entries whose stashes have disappeared (you popped them).

Inspect outstanding stashes:

```bash
aupr recovery
```

## Cost tracking

If the agent backend reports token usage and cost, aupr persists them
on each attempt and aggregates them in the digest.

- **claude-code** parses `total_cost_usd` and `usage.{input,output}_tokens`
  out of `claude -p --output-format=json` natively.
- **command** backend: set `input_tokens_from`, `output_tokens_from`,
  `cost_usd_from` in `[agent.command]`, same form as `session_id_from`
  (`json:path` or `line:prefix`).

Report:

```bash
aupr digest               # shows tokens + cost for the last 24h
sqlite3 ~/.local/state/aupr/state.db \
  "SELECT date(finished_at, 'unixepoch') AS day, sum(input_tokens), sum(output_tokens), printf('$%.2f', sum(cost_usd)) FROM attempts GROUP BY day ORDER BY day DESC;"
```

## State database

Default path: `~/.local/state/aupr/state.db` (SQLite, WAL mode).

| Table | What it holds |
|---|---|
| `pr_cursor` | Last acted-on event ID per (repo, pr_number). Advanced only on successful non-dry-run actions. |
| `attempts` | One row per act attempt — start/finish timestamps, agent, outcome, summary, commit sha, error. Circuit breaker reads the last 3. |
| `agent_sessions` | Session IDs per (repo, pr_number, agent). Used to `--resume` claude sessions across feedback events on the same PR. |
| `pr_skip` | User-maintained skip list (`aupr skip`/`unskip`) and circuit-breaker entries. |
| `recovery_stashes` | Dedup table for orphaned `aupr: auto-stash` stashes; drives `aupr recovery` and first-sight notifications. |
| `daemon_settings` | Key/value: `pause_reason`, `last_digest_at`, future scheduler state. |

Useful queries while iterating:

```sql
-- How is calibration going?
SELECT outcome, count(*) FROM attempts GROUP BY outcome;

-- Which PRs have aupr been touching?
SELECT repo, pr_number, max(finished_at) AS last
FROM attempts GROUP BY repo, pr_number ORDER BY last DESC;

-- What's currently auto-skipped by the circuit breaker?
SELECT * FROM pr_skip WHERE reason LIKE 'circuit breaker%';
```

## CLI subcommands

```
aupr [--dry-run] [--verbose] [--config PATH] <command>

  run                         ticker loop (daemon-friendly)
  once                        single tick + decision table
  test <repo> <pr>            one PR through the full pipeline
                              (dry-run by default; pass --dry-run=false
                              to actually push)
  status [repo pr]            summary, or detail for one PR
  pause [reason...]           suspend the act-loop (polling continues)
  resume                      resume the act-loop
  skip <repo> <pr> [reason]   add to persistent skip list
  unskip <repo> <pr>          remove from skip list
  logs [-f] [-n N] [--err]    tail launchd log files
  digest [--since DUR]        print a summary (default: last 24h)
  recovery                    list aupr-authored stashes left behind
  config show|path|init|edit
```

### Pause / resume

`aupr pause [reason]` writes a flag to the state DB. The daemon's *next
tick* observes it (paused state is per-DB, not per-process), renders a
`[paused] act-loop suspended` banner, and skips every AUTO-action — but
continues polling, classifying, and displaying the decision table.
`aupr resume` clears the flag. Safe to use at any time; no daemon
restart needed.

### Slack notifications

```toml
[notify]
slack_enabled      = true
slack_webhook_url  = "https://hooks.slack.com/services/T0.../B0.../abc"
```

Any incoming-webhook URL works (create one via Slack app settings). aupr
routes `error`, `circuit-breaker`, `flagged`, and `acted` events; other
kinds are suppressed to avoid notification fatigue. If `slack_enabled`
is true but the URL is empty, aupr logs a warning and falls back to
log-only.

### macOS notifications

```toml
[notify]
macos_notifications = true
```

Fires via `osascript -e 'display notification ...'` for `error`,
`circuit-breaker`, and `flagged` events only. Successes are silent.
Requires the invoking process (or launchd) to have Notification Center
permissions on first use.

## Logs

- Structured JSON to stderr (or the launchd log files once installed).
- `--verbose` / `-v` promotes to DEBUG.
- Launchd log paths: `~/.local/state/aupr/aupr.out.log` and `aupr.err.log`.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `gh search prs` times out | Network or gh auth | `gh auth status`; re-run `gh auth refresh` |
| `agent: unknown backend` | Wrong `[agent] default` | Only `claude-code` is wired in M3. Set `default = "claude-code"`. |
| `no new commits to push` | Agent didn't commit | Check attempt row's `summary`/`output` in state.db; tune prompt |
| `land: push: Updates were rejected` | Branch diverged during action | aupr leaves the workspace dirty; rebase manually |
| Circuit-breaker auto-skipped a real PR | 3 unrelated failures in a row | `aupr unskip <repo> <pr>`; investigate the `attempts` rows |
