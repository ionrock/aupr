package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ionrock/aupr/internal/execx"
)

// ClaudeCode invokes `claude` in print mode.
//
// Command shape:
//
//	claude -p --output-format=json --max-turns=N [--resume=<sid>] <prompt>
//
// The JSON output contains a `session_id` we persist for resume across
// feedback events on the same PR, and a `result` field we propagate as
// the human-readable summary.
type ClaudeCode struct {
	Runner execx.Runner
	Logger *slog.Logger
	Bin    string // optional override; defaults to "claude"
}

// Name implements Agent.
func (c *ClaudeCode) Name() string { return "claude-code" }

// Invoke implements Agent.
func (c *ClaudeCode) Invoke(ctx context.Context, req Request) (*Response, error) {
	if req.Workspace == "" {
		return nil, fmt.Errorf("claude: empty workspace")
	}
	prompt := RenderPrompt(req)
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	args := []string{"-p", "--output-format=json"}
	if req.MaxTurns > 0 {
		args = append(args, fmt.Sprintf("--max-turns=%d", req.MaxTurns))
	}
	if req.SessionID != "" {
		args = append(args, "--resume="+req.SessionID)
	}
	args = append(args, prompt)

	if req.DryRun {
		c.log().Info("claude dry-run",
			"workspace", req.Workspace,
			"bin", bin,
			"args_len", len(args),
			"resume", req.SessionID,
			"prompt_bytes", len(prompt))
		return &Response{
			SessionID: req.SessionID,
			Summary:   "[dry-run] would invoke claude with " + fmt.Sprint(len(req.Classifications)) + " feedback event(s)",
			Output:    prompt,
		}, nil
	}

	c.log().Info("claude invoking",
		"workspace", req.Workspace,
		"resume", req.SessionID,
		"max_turns", req.MaxTurns)

	res, err := c.Runner.RunIn(ctx, req.Workspace, bin, args...)
	if err != nil {
		return nil, fmt.Errorf("claude exec: %w", err)
	}
	return c.parseOutput(res.Stdout, req)
}

// claudeOutput matches the shape of `claude -p --output-format=json` stdout.
// Field names here follow the current Anthropic CLI schema; unknown fields
// are ignored so schema evolution doesn't break us.
type claudeOutput struct {
	SessionID    string  `json:"session_id"`
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"` // newer claude versions include cost
	Usage        struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	// Some claude versions emit NDJSON instead; we handle that below.
}

func (c *ClaudeCode) parseOutput(out string, req Request) (*Response, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, fmt.Errorf("claude: empty output")
	}

	// Fast path: single JSON object.
	var o claudeOutput
	if err := json.Unmarshal([]byte(trimmed), &o); err == nil && o.SessionID != "" {
		if o.IsError {
			return nil, fmt.Errorf("claude reported is_error: %s", o.Result)
		}
		return &Response{
			SessionID:    o.SessionID,
			Summary:      extractSummary(o.Result),
			Output:       trimmed,
			InputTokens:  o.Usage.InputTokens,
			OutputTokens: o.Usage.OutputTokens,
			CostUSD:      o.TotalCostUSD,
		}, nil
	}

	// NDJSON path: find the last non-empty line that parses and use its
	// session_id / result. This handles stream-style output.
	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var o2 claudeOutput
		if err := json.Unmarshal([]byte(line), &o2); err == nil && o2.SessionID != "" {
			if o2.IsError {
				return nil, fmt.Errorf("claude reported is_error: %s", o2.Result)
			}
			return &Response{
				SessionID:    o2.SessionID,
				Summary:      extractSummary(o2.Result),
				Output:       trimmed,
				InputTokens:  o2.Usage.InputTokens,
				OutputTokens: o2.Usage.OutputTokens,
				CostUSD:      o2.TotalCostUSD,
			}, nil
		}
	}

	// Couldn't parse; surface as error but include raw output for debugging.
	short := trimmed
	if len(short) > 400 {
		short = short[:400] + "..."
	}
	return nil, fmt.Errorf("claude: could not parse output: %s", short)
}

// extractSummary pulls a "SUMMARY: ..." line out of free-form output if
// present; otherwise returns the first non-empty line trimmed.
func extractSummary(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "SUMMARY:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "SUMMARY:"))
		}
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 140 {
				line = line[:137] + "..."
			}
			return line
		}
	}
	return ""
}

func (c *ClaudeCode) log() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}
