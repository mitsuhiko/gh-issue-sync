// Package theme provides a semantic color theme for terminal output.
package theme

import (
	"github.com/mitsuhiko/gh-issue-sync/internal/termcolor"
)

// Theme provides semantic colors for terminal output.
type Theme struct {
	styler *termcolor.Styler

	// Semantic colors
	Accent  termcolor.Color // Primary accent (cyan)
	Success termcolor.Color // Success/added (green)
	Error   termcolor.Color // Error/removed (red)
	Warning termcolor.Color // Warning (orange)
	Muted   termcolor.Color // Muted/secondary text (gray)
	Dim     termcolor.Color // Very dim text

	// Change indicators
	Added   termcolor.Color // Added items (green)
	Removed termcolor.Color // Removed items (red)
	Changed termcolor.Color // Changed items (yellow/orange)

	// Issue-specific colors
	IssueNumber termcolor.Color // Issue number (#123)
	IssueTitle  termcolor.Color // Issue title
	LabelText   termcolor.Color // Text on label backgrounds (calculated per-label)

	// Field names/values
	FieldName  termcolor.Color // Field names (title:, labels:, etc.)
	OldValue   termcolor.Color // Old values in diffs
	NewValue   termcolor.Color // New values in diffs
	Arrow      termcolor.Color // Arrow in diffs (->)
	StatusChar termcolor.Color // Status characters (A, U, M, etc.)
}

// Default returns the default theme with nice colors.
func Default() *Theme {
	return &Theme{
		styler: termcolor.DefaultStyler(),

		// Core semantic colors
		Accent:  termcolor.MustParseHex("#00b4d8"), // Nice cyan
		Success: termcolor.MustParseHex("#22c55e"), // Green
		Error:   termcolor.MustParseHex("#ef4444"), // Red
		Warning: termcolor.MustParseHex("#f97316"), // Orange
		Muted:   termcolor.MustParseHex("#6b7280"), // Gray
		Dim:     termcolor.MustParseHex("#4b5563"), // Darker gray

		// Change indicators
		Added:   termcolor.MustParseHex("#22c55e"), // Green
		Removed: termcolor.MustParseHex("#ef4444"), // Red
		Changed: termcolor.MustParseHex("#eab308"), // Yellow

		// Issue-specific
		IssueNumber: termcolor.MustParseHex("#00b4d8"), // Cyan
		IssueTitle:  termcolor.MustParseHex("#f8fafc"), // Near white

		// Diff display
		FieldName:  termcolor.MustParseHex("#94a3b8"), // Slate gray
		OldValue:   termcolor.MustParseHex("#ff6b8a"), // Coral pink
		NewValue:   termcolor.MustParseHex("#36d399"), // Mint teal
		Arrow:      termcolor.MustParseHex("#64748b"), // Dim slate
		StatusChar: termcolor.MustParseHex("#22d3ee"), // Cyan
	}
}

// Styler returns the underlying termcolor Styler.
func (t *Theme) Styler() *termcolor.Styler {
	return t.styler
}

// Fg returns text with the given foreground color.
func (t *Theme) Fg(c termcolor.Color, text string) string {
	return t.styler.Fg(c, text)
}

// Bold returns bold text.
func (t *Theme) Bold(text string) string {
	return t.styler.Bold(text)
}

// Dim returns dim text.
func (t *Theme) DimText(text string) string {
	return t.styler.Dim(text)
}

// Status formatting helpers

// FormatStatus formats a status character (A, U, M, D, R).
func (t *Theme) FormatStatus(status string) string {
	var c termcolor.Color
	switch status {
	case "A":
		c = t.Added
	case "U", "M":
		c = t.Changed
	case "D":
		c = t.Removed
	case "R":
		c = t.Changed // Restored
	default:
		c = t.Muted
	}
	return t.styler.Fg(c, status)
}

// FormatIssueHeader formats an issue header line like "U Issue #123: Title".
func (t *Theme) FormatIssueHeader(status, number, title string) string {
	return t.FormatStatus(status) + " Issue " +
		t.styler.Fg(t.IssueNumber, "#"+number) + ": " +
		t.styler.Bold(title)
}

// FormatChange formats a change line like "  title: "old" -> "new"".
// Old values are shown with strikethrough, new values with underline.
func (t *Theme) FormatChange(field, oldVal, newVal string) string {
	return "    " +
		t.styler.Fg(t.FieldName, field+": ") +
		t.styler.FgStrikethrough(t.OldValue, oldVal) +
		t.styler.Fg(t.Arrow, " -> ") +
		t.styler.FgUnderline(t.NewValue, newVal)
}

// FormatLabel formats a label with its background color from GitHub.
// The text color is automatically calculated for readability.
func (t *Theme) FormatLabel(name, hexColor string) string {
	bg, err := termcolor.ParseHex(hexColor)
	if err != nil {
		return name
	}
	// Calculate luminance to determine text color
	fg := t.calculateTextColor(bg)
	return t.styler.FgBg(fg, bg, " "+name+" ")
}

// calculateTextColor returns black or white depending on background luminance.
func (t *Theme) calculateTextColor(bg termcolor.Color) termcolor.Color {
	// Calculate relative luminance using sRGB coefficients
	luminance := 0.299*float64(bg.R) + 0.587*float64(bg.G) + 0.114*float64(bg.B)
	if luminance > 140 {
		return termcolor.RGB(0, 0, 0) // Black text
	}
	return termcolor.RGB(255, 255, 255) // White text
}

// FormatLabelList formats a list of labels with colors.
func (t *Theme) FormatLabelList(labels []LabelColor) string {
	if len(labels) == 0 {
		return t.styler.Fg(t.Muted, "[]")
	}
	result := ""
	for i, l := range labels {
		if i > 0 {
			result += " "
		}
		result += t.FormatLabel(l.Name, l.Color)
	}
	return result
}

// FormatLabelChange formats label changes with +/- indicators.
func (t *Theme) FormatLabelChange(added, removed []LabelColor) string {
	result := ""
	for i, l := range removed {
		if i > 0 || result != "" {
			result += " "
		}
		result += t.styler.Fg(t.Removed, "-") + t.FormatLabel(l.Name, l.Color)
	}
	for i, l := range added {
		if i > 0 || result != "" {
			result += " "
		}
		result += t.styler.Fg(t.Added, "+") + t.FormatLabel(l.Name, l.Color)
	}
	return result
}

// LabelColor represents a label with its GitHub color.
type LabelColor struct {
	Name  string
	Color string // Hex color without #
}

// Convenience methods for common formatting

// AccentText returns text in accent color.
func (t *Theme) AccentText(text string) string {
	return t.styler.Fg(t.Accent, text)
}

// SuccessText returns text in success color.
func (t *Theme) SuccessText(text string) string {
	return t.styler.Fg(t.Success, text)
}

// ErrorText returns text in error color.
func (t *Theme) ErrorText(text string) string {
	return t.styler.Fg(t.Error, text)
}

// WarningText returns text in warning color.
func (t *Theme) WarningText(text string) string {
	return t.styler.Fg(t.Warning, text)
}

// MutedText returns text in muted color.
func (t *Theme) MutedText(text string) string {
	return t.styler.Fg(t.Muted, text)
}

// Strikethrough returns text with strikethrough styling.
func (t *Theme) Strikethrough(text string) string {
	return t.styler.Strikethrough(text)
}

// Underline returns text with underline styling.
func (t *Theme) Underline(text string) string {
	return t.styler.Underline(text)
}
