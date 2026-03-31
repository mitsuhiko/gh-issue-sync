package ghcli

import (
	"context"
	"reflect"
	"testing"
)

type recordingRunner struct {
	name string
	args []string
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return "[]", nil
}

func TestClientAddsRepoFlag(t *testing.T) {
	runner := &recordingRunner{}
	client := NewClient(runner, "octo/repo")

	if _, err := client.ListIssues(context.Background(), "open", nil); err != nil {
		t.Fatalf("list issues: %v", err)
	}

	if runner.name != "gh" {
		t.Fatalf("expected gh invocation, got %q", runner.name)
	}
	if !hasRepoFlag(runner.args, "octo/repo") {
		t.Fatalf("expected --repo octo/repo, got %v", runner.args)
	}
}

func hasRepoFlag(args []string, repo string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--repo" && args[i+1] == repo {
			return true
		}
	}
	return false
}

func TestCloseIssueReasonNormalization(t *testing.T) {
	tests := []struct {
		name     string
		reason   string
		expected []string
	}{
		{
			name:     "not_planned converts to gh format",
			reason:   "not_planned",
			expected: []string{"api", "repos/octo/repo/issues/929", "--method", "PATCH", "-f", "state=closed", "-f", "state_reason=not_planned"},
		},
		{
			name:     "completed reason",
			reason:   "completed",
			expected: []string{"api", "repos/octo/repo/issues/929", "--method", "PATCH", "-f", "state=closed", "-f", "state_reason=completed"},
		},
		{
			name:     "empty reason omits flag",
			reason:   "",
			expected: []string{"api", "repos/octo/repo/issues/929", "--method", "PATCH", "-f", "state=closed"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{}
			client := NewClient(runner, "octo/repo")

			if err := client.CloseIssue(context.Background(), "929", tc.reason); err != nil {
				t.Fatalf("close issue: %v", err)
			}

			if runner.name != "gh" {
				t.Fatalf("expected gh invocation, got %q", runner.name)
			}

			if !reflect.DeepEqual(runner.args, tc.expected) {
				t.Fatalf("unexpected args\n got: %#v\nwant: %#v", runner.args, tc.expected)
			}
		})
	}
}

func TestCloseIssueRejectsNonCanonicalReason(t *testing.T) {
	runner := &recordingRunner{}
	client := NewClient(runner, "octo/repo")

	err := client.CloseIssue(context.Background(), "929", "not planned")
	if err == nil {
		t.Fatalf("expected error")
	}
}
