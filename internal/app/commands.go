package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/mitsuhiko/gh-issue-sync/internal/ghcli"
	"github.com/mitsuhiko/gh-issue-sync/internal/issue"
	"github.com/mitsuhiko/gh-issue-sync/internal/localid"
	"github.com/mitsuhiko/gh-issue-sync/internal/lock"
	"github.com/mitsuhiko/gh-issue-sync/internal/paths"
)

func (a *App) Status(ctx context.Context) error {
	p := paths.New(a.Root)
	cfg, err := loadConfig(p.ConfigPath)
	if err != nil {
		return err
	}
	t := a.Theme

	fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Repository:"), t.AccentText(cfg.Repository.Owner+"/"+cfg.Repository.Repo))
	if cfg.Sync.LastFullPull != nil {
		fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Last full pull:"), cfg.Sync.LastFullPull.Format(time.RFC3339))
	} else {
		fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Last full pull:"), t.WarningText("never"))
	}

	// Load label cache for colored output
	labelCache, _ := loadLabelCache(p)
	labelColors := labelCacheToColorMap(labelCache)

	result := loadLocalIssuesWithErrors(p)
	for _, parseErr := range result.Errors {
		fmt.Fprintf(a.Err, "%s %v\n", t.WarningText("Warning:"), parseErr)
	}
	localIssues := result.Issues

	type modifiedIssue struct {
		item     IssueFile
		original issue.Issue
	}

	var modified []modifiedIssue
	var newLocal []IssueFile

	for _, item := range localIssues {
		if item.Issue.Number.IsLocal() {
			newLocal = append(newLocal, item)
			continue
		}
		original, hasOriginal := readOriginalIssue(p, item.Issue.Number.String())
		if !hasOriginal {
			// No original means we can't compare - treat as modified without baseline
			modified = append(modified, modifiedIssue{item: item, original: issue.Issue{}})
			continue
		}
		if !issue.EqualIgnoringSyncedAt(item.Issue, original) {
			modified = append(modified, modifiedIssue{item: item, original: original})
		}
	}

	// Sort by issue number
	sort.Slice(modified, func(i, j int) bool {
		return modified[i].item.Issue.Number.String() < modified[j].item.Issue.Number.String()
	})
	sort.Slice(newLocal, func(i, j int) bool {
		return newLocal[i].Issue.Number.String() < newLocal[j].Issue.Number.String()
	})

	// Display modified issues in push/pull format
	if len(modified) > 0 {
		fmt.Fprintln(a.Out)
		fmt.Fprintln(a.Out, t.Bold("Modified locally:"))
		for _, m := range modified {
			fmt.Fprintln(a.Out, t.FormatIssueHeader("M", m.item.Issue.Number.String(), m.item.Issue.Title))
			for _, line := range a.formatChangeLines(m.original, m.item.Issue, labelColors) {
				fmt.Fprintln(a.Out, line)
			}
		}
	}

	// Display new local issues
	if len(newLocal) > 0 {
		fmt.Fprintln(a.Out)
		fmt.Fprintln(a.Out, t.Bold("New local issues:"))
		for _, item := range newLocal {
			fmt.Fprintln(a.Out, t.FormatIssueHeader("A", item.Issue.Number.String(), item.Issue.Title))
		}
	}

	// Summary
	if len(modified) == 0 && len(newLocal) == 0 {
		fmt.Fprintf(a.Out, "\n%s\n", t.MutedText("No local changes"))
	}

	// Check if projects are used and warn about missing scope
	projectsUsed := false
	for _, item := range localIssues {
		if len(item.Issue.Projects) > 0 {
			projectsUsed = true
			break
		}
	}
	if !projectsUsed {
		// Check if projects.json has entries
		if cache, err := loadProjectCache(p); err == nil && len(cache.Projects) > 0 {
			projectsUsed = true
		}
	}
	if projectsUsed {
		client := ghcli.NewClient(a.Runner, repoSlug(cfg))
		if hasScope, err := client.HasProjectScope(ctx); err == nil && !hasScope {
			fmt.Fprintf(a.Err, "%s %v\n", t.WarningText("Warning:"), ghcli.ErrMissingProjectScope)
		}
	}

	return nil
}

