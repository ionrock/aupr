# PR Feedback Daemon — Plan

**Project name (working):** `aupr` at `~/Projects/aupr/`
**Status:** PLAN — awaiting Eric's review before any code is written.
**Author:** CTO Assistant, 2026-04-22

---

## 1. What this is

A local daemon that polls Eric's open PRs on a cadence, detects new human
feedback (review comments, PR comments, requested changes, failing CI with
reviewer flags), and spawns an AI coding session in an appropriate git worktree
to address it. On completion the agent pushes commits and replies to the
comment thread.

The daemon does NOT merge, does NOT force-push over review state, does NOT
touch PRs that are approved/blocked/draft. It is a reviewer-feedback triager
for Eric's open work — not an autonomous merger.

## 2. Scope (explicitly)

### In-scope repositories (initial)
- Everything under `~/Dagster/` (internal, dagster, dagster-compass, dagster-support, etc.)
- Everything under `~/Projects/` (assistant, autotriage, aws-sdk-*, workset, dev-success-*, etc.)

Daemon walks these two roots at startup and on a sparse re-scan cadence, finds
git repos, and resolves the GitHub remote for each. Repos without a GitHub
remote are ignored.

### In-scope feedback types
- Unresolved review comments (line-level)
- General PR comments mentioning Eric (@ionrock) or posted after his last commit
- Requested-changes reviews
- Reviewer-reported CI-failure comments (heuristic match)

### OUT of scope (v1)
- Repos outside `~/Dagster/` and `~/Projects/`
- PRs Eric didn't author
- Auto-merging
- Writing NEW code features (only addressing feedback on existing PRs)
- Responding to Dependabot / Renovate / security bots
- Force-pushes that overwrite reviewer suggestions not yet addressed

## 3. Eric's existing workflow (what the daemon must respect)

Observed from `~/Projects/workset/` and tooling in place:

- **`wt` CLI** (`/opt/homebrew/bin/wt`) is the canonical worktree tool. Supports
  `wt switch -c <branch> --no-cd`, `wt list --format=json`, `wt remove`,
  `wt merge`, hooks, and a config system. This is NOT plain `git worktree`.
- **`workset.el`** is the Emacs wrapper. Branches are prefixed `eric/`, worktrees
  live under `~/.workset/` by default, and vterm sessions are named
  `*workset: %r/%t<%n>*`.
- **`bd` (beads)** tracks issues per-project. Per `workset/AGENTS.md`:
  `bd ready`, `bd show`, `bd update --status in_progress`, `bd close`, `bd sync`.
- **"Landing the plane"** is a mandatory session-close ritual:
  `git pull --rebase && bd sync && git push`. Work is NOT complete until
  push succeeds. Daemon must follow this.
- **Coding agents already installed:** Claude Code, Codex, OpenCode. Eric has
  Hermes skills for each. `claude` auth was recently refreshed (OAuth),
  `codex` is authed.
- **`bk` CLI** (Buildkite) just installed and authed to the `dagster` org.
  Useful for detecting failing CI on a PR.
- **`gh` CLI** is authed as `ionrock` with SSH for git operations.
- **Existing tool — `autotriage`** lives at `~/Projects/autotriage/`. Worth
  checking before reinventing anything related.

## 4. Architecture

### 4.1 Process shape

Single Python daemon launched by `launchctl` (reusing the pattern in
`~/Projects/assistant/scheduler/`). One process, async loop, no web server.

```
aupr (launchd)
  └─ tick every N minutes
      ├─ refresh PR list from gh (--author @me, --state open)
      ├─ for each PR:
      │    compute feedback delta since last run
      │    decide: ignore | queue | skip-with-reason
      │    queue → spawn worker (bounded concurrency)
      └─ worker:
            acquire worktree (reuse if exists, else create via wt)
            acquire agent session (reuse if compatible, else spawn)
            pull + rebase
            invoke agent with structured prompt
            validate result (lint, test, sanity check)
            commit + push
            reply in comment thread(s)
            release resources per config
```

