package app

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/term"
	"github.com/mitsuhiko/gh-issue-sync/internal/issue"
	"github.com/mitsuhiko/gh-issue-sync/internal/theme"
)

func (a *App) formatChangeLines(oldIssue, newIssue issue.Issue, labelColors map[string]string) []string {
	oldIssue = issue.Normalize(oldIssue)
	newIssue = issue.Normalize(newIssue)
	t := a.Theme

	lines := []string{}
	if oldIssue.Title != newIssue.Title {
		// Use inline word diff for title
		titleDiff := a.formatInlineWordDiff(oldIssue.Title, newIssue.Title)
		lines = append(lines, "    "+t.Styler().Fg(t.FieldName, "title: ")+titleDiff)
	}
	if oldIssue.Body != newIssue.Body {
		// Show body change as a simple info line, not as old->new since it's just a summary
		lines = append(lines, "    "+t.Styler().Fg(t.FieldName, "body: ")+t.MutedText(fmt.Sprintf("changed (%s -> %s)", formatBodySummary(oldIssue.Body), formatBodySummary(newIssue.Body))))
	}
	if !stringSlicesEqual(oldIssue.Labels, newIssue.Labels) {
		oldLabels := labelsToTheme(oldIssue.Labels, labelColors)
		newLabels := labelsToTheme(newIssue.Labels, labelColors)
		added, removed := diffLabelColors(oldLabels, newLabels)
		if len(added) > 0 || len(removed) > 0 {
			lines = append(lines, "    "+t.Styler().Fg(t.FieldName, "labels: ")+t.FormatLabelChange(added, removed))
		}
	}
	if !stringSlicesEqual(oldIssue.Assignees, newIssue.Assignees) {
		lines = append(lines, t.FormatChange("assignees", formatStringList(oldIssue.Assignees), formatStringList(newIssue.Assignees)))
	}
	if oldIssue.Milestone != newIssue.Milestone {
		// Use inline word diff for milestone
		if oldIssue.Milestone == "" {
			lines = append(lines, t.FormatChange("milestone", "<none>", fmt.Sprintf("%q", newIssue.Milestone)))
		} else if newIssue.Milestone == "" {
			lines = append(lines, t.FormatChange("milestone", fmt.Sprintf("%q", oldIssue.Milestone), "<none>"))
		} else {
			milestoneDiff := a.formatInlineWordDiff(oldIssue.Milestone, newIssue.Milestone)
			lines = append(lines, "    "+t.Styler().Fg(t.FieldName, "milestone: ")+milestoneDiff)
		}
	}
	if oldIssue.IssueType != newIssue.IssueType {
		// Use inline word diff for issue type
		if oldIssue.IssueType == "" {
			lines = append(lines, t.FormatChange("type", "<none>", fmt.Sprintf("%q", newIssue.IssueType)))
		} else if newIssue.IssueType == "" {
			lines = append(lines, t.FormatChange("type", fmt.Sprintf("%q", oldIssue.IssueType), "<none>"))
		} else {
			typeDiff := a.formatInlineWordDiff(oldIssue.IssueType, newIssue.IssueType)
			lines = append(lines, "    "+t.Styler().Fg(t.FieldName, "type: ")+typeDiff)
		}
	}
	if !stringSlicesEqual(oldIssue.Projects, newIssue.Projects) {
		lines = append(lines, t.FormatChange("projects", formatStringList(oldIssue.Projects), formatStringList(newIssue.Projects)))
	}
	if oldIssue.State != newIssue.State {
		lines = append(lines, t.FormatChange("state", formatOptionalString(oldIssue.State), formatOptionalString(newIssue.State)))
	}
	if normalizeOptional(oldIssue.StateReason) != normalizeOptional(newIssue.StateReason) {
		lines = append(lines, t.FormatChange("state_reason", formatOptionalStringPtr(oldIssue.StateReason), formatOptionalStringPtr(newIssue.StateReason)))
	}
	return lines
}

