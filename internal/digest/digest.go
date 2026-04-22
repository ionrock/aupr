// Package digest formats a summary of aupr activity over a time window.
//
// Used by the `aupr digest` subcommand and by the daemon when
// [notify] summary_cadence = "daily".
package digest

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ionrock/aupr/internal/state"
)

// Summary aggregates attempts + skips + stashes over a window.
type Summary struct {
	Since         time.Time
	Until         time.Time
	Attempts      []state.Attempt
	Skips         []state.Skip
	RecoveryStash []state.RecoveryStash

	// Precomputed aggregates.
	SuccessCount int
	ErrorCount   int
	DryRunCount  int
	OtherCount   int
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	RepoCounts   map[string]int
}

// Build aggregates the raw data into a Summary.
func Build(since, until time.Time, attempts []state.Attempt, skips []state.Skip, stashes []state.RecoveryStash) *Summary {
	s := &Summary{
		Since: since, Until: until,
		Attempts: attempts, Skips: skips, RecoveryStash: stashes,
		RepoCounts: map[string]int{},
	}
	for _, a := range attempts {
		s.RepoCounts[a.Repo]++
		s.InputTokens += a.InputTokens
		s.OutputTokens += a.OutputTokens
		s.CostUSD += a.CostUSD
		switch a.Outcome {
		case "success":
			s.SuccessCount++
		case "error":
			s.ErrorCount++
		case "dry-run":
			s.DryRunCount++
		default:
			s.OtherCount++
		}
	}
	return s
}

// Format renders the Summary as a human-readable text block.
func (s *Summary) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "aupr digest — %s → %s\n",
		s.Since.Format("2006-01-02 15:04"), s.Until.Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "  %d attempt(s): %d success, %d error, %d dry-run, %d other\n",
		len(s.Attempts), s.SuccessCount, s.ErrorCount, s.DryRunCount, s.OtherCount)
	if s.InputTokens+s.OutputTokens > 0 {
		fmt.Fprintf(&b, "  tokens: %d in / %d out", s.InputTokens, s.OutputTokens)
		if s.CostUSD > 0 {
			fmt.Fprintf(&b, "   cost: $%.4f", s.CostUSD)
		}
		b.WriteByte('\n')
	}

	if len(s.RepoCounts) > 0 {
		keys := make([]string, 0, len(s.RepoCounts))
		for k := range s.RepoCounts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("  by repo:\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "    %-40s %d\n", k, s.RepoCounts[k])
		}
	}

	if s.ErrorCount > 0 {
		b.WriteString("\n  Errors:\n")
		n := 0
		for _, a := range s.Attempts {
			if a.Outcome != "error" {
				continue
			}
			n++
			if n > 10 {
				fmt.Fprintf(&b, "    … and %d more\n", s.ErrorCount-10)
				break
			}
			msg := a.Error
			if msg == "" {
				msg = a.Summary
			}
			fmt.Fprintf(&b, "    %s #%d  %s\n", a.Repo, a.PRNumber, truncate(msg, 100))
		}
	}

	if len(s.RecoveryStash) > 0 {
		b.WriteString("\n  Recovery stashes (run `git stash pop <ref>` in each):\n")
		for _, r := range s.RecoveryStash {
			fmt.Fprintf(&b, "    %s  %s  (%s)\n", r.RepoPath, r.Ref, truncate(r.Message, 80))
		}
	}

	if len(s.Attempts) == 0 && len(s.Skips) == 0 && len(s.RecoveryStash) == 0 {
		b.WriteString("  (no activity in window)\n")
	}

	return b.String()
}

// Empty reports whether the summary contains anything worth sending.
func (s *Summary) Empty() bool {
	return len(s.Attempts) == 0 && len(s.RecoveryStash) == 0
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
