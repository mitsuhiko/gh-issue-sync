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
)

func (a *App) Push(ctx context.Context, opts PushOptions, args []string) error {
	p := a.issuePaths()
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

	// Count missing labels and milestones
	var missingLabels []string
	for label := range neededLabels {
		if _, exists := labelColors[strings.ToLower(label)]; !exists {
			missingLabels = append(missingLabels, label)
		}
	}
	sort.Strings(missingLabels)

	var missingMilestones []string
	for milestone := range neededMilestones {
		if _, exists := knownMilestones[strings.ToLower(milestone)]; !exists {
			missingMilestones = append(missingMilestones, milestone)
		}
	}
	sort.Strings(missingMilestones)

	// Count new issues (T-numbered)
	var newIssues []*IssueFile
	for i := range filteredIssues {
		if filteredIssues[i].Issue.Number.IsLocal() {
			newIssues = append(newIssues, &filteredIssues[i])
		}
	}

	// Count comments to post
	var commentsToPost []PendingComment
	if !opts.NoComments {
		pendingComments := loadAllPendingComments(p)
		if len(args) > 0 {
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
		sort.Slice(commentsToPost, func(i, j int) bool {
			return commentsToPost[i].IssueNumber.String() < commentsToPost[j].IssueNumber.String()
		})
	}

	// Handle dry-run: we need to check pending updates for dry-run output
	if opts.DryRun {
		for _, label := range missingLabels {
			fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Would create label"), label)
		}
		for _, milestone := range missingMilestones {
			fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Would create milestone"), milestone)
		}
		for _, item := range newIssues {
			fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Would create issue"), item.Issue.Title)
		}
		unchanged := 0
		for i := range filteredIssues {
			item := &filteredIssues[i]
			if item.Issue.Number.IsLocal() {
				continue
			}
			original, hasOriginal := readOriginalIssue(p, item.Issue.Number.String())
			localChanged := !hasOriginal || !issue.EqualIgnoringSyncedAt(item.Issue, original)
			if !localChanged {
				unchanged++
				continue
			}
			fmt.Fprintf(a.Out, "%s %s\n", t.MutedText("Would push issue"), t.AccentText("#"+item.Issue.Number.String()))
		}
		for _, comment := range commentsToPost {
			fmt.Fprintf(a.Out, "%s #%s\n", t.MutedText("Would post comment to"), comment.IssueNumber.String())
		}
		if unchanged > 0 {
			noun := "issues"
			if unchanged == 1 {
				noun = "issue"
			}
			fmt.Fprintf(a.Out, "%s\n", t.MutedText(fmt.Sprintf("Nothing to push: %d %s up to date", unchanged, noun)))
		}
		return nil
	}

	// Start progress bar with initial count (labels + milestones + new issues + comments)
	// We'll add pending updates after creating new issues
	progress := newProgressReporter(a.Err, t)
	progress.SetTotal(len(missingLabels) + len(missingMilestones) + len(newIssues) + len(commentsToPost))
	progress.SetPhase("Preparing")
	progress.Start()
	defer progress.Done()

	// Create missing labels
	labelCacheUpdated := false
	for _, label := range missingLabels {
		color := randomLabelColor()
		if err := client.CreateLabel(ctx, label, color); err != nil {
			progress.Log(fmt.Sprintf("%s creating label %q: %v", t.WarningText("Warning:"), label, err))
			progress.Advance()
			continue
		}
		progress.Log(fmt.Sprintf("%s %s", t.SuccessText("Created label"), label))
		labelColors[strings.ToLower(label)] = color
		labelCache.Labels = append(labelCache.Labels, LabelEntry{Name: label, Color: color})
		labelCacheUpdated = true
		progress.Advance()
	}

	// Create missing milestones
	milestoneCacheUpdated := false
	for _, milestone := range missingMilestones {
		if err := client.CreateMilestone(ctx, milestone); err != nil {
			progress.Log(fmt.Sprintf("%s creating milestone %q: %v", t.WarningText("Warning:"), milestone, err))
			progress.Advance()
			continue
		}
		progress.Log(fmt.Sprintf("%s %s", t.SuccessText("Created milestone"), milestone))
		knownMilestones[strings.ToLower(milestone)] = struct{}{}
		milestoneCache.Milestones = append(milestoneCache.Milestones, MilestoneEntry{
			Title: milestone,
			State: "open",
		})
		milestoneCacheUpdated = true
		progress.Advance()
	}

	// Save updated label cache
	if labelCacheUpdated {
		labelCache.SyncedAt = a.Now().UTC()
		if err := saveLabelCache(p, labelCache); err != nil {
			progress.Log(fmt.Sprintf("%s saving label cache: %v", t.WarningText("Warning:"), err))
		}
	}

	// Save updated milestone cache
	if milestoneCacheUpdated {
		milestoneCache.SyncedAt = a.Now().UTC()
		if err := saveMilestoneCache(p, milestoneCache); err != nil {
			progress.Log(fmt.Sprintf("%s saving milestone cache: %v", t.WarningText("Warning:"), err))
		}
	}

	// Create new issues
	progress.SetPhase("Creating issues")
	mapping := map[string]string{}
	createdNumbers := map[string]struct{}{}
	for _, item := range newIssues {
		newNumber, err := client.CreateIssue(ctx, item.Issue)
		if err != nil {
			progress.Done()
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
				progress.Done()
				return err
			}
			item.Path = newPath
		}
		if err := issue.WriteFile(item.Path, item.Issue); err != nil {
			progress.Done()
			return err
		}
		if err := writeOriginalIssue(p, item.Issue); err != nil {
			progress.Done()
			return err
		}
		progress.Log(t.FormatIssueHeader("A", newNumber, item.Issue.Title))
		progress.Advance()
	}

	// Update references in all issues if we created new ones
	if len(mapping) > 0 {
		allIssues, err := loadLocalIssues(p)
		if err != nil {
			progress.Done()
			return err
		}
		for i := range allIssues {
			changed := applyMapping(&allIssues[i].Issue, mapping)
			if changed {
				if err := issue.WriteFile(allIssues[i].Path, allIssues[i].Issue); err != nil {
					progress.Done()
					return err
				}
				progress.Log(fmt.Sprintf("%s %s", t.MutedText("Updated references in"), relPath(a.Root, allIssues[i].Path)))
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
			progress.Done()
			return err
		}

		// Sync relationships and issue type for newly created issues
		for number := range createdNumbers {
			for _, item := range filteredIssues {
				if item.Issue.Number.String() == number {
					if err := client.SyncRelationships(ctx, number, item.Issue); err != nil {
						progress.Log(fmt.Sprintf("%s syncing relationships for #%s: %v",
							t.WarningText("Warning:"), number, err))
					}
					if item.Issue.IssueType != "" {
						if it, ok := knownIssueTypes[strings.ToLower(item.Issue.IssueType)]; ok {
							if err := client.SetIssueType(ctx, number, it.ID); err != nil {
								progress.Log(fmt.Sprintf("%s setting issue type for #%s: %v",
									t.WarningText("Warning:"), number, err))
							}
						} else {
							progress.Log(fmt.Sprintf("%s unknown issue type %q for #%s",
								t.WarningText("Warning:"), item.Issue.IssueType, number))
						}
					}
					if len(item.Issue.Projects) > 0 {
						projectIDs := make(map[string]string)
						for _, proj := range knownProjects {
							projectIDs[strings.ToLower(proj.Title)] = proj.ID
						}
						if err := client.SyncProjects(ctx, number, item.Issue.Projects, projectIDs); err != nil {
							progress.Log(fmt.Sprintf("%s syncing projects for #%s: %v",
								t.WarningText("Warning:"), number, err))
						}
					}
					break
				}
			}
		}
	}

	// Now count issues that need updating (after reference mapping)
	progress.SetPhase("Updating issues")
	type pendingUpdate struct {
		Item        *IssueFile
		Original    issue.Issue
		HasOriginal bool
	}
	var pendingUpdates []pendingUpdate
	var issueNumbersToFetch []string
	unchanged := 0

	for i := range filteredIssues {
		item := &filteredIssues[i]
		if item.Issue.Number.IsLocal() {
			continue
		}
		// Skip issues we just created
		if _, created := createdNumbers[item.Issue.Number.String()]; created {
			continue
		}
		original, hasOriginal := readOriginalIssue(p, item.Issue.Number.String())
		localChanged := !hasOriginal || !issue.EqualIgnoringSyncedAt(item.Issue, original)
		if !localChanged {
			unchanged++
			continue
		}
		pendingUpdates = append(pendingUpdates, pendingUpdate{
			Item:        item,
			Original:    original,
			HasOriginal: hasOriginal,
		})
		issueNumbersToFetch = append(issueNumbersToFetch, item.Issue.Number.String())
	}

	// Update progress total with pending updates count
	progress.SetTotal(progress.Completed() + len(pendingUpdates) + len(commentsToPost))

	// Batch fetch remote issues for conflict detection
	var remoteIssues map[string]issue.Issue
	if len(issueNumbersToFetch) > 0 {
		var err error
		remoteIssues, err = client.GetIssuesBatch(ctx, issueNumbersToFetch)
		if err != nil {
			progress.Done()
			return fmt.Errorf("failed to fetch remote issues: %w", err)
		}
	}

	// Detect conflicts and compute changes
	type conflictInfo struct {
		Number string
		Fields []string
		Local  issue.Issue
		Remote issue.Issue
	}
	var conflicts []conflictInfo
	var batchUpdates []ghcli.BatchIssueUpdate
	type postBatchWork struct {
		Item     *IssueFile
		Original issue.Issue
		Change   ghcli.IssueChange
	}
	var postBatchWorks []postBatchWork
	var autoMerged []string

	conflictCount := 0
	for _, pu := range pendingUpdates {
		numStr := pu.Item.Issue.Number.String()
		remote, ok := remoteIssues[numStr]
		if !ok {
			progress.Log(fmt.Sprintf("%s issue #%s not found on remote", t.WarningText("Warning:"), numStr))
			conflictCount++
			continue
		}

		if !opts.Force && pu.HasOriginal && !issue.EqualForConflictCheck(remote, pu.Original) {
			// Remote changed since last sync - try three-way merge
			mergeResult := issue.ThreeWayMerge(pu.Original, pu.Item.Issue, remote)

			if !mergeResult.OK {
				// Real conflict - fields overlap
				conflicts = append(conflicts, conflictInfo{
					Number: numStr,
					Fields: mergeResult.ConflictingFields.Fields(),
					Local:  pu.Item.Issue,
					Remote: remote,
				})
				conflictCount++
				continue
			}

			if mergeResult.LocalChanges.IsEmpty() {
				// No local changes - just update original to match remote
				if err := writeOriginalIssue(p, remote); err != nil {
					progress.Log(fmt.Sprintf("%s updating original for #%s: %v", t.WarningText("Warning:"), numStr, err))
				}
				// Update local file with remote changes
				remote.SyncedAt = ptrTime(a.Now().UTC())
				if err := issue.WriteFile(pu.Item.Path, remote); err != nil {
					progress.Log(fmt.Sprintf("%s updating local file for #%s: %v", t.WarningText("Warning:"), numStr, err))
				}
				unchanged++
				continue
			}

			// Auto-merge succeeded - use merged issue
			pu.Item.Issue = mergeResult.Merged
			autoMerged = append(autoMerged, numStr)
		}

		// Use remote as baseline if no original exists (for state transitions)
		baseline := pu.Original
		if !pu.HasOriginal {
			baseline = remote
		}
		change := diffIssue(baseline, pu.Item.Issue)

		// Handle state transitions immediately (can't be batched)
		if change.StateTransition != nil {
			if *change.StateTransition == "close" {
				reason := ""
				if change.StateReason != nil {
					reason = *change.StateReason
				}
				if err := client.CloseIssue(ctx, numStr, reason); err != nil {
					progress.Done()
					return err
				}
			} else if *change.StateTransition == "reopen" {
				if err := client.ReopenIssue(ctx, numStr); err != nil {
					progress.Done()
					return err
				}
			}
		}

		// Build batch update for basic fields
		if hasEdits(change) {
			update := ghcli.BatchIssueUpdate{Number: numStr}
			if change.Title != nil {
				update.Title = change.Title
			}
			if change.Body != nil {
				update.Body = change.Body
			}
			if change.Milestone != nil {
				update.Milestone = change.Milestone
			}
			if len(change.AddLabels) > 0 || len(change.RemoveLabels) > 0 {
				if pu.Item.Issue.Labels == nil {
					update.Labels = []string{}
				} else {
					update.Labels = pu.Item.Issue.Labels
				}
			}
			if len(change.AddAssignees) > 0 || len(change.RemoveAssignees) > 0 {
				if pu.Item.Issue.Assignees == nil {
					update.Assignees = []string{}
				} else {
					update.Assignees = pu.Item.Issue.Assignees
				}
			}
			batchUpdates = append(batchUpdates, update)
		}

		postBatchWorks = append(postBatchWorks, postBatchWork{
			Item:     pu.Item,
			Original: pu.Original,
			Change:   change,
		})
	}

	// Execute batch update
	if len(batchUpdates) > 0 {
		result, err := client.BatchEditIssues(ctx, batchUpdates)
		if err != nil {
			progress.Done()
			return fmt.Errorf("batch update failed: %w", err)
		}
		for num, errMsg := range result.Errors {
			progress.Log(fmt.Sprintf("%s updating #%s: %s", t.WarningText("Warning:"), num, errMsg))
		}
	}

	// Handle post-batch work and finalize
	for _, work := range postBatchWorks {
		numStr := work.Item.Issue.Number.String()

		// Sync issue type via GraphQL (if changed)
		if work.Change.IssueType != nil {
			issueTypeID := ""
			if *work.Change.IssueType != "" {
				if it, ok := knownIssueTypes[strings.ToLower(*work.Change.IssueType)]; ok {
					issueTypeID = it.ID
				} else {
					progress.Log(fmt.Sprintf("%s unknown issue type %q for #%s",
						t.WarningText("Warning:"), *work.Change.IssueType, numStr))
				}
			}
			if issueTypeID != "" || *work.Change.IssueType == "" {
				if err := client.SetIssueType(ctx, numStr, issueTypeID); err != nil {
					progress.Log(fmt.Sprintf("%s setting issue type for #%s: %v",
						t.WarningText("Warning:"), numStr, err))
				}
			}
		}

		// Sync parent and blocking relationships via GraphQL
		if err := client.SyncRelationships(ctx, numStr, work.Item.Issue); err != nil {
			progress.Log(fmt.Sprintf("%s syncing relationships for #%s: %v",
				t.WarningText("Warning:"), numStr, err))
		}

		// Sync projects via GraphQL (if changed)
		if len(work.Change.AddProjects) > 0 || len(work.Change.RemoveProjects) > 0 {
			projectIDs := make(map[string]string)
			for _, proj := range knownProjects {
				projectIDs[strings.ToLower(proj.Title)] = proj.ID
			}
			if err := client.SyncProjects(ctx, numStr, work.Item.Issue.Projects, projectIDs); err != nil {
				progress.Log(fmt.Sprintf("%s syncing projects for #%s: %v",
					t.WarningText("Warning:"), numStr, err))
			}
		}

		work.Item.Issue.SyncedAt = ptrTime(a.Now().UTC())
		if err := issue.WriteFile(work.Item.Path, work.Item.Issue); err != nil {
			progress.Done()
			return err
		}
		if err := writeOriginalIssue(p, work.Item.Issue); err != nil {
			progress.Done()
			return err
		}
		progress.Log(t.FormatIssueHeader("U", numStr, work.Item.Issue.Title))
		for _, line := range a.formatChangeLines(work.Original, work.Item.Issue, labelColors) {
			progress.Log(line)
		}
		progress.Advance()
	}

	// Advance for conflicts (they were counted but not processed)
	for i := 0; i < conflictCount; i++ {
		progress.Advance()
	}

	// Post comments
	progress.SetPhase("Posting comments")
	conflictSet := make(map[string]struct{})
	for _, c := range conflicts {
		conflictSet[c.Number] = struct{}{}
	}

	for _, comment := range commentsToPost {
		numStr := comment.IssueNumber.String()

		// Skip local issues (can't post comments to issues that don't exist yet)
		if comment.IssueNumber.IsLocal() {
			if realNum, ok := mapping[numStr]; ok {
				comment.IssueNumber = issue.IssueNumber(realNum)
				numStr = realNum
			} else {
				progress.Advance()
				continue
			}
		}

		// Skip issues that had conflicts
		if _, isConflict := conflictSet[numStr]; isConflict {
			progress.Advance()
			continue
		}

		if err := client.CreateComment(ctx, numStr, comment.Body); err != nil {
			progress.Log(fmt.Sprintf("%s posting comment to #%s: %v", t.WarningText("Warning:"), numStr, err))
			progress.Advance()
			continue
		}

		if err := deletePendingComment(comment); err != nil {
			progress.Log(fmt.Sprintf("%s removing comment file %s: %v", t.WarningText("Warning:"), relPath(a.Root, comment.Path), err))
		}

		progress.Log(fmt.Sprintf("%s #%s", t.SuccessText("Posted comment to"), numStr))
		progress.Advance()
	}

	// Done with progress bar
	progress.Done()

	// Print final messages
	if len(autoMerged) > 0 {
		sort.Strings(autoMerged)
		fmt.Fprintf(a.Out, "%s %s\n", t.SuccessText("Auto-merged (no conflicts):"), strings.Join(autoMerged, ", "))
	}
	if len(conflicts) > 0 {
		sort.Slice(conflicts, func(i, j int) bool {
			return conflicts[i].Number < conflicts[j].Number
		})
		fmt.Fprintf(a.Err, "%s\n", t.WarningText("Conflicts (remote changed, skipped):"))
		for _, c := range conflicts {
			fmt.Fprintf(a.Err, "  %s %s\n", t.AccentText("#"+c.Number), t.MutedText("("+strings.Join(c.Fields, ", ")+")"))
			for _, line := range a.formatConflictLines(c.Local, c.Remote, c.Fields, labelColors) {
				fmt.Fprintf(a.Err, "%s\n", line)
			}
			// Show body diff if body is conflicting
			for _, f := range c.Fields {
				if f == "body" {
					fmt.Fprintf(a.Err, "      %s\n", t.MutedText("local -> remote:"))
					a.printWordDiff(c.Local.Body, c.Remote.Body)
					break
				}
			}
		}
	}
	if unchanged > 0 {
		noun := "issues"
		if unchanged == 1 {
			noun = "issue"
		}
		fmt.Fprintf(a.Out, "%s\n", t.MutedText(fmt.Sprintf("Nothing to push: %d %s up to date", unchanged, noun)))
	}

	return nil
}
