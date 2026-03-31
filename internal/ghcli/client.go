package ghcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mitsuhiko/gh-issue-sync/internal/issue"
)

// ErrMissingProjectScope is returned when the token lacks project scope
var ErrMissingProjectScope = errors.New("missing 'project' scope - run 'gh auth refresh -s project' to enable")

type Client struct {
	runner   Runner
	repo     string
	progress func(ProgressEvent)
}

func NewClient(runner Runner, repo string) *Client {
	return &Client{runner: runner, repo: repo}
}

type ProgressStage string

const (
	ProgressListIssuesPageStart ProgressStage = "list_issues_page_start"
	ProgressListIssuesPageDone  ProgressStage = "list_issues_page_done"
)

type ProgressEvent struct {
	Stage      ProgressStage
	Page       int
	Issues     int
	PageIssues int
	Total      int
}

func (c *Client) SetProgress(fn func(ProgressEvent)) {
	c.progress = fn
}

func (c *Client) reportProgress(event ProgressEvent) {
	if c.progress != nil {
		c.progress(event)
	}
}

// HasProjectScope checks if the current GitHub token has the 'project' scope.
func (c *Client) HasProjectScope(ctx context.Context) (bool, error) {
	// Make a simple API call and check the X-Oauth-Scopes header
	out, err := c.runner.Run(ctx, "gh", "api", "user", "-i")
	if err != nil {
		return false, err
	}

	// Parse headers from the response
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.HasPrefix(strings.ToLower(line), "x-oauth-scopes:") {
			scopes := strings.ToLower(line[len("x-oauth-scopes:"):])
			// Check for 'project' scope (which implies read:project)
			return strings.Contains(scopes, "project"), nil
		}
	}

	return false, nil
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
	Author      *apiUser      `json:"author"`
	CreatedAt   string        `json:"createdAt"`
	UpdatedAt   string        `json:"updatedAt"`
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
	author := ""
	if a.Author != nil {
		author = a.Author.Login
	}
	iss := issue.Issue{
		Number:      issue.IssueNumber(strconv.Itoa(a.Number)),
		Title:       a.Title,
		Labels:      labels,
		Assignees:   assignees,
		Milestone:   milestone,
		State:       strings.ToLower(a.State),
		StateReason: canonicalStateReasonPtr(a.StateReason),
		Body:        a.Body,
		Author:      author,
	}
	if a.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, a.CreatedAt); err == nil {
			iss.CreatedAt = &t
		}
	}
	if a.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, a.UpdatedAt); err == nil {
			iss.UpdatedAt = &t
		}
	}
	return iss
}