func (a *App) List(ctx context.Context, opts ListOptions) error {
	p := paths.New(a.Root)
	if _, err := loadConfig(p.ConfigPath); err != nil {
		return err
	}
	t := a.Theme

	// Load label colors for display
	labelCache, _ := loadLabelCache(p)
	labelColors := labelCacheToColorMap(labelCache)

	result := loadLocalIssuesWithErrors(p)
	for _, parseErr := range result.Errors {
		fmt.Fprintf(a.Err, "%s %v\n", t.WarningText("Warning:"), parseErr)
	}
	localIssues := result.Issues

	// Apply filters
	var filtered []IssueFile
	for _, item := range localIssues {
		// State filter
		if opts.State != "" && item.State != opts.State {
			continue
		}
		if !opts.All && opts.State == "" && item.State != "open" {
			continue
		}

		// Local-only filter
		if opts.Local && !item.Issue.Number.IsLocal() {
			continue
		}

		// Modified filter
		if opts.Modified {
			if item.Issue.Number.IsLocal() {
				// Local issues are always "modified" (unpushed)
			} else {
				original, hasOriginal := readOriginalIssue(p, item.Issue.Number.String())
				if hasOriginal && issue.EqualIgnoringSyncedAt(item.Issue, original) {
					continue
				}
			}
		}

		// Label filter
		if len(opts.Label) > 0 {
			hasLabel := false
			for _, wantLabel := range opts.Label {
				for _, haveLabel := range item.Issue.Labels {
					if strings.EqualFold(wantLabel, haveLabel) {
						hasLabel = true
						break
					}
				}
				if hasLabel {
					break
				}
			}
			if !hasLabel {
				continue
			}
		}

		// Assignee filter
		if opts.Assignee != "" {
			hasAssignee := false
			for _, assignee := range item.Issue.Assignees {
				if strings.EqualFold(opts.Assignee, assignee) {
					hasAssignee = true
					break
				}
			}
			if !hasAssignee {
				continue
			}
		}

		filtered = append(filtered, item)
	}

	// Sort: remote issues first (by number), then local issues
	sort.Slice(filtered, func(i, j int) bool {
		iLocal := filtered[i].Issue.Number.IsLocal()
		jLocal := filtered[j].Issue.Number.IsLocal()
		if iLocal != jLocal {
			return !iLocal // Remote issues first
		}
		return filtered[i].Issue.Number.String() < filtered[j].Issue.Number.String()
	})

	if len(filtered) == 0 {
		fmt.Fprintln(a.Out, t.MutedText("No issues found"))
		return nil
	}

	// Format and print
	for _, item := range filtered {
		a.printIssueLine(item, labelColors)
	}

	return nil
}

func (a *App) printIssueLine(item IssueFile, labelColors map[string]string) {
	t := a.Theme
	iss := item.Issue

	// Issue number
	numRaw := iss.Number.String()
	if !iss.Number.IsLocal() {
		numRaw = "#" + numRaw
	}
	var numDisplay string
	if iss.Number.IsLocal() {
		numDisplay = t.WarningText(numRaw)
	} else {
		numDisplay = t.AccentText(numRaw)
	}

	// Title (truncate if too long)
	title := iss.Title
	maxTitleLen := 50
	if len(title) > maxTitleLen {
		title = title[:maxTitleLen-3] + "..."
	}

	// Labels
	var labelStrs []string
	for _, label := range iss.Labels {
		color := labelColors[strings.ToLower(label)]
		if color != "" {
			labelStrs = append(labelStrs, t.FormatLabel(label, color))
		} else {
			labelStrs = append(labelStrs, t.MutedText(label))
		}
	}
	labelDisplay := strings.Join(labelStrs, " ")

	// Assignees
	var assigneeDisplay string
	if len(iss.Assignees) > 0 {
		assignees := make([]string, len(iss.Assignees))
		for i, a := range iss.Assignees {
			assignees[i] = "@" + a
		}
		assigneeDisplay = t.MutedText(strings.Join(assignees, ", "))
	}

	// Build output line with proper padding
	line := padRight(numDisplay, 6) + "  " + padRight(title, 50)
	if labelDisplay != "" {
		line += "  " + labelDisplay
	}
	if assigneeDisplay != "" {
		line += "  " + assigneeDisplay
	}

	fmt.Fprintln(a.Out, line)
}

