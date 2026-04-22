# prbot

PR feedback daemon — polls your open PRs on a cadence and spawns an AI
coding session in an appropriate git worktree to address new human feedback.

**Status:** planning. See [PLAN.md](./PLAN.md).

## Scope

- Authored PRs only (`--author @me`)
- Repos under `~/Dagster/` and `~/Projects/`
- Respects `wt` + `workset` + `bd` workflows
- Never merges, never force-pushes, never touches a dirty worktree
