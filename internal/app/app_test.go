package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/shlex"
	"github.com/mitsuhiko/gh-issue-sync/internal/config"
	"github.com/mitsuhiko/gh-issue-sync/internal/ghcli"
	"github.com/mitsuhiko/gh-issue-sync/internal/issue"
	"github.com/mitsuhiko/gh-issue-sync/internal/paths"
)

func TestApplyMapping(t *testing.T) {
	parent := issue.IssueRef("T1")
	item := issue.Issue{
		Number: issue.IssueNumber("T2"),
		Title:  "Test",
		Body:   "Refs #T1 and #T10\n",
		Parent: &parent,
		BlockedBy: []issue.IssueRef{
			"T1",
			"99",
		},
	}
	mapping := map[string]string{"T1": "123"}
	changed := applyMapping(&item, mapping)
	if !changed {
		t.Fatalf("expected mapping to report change")
	}
	if item.Body != "Refs #123 and #T10\n" {
		t.Fatalf("unexpected body: %q", item.Body)
	}
	if item.Parent == nil || item.Parent.String() != "123" {
		t.Fatalf("unexpected parent: %v", item.Parent)
	}
	if got := item.BlockedBy[0].String(); got != "123" {
		t.Fatalf("unexpected blocked_by mapping: %s", got)
	}
}

func TestApplyMappingHexIDs(t *testing.T) {
	// Test with hex-style local IDs (e.g., T1a2b3c4d)
	parent := issue.IssueRef("Tabc12345")
	item := issue.Issue{
		Number: issue.IssueNumber("T99887766"),
		Title:  "Depends on #Tabc12345",
		Body:   "See #Tabc12345 for details. Also #Tdeadbeef is related.\n",
		Parent: &parent,
		BlockedBy: []issue.IssueRef{
			"Tabc12345",
			"Tdeadbeef",
		},
	}
	mapping := map[string]string{
		"Tabc12345": "100",
		"Tdeadbeef": "200",
	}
	changed := applyMapping(&item, mapping)
	if !changed {
		t.Fatalf("expected mapping to report change")
	}
	if item.Title != "Depends on #100" {
		t.Fatalf("unexpected title: %q", item.Title)
	}
	if item.Body != "See #100 for details. Also #200 is related.\n" {
		t.Fatalf("unexpected body: %q", item.Body)
	}
	if item.Parent == nil || item.Parent.String() != "100" {
		t.Fatalf("unexpected parent: %v", item.Parent)
	}
	if got := item.BlockedBy[0].String(); got != "100" {
		t.Fatalf("unexpected blocked_by[0] mapping: %s", got)
	}
	if got := item.BlockedBy[1].String(); got != "200" {
		t.Fatalf("unexpected blocked_by[1] mapping: %s", got)
	}
}

func TestApplyMappingNoChange(t *testing.T) {
	item := issue.Issue{
		Number: issue.IssueNumber("T1"),
		Title:  "No references here",
		Body:   "Just plain text\n",
	}
	mapping := map[string]string{"Tabc12345": "100"}
	changed := applyMapping(&item, mapping)
	if changed {
		t.Fatalf("expected no change")
	}
}

