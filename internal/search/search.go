// Package search implements GitHub-style issue search query parsing.
package search

import (
	"sort"
	"strings"

	"github.com/mitsuhiko/gh-issue-sync/internal/issue"
)

// Query represents a parsed search query.
type Query struct {
	// Text is the free-text portion to search in title/body
	Text string

	// Qualifiers
	State       string   // "open" or "closed"
	Labels      []string // label:X
	NoLabel     bool     // no:label
	Assignees   []string // assignee:X
	NoAssignee  bool     // no:assignee
	Authors     []string // author:X
	Milestones  []string // milestone:X
	NoMilestone bool     // no:milestone
	Mentions    []string // mentions:X
	Types       []string // type:X
	NoType      bool     // no:type
	Projects    []string // project:X
	NoProject   bool     // no:project

	// Sort
	SortField string // "created", "updated", "comments" (default: "created")
	SortAsc   bool   // true for ascending, false for descending (default: false = desc)
}

// Parse parses a GitHub-style search query string.
// Examples:
//   - "error no:assignee sort:created-asc"
//   - "label:bug label:urgent is:open"
//   - "fix login author:alice"
func Parse(query string) Query {
	q := Query{
		SortField: "created",
		SortAsc:   false,
	}

	var textParts []string
	tokens := tokenize(query)

	for _, tok := range tokens {
		// Handle qualifier:value syntax
		if idx := strings.Index(tok, ":"); idx > 0 {
			qualifier := strings.ToLower(tok[:idx])
			value := tok[idx+1:]

			// Handle quoted values
			value = strings.Trim(value, "\"'")

			switch qualifier {
			case "is":
				switch strings.ToLower(value) {
				case "open":
					q.State = "open"
				case "closed":
					q.State = "closed"
				}
			case "state":
				switch strings.ToLower(value) {
				case "open":
					q.State = "open"
				case "closed":
					q.State = "closed"
				}
			case "label":
				q.Labels = append(q.Labels, value)
			case "assignee":
				q.Assignees = append(q.Assignees, value)
			case "author":
				q.Authors = append(q.Authors, value)
			case "milestone":
				q.Milestones = append(q.Milestones, value)
			case "mentions":
				q.Mentions = append(q.Mentions, value)
			case "type":
				q.Types = append(q.Types, value)
			case "project":
				q.Projects = append(q.Projects, value)
			case "no":
				switch strings.ToLower(value) {
				case "label":
					q.NoLabel = true
				case "assignee":
					q.NoAssignee = true
				case "milestone":
					q.NoMilestone = true
				case "type":
					q.NoType = true
				case "project":
					q.NoProject = true
				}
			case "sort":
				parseSortValue(&q, value)
			default:
				// Unknown qualifier, treat as text
				textParts = append(textParts, tok)
			}
		} else {
			// Plain text token
			textParts = append(textParts, tok)
		}
	}

	q.Text = strings.Join(textParts, " ")
	return q
}

// parseSortValue parses sort values like "created-asc", "updated-desc", "comments"
func parseSortValue(q *Query, value string) {
	value = strings.ToLower(value)

	// Check for -asc or -desc suffix
	if strings.HasSuffix(value, "-asc") {
		q.SortAsc = true
		value = strings.TrimSuffix(value, "-asc")
	} else if strings.HasSuffix(value, "-desc") {
		q.SortAsc = false
		value = strings.TrimSuffix(value, "-desc")
	}

	// Map sort field
	switch value {
	case "created":
		q.SortField = "created"
	case "updated":
		q.SortField = "updated"
	case "comments":
		q.SortField = "comments"
	}
}

// tokenize splits the query into tokens, respecting quoted strings
func tokenize(query string) []string {
	var tokens []string
	var current strings.Builder
	inQuote := false
	quoteChar := byte(0)

	for i := 0; i < len(query); i++ {
		c := query[i]

		if inQuote {
			if c == quoteChar {
				inQuote = false
				current.WriteByte(c)
			} else {
				current.WriteByte(c)
			}
		} else if c == '"' || c == '\'' {
			inQuote = true
			quoteChar = c
			current.WriteByte(c)
		} else if c == ' ' || c == '\t' {
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		} else {
			current.WriteByte(c)
		}
	}

	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	return tokens
}

