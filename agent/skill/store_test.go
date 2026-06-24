package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{
		"code-review", "git-commit", "a", "abc123", "test1-test2",
		"CodeReview", "code review", "code_review", "code.review",
		"代码审查", "中文技能", "测试-skill-1",
	}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) should be valid, got: %v", name, err)
		}
	}

	invalid := map[string]string{
		"": "empty",
	}
	for name, reason := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) should be invalid (%s)", name, reason)
		}
	}
}

func TestNewStore(t *testing.T) {
	s := NewStore("")
	if s == nil {
		t.Fatal("NewStore returned nil")
	}
	if len(s.dirs) != 1 {
		t.Errorf("expected 1 dir (global only), got %d: %v", len(s.dirs), s.dirs)
	}

	wd, _ := os.Getwd()
	s2 := NewStore(wd)
	// Now includes .tachi/skills, .claude/skills, .cursor/skills, and global
	if len(s2.dirs) < 2 {
		t.Errorf("expected at least 2 dirs (project-level + global), got %d: %v", len(s2.dirs), s2.dirs)
	}
}

func TestStoreListAndLoad(t *testing.T) {
	// Create a temp project structure with isolated global dir
	tmpDir := t.TempDir()
	tmpGlobal := t.TempDir()
	globalSkillDir := filepath.Join(tmpGlobal, "skills")
	projectSkillDir := filepath.Join(tmpDir, ".tachi", "skills")
	skillDir := filepath.Join(projectSkillDir, "code-review")
	err := os.MkdirAll(skillDir, 0755)
	if err != nil {
		t.Fatal(err)
	}

	skillMD := `---
name: code-review
description: Review code for bugs and security issues
tags: [review, security]
---
# Code Review

When reviewing code:
1. Check for security issues
2. Check for bugs
3. Check style
`
	err = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Also create a reference file
	refDir := filepath.Join(skillDir, "references")
	err = os.MkdirAll(refDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(refDir, "checklist.md"), []byte("## Checklist\n- Item 1\n- Item 2"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	s := newStore([]string{projectSkillDir, globalSkillDir}, []string{SourceProject, SourceGlobal})

	// Test List
	metas := s.List()
	if len(metas) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(metas))
	}
	if metas[0].Name != "code-review" {
		t.Errorf("expected name 'code-review', got %q", metas[0].Name)
	}
	if metas[0].Description != "Review code for bugs and security issues" {
		t.Errorf("unexpected description: %q", metas[0].Description)
	}
	if len(metas[0].Tags) != 2 || metas[0].Tags[0] != "review" || metas[0].Tags[1] != "security" {
		t.Errorf("unexpected tags: %v", metas[0].Tags)
	}
	if metas[0].Source != SourceProject {
		t.Errorf("expected source 'project', got %q", metas[0].Source)
	}

	// Test Load
	sk, err := s.Load("code-review")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if sk.Meta.Name != "code-review" {
		t.Errorf("unexpected name: %q", sk.Meta.Name)
	}
	if sk.Body == "" {
		t.Error("expected non-empty body")
	}
	if sk.Dir != skillDir {
		t.Errorf("dir mismatch: %q vs %q", sk.Dir, skillDir)
	}
	if len(sk.Files) != 1 {
		t.Errorf("expected 1 supporting file, got %d: %v", len(sk.Files), sk.Files)
	}
	if content, ok := sk.Files["references/checklist.md"]; !ok || content != "## Checklist\n- Item 1\n- Item 2" {
		t.Errorf("unexpected supporting file content: %q", content)
	}

	// Test Load non-existent
	_, err = s.Load("nonexistent")
	if err == nil {
		t.Error("expected error for non-existent skill")
	}
}

func TestStoreList_Empty(t *testing.T) {
	tmpDir := t.TempDir()
	tmpGlobal := t.TempDir()
	projectSkillDir := filepath.Join(tmpDir, ".tachi", "skills")
	globalSkillDir := filepath.Join(tmpGlobal, "skills")
	s := newStore([]string{projectSkillDir, globalSkillDir}, []string{SourceProject, SourceGlobal})
	metas := s.List()
	if len(metas) != 0 {
		t.Errorf("expected 0 skills, got %d", len(metas))
	}
}

