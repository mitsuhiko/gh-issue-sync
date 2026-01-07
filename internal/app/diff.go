package app

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// printWordDiff prints a colorized word-level diff that preserves line structure.
// Returns true if there were additional whitespace differences beyond the word changes
// (but NOT if it's whitespace-only - that case is handled separately).
func (a *App) printWordDiff(oldText, newText string) bool {
	t := a.Theme

	// Normalize whitespace for comparison (collapse all whitespace including newlines)
	oldNorm := normalizeWhitespace(oldText)
	newNorm := normalizeWhitespace(newText)

	// Check if texts are equal when ignoring whitespace
	if oldNorm == newNorm {
		fmt.Fprintf(a.Out, "    %s\n", t.MutedText("(whitespace changes only)"))
		return false // Don't add the extra note since we already said it's whitespace-only
	}

	// Split into tokens (words + newlines)
	oldTokens := splitIntoTokens(oldText)
	newTokens := splitIntoTokens(newText)

	// Compute token-level diff
	ops := computeWordDiff(oldTokens, newTokens)

	// Refine adjacent delete+insert pairs into character-level diffs (skip newlines)
	ops = refineWordDiff(ops)

	// Render the diff with line structure
	a.renderWordDiff(ops)

	// Check if there are additional whitespace differences beyond the token changes
	oldWords := splitIntoWords(oldText)
	newWords := splitIntoWords(newText)
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

// splitIntoTokens splits text into words and newline tokens for diff comparison.
// Newlines are preserved as "\n" tokens, other whitespace is collapsed.
func splitIntoTokens(text string) []string {
	var tokens []string
	var current strings.Builder
	inWord := false

	for _, r := range text {
		if r == '\n' {
			// Flush current word if any
			if inWord {
				tokens = append(tokens, current.String())
				current.Reset()
				inWord = false
			}
			// Add newline as a token
			tokens = append(tokens, "\n")
		} else if unicode.IsSpace(r) {
			if inWord {
				tokens = append(tokens, current.String())
				current.Reset()
				inWord = false
			}
			// Skip other whitespace - we'll add spaces back when rendering
		} else {
			if !inWord {
				inWord = true
			}
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

// splitIntoWords splits text into words only (no newlines), for inline diffs like titles
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

// refineWordDiff looks for adjacent delete+insert pairs and refines them
// into character-level diffs if the words are similar enough.
// Newline tokens are never refined.
func refineWordDiff(ops []diffOp) []diffOp {
	var result []diffOp
	i := 0
	for i < len(ops) {
		// Look for delete followed by insert (or vice versa)
		if i+1 < len(ops) {
			op1, op2 := ops[i], ops[i+1]
			var oldWord, newWord string

			if op1.Type == diffDelete && op2.Type == diffInsert {
				oldWord, newWord = op1.Text, op2.Text
			} else if op1.Type == diffInsert && op2.Type == diffDelete {
				oldWord, newWord = op2.Text, op1.Text
			}

			// Don't refine newline tokens
			if oldWord != "" && newWord != "" && oldWord != "\n" && newWord != "\n" && wordsSimilar(oldWord, newWord) {
				// Refine into character-level diff
				charOps := computeCharDiff(oldWord, newWord)
				result = append(result, diffOp{
					Type:    diffChange,
					Text:    oldWord,
					NewText: newWord,
					CharOps: charOps,
				})
				i += 2
				continue
			}
		}
		result = append(result, ops[i])
		i++
	}
	return result
}

// wordsSimilar returns true if two words are similar enough to warrant
// character-level diffing. We use a simple heuristic: common prefix+suffix
// must cover at least 50% of the longer word, OR Levenshtein distance
// must be less than 50% of the longer word's length.
func wordsSimilar(a, b string) bool {
	if a == b {
		return true
	}

	// Convert to runes for proper unicode handling
	ar := []rune(a)
	br := []rune(b)

	maxLen := len(ar)
	if len(br) > maxLen {
		maxLen = len(br)
	}
	if maxLen == 0 {
		return true
	}

	// Check common prefix
	prefix := 0
	for prefix < len(ar) && prefix < len(br) && ar[prefix] == br[prefix] {
		prefix++
	}

	// Check common suffix (but don't overlap with prefix)
	suffix := 0
	for suffix < len(ar)-prefix && suffix < len(br)-prefix &&
		ar[len(ar)-1-suffix] == br[len(br)-1-suffix] {
		suffix++
	}

	// If common parts cover at least 40% of the longer word, consider similar
	commonRatio := float64(prefix+suffix) / float64(maxLen)
	if commonRatio >= 0.4 {
		return true
	}

	// Fall back to Levenshtein distance for cases like "color" -> "colour"
	dist := levenshteinRunes(ar, br)
	distRatio := float64(dist) / float64(maxLen)
	return distRatio <= 0.5
}

// levenshteinRunes computes the Levenshtein distance between two rune slices.
func levenshteinRunes(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	// Use two rows for space efficiency
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)

	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(
				prev[j]+1,      // deletion
				curr[j-1]+1,    // insertion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// computeCharDiff computes character-level diff between two strings using LCS.
func computeCharDiff(oldText, newText string) []diffOp {
	oldRunes := []rune(oldText)
	newRunes := []rune(newText)
	m, n := len(oldRunes), len(newRunes)

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
			if oldRunes[i-1] == newRunes[j-1] {
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
		if i > 0 && j > 0 && oldRunes[i-1] == newRunes[j-1] {
			ops = append(ops, diffOp{Type: diffEqual, Text: string(oldRunes[i-1])})
			i--
			j--
		} else if j > 0 && (i == 0 || lcs[i][j-1] >= lcs[i-1][j]) {
			ops = append(ops, diffOp{Type: diffInsert, Text: string(newRunes[j-1])})
			j--
		} else {
			ops = append(ops, diffOp{Type: diffDelete, Text: string(oldRunes[i-1])})
			i--
		}
	}

	// Reverse to get correct order
	for left, right := 0, len(ops)-1; left < right; left, right = left+1, right-1 {
		ops[left], ops[right] = ops[right], ops[left]
	}

	// Merge consecutive ops of the same type for cleaner output
	return mergeConsecutiveOps(ops)
}

// mergeConsecutiveOps merges consecutive diff operations of the same type.
func mergeConsecutiveOps(ops []diffOp) []diffOp {
	if len(ops) == 0 {
		return ops
	}

	var result []diffOp
	current := ops[0]

	for i := 1; i < len(ops); i++ {
		if ops[i].Type == current.Type {
			current.Text += ops[i].Text
		} else {
			result = append(result, current)
			current = ops[i]
		}
	}
	result = append(result, current)
	return result
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

// renderWordDiff renders a word diff with inline coloring, preserving newlines
func (a *App) renderWordDiff(ops []diffOp) {
	t := a.Theme
	var line strings.Builder
	lineLen := 0
	maxLineLen := 80
	indent := "    "

	flushLine := func() {
		fmt.Fprintf(a.Out, "%s%s\n", indent, line.String())
		line.Reset()
		lineLen = 0
	}

	addWord := func(word, styled string) {
		wordLen := utf8.RuneCountInString(word)
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
		// Handle newline tokens specially
		if op.Text == "\n" {
			switch op.Type {
			case diffEqual:
				flushLine()
			case diffDelete:
				// Show deleted newline as a marker at end of line, then newline
				line.WriteString(t.Styler().FgStrikethrough(t.OldValue, "\\n"))
				flushLine()
			case diffInsert:
				// Show inserted newline as a marker, then newline
				line.WriteString(t.Styler().FgUnderline(t.NewValue, "\\n"))
				flushLine()
			}
			continue
		}

		switch op.Type {
		case diffEqual:
			addWord(op.Text, op.Text)
		case diffDelete:
			styled := t.Styler().FgStrikethrough(t.OldValue, op.Text)
			addWord(op.Text, styled)
		case diffInsert:
			styled := t.Styler().FgUnderline(t.NewValue, op.Text)
			addWord(op.Text, styled)
		case diffChange:
			// Render character-level diff inline
			styled := a.renderCharDiff(op.CharOps)
			// Use the longer of old/new for line length calculation
			wordLen := utf8.RuneCountInString(op.Text)
			if newLen := utf8.RuneCountInString(op.NewText); newLen > wordLen {
				wordLen = newLen
			}
			addWord(string(make([]rune, wordLen)), styled)
		}
	}
	if line.Len() > 0 {
		flushLine()
	}
}

// renderCharDiff renders character-level diff operations as a single styled string.
func (a *App) renderCharDiff(ops []diffOp) string {
	t := a.Theme
	var result strings.Builder

	for _, op := range ops {
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

// formatInlineWordDiff returns an inline word diff string for short text like titles.
// It shows deleted words with strikethrough and added words with underline.
// Similar words are refined to show character-level changes.
func (a *App) formatInlineWordDiff(oldText, newText string) string {
	t := a.Theme

	oldWords := splitIntoWords(oldText)
	newWords := splitIntoWords(newText)
	ops := computeWordDiff(oldWords, newWords)
	ops = refineWordDiff(ops)

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
		case diffChange:
			result.WriteString(a.renderCharDiff(op.CharOps))
		}
	}
	return result.String()
}

type diffOpType int

const (
	diffEqual diffOpType = iota
	diffDelete
	diffInsert
	diffChange // A word that changed - has character-level diff in CharOps
)

type diffOp struct {
	Type    diffOpType
	Text    string   // The text (or old text for diffChange)
	NewText string   // Only used for diffChange - the new text
	CharOps []diffOp // Only used for diffChange - character-level operations
}
