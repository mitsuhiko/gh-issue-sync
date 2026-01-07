package issue

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type IssueNumber string

type IssueRef string

type Issue struct {
	Number      IssueNumber
	Title       string
	Labels      []string
	Assignees   []string
	Milestone   string
	IssueType   string
	Projects    []string
	State       string
	StateReason *string
	Parent      *IssueRef
	BlockedBy   []IssueRef
	Blocks      []IssueRef
	SyncedAt    *time.Time
	Body        string

	// Informational fields (read-only, not synced back to GitHub)
	Author    string
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

// InfoSection contains read-only informational fields that are synced from
// GitHub but never written back. These are for display/filtering only.
type InfoSection struct {
	Author    string     `yaml:"author,omitempty"`
	CreatedAt *time.Time `yaml:"created_at,omitempty"`
	UpdatedAt *time.Time `yaml:"updated_at,omitempty"`
}

type FrontMatter struct {
	Title       string       `yaml:"title"`
	Labels      []string     `yaml:"labels,omitempty"`
	Assignees   []string     `yaml:"assignees,omitempty"`
	Milestone   string       `yaml:"milestone,omitempty"`
	IssueType   string       `yaml:"type,omitempty"`
	Projects    []string     `yaml:"projects,omitempty"`
	State       string       `yaml:"state,omitempty"`
	StateReason *string      `yaml:"state_reason"`
	Parent      *IssueRef    `yaml:"parent,omitempty"`
	BlockedBy   []IssueRef   `yaml:"blocked_by,omitempty"`
	Blocks      []IssueRef   `yaml:"blocks,omitempty"`
	SyncedAt    *time.Time   `yaml:"synced_at,omitempty"`
	Info        *InfoSection `yaml:"info,omitempty"`
}

func (n IssueNumber) String() string {
	return string(n)
}

func (n IssueNumber) IsLocal() bool {
	return strings.HasPrefix(string(n), "T")
}

func (r IssueRef) String() string {
	return string(r)
}

func (r IssueRef) IsLocal() bool {
	return strings.HasPrefix(string(r), "T")
}

func (r *IssueRef) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("issue reference must be scalar")
	}
	*r = IssueRef(value.Value)
	return nil
}

func (r IssueRef) MarshalYAML() (interface{}, error) {
	if r == "" {
		return nil, nil
	}
	if r.IsLocal() {
		return string(r), nil
	}
	parsed, err := strconv.Atoi(string(r))
	if err != nil {
		return string(r), nil
	}
	return parsed, nil
}

var frontMatterDelimiter = []byte("---")

// numberFromFilename extracts the issue number from a filename like "42-title.md" or "T5-title.md"
// Also handles simple filenames like "42.md" (used for originals)
func numberFromFilename(path string) IssueNumber {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".md")
	idx := strings.Index(base, "-")
	if idx == -1 {
		// No dash - entire base is the number (e.g., "42.md")
		return IssueNumber(base)
	}
	return IssueNumber(base[:idx])
}

func ParseFile(path string) (Issue, error) {
	data, err := osReadFile(path)
	if err != nil {
		return Issue{}, err
	}
	issue, err := Parse(data)
	if err != nil {
		return Issue{}, err
	}
	issue.Number = numberFromFilename(path)
	return issue, nil
}

func Parse(data []byte) (Issue, error) {
	frontMatter, body, err := splitFrontMatter(data)
	if err != nil {
		return Issue{}, err
	}
	var fm FrontMatter
	if err := yaml.Unmarshal(frontMatter, &fm); err != nil {
		return Issue{}, err
	}
	issue := Issue{
		Title:       fm.Title,
		Labels:      fm.Labels,
		Assignees:   fm.Assignees,
		Milestone:   fm.Milestone,
		IssueType:   fm.IssueType,
		Projects:    fm.Projects,
		State:       fm.State,
		StateReason: fm.StateReason,
		Parent:      fm.Parent,
		BlockedBy:   fm.BlockedBy,
		Blocks:      fm.Blocks,
		SyncedAt:    fm.SyncedAt,
		Body:        normalizeBody(string(body)),
	}
	if fm.Info != nil {
		issue.Author = fm.Info.Author
		issue.CreatedAt = fm.Info.CreatedAt
		issue.UpdatedAt = fm.Info.UpdatedAt
	}
	return issue, nil
}

