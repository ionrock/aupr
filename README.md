# aupr

PR feedback daemon — polls your open PRs on a cadence and spawns an AI
coding session in an appropriate git worktree to address new human feedback.

## Why "aupr"?

Short for **au pair** — a live-in helper who watches over the kids while
you're busy. `aupr` does the same for your PRs: it keeps an eye on them,
handles the small stuff (nits, typos, review comments), and hands the
tricky decisions back to you. Think of it less as an autonomous agent
and more as an attentive nanny for your open pull requests.

**Status:** M1–M5 shipped. End-to-end pipeline behind `--dry-run` with
pause/resume, Slack + macOS notifiers, pluggable agent backends, crash-
recovery stash scan, cost tracking, and daily digest. See
[PLAN.md](./PLAN.md) for roadmap and
[docs/operations.md](./docs/operations.md) for running the daemon.

## Scope

- Authored PRs only (`--author @me`)
- Repos under `~/Dagster/` and `~/Projects/` (configurable)
- Respects `wt` + `workset` workflows
- Never merges, never force-pushes, never touches a dirty worktree

## Install

Requires Go 1.22+, `git`, and the [`gh`](https://cli.github.com/) CLI
(authenticated: `gh auth login`).

```bash
# from a clone
git clone https://github.com/ionrock/aupr.git
cd aupr
make install                        # go install into $GOBIN

# or build locally without installing
make build                          # ./bin/aupr

# or directly via go
go install github.com/ionrock/aupr/cmd/aupr@latest
```

Make sure `$GOBIN` (or `$(go env GOPATH)/bin`) is on your `PATH`.

## Quick start

```bash
aupr config init                    # writes ~/.config/aupr/config.toml
$EDITOR ~/.config/aupr/config.toml  # point it at your repo roots, pick an agent
aupr --dry-run once                 # print the current decision table, no side effects
```

`--dry-run` (or `-n`, or `AUPR_DRY_RUN=1`) is the global safety flag.
Keep it on while developing or calibrating.

## Examples

```bash
# See what aupr would do right now, across all watched repos.
aupr --dry-run once

# Run the ticker loop in the foreground (Ctrl-C to stop).
aupr run

# Preview or act on one of your own PRs.
aupr --dry-run test dagster-io/dagster 12345
aupr test dagster-io/dagster 12345

# Read-only: run the agent on someone else's PR, show the diff, don't commit.
aupr inspect dagster-io/dagster 12345

# Temporarily stop processing (survives restarts) and resume later.
aupr pause "on vacation"
aupr resume

# Skip a specific PR forever (or until you unskip it).
aupr skip dagster-io/dagster 12345 "WIP, handling manually"
aupr unskip dagster-io/dagster 12345

# What has it been up to?
aupr status
aupr digest --since 24h
aupr logs -f              # tail launchd logs when running as a service
aupr recovery             # list orphaned stashes from crashed runs
```

For running `aupr` as a background service (launchd on macOS, systemd on
Linux), see [docs/operations.md](docs/operations.md).

## Documentation

| | |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Package map + one-tick data flow |
| [docs/configuration.md](docs/configuration.md) | TOML schema, resolution order, adding config fields |
| [**docs/policy.md**](docs/policy.md) | **How PR-feedback classification works and how to change it** |
| [docs/development.md](docs/development.md) | Make targets, test conventions, inner loop |
| [PLAN.md](PLAN.md) | Design rationale, milestones, open questions |

## Commands

```
aupr [--dry-run] [--verbose] [--config PATH] <command>

  run                         ticker loop (daemon-friendly)        ✅
  once                        one tick + decision table             ✅
  test <repo> <pr>            preview or act on your own PR         ✅
  inspect <repo> <pr>         run agent on ANY PR; show diff only   ✅
  status                      skip list + recent activity           ✅
  skip   <repo> <pr> [reason] persistently skip                     ✅
  unskip <repo> <pr>          remove from skip list                 ✅
  config show|path|init|edit                                        ✅
  pause [reason] | resume     runtime control                       ✅
  logs [-f] [-n N] [--err]    tail launchd log files                ✅
  digest [--since DUR]        print activity summary                 ✅
  recovery                    list orphaned aupr stashes             ✅
```
