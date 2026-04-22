package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/execx"
)

// Command is a pluggable agent backend that runs a user-configured argv
// with token substitution. It's the escape hatch for invoking anything
// you can exec — custom wrappers, team tooling, alternative LLM CLIs.
type Command struct {
	Cfg    config.AgentCommandConfig
	Runner execx.Runner
	Logger *slog.Logger
}

// Name implements Agent.
func (c *Command) Name() string { return "command" }

// Invoke implements Agent.
func (c *Command) Invoke(ctx context.Context, req Request) (*Response, error) {
	if len(c.Cfg.Argv) == 0 {
		return nil, errors.New("command agent: argv is empty")
	}
	if req.Workspace == "" {
		return nil, errors.New("command agent: empty workspace")
	}

	prompt := RenderPrompt(req)
	delivery := c.Cfg.PromptDelivery
	if delivery == "" {
		delivery = "arg"
	}

	tokens := map[string]string{
		"workspace":  req.Workspace,
		"repo":       repoName(req.PR.Repo),
		"nwo":        req.PR.Repo,
		"branch":     req.PR.HeadRefName,
		"pr_number":  strconv.Itoa(req.PR.Number),
		"pr_title":   req.PR.Title,
		"pr_url":     req.PR.URL,
		"session_id": req.SessionID,
		"max_turns":  strconv.Itoa(req.MaxTurns),
		"prompt":     prompt, // may or may not be used
	}

	var cleanup func()
	if delivery == "file" {
		f, err := os.CreateTemp("", "aupr-prompt-*.txt")
		if err != nil {
			return nil, fmt.Errorf("create prompt file: %w", err)
		}
		if _, err := f.WriteString(prompt); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, fmt.Errorf("write prompt file: %w", err)
		}
		f.Close()
		tokens["prompt_file"] = f.Name()
		cleanup = func() { os.Remove(f.Name()) }
	}
	if cleanup != nil {
		defer cleanup()
	}

	argv := substituteArgv(c.Cfg.Argv, tokens)

	if req.DryRun {
		c.log().Info("command agent dry-run",
			"workspace", req.Workspace,
			"argv", redactPrompt(argv),
			"prompt_bytes", len(prompt),
			"delivery", delivery,
			"resume", req.SessionID)
		return &Response{
			SessionID: req.SessionID,
			Summary:   fmt.Sprintf("[dry-run] would invoke %s with %d feedback event(s)", argv[0], len(req.Classifications)),
			Output:    prompt,
		}, nil
	}

	c.log().Info("command agent invoking",
		"workspace", req.Workspace,
		"bin", argv[0],
		"argc", len(argv)-1,
		"delivery", delivery,
		"resume", req.SessionID)

	res, err := c.Runner.RunIn(ctx, req.Workspace, argv[0], argv[1:]...)
	if err != nil {
		return nil, fmt.Errorf("command agent exec: %w", err)
	}
	out := strings.TrimRight(res.Stdout, "\n")

	sessionID := ""
	if c.Cfg.SessionIDFrom != "" {
		if v, ok := captureField(out, c.Cfg.SessionIDFrom); ok {
			sessionID = v
		}
	}
	summary := ""
	if c.Cfg.SummaryFrom != "" {
		if v, ok := captureField(out, c.Cfg.SummaryFrom); ok {
			summary = v
		}
	}
	if summary == "" {
		summary = lastNonEmptyLine(out)
	}
	if sessionID == "" {
		// Reuse caller's session id if we didn't parse a new one.
		sessionID = req.SessionID
	}

	return &Response{
		SessionID: sessionID,
		Summary:   truncate(summary, 160),
		Output:    out,
	}, nil
}

// substituteArgv replaces every {token} occurrence in each argv element
// with its looked-up value. If an element is exactly "{prompt}" and the
// prompt is empty (i.e. delivery!=arg), the element is dropped rather
// than producing an empty argv slot.
func substituteArgv(argv []string, tokens map[string]string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		replaced := a
		for k, v := range tokens {
			replaced = strings.ReplaceAll(replaced, "{"+k+"}", v)
		}
		// Skip argv elements that resolved to empty because of a missing
		// token like {prompt_file} when delivery!=file. Keeps argv clean.
		if replaced == "" && a != "" {
			continue
		}
		out = append(out, replaced)
	}
	return out
}

// captureField extracts a field from `out` per a `spec` like:
//
//	"json:foo.bar"      dotted-path lookup in a JSON object
//	"line:PREFIX"       first line starting with PREFIX (prefix stripped)
//
// Returns ("", false) if the spec doesn't match.
func captureField(out, spec string) (string, bool) {
	if strings.HasPrefix(spec, "json:") {
		path := strings.TrimPrefix(spec, "json:")
		return jsonLookup(out, path)
	}
	if strings.HasPrefix(spec, "line:") {
		prefix := strings.TrimPrefix(spec, "line:")
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix)), true
			}
		}
	}
	return "", false
}

// jsonLookup tries the full output as a JSON object, then each line, and
// descends a dotted path.
func jsonLookup(out, path string) (string, bool) {
	try := func(s string) (string, bool) {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return "", false
		}
		for _, key := range strings.Split(path, ".") {
			m, ok := v.(map[string]any)
			if !ok {
				return "", false
			}
			v = m[key]
		}
		switch x := v.(type) {
		case string:
			return x, x != ""
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64), true
		case bool:
			return strconv.FormatBool(x), true
		case nil:
			return "", false
		default:
			b, err := json.Marshal(x)
			if err != nil {
				return "", false
			}
			return string(b), true
		}
	}
	if v, ok := try(strings.TrimSpace(out)); ok {
		return v, true
	}
	// Try last-line-first (NDJSON).
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if v, ok := try(line); ok {
			return v, true
		}
	}
	return "", false
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" {
			return l
		}
	}
	return ""
}

// repoName extracts the name from "owner/name".
func repoName(nwo string) string {
	if i := strings.Index(nwo, "/"); i != -1 {
		return nwo[i+1:]
	}
	return nwo
}

// redactPrompt avoids dumping a 5KB prompt into every log line.
func redactPrompt(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if len(a) > 200 && strings.Contains(a, "\n") {
			out[i] = fmt.Sprintf("<prompt %d bytes>", len(a))
		} else {
			out[i] = a
		}
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func (c *Command) log() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}