func (c *Client) ListIssues(ctx context.Context, state string, labels []string) ([]issue.Issue, error) {
	args := []string{"issue", "list", "--state", state, "--limit", "1000", "--json", "number,title,body,labels,assignees,milestone,state,stateReason,author"}
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

// ListIssuesOptions configures the ListIssuesWithRelationships query.
type ListIssuesOptions struct {
	State  string    // "open", "closed", or "all"
	Labels []string  // Filter by labels
	Since  time.Time // Only fetch issues updated after this time (zero means no filter)
}

// ListIssuesWithRelationships fetches issues with their relationships and label colors
// using GraphQL with pagination. This is much faster than separate calls.
func (c *Client) ListIssuesWithRelationships(ctx context.Context, opts ListIssuesOptions) (ListIssuesResult, error) {
	owner, repo := splitRepo(c.repo)
	if owner == "" || repo == "" {
		return ListIssuesResult{}, fmt.Errorf("invalid repository format")
	}

	// Map state to GraphQL enum
	stateFilter := "OPEN"
	if opts.State == "closed" {
		stateFilter = "CLOSED"
	} else if opts.State == "all" {
		stateFilter = ""
	}

	// Build label filter
	labelFilter := ""
	if len(opts.Labels) > 0 {
		quoted := make([]string, len(opts.Labels))
		for i, l := range opts.Labels {
			quoted[i] = fmt.Sprintf("%q", l)
		}
		labelFilter = fmt.Sprintf(", labels: [%s]", strings.Join(quoted, ", "))
	}

	stateArg := ""
	if stateFilter != "" {
		stateArg = fmt.Sprintf(", states: [%s]", stateFilter)
	}

	// Build since filter for incremental sync
	sinceArg := ""
	if !opts.Since.IsZero() {
		sinceArg = fmt.Sprintf(", filterBy: {since: %q}", opts.Since.Format(time.RFC3339))
	}

	result := ListIssuesResult{
		LabelColors: make(map[string]string),
	}

	// Paginate through issues, fetching labels on first page
	var cursor *string
	firstPage := true
	page := 0
	totalCount := 0
	includeProjectItems := true
	for {
		page++
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

		projectItemsFragment := ""
		if includeProjectItems {
			projectItemsFragment = "projectItems(first: 20) { nodes { project { title } } }"
		}

		query := fmt.Sprintf(`query($owner: String!, $repo: String!) {
  repository(owner: $owner, name: $repo) {
    %s
    issues(first: 100%s%s%s, after: %s) {
      totalCount
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
        createdAt
        updatedAt
        author { login }
        labels(first: 100) { nodes { name } }
        assignees(first: 100) { nodes { login } }
        milestone { title }
        issueType { name }
        %s
        parent { number }
        blockedBy(first: 100) { nodes { number } }
        blocking(first: 100) { nodes { number } }
      }
    }
  }
}`, labelsFragment, stateArg, labelFilter, sinceArg, cursorArg, projectItemsFragment)

		args := []string{"api", "graphql",
			"-f", fmt.Sprintf("query=%s", query),
			"-F", fmt.Sprintf("owner=%s", owner),
			"-F", fmt.Sprintf("repo=%s", repo),
		}

		c.reportProgress(ProgressEvent{
			Stage:  ProgressListIssuesPageStart,
			Page:   page,
			Issues: len(result.Issues),
			Total:  totalCount,
		})

		out, err := c.runner.Run(ctx, "gh", args...)
		if err != nil {
			if includeProjectItems && isProjectScopeError(err) {
				includeProjectItems = false
				continue
			}
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
						TotalCount int `json:"totalCount"`
						PageInfo   struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							Number      int     `json:"number"`
							Title       string  `json:"title"`
							Body        string  `json:"body"`
							State       string  `json:"state"`
							StateReason *string `json:"stateReason"`
							CreatedAt   string  `json:"createdAt"`
							UpdatedAt   string  `json:"updatedAt"`
							Author      *struct {
								Login string `json:"login"`
							} `json:"author"`
							Labels struct {
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
							IssueType *struct {
								Name string `json:"name"`
							} `json:"issueType"`
							ProjectItems *struct {
								Nodes []struct {
									Project struct {
										Title string `json:"title"`
									} `json:"project"`
								} `json:"nodes"`
							} `json:"projectItems"`
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
			if includeProjectItems && isProjectScopeErrorText(resp.Errors[0].Message) {
				includeProjectItems = false
				continue
			}
			return ListIssuesResult{}, fmt.Errorf("GraphQL error: %s", resp.Errors[0].Message)
		}

		totalCount = resp.Data.Repository.Issues.TotalCount

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
			issueType := ""
			if node.IssueType != nil {
				issueType = node.IssueType.Name
			}

			var projects []string
			if node.ProjectItems != nil {
				for _, pi := range node.ProjectItems.Nodes {
					projects = append(projects, pi.Project.Title)
				}
			}

			author := ""
			if node.Author != nil {
				author = node.Author.Login
			}

			iss := issue.Issue{
				Number:      issue.IssueNumber(strconv.Itoa(node.Number)),
				Title:       node.Title,
				Body:        node.Body,
				State:       strings.ToLower(node.State),
				StateReason: canonicalStateReasonPtr(node.StateReason),
				Labels:      issLabels,
				Assignees:   assignees,
				Milestone:   milestone,
				IssueType:   issueType,
				Projects:    projects,
				Author:      author,
			}

			// Parse timestamps
			if node.CreatedAt != "" {
				if t, err := time.Parse(time.RFC3339, node.CreatedAt); err == nil {
					iss.CreatedAt = &t
				}
			}
			if node.UpdatedAt != "" {
				if t, err := time.Parse(time.RFC3339, node.UpdatedAt); err == nil {
					iss.UpdatedAt = &t
				}
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

		c.reportProgress(ProgressEvent{
			Stage:      ProgressListIssuesPageDone,
			Page:       page,
			Issues:     len(result.Issues),
			PageIssues: len(resp.Data.Repository.Issues.Nodes),
			Total:      totalCount,
		})

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
	iss.IssueType = rels.IssueType
	iss.Projects = rels.Projects
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
			issues[i].IssueType = rel.IssueType
			issues[i].Projects = rel.Projects
		}
	}

	return nil
}

func (c *Client) GetIssue(ctx context.Context, number string) (issue.Issue, error) {
	args := []string{"issue", "view", number, "--json", "number,title,body,labels,assignees,milestone,state,stateReason,author,createdAt,updatedAt"}
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

// batchQueryChunkSize is the maximum number of issues to query in a single GraphQL call.
const batchQueryChunkSize = 20

// GetIssuesBatch fetches multiple issues in a single GraphQL call.
// Returns a map of issue number -> issue. Issues that don't exist are not included.
func (c *Client) GetIssuesBatch(ctx context.Context, numbers []string) (map[string]issue.Issue, error) {
	if len(numbers) == 0 {
		return map[string]issue.Issue{}, nil
	}

	// Process in chunks to avoid GitHub's resource limits
	results := make(map[string]issue.Issue)
	for i := 0; i < len(numbers); i += batchQueryChunkSize {
		end := i + batchQueryChunkSize
		if end > len(numbers) {
			end = len(numbers)
		}
		chunk := numbers[i:end]

		chunkResults, err := c.getIssuesBatchChunk(ctx, chunk)
		if err != nil {
			return nil, err
		}

		for k, v := range chunkResults {
			results[k] = v
		}
	}

	return results, nil
}

// getIssuesBatchChunk fetches a single chunk of issues.
func (c *Client) getIssuesBatchChunk(ctx context.Context, numbers []string) (map[string]issue.Issue, error) {
	if len(numbers) == 0 {
		return map[string]issue.Issue{}, nil
	}

	owner, repo := splitRepo(c.repo)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("invalid repository format")
	}

	includeProjectItems := true

	buildIssueQueries := func(withProjects bool) []string {
		var issueQueries []string
		projectItemsFragment := ""
		if withProjects {
			projectItemsFragment = "projectItems(first: 20) { nodes { project { title } } }"
		}

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
      createdAt
      updatedAt
      author { login }
      labels(first: 100) { nodes { name } }
      assignees(first: 100) { nodes { login } }
      milestone { title }
      issueType { name }
      %s
      parent { number }
      blockedBy(first: 100) { nodes { number } }
      blocking(first: 100) { nodes { number } }
    }`, i, n, projectItemsFragment))
		}

		return issueQueries
	}

	var out string
	for {
		issueQueries := buildIssueQueries(includeProjectItems)
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

		var err error
		out, err = c.runner.Run(ctx, "gh", args...)
		if err != nil {
			if includeProjectItems && isProjectScopeError(err) {
				includeProjectItems = false
				continue
			}
			return nil, err
		}
		break
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
		if includeProjectItems && isProjectScopeErrorText(resp.Errors[0].Message) {
			includeProjectItems = false
			issueQueries := buildIssueQueries(includeProjectItems)
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
			if err := json.Unmarshal([]byte(out), &resp); err != nil {
				return nil, fmt.Errorf("failed to parse GraphQL response: %w", err)
			}
			if len(resp.Errors) > 0 {
				return nil, fmt.Errorf("GraphQL error: %s", resp.Errors[0].Message)
			}
		} else {
			return nil, fmt.Errorf("GraphQL error: %s", resp.Errors[0].Message)
		}
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
			CreatedAt   string  `json:"createdAt"`
			UpdatedAt   string  `json:"updatedAt"`
			Author      *struct {
				Login string `json:"login"`
			} `json:"author"`
			Labels struct {
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
			IssueType *struct {
				Name string `json:"name"`
			} `json:"issueType"`
			ProjectItems *struct {
				Nodes []struct {
					Project struct {
						Title string `json:"title"`
					} `json:"project"`
				} `json:"nodes"`
			} `json:"projectItems"`
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
		issueType := ""
		if issueData.IssueType != nil {
			issueType = issueData.IssueType.Name
		}
		var projects []string
		if issueData.ProjectItems != nil {
			for _, pi := range issueData.ProjectItems.Nodes {
				projects = append(projects, pi.Project.Title)
			}
		}

		author := ""
		if issueData.Author != nil {
			author = issueData.Author.Login
		}

		iss := issue.Issue{
			Number:      issue.IssueNumber(strconv.Itoa(issueData.Number)),
			Title:       issueData.Title,
			Body:        issueData.Body,
			State:       strings.ToLower(issueData.State),
			StateReason: canonicalStateReasonPtr(issueData.StateReason),
			Labels:      labels,
			Assignees:   assignees,
			Milestone:   milestone,
			IssueType:   issueType,
			Projects:    projects,
			Author:      author,
		}

		// Parse timestamps
		if issueData.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, issueData.CreatedAt); err == nil {
				iss.CreatedAt = &t
			}
		}
		if issueData.UpdatedAt != "" {
			if t, err := time.Parse(time.RFC3339, issueData.UpdatedAt); err == nil {
				iss.UpdatedAt = &t
			}
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
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%s", c.repo, number), "--method", "PATCH", "-f", "state=closed"}
	if reason != "" {
		normalized, ok := normalizeCloseReason(reason)
		if !ok {
			return fmt.Errorf("unsupported close reason %q (expected completed or not_planned)", reason)
		}
		args = append(args, "-f", "state_reason="+normalized)
	}
	_, err := c.runner.Run(ctx, "gh", c.withRepo(args)...)
	return err
}

func canonicalStateReason(reason string) string {
	raw := strings.TrimSpace(reason)
	if raw == "" {
		return ""
	}

	switch strings.ToUpper(raw) {
	case "NOT_PLANNED":
		return "not_planned"
	case "COMPLETED":
		return "completed"
	default:
		return raw
	}
}

func canonicalStateReasonPtr(reason *string) *string {
	if reason == nil {
		return nil
	}
	normalized := canonicalStateReason(*reason)
	if normalized == "" {
		return nil
	}
	return &normalized
}

func normalizeCloseReason(reason string) (string, bool) {
	canonical := canonicalStateReason(reason)
	switch canonical {
	case "completed", "not_planned":
		return canonical, true
	default:
		return "", false
	}
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
// Uses the GitHub API with pagination to fetch all labels (gh label list is limited to 1000).
func (c *Client) ListLabels(ctx context.Context) ([]Label, error) {
	endpoint := fmt.Sprintf("repos/%s/labels", c.repo)
	args := []string{"api", endpoint, "--paginate", "-q", ".[] | {name, color}"}
	out, err := c.runner.Run(ctx, "gh", args...)
	if err != nil {
		return nil, err
	}
	// Output is newline-delimited JSON objects
	var labels []Label
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var l apiLabel
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			return nil, fmt.Errorf("failed to parse label JSON %q: %w", line, err)
		}
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
	IssueType       *string
	AddProjects     []string
	RemoveProjects  []string
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

// IssueType represents a GitHub issue type (org-level).
type IssueType struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListIssueTypes fetches all issue types from the repository's organization.
// Issue types are an organization-level feature, so this queries the org that owns the repo.
// Returns an empty list (not an error) if issue types are not available.
func (c *Client) ListIssueTypes(ctx context.Context) ([]IssueType, error) {
	owner, repo := splitRepo(c.repo)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("invalid repository format")
	}

	// First, try to get issue types from the repository directly
	// This works for organization repos
	query := `query($owner: String!, $repo: String!) {
  repository(owner: $owner, name: $repo) {
    issueTypes(first: 50) {
      nodes {
        id
        name
        description
      }
    }
  }
}`

	args := []string{"api", "graphql",
		"-f", fmt.Sprintf("query=%s", query),
		"-F", fmt.Sprintf("owner=%s", owner),
		"-F", fmt.Sprintf("repo=%s", repo),
	}

	out, err := c.runner.Run(ctx, "gh", args...)
	if err != nil {
		// Issue types might not be available (e.g., personal repo)
		return nil, nil
	}

	var resp struct {
		Data struct {
			Repository struct {
				IssueTypes struct {
					Nodes []struct {
						ID          string `json:"id"`
						Name        string `json:"name"`
						Description string `json:"description"`
					} `json:"nodes"`
				} `json:"issueTypes"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, nil
	}

	if len(resp.Errors) > 0 {
		// Likely not an org repo or feature not enabled
		return nil, nil
	}

	var types []IssueType
	for _, t := range resp.Data.Repository.IssueTypes.Nodes {
		types = append(types, IssueType{
			ID:          t.ID,
			Name:        t.Name,
			Description: t.Description,
		})
	}

	return types, nil
}

// SetIssueType sets or clears the issue type for an issue.
// If issueTypeID is empty, the issue type is cleared.
func (c *Client) SetIssueType(ctx context.Context, issueNumber string, issueTypeID string) error {
	issueNodeID, err := c.GetIssueNodeID(ctx, issueNumber)
	if err != nil {
		return fmt.Errorf("failed to get issue node ID: %w", err)
	}

	var mutation string
	var args []string

	if issueTypeID == "" {
		// Clear issue type by setting to null
		mutation = `mutation($issueId: ID!) {
  updateIssue(input: {id: $issueId, issueTypeId: null}) {
    issue { id }
  }
}`
		args = []string{"api", "graphql",
			"-f", fmt.Sprintf("query=%s", mutation),
			"-f", fmt.Sprintf("issueId=%s", issueNodeID),
		}
	} else {
		mutation = `mutation($issueId: ID!, $issueTypeId: ID!) {
  updateIssue(input: {id: $issueId, issueTypeId: $issueTypeId}) {
    issue { id }
  }
}`
		args = []string{"api", "graphql",
			"-f", fmt.Sprintf("query=%s", mutation),
			"-f", fmt.Sprintf("issueId=%s", issueNodeID),
			"-f", fmt.Sprintf("issueTypeId=%s", issueTypeID),
		}
	}

	out, err := c.runner.Run(ctx, "gh", args...)
	if err != nil {
		return err
	}

	var resp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return fmt.Errorf("failed to parse GraphQL response: %w", err)
	}

	if len(resp.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", resp.Errors[0].Message)
	}

	return nil
}