func (a *App) NewIssue(ctx context.Context, title string, opts NewOptions) error {
	p := paths.New(a.Root)
	if _, err := loadConfig(p.ConfigPath); err != nil {
		return err
	}

	if strings.TrimSpace(title) == "" && !opts.Edit {
		return fmt.Errorf("title is required (provide a title or use --edit)")
	}

	// Acquire lock
	lck, err := lock.Acquire(p.SyncDir, lock.DefaultTimeout)
	if err != nil {
		return err
	}
	defer lck.Release()

	// Generate a random local ID
	id, err := localid.Generate()
	if err != nil {
		return fmt.Errorf("failed to generate local ID: %w", err)
	}

	localNumber := issue.IssueNumber(fmt.Sprintf("T%s", id))
	var newIssue issue.Issue
	if strings.TrimSpace(title) == "" && opts.Edit {
		edited, err := issueFromEditor(ctx, localNumber, opts.Labels)
		if err != nil {
			return err
		}
		newIssue = edited
	} else {
		newIssue = issue.Issue{
			Number: localNumber,
			Title:  strings.TrimSpace(title),
			Labels: opts.Labels,
			State:  "open",
			Body:   "",
		}
	}
	newIssue.Number = localNumber
	if strings.TrimSpace(newIssue.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if newIssue.State == "" {
		newIssue.State = "open"
	}

	path := issue.PathFor(p.OpenDir, localNumber, newIssue.Title)
	if err := issue.WriteFile(path, newIssue); err != nil {
		return err
	}
	if opts.Edit && strings.TrimSpace(title) != "" {
		if err := openEditor(ctx, path); err != nil {
			return err
		}
		updatedPath, err := finalizeEditedIssue(path, localNumber)
		if err != nil {
			return err
		}
		path = updatedPath
	}
	fmt.Fprintf(a.Out, "%s %s\n", a.Theme.SuccessText("Created"), relPath(a.Root, path))
	return nil
}

func issueFromEditor(ctx context.Context, number issue.IssueNumber, labels []string) (issue.Issue, error) {
	tempFile, err := os.CreateTemp("", "gh-issue-sync-issue-*.md")
	if err != nil {
		return issue.Issue{}, err
	}
	tempPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		return issue.Issue{}, err
	}
	defer os.Remove(tempPath)

	template := issue.Issue{
		Number: number,
		Title:  "",
		Labels: labels,
		State:  "open",
		Body:   "",
	}
	if err := issue.WriteFile(tempPath, template); err != nil {
		return issue.Issue{}, err
	}
	if err := openEditor(ctx, tempPath); err != nil {
		return issue.Issue{}, err
	}
	edited, err := issue.ParseFile(tempPath)
	if err != nil {
		return issue.Issue{}, err
	}
	edited.Title = strings.TrimSpace(edited.Title)
	if edited.Title == "" {
		return issue.Issue{}, fmt.Errorf("title is required (set it in the editor)")
	}
	edited.Number = number
	if edited.State == "" {
		edited.State = "open"
	}
	return edited, nil
}