### 4.2 Repo layout (proposed)

```
~/Projects/aupr/
├── README.md
├── pyproject.toml           # uv-managed
├── aupr/
│   ├── __init__.py
│   ├── cli.py               # `aupr run|once|status|pause|resume|config`
│   ├── daemon.py            # main async loop
│   ├── config.py            # pydantic settings, ~/.config/aupr/config.toml
│   ├── discovery.py         # walk ~/Dagster/ and ~/Projects/, find git+gh remotes
│   ├── feedback.py          # gh GraphQL → normalized "feedback events"
│   ├── scheduler.py         # decide which PRs to act on + bounded concurrency
│   ├── worktree.py          # wt CLI integration (reuse-or-create)
│   ├── agent_session.py     # claude / codex / opencode session management
│   ├── policies.py          # what's safe to auto-address vs flag for human
│   ├── land.py              # "landing the plane" — rebase, push, comment reply
│   ├── state.py             # sqlite cursor: last-seen comment-id per PR
│   └── notify.py            # Slack/macOS/log notifications
├── scripts/
│   ├── install-launchd.sh
│   └── uninstall-launchd.sh
└── tests/
```

### 4.3 State

SQLite at `~/.local/state/aupr/state.db` with tables:
- `pr_cursor` — (repo, pr_number) → last_seen_comment_id, last_acted_at, last_commit_pushed
- `attempts` — (pr_number, feedback_id, started_at, agent, outcome, notes)
- `worktree_leases` — (worktree_path, pr_number, acquired_at, released_at)
- `agent_sessions` — (session_id, agent, worktree_path, last_used_at)

### 4.4 Worktree reuse strategy (configurable)

Config knob: `worktree_reuse_policy`
- `per_pr` (default): one worktree per PR branch for the life of the PR.
  Path convention `~/.workset/<repo>/<branch>`. Reuse on subsequent feedback.
- `per_repo_pool`: N worktrees per repo, assigned round-robin. Cheaper on
  disk for low-touch repos.
- `ephemeral`: fresh worktree per feedback event. Safest, slowest.

Reuse rule:
1. Check `wt list --format=json` for an existing worktree on the PR's branch.
2. If yes, and `git status` is clean, use it. Run `git pull --rebase origin <branch>`.
3. If yes but dirty (from a human session), DO NOT touch — log and skip.
4. If no, `wt switch -c <branch> --no-cd -y`.

### 4.5 LLM session reuse strategy (configurable)

Config knob: `agent_session_reuse_policy`
- `per_pr` (default): resume the same Claude session ID for the life of a PR
  (stored in state). Carries history of prior feedback addressed.
- `fresh`: new session per feedback event. Cleanest context, highest cost.
- `per_repo`: shared session per repo (discouraged — context bleed).

Session-resumption mechanics:
- **claude-code:** `claude -p --resume <session-id>` (per our claude-code skill).
- **codex:** `codex exec resume --last` or `resume <session-id>`.
- **opencode:** per its documented resume flag.

The daemon picks the agent based on repo configuration or per-PR override;
default is Claude Code since Eric has that authed and fresh.

### 4.6 Decision policy (what gets auto-addressed vs flagged)

Safe to auto-address:
- Typo/phrasing fixes in docs
- Rename/formatting/style nit comments
- "Add a test for X" when X is clearly scoped
- "Extract this to a variable / function" refactors
- Failing CI that matches a known-flaky pattern (just re-run)