// Project represents a GitHub Project V2.
type Project struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ListProjects fetches all projects accessible from the repository.
// This includes both organization projects and user projects.
// Returns an empty list (not an error) if projects are not available or scope is missing.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	owner, repo := splitRepo(c.repo)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("invalid repository format")
	}

	// Try to get projects from the repository owner (org or user)
	// First try as organization
	query := `query($owner: String!) {
  organization(login: $owner) {
    projectsV2(first: 100) {
      nodes {
        id
        title
      }
    }
  }
}`

	args := []string{"api", "graphql",
		"-f", fmt.Sprintf("query=%s", query),
		"-F", fmt.Sprintf("owner=%s", owner),
	}

	out, err := c.runner.Run(ctx, "gh", args...)
	if err != nil {
		// Try as user instead
		return c.listUserProjects(ctx, owner)
	}

	var resp struct {
		Data struct {
			Organization struct {
				ProjectsV2 struct {
					Nodes []struct {
						ID    string `json:"id"`
						Title string `json:"title"`
					} `json:"nodes"`
				} `json:"projectsV2"`
			} `json:"organization"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}

	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, nil
	}

	// Check for scope errors
	for _, e := range resp.Errors {
		if e.Type == "INSUFFICIENT_SCOPES" {
			return nil, ErrMissingProjectScope
		}
	}

	if len(resp.Errors) > 0 {
		// Try as user
		return c.listUserProjects(ctx, owner)
	}

	var projects []Project
	for _, p := range resp.Data.Organization.ProjectsV2.Nodes {
		projects = append(projects, Project{
			ID:    p.ID,
			Title: p.Title,
		})
	}

	return projects, nil
}

func (c *Client) listUserProjects(ctx context.Context, login string) ([]Project, error) {
	query := `query($login: String!) {
  user(login: $login) {
    projectsV2(first: 100) {
      nodes {
        id
        title
      }
    }
  }
}`

	args := []string{"api", "graphql",
		"-f", fmt.Sprintf("query=%s", query),
		"-F", fmt.Sprintf("login=%s", login),
	}

	out, err := c.runner.Run(ctx, "gh", args...)
	if err != nil {
		return nil, nil
	}

	var resp struct {
		Data struct {
			User struct {
				ProjectsV2 struct {
					Nodes []struct {
						ID    string `json:"id"`
						Title string `json:"title"`
					} `json:"nodes"`
				} `json:"projectsV2"`
			} `json:"user"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}

	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, nil
	}

	// Check for scope errors
	for _, e := range resp.Errors {
		if e.Type == "INSUFFICIENT_SCOPES" {
			return nil, ErrMissingProjectScope
		}
	}

	var projects []Project
	for _, p := range resp.Data.User.ProjectsV2.Nodes {
		projects = append(projects, Project{
			ID:    p.ID,
			Title: p.Title,
		})
	}

	return projects, nil
}

