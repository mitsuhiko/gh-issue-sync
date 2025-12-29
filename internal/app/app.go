package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mitsuhiko/gh-issue-sync/internal/config"
	"github.com/mitsuhiko/gh-issue-sync/internal/ghcli"
	"github.com/mitsuhiko/gh-issue-sync/internal/issue"
	"github.com/mitsuhiko/gh-issue-sync/internal/paths"
	"github.com/mitsuhiko/gh-issue-sync/internal/theme"
)

type App struct {
	Root   string
	Runner ghcli.Runner
	Now    func() time.Time
	Out    io.Writer
	Err    io.Writer
	Theme  *theme.Theme
}

type PullOptions struct {
	All   bool
	Force bool
	Label []string
}

type PushOptions struct {
	DryRun bool
}

type NewOptions struct {
	Labels []string
	Edit   bool
}

type CloseOptions struct {
	Reason string
}

type IssueFile struct {
	Issue issue.Issue
	Path  string
	State string
}

func New(root string, runner ghcli.Runner, out io.Writer, errOut io.Writer) *App {
	return &App{
		Root:   root,
		Runner: runner,
		Now:    time.Now,
		Out:    out,
		Err:    errOut,
		Theme:  theme.Default(),
	}
}

func (a *App) Init(ctx context.Context, owner, repo string) error {
	if owner == "" || repo == "" {
		ownerGuess, repoGuess, err := a.detectRepoFromGit(ctx)
		if err != nil {
			return fmt.Errorf("unable to detect repo from git: %w (use --owner and --repo)", err)
		}
		if owner == "" {
			owner = ownerGuess
		}
		if repo == "" {
			repo = repoGuess
		}
	}

	p := paths.New(a.Root)
	if err := p.EnsureLayout(); err != nil {
		return err
	}
	if _, err := os.Stat(p.ConfigPath); err == nil {
		return fmt.Errorf("config already exists at %s", p.ConfigPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfg := config.Default(owner, repo)
	if err := config.Save(p.ConfigPath, cfg); err != nil {
		return err
	}
	t := a.Theme
	fmt.Fprintf(a.Out, "%s %s %s %s\n", t.SuccessText("Initialized"), t.AccentText(owner+"/"+repo), t.MutedText("in"), p.IssuesDir)
	return nil
}

func (a *App) Pull(ctx context.Context, opts PullOptions, args []string) error {
	p := paths.New(a.Root)
	cfg, err := loadConfig(p.ConfigPath)
	if err != nil {
		return err
	}
	client := ghcli.NewClient(a.Runner, repoSlug(cfg))
	t := a.Theme

	// Fetch label colors for nice output
	labelColors := a.fetchLabelColors(ctx, client)

	var remoteIssues []issue.Issue
	if len(args) > 0 {
		for _, arg := range args {
			number := strings.TrimSpace(arg)
			if number == "" {
				continue
			}
			remote, err := client.GetIssue(ctx, number)
			if err != nil {
				return err
			}
			remoteIssues = append(remoteIssues, remote)
		}
	} else {
		state := "open"
		if opts.All {
			state = "all"
		}
		remoteIssues, err = client.ListIssues(ctx, state, opts.Label)
		if err != nil {
			return err
		}
	}

	localIssues, err := loadLocalIssues(p)
	if err != nil {
		return err
	}
	localByNumber := map[string]IssueFile{}
	for _, item := range localIssues {
		localByNumber[item.Issue.Number.String()] = item
	}

	var conflicts []string
	unchanged := 0
	for _, remote := range remoteIssues {
		remote.State = strings.ToLower(remote.State)
		remote.SyncedAt = ptrTime(a.Now().UTC())

		local, hasLocal := localByNumber[remote.Number.String()]
		original, hasOriginal := readOriginalIssue(p, remote.Number.String())
		localChanged := false
		if hasLocal {
			if !hasOriginal {
				localChanged = true
			} else {
				localChanged = !issue.EqualIgnoringSyncedAt(local.Issue, original)
			}
		}

		if hasLocal && localChanged && !opts.Force {
			conflicts = append(conflicts, remote.Number.String())
			continue
		}

		targetDir := p.OpenDir
		if remote.State == "closed" {
			targetDir = p.ClosedDir
		}
		newPath := issue.PathFor(targetDir, remote.Number, remote.Title)
		contentChanged := !hasLocal || !issue.EqualIgnoringSyncedAt(local.Issue, remote)
		pathChanged := hasLocal && local.Path != newPath
		if hasOriginal && !contentChanged && !pathChanged {
			unchanged++
			continue
		}

		if hasLocal && local.Path != newPath {
			if err := os.Rename(local.Path, newPath); err != nil {
				return err
			}
		}
		if err := issue.WriteFile(newPath, remote); err != nil {
			return err
		}
		if err := writeOriginalIssue(p, remote); err != nil {
			return err
		}
		if !hasLocal {
			fmt.Fprintln(a.Out, t.FormatIssueHeader("A", remote.Number.String(), remote.Title))
			continue
		}
		lines := a.formatChangeLines(local.Issue, remote, labelColors)
		if len(lines) == 0 && pathChanged {
			lines = append(lines, t.FormatChange("file", fmt.Sprintf("%q", relPath(a.Root, local.Path)), fmt.Sprintf("%q", relPath(a.Root, newPath))))
		}
		fmt.Fprintln(a.Out, t.FormatIssueHeader("U", remote.Number.String(), remote.Title))
		for _, line := range lines {
			fmt.Fprintln(a.Out, line)
		}
	}

	if len(args) == 0 {
		now := a.Now().UTC()
		cfg.Sync.LastFullPull = &now
		if err := config.Save(p.ConfigPath, cfg); err != nil {
			return err
		}
	}

	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		fmt.Fprintf(a.Err, "%s %s\n", t.WarningText("Conflicts (local changes, skipped):"), strings.Join(conflicts, ", "))
	}
	if unchanged > 0 {
		fmt.Fprintf(a.Out, "%s\n", t.MutedText(fmt.Sprintf("No changes needed for %d issue(s)", unchanged)))
	}
	return nil
}

// fetchLabelColors fetches label colors from GitHub, returning a map of name -> hex color.
// Errors are silently ignored (we'll just use default colors).
func (a *App) fetchLabelColors(ctx context.Context, client *ghcli.Client) map[string]string {
	colors := make(map[string]string)
	labels, err := client.ListLabels(ctx)
	if err != nil {
		return colors
	}
	for _, l := range labels {
		colors[l.Name] = l.Color
	}
	return colors
}

func (a *App) Push(ctx context.Context, opts PushOptions, args []string) error {
	p := paths.New(a.Root)
	cfg, err := loadConfig(p.ConfigPath)
	if err != nil {
		return err
	}
	client := ghcli.NewClient(a.Runner, repoSlug(cfg))
	t := a.Theme

	// Fetch label colors for nice output
	labelColors := a.fetchLabelColors(ctx, client)

	localIssues, err := loadLocalIssues(p)
	if err != nil {
		return err
	}
	filteredIssues, err := filterIssuesByArgs(a.Root, localIssues, args)
	if err != nil {
		return err
	}

	mapping := map[string]string{}
	createdNumbers := map[string]struct{}{}
	for i := range filteredIssues {
		item := &filteredIssues[i]
		if !item.Issue.Number.IsLocal() {
			continue
		}
		if opts.DryRun {
			fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Would create issue"), item.Issue.Title)
			continue
		}
		newNumber, err := client.CreateIssue(ctx, item.Issue)
		if err != nil {
			return err
		}
		oldNumber := item.Issue.Number.String()
		mapping[oldNumber] = newNumber
		createdNumbers[newNumber] = struct{}{}
		item.Issue.Number = issue.IssueNumber(newNumber)
		item.Issue.SyncedAt = ptrTime(a.Now().UTC())
		newPath := issue.PathFor(dirForState(p, item.State), item.Issue.Number, item.Issue.Title)
		if item.Path != newPath {
			if err := os.Rename(item.Path, newPath); err != nil {
				return err
			}
			item.Path = newPath
		}
		if err := issue.WriteFile(item.Path, item.Issue); err != nil {
			return err
		}
		if err := writeOriginalIssue(p, item.Issue); err != nil {
			return err
		}
		fmt.Fprintln(a.Out, t.FormatIssueHeader("A", newNumber, item.Issue.Title))
	}

	if len(mapping) > 0 {
		allIssues, err := loadLocalIssues(p)
		if err != nil {
			return err
		}
		for i := range allIssues {
			changed := applyMapping(&allIssues[i].Issue, mapping)
			if changed {
				if opts.DryRun {
					fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Would update references in"), allIssues[i].Path)
					continue
				}
				if err := issue.WriteFile(allIssues[i].Path, allIssues[i].Issue); err != nil {
					return err
				}
				fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Updated references in"), relPath(a.Root, allIssues[i].Path))
			}
		}
		if len(args) > 0 {
			for i, arg := range args {
				if newID, ok := mapping[arg]; ok {
					args[i] = newID
				}
			}
		}
		filteredIssues, err = filterIssuesByArgs(a.Root, allIssues, args)
		if err != nil {
			return err
		}
	}

	var conflicts []string
	unchanged := 0
	for _, item := range filteredIssues {
		if item.Issue.Number.IsLocal() {
			continue
		}
		original, hasOriginal := readOriginalIssue(p, item.Issue.Number.String())
		localChanged := !hasOriginal || !issue.EqualIgnoringSyncedAt(item.Issue, original)
		if !localChanged {
			if _, ok := createdNumbers[item.Issue.Number.String()]; !ok {
				unchanged++
			}
			continue
		}
		if opts.DryRun {
			fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Would push issue"), t.AccentText("#"+item.Issue.Number.String()))
			continue
		}
		remote, err := client.GetIssue(ctx, item.Issue.Number.String())
		if err != nil {
			return err
		}
		if hasOriginal && !issue.EqualIgnoringSyncedAt(remote, original) {
			conflicts = append(conflicts, item.Issue.Number.String())
			continue
		}
		change := diffIssue(original, item.Issue)
		if change.StateTransition != nil {
			if *change.StateTransition == "close" {
				reason := ""
				if change.StateReason != nil {
					reason = *change.StateReason
				}
				if err := client.CloseIssue(ctx, item.Issue.Number.String(), reason); err != nil {
					return err
				}
			} else if *change.StateTransition == "reopen" {
				if err := client.ReopenIssue(ctx, item.Issue.Number.String()); err != nil {
					return err
				}
			}
		}
		if hasEdits(change) {
			if err := client.EditIssue(ctx, item.Issue.Number.String(), change); err != nil {
				return err
			}
		}
		item.Issue.SyncedAt = ptrTime(a.Now().UTC())
		if err := issue.WriteFile(item.Path, item.Issue); err != nil {
			return err
		}
		if err := writeOriginalIssue(p, item.Issue); err != nil {
			return err
		}
		fmt.Fprintln(a.Out, t.FormatIssueHeader("U", item.Issue.Number.String(), item.Issue.Title))
		for _, line := range a.formatChangeLines(original, item.Issue, labelColors) {
			fmt.Fprintln(a.Out, line)
		}
	}

	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		fmt.Fprintf(a.Err, "%s %s\n", t.WarningText("Conflicts (remote changed, skipped):"), strings.Join(conflicts, ", "))
	}
	if unchanged > 0 {
		fmt.Fprintf(a.Out, "%s\n", t.MutedText(fmt.Sprintf("No changes needed for %d issue(s)", unchanged)))
	}
	return nil
}

