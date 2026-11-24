package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIssueDirectoryOverride(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	binary := filepath.Join(t.TempDir(), "gh-issue-sync")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}

	for _, tc := range []struct {
		name     string
		absolute bool
		existing bool
	}{
		{name: "absolute existing", absolute: true, existing: true},
		{name: "absolute missing", absolute: true},
		{name: "relative existing", existing: true},
		{name: "relative missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".git"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "work"), 0755); err != nil {
				t.Fatal(err)
			}
			issuesDir := filepath.Join(root, "private", "issues")
			if tc.existing {
				if err := os.MkdirAll(issuesDir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			override := filepath.Join("..", "private", "issues")
			if tc.absolute {
				override = issuesDir
			}
			t.Setenv("GH_ISSUE_SYNC_DIR", override)
			run := func(args ...string) string {
				t.Helper()
				cmd := exec.Command(binary, args...)
				cmd.Dir = filepath.Join(root, "work")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Errorf("%v: %v\n%s", args, err, out)
				}
				return string(out)
			}
			run("init", "--owner", "example", "--repo", "repro")
			if _, err := os.Stat(filepath.Join(issuesDir, ".sync", "config.json")); err != nil {
				t.Errorf("expected config in override directory: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, ".issues")); !os.IsNotExist(err) {
				t.Errorf("expected default .issues directory to remain absent, got %v", err)
			}
			if out := run("status"); !strings.Contains(out, "Repository: example/repro") {
				t.Errorf("status did not read the initialized repository:\n%s", out)
			}
		})
	}
}