func finalizeEditedIssue(path string, number issue.IssueNumber) (string, error) {
	edited, err := issue.ParseFile(path)
	if err != nil {
		return path, err
	}
	edited.Title = strings.TrimSpace(edited.Title)
	if edited.Title == "" {
		return path, fmt.Errorf("title is required")
	}
	if edited.Number != "" && edited.Number != number {
		return path, fmt.Errorf("issue number changed; expected %s", number)
	}
	if edited.Number != number {
		edited.Number = number
		if err := issue.WriteFile(path, edited); err != nil {
			return path, err
		}
	}
	newPath := issue.PathFor(filepath.Dir(path), number, edited.Title)
	if path != newPath {
		if err := os.Rename(path, newPath); err != nil {
			return path, err
		}
		return newPath, nil
	}
	return path, nil
}

func (a *App) Close(ctx context.Context, number string, opts CloseOptions) error {
	p := paths.New(a.Root)

	// Acquire lock
	lck, err := lock.Acquire(p.SyncDir, lock.DefaultTimeout)
	if err != nil {
		return err
	}
	defer lck.Release()

	file, err := findIssueByNumber(p, number)
	if err != nil {
		return err
	}
	if file.State == "closed" {
		return nil
	}
	reason := strings.TrimSpace(opts.Reason)
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	file.Issue.State = "closed"
	file.Issue.StateReason = reasonPtr
	newPath := issue.PathFor(p.ClosedDir, file.Issue.Number, file.Issue.Title)
	if err := os.Rename(file.Path, newPath); err != nil {
		return err
	}
	file.Path = newPath
	if err := issue.WriteFile(file.Path, file.Issue); err != nil {
		return err
	}
	return nil
}

func (a *App) Reopen(ctx context.Context, number string) error {
	p := paths.New(a.Root)

	// Acquire lock
	lck, err := lock.Acquire(p.SyncDir, lock.DefaultTimeout)
	if err != nil {
		return err
	}
	defer lck.Release()

	file, err := findIssueByNumber(p, number)
	if err != nil {
		return err
	}
	if file.State == "open" {
		return nil
	}
	file.Issue.State = "open"
	file.Issue.StateReason = nil
	newPath := issue.PathFor(p.OpenDir, file.Issue.Number, file.Issue.Title)
	if err := os.Rename(file.Path, newPath); err != nil {
		return err
	}
	file.Path = newPath
	if err := issue.WriteFile(file.Path, file.Issue); err != nil {
		return err
	}
	return nil
}

func (a *App) Edit(ctx context.Context, number string) error {
	p := paths.New(a.Root)
	file, err := findIssueByNumber(p, number)
	if err != nil {
		return err
	}

	if err := openEditor(ctx, file.Path); err != nil {
		return err
	}

	// After editing, re-read and handle title changes (file may need renaming)
	edited, err := issue.ParseFile(file.Path)
	if err != nil {
		return err
	}

	// Validate the issue number wasn't changed
	if edited.Number != "" && edited.Number != file.Issue.Number {
		return fmt.Errorf("issue number changed; expected %s, got %s", file.Issue.Number, edited.Number)
	}

	// Check if title changed and rename file accordingly
	edited.Title = strings.TrimSpace(edited.Title)
	if edited.Title == "" {
		return fmt.Errorf("title is required")
	}

	newPath := issue.PathFor(dirForState(p, file.State), file.Issue.Number, edited.Title)
	if file.Path != newPath {
		if err := os.Rename(file.Path, newPath); err != nil {
			return err
		}
	}

	return nil
}

