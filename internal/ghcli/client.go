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

// ListIssuesResult contains the result of ListIssuesWithRelationships
type ListIssuesResult struct {
	Issues      []issue.Issue
	LabelColors map[string]string
}

// ListIssuesWithRelationships fetches issues with their relationships and label colors
// using GraphQL with pagination. This is much faster than separate calls.
func (c *Client) ListIssuesWithRelationships(ctx context.Context, state string, labels []string) (ListIssuesResult, error) {
	owner, repo := splitRepo(c.repo)
	if owner == "" || repo == "" {
		return ListIssuesResult{}, fmt.Errorf("invalid repository format")
	}

	// Map state to GraphQL enum
	stateFilter := "OPEN"
	if state == "closed" {
		stateFilter = "CLOSED"
	} else if state == "all" {
		stateFilter = ""
	}

	// Build label filter
	labelFilter := ""
	if len(labels) > 0 {
		quoted := make([]string, len(labels))
		for i, l := range labels {
			quoted[i] = fmt.Sprintf("%q", l)
		}
		labelFilter = fmt.Sprintf(", labels: [%s]", strings.Join(quoted, ", "))
	}

	stateArg := ""
	if stateFilter != "" {
		stateArg = fmt.Sprintf(", states: [%s]", stateFilter)
	}

	result := ListIssuesResult{
		LabelColors: make(map[string]string),
	}

	// Paginate through issues, fetching labels on first page
	var cursor *string
	firstPage := true
	for {
		cursorArg := "null"
		if cursor != nil {
			cursorArg = fmt.Sprintf("%q", *cursor)
		}

		// Include labels query only on first page
		labelsFragment := ""
		if firstPage {
			labelsFragment = `labels(first: 100) {
      nodes {
        name
        color
      }
    }`
		}

		query := fmt.Sprintf(`query($owner: String!, $repo: String!) {
  repository(owner: $owner, name: $repo) {
    %s
    issues(first: 100%s%s, after: %s) {
      pageInfo {
        hasNextPage
        endCursor
      }
      nodes {
        number
        title
        body
        state
        stateReason
        labels(first: 100) { nodes { name } }
        assignees(first: 100) { nodes { login } }
        milestone { title }
        parent { number }
        blockedBy(first: 100) { nodes { number } }
        blocking(first: 100) { nodes { number } }
      }
    }
  }
}`, labelsFragment, stateArg, labelFilter, cursorArg)

		args := []string{"api", "graphql",
			"-f", fmt.Sprintf("query=%s", query),
			"-F", fmt.Sprintf("owner=%s", owner),
			"-F", fmt.Sprintf("repo=%s", repo),
		}

		out, err := c.runner.Run(ctx, "gh", args...)
		if err != nil {
			return ListIssuesResult{}, err
		}

		var resp struct {
			Data struct {
				Repository struct {
					Labels struct {
						Nodes []struct {
							Name  string `json:"name"`
							Color string `json:"color"`
						} `json:"nodes"`
					} `json:"labels"`
					Issues struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							Number      int     `json:"number"`
							Title       string  `json:"title"`
							Body        string  `json:"body"`
							State       string  `json:"state"`
							StateReason *string `json:"stateReason"`
							Labels      struct {
								Nodes []struct {
									Name string `json:"name"`
								} `json:"nodes"`
							} `json:"labels"`
							Assignees struct {
								Nodes []struct {
									Login string `json:"login"`
								} `json:"nodes"`
							} `json:"assignees"`
							Milestone *struct {
								Title string `json:"title"`
							} `json:"milestone"`
							Parent *struct {
								Number int `json:"number"`
							} `json:"parent"`
							BlockedBy struct {
								Nodes []struct {
									Number int `json:"number"`
								} `json:"nodes"`
							} `json:"blockedBy"`
							Blocking struct {
								Nodes []struct {
									Number int `json:"number"`
								} `json:"nodes"`
							} `json:"blocking"`
						} `json:"nodes"`
					} `json:"issues"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			return ListIssuesResult{}, fmt.Errorf("failed to parse GraphQL response: %w", err)
		}

		if len(resp.Errors) > 0 {
			return ListIssuesResult{}, fmt.Errorf("GraphQL error: %s", resp.Errors[0].Message)
		}

		// Parse labels from first page
		if firstPage {
			for _, l := range resp.Data.Repository.Labels.Nodes {
				result.LabelColors[l.Name] = l.Color
			}
			firstPage = false
		}

		for _, node := range resp.Data.Repository.Issues.Nodes {
			issLabels := make([]string, 0, len(node.Labels.Nodes))
			for _, l := range node.Labels.Nodes {
				issLabels = append(issLabels, l.Name)
			}
			assignees := make([]string, 0, len(node.Assignees.Nodes))
			for _, a := range node.Assignees.Nodes {
				assignees = append(assignees, a.Login)
			}
			milestone := ""
			if node.Milestone != nil {
				milestone = node.Milestone.Title
			}

			iss := issue.Issue{
				Number:      issue.IssueNumber(strconv.Itoa(node.Number)),
				Title:       node.Title,
				Body:        node.Body,
				State:       strings.ToLower(node.State),
				StateReason: node.StateReason,
				Labels:      issLabels,
				Assignees:   assignees,
				Milestone:   milestone,
			}

			if node.Parent != nil {
				ref := issue.IssueRef(strconv.Itoa(node.Parent.Number))
				iss.Parent = &ref
			}
			for _, b := range node.BlockedBy.Nodes {
				iss.BlockedBy = append(iss.BlockedBy, issue.IssueRef(strconv.Itoa(b.Number)))
			}
			for _, b := range node.Blocking.Nodes {
				iss.Blocks = append(iss.Blocks, issue.IssueRef(strconv.Itoa(b.Number)))
			}

			result.Issues = append(result.Issues, iss)
		}

		if !resp.Data.Repository.Issues.PageInfo.HasNextPage {
			break
		}
		cursor = &resp.Data.Repository.Issues.PageInfo.EndCursor
	}

	return result, nil
}

// EnrichWithRelationships fetches parent and blocking relationships for an issue via GraphQL
// and updates the issue in place.
func (c *Client) EnrichWithRelationships(ctx context.Context, iss *issue.Issue) error {
	if iss.Number.IsLocal() {
		return nil
	}

	rels, _, err := c.GetIssueRelationships(ctx, iss.Number.String())
	if err != nil {
		// Don't fail if relationships can't be fetched (e.g., feature not available)
		return nil
	}

	iss.Parent = rels.Parent
	iss.BlockedBy = rels.BlockedBy
	iss.Blocks = rels.Blocks
	return nil
}

// EnrichWithRelationshipsBatch fetches parent and blocking relationships for multiple issues
// in a single API call and updates each issue in place.
func (c *Client) EnrichWithRelationshipsBatch(ctx context.Context, issues []issue.Issue) error {
	// Collect issue numbers for non-local issues
	var numbers []string
	for i := range issues {
		if !issues[i].Number.IsLocal() {
			numbers = append(numbers, issues[i].Number.String())
		}
	}

	if len(numbers) == 0 {
		return nil
	}

	// Fetch all relationships in one call
	rels, err := c.GetIssueRelationshipsBatch(ctx, numbers)
	if err != nil {
		// Don't fail if relationships can't be fetched (e.g., feature not available)
		return nil
	}

	// Apply relationships to each issue
	for i := range issues {
		num := issues[i].Number.String()
		if rel, ok := rels[num]; ok {
			issues[i].Parent = rel.Parent
			issues[i].BlockedBy = rel.BlockedBy
			issues[i].Blocks = rel.Blocks
		}
	}

	return nil
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

// GetIssuesBatch fetches multiple issues in a single GraphQL call.
// Returns a map of issue number -> issue. Issues that don't exist are not included.
func (c *Client) GetIssuesBatch(ctx context.Context, numbers []string) (map[string]issue.Issue, error) {
	if len(numbers) == 0 {
		return map[string]issue.Issue{}, nil
	}

	owner, repo := splitRepo(c.repo)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("invalid repository format")
	}

	// Build a batched GraphQL query with aliases for each issue
	var issueQueries []string
	for i, num := range numbers {
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		issueQueries = append(issueQueries, fmt.Sprintf(`issue%d: issue(number: %d) {
      number
      title
      body
      state
      stateReason
      labels(first: 100) { nodes { name } }
      assignees(first: 100) { nodes { login } }
      milestone { title }
      parent { number }
      blockedBy(first: 100) { nodes { number } }
      blocking(first: 100) { nodes { number } }
    }`, i, n))
	}

	if len(issueQueries) == 0 {
		return map[string]issue.Issue{}, nil
	}

	query := fmt.Sprintf(`query($owner: String!, $repo: String!) {
  repository(owner: $owner, name: $repo) {
    %s
  }
}`, strings.Join(issueQueries, "\n    "))

	args := []string{"api", "graphql",
		"-f", fmt.Sprintf("query=%s", query),
		"-F", fmt.Sprintf("owner=%s", owner),
		"-F", fmt.Sprintf("repo=%s", repo),
	}

	out, err := c.runner.Run(ctx, "gh", args...)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data struct {
			Repository map[string]json.RawMessage `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("failed to parse GraphQL response: %w", err)
	}

	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("GraphQL error: %s", resp.Errors[0].Message)
	}

	results := make(map[string]issue.Issue)

	for alias, rawIssue := range resp.Data.Repository {
		if !strings.HasPrefix(alias, "issue") {
			continue
		}
		if string(rawIssue) == "null" {
			continue
		}

		var issueData struct {
			Number      int     `json:"number"`
			Title       string  `json:"title"`
			Body        string  `json:"body"`
			State       string  `json:"state"`
			StateReason *string `json:"stateReason"`
			Labels      struct {
				Nodes []struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"labels"`
			Assignees struct {
				Nodes []struct {
					Login string `json:"login"`
				} `json:"nodes"`
			} `json:"assignees"`
			Milestone *struct {
				Title string `json:"title"`
			} `json:"milestone"`
			Parent *struct {
				Number int `json:"number"`
			} `json:"parent"`
			BlockedBy struct {
				Nodes []struct {
					Number int `json:"number"`
				} `json:"nodes"`
			} `json:"blockedBy"`
			Blocking struct {
				Nodes []struct {
					Number int `json:"number"`
				} `json:"nodes"`
			} `json:"blocking"`
		}
		if err := json.Unmarshal(rawIssue, &issueData); err != nil {
			continue
		}

		labels := make([]string, 0, len(issueData.Labels.Nodes))
		for _, l := range issueData.Labels.Nodes {
			labels = append(labels, l.Name)
		}
		assignees := make([]string, 0, len(issueData.Assignees.Nodes))
		for _, a := range issueData.Assignees.Nodes {
			assignees = append(assignees, a.Login)
		}
		milestone := ""
		if issueData.Milestone != nil {
			milestone = issueData.Milestone.Title
		}

		iss := issue.Issue{
			Number:      issue.IssueNumber(strconv.Itoa(issueData.Number)),
			Title:       issueData.Title,
			Body:        issueData.Body,
			State:       strings.ToLower(issueData.State),
			StateReason: issueData.StateReason,
			Labels:      labels,
			Assignees:   assignees,
			Milestone:   milestone,
		}

		if issueData.Parent != nil {
			ref := issue.IssueRef(strconv.Itoa(issueData.Parent.Number))
			iss.Parent = &ref
		}
		for _, b := range issueData.BlockedBy.Nodes {
			iss.BlockedBy = append(iss.BlockedBy, issue.IssueRef(strconv.Itoa(b.Number)))
		}
		for _, b := range issueData.Blocking.Nodes {
			iss.Blocks = append(iss.Blocks, issue.IssueRef(strconv.Itoa(b.Number)))
		}

		results[strconv.Itoa(issueData.Number)] = iss
	}

	return results, nil
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

// CreateLabel creates a new label with the given name and color.
// Color should be a 6-character hex string without the # prefix.
func (c *Client) CreateLabel(ctx context.Context, name, color string) error {
	args := []string{"label", "create", name, "--color", color}
	_, err := c.runner.Run(ctx, "gh", c.withRepo(args)...)
	return err
}

// Milestone represents a GitHub milestone.
type Milestone struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	DueOn       *string `json:"due_on"` // ISO 8601 format
	State       string  `json:"state"`  // open or closed
}

// ListMilestones fetches all milestones from the repository.
func (c *Client) ListMilestones(ctx context.Context) ([]Milestone, error) {
	// Use gh api to get milestones (gh doesn't have a built-in milestone list command)
	// We need to fetch both open and closed milestones
	var allMilestones []Milestone

	owner, repo := splitRepo(c.repo)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("invalid repository format")
	}

	for _, state := range []string{"open", "closed"} {
		// Use query parameters in URL for GET request
		// Note: gh api doesn't support --repo, so we must expand the repo in the URL
		endpoint := fmt.Sprintf("repos/%s/%s/milestones?state=%s&per_page=100", owner, repo, state)
		args := []string{"api", endpoint, "--paginate", "-q", ".[]"}
		out, err := c.runner.Run(ctx, "gh", args...)
		if err != nil {
			// If there are no milestones, gh api might return an error or empty
			continue
		}
		if strings.TrimSpace(out) == "" {
			continue
		}

		// Parse line-delimited JSON objects
		lines := strings.Split(strings.TrimSpace(out), "\n")
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var m struct {
				Title       string  `json:"title"`
				Description string  `json:"description"`
				DueOn       *string `json:"due_on"`
				State       string  `json:"state"`
			}
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				continue
			}
			allMilestones = append(allMilestones, Milestone{
				Title:       m.Title,
				Description: m.Description,
				DueOn:       m.DueOn,
				State:       m.State,
			})
		}
	}

	return allMilestones, nil
}

// CreateMilestone creates a new milestone with the given title.
func (c *Client) CreateMilestone(ctx context.Context, title string) error {
	owner, repo := splitRepo(c.repo)
	if owner == "" || repo == "" {
		return fmt.Errorf("invalid repository format")
	}

	endpoint := fmt.Sprintf("repos/%s/%s/milestones", owner, repo)
	args := []string{"api", endpoint, "-X", "POST", "-f", "title=" + title}
	_, err := c.runner.Run(ctx, "gh", args...)
	return err
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
