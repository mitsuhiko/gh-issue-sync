package paths

import (
	"os"
	"path/filepath"
)

const EnvIssuesDir = "GH_ISSUE_SYNC_DIR"

const (
	IssuesDirName      = ".issues"
	SyncDirName        = ".sync"
	OriginalsDirName   = "originals"
	OpenDirName        = "open"
	ClosedDirName      = "closed"
	ConfigFileName     = "config.json"
	LabelsFileName     = "labels.json"
	MilestonesFileName = "milestones.json"
	IssueTypesFileName = "issue_types.json"
	ProjectsFileName   = "projects.json"
)

type Paths struct {
	Root           string
	IssuesDir      string
	SyncDir        string
	OriginalsDir   string
	OpenDir        string
	ClosedDir      string
	ConfigPath     string
	LabelsPath     string
	MilestonesPath string
	IssueTypesPath string
	ProjectsPath   string
}

func New(root string) Paths {
	issuesDir := filepath.Join(root, IssuesDirName)
	syncDir := filepath.Join(issuesDir, SyncDirName)
	originalsDir := filepath.Join(syncDir, OriginalsDirName)
	openDir := filepath.Join(issuesDir, OpenDirName)
	closedDir := filepath.Join(issuesDir, ClosedDirName)
	configPath := filepath.Join(syncDir, ConfigFileName)
	labelsPath := filepath.Join(syncDir, LabelsFileName)
	milestonesPath := filepath.Join(syncDir, MilestonesFileName)
	issueTypesPath := filepath.Join(syncDir, IssueTypesFileName)

	projectsPath := filepath.Join(syncDir, ProjectsFileName)

	return Paths{
		Root:           root,
		IssuesDir:      issuesDir,
		SyncDir:        syncDir,
		OriginalsDir:   originalsDir,
		OpenDir:        openDir,
		ClosedDir:      closedDir,
		ConfigPath:     configPath,
		LabelsPath:     labelsPath,
		MilestonesPath: milestonesPath,
		IssueTypesPath: issueTypesPath,
		ProjectsPath:   projectsPath,
	}
}

func (p Paths) EnsureLayout() error {
	for _, dir := range []string{p.IssuesDir, p.SyncDir, p.OriginalsDir, p.OpenDir, p.ClosedDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// FindIssuesDir searches for an existing .issues directory.
// It first checks the GH_ISSUE_SYNC_DIR environment variable,
// then walks upward from startDir until it finds .issues or hits a .git root.
// Returns the directory containing .issues (not .issues itself), or empty string if not found.
func FindIssuesDir(startDir string) string {
	// Check environment variable first
	if envDir := os.Getenv(EnvIssuesDir); envDir != "" {
		if !filepath.IsAbs(envDir) {
			envDir = filepath.Join(startDir, envDir)
		}
		// The env var points to the .issues directory itself
		if info, err := os.Stat(envDir); err == nil && info.IsDir() {
			return filepath.Dir(envDir)
		}
		return ""
	}

	// Walk upward looking for .issues
	dir := startDir
	for {
		issuesPath := filepath.Join(dir, IssuesDirName)
		if info, err := os.Stat(issuesPath); err == nil && info.IsDir() {
			return dir
		}

		// Stop at git root
		gitPath := filepath.Join(dir, ".git")
		if _, err := os.Stat(gitPath); err == nil {
			return ""
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Hit filesystem root
			return ""
		}
		dir = parent
	}
}

// FindGitRoot walks upward from startDir to find the directory containing .git.
// Returns empty string if not found.
func FindGitRoot(startDir string) string {
	dir := startDir
	for {
		gitPath := filepath.Join(dir, ".git")
		if _, err := os.Stat(gitPath); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
