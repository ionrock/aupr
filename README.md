# aupr

PR feedback daemon — polls your open PRs on a cadence and spawns an AI
coding session in an appropriate git worktree to address new human feedback.

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

## Quick start

```bash
make build                          # ./bin/aupr
./bin/aupr --help
./bin/aupr config init             # writes ~/.config/aupr/config.toml
./bin/aupr --dry-run once          # print the current decision table
make install                        # go install into $GOBIN
```

`--dry-run` (or `-n`, or `AUPR_DRY_RUN=1`) is the global safety flag.
Keep it on while developing or calibrating.

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
  test <repo> <pr>            preview or act on a single PR         ✅
  status                      skip list + recent activity           ✅
  skip   <repo> <pr> [reason] persistently skip                     ✅
  unskip <repo> <pr>          remove from skip list                 ✅
  config show|path|init|edit                                        ✅
  pause [reason] | resume     runtime control                       ✅
  logs [-f] [-n N] [--err]    tail launchd log files                ✅
  digest [--since DUR]        print activity summary                 ✅
  recovery                    list orphaned aupr stashes             ✅
```
