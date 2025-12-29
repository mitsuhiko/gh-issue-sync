package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/mitsuhiko/gh-issue-sync/internal/config"
	"github.com/mitsuhiko/gh-issue-sync/internal/ghcli"
	"github.com/mitsuhiko/gh-issue-sync/internal/issue"
	"github.com/mitsuhiko/gh-issue-sync/internal/localid"
	"github.com/mitsuhiko/gh-issue-sync/internal/lock"
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

type DiffOptions struct {
	Remote bool
}

type ViewOptions struct {
	Raw bool
}

type ListOptions struct {
	All      bool
	State    string
	Label    []string
	Assignee string
	Local    bool
	Modified bool
}

type IssueFile struct {
	Issue issue.Issue
	Path  string
	State string
}

// LabelCache stores the synced labels from GitHub
type LabelCache struct {
	Labels   []LabelEntry `json:"labels"`
	SyncedAt time.Time    `json:"synced_at"`
}

// LabelEntry represents a single label with its color
type LabelEntry struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// MilestoneCache stores the synced milestones from GitHub
type MilestoneCache struct {
	Milestones []MilestoneEntry `json:"milestones"`
	SyncedAt   time.Time        `json:"synced_at"`
}

// MilestoneEntry represents a single milestone
type MilestoneEntry struct {
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	DueOn       *string `json:"due_on,omitempty"`
	State       string  `json:"state"`
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

	// Acquire lock
	lck, err := lock.Acquire(p.SyncDir, lock.DefaultTimeout)
	if err != nil {
		return err
	}
	defer lck.Release()

	client := ghcli.NewClient(a.Runner, repoSlug(cfg))
	t := a.Theme

	localIssues, err := loadLocalIssues(p)
	if err != nil {
		return err
	}

	var remoteIssues []issue.Issue
	var labelColors map[string]string

	if len(args) > 0 {
		// Fetch specific issues by number
		labelColors = a.fetchLabelColors(ctx, client)

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
		// Enrich with relationships
		if err := client.EnrichWithRelationshipsBatch(ctx, remoteIssues); err != nil {
			fmt.Fprintf(a.Err, "%s fetching relationships: %v\n", t.WarningText("Warning:"), err)
		}
	} else {
		state := "open"
		if opts.All {
			state = "all"
		}

		// Collect issue numbers we need to fetch for closed issues
		var toFetch []string
		if !opts.All {
			// We don't know remote issue numbers yet, so we'll collect all local non-local issues
			// and filter after we get the open issues
			for _, local := range localIssues {
				if !local.Issue.Number.IsLocal() {
					toFetch = append(toFetch, local.Issue.Number.String())
				}
			}
		}

		// Run both queries in parallel
		type listResult struct {
			result ghcli.ListIssuesResult
			err    error
		}
		type batchResult struct {
			issues map[string]issue.Issue
			err    error
		}

		listCh := make(chan listResult, 1)
		batchCh := make(chan batchResult, 1)

		go func() {
			r, e := client.ListIssuesWithRelationships(ctx, state, opts.Label)
			listCh <- listResult{r, e}
		}()

		go func() {
			if len(toFetch) > 0 {
				r, e := client.GetIssuesBatch(ctx, toFetch)
				batchCh <- batchResult{r, e}
			} else {
				batchCh <- batchResult{nil, nil}
			}
		}()

		listRes := <-listCh
		if listRes.err != nil {
			return listRes.err
		}
		remoteIssues = listRes.result.Issues
		labelColors = listRes.result.LabelColors

		batchRes := <-batchCh
		if batchRes.err == nil && len(batchRes.issues) > 0 {
			// Filter out issues we already have from the open list
			fetched := make(map[string]struct{}, len(remoteIssues))
			for _, ri := range remoteIssues {
				fetched[ri.Number.String()] = struct{}{}
			}
			for num, iss := range batchRes.issues {
				if _, ok := fetched[num]; !ok {
					remoteIssues = append(remoteIssues, iss)
				}
			}
		}
	}

	localIssues, err = loadLocalIssues(p)
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

		// Save labels to cache
		if len(labelColors) > 0 {
			labels := make([]LabelEntry, 0, len(labelColors))
			for name, color := range labelColors {
				labels = append(labels, LabelEntry{Name: name, Color: color})
			}
			// Sort for consistent output
			sort.Slice(labels, func(i, j int) bool {
				return strings.ToLower(labels[i].Name) < strings.ToLower(labels[j].Name)
			})
			cache := LabelCache{Labels: labels, SyncedAt: now}
			if err := saveLabelCache(p, cache); err != nil {
				fmt.Fprintf(a.Err, "%s saving label cache: %v\n", t.WarningText("Warning:"), err)
			}
		}

		// Fetch and save milestones to cache
		milestones, err := client.ListMilestones(ctx)
		if err != nil {
			fmt.Fprintf(a.Err, "%s fetching milestones: %v\n", t.WarningText("Warning:"), err)
		} else {
			entries := make([]MilestoneEntry, 0, len(milestones))
			for _, m := range milestones {
				entries = append(entries, MilestoneEntry{
					Title:       m.Title,
					Description: m.Description,
					DueOn:       m.DueOn,
					State:       m.State,
				})
			}
			// Sort for consistent output
			sort.Slice(entries, func(i, j int) bool {
				return strings.ToLower(entries[i].Title) < strings.ToLower(entries[j].Title)
			})
			msCache := MilestoneCache{Milestones: entries, SyncedAt: now}
			if err := saveMilestoneCache(p, msCache); err != nil {
				fmt.Fprintf(a.Err, "%s saving milestone cache: %v\n", t.WarningText("Warning:"), err)
			}
		}
	}

	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		fmt.Fprintf(a.Err, "%s %s\n", t.WarningText("Conflicts (local changes, skipped):"), strings.Join(conflicts, ", "))
	}
	if unchanged > 0 {
		fmt.Fprintf(a.Out, "%s\n", t.MutedText(fmt.Sprintf("No changes needed for %d issue(s)", unchanged)))
	}

	// Restore locally deleted issues (originals exist but no local file)
	if len(args) == 0 {
		if err := a.restoreDeletedIssues(ctx, p, client, labelColors); err != nil {
			return err
		}
	}

	return nil
}

