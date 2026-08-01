package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ---- stubs ----

type stubSkillManager struct {
	entries      []SkillListEntry
	skills       map[string]*SkillData
	created      []SkillCreateParams
	createErr    error
	createResult *SkillCreateResult

	deleted   []struct{ name, source string }
	deleteErr error

	updated      []SkillUpdateParams
	updateErr    error
	updateResult *SkillUpdateResult
}

func (s *stubSkillManager) ListSkills() []SkillListEntry {
	return s.entries
}

func (s *stubSkillManager) LoadSkill(name string) (*SkillData, error) {
	if d, ok := s.skills[name]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("skill %q not found", name)
}

func (s *stubSkillManager) CreateSkill(params SkillCreateParams) (*SkillCreateResult, error) {
	s.created = append(s.created, params)
	if s.createErr != nil {
		return nil, s.createErr
	}
	if s.createResult != nil {
		return s.createResult, nil
	}
	return &SkillCreateResult{
		Name:        params.Name,
		Description: params.Description,
		Tags:        params.Tags,
		Source:      params.Source,
		Path:        "/tmp/test/" + params.Name + "/SKILL.md",
	}, nil
}

func (s *stubSkillManager) DeleteSkill(name, source string) error {
	s.deleted = append(s.deleted, struct{ name, source string }{name, source})
	if s.deleteErr != nil {
		return s.deleteErr
	}
	// Simulate: remove from skills map
	delete(s.skills, name)
	return nil
}

func (s *stubSkillManager) UpdateSkill(params SkillUpdateParams) (*SkillUpdateResult, error) {
	s.updated = append(s.updated, params)
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	if s.updateResult != nil {
		return s.updateResult, nil
	}
	return &SkillUpdateResult{
		Name:        params.Name,
		Description: params.Description,
		Tags:        params.Tags,
		Source:      params.Source,
		Path:        "/tmp/test/" + params.Name + "/SKILL.md",
	}, nil
}

// ---- Tests ----

func TestSkillTool_Name(t *testing.T) {
	mgr := &stubSkillManager{}
	tool := NewSkillTool(mgr)
	if tool.Name() != ToolNameSkill {
		t.Errorf("unexpected name: %s", tool.Name())
	}
	if tool.Parallel() {
		t.Error("SkillTool should not be parallel")
	}
	if len(tool.Required()) != 1 || tool.Required()[0] != "operation" {
		t.Errorf("expected required=[\"operation\"], got %v", tool.Required())
	}
}

// ---- List Operation ----

