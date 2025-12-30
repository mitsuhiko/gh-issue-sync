package app

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// printWordDiff prints a colorized word-level diff that ignores whitespace differences.
// Returns true if there were additional whitespace differences beyond the word changes
// (but NOT if it's whitespace-only - that case is handled separately).
func (a *App) printWordDiff(oldText, newText string) bool {
	t := a.Theme

	// Normalize whitespace for comparison
	oldNorm := normalizeWhitespace(oldText)
	newNorm := normalizeWhitespace(newText)

	// Check if texts are equal when ignoring whitespace
	if oldNorm == newNorm {
		fmt.Fprintf(a.Out, "    %s\n", t.MutedText("(whitespace changes only)"))
		return false // Don't add the extra note since we already said it's whitespace-only
	}

	// Split into words (preserving structure for display)
	oldWords := splitIntoWords(oldText)
	newWords := splitIntoWords(newText)

	// Compute word-level diff
	ops := computeWordDiff(oldWords, newWords)

	// Render the diff
	a.renderWordDiff(ops)

	// Check if there are additional whitespace differences beyond the word changes
	// This happens when the word content is the same but whitespace formatting differs
	return hasAdditionalWhitespaceChanges(oldText, newText, oldWords, newWords)
}

// normalizeWhitespace collapses all whitespace into single spaces and trims
func normalizeWhitespace(s string) string {
	// Replace all whitespace sequences with a single space
	ws := regexp.MustCompile(`\s+`)
	return strings.TrimSpace(ws.ReplaceAllString(s, " "))
}

// hasAdditionalWhitespaceChanges checks if there are whitespace differences beyond the word changes.
// This is true when the words are the same but the actual text differs in whitespace.
func hasAdditionalWhitespaceChanges(oldText, newText string, oldWords, newWords []string) bool {
	// If the word lists are identical, check if the raw texts differ
	if len(oldWords) != len(newWords) {
		return false
	}
	for i := range oldWords {
		if oldWords[i] != newWords[i] {
			return false
		}
	}
	// Words are identical, but if raw texts differ, it's whitespace
	return oldText != newText
}

// splitIntoWords splits text into words, preserving whitespace as separate tokens for context
func splitIntoWords(text string) []string {
	var words []string
	var current strings.Builder
	inWord := false

	for _, r := range text {
		isSpace := unicode.IsSpace(r)
		if isSpace {
			if inWord {
				words = append(words, current.String())
				current.Reset()
				inWord = false
			}
			// Skip whitespace tokens - we'll add them back when rendering
		} else {
			if !inWord {
				inWord = true
			}
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		words = append(words, current.String())
	}
	return words
}

// computeWordDiff computes word-level diff using LCS algorithm
func computeWordDiff(oldWords, newWords []string) []diffOp {
	m, n := len(oldWords), len(newWords)
	if m == 0 && n == 0 {
		return nil
	}

	// Build LCS table
	lcs := make([][]int, m+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}

	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if oldWords[i-1] == newWords[j-1] {
				lcs[i][j] = lcs[i-1][j-1] + 1
			} else if lcs[i-1][j] >= lcs[i][j-1] {
				lcs[i][j] = lcs[i-1][j]
			} else {
				lcs[i][j] = lcs[i][j-1]
			}
		}
	}

	// Backtrack to build diff
	var ops []diffOp
	i, j := m, n
	for i > 0 || j > 0 {
		if i > 0 && j > 0 && oldWords[i-1] == newWords[j-1] {
			ops = append(ops, diffOp{Type: diffEqual, Text: oldWords[i-1]})
			i--
			j--
		} else if j > 0 && (i == 0 || lcs[i][j-1] >= lcs[i-1][j]) {
			ops = append(ops, diffOp{Type: diffInsert, Text: newWords[j-1]})
			j--
		} else {
			ops = append(ops, diffOp{Type: diffDelete, Text: oldWords[i-1]})
			i--
		}
	}

	// Reverse to get correct order
	for left, right := 0, len(ops)-1; left < right; left, right = left+1, right-1 {
		ops[left], ops[right] = ops[right], ops[left]
	}

	return ops
}

// renderWordDiff renders a word diff with inline coloring
func (a *App) renderWordDiff(ops []diffOp) {
	t := a.Theme
	var line strings.Builder
	lineLen := 0
	maxLineLen := 80
	indent := "    "

	flushLine := func() {
		if line.Len() > 0 {
			fmt.Fprintf(a.Out, "%s%s\n", indent, line.String())
			line.Reset()
			lineLen = 0
		}
	}

	addWord := func(word, styled string) {
		wordLen := len(word)
		if lineLen > 0 && lineLen+1+wordLen > maxLineLen {
			flushLine()
		}
		if lineLen > 0 {
			line.WriteString(" ")
			lineLen++
		}
		line.WriteString(styled)
		lineLen += wordLen
	}

	for _, op := range ops {
		switch op.Type {
		case diffEqual:
			addWord(op.Text, op.Text)
		case diffDelete:
			styled := t.Styler().FgStrikethrough(t.OldValue, op.Text)
			addWord(op.Text, styled)
		case diffInsert:
			styled := t.Styler().FgUnderline(t.NewValue, op.Text)
			addWord(op.Text, styled)
		}
	}
	flushLine()
}

// formatInlineWordDiff returns an inline word diff string for short text like titles.
// It shows deleted words with strikethrough and added words with underline.
func (a *App) formatInlineWordDiff(oldText, newText string) string {
	t := a.Theme

	oldWords := splitIntoWords(oldText)
	newWords := splitIntoWords(newText)
	ops := computeWordDiff(oldWords, newWords)

	var result strings.Builder
	for i, op := range ops {
		if i > 0 {
			result.WriteString(" ")
		}
		switch op.Type {
		case diffEqual:
			result.WriteString(op.Text)
		case diffDelete:
			result.WriteString(t.Styler().FgStrikethrough(t.OldValue, op.Text))
		case diffInsert:
			result.WriteString(t.Styler().FgUnderline(t.NewValue, op.Text))
		}
	}
	return result.String()
}

type diffOpType int

const (
	diffEqual diffOpType = iota
	diffDelete
	diffInsert
)

type diffOp struct {
	Type diffOpType
	Text string
}
