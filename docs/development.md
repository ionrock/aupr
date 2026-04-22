# Development

## Prerequisites

- Go 1.23+
- `gh` CLI authenticated as the operator whose PRs you want to monitor
  (check with `gh auth status`)
- SSH-based git remotes on GitHub (aupr never prompts for HTTPS creds)
- For M2+: `wt`, `claude` / `codex` / `opencode`

## Layout

Standard Go module. Commands under `cmd/`, everything else under
`internal/` so nothing is importable by external modules. See
[architecture.md](architecture.md) for the package map.

## Make targets

```
make build      # → bin/aupr (stamped with `git describe`)
make install    # go install into $GOBIN (falls back to $GOPATH/bin, ~/go/bin)
make uninstall
make test       # go test ./...
make vet        # go vet ./...
make lint       # vet + golangci-lint (if installed)
make fmt        # gofmt -w
make tidy       # go mod tidy
make once       # build then `bin/aupr once`
make clean
```

`VERSION` is auto-stamped from `git describe --tags --always --dirty`
and injected via `-ldflags -X .../internal/cli.Version=...`. `aupr
--version` prints it.

## The inner loop

For read-only work (policy tuning, discovery bugs, rendering tweaks):

```
vim internal/policy/policy.go
go test ./internal/policy/ -run TestX -v
make build && ./bin/aupr --dry-run once
```

For anything that could mutate state (M2+):

```
./bin/aupr --dry-run <subcommand>   # always first
# inspect logs / git status / wt list
./bin/aupr <subcommand>              # remove guard only when confident
```

Treat `--dry-run` as the default posture during development. It is the
only thing standing between a buggy heuristic and a force-push.

## External commands & `execx`

Every subprocess in aupr must go through `internal/execx`. That gives us
one chokepoint for:

- Context cancellation on SIGTERM
- Token redaction (`ghp_`, `gho_`, `github_pat_`, `xoxb-`, `xoxp-`)
- A future test-mode `Runner` that records calls and returns canned output

Never import `os/exec` outside `internal/execx/`. `go vet` won't catch this
— code review must.

## Tests

- **Pure logic packages** (`config`, `discovery`, `policy`) have unit
  tests colocated as `*_test.go`. They are fast and network-free.
- **I/O packages** (`feedback`, `scheduler`) have no tests yet. When we
  add them, fake the `execx.Runner` — do not hit `gh` from a test.
- `go test ./...` is the pre-commit gate. If it doesn't pass, don't push.

## Logging

`internal/logging` returns a `*slog.Logger` with a JSON handler on stderr.
`--verbose` flips the level to DEBUG. Structured fields preferred over
embedded values:

```go
logger.Info("worker acting", "repo", pr.Repo, "pr", pr.Number, "kind", ev.Kind)
```

Human-facing output (the decision table, the dry-run banner) goes to
stdout via the scheduler's `Options.Output`. Don't mix.

## Adding a new subcommand

1. Add a `cmd<Name>()` function in `internal/cli/cli.go` that returns a
   `*cli.Command` with its own Flags and Action.
2. Append it to the `Commands:` slice in `NewApp`.
3. Access global flags via `c.String("config") / c.Bool("dry-run") /
   c.Bool("verbose")` — they inherit from the root command.
4. Keep the Action thin: construct the logger and config, then delegate
   to a package under `internal/`. CLI code is plumbing, not logic.

## Release

There is no release automation yet. When we need one:

- Tag: `git tag vX.Y.Z && git push --tags`
- `make build` produces a version-stamped binary
- `make install` is the local-machine install path today
- launchd plist + install script will live under `scripts/` (M3)

## Milestones (from PLAN.md)

| Milestone | State | Scope |
|---|---|---|
| M0 | ✅ shipped | go mod, cobra→urfave CLI, config, Makefile |
| M1 | ✅ shipped | discovery, feedback, policy, scheduler, `aupr once` |
| M2 | planned | `internal/worktree/`, `internal/agent/`, still dry-run |
| M3 | planned | writes, state (sqlite), launchd, audit trail |
| M4 | planned | per-repo tuning, Slack digest, status CLI polish |