// IssueData represents the data needed for filtering and sorting issues.
// This is an abstraction over IssueFile to allow the search package to work
// without depending on the app package.
type IssueData struct {
	Number    issue.IssueNumber
	Title     string
	Body      string
	State     string
	Labels    []string
	Assignees []string
	Author    string
	Milestone string
	IssueType string
	Projects  []string
	SyncedAt  *int64 // Unix timestamp, nil if not synced
	CreatedAt *int64 // Unix timestamp from GitHub
	UpdatedAt *int64 // Unix timestamp from GitHub
}

// Match returns true if the issue matches the query.
func (q *Query) Match(iss IssueData) bool {
	// State filter
	if q.State != "" && !strings.EqualFold(iss.State, q.State) {
		return false
	}

	// Label filters
	if q.NoLabel && len(iss.Labels) > 0 {
		return false
	}
	for _, wantLabel := range q.Labels {
		if !containsIgnoreCase(iss.Labels, wantLabel) {
			return false
		}
	}

	// Assignee filters
	if q.NoAssignee && len(iss.Assignees) > 0 {
		return false
	}
	for _, wantAssignee := range q.Assignees {
		if !containsIgnoreCase(iss.Assignees, wantAssignee) {
			return false
		}
	}

	// Author filter
	for _, wantAuthor := range q.Authors {
		if !strings.EqualFold(iss.Author, wantAuthor) {
			return false
		}
	}

	// Milestone filters
	if q.NoMilestone && iss.Milestone != "" {
		return false
	}
	for _, wantMilestone := range q.Milestones {
		if !strings.EqualFold(iss.Milestone, wantMilestone) {
			return false
		}
	}

	// Type filters
	if q.NoType && iss.IssueType != "" {
		return false
	}
	for _, wantType := range q.Types {
		if !strings.EqualFold(iss.IssueType, wantType) {
			return false
		}
	}

	// Project filters
	if q.NoProject && len(iss.Projects) > 0 {
		return false
	}
	for _, wantProject := range q.Projects {
		if !containsIgnoreCase(iss.Projects, wantProject) {
			return false
		}
	}

	// Mentions filter (search for @username in body)
	for _, mention := range q.Mentions {
		searchMention := "@" + mention
		if !strings.Contains(strings.ToLower(iss.Body), strings.ToLower(searchMention)) {
			return false
		}
	}

	// Free text search (in title and body)
	if q.Text != "" {
		textLower := strings.ToLower(q.Text)
		titleLower := strings.ToLower(iss.Title)
		bodyLower := strings.ToLower(iss.Body)
		if !strings.Contains(titleLower, textLower) && !strings.Contains(bodyLower, textLower) {
			return false
		}
	}

	return true
}

// Sort sorts issues according to the query's sort specification.
func (q *Query) Sort(issues []IssueData) {
	sort.SliceStable(issues, func(i, j int) bool {
		// Select timestamp based on sort field
		var ti, tj *int64
		switch q.SortField {
		case "created":
			ti, tj = issues[i].CreatedAt, issues[j].CreatedAt
		case "updated":
			ti, tj = issues[i].UpdatedAt, issues[j].UpdatedAt
		default:
			// Default to created for unknown sort fields
			ti, tj = issues[i].CreatedAt, issues[j].CreatedAt
		}

		// Issues without timestamps (local issues) always go to the end
		if ti == nil && tj == nil {
			return false
		}
		if ti == nil {
			return false // i goes after j
		}
		if tj == nil {
			return true // i goes before j
		}

		// Both have timestamps, compare
		var cmp int
		if *ti < *tj {
			cmp = -1
		} else if *ti > *tj {
			cmp = 1
		} else {
			cmp = 0
		}

		if q.SortAsc {
			return cmp < 0
		}
		return cmp > 0
	})
}

func containsIgnoreCase(slice []string, target string) bool {
	for _, s := range slice {
		if strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}
