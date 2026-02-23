package issue

import (
	"strings"
	"testing"
	"time"
)

func TestParseRenderRoundTrip(t *testing.T) {
	syncedAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	// Note: number is derived from filename, not frontmatter
	input := strings.TrimSpace(`---
title: "Test issue"
labels:
  - bug
assignees:
  - alice
milestone: "v1"
state: open
state_reason: null
blocked_by:
  - 2
synced_at: 2025-01-02T03:04:05Z
---
Body line
`) + "\n"

	parsed, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	// Number is empty when using Parse directly (use ParseFile to get number from path)
	if parsed.Number != "" {
		t.Fatalf("expected empty number from Parse, got %s", parsed.Number)
	}
	if parsed.Title != "Test issue" {
		t.Fatalf("expected title, got %q", parsed.Title)
	}
	if parsed.SyncedAt == nil || !parsed.SyncedAt.Equal(syncedAt) {
		t.Fatalf("unexpected synced_at")
	}

	rendered, err := Render(parsed)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	parsedAgain, err := Parse([]byte(rendered))
	if err != nil {
		t.Fatalf("parse rendered failed: %v", err)
	}
	if !EqualIgnoringSyncedAt(parsed, parsedAgain) {
		t.Fatalf("round-trip mismatch")
	}
}

func TestParseFileExtractsNumber(t *testing.T) {
	// Mock file read
	oldReadFile := osReadFile
	defer func() { osReadFile = oldReadFile }()

	osReadFile = func(path string) ([]byte, error) {
		return []byte(`---
title: Test
state: open
---
Body
`), nil
	}

	issue, err := ParseFile("/tmp/.issues/open/42-test-issue.md")
	if err != nil {
		t.Fatalf("ParseFile failed: %v", err)
	}
	if issue.Number != "42" {
		t.Fatalf("expected number 42, got %s", issue.Number)
	}

	issue, err = ParseFile("/tmp/.issues/open/T5-new-issue.md")
	if err != nil {
		t.Fatalf("ParseFile failed: %v", err)
	}
	if issue.Number != "T5" {
		t.Fatalf("expected number T5, got %s", issue.Number)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Fix login bug":          "fix-login-bug",
		"  Weird---Title  ":      "weird-title",
		"Symbols & stuff!":       "symbols-stuff",
		"":                       "",
		"Multiple     spaces":    "multiple-spaces",
		"Already-slugified-text": "already-slugified-text",
	}
	for input, expected := range cases {
		if got := Slugify(input); got != expected {
			t.Fatalf("slugify %q => %q, want %q", input, got, expected)
		}
	}
}

func TestFileNameTruncatesLongSlugToFilesystemLimit(t *testing.T) {
	title := strings.Repeat("a", 600)
	name := FileName(IssueNumber("7895"), title)

	if len(name) > 255 {
		t.Fatalf("filename too long: got %d bytes (%q)", len(name), name)
	}
	if !strings.HasPrefix(name, "7895-") {
		t.Fatalf("filename missing number prefix: %q", name)
	}
	if !strings.HasSuffix(name, ".md") {
		t.Fatalf("filename missing extension: %q", name)
	}
}

func TestInfoSectionRoundTrip(t *testing.T) {
	input := strings.TrimSpace(`---
title: "Test issue with author"
state: open
info:
    author: testuser
---
Body
`) + "\n"

	parsed, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if parsed.Author != "testuser" {
		t.Fatalf("expected author 'testuser', got %q", parsed.Author)
	}

	rendered, err := Render(parsed)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if !strings.Contains(rendered, "info:") {
		t.Fatalf("rendered should contain info section: %s", rendered)
	}
	if !strings.Contains(rendered, "author: testuser") {
		t.Fatalf("rendered should contain author: %s", rendered)
	}

	parsedAgain, err := Parse([]byte(rendered))
	if err != nil {
		t.Fatalf("parse rendered failed: %v", err)
	}
	if parsedAgain.Author != "testuser" {
		t.Fatalf("expected author 'testuser' after round-trip, got %q", parsedAgain.Author)
	}
}

