package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/feedback"
	"github.com/ionrock/aupr/internal/policy"
)

func TestRenderPromptIncludesAllEvents(t *testing.T) {
	req := Request{
		PR: feedback.PR{
			Repo: "x/y", Number: 1, Title: "fix it", URL: "https://example/1",
			HeadRefName: "eric/foo",
		},
		Classifications: []policy.EventClass{
			{
				Event: feedback.Event{
					Author: "alice", Body: "typo on L3", Path: "main.go", Line: 3,
					URL: "u1", Kind: feedback.KindReviewComment, CreatedAt: time.Now(),
				},
				Action: policy.ActAuto, Reason: "typo/grammar fix",
			},
			{
				Event: feedback.Event{
					Author: "bob", Body: "rename foo to bar", URL: "u2",
					Kind: feedback.KindIssueComment, CreatedAt: time.Now(),
				},
				Action: policy.ActAuto, Reason: "style/rename/format",
			},
		},
	}
	got := RenderPrompt(req)
	for _, want := range []string{
		"x/y", "#1", "fix it", "eric/foo",
		"Comment 1", "Comment 2",
		"alice", "bob",
		"main.go:3",
		"typo/grammar fix",
		"style/rename/format",
		"aupr:", // guideline mentions aupr: commit prefix
		"SUMMARY:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q; full output:\n%s", want, got)
		}
	}
}

func TestExtractSummary(t *testing.T) {
	cases := []struct{ in, out string }{
		{"SUMMARY: fixed typo", "fixed typo"},
		{"did stuff\nSUMMARY: renamed Foo", "renamed Foo"},
		{"just prose here", "just prose here"},
		{"", ""},
	}
	for _, c := range cases {
		if got := extractSummary(c.in); got != c.out {
			t.Errorf("extractSummary(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

// Runner that records the last call and returns a canned JSON response.
type fakeRunner struct {
	stdout string
	err    error
	last   struct {
		dir  string
		name string
		args []string
	}
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (*execx.Result, error) {
	return f.RunIn(ctx, "", name, args...)
}

func (f *fakeRunner) RunIn(_ context.Context, dir, name string, args ...string) (*execx.Result, error) {
	f.last.dir = dir
	f.last.name = name
	f.last.args = args
	return &execx.Result{Stdout: f.stdout}, f.err
}

func TestClaudeInvokeHappyPath(t *testing.T) {
	fr := &fakeRunner{
		stdout: `{"session_id":"sess-123","result":"SUMMARY: fixed typo","is_error":false,"total_cost_usd":0.0125,"usage":{"input_tokens":1234,"output_tokens":567}}`,
	}
	c := &ClaudeCode{Runner: fr}
	resp, err := c.Invoke(context.Background(), Request{
		Workspace: "/tmp/wk", PR: feedback.PR{Repo: "x/y", Number: 1},
		MaxTurns: 15,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.SessionID != "sess-123" || resp.Summary != "fixed typo" {
		t.Errorf("bad response: %+v", resp)
	}
	if resp.InputTokens != 1234 || resp.OutputTokens != 567 {
		t.Errorf("tokens: in %d out %d", resp.InputTokens, resp.OutputTokens)
	}
	if resp.CostUSD < 0.012 || resp.CostUSD > 0.013 {
		t.Errorf("cost: %f", resp.CostUSD)
	}
	if fr.last.dir != "/tmp/wk" {
		t.Errorf("wrong cwd: %q", fr.last.dir)
	}
	if fr.last.name != "claude" {
		t.Errorf("wrong bin: %q", fr.last.name)
	}
	// Expect no --resume flag when SessionID is empty.
	for _, a := range fr.last.args {
		if strings.HasPrefix(a, "--resume") {
			t.Errorf("should not have --resume: %v", fr.last.args)
		}
	}
}

func TestClaudeDryRunDoesNotExec(t *testing.T) {
	fr := &fakeRunner{err: nil}
	c := &ClaudeCode{Runner: fr}
	resp, err := c.Invoke(context.Background(), Request{
		Workspace: "/tmp/wk", PR: feedback.PR{Repo: "x/y", Number: 1},
		DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fr.last.name != "" {
		t.Errorf("dry-run should not call Runner, got %q", fr.last.name)
	}
	if !strings.Contains(resp.Summary, "dry-run") {
		t.Errorf("expected dry-run summary, got %q", resp.Summary)
	}
}

func TestClaudeSessionResume(t *testing.T) {
	fr := &fakeRunner{stdout: `{"session_id":"sess-new","result":"ok"}`}
	c := &ClaudeCode{Runner: fr}
	_, err := c.Invoke(context.Background(), Request{
		Workspace: "/tmp/wk", SessionID: "sess-prev",
		PR: feedback.PR{Repo: "x/y", Number: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range fr.last.args {
		if a == "--resume=sess-prev" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --resume=sess-prev in args; got %v", fr.last.args)
	}
}

func TestClaudeIsErrorPropagates(t *testing.T) {
	fr := &fakeRunner{stdout: `{"session_id":"s","result":"context too long","is_error":true}`}
	c := &ClaudeCode{Runner: fr}
	if _, err := c.Invoke(context.Background(), Request{Workspace: "/tmp/wk"}); err == nil {
		t.Fatal("expected error")
	}
}