func (a *App) View(ctx context.Context, ref string, opts ViewOptions) error {
	p := paths.New(a.Root)

	file, err := findIssueByRef(a.Root, p, ref)
	if err != nil {
		return err
	}

	if opts.Raw {
		content, err := os.ReadFile(file.Path)
		if err != nil {
			return err
		}
		fmt.Fprint(a.Out, string(content))
		return nil
	}

	t := a.Theme
	iss := file.Issue

	// Title
	fmt.Fprintf(a.Out, "%s\t%s\n", t.MutedText("title:"), t.Bold(iss.Title))

	// State
	stateText := strings.ToUpper(iss.State)
	if iss.StateReason != nil && *iss.StateReason != "" {
		stateText = fmt.Sprintf("%s (%s)", stateText, *iss.StateReason)
	}
	fmt.Fprintf(a.Out, "%s\t%s\n", t.MutedText("state:"), stateText)

	// Number
	fmt.Fprintf(a.Out, "%s\t%s\n", t.MutedText("number:"), iss.Number.String())

	// Labels
	if len(iss.Labels) > 0 {
		fmt.Fprintf(a.Out, "%s\t%s\n", t.MutedText("labels:"), strings.Join(iss.Labels, ", "))
	}

	// Assignees
	if len(iss.Assignees) > 0 {
		fmt.Fprintf(a.Out, "%s\t%s\n", t.MutedText("assignees:"), strings.Join(iss.Assignees, ", "))
	}

	// Milestone
	if iss.Milestone != "" {
		fmt.Fprintf(a.Out, "%s\t%s\n", t.MutedText("milestone:"), iss.Milestone)
	}

	// Issue Type
	if iss.IssueType != "" {
		fmt.Fprintf(a.Out, "%s\t%s\n", t.MutedText("type:"), iss.IssueType)
	}

	// Projects
	if len(iss.Projects) > 0 {
		fmt.Fprintf(a.Out, "%s\t%s\n", t.MutedText("projects:"), strings.Join(iss.Projects, ", "))
	}

	// Parent
	if iss.Parent != nil {
		fmt.Fprintf(a.Out, "%s\t#%s\n", t.MutedText("parent:"), iss.Parent.String())
	}

	// Blocked by
	if len(iss.BlockedBy) > 0 {
		refs := make([]string, len(iss.BlockedBy))
		for i, r := range iss.BlockedBy {
			refs[i] = "#" + r.String()
		}
		fmt.Fprintf(a.Out, "%s\t%s\n", t.MutedText("blocked_by:"), strings.Join(refs, ", "))
	}

	// Blocks
	if len(iss.Blocks) > 0 {
		refs := make([]string, len(iss.Blocks))
		for i, r := range iss.Blocks {
			refs[i] = "#" + r.String()
		}
		fmt.Fprintf(a.Out, "%s\t%s\n", t.MutedText("blocks:"), strings.Join(refs, ", "))
	}

	// Synced at with relative time
	if iss.SyncedAt != nil {
		relTime := formatRelativeTime(a.Now(), *iss.SyncedAt)
		fmt.Fprintf(a.Out, "%s\t%s\n", t.MutedText("synced:"), relTime)
	}

	// Separator and body
	fmt.Fprintln(a.Out, "--")
	if strings.TrimSpace(iss.Body) != "" {
		rendered, err := renderMarkdown(iss.Body)
		if err != nil {
			// Fall back to plain text on error
			fmt.Fprintln(a.Out, iss.Body)
		} else {
			fmt.Fprint(a.Out, rendered)
		}
	}

	return nil
}

// renderMarkdown renders markdown text for terminal output using glamour
func renderMarkdown(text string) (string, error) {
	renderer, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(80),
	)
	if err != nil {
		return "", err
	}
	return renderer.Render(text)
}

