// Package feedback enumerates open PRs and normalizes reviewer feedback events.
//
// M1 strategy: shell out to the `gh` CLI (already authed as the user).
// This avoids reimplementing OAuth and keeps the surface small. We can swap in
// githubv4 later behind the same interface.
package feedback

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dagster-io/aupr/internal/execx"
)

// PR is a normalized open PR authored by the user.
type PR struct {
	Repo          string    // "owner/name"
	Number        int
	Title         string
	URL           string
	HeadRefName   string
	BaseRefName   string
	IsDraft       bool
	Mergeable     string // MERGEABLE / CONFLICTING / UNKNOWN
	ReviewDecision string // APPROVED / CHANGES_REQUESTED / REVIEW_REQUIRED / ""
	UpdatedAt     time.Time
	CreatedAt     time.Time
	Author        string
}

// Event is one normalized piece of reviewer feedback on a PR.
type Event struct {
	ID        string    // stable identifier (comment node id)
	PR        PR
	Kind      Kind
	Author    string
	Body      string
	URL       string
	CreatedAt time.Time
	Path      string // for review comments
	Line      int    // for review comments
	State     string // for reviews: APPROVED/CHANGES_REQUESTED/COMMENTED
}

// Kind categorizes a feedback event.
type Kind string

const (
	KindReviewComment Kind = "review_comment" // line-level review comment
	KindIssueComment  Kind = "issue_comment"  // general PR conversation
	KindReview        Kind = "review"         // review summary (approve / request changes / comment)
)

// Client pulls PRs and feedback through `gh`.
type Client struct {
	Runner execx.Runner
	User   string // GitHub login; used to filter authored PRs
}

// ListAuthoredOpenPRs uses `gh search prs` to enumerate open PRs authored by
// Client.User across all repos. Returns only PRs whose repo is present in
// allowedRepos (an "owner/name" set); if allowedRepos is nil, all are returned.
func (c *Client) ListAuthoredOpenPRs(ctx context.Context, allowedRepos map[string]struct{}) ([]PR, error) {
	if c.User == "" {
		return nil, fmt.Errorf("feedback: GithubUser is empty")
	}
	args := []string{
		"search", "prs",
		"--author", c.User,
		"--state", "open",
		"--limit", "200",
		"--json", "repository,number,title,url,isDraft,createdAt,updatedAt,author",
	}
	res, err := c.Runner.Run(ctx, "gh", args...)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
		Number    int       `json:"number"`
		Title     string    `json:"title"`
		URL       string    `json:"url"`
		IsDraft   bool      `json:"isDraft"`
		CreatedAt time.Time `json:"createdAt"`
		UpdatedAt time.Time `json:"updatedAt"`
		Author    struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &raw); err != nil {
		return nil, fmt.Errorf("parse gh search prs json: %w", err)
	}
	var out []PR
	for _, r := range raw {
		if allowedRepos != nil {
			if _, ok := allowedRepos[r.Repository.NameWithOwner]; !ok {
				continue
			}
		}
		out = append(out, PR{
			Repo:      r.Repository.NameWithOwner,
			Number:    r.Number,
			Title:     r.Title,
			URL:       r.URL,
			IsDraft:   r.IsDraft,
			CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
			Author:    r.Author.Login,
		})
	}
	return out, nil
}

