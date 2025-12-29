package ghcli

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/mitsuhiko/gh-issue-sync/internal/issue"
)

type Client struct {
	runner Runner
	repo   string
}

func NewClient(runner Runner, repo string) *Client {
	return &Client{runner: runner, repo: repo}
}

func (c *Client) withRepo(args []string) []string {
	if c.repo == "" {
		return args
	}
	for i := range args {
		if args[i] == "--repo" {
			return args
		}
	}
	return append(args, "--repo", c.repo)
}

type apiLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Label represents a GitHub label with its color.
type Label struct {
	Name  string
	Color string // Hex color without #
}

type apiUser struct {
	Login string `json:"login"`
}

type apiMilestone struct {
	Title string `json:"title"`
}

type apiIssue struct {
	Number      int           `json:"number"`
	Title       string        `json:"title"`
	Body        string        `json:"body"`
	Labels      []apiLabel    `json:"labels"`
	Assignees   []apiUser     `json:"assignees"`
	Milestone   *apiMilestone `json:"milestone"`
	State       string        `json:"state"`
	StateReason *string       `json:"stateReason"`
}

func (a apiIssue) ToIssue() issue.Issue {
	labels := make([]string, 0, len(a.Labels))
	for _, label := range a.Labels {
		labels = append(labels, label.Name)
	}
	assignees := make([]string, 0, len(a.Assignees))
	for _, user := range a.Assignees {
		assignees = append(assignees, user.Login)
	}
	milestone := ""
	if a.Milestone != nil {
		milestone = a.Milestone.Title
	}
	return issue.Issue{
		Number:      issue.IssueNumber(strconv.Itoa(a.Number)),
		Title:       a.Title,
		Labels:      labels,
		Assignees:   assignees,
		Milestone:   milestone,
		State:       strings.ToLower(a.State),
		StateReason: a.StateReason,
		Body:        a.Body,
	}
}

func (c *Client) ListIssues(ctx context.Context, state string, labels []string) ([]issue.Issue, error) {
	args := []string{"issue", "list", "--state", state, "--limit", "1000", "--json", "number,title,body,labels,assignees,milestone,state,stateReason"}
	for _, label := range labels {
		args = append(args, "--label", label)
	}
	out, err := c.runner.Run(ctx, "gh", c.withRepo(args)...)
	if err != nil {
		return nil, err
	}
	var payload []apiIssue
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return nil, err
	}
	issues := make([]issue.Issue, 0, len(payload))
	for _, item := range payload {
		issues = append(issues, item.ToIssue())
	}
	return issues, nil
}

func (c *Client) GetIssue(ctx context.Context, number string) (issue.Issue, error) {
	args := []string{"issue", "view", number, "--json", "number,title,body,labels,assignees,milestone,state,stateReason"}
	out, err := c.runner.Run(ctx, "gh", c.withRepo(args)...)
	if err != nil {
		return issue.Issue{}, err
	}
	var payload apiIssue
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return issue.Issue{}, err
	}
	return payload.ToIssue(), nil
}

func (c *Client) CreateIssue(ctx context.Context, issue issue.Issue) (string, error) {
	args := []string{"issue", "create", "--title", issue.Title, "--body", issue.Body}
	for _, label := range issue.Labels {
		args = append(args, "--label", label)
	}
	for _, assignee := range issue.Assignees {
		args = append(args, "--assignee", assignee)
	}
	if issue.Milestone != "" {
		args = append(args, "--milestone", issue.Milestone)
	}
	out, err := c.runner.Run(ctx, "gh", c.withRepo(args)...)
	if err != nil {
		return "", err
	}
	return parseIssueNumber(out)
}

func (c *Client) EditIssue(ctx context.Context, number string, change IssueChange) error {
	args := []string{"issue", "edit", number}
	if change.Title != nil {
		args = append(args, "--title", *change.Title)
	}
	if change.Body != nil {
		args = append(args, "--body", *change.Body)
	}
	for _, label := range change.AddLabels {
		args = append(args, "--add-label", label)
	}
	for _, label := range change.RemoveLabels {
		args = append(args, "--remove-label", label)
	}
	for _, assignee := range change.AddAssignees {
		args = append(args, "--add-assignee", assignee)
	}
	for _, assignee := range change.RemoveAssignees {
		args = append(args, "--remove-assignee", assignee)
	}
	if change.Milestone != nil {
		if *change.Milestone == "" {
			args = append(args, "--remove-milestone")
		} else {
			args = append(args, "--milestone", *change.Milestone)
		}
	}
	_, err := c.runner.Run(ctx, "gh", c.withRepo(args)...)
	return err
}

func (c *Client) CloseIssue(ctx context.Context, number string, reason string) error {
	args := []string{"issue", "close", number}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	_, err := c.runner.Run(ctx, "gh", c.withRepo(args)...)
	return err
}

func (c *Client) ReopenIssue(ctx context.Context, number string) error {
	args := []string{"issue", "reopen", number}
	_, err := c.runner.Run(ctx, "gh", c.withRepo(args)...)
	return err
}

var issueNumberPattern = regexp.MustCompile(`(?i)(?:/issues/|#)(\d+)`)

func parseIssueNumber(output string) (string, error) {
	match := issueNumberPattern.FindStringSubmatch(output)
	if len(match) < 2 {
		return "", fmt.Errorf("unable to parse issue number from output: %q", strings.TrimSpace(output))
	}
	return match[1], nil
}

// ListLabels fetches all labels from the repository with their colors.
func (c *Client) ListLabels(ctx context.Context) ([]Label, error) {
	args := []string{"label", "list", "--json", "name,color", "--limit", "1000"}
	out, err := c.runner.Run(ctx, "gh", c.withRepo(args)...)
	if err != nil {
		return nil, err
	}
	var payload []apiLabel
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return nil, err
	}
	labels := make([]Label, 0, len(payload))
	for _, l := range payload {
		labels = append(labels, Label{Name: l.Name, Color: l.Color})
	}
	return labels, nil
}

// IssueChange captures the edits we need to apply to an issue.
type IssueChange struct {
	Title           *string
	Body            *string
	Milestone       *string
	AddLabels       []string
	RemoveLabels    []string
	AddAssignees    []string
	RemoveAssignees []string
	State           *string
	StateReason     *string
	StateTransition *string
	StateWasOpen    bool
	StateWasClosed  bool
	StateIsOpen     bool
	StateIsClosed   bool
}
