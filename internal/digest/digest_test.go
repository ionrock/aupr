package digest

import (
	"strings"
	"testing"
	"time"

	"github.com/ionrock/aupr/internal/state"
)

func TestBuildAndFormat(t *testing.T) {
	now := time.Now()
	atts := []state.Attempt{
		{Repo: "x/y", PRNumber: 1, Outcome: "success", Summary: "fixed typo", FinishedAt: now,
			InputTokens: 500, OutputTokens: 200, CostUSD: 0.03},
		{Repo: "x/y", PRNumber: 1, Outcome: "success", Summary: "second fix", FinishedAt: now,
			InputTokens: 300, OutputTokens: 100, CostUSD: 0.01},
		{Repo: "x/y", PRNumber: 2, Outcome: "error", Error: "agent: exit 1", FinishedAt: now},
		{Repo: "a/b", PRNumber: 3, Outcome: "dry-run", Summary: "preview", FinishedAt: now},
	}
	s := Build(now.Add(-24*time.Hour), now, atts, nil, nil)
	if s.SuccessCount != 2 || s.ErrorCount != 1 || s.DryRunCount != 1 {
		t.Errorf("counts wrong: +2s/1e/1d got %+v", s)
	}
	if s.InputTokens != 800 || s.OutputTokens != 300 {
		t.Errorf("tokens: in %d out %d", s.InputTokens, s.OutputTokens)
	}
	if s.CostUSD < 0.039 || s.CostUSD > 0.041 {
		t.Errorf("cost: %f", s.CostUSD)
	}
	if s.RepoCounts["x/y"] != 3 || s.RepoCounts["a/b"] != 1 {
		t.Errorf("repo counts: %v", s.RepoCounts)
	}
	out := s.Format()
	for _, want := range []string{
		"4 attempt(s)", "2 success", "1 error", "x/y", "a/b",
		"agent: exit 1", "tokens: 800 in / 300 out",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("format missing %q; full output:\n%s", want, out)
		}
	}
}

func TestEmptySummary(t *testing.T) {
	s := Build(time.Now(), time.Now(), nil, nil, nil)
	if !s.Empty() {
		t.Error("expected empty")
	}
	if !strings.Contains(s.Format(), "no activity") {
		t.Errorf("expected 'no activity' in empty format, got:\n%s", s.Format())
	}
}

func TestRecoveryStashesInFormat(t *testing.T) {
	stashes := []state.RecoveryStash{
		{RepoPath: "/a", Ref: "stash@{0}", Message: "aupr: auto-stash main->eric/foo"},
	}
	s := Build(time.Now(), time.Now(), nil, nil, stashes)
	out := s.Format()
	if !strings.Contains(out, "Recovery stashes") {
		t.Errorf("expected recovery header; got:\n%s", out)
	}
	if !strings.Contains(out, "stash@{0}") {
		t.Errorf("expected stash ref; got:\n%s", out)
	}
}
