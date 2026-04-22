# Architecture

aupr is one Go binary. Everything below lives under
`github.com/ionrock/aupr/`.

## Package map

```
cmd/aupr/main.go              Entry point. Calls cli.NewApp().Run().
internal/cli/                  urfave/cli/v3 command tree.
                               Global flags (--config, --verbose, --dry-run);
                               subcommands: run, once, status, pause, resume,
                               skip, unskip, config {show,path,init,edit},
                               logs, test.
internal/config/               TOML loader + validator. Defaults() is the
                               single source of truth for built-in config.
                               Resolution order: --config → $AUPR_CONFIG →
                               ~/.config/aupr/config.toml → Defaults().
internal/logging/              slog JSON handler to stderr.
internal/execx/                Subprocess runner with context deadlines and
                               token redaction. Every external command
                               (git, gh, claude, codex, user-configured
                               create_command) must go through this so
                               tests can swap in a fake.
internal/discovery/            Walks roots, finds .git dirs, parses
                               .git/config to resolve GitHub remotes.
                               Handles linked worktrees by following the
                               gitdir pointer → commondir → config.
internal/feedback/             Shells out to `gh` to list authored open PRs
                               and fetch review comments, issue comments,
                               and review summaries. Normalizes into
                               feedback.PR and feedback.Event.
internal/policy/               The classifier. Decides AUTO / FLAG / SKIP
                               per event and rolls up per PR. See
                               docs/policy.md.
internal/state/                Persistence. Store interface with Memory
                               (tests) and SQLite (production, via
                               modernc.org/sqlite) backends. Tables:
                               pr_cursor, attempts, agent_sessions,
                               pr_skip.
internal/agent/                Pluggable agent backend. Registry.Get()
                               resolves a name to an Agent:
                                 claude-code: built-in wrapper over
                                   `claude -p --output-format=json`
                                 command:     user-configured argv with
                                   token substitution (see
                                   docs/configuration.md)
                                 codex/opencode: stubs for now
                               Per-repo [repos."x/y"].agent_command
                               lets one repo swap the argv wholesale.
internal/land/                 "Landing the plane": quality gates (auto-
                               detected from justfile/Makefile/go.mod/
                               pyproject.toml/package.json), pull-rebase,
                               push, optional gh PR comment reply.
internal/daemon/               Ticker loop wrapping scheduler.RunOnce.
                               Honors context cancellation on SIGTERM/
                               SIGINT; per-tick 30-min deadline.
internal/notify/               User-facing notifications. Log sink
                               ships; Slack/macOS to be added.
internal/worktree/             Plans and acquires a workspace for a PR.
                               Prefers existing worktrees (git worktree
                               list --porcelain); otherwise follows the
                               configured mode: create (run a pluggable
                               command), checkout (swap branches in the
                               main repo with stash protection + prompt),
                               or skip. See docs/configuration.md.
internal/scheduler/            One-tick orchestrator. Drives discovery,
                               feedback fetching, classification, and
                               table rendering. Will host the goroutine
                               worker pool once M2+ gets real work to do.
```

## One tick, end-to-end

`scheduler.RunOnce` is the whole M1 pipeline:

```
1. discovery.Walker.Walk(cfg.Daemon.Roots)
      → []Repo{Owner, Name, NWO, Path, Remote}
      (filesystem walk; no network)

2. feedback.Client.ListAuthoredOpenPRs(allowed=Repo NWO set)
      → []PR
      (one `gh search prs --author ionrock --state open --json ...`)

3. For each PR:
      feedback.Client.EnrichPR(pr)
          → fills HeadRefName, BaseRefName, Mergeable, ReviewDecision
          (one `gh pr view <n> --repo <nwo> --json ...`)

      feedback.Client.FetchEvents(pr)
          → []Event (review comments + issue comments + reviews)
          (three `gh api --paginate` calls)

      cursor := state.Store.LastSeen(repo, pr)
      decision := policy.Engine.Classify(pr, events, cursor)

      if decision.Action != SKIP:
          plan := worktree.Manager.Plan(repo, pr)
              (one `git worktree list --porcelain` per non-skipped PR;
               no mutations)

4. scheduler.renderTable(decisions, plans) → stdout
      Log line: "aupr tick: done"
```

Dry-run is threaded through as `scheduler.Options{DryRun: bool}` and — once
agent invocation lands in M3 — will gate every mutating call site. Today,
`Plan` is the only worktree-package call reachable; `Acquire` only runs
when the scheduler decides to act on a PR, which requires the agent loop.

## Process topology (forward-looking)

M3 turns `scheduler.RunOnce` into the body of a `time.Ticker` loop. The
worker-pool fan-out looks like:

```
ticker ──▶ scheduler.tick ──▶ chan FeedbackJob ──▶ N goroutines
                                                      │
                                                      ▼
                                              worktree → agent → land
```

Per-PR serialization is enforced by a `sync.Map[prKey]*sync.Mutex` so two
feedback events on the same PR can't be processed concurrently.
`context.Context` cancellation on SIGTERM lets in-flight workers finish
their current `git push` before the process exits.

None of that exists yet — it is scaffolding to keep in mind when adding
new code so we don't back ourselves into a corner.

## External tools aupr depends on

| Tool | Why | Auth state we assume |
|---|---|---|
| `gh` | PR + comment enumeration (M1), comment replies (M3) | Already authed as `ionrock` |
| `git` | `worktree list`, `stash`, `checkout`, `pull --rebase`, `push` | SSH remotes configured |
| `create_command` (configurable) | Produce a workspace when none exists. Default: `git worktree add`. Can also be `wt`, `superset.sh`, or any team-specific wrapper | argv-callable; prints nothing we need to parse |
| `claude` / `codex` / `opencode` | Agent invocation (M3+) | Authed; session IDs persisted |

All of these are invoked through `execx.Runner`, never with `os/exec`
directly, so every test can substitute a fake.