Flag for human (notify, don't act):
- Architectural questions ("why did you do it this way?")
- Comments containing "revert", "remove this", "I disagree"
- Changes that would touch security/auth code paths
- Requested-changes reviews where the ask isn't a concrete code change
- Any comment whose text length exceeds a threshold (suggests nuance)
- Any PR blocked on merge conflicts (daemon resolves rebase conflicts only
  if the diff is strictly mechanical; otherwise flag)

Implementation: `policies.py` returns an enum `{AUTO, FLAG, SKIP}` with a
reason. Every decision is logged with its reason so Eric can tune heuristics.

### 4.7 "Landing the plane" integration

After a successful agent pass:
1. Run repo's quality gates (detect from `justfile` / `Makefile` / `package.json`)
2. `git pull --rebase` on the PR branch
3. `bd sync` if the repo has a `.bd/` dir
4. `git push`
5. Verify `git status` shows up-to-date-with-origin
6. Reply to originating comment thread with a link to the new commit and a
   short summary

If any step fails, daemon stops, leaves the worktree dirty, and notifies Eric.

## 5. Configuration

Default config at `~/.config/aupr/config.toml`:

```toml
[daemon]
tick_minutes = 15
roots = ["~/Dagster", "~/Projects"]
github_user = "ionrock"
bounded_concurrency = 2
log_path = "~/.local/state/aupr/aupr.log"

[worktree]
reuse_policy = "per_pr"       # per_pr | per_repo_pool | ephemeral
base_path = "~/.workset"
branch_prefix = "eric/"

[agent]
default = "claude-code"       # claude-code | codex | opencode
session_reuse_policy = "per_pr"  # per_pr | fresh | per_repo
max_turns_per_feedback = 15
dry_run = false

[policy]
auto_address_types = ["typo", "style", "rename", "add-test", "flaky-ci"]
flag_but_dont_act = ["architectural", "revert", "security-touching"]
skip = ["draft", "approved", "dependabot"]
max_feedback_age_days = 14

[notify]
slack_enabled = true
slack_channel = "dm-ionrock"    # Eric's self-DM for log
macos_notifications = false
summary_cadence = "daily"       # never | per_action | daily

[repos."dagster-io/internal"]   # per-repo overrides
agent = "codex"                 # override default for this repo
bounded_concurrency = 1         # only one concurrent action in this heavy repo
quality_gates = ["just check"]  # explicit command list

[repos."ionrock/workset"]
agent = "claude-code"
quality_gates = ["cask eval", "emacs -batch -l ert -l test/workset-test.el -f ert-run-tests-batch-and-exit"]
```

## 6. CLI UX

```
aupr run                 # start daemon (fg)
aupr once                # one tick, then exit (for cron / debugging)
aupr status              # show active PRs, last-seen cursor, worktree leases
aupr status <pr>         # detail for one PR
aupr pause               # stop acting (daemon keeps running, just skips action)
aupr resume
aupr skip <pr>           # never act on this PR
aupr unskip <pr>
aupr config show
aupr config edit
aupr logs [--follow]
aupr test <pr> --dry-run # preview what the daemon would do
```

## 7. Safety (non-negotiable)

- **Never force-push.** Never rewrite history on a remote branch.
- **Never merge.** That's Eric's decision.
- **Never touch a dirty worktree.**
- **Never act on a PR in "draft" status.**
- **Never act on a PR with approved review state unless new feedback is posted after approval.**
- **Never act twice on the same feedback id.** State store prevents this.
- **Always leave a visible audit trail.** Every action commits with a
  descriptive message prefixed `aupr:` AND posts a reply in the comment
  thread identifying itself as the daemon.
- **Circuit breaker.** If 3 consecutive failures on the same PR, mark it
  `skip` automatically and notify Eric.
- **Dry-run mode.** Config flag and CLI flag. No mutations, just print what
  would happen.

## 8. Observability

- Structured JSON log per tick + per worker-action
- Daily digest sent to Eric's self-DM on Slack: actions taken, flags for
  review, skipped items
- `aupr status` renders a live table
- Metrics rolled into `~/Projects/assistant/cost-pulse/` if we want cost
  tracking down the line (out of v1 scope)

## 9. Milestones

**M1 — Read-only scout (1 day)**
- Discovery walks `~/Dagster/` + `~/Projects/`, resolves GH remotes
- `gh` polling for Eric's open PRs with normalized feedback events
- `aupr once` prints a decision table ("would act | would flag | would skip" per PR)
- No mutations, no worktrees touched

**M2 — Worktree + agent spawn, dry-run (1 day)**
- `worktree.py` reuses via `wt list --format=json` + `wt switch -c`
- `agent_session.py` invokes claude-code in print mode with a structured prompt
- Still --dry-run by default — prints diff but does not commit

**M3 — Full loop, opt-in writes (1-2 days)**
- Commit + push + reply enabled behind `--write` flag
- State DB, circuit breaker, audit trail
- launchd install script

**M4 — Polish (as needed)**
- Per-repo config, session reuse, policy tuning based on real traffic
- Slack digest, status CLI niceties

## 10. Open questions for Eric

0. **Go vs Python?** This plan now targets Go. Implications:
   - **Pros:** single static binary (clean launchd story, no venv drift,
     trivial `go install`); first-class concurrency primitives for the
     worker pool; stdlib `log/slog` covers structured logging without deps;
     fast startup matters for `aupr once` cron-style invocation; the
     existing `wt` CLI is Go, so style/idioms line up.
   - **Cons:** none of Eric's other daemons (`assistant/scheduler/`,
     `autotriage`) are Go — loses code sharing if those are Python; GitHub
     GraphQL ergonomics are better in Python; LLM/agent SDKs are
     Python-first, though we mostly shell out to `claude`/`codex` anyway so
     that's a wash; TOML+pydantic-style validation is nicer than koanf.
   - **Net:** Go is a good fit because aupr is mostly process orchestration
     (subprocess, sqlite, HTTP) and we want a reliable long-running daemon.
     The one real loss is shared code with `autotriage` — revisit after
     reading it (see §11).
