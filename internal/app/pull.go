package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mitsuhiko/gh-issue-sync/internal/config"
	"github.com/mitsuhiko/gh-issue-sync/internal/ghcli"
	"github.com/mitsuhiko/gh-issue-sync/internal/issue"
	"github.com/mitsuhiko/gh-issue-sync/internal/lock"
	"github.com/mitsuhiko/gh-issue-sync/internal/paths"
)

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
		// Resolve args: can be issue numbers, local IDs, or paths
		labelColors = a.fetchLabelColors(ctx, client)

		// First, try to match args against local issues (for paths and local IDs)
		var remoteNumbers []string
		matched, _ := filterIssuesByArgs(a.Root, localIssues, args)
		matchedArgs := make(map[string]struct{})
		for _, item := range matched {
			if !item.Issue.Number.IsLocal() {
				remoteNumbers = append(remoteNumbers, item.Issue.Number.String())
			}
			matchedArgs[item.Issue.Number.String()] = struct{}{}
		}
		// Also include any args that look like remote issue numbers but weren't matched locally
		for _, arg := range args {
			arg = strings.TrimSpace(arg)
			if arg == "" {
				continue
			}
			// Skip paths (they should have been matched above or don't exist)
			if strings.HasSuffix(arg, ".md") || strings.Contains(arg, string(os.PathSeparator)) {
				continue
			}
			// If not already matched as a local issue, treat as remote number
			if _, ok := matchedArgs[arg]; !ok {
				remoteNumbers = append(remoteNumbers, arg)
			}
		}

		for _, number := range remoteNumbers {
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

		progress := newProgressReporter(a.Err, a.Theme)
		client.SetProgress(progress.Update)

		// Determine if we can do an incremental sync
		// Incremental sync: only fetch issues updated since last pull
		// We use "all" state for incremental sync to catch issues that were closed
		var since time.Time
		isIncremental := false
		if cfg.Sync.LastFullPull != nil && !opts.All && !opts.Full && len(opts.Label) == 0 {
			since = *cfg.Sync.LastFullPull
			isIncremental = true
		}

		// Collect issue numbers we need to fetch for closed issues (only for full sync)
		var toFetch []string
		if !opts.All && !isIncremental {
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
			listOpts := ghcli.ListIssuesOptions{
				State:  state,
				Labels: opts.Label,
			}
			if isIncremental {
				// For incremental sync, fetch all states to catch closed issues
				listOpts.State = "all"
				listOpts.Since = since
			}
			r, e := client.ListIssuesWithRelationships(ctx, listOpts)
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
		progress.Done()
		if listRes.err != nil {
			return listRes.err
		}
		remoteIssues = listRes.result.Issues

		if isIncremental && len(remoteIssues) == 0 {
			// Nothing changed since last sync - fast path
			// Still update the last pull timestamp
			now := a.Now().UTC()
			cfg.Sync.LastFullPull = &now
			if err := config.Save(p.ConfigPath, cfg); err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "%s\n", t.MutedText("Nothing to pull: no issues updated since last sync"))
			return nil
		}

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

		// Fetch all labels separately (GraphQL only returns first 100)
		labelColors = a.fetchLabelColors(ctx, client)
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

		type milestonesResult struct {
			items []ghcli.Milestone
			err   error
		}
		type issueTypesResult struct {
			items []ghcli.IssueType
			err   error
		}
		type projectsResult struct {
			items []ghcli.Project
			err   error
		}

		milestonesCh := make(chan milestonesResult, 1)
		issueTypesCh := make(chan issueTypesResult, 1)
		projectsCh := make(chan projectsResult, 1)

		go func() {
			items, err := client.ListMilestones(ctx)
			milestonesCh <- milestonesResult{items: items, err: err}
		}()
		go func() {
			items, err := client.ListIssueTypes(ctx)
			issueTypesCh <- issueTypesResult{items: items, err: err}
		}()
		go func() {
			items, err := client.ListProjects(ctx)
			projectsCh <- projectsResult{items: items, err: err}
		}()

		milestonesRes := <-milestonesCh
		if milestonesRes.err != nil {
			fmt.Fprintf(a.Err, "%s fetching milestones: %v\n", t.WarningText("Warning:"), milestonesRes.err)
		} else {
			entries := make([]MilestoneEntry, 0, len(milestonesRes.items))
			for _, m := range milestonesRes.items {
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

		issueTypesRes := <-issueTypesCh
		if issueTypesRes.err != nil {
			fmt.Fprintf(a.Err, "%s fetching issue types: %v\n", t.WarningText("Warning:"), issueTypesRes.err)
		} else if len(issueTypesRes.items) > 0 {
			entries := make([]IssueTypeEntry, 0, len(issueTypesRes.items))
			for _, it := range issueTypesRes.items {
				entries = append(entries, IssueTypeEntry{
					ID:          it.ID,
					Name:        it.Name,
					Description: it.Description,
				})
			}
			// Sort for consistent output
			sort.Slice(entries, func(i, j int) bool {
				return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
			})
			itCache := IssueTypeCache{IssueTypes: entries, SyncedAt: now}
			if err := saveIssueTypeCache(p, itCache); err != nil {
				fmt.Fprintf(a.Err, "%s saving issue type cache: %v\n", t.WarningText("Warning:"), err)
			}
		}

		projectsRes := <-projectsCh
		if projectsRes.err != nil {
			if errors.Is(projectsRes.err, ghcli.ErrMissingProjectScope) {
				// Check if any local issues use projects
				hasProjects := false
				for _, item := range localIssues {
					if len(item.Issue.Projects) > 0 {
						hasProjects = true
						break
					}
				}
				if hasProjects {
					fmt.Fprintf(a.Err, "%s %v\n", t.WarningText("Warning:"), projectsRes.err)
				}
			}
		} else if len(projectsRes.items) > 0 {
			entries := make([]ProjectEntry, 0, len(projectsRes.items))
			for _, proj := range projectsRes.items {
				entries = append(entries, ProjectEntry{
					ID:    proj.ID,
					Title: proj.Title,
				})
			}
			// Sort for consistent output
			sort.Slice(entries, func(i, j int) bool {
				return strings.ToLower(entries[i].Title) < strings.ToLower(entries[j].Title)
			})
			projCache := ProjectCache{Projects: entries, SyncedAt: now}
			if err := saveProjectCache(p, projCache); err != nil {
				fmt.Fprintf(a.Err, "%s saving project cache: %v\n", t.WarningText("Warning:"), err)
			}
		}
	}

	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		fmt.Fprintf(a.Err, "%s %s\n", t.WarningText("Conflicts (local changes, skipped):"), strings.Join(conflicts, ", "))
	}
	if unchanged > 0 {
		noun := "issues"
		if unchanged == 1 {
			noun = "issue"
		}
		fmt.Fprintf(a.Out, "%s\n", t.MutedText(fmt.Sprintf("Nothing to pull: %d %s up to date", unchanged, noun)))
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
