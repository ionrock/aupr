# PR feedback handling

This document is the full lifecycle of a reviewer comment, from the `gh`
API to an `AUTO` / `FLAG` / `SKIP` decision, and **exactly how to tune
that pipeline**. It's the doc you open when you look at a `aupr once`
table and think "why did it classify that PR that way?"

- [Data flow](#data-flow)
- [Data model](#data-model)
- [The decision pipeline](#the-decision-pipeline)
- [Where to make each kind of change](#where-to-make-each-kind-of-change)
- [Step-by-step: adding a new classification rule](#step-by-step-adding-a-new-classification-rule)
- [Step-by-step: ignoring a new kind of author](#step-by-step-ignoring-a-new-kind-of-author)
- [Step-by-step: adding a new hard skip](#step-by-step-adding-a-new-hard-skip)
- [Step-by-step: adding a new feedback source](#step-by-step-adding-a-new-feedback-source)
- [Testing your changes](#testing-your-changes)
- [Calibration loop](#calibration-loop)
- [Design constraints](#design-constraints)

## Data flow

```
 gh CLI                     feedback package                    policy package                scheduler
 ──────                     ────────────────                    ──────────────                ─────────
 gh search prs ──▶ ListAuthoredOpenPRs ──▶ []PR
                                            │
                                            ▼
 gh pr view   ──▶ EnrichPR(pr) ─────────▶ PR (w/ Mergeable, ReviewDecision, HeadRef)
                                            │
                                            ▼
 gh api pulls/N/comments ──┐
 gh api issues/N/comments  ├▶ FetchEvents(pr) ──▶ []Event
 gh api pulls/N/reviews  ──┘                        │
                                                    ▼
                               state.LastSeen(repo, n) ─▶ cursor
                                                    │
                                                    ▼
                                   Engine.Classify(pr, events, cursor)
                                                    │
                                                    ▼
                                              Decision{Action, Reason,
                                                       NewEvents,
                                                       Classifications}
                                                    │
                                                    ▼
                                           renderTable(...)
```

Every arrow above is covered by one function; if you know which arrow you
want to change, you know which function to touch.

## Data model

All in `internal/feedback/gh.go` and `internal/policy/policy.go`.

```go
// feedback package
type PR struct {
    Repo, Title, URL               string
    Number                         int
    HeadRefName, BaseRefName       string
    IsDraft                        bool
    Mergeable                      string // MERGEABLE / CONFLICTING / UNKNOWN
    ReviewDecision                 string // APPROVED / CHANGES_REQUESTED / REVIEW_REQUIRED / ""
    CreatedAt, UpdatedAt           time.Time
    Author                         string
}

type Kind string
const (
    KindReviewComment Kind = "review_comment" // line-level
    KindIssueComment  Kind = "issue_comment"  // general PR conversation
    KindReview        Kind = "review"         // approve / request changes / comment summary
)

type Event struct {
    ID        string    // GitHub node_id — the unique identity we store in the cursor
    PR        PR
    Kind      Kind
    Author    string
    Body      string
    URL       string
    CreatedAt time.Time
    Path      string // review comments only
    Line      int    // review comments only
    State     string // reviews only: APPROVED / CHANGES_REQUESTED / COMMENTED
}

// policy package
type Action string
const (
    ActAuto Action = "AUTO"
    ActFlag Action = "FLAG"
    ActSkip Action = "SKIP"
)

type EventClass struct { Event feedback.Event; Action Action; Reason string }

type Decision struct {
    PR              feedback.PR
    Action          Action       // roll-up over all NewEvents (most conservative wins)
    Reason          string       // human-readable explanation
    NewEvents       []feedback.Event
    Classifications []EventClass // one per NewEvent
}
```

**Identity:** `Event.ID` is the GitHub `node_id` — stable, globally unique,
and what the state store uses as a cursor. Never use a composite key; if a
comment is edited the body changes but the node_id does not.

## The decision pipeline

Implemented in `policy.Engine.Classify(pr, events, lastSeenID)`. The order
matters:

### 1. Hard skips on the PR itself

In order:

1. `pr.IsDraft` → `SKIP "draft PR"`
2. `pr.ReviewDecision == "APPROVED"` **and** no non-review event after the
   latest approval → `SKIP "approved with no new post-approval feedback"`
3. Repo is marked `skip = true` in config → `SKIP "repo skipped in config"`
4. `pr.Author != cfg.github_user` → `SKIP "PR not authored by operator"`

These run before any event-level work. They short-circuit.

### 2. Event filtering (build `NewEvents`)

For each event, drop it if **any** of the following are true:

- It is at or before the cursor `lastSeenID` (we've already seen/acted on it).
- Its author is the operator (self-talk).
- `isBot(author)` matches — currently `[bot]` suffix, `dependabot`,
  `renovate`, `github-actions`.
- `CreatedAt < now - max_feedback_age_days`.

If zero events survive → `SKIP "no new actionable feedback"`.

### 3. Per-event classification (`classifyEvent`)

For each surviving event we produce an `EventClass{Action, Reason}`. Rules
are evaluated top-down, **first match wins**:

1. **Review summaries** (`Kind == KindReview`)
   - `State == APPROVED` and empty body → `SKIP "approval with empty body"`
   - `State == CHANGES_REQUESTED` → `FLAG "changes requested (needs human judgment)"`
2. **Discussion markers** (`revert`, `remove this`, `i disagree`,
   `why did you`, `why do we`) → `FLAG "discussion/disagreement"`
3. **Security-adjacent** (`security`, `auth`, `token`, `secret`, `password`)
   → `FLAG "security-adjacent"`
4. **Long body** (`len(Body) > 600`) → `FLAG "long comment suggests nuance"`
5. **Auto candidates** (first match wins):
   - `typo` / `spelling` / `grammar` → `AUTO "typo/grammar fix"`
   - `rename` / `extract` / `format` / `lint` / `style` / `nit:` / `nit ` →
     `AUTO "style/rename/format"`
   - `add a test` / `add test` / `missing test` / `write a test` →
     `AUTO "add-test"`
   - `flaky` / `re-run ci` / `retrigger` / `restart build` →
     `AUTO "likely flaky CI"`
6. **Default** → `FLAG "unclassified; surfacing for review"`

### 4. Roll-up

The PR's `Action` is the **most conservative** of any event's action:

```
rank: AUTO < FLAG < SKIP
PR.Action = max(rank, for ev in NewEvents)
```

So one `FLAG` contaminates the whole PR. That's intentional — if any
comment needs human judgment, the whole PR needs it.

`Decision.Reason` concatenates the deduplicated `"ACTION (reason)"` strings
from every event classification.

## Where to make each kind of change

| You want to… | Edit |
|---|---|
| Change how a comment body maps to AUTO/FLAG | `classifyEvent` in `internal/policy/policy.go` |
| Treat a new bot as noise | `isBot` in `internal/policy/policy.go` |
| Hard-skip PRs by some new attribute | Top of `Engine.Classify` in `internal/policy/policy.go` |
| Change the cursor-and-age filter logic | Middle of `Engine.Classify` (event filter loop) |
| Roll up differently (e.g. majority vote) | Bottom of `Engine.Classify` (the `worst` loop) |
| Fetch a new kind of feedback | `feedback.Client.FetchEvents` in `internal/feedback/gh.go` |
| Add a new `Kind` of event | `internal/feedback/gh.go` (type) + `classifyEvent` (rule) |
| Show more columns in the decision table | `renderTable` in `internal/scheduler/scheduler.go` |
| Expose a new tuning knob via config | `internal/config/config.go` (see configuration.md) |

## Step-by-step: adding a new classification rule

Concrete example: Eric's team often posts comments like `"let's move this
to a follow-up"` that should never be auto-addressed — aupr should FLAG
them so they don't get picked up by the default.

### 1. Write the test first (`internal/policy/policy_test.go`)

```go
func TestFollowupIsFlag(t *testing.T) {
    pr := feedback.PR{Author: "ionrock"}
    ev := feedback.Event{
        ID: "1", Author: "reviewer",
        Body: "let's move this to a follow-up",
        CreatedAt: time.Now(),
    }
    d := eng().Classify(pr, []feedback.Event{ev}, "")
    if d.Action != ActFlag {
        t.Fatalf("want FLAG, got %s (reason=%s)", d.Action, d.Reason)
    }
}
```

Run it, watch it fail:

```
$ go test ./internal/policy/ -run TestFollowup
--- FAIL: TestFollowupIsFlag
    want FLAG (reason=FLAG (unclassified; surfacing for review))
```

Wait — that actually passes today via the default fall-through. Use this
moment to decide: do you want a **specific reason string** so it shows up
nicely in the decision table? If yes, assert the reason too:

```go
if !strings.Contains(d.Reason, "follow-up deferral") {
    t.Fatalf("want follow-up reason, got %q", d.Reason)
}
```

That will fail, and drives the next step.

### 2. Add the rule

Open `internal/policy/policy.go`, find `classifyEvent`, and insert **above
the Auto block** (FLAG rules come first so they outrank AUTO matches):

```go
if containsAny(body, []string{"follow-up", "followup", "follow up"}) {
    return EventClass{Event: ev, Action: ActFlag, Reason: "follow-up deferral"}
}
```

### 3. Verify against live data

```
$ make build
$ ./bin/aupr --dry-run once
```

Look for your new reason string in the REASON column. If the decision
table now classifies a PR differently than you expected, you have real
feedback without making any API writes.

### 4. Commit with the test

```
aupr: flag follow-up-deferral comments
- new rule in classifyEvent matches "follow-up" / "followup" / "follow up"
- test TestFollowupIsFlag
- verified against live gh output via aupr --dry-run once
```

### Rule-writing conventions

- **Match on lowercased body.** `body := strings.ToLower(ev.Body)` is
  already done at the top of `classifyEvent`; use that.
- **FLAG rules go before AUTO rules.** A typo comment that also says
  "revert" must FLAG, not AUTO. The order of blocks encodes this.
- **Prefer distinctive substrings.** `"nit:"` is safer than `"nit"`
  because `"infinite"` contains `nit`. We already use `"nit:"` and
  `"nit "` for exactly this reason.
- **Keep Reason short and category-like.** It shows up in the table.
  `"follow-up deferral"` good; `"reviewer wants to postpone this until
  a follow-up PR"` bad.
- **Don't build giant regex.** Plain substring lookups via `containsAny`
  are fine and fast. If you need real structure, promote to a dedicated
  function and test it separately.

## Step-by-step: ignoring a new kind of author

Add the login to `isBot` in `internal/policy/policy.go`:

```go
func isBot(login string) bool {
    l := strings.ToLower(login)
    return strings.HasSuffix(l, "[bot]") ||
        l == "dependabot" || l == "renovate" || l == "github-actions" ||
        l == "codecov-io" // ← add here
}
```

Add a test that mirrors `TestBotIsIgnored`.

If the list is going to grow, promote it to a config field under
`[policy]` (see `docs/configuration.md` "Adding a new config field").
Until then, the hard-coded list is the source of truth.

## Step-by-step: adding a new hard skip

Concrete example: skip PRs where `pr.Mergeable == "CONFLICTING"` unless
the config opts in.

Edit `Engine.Classify`, **before** the event-filter loop:

```go
if strings.EqualFold(pr.Mergeable, "CONFLICTING") {
    d.Action, d.Reason = ActSkip, "merge conflicts (not mechanical)"
    return d
}
```

Add the test:

```go
func TestConflictingIsSkip(t *testing.T) {
    pr := feedback.PR{Author: "ionrock", Mergeable: "CONFLICTING"}
    ev := feedback.Event{ID: "1", Author: "r", Body: "typo", CreatedAt: time.Now()}
    d := eng().Classify(pr, []feedback.Event{ev}, "")
    if d.Action != ActSkip { t.Fatalf("want SKIP, got %s", d.Action) }
}
```

Hard skips short-circuit everything — use them sparingly. A soft "FLAG with
a specific reason" is usually better because it keeps the PR visible in
the table.

## Step-by-step: adding a new feedback source

If, say, you want aupr to consider failing CI check-run annotations as
feedback events:

1. **Model it.** Add a new `Kind` constant and any new fields to `Event`
   that make sense (e.g. `CheckName`).
2. **Fetch it.** Add a method on `feedback.Client` that calls
   `gh api repos/{nwo}/commits/{head_sha}/check-runs` (or equivalent)
   and returns `[]Event`.
3. **Splice it in.** Append the new events inside `Client.FetchEvents`
   so every caller gets them for free.
4. **Classify it.** Add a branch at the top of `classifyEvent`:

   ```go
   if ev.Kind == KindCheckRun {
       if strings.Contains(body, "flaky") { return ... }
       return EventClass{Event: ev, Action: ActFlag, Reason: "failing check-run"}
   }
   ```

5. **Test each layer independently.** The feedback layer via a fake
   `execx.Runner`, the policy layer with a hand-rolled `Event`.

## Testing your changes

aupr's policy tests are intentionally cheap and deterministic. They don't
touch the network or disk.

```
make test                         # run everything
go test ./internal/policy/        # just the policy pkg
go test ./internal/policy/ -run TestFollowup -v
```

The helper `eng()` at the top of `policy_test.go` gives you an `Engine`
with default config and `User: "ionrock"`. Construct your `PR` and
`[]Event` by hand — no `gh` calls.

When you want to see the rule fire against live GitHub state:

```
make build
./bin/aupr --dry-run once
```

The `--dry-run` flag is a global belt-and-suspenders guard; it prints a
banner, prevents any future mutations, and honors `$AUPR_DRY_RUN` and
`[agent] dry_run = true` in the config.

## Calibration loop

This is the workflow we actually use when tuning rules.

1. **Capture the current baseline.** `./bin/aupr --dry-run once > before.txt`
2. **Pick a surprising row.** Usually a `FLAG (unclassified)` that you
   know should be AUTO, or the inverse.
3. **Inspect the raw events for that PR** (until `aupr test <pr>` is
   implemented — tracked in docs/TODO-style notes):
   ```
   gh pr view <n> --repo <nwo> --json comments,reviews
   gh api repos/<nwo>/pulls/<n>/comments
   ```
4. **Read the bodies.** What substring would have caught it?
5. **Add the rule + test** (see above).
6. **Re-run** `./bin/aupr --dry-run once > after.txt` and diff.
7. Repeat. Keep commits small — one rule per commit makes bisecting
   false-positives trivial.

If a rule causes more harm than good, revert the single commit. That's
the whole value of keeping rules in dedicated commits.

## Design constraints

Rules you should not break without explicit CTO-level discussion:

1. **Most-conservative roll-up.** If any event FLAGs, the PR FLAGs. Don't
   add a "majority vote" or "ignore one outlier" heuristic. Trust is
   cheap to lose.
2. **First-match-wins ordering in `classifyEvent`.** FLAG rules come
   before AUTO rules. Do not reorder without reading the test suite.
3. **Cursor filter is non-negotiable.** Every acted-on event must be
   behind the cursor or the daemon will act on the same comment twice.
4. **No network in policy tests.** Policy is pure; keep it that way so
   CI stays fast and deterministic.
5. **Never classify on `pr.Title` alone.** Title is human-authored
   context; bodies are where intent lives.
6. **Default is FLAG, not AUTO.** If you add a fall-through, it must be
   FLAG or SKIP. An unknown rule must never silently auto-act.