// restoreDeletedIssues finds issues that have originals but no local file and restores them
func (a *App) restoreDeletedIssues(ctx context.Context, p paths.Paths, client *ghcli.Client, labelColors map[string]string) error {
	t := a.Theme

	// List all originals
	entries, err := os.ReadDir(p.OriginalsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	// Build set of local issue numbers
	localIssues, err := loadLocalIssues(p)
	if err != nil {
		return err
	}
	localNumbers := make(map[string]struct{}, len(localIssues))
	for _, item := range localIssues {
		localNumbers[item.Issue.Number.String()] = struct{}{}
	}

	// Find orphaned originals (original exists but no local file)
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
		if _, exists := localNumbers[number]; !exists {
			orphaned = append(orphaned, number)
		}
	}

	if len(orphaned) == 0 {
		return nil
	}

	// Fetch and restore orphaned issues from GitHub
	for _, number := range orphaned {
		remote, err := client.GetIssue(ctx, number)
		if err != nil {
			fmt.Fprintf(a.Err, "%s restoring #%s: %v\n", t.WarningText("Warning:"), number, err)
			continue
		}
		if err := client.EnrichWithRelationships(ctx, &remote); err != nil {
			fmt.Fprintf(a.Err, "%s fetching relationships for #%s: %v\n", t.WarningText("Warning:"), number, err)
		}

		remote.State = strings.ToLower(remote.State)
		remote.SyncedAt = ptrTime(a.Now().UTC())

		targetDir := p.OpenDir
		if remote.State == "closed" {
			targetDir = p.ClosedDir
		}
		newPath := issue.PathFor(targetDir, remote.Number, remote.Title)

		if err := issue.WriteFile(newPath, remote); err != nil {
			return err
		}
		if err := writeOriginalIssue(p, remote); err != nil {
			return err
		}

		fmt.Fprintln(a.Out, t.FormatIssueHeader("R", remote.Number.String(), remote.Title))
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
		colors[strings.ToLower(l.Name)] = l.Color
	}
	return colors
}