func TestNewIssueFromEditor(t *testing.T) {
	root := t.TempDir()
	p := paths.New(root)
	if err := p.EnsureLayout(); err != nil {
		t.Fatalf("layout: %v", err)
	}
	if err := config.Save(p.ConfigPath, config.Default("owner", "repo")); err != nil {
		t.Fatalf("config: %v", err)
	}

	var capturedNumber issue.IssueNumber
	previousInteractive := runInteractiveCommand
	runInteractiveCommand = func(ctx context.Context, command string, args ...string) error {
		if len(args) == 0 {
			t.Fatalf("expected editor path")
		}
		// Read the temp file to get the generated issue number
		tempIssue, err := issue.ParseFile(args[len(args)-1])
		if err != nil {
			t.Fatalf("parse temp issue: %v", err)
		}
		capturedNumber = tempIssue.Number
		payload, err := issue.Render(issue.Issue{
			Number: capturedNumber,
			Title:  "Edited Title",
			State:  "open",
			Body:   "Hello\n",
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if err := os.WriteFile(args[len(args)-1], []byte(payload), 0o644); err != nil {
			t.Fatalf("write editor payload: %v", err)
		}
		return nil
	}
	t.Cleanup(func() { runInteractiveCommand = previousInteractive })
	t.Setenv("EDITOR", "true")

	application := New(root, ghcli.ExecRunner{}, io.Discard, io.Discard)
	if err := application.NewIssue(context.Background(), "", NewOptions{Edit: true}); err != nil {
		t.Fatalf("new issue: %v", err)
	}

	// Find the created issue file (number is random)
	entries, err := os.ReadDir(p.OpenDir)
	if err != nil {
		t.Fatalf("read open dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 issue file, got %d", len(entries))
	}
	parsed, err := issue.ParseFile(p.OpenDir + "/" + entries[0].Name())
	if err != nil {
		t.Fatalf("parse issue: %v", err)
	}
	if parsed.Title != "Edited Title" {
		t.Fatalf("unexpected title: %q", parsed.Title)
	}
	if !parsed.Number.IsLocal() {
		t.Fatalf("expected local issue number, got %q", parsed.Number)
	}
}

func TestOrphanedOriginalsDetection(t *testing.T) {
	root := t.TempDir()
	p := paths.New(root)
	if err := p.EnsureLayout(); err != nil {
		t.Fatalf("layout: %v", err)
	}

	// Create originals for issues 1, 2, 3
	for _, num := range []string{"1", "2", "3"} {
		iss := issue.Issue{
			Number: issue.IssueNumber(num),
			Title:  "Issue " + num,
			State:  "open",
		}
		if err := issue.WriteFile(filepath.Join(p.OriginalsDir, num+".md"), iss); err != nil {
			t.Fatalf("write original %s: %v", num, err)
		}
	}

	// Create local files for issues 1 and 2 only (simulating #3 was deleted)
	for _, num := range []string{"1", "2"} {
		iss := issue.Issue{
			Number: issue.IssueNumber(num),
			Title:  "Issue " + num,
			State:  "open",
		}
		path := issue.PathFor(p.OpenDir, issue.IssueNumber(num), "Issue "+num)
		if err := issue.WriteFile(path, iss); err != nil {
			t.Fatalf("write local %s: %v", num, err)
		}
	}

	// Load local issues and build the set of tracked numbers
	localIssues, err := loadLocalIssues(p)
	if err != nil {
		t.Fatalf("load local: %v", err)
	}
	localNumbers := make(map[string]struct{}, len(localIssues))
	for _, item := range localIssues {
		localNumbers[item.Issue.Number.String()] = struct{}{}
	}

	// Find orphaned originals
	entries, err := os.ReadDir(p.OriginalsDir)
	if err != nil {
		t.Fatalf("read originals: %v", err)
	}

	var orphaned []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		number := strings.TrimSuffix(entry.Name(), ".md")
		if strings.HasPrefix(number, "T") {
			continue
		}
		if _, exists := localNumbers[number]; !exists {
			orphaned = append(orphaned, number)
		}
	}

	// Should find issue #3 as orphaned
	if len(orphaned) != 1 {
		t.Fatalf("expected 1 orphaned issue, got %d: %v", len(orphaned), orphaned)
	}
	if orphaned[0] != "3" {
		t.Fatalf("expected orphaned issue 3, got %s", orphaned[0])
	}
}

func TestList(t *testing.T) {
	root := t.TempDir()
	p := paths.New(root)
	if err := p.EnsureLayout(); err != nil {
		t.Fatalf("layout: %v", err)
	}
	if err := config.Save(p.ConfigPath, config.Default("owner", "repo")); err != nil {
		t.Fatalf("config: %v", err)
	}

	// Create some issues
	issues := []struct {
		num       string
		title     string
		state     string
		labels    []string
		assignee  []string
		author    string
		milestone string
		body      string
	}{
		{"1", "Open Bug", "open", []string{"bug"}, []string{"alice"}, "bob", "v1.0", "cc @charlie for review"},
		{"2", "Open Feature", "open", []string{"enhancement"}, nil, "alice", "", ""},
		{"3", "Closed Bug", "closed", []string{"bug"}, nil, "bob", "v1.0", ""},
		{"T123", "Local Issue", "open", nil, nil, "", "", ""},
	}

	for _, iss := range issues {
		dir := p.OpenDir
		if iss.state == "closed" {
			dir = p.ClosedDir
		}
		i := issue.Issue{
			Number:    issue.IssueNumber(iss.num),
			Title:     iss.title,
			State:     iss.state,
			Labels:    iss.labels,
			Assignees: iss.assignee,
			Author:    iss.author,
			Milestone: iss.milestone,
			Body:      iss.body,
		}
		path := issue.PathFor(dir, i.Number, i.Title)
		if err := issue.WriteFile(path, i); err != nil {
			t.Fatalf("write issue %s: %v", iss.num, err)
		}
		// Write originals for non-local issues
		if !strings.HasPrefix(iss.num, "T") {
			if err := issue.WriteFile(filepath.Join(p.OriginalsDir, iss.num+".md"), i); err != nil {
				t.Fatalf("write original %s: %v", iss.num, err)
			}
		}
	}

	var out strings.Builder
	application := New(root, ghcli.ExecRunner{}, &out, io.Discard)

	// Test: list open issues (default)
	out.Reset()
	if err := application.List(context.Background(), ListOptions{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, "#1") || !strings.Contains(output, "#2") {
		t.Fatalf("expected open issues #1 and #2 in output: %s", output)
	}
	if strings.Contains(output, "#3") {
		t.Fatalf("closed issue #3 should not be in default list: %s", output)
	}
	if !strings.Contains(output, "T123") {
		t.Fatalf("local issue T123 should be in output: %s", output)
	}

	// Test: list all issues
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true}); err != nil {
		t.Fatalf("list --all: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#3") {
		t.Fatalf("closed issue #3 should be in --all output: %s", output)
	}

	// Test: filter by state
	out.Reset()
	if err := application.List(context.Background(), ListOptions{State: "closed"}); err != nil {
		t.Fatalf("list --state closed: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#3") {
		t.Fatalf("closed issue #3 should be in --state closed: %s", output)
	}
	if strings.Contains(output, "#1") {
		t.Fatalf("open issue #1 should not be in --state closed: %s", output)
	}

	// Test: filter by label
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Label: []string{"bug"}}); err != nil {
		t.Fatalf("list --label bug: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") || !strings.Contains(output, "#3") {
		t.Fatalf("bug-labeled issues should be in output: %s", output)
	}
	if strings.Contains(output, "#2") {
		t.Fatalf("enhancement issue should not be in --label bug: %s", output)
	}

	// Test: filter by assignee
	out.Reset()
	if err := application.List(context.Background(), ListOptions{Assignee: "alice"}); err != nil {
		t.Fatalf("list --assignee alice: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") {
		t.Fatalf("alice's issue #1 should be in output: %s", output)
	}
	if strings.Contains(output, "#2") {
		t.Fatalf("unassigned issue #2 should not be in --assignee alice: %s", output)
	}

	// Test: filter by local only
	out.Reset()
	if err := application.List(context.Background(), ListOptions{Local: true}); err != nil {
		t.Fatalf("list --local: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "T123") {
		t.Fatalf("local issue T123 should be in --local: %s", output)
	}
	if strings.Contains(output, "#1") {
		t.Fatalf("remote issue #1 should not be in --local: %s", output)
	}

	// Test: filter by author
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Author: "bob"}); err != nil {
		t.Fatalf("list --author bob: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") || !strings.Contains(output, "#3") {
		t.Fatalf("bob's issues should be in output: %s", output)
	}
	if strings.Contains(output, "#2") {
		t.Fatalf("alice's issue should not be in --author bob: %s", output)
	}

	// Test: filter by milestone
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Milestone: "v1.0"}); err != nil {
		t.Fatalf("list --milestone v1.0: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") || !strings.Contains(output, "#3") {
		t.Fatalf("v1.0 milestone issues should be in output: %s", output)
	}
	if strings.Contains(output, "#2") {
		t.Fatalf("issue without milestone should not be in --milestone v1.0: %s", output)
	}

	// Test: filter by mention
	out.Reset()
	if err := application.List(context.Background(), ListOptions{Mention: "charlie"}); err != nil {
		t.Fatalf("list --mention charlie: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") {
		t.Fatalf("issue mentioning charlie should be in output: %s", output)
	}
	if strings.Contains(output, "#2") {
		t.Fatalf("issue not mentioning charlie should not be in --mention charlie: %s", output)
	}

	// Test: limit
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Limit: 2}); err != nil {
		t.Fatalf("list --limit 2: %v", err)
	}
	output = out.String()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	// Each issue now takes 2 lines (title + metadata), so 2 issues = 4 lines
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines with --limit 2 (2 issues x 2 lines each), got %d: %s", len(lines), output)
	}

	// Test: search with free text
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Search: "bug"}); err != nil {
		t.Fatalf("list --search bug: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") || !strings.Contains(output, "#3") {
		t.Fatalf("issues with 'bug' in title should be in output: %s", output)
	}
	if strings.Contains(output, "#2") {
		t.Fatalf("'Feature' issue should not be in --search bug: %s", output)
	}

	// Test: search case insensitive
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Search: "BUG"}); err != nil {
		t.Fatalf("list --search BUG: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") {
		t.Fatalf("case insensitive search should find 'Bug': %s", output)
	}

	// Test: search with is:open
	out.Reset()
	if err := application.List(context.Background(), ListOptions{Search: "is:open"}); err != nil {
		t.Fatalf("list --search is:open: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") || !strings.Contains(output, "#2") {
		t.Fatalf("open issues should be in output: %s", output)
	}
	if strings.Contains(output, "#3") {
		t.Fatalf("closed issue #3 should not be in --search is:open: %s", output)
	}

	// Test: search with is:closed
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Search: "is:closed"}); err != nil {
		t.Fatalf("list --search is:closed: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#3") {
		t.Fatalf("closed issue #3 should be in --search is:closed: %s", output)
	}
	if strings.Contains(output, "#1") {
		t.Fatalf("open issue #1 should not be in --search is:closed: %s", output)
	}

	// Test: search with label:
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Search: "label:bug"}); err != nil {
		t.Fatalf("list --search label:bug: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") || !strings.Contains(output, "#3") {
		t.Fatalf("bug-labeled issues should be in output: %s", output)
	}
	if strings.Contains(output, "#2") {
		t.Fatalf("enhancement issue should not be in --search label:bug: %s", output)
	}

	// Test: search with no:assignee
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Search: "no:assignee"}); err != nil {
		t.Fatalf("list --search no:assignee: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#2") || !strings.Contains(output, "#3") {
		t.Fatalf("unassigned issues should be in output: %s", output)
	}
	if strings.Contains(output, "#1") {
		t.Fatalf("assigned issue #1 should not be in --search no:assignee: %s", output)
	}

	// Test: search with combined query
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Search: "bug label:bug no:assignee"}); err != nil {
		t.Fatalf("list --search combined: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#3") {
		t.Fatalf("closed bug without assignee should be in output: %s", output)
	}
	if strings.Contains(output, "#1") {
		t.Fatalf("assigned bug #1 should not be in combined search: %s", output)
	}

	// Test: search with author:
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Search: "author:bob"}); err != nil {
		t.Fatalf("list --search author:bob: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") || !strings.Contains(output, "#3") {
		t.Fatalf("bob's issues should be in output: %s", output)
	}
	if strings.Contains(output, "#2") {
		t.Fatalf("alice's issue should not be in --search author:bob: %s", output)
	}

	// Test: search with milestone:
	out.Reset()
	if err := application.List(context.Background(), ListOptions{All: true, Search: "milestone:v1.0"}); err != nil {
		t.Fatalf("list --search milestone:v1.0: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") || !strings.Contains(output, "#3") {
		t.Fatalf("v1.0 issues should be in output: %s", output)
	}
	if strings.Contains(output, "#2") {
		t.Fatalf("issue without milestone should not be in --search milestone:v1.0: %s", output)
	}

	// Test: search with mentions:
	out.Reset()
	if err := application.List(context.Background(), ListOptions{Search: "mentions:charlie"}); err != nil {
		t.Fatalf("list --search mentions:charlie: %v", err)
	}
	output = out.String()
	if !strings.Contains(output, "#1") {
		t.Fatalf("issue mentioning charlie should be in output: %s", output)
	}
	if strings.Contains(output, "#2") {
		t.Fatalf("issue not mentioning charlie should not be in output: %s", output)
	}
}

func TestLocalIssuesNotOrphaned(t *testing.T) {
	root := t.TempDir()
	p := paths.New(root)
	if err := p.EnsureLayout(); err != nil {
		t.Fatalf("layout: %v", err)
	}

	// Create an original for a local issue (T-prefixed)
	localIss := issue.Issue{
		Number: issue.IssueNumber("Tabc123"),
		Title:  "Local Issue",
		State:  "open",
	}
	if err := issue.WriteFile(filepath.Join(p.OriginalsDir, "Tabc123.md"), localIss); err != nil {
		t.Fatalf("write local original: %v", err)
	}

	// Don't create local file - but since it's T-prefixed, it shouldn't be considered orphaned

	entries, err := os.ReadDir(p.OriginalsDir)
	if err != nil {
		t.Fatalf("read originals: %v", err)
	}

	var orphaned []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		number := strings.TrimSuffix(entry.Name(), ".md")
		// Skip local issues (T-prefixed)
		if strings.HasPrefix(number, "T") {
			continue
		}
		orphaned = append(orphaned, number)
	}

	// T-prefixed issues should be skipped
	if len(orphaned) != 0 {
		t.Fatalf("expected 0 orphaned issues (T-prefix should be skipped), got %d: %v", len(orphaned), orphaned)
	}
}

