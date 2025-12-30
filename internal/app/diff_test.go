package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mitsuhiko/gh-issue-sync/internal/theme"
)

func TestWordsSimilar(t *testing.T) {
	tests := []struct {
		a, b   string
		expect bool
	}{
		{"foo", "foo!", true},          // append
		{"foo", "!foo", true},          // prepend
		{"color", "colour", true},      // small edit
		{"hello", "hallo", true},       // single char change
		{"test", "testing", true},      // suffix add
		{"prefix", "prefix_new", true}, // suffix add
		{"cat", "dog", false},          // completely different
		{"abc", "xyz", false},          // completely different
		{"a", "abcdefgh", false},       // too different in length
		{"", "", true},                 // both empty
		{"foo", "foo", true},           // identical
	}

	for _, tc := range tests {
		got := wordsSimilar(tc.a, tc.b)
		if got != tc.expect {
			t.Errorf("wordsSimilar(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.expect)
		}
	}
}

func TestComputeCharDiff(t *testing.T) {
	tests := []struct {
		old, new    string
		wantDel     string // expected deleted chars
		wantIns     string // expected inserted chars
		wantMinEq   int    // minimum expected equal chars
	}{
		{"foo", "foo!", "", "!", 3},      // append: foo stays, ! added
		{"foo", "!foo", "", "!", 3},      // prepend: foo stays, ! added
		{"color", "colour", "", "u", 5},  // LCS keeps "color", inserts "u"
		{"hello", "hallo", "e", "a", 4},  // replace e with a
	}

	for _, tc := range tests {
		ops := computeCharDiff(tc.old, tc.new)

		var del, ins, eq strings.Builder
		for _, op := range ops {
			switch op.Type {
			case diffDelete:
				del.WriteString(op.Text)
			case diffInsert:
				ins.WriteString(op.Text)
			case diffEqual:
				eq.WriteString(op.Text)
			}
		}

		if del.String() != tc.wantDel {
			t.Errorf("computeCharDiff(%q, %q): deleted = %q, want %q", tc.old, tc.new, del.String(), tc.wantDel)
		}
		if ins.String() != tc.wantIns {
			t.Errorf("computeCharDiff(%q, %q): inserted = %q, want %q", tc.old, tc.new, ins.String(), tc.wantIns)
		}
		if len(eq.String()) < tc.wantMinEq {
			t.Errorf("computeCharDiff(%q, %q): equal = %q (len %d), want at least %d chars", 
				tc.old, tc.new, eq.String(), len(eq.String()), tc.wantMinEq)
		}
	}
}

func TestRefineWordDiff(t *testing.T) {
	// Test that delete+insert of similar words gets refined to diffChange
	ops := []diffOp{
		{Type: diffDelete, Text: "foo"},
		{Type: diffInsert, Text: "foo!"},
	}

	refined := refineWordDiff(ops)

	if len(refined) != 1 {
		t.Fatalf("expected 1 op, got %d", len(refined))
	}
	if refined[0].Type != diffChange {
		t.Errorf("expected diffChange, got %v", refined[0].Type)
	}
	if refined[0].Text != "foo" {
		t.Errorf("expected old text 'foo', got %q", refined[0].Text)
	}
	if refined[0].NewText != "foo!" {
		t.Errorf("expected new text 'foo!', got %q", refined[0].NewText)
	}
	if len(refined[0].CharOps) == 0 {
		t.Error("expected CharOps to be populated")
	}
}

func TestRefineWordDiffDissimilar(t *testing.T) {
	// Test that delete+insert of dissimilar words stays as separate ops
	ops := []diffOp{
		{Type: diffDelete, Text: "cat"},
		{Type: diffInsert, Text: "dog"},
	}

	refined := refineWordDiff(ops)

	if len(refined) != 2 {
		t.Fatalf("expected 2 ops (dissimilar words), got %d", len(refined))
	}
	if refined[0].Type != diffDelete || refined[1].Type != diffInsert {
		t.Errorf("expected delete+insert, got %v+%v", refined[0].Type, refined[1].Type)
	}
}

func TestFormatInlineWordDiff(t *testing.T) {
	var buf bytes.Buffer
	app := &App{
		Out:   &buf,
		Theme: theme.Default(),
	}

	// Just verify it doesn't panic and returns something
	result := app.formatInlineWordDiff("foo bar", "foo bar!")
	if result == "" {
		t.Error("expected non-empty result")
	}
	// The result should contain "foo" unchanged and show the change from "bar" to "bar!"
	t.Logf("Result: %s", result)
}