func (a *App) Status(ctx context.Context) error {
	p := paths.New(a.Root)
	cfg, err := loadConfig(p.ConfigPath)
	if err != nil {
		return err
	}
	t := a.Theme

	fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Repository:"), t.AccentText(cfg.Repository.Owner+"/"+cfg.Repository.Repo))
	if cfg.Sync.LastFullPull != nil {
		fmt.Fprintf(a.Out, "%s %s\n\n", t.MutedText("Last full pull:"), cfg.Sync.LastFullPull.Format(time.RFC3339))
	} else {
		fmt.Fprintf(a.Out, "%s %s\n\n", t.MutedText("Last full pull:"), t.WarningText("never"))
	}

	localIssues, err := loadLocalIssues(p)
	if err != nil {
		return err
	}
	var modified []string
	var newLocal []string
	var stateChanges []string

	for _, item := range localIssues {
		if item.Issue.Number.IsLocal() {
			newLocal = append(newLocal, item.Path)
			continue
		}
		original, hasOriginal := readOriginalIssue(p, item.Issue.Number.String())
		if !hasOriginal {
			modified = append(modified, item.Path)
			continue
		}
		if !issue.EqualIgnoringSyncedAt(item.Issue, original) {
			modified = append(modified, item.Path)
		}
		if item.Issue.State != original.State {
			stateChanges = append(stateChanges, item.Path)
		}
	}

	if len(modified) > 0 {
		sort.Strings(modified)
		fmt.Fprintln(a.Out, t.Bold("Modified locally:"))
		for _, path := range modified {
			fmt.Fprintf(a.Out, "  %s %s\n", t.FormatStatus("M"), relPath(a.Root, path))
		}
		fmt.Fprintln(a.Out)
	}
	if len(newLocal) > 0 {
		sort.Strings(newLocal)
		fmt.Fprintln(a.Out, t.Bold("New local issues:"))
		for _, path := range newLocal {
			fmt.Fprintf(a.Out, "  %s %s\n", t.FormatStatus("A"), relPath(a.Root, path))
		}
		fmt.Fprintln(a.Out)
	}
	if len(stateChanges) > 0 {
		sort.Strings(stateChanges)
		fmt.Fprintln(a.Out, t.Bold("State changes:"))
		for _, path := range stateChanges {
			fmt.Fprintf(a.Out, "  %s %s\n", t.AccentText("->"), relPath(a.Root, path))
		}
	}
	return nil
}

