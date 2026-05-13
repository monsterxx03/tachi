package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{"code-review", "git-commit", "a", "abc123", "test1-test2"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) should be valid, got: %v", name, err)
		}
	}

	invalid := map[string]string{
		"":          "empty",
		"CodeReview": "uppercase",
		"code review": "space",
		"code_review": "underscore",
		"code.review": "dot",
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
	if len(s2.dirs) != 2 {
		t.Errorf("expected 2 dirs (project + global), got %d: %v", len(s2.dirs), s2.dirs)
	}
}

func TestStoreListAndLoad(t *testing.T) {
	// Create a temp project structure
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, ".tachi", "skills", "code-review")
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

	s := NewStore(tmpDir)

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
	s := NewStore(tmpDir)
	metas := s.List()
	if len(metas) != 0 {
		t.Errorf("expected 0 skills, got %d", len(metas))
	}
}

func TestStoreList_InvalidFrontmatter(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, ".tachi", "skills", "bad-skill")
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

	s := NewStore(tmpDir)
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
	skillDir := filepath.Join(tmpDir, ".tachi", "skills", "code-review")
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

	s := NewStore(tmpDir)

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

To use a skill, call skill_view(name) or the user can type /skill-name.`,
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