func Render(issue Issue) (string, error) {
	fm := FrontMatter{
		Title:       issue.Title,
		Labels:      sortedStrings(issue.Labels),
		Assignees:   sortedStrings(issue.Assignees),
		Milestone:   issue.Milestone,
		IssueType:   issue.IssueType,
		Projects:    sortedStrings(issue.Projects),
		State:       issue.State,
		StateReason: issue.StateReason,
		Parent:      issue.Parent,
		BlockedBy:   sortedRefs(issue.BlockedBy),
		Blocks:      sortedRefs(issue.Blocks),
		SyncedAt:    issue.SyncedAt,
	}
	if issue.Author != "" || issue.CreatedAt != nil || issue.UpdatedAt != nil {
		fm.Info = &InfoSection{
			Author:    issue.Author,
			CreatedAt: issue.CreatedAt,
			UpdatedAt: issue.UpdatedAt,
		}
	}
	payload, err := yaml.Marshal(&fm)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	buf.Write(frontMatterDelimiter)
	buf.WriteByte('\n')
	buf.Write(payload)
	buf.Write(frontMatterDelimiter)
	buf.WriteByte('\n')
	buf.WriteByte('\n')
	buf.WriteString(normalizeBody(issue.Body))
	return buf.String(), nil
}

func WriteFile(path string, issue Issue) error {
	content, err := Render(issue)
	if err != nil {
		return err
	}
	return osWriteFile(path, []byte(content), 0o644)
}

func FileName(number IssueNumber, title string) string {
	slug := Slugify(title)
	if slug == "" {
		slug = "issue"
	}
	return fmt.Sprintf("%s-%s.md", number, slug)
}

func PathFor(dir string, number IssueNumber, title string) string {
	return filepath.Join(dir, FileName(number, title))
}

func Normalize(issue Issue) Issue {
	issue.Labels = sortedStrings(issue.Labels)
	issue.Assignees = sortedStrings(issue.Assignees)
	issue.Projects = sortedStrings(issue.Projects)
	issue.BlockedBy = sortedRefs(issue.BlockedBy)
	issue.Blocks = sortedRefs(issue.Blocks)
	issue.Body = normalizeBody(issue.Body)
	return issue
}

func EqualIgnoringSyncedAt(a, b Issue) bool {
	return equalIssues(a, b, false)
}

// EqualForConflictCheck compares issues ignoring SyncedAt and StateReason.
// StateReason is ignored because GitHub can set it automatically when closing
// issues, which would cause false conflicts.
func EqualForConflictCheck(a, b Issue) bool {
	return equalIssues(a, b, true)
}

func equalIssues(a, b Issue, ignoreStateReason bool) bool {
	a = Normalize(a)
	b = Normalize(b)
	a.SyncedAt = nil
	b.SyncedAt = nil

	if a.Number != b.Number {
		return false
	}
	if a.Title != b.Title {
		return false
	}
	if !stringSlicesEqual(a.Labels, b.Labels) {
		return false
	}
	if !stringSlicesEqual(a.Assignees, b.Assignees) {
		return false
	}
	if a.Milestone != b.Milestone {
		return false
	}
	if a.IssueType != b.IssueType {
		return false
	}
	if !stringSlicesEqual(a.Projects, b.Projects) {
		return false
	}
	if a.State != b.State {
		return false
	}
	if !ignoreStateReason && normalizeOptional(a.StateReason) != normalizeOptional(b.StateReason) {
		return false
	}
	if normalizeOptionalRef(a.Parent) != normalizeOptionalRef(b.Parent) {
		return false
	}
	if !refSlicesEqual(a.BlockedBy, b.BlockedBy) {
		return false
	}
	if !refSlicesEqual(a.Blocks, b.Blocks) {
		return false
	}
	if a.Body != b.Body {
		return false
	}
	return true
}

