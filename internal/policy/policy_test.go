package policy

import (
	"testing"
	"time"

	"github.com/dagster-io/aupr/internal/config"
	"github.com/dagster-io/aupr/internal/feedback"
)

func eng() *Engine {
	return &Engine{Cfg: config.Defaults(), User: "ionrock"}
}

func TestDraftIsSkipped(t *testing.T) {
	pr := feedback.PR{IsDraft: true, Author: "ionrock"}
	d := eng().Classify(pr, nil, "")
	if d.Action != ActSkip {
		t.Fatalf("want SKIP, got %s", d.Action)
	}
}

func TestNoEventsIsSkip(t *testing.T) {
	pr := feedback.PR{Author: "ionrock"}
	d := eng().Classify(pr, nil, "")
	if d.Action != ActSkip {
		t.Fatalf("want SKIP, got %s", d.Action)
	}
}

func TestTypoIsAuto(t *testing.T) {
	pr := feedback.PR{Author: "ionrock"}
	ev := feedback.Event{ID: "1", Author: "reviewer", Body: "typo: teh", CreatedAt: time.Now()}
	d := eng().Classify(pr, []feedback.Event{ev}, "")
	if d.Action != ActAuto {
		t.Fatalf("want AUTO, got %s (reason=%s)", d.Action, d.Reason)
	}
}

func TestSecurityIsFlag(t *testing.T) {
	pr := feedback.PR{Author: "ionrock"}
	ev := feedback.Event{ID: "1", Author: "reviewer", Body: "nit: don't log the auth token", CreatedAt: time.Now()}
	d := eng().Classify(pr, []feedback.Event{ev}, "")
	if d.Action != ActFlag {
		t.Fatalf("want FLAG, got %s (reason=%s)", d.Action, d.Reason)
	}
}

func TestBotIsIgnored(t *testing.T) {
	pr := feedback.PR{Author: "ionrock"}
	ev := feedback.Event{ID: "1", Author: "dependabot[bot]", Body: "typo", CreatedAt: time.Now()}
	d := eng().Classify(pr, []feedback.Event{ev}, "")
	if d.Action != ActSkip {
		t.Fatalf("want SKIP (bot), got %s", d.Action)
	}
}

func TestNotAuthoredByOperatorSkipped(t *testing.T) {
	pr := feedback.PR{Author: "someone-else"}
	d := eng().Classify(pr, nil, "")
	if d.Action != ActSkip {
		t.Fatalf("want SKIP, got %s", d.Action)
	}
}

func TestCursorFiltersOldEvents(t *testing.T) {
	pr := feedback.PR{Author: "ionrock"}
	now := time.Now()
	old := feedback.Event{ID: "old", Author: "reviewer", Body: "typo", CreatedAt: now.Add(-time.Hour)}
	newer := feedback.Event{ID: "new", Author: "reviewer", Body: "security concern with token", CreatedAt: now}
	d := eng().Classify(pr, []feedback.Event{old, newer}, "old")
	if d.Action != ActFlag {
		t.Fatalf("want FLAG from post-cursor event, got %s", d.Action)
	}
	if len(d.NewEvents) != 1 || d.NewEvents[0].ID != "new" {
		t.Fatalf("expected only the post-cursor event, got %+v", d.NewEvents)
	}
}
