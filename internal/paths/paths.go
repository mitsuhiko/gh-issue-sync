package paths

import (
	"os"
	"path/filepath"
)

const (
	IssuesDirName      = ".issues"
	SyncDirName        = ".sync"
	OriginalsDirName   = "originals"
	OpenDirName        = "open"
	ClosedDirName      = "closed"
	ConfigFileName     = "config.json"
	LabelsFileName     = "labels.json"
	MilestonesFileName = "milestones.json"
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
