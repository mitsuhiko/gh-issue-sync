package app

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mitsuhiko/gh-issue-sync/internal/ghcli"
	"github.com/mitsuhiko/gh-issue-sync/internal/issue"
	"github.com/mitsuhiko/gh-issue-sync/internal/lock"
	"github.com/mitsuhiko/gh-issue-sync/internal/paths"
)

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

	// Load issue type cache (or fetch from remote if not cached)
	issueTypeCache, err := loadIssueTypeCache(p)
	if err != nil {
		fmt.Fprintf(a.Err, "%s loading issue type cache: %v\n", t.WarningText("Warning:"), err)
	}
	knownIssueTypes := issueTypeByName(issueTypeCache)

	// If no cache, fetch from remote
	if len(knownIssueTypes) == 0 {
		issueTypes, err := client.ListIssueTypes(ctx)
		if err == nil {
			for _, it := range issueTypes {
				knownIssueTypes[strings.ToLower(it.Name)] = IssueTypeEntry{
					ID:          it.ID,
					Name:        it.Name,
					Description: it.Description,
				}
				issueTypeCache.IssueTypes = append(issueTypeCache.IssueTypes, IssueTypeEntry{
					ID:          it.ID,
					Name:        it.Name,
					Description: it.Description,
				})
			}
			issueTypeCache.SyncedAt = a.Now().UTC()
		}
	}

	// Load project cache (or fetch from remote if not cached)
	projectCache, err := loadProjectCache(p)
	if err != nil {
		// Don't warn - projects are optional
	}
	knownProjects := projectByTitle(projectCache)

	// If no cache, fetch from remote
	if len(knownProjects) == 0 {
		projects, err := client.ListProjects(ctx)
		if err == nil {
			for _, proj := range projects {
				knownProjects[strings.ToLower(proj.Title)] = ProjectEntry{
					ID:    proj.ID,
					Title: proj.Title,
				}
				projectCache.Projects = append(projectCache.Projects, ProjectEntry{
					ID:    proj.ID,
					Title: proj.Title,
				})
			}
			projectCache.SyncedAt = a.Now().UTC()
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

		// Sync relationships and issue type for newly created issues (now that T-numbers are resolved)
		if !opts.DryRun {
			for number := range createdNumbers {
				// Find the issue in filteredIssues
				for _, item := range filteredIssues {
					if item.Issue.Number.String() == number {
						if err := client.SyncRelationships(ctx, number, item.Issue); err != nil {
							fmt.Fprintf(a.Err, "%s syncing relationships for #%s: %v\n",
								t.WarningText("Warning:"), number, err)
						}
						// Set issue type if specified
						if item.Issue.IssueType != "" {
							if it, ok := knownIssueTypes[strings.ToLower(item.Issue.IssueType)]; ok {
								if err := client.SetIssueType(ctx, number, it.ID); err != nil {
									fmt.Fprintf(a.Err, "%s setting issue type for #%s: %v\n",
										t.WarningText("Warning:"), number, err)
								}
							} else {
								fmt.Fprintf(a.Err, "%s unknown issue type %q for #%s\n",
									t.WarningText("Warning:"), item.Issue.IssueType, number)
							}
						}
						// Add to projects if specified
						if len(item.Issue.Projects) > 0 {
							projectIDs := make(map[string]string)
							for _, p := range knownProjects {
								projectIDs[strings.ToLower(p.Title)] = p.ID
							}
							if err := client.SyncProjects(ctx, number, item.Issue.Projects, projectIDs); err != nil {
								fmt.Fprintf(a.Err, "%s syncing projects for #%s: %v\n",
									t.WarningText("Warning:"), number, err)
							}
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
		// Use remote as baseline if no original exists (for state transitions)
		baseline := original
		if !hasOriginal {
			baseline = remote
		}
		change := diffIssue(baseline, item.Issue)
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

		// Sync issue type via GraphQL (if changed)
		if change.IssueType != nil {
			issueTypeID := ""
			if *change.IssueType != "" {
				if it, ok := knownIssueTypes[strings.ToLower(*change.IssueType)]; ok {
					issueTypeID = it.ID
				} else {
					fmt.Fprintf(a.Err, "%s unknown issue type %q for #%s\n",
						t.WarningText("Warning:"), *change.IssueType, item.Issue.Number)
				}
			}
			if issueTypeID != "" || *change.IssueType == "" {
				if err := client.SetIssueType(ctx, item.Issue.Number.String(), issueTypeID); err != nil {
					fmt.Fprintf(a.Err, "%s setting issue type for #%s: %v\n",
						t.WarningText("Warning:"), item.Issue.Number, err)
				}
			}
		}

		// Sync parent and blocking relationships via GraphQL
		if err := client.SyncRelationships(ctx, item.Issue.Number.String(), item.Issue); err != nil {
			// Log but don't fail - relationships might not be supported
			fmt.Fprintf(a.Err, "%s syncing relationships for #%s: %v\n",
				t.WarningText("Warning:"), item.Issue.Number, err)
		}

		// Sync projects via GraphQL (if changed)
		if len(change.AddProjects) > 0 || len(change.RemoveProjects) > 0 {
			projectIDs := make(map[string]string)
			for _, p := range knownProjects {
				projectIDs[strings.ToLower(p.Title)] = p.ID
			}
			if err := client.SyncProjects(ctx, item.Issue.Number.String(), item.Issue.Projects, projectIDs); err != nil {
				fmt.Fprintf(a.Err, "%s syncing projects for #%s: %v\n",
					t.WarningText("Warning:"), item.Issue.Number, err)
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
		noun := "issues"
		if unchanged == 1 {
			noun = "issue"
		}
		fmt.Fprintf(a.Out, "%s\n", t.MutedText(fmt.Sprintf("Nothing to push: %d %s up to date", unchanged, noun)))
	}

	// Handle pending comments (unless --no-comments)
	if !opts.NoComments {
		pendingComments := loadAllPendingComments(p)

		// Filter comments to only those for issues we're pushing (if args specified)
		var commentsToPost []PendingComment
		if len(args) > 0 {
			// Build set of issue numbers we're pushing
			pushingNumbers := make(map[string]struct{})
			for _, item := range filteredIssues {
				pushingNumbers[item.Issue.Number.String()] = struct{}{}
			}
			for _, comment := range pendingComments {
				if _, ok := pushingNumbers[comment.IssueNumber.String()]; ok {
					commentsToPost = append(commentsToPost, comment)
				}
			}
		} else {
			for _, comment := range pendingComments {
				commentsToPost = append(commentsToPost, comment)
			}
		}

		// Sort by issue number for consistent output
		sort.Slice(commentsToPost, func(i, j int) bool {
			return commentsToPost[i].IssueNumber.String() < commentsToPost[j].IssueNumber.String()
		})

		// Skip comments for issues that had conflicts
		conflictSet := make(map[string]struct{})
		for _, num := range conflicts {
			conflictSet[num] = struct{}{}
		}

		for _, comment := range commentsToPost {
			numStr := comment.IssueNumber.String()

			// Skip local issues (can't post comments to issues that don't exist yet)
			if comment.IssueNumber.IsLocal() {
				// Check if it was mapped to a real number
				if realNum, ok := mapping[numStr]; ok {
					comment.IssueNumber = issue.IssueNumber(realNum)
					numStr = realNum
				} else {
					continue
				}
			}

			// Skip issues that had conflicts
			if _, isConflict := conflictSet[numStr]; isConflict {
				continue
			}

			if opts.DryRun {
				fmt.Fprintf(a.Out, "%s #%s\n", t.MutedText("Would post comment to"), numStr)
				continue
			}

			if err := client.CreateComment(ctx, numStr, comment.Body); err != nil {
				fmt.Fprintf(a.Err, "%s posting comment to #%s: %v\n", t.WarningText("Warning:"), numStr, err)
				continue
			}

			// Delete the comment file on success
			if err := deletePendingComment(comment); err != nil {
				fmt.Fprintf(a.Err, "%s removing comment file %s: %v\n", t.WarningText("Warning:"), relPath(a.Root, comment.Path), err)
			}

			fmt.Fprintf(a.Out, "%s #%s\n", t.SuccessText("Posted comment to"), numStr)
		}
	}

	return nil
}
