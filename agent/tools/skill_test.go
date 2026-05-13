package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type stubSkillLister struct {
	entries []SkillListEntry
}

func (s *stubSkillLister) ListSkills() []SkillListEntry {
	return s.entries
}

func TestSkillsListTool(t *testing.T) {
	lister := &stubSkillLister{
		entries: []SkillListEntry{
			{Name: "code-review", Description: "Review code", Tags: []string{"review"}, Source: "project"},
			{Name: "git-commit", Description: "Commit messages", Tags: []string{"git"}, Source: "global"},
		},
	}

	tool := NewSkillsListTool(lister)

	if tool.Name() != ToolNameSkillsList {
		t.Errorf("unexpected name: %s", tool.Name())
	}
	if tool.Parallel() {
		t.Error("SkillsListTool should not be parallel")
	}

	// Test list all
	result, err := tool.ExecuteContext(t.Context(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed skillsListResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !parsed.Success {
		t.Error("expected success=true")
	}
	if parsed.Count != 2 {
		t.Errorf("expected count=2, got %d", parsed.Count)
	}
	if len(parsed.Skills) != 2 {
		t.Errorf("expected 2 skills, got %d", len(parsed.Skills))
	}

	// Test filter by tag
	result, err = tool.ExecuteContext(t.Context(), `{"tag": "review"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed.Count != 1 {
		t.Errorf("expected count=1 after tag filter, got %d", parsed.Count)
	}

	// Test filter by non-matching tag
	result, err = tool.ExecuteContext(t.Context(), `{"tag": "nonexistent"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed.Count != 0 {
		t.Errorf("expected count=0 after non-matching tag filter, got %d", parsed.Count)
	}
}

type stubSkillLoader struct {
	data map[string]*SkillData
}

func (s *stubSkillLoader) LoadSkill(name string) (*SkillData, error) {
	if d, ok := s.data[name]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("skill %q not found", name)
}

func TestSkillViewTool(t *testing.T) {
	loader := &stubSkillLoader{data: map[string]*SkillData{
		"code-review": {
			Name:        "code-review",
			Description: "Review code",
			Body:        "# Code Review\n\n1. Check security\n2. Check style",
			Source:      "project",
			Dir:         "/tmp/test",
			Files: map[string]string{
				"references/checklist.md": "## Checklist\n- Item 1",
			},
		},
	}}

	tool := NewSkillViewTool(loader)

	if tool.Name() != ToolNameSkillView {
		t.Errorf("unexpected name: %s", tool.Name())
	}
	if tool.Parallel() {
		t.Error("SkillViewTool should not be parallel")
	}
	if len(tool.Required()) != 1 || tool.Required()[0] != "name" {
		t.Errorf("expected required=[\"name\"], got %v", tool.Required())
	}

	// Test load skill
	result, err := tool.ExecuteContext(t.Context(), `{"name": "code-review"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "code-review") {
		t.Error("result should contain skill name")
	}
	if !strings.Contains(result, "Check security") {
		t.Error("result should contain body")
	}

	// Test non-existent skill
	_, err = tool.ExecuteContext(t.Context(), `{"name": "nonexistent"}`)
	if err == nil {
		t.Error("expected error for non-existent skill")
	}
}