// AddToProject adds an issue to a project.
// Returns nil if successful, or an error (including scope errors).
func (c *Client) AddToProject(ctx context.Context, issueNumber string, projectID string) error {
	issueNodeID, err := c.GetIssueNodeID(ctx, issueNumber)
	if err != nil {
		return fmt.Errorf("failed to get issue node ID: %w", err)
	}

	mutation := `mutation($projectId: ID!, $contentId: ID!) {
  addProjectV2ItemById(input: {projectId: $projectId, contentId: $contentId}) {
    item { id }
  }
}`

	args := []string{"api", "graphql",
		"-f", fmt.Sprintf("query=%s", mutation),
		"-f", fmt.Sprintf("projectId=%s", projectID),
		"-f", fmt.Sprintf("contentId=%s", issueNodeID),
	}

	out, err := c.runner.Run(ctx, "gh", args...)
	if err != nil {
		// Check if it's a scope error
		if strings.Contains(err.Error(), "INSUFFICIENT_SCOPES") {
			return fmt.Errorf("missing 'project' scope - run 'gh auth refresh -s project' to enable")
		}
		return err
	}

	var resp struct {
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return fmt.Errorf("failed to parse GraphQL response: %w", err)
	}

	for _, e := range resp.Errors {
		if e.Type == "INSUFFICIENT_SCOPES" {
			return fmt.Errorf("missing 'project' scope - run 'gh auth refresh -s project' to enable")
		}
	}

	if len(resp.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", resp.Errors[0].Message)
	}

	return nil
}

