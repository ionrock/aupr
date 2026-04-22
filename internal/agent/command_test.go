package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ionrock/aupr/internal/config"
	"github.com/ionrock/aupr/internal/execx"
	"github.com/ionrock/aupr/internal/feedback"
	"github.com/ionrock/aupr/internal/policy"
)

type cmdFakeRunner struct {
	stdout string
	err    error
	last   struct {
		dir  string
		name string
		args []string
	}
}

func (f *cmdFakeRunner) Run(ctx context.Context, name string, args ...string) (*execx.Result, error) {
	return f.RunIn(ctx, "", name, args...)
}
func (f *cmdFakeRunner) RunIn(_ context.Context, dir, name string, args ...string) (*execx.Result, error) {
	f.last.dir = dir
	f.last.name = name
	f.last.args = append([]string(nil), args...)
	return &execx.Result{Stdout: f.stdout}, f.err
}

func sampleRequest() Request {
	return Request{
		Workspace: "/wk",
		PR: feedback.PR{
			Repo: "dagster-io/internal", Number: 42,
			Title: "fix it", URL: "https://github.com/dagster-io/internal/pull/42",
			HeadRefName: "eric/foo",
		},
		Classifications: []policy.EventClass{{
			Event:  feedback.Event{Author: "a", Body: "typo", Kind: feedback.KindReviewComment},
			Action: policy.ActAuto, Reason: "typo/grammar fix",
		}},
		MaxTurns: 7,
	}
}

func TestCommandTokenSubstitutionArg(t *testing.T) {
	fr := &cmdFakeRunner{stdout: `{"session_id":"sess-7"}` + "\n"}
	c := &Command{
		Cfg: config.AgentCommandConfig{
			Argv:           []string{"my-wrap", "--pr={pr_number}", "--branch={branch}", "--turns={max_turns}", "{prompt}"},
			PromptDelivery: "arg",
			SessionIDFrom:  "json:session_id",
		},
		Runner: fr,
	}
	resp, err := c.Invoke(context.Background(), sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if fr.last.name != "my-wrap" {
		t.Errorf("bin: %q", fr.last.name)
	}
	if fr.last.dir != "/wk" {
		t.Errorf("cwd: %q", fr.last.dir)
	}
	want := []string{"--pr=42", "--branch=eric/foo", "--turns=7"}
	for i, w := range want {
		if fr.last.args[i] != w {
			t.Errorf("arg %d: got %q want %q", i, fr.last.args[i], w)
		}
	}
	// Last arg is the prompt; just verify it contains expected content.
	prompt := fr.last.args[len(fr.last.args)-1]
	if !strings.Contains(prompt, "PR:         #42") {
		t.Errorf("prompt missing expected text; got:\n%s", prompt)
	}
	if resp.SessionID != "sess-7" {
		t.Errorf("sessionID: %q", resp.SessionID)
	}
}

func TestCommandPromptFileDelivery(t *testing.T) {
	fr := &cmdFakeRunner{stdout: "SUMMARY: nothing to do\n"}
	c := &Command{
		Cfg: config.AgentCommandConfig{
			Argv:           []string{"my-wrap", "--prompt-file", "{prompt_file}"},
			PromptDelivery: "file",
			SummaryFrom:    "line:SUMMARY:",
		},
		Runner: fr,
	}
	resp, err := c.Invoke(context.Background(), sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	// The second arg should be a real file path that exists at invocation
	// time; it's cleaned up after Invoke returns.
	if len(fr.last.args) != 2 {
		t.Fatalf("argc: %v", fr.last.args)
	}
	if fr.last.args[0] != "--prompt-file" {
		t.Errorf("arg0: %q", fr.last.args[0])
	}
	// File should have been removed after Invoke returned.
	if _, err := os.Stat(fr.last.args[1]); err == nil {
		t.Errorf("prompt file not cleaned up: %s", fr.last.args[1])
	}
	if resp.Summary != "nothing to do" {
		t.Errorf("summary: %q", resp.Summary)
	}
}

func TestCommandDryRunDoesNotExec(t *testing.T) {
	fr := &cmdFakeRunner{}
	c := &Command{
		Cfg:    config.AgentCommandConfig{Argv: []string{"real-bin"}},
		Runner: fr,
	}
	req := sampleRequest()
	req.DryRun = true
	resp, err := c.Invoke(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if fr.last.name != "" {
		t.Errorf("dry-run executed: %q", fr.last.name)
	}
	if !strings.Contains(resp.Summary, "dry-run") {
		t.Errorf("unexpected summary: %q", resp.Summary)
	}
}

func TestCommandFailurePropagates(t *testing.T) {
	fr := &cmdFakeRunner{err: errors.New("nope")}
	c := &Command{
		Cfg:    config.AgentCommandConfig{Argv: []string{"real-bin"}},
		Runner: fr,
	}
	if _, err := c.Invoke(context.Background(), sampleRequest()); err == nil {
		t.Fatal("expected error")
	}
}

func TestCommandEmptyArgv(t *testing.T) {
	c := &Command{Cfg: config.AgentCommandConfig{}}
	if _, err := c.Invoke(context.Background(), sampleRequest()); err == nil {
		t.Fatal("expected error on empty argv")
	}
}

func TestCaptureField(t *testing.T) {
	cases := []struct {
		out, spec, want string
		ok              bool
	}{
		{`{"session_id":"s-1"}`, "json:session_id", "s-1", true},
		{"log line\n{\"session_id\":\"s-2\"}\n", "json:session_id", "s-2", true},
		{"SUMMARY: did it\n", "line:SUMMARY:", "did it", true},
		{"nothing here", "json:missing", "", false},
		{`{"session_id":123}`, "json:session_id", "123", true},
	}
	for _, c := range cases {
		got, ok := captureField(c.out, c.spec)
		if got != c.want || ok != c.ok {
			t.Errorf("captureField(%q, %q) = (%q, %v), want (%q, %v)",
				c.out, c.spec, got, ok, c.want, c.ok)
		}
	}
}
