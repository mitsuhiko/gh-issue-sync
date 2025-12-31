package ghcli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// BatchIssueUpdate represents updates to apply to a single issue.
type BatchIssueUpdate struct {
	Number          string   // Issue number
	Title           *string  // New title (nil = no change)
	Body            *string  // New body (nil = no change)
	Milestone       *string  // New milestone title (nil = no change, empty string = remove)
	Labels          []string // Final set of labels (nil = no change)
	Assignees       []string // Final set of assignees (nil = no change)
	ClearMilestone  bool     // If true, remove milestone
	ClearLabels     bool     // If true and Labels is nil, remove all labels
	ClearAssignees  bool     // If true and Assignees is nil, remove all assignees
}

// BatchUpdateResult contains the result of a batch update operation.
type BatchUpdateResult struct {
	Updated []string          // Issue numbers that were updated
	Errors  map[string]string // Issue number -> error message
}

// BatchEditIssues updates multiple issues in a single GraphQL call.
// This is much faster than calling EditIssue for each issue individually.
// Note: This only handles title, body, milestone, labels, and assignees.
// State changes, relationships, issue types, and projects must be handled separately.
func (c *Client) BatchEditIssues(ctx context.Context, updates []BatchIssueUpdate) (BatchUpdateResult, error) {
	result := BatchUpdateResult{
		Errors: make(map[string]string),
	}

	if len(updates) == 0 {
		return result, nil
	}

	owner, repo := splitRepo(c.repo)
	if owner == "" || repo == "" {
		return result, fmt.Errorf("invalid repository format")
	}

	// First, fetch all the IDs we need (issue node IDs, milestone IDs, label IDs, user IDs)
	issueNumbers := make([]string, 0, len(updates))
	milestoneNames := make(map[string]struct{})
	labelNames := make(map[string]struct{})
	userLogins := make(map[string]struct{})

	for _, u := range updates {
		issueNumbers = append(issueNumbers, u.Number)
		if u.Milestone != nil && *u.Milestone != "" {
			milestoneNames[*u.Milestone] = struct{}{}
		}
		for _, l := range u.Labels {
			labelNames[l] = struct{}{}
		}
		for _, a := range u.Assignees {
			userLogins[a] = struct{}{}
		}
	}

	// Build lookup query
	lookups, err := c.fetchBatchLookups(ctx, owner, repo, issueNumbers, milestoneNames, labelNames, userLogins)
	if err != nil {
		return result, fmt.Errorf("failed to fetch IDs: %w", err)
	}

	// Build the batch mutation
	var mutations []string
	for i, u := range updates {
		issueID, ok := lookups.IssueIDs[u.Number]
		if !ok {
			result.Errors[u.Number] = "issue not found"
			continue
		}

		var inputParts []string
		inputParts = append(inputParts, fmt.Sprintf("id: %q", issueID))

		if u.Title != nil {
			inputParts = append(inputParts, fmt.Sprintf("title: %q", *u.Title))
		}
		if u.Body != nil {
			inputParts = append(inputParts, fmt.Sprintf("body: %q", escapeGraphQLString(*u.Body)))
		}

		// Handle milestone
		if u.Milestone != nil {
			if *u.Milestone == "" || u.ClearMilestone {
				inputParts = append(inputParts, "milestoneId: null")
			} else if milestoneID, ok := lookups.MilestoneIDs[*u.Milestone]; ok {
				inputParts = append(inputParts, fmt.Sprintf("milestoneId: %q", milestoneID))
			} else {
				result.Errors[u.Number] = fmt.Sprintf("milestone %q not found", *u.Milestone)
				continue
			}
		}

		// Handle labels - GraphQL requires the full set of label IDs
		if u.Labels != nil || u.ClearLabels {
			var labelIDs []string
			for _, l := range u.Labels {
				if id, ok := lookups.LabelIDs[l]; ok {
					labelIDs = append(labelIDs, fmt.Sprintf("%q", id))
				} else {
					result.Errors[u.Number] = fmt.Sprintf("label %q not found", l)
					continue
				}
			}
			inputParts = append(inputParts, fmt.Sprintf("labelIds: [%s]", strings.Join(labelIDs, ", ")))
		}

		// Handle assignees - GraphQL requires the full set of assignee IDs
		if u.Assignees != nil || u.ClearAssignees {
			var assigneeIDs []string
			for _, a := range u.Assignees {
				if id, ok := lookups.UserIDs[a]; ok {
					assigneeIDs = append(assigneeIDs, fmt.Sprintf("%q", id))
				} else {
					result.Errors[u.Number] = fmt.Sprintf("user %q not found", a)
					continue
				}
			}
			inputParts = append(inputParts, fmt.Sprintf("assigneeIds: [%s]", strings.Join(assigneeIDs, ", ")))
		}

		mutations = append(mutations, fmt.Sprintf(`  update%d: updateIssue(input: {%s}) { issue { number } }`,
			i, strings.Join(inputParts, ", ")))
	}

	if len(mutations) == 0 {
		return result, nil
	}

	query := fmt.Sprintf("mutation {\n%s\n}", strings.Join(mutations, "\n"))

	args := []string{"api", "graphql", "-f", fmt.Sprintf("query=%s", query)}
	out, err := c.runner.Run(ctx, "gh", args...)
	if err != nil {
		return result, fmt.Errorf("batch update failed: %w", err)
	}

	// Parse response
	var resp struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string   `json:"message"`
			Path    []string `json:"path"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return result, fmt.Errorf("failed to parse response: %w", err)
	}

	// Map errors to issue numbers
	for _, e := range resp.Errors {
		if len(e.Path) > 0 {
			// Path is like ["update0"]
			alias := e.Path[0]
			if strings.HasPrefix(alias, "update") {
				if idx, err := strconv.Atoi(alias[6:]); err == nil && idx < len(updates) {
					result.Errors[updates[idx].Number] = e.Message
				}
			}
		}
	}

	// Mark successful updates
	for i, u := range updates {
		alias := fmt.Sprintf("update%d", i)
		if _, hasError := result.Errors[u.Number]; !hasError {
			if _, inData := resp.Data[alias]; inData {
				result.Updated = append(result.Updated, u.Number)
			}
		}
	}

	return result, nil
}

// batchLookups holds the ID mappings needed for batch updates.
type batchLookups struct {
	IssueIDs     map[string]string // issue number -> node ID
	MilestoneIDs map[string]string // milestone title -> node ID
	LabelIDs     map[string]string // label name -> node ID
	UserIDs      map[string]string // user login -> node ID
}

// fetchBatchLookups fetches all the IDs needed for batch updates in a single query.
func (c *Client) fetchBatchLookups(ctx context.Context, owner, repo string, issueNumbers []string, milestones, labels, users map[string]struct{}) (batchLookups, error) {
	lookups := batchLookups{
		IssueIDs:     make(map[string]string),
		MilestoneIDs: make(map[string]string),
		LabelIDs:     make(map[string]string),
		UserIDs:      make(map[string]string),
	}

	// Build issue queries
	var issueQueries []string
	for i, num := range issueNumbers {
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		issueQueries = append(issueQueries, fmt.Sprintf(`issue%d: issue(number: %d) { id number }`, i, n))
	}

	// Build user queries
	var userQueries []string
	userList := make([]string, 0, len(users))
	for login := range users {
		userList = append(userList, login)
	}
	for i, login := range userList {
		userQueries = append(userQueries, fmt.Sprintf(`user%d: user(login: %q) { id login }`, i, login))
	}

	// Build the combined query
	// Note: milestones and labels are fetched from the repository
	query := fmt.Sprintf(`query($owner: String!, $repo: String!) {
  repository(owner: $owner, name: $repo) {
    %s
    milestones(first: 100, states: [OPEN, CLOSED]) {
      nodes { id title }
    }
    labels(first: 100) {
      nodes { id name }
    }
  }
  %s
}`, strings.Join(issueQueries, "\n    "), strings.Join(userQueries, "\n  "))

	args := []string{"api", "graphql",
		"-f", fmt.Sprintf("query=%s", query),
		"-F", fmt.Sprintf("owner=%s", owner),
		"-F", fmt.Sprintf("repo=%s", repo),
	}

	out, err := c.runner.Run(ctx, "gh", args...)
	if err != nil {
		return lookups, err
	}

	// First unmarshal to get top-level structure
	var rawResp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &rawResp); err != nil {
		return lookups, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(rawResp.Errors) > 0 {
		return lookups, fmt.Errorf("GraphQL error: %s", rawResp.Errors[0].Message)
	}

	// Parse the data section
	var data map[string]json.RawMessage
	if err := json.Unmarshal(rawResp.Data, &data); err != nil {
		return lookups, fmt.Errorf("failed to parse data: %w", err)
	}

	// Parse repository data
	if repoData, ok := data["repository"]; ok {
		var repoMap map[string]json.RawMessage
		if err := json.Unmarshal(repoData, &repoMap); err == nil {
			// Parse issues
			for key, val := range repoMap {
				if strings.HasPrefix(key, "issue") {
					var issueData struct {
						ID     string `json:"id"`
						Number int    `json:"number"`
					}
					if err := json.Unmarshal(val, &issueData); err == nil && issueData.ID != "" {
						lookups.IssueIDs[strconv.Itoa(issueData.Number)] = issueData.ID
					}
				}
			}

			// Parse milestones
			if milestonesData, ok := repoMap["milestones"]; ok {
				var milestones struct {
					Nodes []struct {
						ID    string `json:"id"`
						Title string `json:"title"`
					} `json:"nodes"`
				}
				if err := json.Unmarshal(milestonesData, &milestones); err == nil {
					for _, m := range milestones.Nodes {
						lookups.MilestoneIDs[m.Title] = m.ID
					}
				}
			}

			// Parse labels
			if labelsData, ok := repoMap["labels"]; ok {
				var labels struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
				}
				if err := json.Unmarshal(labelsData, &labels); err == nil {
					for _, l := range labels.Nodes {
						lookups.LabelIDs[l.Name] = l.ID
					}
				}
			}
		}
	}

	// Parse user data
	for key, val := range data {
		if strings.HasPrefix(key, "user") {
			var userData struct {
				ID    string `json:"id"`
				Login string `json:"login"`
			}
			if err := json.Unmarshal(val, &userData); err == nil && userData.ID != "" {
				lookups.UserIDs[userData.Login] = userData.ID
			}
		}
	}

	return lookups, nil
}

// escapeGraphQLString escapes a string for use in a GraphQL query.
// This handles newlines, quotes, and backslashes.
func escapeGraphQLString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}