// EnrichPR fills in HeadRefName, BaseRefName, Mergeable, and ReviewDecision.
func (c *Client) EnrichPR(ctx context.Context, pr *PR) error {
	args := []string{
		"pr", "view", fmt.Sprint(pr.Number),
		"--repo", pr.Repo,
		"--json", "headRefName,baseRefName,mergeable,reviewDecision",
	}
	res, err := c.Runner.Run(ctx, "gh", args...)
	if err != nil {
		return err
	}
	var v struct {
		HeadRefName    string `json:"headRefName"`
		BaseRefName    string `json:"baseRefName"`
		Mergeable      string `json:"mergeable"`
		ReviewDecision string `json:"reviewDecision"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &v); err != nil {
		return fmt.Errorf("parse gh pr view: %w", err)
	}
	pr.HeadRefName = v.HeadRefName
	pr.BaseRefName = v.BaseRefName
	pr.Mergeable = v.Mergeable
	pr.ReviewDecision = v.ReviewDecision
	return nil
}

// FetchEvents collects review comments, issue comments, and reviews for a PR.
// The order is roughly chronological (by CreatedAt).
func (c *Client) FetchEvents(ctx context.Context, pr PR) ([]Event, error) {
	var events []Event

	// Review comments (line-level).
	rcPath := fmt.Sprintf("repos/%s/pulls/%d/comments", pr.Repo, pr.Number)
	rcRes, err := c.Runner.Run(ctx, "gh", "api", "--paginate", rcPath)
	if err != nil {
		return nil, fmt.Errorf("fetch review comments: %w", err)
	}
	var rcs []struct {
		NodeID    string    `json:"node_id"`
		Body      string    `json:"body"`
		HTMLURL   string    `json:"html_url"`
		CreatedAt time.Time `json:"created_at"`
		Path      string    `json:"path"`
		Line      int       `json:"line"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := unmarshalPaginated(rcRes.Stdout, &rcs); err != nil {
		return nil, fmt.Errorf("parse review comments: %w", err)
	}
	for _, r := range rcs {
		events = append(events, Event{
			ID:        r.NodeID,
			PR:        pr,
			Kind:      KindReviewComment,
			Author:    r.User.Login,
			Body:      r.Body,
			URL:       r.HTMLURL,
			CreatedAt: r.CreatedAt,
			Path:      r.Path,
			Line:      r.Line,
		})
	}

	// Issue comments (general PR conversation).
	icPath := fmt.Sprintf("repos/%s/issues/%d/comments", pr.Repo, pr.Number)
	icRes, err := c.Runner.Run(ctx, "gh", "api", "--paginate", icPath)
	if err != nil {
		return nil, fmt.Errorf("fetch issue comments: %w", err)
	}
	var ics []struct {
		NodeID    string    `json:"node_id"`
		Body      string    `json:"body"`
		HTMLURL   string    `json:"html_url"`
		CreatedAt time.Time `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := unmarshalPaginated(icRes.Stdout, &ics); err != nil {
		return nil, fmt.Errorf("parse issue comments: %w", err)
	}
	for _, r := range ics {
		events = append(events, Event{
			ID:        r.NodeID,
			PR:        pr,
			Kind:      KindIssueComment,
			Author:    r.User.Login,
			Body:      r.Body,
			URL:       r.HTMLURL,
			CreatedAt: r.CreatedAt,
		})
	}

	// Reviews.
	rvPath := fmt.Sprintf("repos/%s/pulls/%d/reviews", pr.Repo, pr.Number)
	rvRes, err := c.Runner.Run(ctx, "gh", "api", "--paginate", rvPath)
	if err != nil {
		return nil, fmt.Errorf("fetch reviews: %w", err)
	}
	var rvs []struct {
		NodeID      string    `json:"node_id"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		SubmittedAt time.Time `json:"submitted_at"`
		State       string    `json:"state"`
		User        struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := unmarshalPaginated(rvRes.Stdout, &rvs); err != nil {
		return nil, fmt.Errorf("parse reviews: %w", err)
	}
	for _, r := range rvs {
		events = append(events, Event{
			ID:        r.NodeID,
			PR:        pr,
			Kind:      KindReview,
			Author:    r.User.Login,
			Body:      r.Body,
			URL:       r.HTMLURL,
			CreatedAt: r.SubmittedAt,
			State:     r.State,
		})
	}
	return events, nil
}

// unmarshalPaginated handles `gh api --paginate` output, which concatenates
// multiple JSON arrays back-to-back (e.g. `[...][...][...]`). We re-stitch
// them into a single slice.
func unmarshalPaginated(s string, dst interface{}) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// Fast path: a single JSON array.
	if err := json.Unmarshal([]byte(s), dst); err == nil {
		return nil
	}
	// Slow path: stream of arrays.
	dec := json.NewDecoder(strings.NewReader(s))
	// Use reflection-free approach by funneling through json.RawMessage.
	var combined []json.RawMessage
	for {
		var chunk []json.RawMessage
		if err := dec.Decode(&chunk); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return err
		}
		combined = append(combined, chunk...)
	}
	b, err := json.Marshal(combined)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
