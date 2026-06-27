package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
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
)

// localRefPattern matches local issue references like #T1, #T42, #Tabc123 (T followed by alphanumerics)
var localRefPattern = regexp.MustCompile(`#(T[a-zA-Z0-9]+)`)

// diffIssue computes the change for a local vs original issue.
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
	change.AddProjects, change.RemoveProjects = diffStringSet(original.Projects, local.Projects)
	if original.Milestone != local.Milestone {
		milestone := local.Milestone
		change.Milestone = &milestone
	}
	if original.IssueType != local.IssueType {
		issueType := local.IssueType
		change.IssueType = &issueType
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
	// Note: IssueType is not included here because it's handled via GraphQL separately
	return change.Title != nil || change.Body != nil || change.Milestone != nil || len(change.AddLabels) > 0 || len(change.RemoveLabels) > 0 || len(change.AddAssignees) > 0 || len(change.RemoveAssignees) > 0
}

func neededMilestonesForPush(p paths.Paths, issues []IssueFile) map[string]struct{} {
	needed := make(map[string]struct{})
	for i := range issues {
		item := &issues[i]
		if item.Issue.Milestone == "" {
			continue
		}
		if item.Issue.Number.IsLocal() {
			needed[item.Issue.Milestone] = struct{}{}
			continue
		}
		original, hasOriginal := readOriginalIssue(p, item.Issue.Number.String())
		if !hasOriginal || original.Milestone != item.Issue.Milestone {
			needed[item.Issue.Milestone] = struct{}{}
		}
	}
	return needed
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

func dirForState(p paths.Paths, state string) string {
	if state == "closed" {
		return p.ClosedDir
	}
	return p.OpenDir
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

func normalizeOptional(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func ptrTime(t time.Time) *time.Time {
	return &t
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