1. **Name?** `aupr` is a placeholder. Alternatives: `prwatch`, `reviewbot`,
   `autopilot`, `lander`, `plane` (since the workflow talks about "landing").
2. **Cadence?** 15 min default feels right for PRs but might be noisy. Want
   different cadences per repo?
3. **Agent default?** Claude Code (recently reauthed, has workflow skills) or
   Codex (no OAuth wobble, handles large diffs well)? My recommendation:
   Claude Code default, Codex for repos where we've seen OAuth issues.
4. **Slack digest or self-DM per action?** I'd default to daily digest + DM
   on flags, not per-action.
5. **Should the daemon file `bd` issues for flagged-but-not-addressed
   feedback?** Fits the workset workflow — you'd see them in `bd ready`.
   Slight complexity bump.
6. **Should `autotriage` be extended instead of a new project?** Haven't read
   it yet; if it already does discovery + queue, worth layering on.
7. **Review-comment reply wording?** Do you want a fixed template, or should
   the agent draft per-comment? I lean fixed template ("addressed in <sha>:
   <one-line summary>") for auditability.
8. **What's the bar for "safe to auto-act"?** My proposal in §4.6 is
   conservative. Easy to loosen later; hard to claw back trust if we start
   too aggressive.
9. **Does anything here conflict with how `workset.el` expects to own
   worktrees?** If workset is the source of truth, aupr should be a client
   that asks workset for worktrees (e.g. via a small emacsclient call or a
   shared registry file).

## 11. Non-code prep I'll do before M1 if approved

- Read `~/Projects/autotriage/` end-to-end
- Read `~/Projects/workset/` to understand hook system + how it persists state
- Check whether `wt` has a hook we can register so it notifies aupr when
  humans change worktrees (avoid fighting humans for a dirty tree)
- Scan Eric's open PRs for typical feedback shapes — calibrate policy heuristics

---

**Review ask:** tell me what to change. In particular questions 1-9 in §10.
Once aligned I'll implement M1 end-to-end for you to kick the tires on.
