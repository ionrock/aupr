package worktree

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// StdinPrompter asks on stdin/stdout. Suitable for `aupr once` and
// `aupr test <pr>` where there is a TTY attached.
type StdinPrompter struct {
	In  io.Reader // defaults to os.Stdin
	Out io.Writer // defaults to os.Stdout
}

// Confirm prints the plan and reads y/N (or yes/no for dirty trees).
func (p StdinPrompter) Confirm(_ context.Context, plan *Plan) (bool, error) {
	in := p.In
	if in == nil {
		in = os.Stdin
	}
	out := p.Out
	if out == nil {
		out = os.Stdout
	}

	fmt.Fprintln(out, formatPrompt(plan))
	// Force explicit "yes" when the tree is dirty, per the CTO-note design.
	if plan.Dirty {
		fmt.Fprint(out, "  Tree is dirty; type 'yes' to proceed, anything else aborts: ")
	} else {
		fmt.Fprint(out, "  Proceed? [y/N] ")
	}

	r := bufio.NewReader(in)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	if plan.Dirty {
		return ans == "yes", nil
	}
	return ans == "y" || ans == "yes", nil
}

func formatPrompt(plan *Plan) string {
	var b strings.Builder
	b.WriteString("\naupr: branch swap required\n\n")
	fmt.Fprintf(&b, "  Path:    %s\n", plan.Path)
	fmt.Fprintf(&b, "  From:    %s", plan.CurrentBranch)
	if plan.Dirty {
		b.WriteString("  (dirty)")
	}
	b.WriteByte('\n')
	fmt.Fprintf(&b, "  To:      %s\n\n", plan.TargetBranch)
	b.WriteString("  Plan:\n")
	step := 1
	if plan.Dirty {
		fmt.Fprintf(&b, "    %d. git stash push --include-untracked -m \"aupr: auto-stash %s->%s\"\n",
			step, plan.CurrentBranch, plan.TargetBranch)
		step++
	}
	fmt.Fprintf(&b, "    %d. git checkout %s\n", step, plan.TargetBranch)
	step++
	fmt.Fprintf(&b, "    %d. git pull --rebase origin %s\n", step, plan.TargetBranch)
	step++
	fmt.Fprintf(&b, "    %d. [run agent to address PR feedback]\n", step)
	step++
	fmt.Fprintf(&b, "    %d. git checkout %s\n", step, plan.CurrentBranch)
	step++
	if plan.Dirty {
		fmt.Fprintf(&b, "    %d. git stash pop\n", step)
	}
	b.WriteString("\n  If any step fails, aupr restores your branch and preserves the stash.\n")
	return b.String()
}
