package theme

import (
	"strings"
	"testing"
)

func TestDefaultTheme(t *testing.T) {
	th := Default()
	if th == nil {
		t.Fatal("Default() returned nil")
	}
	if th.styler == nil {
		t.Fatal("Theme has no styler")
	}
}

func TestFormatStatus(t *testing.T) {
	th := Default()
	
	// Each status should have color escape codes
	for _, status := range []string{"A", "U", "M", "D"} {
		result := th.FormatStatus(status)
		if !strings.Contains(result, "\x1b[") {
			t.Errorf("FormatStatus(%q) has no escape codes", status)
		}
		if !strings.Contains(result, status) {
			t.Errorf("FormatStatus(%q) doesn't contain status", status)
		}
	}
}

func TestFormatIssueHeader(t *testing.T) {
	th := Default()
	result := th.FormatIssueHeader("U", "123", "Test issue")
	
	// Should contain the status, number, and title
	if !strings.Contains(result, "#123") {
		t.Errorf("Header missing issue number: %q", result)
	}
	if !strings.Contains(result, "Test issue") {
		t.Errorf("Header missing title: %q", result)
	}
	// Should have escape codes
	if !strings.Contains(result, "\x1b[") {
		t.Errorf("Header has no escape codes: %q", result)
	}
}

func TestFormatChange(t *testing.T) {
	th := Default()
	result := th.FormatChange("title", `"old"`, `"new"`)
	
	if !strings.Contains(result, "title:") {
		t.Errorf("Change missing field name: %q", result)
	}
	if !strings.Contains(result, "old") {
		t.Errorf("Change missing old value: %q", result)
	}
	if !strings.Contains(result, "new") {
		t.Errorf("Change missing new value: %q", result)
	}
	if !strings.Contains(result, "->") {
		t.Errorf("Change missing arrow: %q", result)
	}
}

func TestFormatLabel(t *testing.T) {
	th := Default()
	
	// Dark background should get light text
	result := th.FormatLabel("bug", "d73a4a")
	if !strings.Contains(result, "bug") {
		t.Errorf("Label missing name: %q", result)
	}
	if !strings.Contains(result, "\x1b[") {
		t.Errorf("Label has no escape codes: %q", result)
	}
	
	// Light background should get dark text
	result = th.FormatLabel("enhancement", "a2eeef")
	if !strings.Contains(result, "enhancement") {
		t.Errorf("Label missing name: %q", result)
	}
	
	// Invalid color should return plain text
	result = th.FormatLabel("test", "invalid")
	if result != "test" {
		t.Errorf("Invalid color label = %q, want plain 'test'", result)
	}
}

func TestFormatLabelList(t *testing.T) {
	th := Default()
	
	// Empty list
	result := th.FormatLabelList(nil)
	if !strings.Contains(result, "[]") {
		t.Errorf("Empty list = %q, want []", result)
	}
	
	// With labels
	labels := []LabelColor{
		{Name: "bug", Color: "d73a4a"},
		{Name: "enhancement", Color: "a2eeef"},
	}
	result = th.FormatLabelList(labels)
	if !strings.Contains(result, "bug") {
		t.Errorf("List missing 'bug': %q", result)
	}
	if !strings.Contains(result, "enhancement") {
		t.Errorf("List missing 'enhancement': %q", result)
	}
}

func TestFormatLabelChange(t *testing.T) {
	th := Default()
	
	added := []LabelColor{{Name: "help wanted", Color: "008672"}}
	removed := []LabelColor{{Name: "wontfix", Color: "ffffff"}}
	
	result := th.FormatLabelChange(added, removed)
	
	if !strings.Contains(result, "+") {
		t.Errorf("Change missing + indicator: %q", result)
	}
	if !strings.Contains(result, "-") {
		t.Errorf("Change missing - indicator: %q", result)
	}
	if !strings.Contains(result, "help wanted") {
		t.Errorf("Change missing added label: %q", result)
	}
	if !strings.Contains(result, "wontfix") {
		t.Errorf("Change missing removed label: %q", result)
	}
}

func TestConvenienceMethods(t *testing.T) {
	th := Default()
	
	tests := []struct {
		name   string
		method func(string) string
	}{
		{"AccentText", th.AccentText},
		{"SuccessText", th.SuccessText},
		{"ErrorText", th.ErrorText},
		{"WarningText", th.WarningText},
		{"MutedText", th.MutedText},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.method("test")
			if !strings.Contains(result, "test") {
				t.Errorf("%s missing text", tt.name)
			}
			if !strings.Contains(result, "\x1b[") {
				t.Errorf("%s has no escape codes", tt.name)
			}
		})
	}
}
