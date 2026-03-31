package ghcli

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type scopeFallbackRunner struct {
	calls   int
	queries []string
}

func (r *scopeFallbackRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.calls++
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-f" && strings.HasPrefix(args[i+1], "query=") {
			r.queries = append(r.queries, strings.TrimPrefix(args[i+1], "query="))
			break
		}
	}

	if r.calls == 1 {
		return "", fmt.Errorf("GraphQL error: INSUFFICIENT_SCOPES")
	}

	return `{"data":{"repository":{"issue0":{"id":"I_123","number":281,"parent":null,"blockedBy":{"nodes":[]},"blocking":{"nodes":[]}}}}}`, nil
}

func TestGetIssueRelationshipsBatchFallsBackWithoutProjectsOnScopeError(t *testing.T) {
	runner := &scopeFallbackRunner{}
	client := NewClient(runner, "octo/repo")

	rels, err := client.GetIssueRelationshipsBatch(context.Background(), []string{"281"})
	if err != nil {
		t.Fatalf("GetIssueRelationshipsBatch failed: %v", err)
	}

	if runner.calls != 2 {
		t.Fatalf("expected 2 calls (retry without projectItems), got %d", runner.calls)
	}
	if len(runner.queries) != 2 {
		t.Fatalf("expected 2 captured queries, got %d", len(runner.queries))
	}
	if !strings.Contains(runner.queries[0], "projectItems(first: 20)") {
		t.Fatalf("expected first query to include projectItems")
	}
	if strings.Contains(runner.queries[1], "projectItems(first: 20)") {
		t.Fatalf("expected second query to omit projectItems")
	}
	if _, ok := rels["281"]; !ok {
		t.Fatalf("expected relationships for issue 281")
	}
}
