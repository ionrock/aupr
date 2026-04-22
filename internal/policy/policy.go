// Package policy decides whether aupr should act on a PR's feedback.
//
// M1 returns conservative classifications so the decision table is meaningful.
// Later milestones will tune heuristics based on Eric's real traffic.
package policy

import (
	"strings"
	"time"

	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/feedback"
)

// Action is the high-level decision for a PR.
type Action string

const (
	ActAuto Action = "AUTO" // daemon will attempt to address
	ActFlag Action = "FLAG" // surface to Eric, don't touch
	ActSkip Action = "SKIP" // ignore entirely
)

// Decision explains a single PR's disposition.
type Decision struct {
	PR              feedback.PR
	Action          Action
	Reason          string
	NewEvents       []feedback.Event // events newer than the last acted cursor
	Classifications []EventClass
}

// EventClass tags a single feedback event.
type EventClass struct {
	Event  feedback.Event
	Action Action
	Reason string
}

// Engine holds the rules.
type Engine struct {
	Cfg  *config.Config
	User string // the operator's GitHub login (feedback *from* this user is self-talk, ignored)
}

// Classify evaluates a PR and its events.
// lastSeenID is the most recent event ID aupr has acted on for this PR; "" means never.
func (e *Engine) Classify(pr feedback.PR, events []feedback.Event, lastSeenID string) Decision {
	d := Decision{PR: pr}

	// Hard skips first.
	if pr.IsDraft {
		d.Action, d.Reason = ActSkip, "draft PR"
		return d
	}
	if strings.EqualFold(pr.ReviewDecision, "APPROVED") && !hasEventsAfterApproval(events) {
		d.Action, d.Reason = ActSkip, "approved with no new post-approval feedback"
		return d
	}
	if e.skipRepo(pr.Repo) {
		d.Action, d.Reason = ActSkip, "repo skipped in config"
		return d
	}
	if pr.Author != "" && e.User != "" && !strings.EqualFold(pr.Author, e.User) {
		d.Action, d.Reason = ActSkip, "PR not authored by operator"
		return d
	}

	// Filter events: drop self-talk and events older than our cursor.
	newEvents := make([]feedback.Event, 0, len(events))
	passedCursor := lastSeenID == ""
	maxAge := time.Duration(e.Cfg.Policy.MaxFeedbackAgeDays) * 24 * time.Hour
	cutoff := time.Now().Add(-maxAge)

	for _, ev := range events {
		if !passedCursor {
			if ev.ID == lastSeenID {
				passedCursor = true
			}
			continue
		}
		if strings.EqualFold(ev.Author, e.User) {
			continue
		}
		if isBot(ev.Author) {
			continue
		}
		if maxAge > 0 && ev.CreatedAt.Before(cutoff) {
			continue
		}
		newEvents = append(newEvents, ev)
	}
	d.NewEvents = newEvents

	if len(newEvents) == 0 {
		d.Action, d.Reason = ActSkip, "no new actionable feedback"
		return d
	}

	// Classify each event; overall action is the most conservative of any.
	worst := ActAuto
	for _, ev := range newEvents {
		cls := classifyEvent(ev)
		d.Classifications = append(d.Classifications, cls)
		if rank(cls.Action) > rank(worst) {
			worst = cls.Action
		}
	}
	d.Action = worst
	d.Reason = summarizeReasons(d.Classifications)
	return d
}

func (e *Engine) skipRepo(nwo string) bool {
	if e.Cfg.Repos == nil {
		return false
	}
	r, ok := e.Cfg.Repos[nwo]
	return ok && r.Skip
}

// classifyEvent is the heart of the heuristic. Intentionally conservative.
func classifyEvent(ev feedback.Event) EventClass {
	body := strings.ToLower(ev.Body)

	// Approval reviews without a body are informational only.
	if ev.Kind == feedback.KindReview {
		switch strings.ToUpper(ev.State) {
		case "APPROVED":
			if strings.TrimSpace(body) == "" {
				return EventClass{Event: ev, Action: ActSkip, Reason: "approval with empty body"}
			}
		case "CHANGES_REQUESTED":
			return EventClass{Event: ev, Action: ActFlag, Reason: "changes requested (needs human judgment)"}
		}
	}

	if containsAny(body, []string{"revert", "remove this", "i disagree", "why did you", "why do we"}) {
		return EventClass{Event: ev, Action: ActFlag, Reason: "discussion/disagreement"}
	}
	if containsAny(body, []string{"security", "auth", "token", "secret", "password"}) {
		return EventClass{Event: ev, Action: ActFlag, Reason: "security-adjacent"}
	}
	if len(ev.Body) > 600 {
		return EventClass{Event: ev, Action: ActFlag, Reason: "long comment suggests nuance"}
	}

	if containsAny(body, []string{"typo", "spelling", "grammar"}) {
		return EventClass{Event: ev, Action: ActAuto, Reason: "typo/grammar fix"}
	}
	if containsAny(body, []string{"rename", "extract", "format", "lint", "style", "nit:", "nit "}) {
		return EventClass{Event: ev, Action: ActAuto, Reason: "style/rename/format"}
	}
	if containsAny(body, []string{"add a test", "add test", "missing test", "write a test"}) {
		return EventClass{Event: ev, Action: ActAuto, Reason: "add-test"}
	}
	if containsAny(body, []string{"flaky", "re-run ci", "retrigger", "restart build"}) {
		return EventClass{Event: ev, Action: ActAuto, Reason: "likely flaky CI"}
	}

	// Default: flag. It's safer to surface unknowns than to act blindly.
	return EventClass{Event: ev, Action: ActFlag, Reason: "unclassified; surfacing for review"}
}

func hasEventsAfterApproval(events []feedback.Event) bool {
	var approvedAt time.Time
	for _, ev := range events {
		if ev.Kind == feedback.KindReview && strings.EqualFold(ev.State, "APPROVED") {
			if ev.CreatedAt.After(approvedAt) {
				approvedAt = ev.CreatedAt
			}
		}
	}
	if approvedAt.IsZero() {
		return false
	}
	for _, ev := range events {
		if ev.CreatedAt.After(approvedAt) && ev.Kind != feedback.KindReview {
			return true
		}
	}
	return false
}

func isBot(login string) bool {
	l := strings.ToLower(login)
	return strings.HasSuffix(l, "[bot]") ||
		l == "dependabot" || l == "renovate" || l == "github-actions"
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func rank(a Action) int {
	switch a {
	case ActAuto:
		return 0
	case ActFlag:
		return 1
	case ActSkip:
		return 2
	}
	return 0
}

func summarizeReasons(cs []EventClass) string {
	seen := map[string]struct{}{}
	var out []string
	for _, c := range cs {
		key := string(c.Action) + ":" + c.Reason
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, string(c.Action)+" ("+c.Reason+")")
	}
	return strings.Join(out, "; ")
}
