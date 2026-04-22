package agent

import (
	"fmt"
	"strings"

	"github.com/ionrock/aupr/internal/feedback"
	"github.com/ionrock/aupr/internal/policy"
)

// RenderPrompt builds the human-readable prompt the agent receives.
// The format is deliberately plain so it works across claude / codex /
// opencode with minimal tweaking.
func RenderPrompt(req Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are addressing reviewer feedback on a GitHub pull request.\n\n")
	fmt.Fprintf(&b, "Repository: %s\n", req.PR.Repo)
	fmt.Fprintf(&b, "PR:         #%d — %s\n", req.PR.Number, req.PR.Title)
	fmt.Fprintf(&b, "URL:        %s\n", req.PR.URL)
	fmt.Fprintf(&b, "Branch:     %s  (already checked out in your working directory)\n\n",
		req.PR.HeadRefName)

	b.WriteString("Reviewer feedback to address, one block per comment:\n\n")
	for i, c := range req.Classifications {
		renderEvent(&b, i+1, c)
	}

	b.WriteString(strings.TrimSpace(`
Guidelines:
- Make the minimum, targeted change that addresses each comment above.
- Commit each logical change on its own with a message prefixed with
  "aupr: " and a concise summary; prefer multiple small commits to one
  large one.
- Do NOT push; aupr will push after validating your work.
- Do NOT force-push, amend commits that are already on origin, or
  rebase the branch.
- If any feedback is ambiguous, stop and explain in one sentence rather
  than guessing.

When you are done, print a single line starting with "SUMMARY: " that
describes what you changed, at most 120 characters.
`))
	b.WriteString("\n")
	return b.String()
}

func renderEvent(b *strings.Builder, n int, c policy.EventClass) {
	ev := c.Event
	fmt.Fprintf(b, "--- Comment %d ---\n", n)
	fmt.Fprintf(b, "Classification: %s (%s)\n", c.Action, c.Reason)
	fmt.Fprintf(b, "Author:         %s\n", ev.Author)
	if ev.Path != "" {
		if ev.Line > 0 {
			fmt.Fprintf(b, "Location:       %s:%d\n", ev.Path, ev.Line)
		} else {
			fmt.Fprintf(b, "Location:       %s\n", ev.Path)
		}
	}
	fmt.Fprintf(b, "URL:            %s\n", ev.URL)
	fmt.Fprintf(b, "Kind:           %s\n", eventKindLabel(ev.Kind))
	body := strings.TrimSpace(ev.Body)
	if body == "" {
		body = "(empty body)"
	}
	fmt.Fprintf(b, "Body:\n%s\n\n", indent(body, "    "))
}

func eventKindLabel(k feedback.Kind) string {
	switch k {
	case feedback.KindReviewComment:
		return "line-level review comment"
	case feedback.KindIssueComment:
		return "general PR comment"
	case feedback.KindReview:
		return "review summary"
	}
	return string(k)
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}
