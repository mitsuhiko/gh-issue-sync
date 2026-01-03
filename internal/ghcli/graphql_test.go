package ghcli

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type queuedResponseRunner struct {
	calls     []recordedCall
	responses []string
}

type recordedCall struct {
	name string
	args []string
}

func (r *queuedResponseRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, recordedCall{
		name: name,
		args: append([]string(nil), args...),
	})
	if len(r.responses) == 0 {
		return "", fmt.Errorf("unexpected command: %s %v", name, args)
	}
	out := r.responses[0]
	r.responses = r.responses[1:]
	return out, nil
}

func TestBatchEditIssuesEscapesBodyOnce(t *testing.T) {
	lookupResponse := `{
  "data": {
    "repository": {
      "issue0": { "id": "ISSUEID1", "number": 1 },
      "milestones": { "nodes": [] },
      "labels": { "nodes": [] }
    }
  }
}`
	mutationResponse := `{
  "data": {
    "update0": { "issue": { "number": 1 } }
  }
}`
	runner := &queuedResponseRunner{
		responses: []string{lookupResponse, mutationResponse},
	}
	client := NewClient(runner, "octo/repo")

	body := "{ code: \"INVALID_COUNTRY_CODE\", message: \"Unknown country code 'XX'\" }\npath C:\\Temp\\file\tend"
	_, err := client.BatchEditIssues(context.Background(), []BatchIssueUpdate{
		{Number: "1", Body: &body},
	})
	if err != nil {
		t.Fatalf("batch edit issues: %v", err)
	}

	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 gh calls, got %d", len(runner.calls))
	}

	query := findGraphQLQueryArg(t, runner.calls[1].args)
	expected := `body: "{ code: \"INVALID_COUNTRY_CODE\", message: \"Unknown country code 'XX'\" }\npath C:\\Temp\\file\tend"`
	if !strings.Contains(query, expected) {
		t.Fatalf("expected query to contain %q, got %q", expected, query)
	}

	doubleEscaped := []string{
		"\\\\\\\"INVALID_COUNTRY_CODE\\\\\\\"",
		"\\\\npath",
		"C:\\\\\\\\Temp",
		"\\\\tend",
	}
	for _, bad := range doubleEscaped {
		if strings.Contains(query, bad) {
			t.Fatalf("found double-escaped sequence %q in query: %q", bad, query)
		}
	}
}

func findGraphQLQueryArg(t *testing.T, args []string) string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-f" && strings.HasPrefix(args[i+1], "query=") {
			return strings.TrimPrefix(args[i+1], "query=")
		}
	}
	t.Fatalf("query argument not found in %v", args)
	return ""
}