func (a *App) Push(ctx context.Context, opts PushOptions, args []string) error {
	p := paths.New(a.Root)
	cfg, err := loadConfig(p.ConfigPath)
	if err != nil {
		return err
	}

	// Acquire lock
	lck, err := lock.Acquire(p.SyncDir, lock.DefaultTimeout)
	if err != nil {
		return err
	}
	defer lck.Release()

	client := ghcli.NewClient(a.Runner, repoSlug(cfg))
	t := a.Theme

	// Load label cache (or fetch from remote if not cached)
	labelCache, err := loadLabelCache(p)
	if err != nil {
		fmt.Fprintf(a.Err, "%s loading label cache: %v\n", t.WarningText("Warning:"), err)
	}
	labelColors := labelCacheToColorMap(labelCache)

	// If no cache, fetch from remote
	if len(labelColors) == 0 {
		labelColors = a.fetchLabelColors(ctx, client)
		// Update cache for future use
		labelCache = labelsFromColorMap(labelColors, a.Now().UTC())
	}

	// Load milestone cache (or fetch from remote if not cached)
	milestoneCache, err := loadMilestoneCache(p)
	if err != nil {
		fmt.Fprintf(a.Err, "%s loading milestone cache: %v\n", t.WarningText("Warning:"), err)
	}
	knownMilestones := milestoneNames(milestoneCache)

	// If no cache, fetch from remote
	if len(knownMilestones) == 0 {
		milestones, err := client.ListMilestones(ctx)
		if err == nil {
			for _, m := range milestones {
				knownMilestones[strings.ToLower(m.Title)] = struct{}{}
				milestoneCache.Milestones = append(milestoneCache.Milestones, MilestoneEntry{
					Title:       m.Title,
					Description: m.Description,
					DueOn:       m.DueOn,
					State:       m.State,
				})
			}
			milestoneCache.SyncedAt = a.Now().UTC()
		}
	}

	localIssues, err := loadLocalIssues(p)
	if err != nil {
		return err
	}
	filteredIssues, err := filterIssuesByArgs(a.Root, localIssues, args)
	if err != nil {
		return err
	}

	// Collect all labels and milestones that will be needed
	neededLabels := make(map[string]struct{})
	neededMilestones := make(map[string]struct{})
	for _, item := range filteredIssues {
		for _, label := range item.Issue.Labels {
			neededLabels[label] = struct{}{}
		}
		if item.Issue.Milestone != "" {
			neededMilestones[item.Issue.Milestone] = struct{}{}
		}
	}

	// Create any missing labels
	labelCacheUpdated := false
	for label := range neededLabels {
		if _, exists := labelColors[strings.ToLower(label)]; !exists {
			if opts.DryRun {
				fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Would create label"), label)
				continue
			}
			color := randomLabelColor()
			if err := client.CreateLabel(ctx, label, color); err != nil {
				fmt.Fprintf(a.Err, "%s creating label %q: %v\n", t.WarningText("Warning:"), label, err)
				continue
			}
			fmt.Fprintf(a.Out, "%s %s\n", t.SuccessText("Created label"), label)
			labelColors[strings.ToLower(label)] = color
			labelCache.Labels = append(labelCache.Labels, LabelEntry{Name: label, Color: color})
			labelCacheUpdated = true
		}
	}

	// Create any missing milestones
	milestoneCacheUpdated := false
	for milestone := range neededMilestones {
		if _, exists := knownMilestones[strings.ToLower(milestone)]; !exists {
			if opts.DryRun {
				fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Would create milestone"), milestone)
				continue
			}
			if err := client.CreateMilestone(ctx, milestone); err != nil {
				fmt.Fprintf(a.Err, "%s creating milestone %q: %v\n", t.WarningText("Warning:"), milestone, err)
				continue
			}
			fmt.Fprintf(a.Out, "%s %s\n", t.SuccessText("Created milestone"), milestone)
			knownMilestones[strings.ToLower(milestone)] = struct{}{}
			milestoneCache.Milestones = append(milestoneCache.Milestones, MilestoneEntry{
				Title: milestone,
				State: "open",
			})
			milestoneCacheUpdated = true
		}
	}

	// Save updated label cache
	if labelCacheUpdated && !opts.DryRun {
		labelCache.SyncedAt = a.Now().UTC()
		if err := saveLabelCache(p, labelCache); err != nil {
			fmt.Fprintf(a.Err, "%s saving label cache: %v\n", t.WarningText("Warning:"), err)
		}
	}

	// Save updated milestone cache
	if milestoneCacheUpdated && !opts.DryRun {
		milestoneCache.SyncedAt = a.Now().UTC()
		if err := saveMilestoneCache(p, milestoneCache); err != nil {
			fmt.Fprintf(a.Err, "%s saving milestone cache: %v\n", t.WarningText("Warning:"), err)
		}
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

		// Sync relationships for newly created issues (now that T-numbers are resolved)
		if !opts.DryRun {
			for number := range createdNumbers {
				// Find the issue in filteredIssues
				for _, item := range filteredIssues {
					if item.Issue.Number.String() == number {
						if err := client.SyncRelationships(ctx, number, item.Issue); err != nil {
							fmt.Fprintf(a.Err, "%s syncing relationships for #%s: %v\n",
								t.WarningText("Warning:"), number, err)
						}
						break
					}
				}
			}
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
		// Enrich with relationships for accurate conflict check
		if err := client.EnrichWithRelationships(ctx, &remote); err != nil {
			// Log but don't fail
			fmt.Fprintf(a.Err, "%s fetching relationships for #%s: %v\n",
				t.WarningText("Warning:"), item.Issue.Number, err)
		}
		if hasOriginal && !issue.EqualForConflictCheck(remote, original) {
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

		// Sync parent and blocking relationships via GraphQL
		if err := client.SyncRelationships(ctx, item.Issue.Number.String(), item.Issue); err != nil {
			// Log but don't fail - relationships might not be supported
			fmt.Fprintf(a.Err, "%s syncing relationships for #%s: %v\n",
				t.WarningText("Warning:"), item.Issue.Number, err)
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

func (a *App) List(ctx context.Context, opts ListOptions) error {
	p := paths.New(a.Root)
	if _, err := loadConfig(p.ConfigPath); err != nil {
		return err
	}
	t := a.Theme

	// Load label colors for display
	labelCache, _ := loadLabelCache(p)
	labelColors := labelCacheToColorMap(labelCache)

	localIssues, err := loadLocalIssues(p)
	if err != nil {
		return err
	}

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

// findIssueByRef finds an issue by number, local ID (T...), or file path
func findIssueByRef(root string, p paths.Paths, ref string) (IssueFile, error) {
	ref = strings.TrimSpace(ref)

	// Check if it's a file path
	if strings.HasSuffix(ref, ".md") || strings.Contains(ref, string(os.PathSeparator)) {
		path := ref
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		parsed, err := issue.ParseFile(path)
		if err != nil {
			return IssueFile{}, fmt.Errorf("failed to parse %s: %w", ref, err)
		}
		// Determine state from path
		state := "open"
		if strings.Contains(path, string(os.PathSeparator)+"closed"+string(os.PathSeparator)) {
			state = "closed"
		}
		parsed.State = state
		return IssueFile{Issue: parsed, Path: path, State: state}, nil
	}

	// Otherwise look up by number
	return findIssueByNumber(p, ref)
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

func (a *App) Diff(ctx context.Context, number string, opts DiffOptions) error {
	p := paths.New(a.Root)
	cfg, err := loadConfig(p.ConfigPath)
	if err != nil {
		return err
	}
	t := a.Theme

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

	// Print header
	fmt.Fprintf(a.Out, "%s %s %s\n\n",
		t.Bold("Diff for"),
		t.AccentText("#"+local.Number.String()),
		t.MutedText(fmt.Sprintf("(local vs %s)", baseLabel)))

	// Diff metadata fields
	if base.Title != local.Title {
		fmt.Fprintln(a.Out, t.FormatChange("title", fmt.Sprintf("%q", base.Title), fmt.Sprintf("%q", local.Title)))
	}
	if base.State != local.State {
		fmt.Fprintln(a.Out, t.FormatChange("state", base.State, local.State))
	}
	if normalizeOptional(base.StateReason) != normalizeOptional(local.StateReason) {
		fmt.Fprintln(a.Out, t.FormatChange("state_reason", formatOptionalStringPtr(base.StateReason), formatOptionalStringPtr(local.StateReason)))
	}
	if !stringSlicesEqual(base.Labels, local.Labels) {
		fmt.Fprintln(a.Out, t.FormatChange("labels", formatStringList(base.Labels), formatStringList(local.Labels)))
	}
	if !stringSlicesEqual(base.Assignees, local.Assignees) {
		fmt.Fprintln(a.Out, t.FormatChange("assignees", formatStringList(base.Assignees), formatStringList(local.Assignees)))
	}
	if base.Milestone != local.Milestone {
		fmt.Fprintln(a.Out, t.FormatChange("milestone", formatOptionalString(base.Milestone), formatOptionalString(local.Milestone)))
	}

	// Diff body with unified diff format
	if base.Body != local.Body {
		fmt.Fprintln(a.Out)
		fmt.Fprintln(a.Out, t.Bold("Body:"))
		a.printUnifiedDiff(base.Body, local.Body, baseLabel, "local")
	}

	return nil
}

func (a *App) printUnifiedDiff(oldText, newText, oldLabel, newLabel string) {
	t := a.Theme

	oldLines := splitLines(oldText)
	newLines := splitLines(newText)

	// Simple line-by-line diff using LCS
	ops := computeDiff(oldLines, newLines)

	fmt.Fprintf(a.Out, "%s\n", t.MutedText(fmt.Sprintf("--- %s", oldLabel)))
	fmt.Fprintf(a.Out, "%s\n", t.MutedText(fmt.Sprintf("+++ %s", newLabel)))

	for _, op := range ops {
		switch op.Type {
		case diffEqual:
			fmt.Fprintf(a.Out, " %s\n", op.Text)
		case diffDelete:
			fmt.Fprintf(a.Out, "%s%s\n", t.Fg(t.Removed, "-"), t.Fg(t.OldValue, op.Text))
		case diffInsert:
			fmt.Fprintf(a.Out, "%s%s\n", t.Fg(t.Added, "+"), t.Fg(t.NewValue, op.Text))
		}
	}
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

func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	text = strings.TrimSuffix(text, "\n")
	return strings.Split(text, "\n")
}

// computeDiff computes a simple line-based diff using the LCS algorithm
func computeDiff(oldLines, newLines []string) []diffOp {
	// Build LCS table
	m, n := len(oldLines), len(newLines)
	lcs := make([][]int, m+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}

	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if oldLines[i-1] == newLines[j-1] {
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
		if i > 0 && j > 0 && oldLines[i-1] == newLines[j-1] {
			ops = append(ops, diffOp{Type: diffEqual, Text: oldLines[i-1]})
			i--
			j--
		} else if j > 0 && (i == 0 || lcs[i][j-1] >= lcs[i-1][j]) {
			ops = append(ops, diffOp{Type: diffInsert, Text: newLines[j-1]})
			j--
		} else {
			ops = append(ops, diffOp{Type: diffDelete, Text: oldLines[i-1]})
			i--
		}
	}

	// Reverse to get correct order
	for left, right := 0, len(ops)-1; left < right; left, right = left+1, right-1 {
		ops[left], ops[right] = ops[right], ops[left]
	}

	return ops
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

func loadLabelCache(p paths.Paths) (LabelCache, error) {
	var cache LabelCache
	data, err := os.ReadFile(p.LabelsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cache, nil
		}
		return cache, err
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return cache, err
	}
	return cache, nil
}

func saveLabelCache(p paths.Paths, cache LabelCache) error {
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(p.LabelsPath, data, 0o644)
}

// labelCacheToColorMap converts a LabelCache to a map of lowercase name -> color for quick lookups.
func labelCacheToColorMap(cache LabelCache) map[string]string {
	m := make(map[string]string, len(cache.Labels))
	for _, l := range cache.Labels {
		m[strings.ToLower(l.Name)] = l.Color
	}
	return m
}

// labelsFromColorMap creates a LabelCache from a color map.
func labelsFromColorMap(colors map[string]string, syncedAt time.Time) LabelCache {
	labels := make([]LabelEntry, 0, len(colors))
	for name, color := range colors {
		labels = append(labels, LabelEntry{Name: name, Color: color})
	}
	sort.Slice(labels, func(i, j int) bool {
		return strings.ToLower(labels[i].Name) < strings.ToLower(labels[j].Name)
	})
	return LabelCache{Labels: labels, SyncedAt: syncedAt}
}

func loadMilestoneCache(p paths.Paths) (MilestoneCache, error) {
	var cache MilestoneCache
	data, err := os.ReadFile(p.MilestonesPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cache, nil
		}
		return cache, err
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return cache, err
	}
	return cache, nil
}

func saveMilestoneCache(p paths.Paths, cache MilestoneCache) error {
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(p.MilestonesPath, data, 0o644)
}

// milestoneNames returns a set of milestone titles (case-insensitive lookup).
func milestoneNames(cache MilestoneCache) map[string]struct{} {
	m := make(map[string]struct{}, len(cache.Milestones))
	for _, ms := range cache.Milestones {
		m[strings.ToLower(ms.Title)] = struct{}{}
	}
	return m
}

// randomLabelColor returns a random visually pleasing color for labels.
func randomLabelColor() string {
	colors := []string{
		"0052CC", "00875A", "5243AA", "FF5630", "FFAB00",
		"36B37E", "00B8D9", "6554C0", "FF8B00", "57D9A3",
		"1D7AFC", "E774BB", "8777D9", "2684FF", "FF991F",
	}
	return colors[rand.Intn(len(colors))]
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

// localRefPattern matches local issue references like #T1, #T42, #Tabc123 (T followed by alphanumerics)
var localRefPattern = regexp.MustCompile(`#(T[a-zA-Z0-9]+)`)

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

func applyMapping(issueItem *issue.Issue, mapping map[string]string) bool {
	changed := false

	// Apply mapping to body
	body := localRefPattern.ReplaceAllStringFunc(issueItem.Body, func(match string) string {
		id := strings.TrimPrefix(match, "#")
		if real, ok := mapping[id]; ok {
			changed = true
			return "#" + real
		}
		return match
	})
	if body != issueItem.Body {
		issueItem.Body = body
		changed = true
	}

	// Apply mapping to title
	title := localRefPattern.ReplaceAllStringFunc(issueItem.Title, func(match string) string {
		id := strings.TrimPrefix(match, "#")
		if real, ok := mapping[id]; ok {
			changed = true
			return "#" + real
		}
		return match
	})
	if title != issueItem.Title {
		issueItem.Title = title
		changed = true
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