func (a *App) NewIssue(ctx context.Context, title string, opts NewOptions) error {
	p := paths.New(a.Root)
	cfg, err := loadConfig(p.ConfigPath)
	if err != nil {
		return err
	}

	if strings.TrimSpace(title) == "" && !opts.Edit {
		return fmt.Errorf("title is required (provide a title or use --edit)")
	}

	id := cfg.Local.NextLocalID
	cfg.Local.NextLocalID++

	localNumber := issue.IssueNumber(fmt.Sprintf("T%d", id))
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
	if err := config.Save(p.ConfigPath, cfg); err != nil {
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

func loadLocalIssues(p paths.Paths) ([]IssueFile, error) {
	issues := []IssueFile{}
	for _, dir := range []struct {
		Path  string
		State string
	}{{p.OpenDir, "open"}, {p.ClosedDir, "closed"}} {
		entries, err := os.ReadDir(dir.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if filepath.Ext(entry.Name()) != ".md" {
				continue
			}
			path := filepath.Join(dir.Path, entry.Name())
			parsed, err := issue.ParseFile(path)
			if err != nil {
				return nil, err
			}
			parsed.State = dir.State
			issues = append(issues, IssueFile{Issue: parsed, Path: path, State: dir.State})
		}
	}
	return issues, nil
}

func findIssueByNumber(p paths.Paths, number string) (IssueFile, error) {
	issues, err := loadLocalIssues(p)
	if err != nil {
		return IssueFile{}, err
	}
	for _, item := range issues {
		if item.Issue.Number.String() == number {
			return item, nil
		}
	}
	return IssueFile{}, fmt.Errorf("issue %s not found", number)
}

func readOriginalIssue(p paths.Paths, number string) (issue.Issue, bool) {
	path := filepath.Join(p.OriginalsDir, fmt.Sprintf("%s.md", number))
	parsed, err := issue.ParseFile(path)
	if err != nil {
		return issue.Issue{}, false
	}
	return parsed, true
}

func writeOriginalIssue(p paths.Paths, item issue.Issue) error {
	path := filepath.Join(p.OriginalsDir, fmt.Sprintf("%s.md", item.Number))
	return issue.WriteFile(path, item)
}

func dirForState(p paths.Paths, state string) string {
	if state == "closed" {
		return p.ClosedDir
	}
	return p.OpenDir
}

func filterIssuesByArgs(root string, issues []IssueFile, args []string) ([]IssueFile, error) {
	if len(args) == 0 {
		return issues, nil
	}
	pathsWanted := map[string]struct{}{}
	idsWanted := map[string]struct{}{}
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		if strings.HasSuffix(arg, ".md") || strings.Contains(arg, string(os.PathSeparator)) {
			cleaned := filepath.Clean(arg)
			pathsWanted[cleaned] = struct{}{}
			if !filepath.IsAbs(cleaned) {
				pathsWanted[filepath.Join(root, cleaned)] = struct{}{}
			}
			continue
		}
		idsWanted[arg] = struct{}{}
	}
	var filtered []IssueFile
	for _, item := range issues {
		if _, ok := idsWanted[item.Issue.Number.String()]; ok {
			filtered = append(filtered, item)
			continue
		}
		rel := filepath.Clean(relPath(root, item.Path))
		cleanPath := filepath.Clean(item.Path)
		if _, ok := pathsWanted[cleanPath]; ok {
			filtered = append(filtered, item)
			continue
		}
		if _, ok := pathsWanted[rel]; ok {
			filtered = append(filtered, item)
			continue
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("no matching issues for arguments: %s", strings.Join(args, ", "))
	}
	return filtered, nil
}

func diffIssue(original issue.Issue, local issue.Issue) ghcli.IssueChange {
	change := ghcli.IssueChange{}
	if original.Title != local.Title {
		change.Title = &local.Title
	}
	if original.Body != local.Body {
		change.Body = &local.Body
	}
	change.AddLabels, change.RemoveLabels = diffStringSet(original.Labels, local.Labels)
	change.AddAssignees, change.RemoveAssignees = diffStringSet(original.Assignees, local.Assignees)
	if original.Milestone != local.Milestone {
		milestone := local.Milestone
		change.Milestone = &milestone
	}
	if original.State != "" && original.State != local.State {
		transition := ""
		if local.State == "closed" {
			transition = "close"
		} else if local.State == "open" {
			transition = "reopen"
		}
		if transition != "" {
			change.StateTransition = &transition
		}
	}
	if original.StateReason != nil || local.StateReason != nil {
		if normalizeOptional(original.StateReason) != normalizeOptional(local.StateReason) {
			reason := normalizeOptional(local.StateReason)
			change.StateReason = &reason
		}
	}
	return change
}

func hasEdits(change ghcli.IssueChange) bool {
	return change.Title != nil || change.Body != nil || change.Milestone != nil || len(change.AddLabels) > 0 || len(change.RemoveLabels) > 0 || len(change.AddAssignees) > 0 || len(change.RemoveAssignees) > 0
}

func (a *App) formatChangeLines(oldIssue, newIssue issue.Issue, labelColors map[string]string) []string {
	oldIssue = issue.Normalize(oldIssue)
	newIssue = issue.Normalize(newIssue)
	t := a.Theme

	lines := []string{}
	if oldIssue.Title != newIssue.Title {
		lines = append(lines, t.FormatChange("title", fmt.Sprintf("%q", oldIssue.Title), fmt.Sprintf("%q", newIssue.Title)))
	}
	if oldIssue.Body != newIssue.Body {
		lines = append(lines, t.FormatChange("body", formatBodySummary(oldIssue.Body), formatBodySummary(newIssue.Body)))
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
		lines = append(lines, t.FormatChange("milestone", formatOptionalString(oldIssue.Milestone), formatOptionalString(newIssue.Milestone)))
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
		color := colors[name]
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

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func diffStringSet(old, new []string) ([]string, []string) {
	oldSet := make(map[string]struct{}, len(old))
	for _, item := range old {
		oldSet[item] = struct{}{}
	}
	newSet := make(map[string]struct{}, len(new))
	for _, item := range new {
		newSet[item] = struct{}{}
	}
	var add []string
	for item := range newSet {
		if _, ok := oldSet[item]; !ok {
			add = append(add, item)
		}
	}
	var remove []string
	for item := range oldSet {
		if _, ok := newSet[item]; !ok {
			remove = append(remove, item)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	return add, remove
}

var localRefPattern = regexp.MustCompile(`(?m)#(T\d+)`)

func applyMapping(issueItem *issue.Issue, mapping map[string]string) bool {
	changed := false
	body := localRefPattern.ReplaceAllStringFunc(issueItem.Body, func(match string) string {
		id := strings.TrimPrefix(match, "#")
		if real, ok := mapping[id]; ok {
			changed = true
			return "#" + real
		}
		return match
	})
	if changed {
		issueItem.Body = body
	}

	if issueItem.Parent != nil {
		if real, ok := mapping[issueItem.Parent.String()]; ok {
			updated := issue.IssueRef(real)
			issueItem.Parent = &updated
			changed = true
		}
	}
	issueItem.BlockedBy, changed = applyMappingToRefs(issueItem.BlockedBy, mapping, changed)
	issueItem.Blocks, changed = applyMappingToRefs(issueItem.Blocks, mapping, changed)

	return changed
}

func applyMappingToRefs(refs []issue.IssueRef, mapping map[string]string, changed bool) ([]issue.IssueRef, bool) {
	if len(refs) == 0 {
		return refs, changed
	}
	updated := make([]issue.IssueRef, 0, len(refs))
	for _, ref := range refs {
		if real, ok := mapping[ref.String()]; ok {
			updated = append(updated, issue.IssueRef(real))
			changed = true
		} else {
			updated = append(updated, ref)
		}
	}
	return updated, changed
}

func normalizeOptional(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func ptrTime(t time.Time) *time.Time {
	return &t
}

func openEditor(ctx context.Context, path string) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		return fmt.Errorf("EDITOR is not set (export EDITOR to your preferred editor)")
	}
	_, err := execCommand(ctx, editor, path)
	return err
}

var execCommand = func(ctx context.Context, name string, args ...string) (string, error) {
	runner := ghcli.ExecRunner{}
	return runner.Run(ctx, name, args...)
}

func (a *App) detectRepoFromGit(ctx context.Context) (string, string, error) {
	out, err := a.Runner.Run(ctx, "git", "config", "--get", "remote.origin.url")
	if err != nil {
		return "", "", err
	}
	return parseRemote(out)
}

var remotePattern = regexp.MustCompile(`(?i)(?:github\.com[:/])([^/]+)/([^/\s]+?)(?:\.git)?$`)

func parseRemote(remote string) (string, string, error) {
	remote = strings.TrimSpace(remote)
	match := remotePattern.FindStringSubmatch(remote)
	if len(match) < 3 {
		return "", "", fmt.Errorf("unsupported remote: %s", remote)
	}
	return match[1], match[2], nil
}

func relPath(root, path string) string {
	if root == "" {
		return filepath.ToSlash(path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func loadConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, fmt.Errorf("not initialized: run `gh-issue-sync init` first")
		}
		return cfg, err
	}
	return cfg, nil
}

func repoSlug(cfg config.Config) string {
	owner := strings.TrimSpace(cfg.Repository.Owner)
	repo := strings.TrimSpace(cfg.Repository.Repo)
	if owner == "" || repo == "" {
		return ""
	}
	return owner + "/" + repo
}