// RemoveFromProject removes an issue from a project.
// Returns nil if successful, or an error (including scope errors).
func (c *Client) RemoveFromProject(ctx context.Context, issueNumber string, projectID string) error {
	issueNodeID, err := c.GetIssueNodeID(ctx, issueNumber)
	if err != nil {
		return fmt.Errorf("failed to get issue node ID: %w", err)
	}

	// First, we need to find the project item ID for this issue in this project
	query := `query($issueId: ID!) {
  node(id: $issueId) {
    ... on Issue {
      projectItems(first: 100) {
        nodes {
          id
          project { id }
        }
      }
    }
  }
}`

	args := []string{"api", "graphql",
		"-f", fmt.Sprintf("query=%s", query),
		"-f", fmt.Sprintf("issueId=%s", issueNodeID),
	}

	out, err := c.runner.Run(ctx, "gh", args...)
	if err != nil {
		return err
	}

	var queryResp struct {
		Data struct {
			Node struct {
				ProjectItems struct {
					Nodes []struct {
						ID      string `json:"id"`
						Project struct {
							ID string `json:"id"`
						} `json:"project"`
					} `json:"nodes"`
				} `json:"projectItems"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}

	if err := json.Unmarshal([]byte(out), &queryResp); err != nil {
		return fmt.Errorf("failed to parse GraphQL response: %w", err)
	}

	// Find the item ID for this project
	var itemID string
	for _, item := range queryResp.Data.Node.ProjectItems.Nodes {
		if item.Project.ID == projectID {
			itemID = item.ID
			break
		}
	}

	if itemID == "" {
		// Issue is not in this project, nothing to do
		return nil
	}

	// Now delete the item
	mutation := `mutation($projectId: ID!, $itemId: ID!) {
  deleteProjectV2Item(input: {projectId: $projectId, itemId: $itemId}) {
    deletedItemId
  }
}`

	args = []string{"api", "graphql",
		"-f", fmt.Sprintf("query=%s", mutation),
		"-f", fmt.Sprintf("projectId=%s", projectID),
		"-f", fmt.Sprintf("itemId=%s", itemID),
	}

	out, err = c.runner.Run(ctx, "gh", args...)
	if err != nil {
		if strings.Contains(err.Error(), "INSUFFICIENT_SCOPES") {
			return fmt.Errorf("missing 'project' scope - run 'gh auth refresh -s project' to enable")
		}
		return err
	}

	var mutResp struct {
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &mutResp); err != nil {
		return fmt.Errorf("failed to parse GraphQL response: %w", err)
	}

	for _, e := range mutResp.Errors {
		if e.Type == "INSUFFICIENT_SCOPES" {
			return fmt.Errorf("missing 'project' scope - run 'gh auth refresh -s project' to enable")
		}
	}

	if len(mutResp.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", mutResp.Errors[0].Message)
	}

	return nil
}

// SyncProjects syncs the project memberships for an issue.
// It compares the desired state (from local issue) with the current remote state
// and adds/removes project memberships as needed.
// Returns nil on success. Scope errors are logged but don't cause failure.
func (c *Client) SyncProjects(ctx context.Context, issueNumber string, localProjects []string, knownProjects map[string]string) error {
	// Get current project memberships
	issueNodeID, err := c.GetIssueNodeID(ctx, issueNumber)
	if err != nil {
		return fmt.Errorf("failed to get issue node ID: %w", err)
	}

	query := `query($issueId: ID!) {
  node(id: $issueId) {
    ... on Issue {
      projectItems(first: 100) {
        nodes {
          project {
            id
            title
          }
        }
      }
    }
  }
}`

	args := []string{"api", "graphql",
		"-f", fmt.Sprintf("query=%s", query),
		"-f", fmt.Sprintf("issueId=%s", issueNodeID),
	}

	out, err := c.runner.Run(ctx, "gh", args...)
	if err != nil {
		return nil // Graceful fallback
	}

	var resp struct {
		Data struct {
			Node struct {
				ProjectItems struct {
					Nodes []struct {
						Project struct {
							ID    string `json:"id"`
							Title string `json:"title"`
						} `json:"project"`
					} `json:"nodes"`
				} `json:"projectItems"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Type string `json:"type"`
		} `json:"errors"`
	}

	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil
	}

	// Check for scope errors
	for _, e := range resp.Errors {
		if e.Type == "INSUFFICIENT_SCOPES" {
			return nil
		}
	}

	// Build sets for comparison
	remoteProjects := make(map[string]string) // title -> id
	for _, item := range resp.Data.Node.ProjectItems.Nodes {
		remoteProjects[item.Project.Title] = item.Project.ID
	}

	localSet := make(map[string]struct{})
	for _, p := range localProjects {
		localSet[p] = struct{}{}
	}

	// Add to new projects
	for _, title := range localProjects {
		if _, inRemote := remoteProjects[title]; !inRemote {
			if projectID, known := knownProjects[strings.ToLower(title)]; known {
				if err := c.AddToProject(ctx, issueNumber, projectID); err != nil {
					// Return error - caller will log it
					return err
				}
			}
		}
	}

	// Remove from old projects
	for title, projectID := range remoteProjects {
		if _, inLocal := localSet[title]; !inLocal {
			if err := c.RemoveFromProject(ctx, issueNumber, projectID); err != nil {
				return err
			}
		}
	}

	return nil
}

// CreateComment posts a comment on an issue.
func (c *Client) CreateComment(ctx context.Context, issueNumber string, body string) error {
	args := []string{"issue", "comment", issueNumber, "--body", body}
	_, err := c.runner.Run(ctx, "gh", c.withRepo(args)...)
	return err
}