func labelsToTheme(labels []string, colors map[string]string) []theme.LabelColor {
	result := make([]theme.LabelColor, 0, len(labels))
	for _, name := range labels {
		// Look up by lowercase for case-insensitive matching
		color := colors[strings.ToLower(name)]
		if color == "" {
			color = "6b7280" // default gray if no color
		}
		result = append(result, theme.LabelColor{Name: name, Color: color})
	}
	return result
}

func diffLabelColors(old, new []theme.LabelColor) (added, removed []theme.LabelColor) {
	oldSet := make(map[string]theme.LabelColor)
	for _, l := range old {
		oldSet[l.Name] = l
	}
	newSet := make(map[string]theme.LabelColor)
	for _, l := range new {
		newSet[l.Name] = l
	}
	for _, l := range new {
		if _, ok := oldSet[l.Name]; !ok {
			added = append(added, l)
		}
	}
	for _, l := range old {
		if _, ok := newSet[l.Name]; !ok {
			removed = append(removed, l)
		}
	}
	return
}

func formatBodySummary(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return "<empty>"
	}
	return fmt.Sprintf("%d chars", len(body))
}

func formatOptionalString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "<none>"
	}
	return fmt.Sprintf("%q", value)
}

func formatOptionalStringPtr(value *string) string {
	if value == nil {
		return "<none>"
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return "<none>"
	}
	return fmt.Sprintf("%q", trimmed)
}

func formatStringList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, fmt.Sprintf("%q", item))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// formatRelativeTime formats a time as a human-readable relative string
func formatRelativeTime(now time.Time, t time.Time) string {
	diff := now.Sub(t)
	if diff < 0 {
		diff = -diff
	}

	switch {
	case diff < time.Minute:
		return "just now"
	case diff < time.Hour:
		mins := int(diff.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	case diff < 24*time.Hour:
		hours := int(diff.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	case diff < 7*24*time.Hour:
		days := int(diff.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	case diff < 30*24*time.Hour:
		weeks := int(diff.Hours() / 24 / 7)
		if weeks == 1 {
			return "1 week ago"
		}
		return fmt.Sprintf("%d weeks ago", weeks)
	case diff < 365*24*time.Hour:
		months := int(diff.Hours() / 24 / 30)
		if months == 1 {
			return "1 month ago"
		}
		return fmt.Sprintf("%d months ago", months)
	default:
		years := int(diff.Hours() / 24 / 365)
		if years == 1 {
			return "1 year ago"
		}
		return fmt.Sprintf("%d years ago", years)
	}
}

// ansiPattern matches ANSI escape sequences
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripAnsi removes ANSI escape sequences from a string
func stripAnsi(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

// padRight pads a string (ignoring ANSI codes) to the given width
func padRight(s string, width int) string {
	visible := len(stripAnsi(s))
	if visible >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visible)
}

// truncateAnsi truncates a string with ANSI codes to fit within max visible characters.
// It appends a reset sequence if the string was truncated mid-styling.
func truncateAnsi(s string, max int, reset string) string {
	if max <= 0 {
		return ""
	}
	var b strings.Builder
	visible := 0
	for i := 0; i < len(s); {
		// Handle ANSI escape sequences
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				j++
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		// Handle regular runes
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			size = 1
		}
		if visible+1 > max {
			if reset != "" {
				b.WriteString(reset)
			}
			return b.String()
		}
		b.WriteRune(r)
		visible++
		i += size
	}
	return b.String()
}

// getTerminalWidth returns the terminal width for the given writer, or 0 if not a terminal.
// Subtracts 1 to avoid wrapping issues on the last column.
func getTerminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	if !term.IsTerminal(f.Fd()) {
		return 0
	}
	width, _, err := term.GetSize(f.Fd())
	if err != nil || width <= 0 {
		return 0
	}
	return width - 1 // Reserve last column to avoid wrapping
}