func TestStoreList_InvalidFrontmatter(t *testing.T) {
	tmpDir := t.TempDir()
	tmpGlobal := t.TempDir()
	projectSkillDir := filepath.Join(tmpDir, ".tachi", "skills")
	globalSkillDir := filepath.Join(tmpGlobal, "skills")
	skillDir := filepath.Join(projectSkillDir, "bad-skill")
	err := os.MkdirAll(skillDir, 0755)
	if err != nil {
		t.Fatal(err)
	}

	// SKILL.md with no frontmatter
	skillMD := `# Bad Skill
This has no frontmatter.`
	err = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0644)
	if err != nil {
		t.Fatal(err)
	}

	s := newStore([]string{projectSkillDir, globalSkillDir}, []string{SourceProject, SourceGlobal})
	metas := s.List()
	if len(metas) != 1 {
		t.Fatalf("expected 1 skill (from directory name fallback), got %d", len(metas))
	}
	if metas[0].Name != "bad-skill" {
		t.Errorf("expected fallback name 'bad-skill', got %q", metas[0].Name)
	}
}

func TestStoreResolveCommand(t *testing.T) {
	tmpDir := t.TempDir()
	tmpGlobal := t.TempDir()
	projectSkillDir := filepath.Join(tmpDir, ".tachi", "skills")
	globalSkillDir := filepath.Join(tmpGlobal, "skills")
	skillDir := filepath.Join(projectSkillDir, "code-review")
	err := os.MkdirAll(skillDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: code-review
description: Test
---
Body
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	s := newStore([]string{projectSkillDir, globalSkillDir}, []string{SourceProject, SourceGlobal})

	tests := []struct {
		input    string
		wantName string
		wantOk   bool
	}{
		{"code-review", "code-review", true},
		{"/code-review", "code-review", true},
		{"CODE-REVIEW", "code-review", true},
		{"Code-Review", "code-review", true},
		{"nonexistent", "", false},
		{"/nonexistent", "", false},
	}

	for _, tt := range tests {
		name, ok := s.ResolveCommand(tt.input)
		if ok != tt.wantOk {
			t.Errorf("ResolveCommand(%q) ok = %v, want %v", tt.input, ok, tt.wantOk)
		}
		if name != tt.wantName {
			t.Errorf("ResolveCommand(%q) name = %q, want %q", tt.input, name, tt.wantName)
		}
	}
}

func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantName    string
		wantDesc    string
		wantTags    []string
		wantBody    string
		wantErr     bool
	}{
		{
			name:     "valid",
			content:  "---\nname: test\ndescription: A test skill\ntags: [a, b]\n---\n\nThis is the body.",
			wantName: "test",
			wantDesc: "A test skill",
			wantTags: []string{"a", "b"},
			wantBody: "This is the body.",
		},
		{
			name:    "no frontmatter",
			content: "Just a body.",
			wantErr: true,
		},
		{
			name:    "unclosed frontmatter",
			content: "---\nname: test\n",
			wantErr: true,
		},
		{
			name:     "minimal",
			content:  "---\nname: minimal\ndescription: min\n---\nBody here",
			wantName: "minimal",
			wantDesc: "min",
			wantBody: "Body here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, body, err := parseFrontmatter(tt.content)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fm.Name != tt.wantName {
				t.Errorf("name = %q, want %q", fm.Name, tt.wantName)
			}
			if fm.Description != tt.wantDesc {
				t.Errorf("description = %q, want %q", fm.Description, tt.wantDesc)
			}
			if body != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func TestBuildSkillListPrompt(t *testing.T) {
	tests := []struct {
		name  string
		metas []SkillMeta
		want  string
	}{
		{
			name:  "empty",
			metas: nil,
			want:  "",
		},
		{
			name: "with skills",
			metas: []SkillMeta{
				{Name: "code-review", Description: "Review code", Tags: []string{"review"}},
				{Name: "git-commit", Description: "Commit messages", Tags: nil},
			},
			want: `<available_skills>
  <skill name="code-review" description="Review code" tags="review"/>
  <skill name="git-commit" description="Commit messages"/>
</available_skills>

To use a skill, call Skill(operation="view", name=...) or the user can type /skill-name.`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildSkillListPrompt(tt.metas)
			if got != tt.want {
				t.Errorf("BuildSkillListPrompt() = %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestBuildActivationMessage(t *testing.T) {
	sk := &Skill{
		Meta: SkillMeta{
			Name:        "code-review",
			Description: "Review code",
		},
		Body: "# Code Review\n\nWhen reviewing code:\n1. Check security\n2. Check bugs",
		Dir:  "/tmp/skills/code-review",
		Files: map[string]string{
			"references/checklist.md": "## Checklist\n- Item 1\n- Item 2",
		},
	}

	msg := BuildActivationMessage(sk, "")
	if msg == "" {
		t.Error("expected non-empty message")
	}
	if !contains(msg, "code-review") {
		t.Error("message should contain skill name")
	}
	if !contains(msg, "When reviewing code") {
		t.Error("message should contain body")
	}
	if !contains(msg, "references/checklist.md") {
		t.Error("message should mention supporting files")
	}

	// With user instruction
	msg2 := BuildActivationMessage(sk, "main.go")
	if !contains(msg2, "main.go") {
		t.Error("message should contain user instruction")
	}
}

func TestXmlEscape(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"<script>", "&lt;script&gt;"},
		{"a & b", "a &amp; b"},
		{`"quoted"`, "&quot;quoted&quot;"},
		{"normal", "normal"},
	}

	for _, tt := range tests {
		got := xmlEscape(tt.input)
		if got != tt.want {
			t.Errorf("xmlEscape(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && len(s) >= len(substr) &&
		searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestStoreCreate(t *testing.T) {
	tmpDir := t.TempDir()
	projectSkillDir := filepath.Join(tmpDir, ".tachi", "skills")
	tmpGlobal := t.TempDir()
	globalSkillDir := filepath.Join(tmpGlobal, "skills")
	s := newStore([]string{projectSkillDir, globalSkillDir}, []string{SourceProject, SourceGlobal})

	// Test successful creation
	sk, err := s.Create("test-skill", "A test skill", "# Test\n\nDo stuff.", []string{"test", "example"}, SourceProject, false)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if sk.Meta.Name != "test-skill" {
		t.Errorf("expected name 'test-skill', got %q", sk.Meta.Name)
	}
	if sk.Meta.Description != "A test skill" {
		t.Errorf("expected description, got %q", sk.Meta.Description)
	}
	if len(sk.Meta.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(sk.Meta.Tags))
	}
	if sk.Meta.Source != SourceProject {
		t.Errorf("expected source 'project', got %q", sk.Meta.Source)
	}
	if sk.Body != "# Test\n\nDo stuff." {
		t.Errorf("body mismatch: %q", sk.Body)
	}

	// Verify the file was actually written
	skillFile := filepath.Join(projectSkillDir, "test-skill", "SKILL.md")
	data, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatalf("failed to read created SKILL.md: %v", err)
	}
	content := string(data)
	if !contains(content, "name: test-skill") {
		t.Error("SKILL.md should contain frontmatter name")
	}
	if !contains(content, "Do stuff.") {
		t.Error("SKILL.md should contain body")
	}
	if !contains(content, "tags:") {
		t.Error("SKILL.md should contain tags")
	}

	// Verify it appears in List
	metas := s.List()
	found := false
	for _, m := range metas {
		if m.Name == "test-skill" {
			found = true
			break
		}
	}
	if !found {
		t.Error("created skill should appear in List()")
	}

	// Test duplicate without overwrite
	_, err = s.Create("test-skill", "Duplicate", "body", nil, SourceProject, false)
	if err == nil {
		t.Error("expected error for duplicate skill without overwrite")
	}

	// Test duplicate with overwrite
	sk2, err := s.Create("test-skill", "Updated", "new body", []string{"updated"}, SourceProject, true)
	if err != nil {
		t.Fatalf("Create with overwrite failed: %v", err)
	}
	if sk2.Meta.Description != "Updated" {
		t.Errorf("expected description 'Updated' after overwrite, got %q", sk2.Meta.Description)
	}
	if sk2.Body != "new body" {
		t.Errorf("expected body 'new body' after overwrite, got %q", sk2.Body)
	}

	// Test global skill creation
	sk3, err := s.Create("global-skill", "Global", "body", nil, SourceGlobal, false)
	if err != nil {
		t.Fatalf("Create global skill failed: %v", err)
	}
	if sk3.Meta.Source != SourceGlobal {
		t.Errorf("expected source 'global', got %q", sk3.Meta.Source)
	}
}

func TestStoreCreate_Validation(t *testing.T) {
	tmpDir := t.TempDir()
	projectSkillDir := filepath.Join(tmpDir, ".tachi", "skills")
	s := newStore([]string{projectSkillDir}, []string{SourceProject})

	// Invalid name (empty)
	_, err := s.Create("", "desc", "body", nil, SourceProject, false)
	if err == nil {
		t.Error("expected error for invalid name")
	}

	// Description too long
	longDesc := make([]byte, MaxDescriptionLen+1)
	for i := range longDesc {
		longDesc[i] = 'a'
	}
	_, err = s.Create("valid-name", string(longDesc), "body", nil, SourceProject, false)
	if err == nil {
		t.Error("expected error for too-long description")
	}

	// Unknown source
	_, err = s.Create("valid-name", "desc", "body", nil, "unknown", false)
	if err == nil {
		t.Error("expected error for unknown source")
	}
}

func TestStoreCreate_NoProjectDir(t *testing.T) {
	// Simulate "no project dir" by using newStore with only a global dir
	tmpGlobal := t.TempDir()
	globalSkillDir := filepath.Join(tmpGlobal, "skills")
	s := newStore([]string{globalSkillDir}, []string{SourceGlobal})

	sk, err := s.Create("global-only", "A global skill", "body", nil, SourceGlobal, false)
	if err != nil {
		t.Fatalf("Create global skill without project dir failed: %v", err)
	}
	if sk.Meta.Source != SourceGlobal {
		t.Errorf("expected source 'global', got %q", sk.Meta.Source)
	}

	// Verify file written to the right place
	skillFile := filepath.Join(globalSkillDir, "global-only", "SKILL.md")
	if _, err := os.Stat(skillFile); os.IsNotExist(err) {
		t.Errorf("expected SKILL.md at %s", skillFile)
	}

	// Project source should fail when no project dir
	_, err = s.Create("project-fail", "desc", "body", nil, SourceProject, false)
	if err == nil {
		t.Error("expected error for project source when no project dir")
	}
}

func TestBuildSkillMarkdown(t *testing.T) {
	result := buildSkillMarkdown("my-skill", "A skill", "# Body\n\nText", []string{"tag1", "tag2"})

	if !contains(result, "name: my-skill") {
		t.Error("frontmatter should contain name")
	}
	if !contains(result, "description: A skill") {
		t.Error("frontmatter should contain description")
	}
	if !contains(result, "tags:") {
		t.Error("frontmatter should contain tags")
	}
	if !contains(result, "  - tag1") {
		t.Error("frontmatter should list tags")
	}
	if !contains(result, "# Body") {
		t.Error("should contain body")
	}
	if !contains(result, "Text\n") {
		t.Error("should end with trailing newline")
	}

	// No tags
	result2 := buildSkillMarkdown("untagged", "desc", "body", nil)
	if contains(result2, "tags:") {
		t.Error("should not contain tags when nil")
	}
}

// TestStoreShadowPriority verifies the search order:
//
//	.tachi/skills > .claude/skills > .cursor/skills > global
//
// Same-named skills in higher-priority dirs shadow lower ones.
func TestStoreShadowPriority(t *testing.T) {
	tmpDir := t.TempDir()
	tmpGlobal := t.TempDir()

	tachiDir := filepath.Join(tmpDir, ".tachi", "skills")
	claudeDir := filepath.Join(tmpDir, ".claude", "skills")
	cursorDir := filepath.Join(tmpDir, ".cursor", "skills")
	globalDir := filepath.Join(tmpGlobal, "skills")

	// Create "my-skill" in all four dirs with different descriptions
	for _, entry := range []struct {
		dir, name, desc, source string
	}{
		{tachiDir, "my-skill", "Tachi version", SourceProject},
		{claudeDir, "my-skill", "Claude version", SourceClaude},
		{cursorDir, "my-skill", "Cursor version", SourceCursor},
		{globalDir, "my-skill", "Global version", SourceGlobal},
	} {
		skillDir := filepath.Join(entry.dir, entry.name)
		if err := os.MkdirAll(skillDir, 0755); err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\nBody\n", entry.name, entry.desc)
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	s := newStore([]string{tachiDir, claudeDir, cursorDir, globalDir},
		[]string{SourceProject, SourceClaude, SourceCursor, SourceGlobal})

	// List should only show one "my-skill" (tachi, since it's first)
	metas := s.List()
	found := false
	for _, m := range metas {
		if m.Name == "my-skill" {
			found = true
			if m.Description != "Tachi version" {
				t.Errorf("expected Tachi version to shadow others, got %q (source=%s)", m.Description, m.Source)
			}
			if m.Source != SourceProject {
				t.Errorf("expected source 'project', got %q", m.Source)
			}
		}
	}
	if !found {
		t.Error("my-skill not found in list")
	}

	// Load should return the tachi version
	sk, err := s.Load("my-skill")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if sk.Meta.Description != "Tachi version" {
		t.Errorf("Load returned wrong version: %q", sk.Meta.Description)
	}

	// If tachi dir is empty, claude should win
	s2 := newStore([]string{claudeDir, cursorDir, globalDir},
		[]string{SourceClaude, SourceCursor, SourceGlobal})
	sk2, err := s2.Load("my-skill")
	if err != nil {
		t.Fatalf("Load from s2 failed: %v", err)
	}
	if sk2.Meta.Description != "Claude version" {
		t.Errorf("expected Claude version, got %q", sk2.Meta.Description)
	}

	// If only cursor and global, cursor should win
	s3 := newStore([]string{cursorDir, globalDir},
		[]string{SourceCursor, SourceGlobal})
	sk3, err := s3.Load("my-skill")
	if err != nil {
		t.Fatalf("Load from s3 failed: %v", err)
	}
	if sk3.Meta.Description != "Cursor version" {
		t.Errorf("expected Cursor version, got %q", sk3.Meta.Description)
	}
}

// TestStoreCreate_ClaudeCursor verifies that claude/cursor sources are
// rejected for write operations (they are read-only for import compatibility).
func TestStoreCreate_ClaudeCursor(t *testing.T) {
	tmpDir := t.TempDir()
	tmpGlobal := t.TempDir()

	tachiDir := filepath.Join(tmpDir, ".tachi", "skills")
	claudeDir := filepath.Join(tmpDir, ".claude", "skills")
	cursorDir := filepath.Join(tmpDir, ".cursor", "skills")
	globalDir := filepath.Join(tmpGlobal, "skills")

	s := newStore([]string{tachiDir, claudeDir, cursorDir, globalDir},
		[]string{SourceProject, SourceClaude, SourceCursor, SourceGlobal})

	// Create in claude dir — should fail (read-only)
	_, err := s.Create("claude-skill", "A Claude skill", "body", nil, SourceClaude, false)
	if err == nil {
		t.Error("expected error when creating skill with source='claude'")
	}

	// Create in cursor dir — should fail (read-only)
	_, err = s.Create("cursor-skill", "A Cursor skill", "body", nil, SourceCursor, false)
	if err == nil {
		t.Error("expected error when creating skill with source='cursor'")
	}

	// Create in tachi dir — should succeed
	sk, err := s.Create("tachi-skill", "A Tachi skill", "body", nil, SourceProject, false)
	if err != nil {
		t.Fatalf("Create in tachi dir failed: %v", err)
	}
	if sk.Meta.Source != SourceProject {
		t.Errorf("expected source 'project', got %q", sk.Meta.Source)
	}

	// Unknown source should fail
	_, err = s.Create("bad", "desc", "body", nil, "unknown", false)
	if err == nil {
		t.Error("expected error for unknown source")
	}
}

// TestStoreDeleteUpdate_ClaudeCursor tests that claude/cursor sources are
// rejected for delete/update operations (read-only for import compatibility).
func TestStoreDeleteUpdate_ClaudeCursor(t *testing.T) {
	tmpDir := t.TempDir()
	tmpGlobal := t.TempDir()

	claudeDir := filepath.Join(tmpDir, ".claude", "skills")
	globalDir := filepath.Join(tmpGlobal, "skills")

	s := newStore([]string{claudeDir, globalDir},
		[]string{SourceClaude, SourceGlobal})

	// Delete with source="claude" should fail
	if err := s.Delete("test-skill", SourceClaude); err == nil {
		t.Error("expected error when deleting with source='claude'")
	}

	// Update with source="claude" should fail
	_, err := s.Update("test-skill", "Updated", "new body", nil, SourceClaude)
	if err == nil {
		t.Error("expected error when updating with source='claude'")
	}

	// Delete with source="cursor" should fail
	if err := s.Delete("test-skill", SourceCursor); err == nil {
		t.Error("expected error when deleting with source='cursor'")
	}

	// Create a skill in global, then delete/update with valid source should work
	_, err = s.Create("global-skill", "Test", "body", nil, SourceGlobal, false)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Delete with source="global" should succeed
	if err := s.Delete("global-skill", SourceGlobal); err != nil {
		t.Fatalf("Delete with global source failed: %v", err)
	}

	// Create another and update it
	_, err = s.Create("updatable", "Original", "original body", nil, SourceGlobal, false)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	updated, err := s.Update("updatable", "Updated desc", "new body", nil, SourceGlobal)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if updated.Meta.Description != "Updated desc" {
		t.Errorf("expected description 'Updated desc', got %q", updated.Meta.Description)
	}
}