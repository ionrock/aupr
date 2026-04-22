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
                               (git, gh, wt, claude, codex) must go through
                               this so tests can swap in a fake.
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
internal/state/                Cursor store: last acted-on event ID per PR.
                               M1 has an in-memory implementation; M3 swaps
                               in sqlite behind the same Store interface.
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

4. scheduler.renderTable(decisions) → stdout
      Log line: "aupr tick: done"
```

Dry-run is threaded through as `scheduler.Options{DryRun: bool}` and — once
M2 introduces writes — will gate every mutating call site. Today, nothing
mutates, so dry-run just prints the banner and is otherwise a no-op.

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
| `git` | Worktree state checks, rebase, push (M2+) | SSH remotes |
| `wt` | Worktree acquisition (M2+) | `wt list --format=json` available |
| `claude` / `codex` / `opencode` | Agent invocation (M2+) | Authed; session IDs persisted |
| `bd` | `bd sync` during "landing the plane" (M3) | Per-repo `.bd/` |

All of these are invoked through `execx.Runner`, never with `os/exec`
directly, so every test can substitute a fake.