func TestSkillTool_List(t *testing.T) {
	mgr := &stubSkillManager{
		entries: []SkillListEntry{
			{Name: "code-review", Description: "Review code", Tags: []string{"review"}, Source: "project"},
			{Name: "git-commit", Description: "Commit messages", Tags: []string{"git"}, Source: "global"},
		},
	}
	tool := NewSkillTool(mgr)

	// Test list all
	result, err := tool.ExecuteContext(t.Context(), `{"operation": "list"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var listResult struct {
		Success bool             `json:"success"`
		Skills  []SkillListEntry `json:"skills"`
		Count   int              `json:"count"`
	}
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !listResult.Success {
		t.Error("expected success=true")
	}
	if listResult.Count != 2 {
		t.Errorf("expected count=2, got %d", listResult.Count)
	}
	if len(listResult.Skills) != 2 {
		t.Errorf("expected 2 skills, got %d", len(listResult.Skills))
	}

	// Test filter by tag
	result, err = tool.ExecuteContext(t.Context(), `{"operation": "list", "tag": "review"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if listResult.Count != 1 {
		t.Errorf("expected count=1 after tag filter, got %d", listResult.Count)
	}

	// Test filter by non-matching tag
	result, err = tool.ExecuteContext(t.Context(), `{"operation": "list", "tag": "nonexistent"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := json.Unmarshal([]byte(result), &listResult); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if listResult.Count != 0 {
		t.Errorf("expected count=0 after non-matching tag filter, got %d", listResult.Count)
	}
}

// ---- View Operation ----

func TestSkillTool_View(t *testing.T) {
	mgr := &stubSkillManager{
		skills: map[string]*SkillData{
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
		},
	}
	tool := NewSkillTool(mgr)

	// Test view skill
	result, err := tool.ExecuteContext(t.Context(), `{"operation": "view", "name": "code-review"}`)
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
	_, err = tool.ExecuteContext(t.Context(), `{"operation": "view", "name": "nonexistent"}`)
	if err == nil {
		t.Error("expected error for non-existent skill")
	}

	// Test view with missing name
	_, err = tool.ExecuteContext(t.Context(), `{"operation": "view"}`)
	if err == nil {
		t.Error("expected error when name is missing")
	}
}

// ---- Create Operation ----

func TestSkillTool_Create(t *testing.T) {
	mgr := &stubSkillManager{}
	tool := NewSkillTool(mgr)

	// Test basic creation
	result, err := tool.ExecuteContext(t.Context(), `{
		"operation": "create",
		"name": "my-skill",
		"description": "My custom skill",
		"body": "# My Skill\n\nDo stuff."
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(mgr.created))
	}
	c := mgr.created[0]
	if c.Name != "my-skill" {
		t.Errorf("expected name=my-skill, got %s", c.Name)
	}
	if c.Source != "project" {
		t.Errorf("expected default source=project, got %s", c.Source)
	}
	if c.Overwrite {
		t.Error("expected overwrite=false by default")
	}

	// Check response
	if !strings.Contains(result, `"success":true`) {
		t.Error("result should contain success=true")
	}
	if !strings.Contains(result, "my-skill") {
		t.Error("result should contain skill name")
	}

	// Test with explicit source and overwrite
	_, err = tool.ExecuteContext(t.Context(), `{
		"operation": "create",
		"name": "global-skill",
		"description": "A global skill",
		"body": "Instructions...",
		"source": "global",
		"overwrite": true
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c2 := mgr.created[1]
	if c2.Source != "global" {
		t.Errorf("expected source=global, got %s", c2.Source)
	}
	if !c2.Overwrite {
		t.Error("expected overwrite=true")
	}

	// Test missing required params
	_, err = tool.ExecuteContext(t.Context(), `{"operation": "create", "name": "x"}`)
	if err == nil {
		t.Error("expected error when description and body are missing")
	}

	// Test error propagation
	errMgr := &stubSkillManager{createErr: fmt.Errorf("skill already exists")}
	errTool := NewSkillTool(errMgr)
	_, err = errTool.ExecuteContext(t.Context(), `{"operation": "create", "name": "x", "description": "x", "body": "x"}`)
	if err == nil {
		t.Error("expected error propagation")
	}
}

// ---- Invalid Operation ----

func TestSkillTool_InvalidOperation(t *testing.T) {
	mgr := &stubSkillManager{}
	tool := NewSkillTool(mgr)

	_, err := tool.ExecuteContext(t.Context(), `{"operation": "invalid"}`)
	if err == nil {
		t.Error("expected error for invalid operation")
	}
	if !strings.Contains(err.Error(), "unknown operation") {
		t.Errorf("expected 'unknown operation' error, got: %v", err)
	}

	_, err = tool.ExecuteContext(t.Context(), `{}`)
	if err == nil {
		t.Error("expected error when operation is missing")
	}
}

// ---- Delete Operation ----

func TestSkillTool_Delete(t *testing.T) {
	mgr := &stubSkillManager{
		skills: map[string]*SkillData{
			"temp-skill": {
				Name:        "temp-skill",
				Description: "Temporary skill",
			},
		},
	}
	tool := NewSkillTool(mgr)

	// Test basic delete
	result, err := tool.ExecuteContext(t.Context(), `{"operation": "delete", "name": "temp-skill"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, `"success":true`) {
		t.Error("result should contain success=true")
	}
	if !strings.Contains(result, "temp-skill") {
		t.Error("result should contain skill name")
	}
	if len(mgr.deleted) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(mgr.deleted))
	}
	if mgr.deleted[0].name != "temp-skill" {
		t.Errorf("expected name=temp-skill, got %s", mgr.deleted[0].name)
	}

	// Test delete with source
	_, err = tool.ExecuteContext(t.Context(), `{"operation": "delete", "name": "global-skill", "source": "global"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr.deleted[1].source != "global" {
		t.Errorf("expected source=global, got %s", mgr.deleted[1].source)
	}

	// Test delete with missing name
	_, err = tool.ExecuteContext(t.Context(), `{"operation": "delete"}`)
	if err == nil {
		t.Error("expected error when name is missing")
	}

	// Test error propagation
	errMgr := &stubSkillManager{deleteErr: fmt.Errorf("not found")}
	errTool := NewSkillTool(errMgr)
	_, err = errTool.ExecuteContext(t.Context(), `{"operation": "delete", "name": "x"}`)
	if err == nil {
		t.Error("expected error propagation")
	}
}

// ---- Update Operation ----

func TestSkillTool_Update(t *testing.T) {
	mgr := &stubSkillManager{
		skills: map[string]*SkillData{
			"my-skill": {
				Name:        "my-skill",
				Description: "Original description",
				Body:        "Original body",
				Source:      "project",
				Dir:         "/tmp/test",
			},
		},
	}
	tool := NewSkillTool(mgr)

	// Test partial update (only description)
	result, err := tool.ExecuteContext(t.Context(), `{
		"operation": "update",
		"name": "my-skill",
		"description": "Updated description"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, `"success":true`) {
		t.Error("result should contain success=true")
	}
	if !strings.Contains(result, "my-skill") {
		t.Error("result should contain skill name")
	}
	if len(mgr.updated) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(mgr.updated))
	}
	u := mgr.updated[0]
	if u.Description != "Updated description" {
		t.Errorf("expected description='Updated description', got %s", u.Description)
	}

	// Test full update
	_, err = tool.ExecuteContext(t.Context(), `{
		"operation": "update",
		"name": "my-skill",
		"description": "New desc",
		"body": "New body",
		"tags": ["tag1", "tag2"],
		"source": "global"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u2 := mgr.updated[1]
	if u2.Body != "New body" {
		t.Errorf("expected body='New body', got %s", u2.Body)
	}
	if len(u2.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(u2.Tags))
	}
	if u2.Source != "global" {
		t.Errorf("expected source=global, got %s", u2.Source)
	}

	// Test update with missing name
	_, err = tool.ExecuteContext(t.Context(), `{"operation": "update"}`)
	if err == nil {
		t.Error("expected error when name is missing")
	}

	// Test error propagation
	errMgr := &stubSkillManager{updateErr: fmt.Errorf("not found")}
	errTool := NewSkillTool(errMgr)
	_, err = errTool.ExecuteContext(t.Context(), `{"operation": "update", "name": "x"}`)
	if err == nil {
		t.Error("expected error propagation")
	}
}
