# aupr docs

Reference material for developers working on aupr itself. User-facing
installation and CLI usage live in the top-level [README](../README.md).
The high-level design rationale is in [PLAN.md](../PLAN.md) — these docs
assume you've skimmed it.

| Document | When to read |
|---|---|
| [architecture.md](architecture.md) | You want the package-by-package map and the data flow through one `aupr once` tick. |
| [configuration.md](configuration.md) | You're adding a config field, tuning defaults, or debugging `config show` output. |
| [policy.md](policy.md) | **You're changing how PR feedback is classified.** Start here. |
| [operations.md](operations.md) | **You want to run aupr as a daemon safely.** Start here. |
| [development.md](development.md) | You want to build, test, vet, or cut a release. |