func normalizeOptional(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func normalizeOptionalRef(value *IssueRef) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.TrimLeft(body, "\n")
	if body == "" {
		return ""
	}
	if !strings.HasSuffix(body, "\n") {
		return body + "\n"
	}
	return body
}

func sortedStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	cleaned := make([]string, 0, len(items))
	seen := make(map[string]struct{})
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		cleaned = append(cleaned, item)
	}
	sort.Strings(cleaned)
	return cleaned
}

func sortedRefs(items []IssueRef) []IssueRef {
	if len(items) == 0 {
		return nil
	}
	cleaned := make([]IssueRef, 0, len(items))
	seen := make(map[string]struct{})
	for _, item := range items {
		key := strings.TrimSpace(item.String())
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		cleaned = append(cleaned, IssueRef(key))
	}
	sort.Slice(cleaned, func(i, j int) bool {
		return cleaned[i].String() < cleaned[j].String()
	})
	return cleaned
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

func refSlicesEqual(a, b []IssueRef) bool {
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

func splitFrontMatter(data []byte) ([]byte, []byte, error) {
	if bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		data = data[3:]
	}
	if !bytes.HasPrefix(data, append(frontMatterDelimiter, '\n')) {
		return nil, nil, errors.New("missing front matter")
	}
	lines := bytes.Split(data, []byte("\n"))
	end := -1
	for i := 1; i < len(lines); i++ {
		if bytes.Equal(lines[i], frontMatterDelimiter) {
			end = i
			break
		}
	}
	if end == -1 {
		return nil, nil, errors.New("unterminated front matter")
	}
	front := bytes.Join(lines[1:end], []byte("\n"))
	body := bytes.Join(lines[end+1:], []byte("\n"))
	body = bytes.TrimPrefix(body, []byte("\n"))
	return front, body, nil
}

var slugRegex = regexp.MustCompile(`[^a-z0-9]+`)

func Slugify(title string) string {
	lower := strings.ToLower(strings.TrimSpace(title))
	if lower == "" {
		return ""
	}
	slug := slugRegex.ReplaceAllString(lower, "-")
	slug = strings.Trim(slug, "-")
	slug = strings.Trim(slug, ".")
	slug = strings.ReplaceAll(slug, "--", "-")
	return slug
}

// osReadFile and osWriteFile are swapped out in tests.
var osReadFile = func(path string) ([]byte, error) {
	return os.ReadFile(path)
}

var osWriteFile = func(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

// FieldSet tracks which fields have been modified.
type FieldSet struct {
	Title     bool
	Labels    bool
	Assignees bool
	Milestone bool
	IssueType bool
	Projects  bool
	State     bool
	Parent    bool
	BlockedBy bool
	Blocks    bool
	Body      bool
}

// Fields returns a list of field names that are set.
func (f FieldSet) Fields() []string {
	var fields []string
	if f.Title {
		fields = append(fields, "title")
	}
	if f.Labels {
		fields = append(fields, "labels")
	}
	if f.Assignees {
		fields = append(fields, "assignees")
	}
	if f.Milestone {
		fields = append(fields, "milestone")
	}
	if f.IssueType {
		fields = append(fields, "issue_type")
	}
	if f.Projects {
		fields = append(fields, "projects")
	}
	if f.State {
		fields = append(fields, "state")
	}
	if f.Parent {
		fields = append(fields, "parent")
	}
	if f.BlockedBy {
		fields = append(fields, "blocked_by")
	}
	if f.Blocks {
		fields = append(fields, "blocks")
	}
	if f.Body {
		fields = append(fields, "body")
	}
	return fields
}

// IsEmpty returns true if no fields are set.
func (f FieldSet) IsEmpty() bool {
	return !f.Title && !f.Labels && !f.Assignees && !f.Milestone &&
		!f.IssueType && !f.Projects && !f.State && !f.Parent &&
		!f.BlockedBy && !f.Blocks && !f.Body
}

// Overlaps returns a FieldSet containing fields that are set in both.
func (f FieldSet) Overlaps(other FieldSet) FieldSet {
	return FieldSet{
		Title:     f.Title && other.Title,
		Labels:    f.Labels && other.Labels,
		Assignees: f.Assignees && other.Assignees,
		Milestone: f.Milestone && other.Milestone,
		IssueType: f.IssueType && other.IssueType,
		Projects:  f.Projects && other.Projects,
		State:     f.State && other.State,
		Parent:    f.Parent && other.Parent,
		BlockedBy: f.BlockedBy && other.BlockedBy,
		Blocks:    f.Blocks && other.Blocks,
		Body:      f.Body && other.Body,
	}
}

// ComputeChanges returns which fields differ between base and changed.
func ComputeChanges(base, changed Issue) FieldSet {
	base = Normalize(base)
	changed = Normalize(changed)

	return FieldSet{
		Title:     base.Title != changed.Title,
		Labels:    !stringSlicesEqual(base.Labels, changed.Labels),
		Assignees: !stringSlicesEqual(base.Assignees, changed.Assignees),
		Milestone: base.Milestone != changed.Milestone,
		IssueType: base.IssueType != changed.IssueType,
		Projects:  !stringSlicesEqual(base.Projects, changed.Projects),
		State:     base.State != changed.State,
		Parent:    normalizeOptionalRef(base.Parent) != normalizeOptionalRef(changed.Parent),
		BlockedBy: !refSlicesEqual(base.BlockedBy, changed.BlockedBy),
		Blocks:    !refSlicesEqual(base.Blocks, changed.Blocks),
		Body:      base.Body != changed.Body,
	}
}

// MergeResult represents the outcome of a three-way merge.
type MergeResult struct {
	// Merged contains the merged issue (only valid if OK is true).
	Merged Issue
	// OK is true if the merge succeeded without conflicts.
	OK bool
	// ConflictingFields lists the fields that conflict (only if OK is false).
	ConflictingFields FieldSet
	// LocalChanges lists fields changed locally.
	LocalChanges FieldSet
	// RemoteChanges lists fields changed remotely.
	RemoteChanges FieldSet
}

// ThreeWayMerge attempts to merge local and remote changes against a common base.
// If changes don't overlap, it returns a merged issue. Otherwise, it returns
// information about which fields conflict.
func ThreeWayMerge(base, local, remote Issue) MergeResult {
	localChanges := ComputeChanges(base, local)
	remoteChanges := ComputeChanges(base, remote)
	conflicts := localChanges.Overlaps(remoteChanges)

	result := MergeResult{
		LocalChanges:  localChanges,
		RemoteChanges: remoteChanges,
	}

	if !conflicts.IsEmpty() {
		result.ConflictingFields = conflicts
		return result
	}

	// No conflicts - merge by starting with remote and applying local changes
	merged := Normalize(remote)

	if localChanges.Title {
		merged.Title = local.Title
	}
	if localChanges.Labels {
		merged.Labels = local.Labels
	}
	if localChanges.Assignees {
		merged.Assignees = local.Assignees
	}
	if localChanges.Milestone {
		merged.Milestone = local.Milestone
	}
	if localChanges.IssueType {
		merged.IssueType = local.IssueType
	}
	if localChanges.Projects {
		merged.Projects = local.Projects
	}
	if localChanges.State {
		merged.State = local.State
	}
	if localChanges.Parent {
		merged.Parent = local.Parent
	}
	if localChanges.BlockedBy {
		merged.BlockedBy = local.BlockedBy
	}
	if localChanges.Blocks {
		merged.Blocks = local.Blocks
	}
	if localChanges.Body {
		merged.Body = local.Body
	}

	result.Merged = merged
	result.OK = true
	return result
}