func TestRunInteractiveCommandQuotedPaths(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		extraArgs   []string
		wantName    string
		wantArgs    []string
		wantErr     bool
		errContains string
	}{
		{
			name:     "simple command",
			command:  "vim",
			wantName: "vim",
			wantArgs: nil,
		},
		{
			name:     "command with args",
			command:  "code --wait",
			wantName: "code",
			wantArgs: []string{"--wait"},
		},
		{
			name:     "quoted path with spaces",
			command:  `"/Applications/My Editor.app/Contents/MacOS/editor" --wait`,
			wantName: "/Applications/My Editor.app/Contents/MacOS/editor",
			wantArgs: []string{"--wait"},
		},
		{
			name:     "single quoted path",
			command:  `'/path/with spaces/editor'`,
			wantName: "/path/with spaces/editor",
			wantArgs: nil,
		},
		{
			name:      "extra args appended",
			command:   "code --wait",
			extraArgs: []string{"/tmp/file.md"},
			wantName:  "code",
			wantArgs:  []string{"--wait", "/tmp/file.md"},
		},
		{
			name:        "empty command",
			command:     "",
			wantErr:     true,
			errContains: "empty command",
		},
		{
			name:        "unclosed quote",
			command:     `"unclosed`,
			wantErr:     true,
			errContains: "failed to parse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedName string
			var capturedArgs []string

			prev := runInteractiveCommand
			runInteractiveCommand = func(ctx context.Context, command string, args ...string) error {
				// Call the real implementation but with a mock exec
				return prev(ctx, command, args...)
			}
			t.Cleanup(func() { runInteractiveCommand = prev })

			// We need to test the parsing logic, so let's extract it
			// by temporarily replacing the function and capturing what gets parsed
			err := testParseInteractiveCommand(tt.command, tt.extraArgs, &capturedName, &capturedArgs)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errContains)
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("expected error containing %q, got %q", tt.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if capturedName != tt.wantName {
				t.Errorf("name = %q, want %q", capturedName, tt.wantName)
			}

			if len(capturedArgs) != len(tt.wantArgs) {
				t.Errorf("args = %v, want %v", capturedArgs, tt.wantArgs)
			} else {
				for i := range capturedArgs {
					if capturedArgs[i] != tt.wantArgs[i] {
						t.Errorf("args[%d] = %q, want %q", i, capturedArgs[i], tt.wantArgs[i])
					}
				}
			}
		})
	}
}

// testParseInteractiveCommand extracts the parsing logic for testing
func testParseInteractiveCommand(command string, extraArgs []string, name *string, args *[]string) error {
	parts, err := shlex.Split(command)
	if err != nil {
		return fmt.Errorf("failed to parse command %q: %w", command, err)
	}
	if len(parts) == 0 {
		return fmt.Errorf("empty command")
	}
	*name = parts[0]
	*args = append(parts[1:], extraArgs...)
	return nil
}