func (a *App) Diff(ctx context.Context, number string, opts DiffOptions) error {
	p := paths.New(a.Root)
	cfg, err := loadConfig(p.ConfigPath)
	if err != nil {
		return err
	}
	t := a.Theme

	// Load label cache for colored output
	labelCache, _ := loadLabelCache(p)
	labelColors := labelCacheToColorMap(labelCache)

	file, err := findIssueByNumber(p, number)
	if err != nil {
		return err
	}
	local := file.Issue

	var base issue.Issue
	var baseLabel string

	if opts.Remote {
		if local.Number.IsLocal() {
			return fmt.Errorf("cannot diff local issue %s against remote (not yet pushed)", local.Number)
		}
		client := ghcli.NewClient(a.Runner, repoSlug(cfg))
		remote, err := client.GetIssue(ctx, local.Number.String())
		if err != nil {
			return err
		}
		base = remote
		baseLabel = "remote"
	} else {
		original, hasOriginal := readOriginalIssue(p, local.Number.String())
		if !hasOriginal {
			if local.Number.IsLocal() {
				return fmt.Errorf("local issue %s has no original (not yet pushed)", local.Number)
			}
			return fmt.Errorf("no original found for issue %s (try pulling first)", local.Number)
		}
		base = original
		baseLabel = "original"
	}

	// Normalize for comparison
	base = issue.Normalize(base)
	local = issue.Normalize(local)

	// Check if there are any differences
	if issue.EqualIgnoringSyncedAt(base, local) {
		fmt.Fprintf(a.Out, "%s\n", t.MutedText(fmt.Sprintf("No differences between local and %s", baseLabel)))
		return nil
	}

	// Print header in same format as push/pull
	fmt.Fprintln(a.Out, t.FormatIssueHeader("M", local.Number.String(), local.Title))

	// Print metadata changes using formatChangeLines (same as push/pull)
	for _, line := range a.formatChangeLines(base, local, labelColors) {
		fmt.Fprintln(a.Out, line)
	}

	// Show word diff for body if changed
	if base.Body != local.Body {
		fmt.Fprintln(a.Out)
		fmt.Fprintf(a.Out, "    %s\n", t.Styler().Fg(t.FieldName, "body:"))
		hasWhitespaceChanges := a.printWordDiff(base.Body, local.Body)
		if hasWhitespaceChanges {
			fmt.Fprintf(a.Out, "\n    %s\n", t.MutedText("(note: whitespace also changed)"))
		}
	}

	return nil
}

func openEditor(ctx context.Context, path string) error {
	editor := getEditor(ctx)
	if editor == "" {
		return fmt.Errorf("no editor configured (set $VISUAL, $EDITOR, or git core.editor)")
	}
	return runEditor(ctx, editor, path)
}

// getEditor returns the preferred editor command following the precedence:
// $VISUAL > $EDITOR > git config core.editor > "vi"
func getEditor(ctx context.Context) string {
	if v := os.Getenv("VISUAL"); v != "" {
		return v
	}
	if e := os.Getenv("EDITOR"); e != "" {
		return e
	}
	// Try to get git's configured editor via `git var GIT_EDITOR`
	// which respects GIT_EDITOR env, core.editor config, VISUAL, EDITOR in that order
	if gitEditor, err := execCommand(ctx, "git", "var", "GIT_EDITOR"); err == nil {
		if ed := strings.TrimSpace(gitEditor); ed != "" {
			return ed
		}
	}
	return "vi"
}

// runEditor runs an editor command with the given path, connecting to the terminal
func runEditor(ctx context.Context, editor string, path string) error {
	return runInteractiveCommand(ctx, editor, path)
}

// runInteractiveCommand runs a command with stdin/stdout/stderr connected to the terminal.
// The command string may contain arguments (e.g., "code --wait").
var runInteractiveCommand = func(ctx context.Context, command string, args ...string) error {
	// Split the command to handle editors with arguments like "code --wait"
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return fmt.Errorf("empty command")
	}
	name := parts[0]
	cmdArgs := append(parts[1:], args...)

	cmd := exec.CommandContext(ctx, name, cmdArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

var execCommand = func(ctx context.Context, name string, args ...string) (string, error) {
	runner := ghcli.ExecRunner{}
	return runner.Run(ctx, name, args...)
}