func TestInfoSectionOmittedWhenEmpty(t *testing.T) {
	iss := Issue{
		Title: "No author",
		State: "open",
	}
	rendered, err := Render(iss)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if strings.Contains(rendered, "info:") {
		t.Fatalf("rendered should not contain info section when empty: %s", rendered)
	}
}

func TestComputeChanges(t *testing.T) {
	base := Issue{
		Title:     "Original title",
		Labels:    []string{"bug"},
		Assignees: []string{"alice"},
		State:     "open",
		Body:      "Original body",
	}

	changed := Issue{
		Title:     "New title",
		Labels:    []string{"bug", "urgent"},
		Assignees: []string{"alice"},
		State:     "open",
		Body:      "Original body",
	}

	changes := ComputeChanges(base, changed)

	if !changes.Title {
		t.Error("expected Title to be changed")
	}
	if !changes.Labels {
		t.Error("expected Labels to be changed")
	}
	if changes.Assignees {
		t.Error("expected Assignees to NOT be changed")
	}
	if changes.Body {
		t.Error("expected Body to NOT be changed")
	}
}

func TestThreeWayMerge_NoConflict(t *testing.T) {
	base := Issue{
		Title:  "Original title",
		Labels: []string{"bug"},
		State:  "open",
		Body:   "Original body",
	}

	// Local changed the title
	local := Issue{
		Title:  "New title",
		Labels: []string{"bug"},
		State:  "open",
		Body:   "Original body",
	}

	// Remote changed labels
	remote := Issue{
		Title:  "Original title",
		Labels: []string{"bug", "urgent"},
		State:  "open",
		Body:   "Original body",
	}

	result := ThreeWayMerge(base, local, remote)

	if !result.OK {
		t.Fatalf("expected merge to succeed, got conflicts: %v", result.ConflictingFields.Fields())
	}

	if result.Merged.Title != "New title" {
		t.Errorf("expected merged title to be 'New title', got %q", result.Merged.Title)
	}
	if len(result.Merged.Labels) != 2 || result.Merged.Labels[0] != "bug" || result.Merged.Labels[1] != "urgent" {
		t.Errorf("expected merged labels to be [bug, urgent], got %v", result.Merged.Labels)
	}
}

func TestThreeWayMerge_Conflict(t *testing.T) {
	base := Issue{
		Title:  "Original title",
		Labels: []string{"bug"},
		State:  "open",
		Body:   "Original body",
	}

	// Local changed the title
	local := Issue{
		Title:  "Local title",
		Labels: []string{"bug"},
		State:  "open",
		Body:   "Original body",
	}

	// Remote also changed the title
	remote := Issue{
		Title:  "Remote title",
		Labels: []string{"bug"},
		State:  "open",
		Body:   "Original body",
	}

	result := ThreeWayMerge(base, local, remote)

	if result.OK {
		t.Fatal("expected merge to fail due to title conflict")
	}

	if !result.ConflictingFields.Title {
		t.Error("expected Title to be in conflicting fields")
	}
	if len(result.ConflictingFields.Fields()) != 1 {
		t.Errorf("expected only 1 conflicting field, got %v", result.ConflictingFields.Fields())
	}
}

func TestThreeWayMerge_NoLocalChanges(t *testing.T) {
	base := Issue{
		Title:  "Original title",
		Labels: []string{"bug"},
		State:  "open",
		Body:   "Original body",
	}

	// Local is same as base
	local := Issue{
		Title:  "Original title",
		Labels: []string{"bug"},
		State:  "open",
		Body:   "Original body",
	}

	// Remote changed labels
	remote := Issue{
		Title:  "Original title",
		Labels: []string{"bug", "urgent"},
		State:  "open",
		Body:   "Original body",
	}

	result := ThreeWayMerge(base, local, remote)

	if !result.OK {
		t.Fatalf("expected merge to succeed")
	}
	if !result.LocalChanges.IsEmpty() {
		t.Errorf("expected no local changes, got %v", result.LocalChanges.Fields())
	}
	// Merged should match remote
	if len(result.Merged.Labels) != 2 {
		t.Errorf("expected merged to have remote labels, got %v", result.Merged.Labels)
	}
}
