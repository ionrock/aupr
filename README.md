# aupr

PR feedback daemon — polls your open PRs on a cadence and spawns an AI
coding session in an appropriate git worktree to address new human feedback.

**Status:** M1 shipped (read-only scout). See [PLAN.md](./PLAN.md) for
roadmap and [docs/](./docs/) for reference.

## Scope

- Authored PRs only (`--author @me`)
- Repos under `~/Dagster/` and `~/Projects/` (configurable)
- Respects `wt` + `workset` + `bd` workflows
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

  run         start the daemon in the foreground            (M3)
  once        one tick of discovery + decision              (M1 ✅)
  status      show daemon / PR status                       (M3)
  pause       stop acting (keep polling)                    (M3)
  resume      resume acting                                 (M3)
  skip   <pr> never act on this PR                          (M3)
  unskip <pr> remove from skip list                         (M3)
  config      show | path | init | edit                     (M1 ✅)
  logs        print / tail the daemon log                   (M3)
  test   <pr> preview action for a single PR                (M2)
```
