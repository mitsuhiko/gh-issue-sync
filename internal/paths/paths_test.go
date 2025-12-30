package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindIssuesDir(t *testing.T) {
	// Create temp directory structure
	tmp := t.TempDir()
	gitRoot := filepath.Join(tmp, "project")
	subDir := filepath.Join(gitRoot, "src", "pkg")
	issuesDir := filepath.Join(gitRoot, IssuesDirName)

	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitRoot, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(issuesDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Should find .issues from subdirectory
	found := FindIssuesDir(subDir)
	if found != gitRoot {
		t.Errorf("FindIssuesDir(%s) = %q, want %q", subDir, found, gitRoot)
	}

	// Should find .issues from root
	found = FindIssuesDir(gitRoot)
	if found != gitRoot {
		t.Errorf("FindIssuesDir(%s) = %q, want %q", gitRoot, found, gitRoot)
	}
}

func TestFindIssuesDirStopsAtGitRoot(t *testing.T) {
	tmp := t.TempDir()
	gitRoot := filepath.Join(tmp, "project")
	subDir := filepath.Join(gitRoot, "src")

	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitRoot, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	// No .issues directory

	found := FindIssuesDir(subDir)
	if found != "" {
		t.Errorf("FindIssuesDir(%s) = %q, want empty (should stop at git root)", subDir, found)
	}
}

func TestFindIssuesDirEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	customIssues := filepath.Join(tmp, "custom-issues")
	if err := os.MkdirAll(customIssues, 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvIssuesDir, customIssues)

	found := FindIssuesDir("/some/random/dir")
	if found != tmp {
		t.Errorf("FindIssuesDir with env = %q, want %q", found, tmp)
	}
}

func TestFindGitRoot(t *testing.T) {
	tmp := t.TempDir()
	gitRoot := filepath.Join(tmp, "project")
	subDir := filepath.Join(gitRoot, "src", "pkg")

	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitRoot, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	found := FindGitRoot(subDir)
	if found != gitRoot {
		t.Errorf("FindGitRoot(%s) = %q, want %q", subDir, found, gitRoot)
	}

	// No git root
	found = FindGitRoot(tmp)
	if found != "" {
		t.Errorf("FindGitRoot(%s) = %q, want empty", tmp, found)
	}
}
