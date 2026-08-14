package systemreminder

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDateReminder_Fires(t *testing.T) {
	r := DateReminder{}
	lines := r.Generate(t.Context(), Context{
		IsFirstMessage: true,
		Now:            time.Date(2025, 7, 15, 14, 30, 45, 0, time.UTC),
	})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "Tuesday, July 15, 2025") {
		t.Errorf("expected date in output, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "14:30:45") {
		t.Errorf("expected time in output, got: %s", lines[0])
	}
}

func TestDateReminder_AlwaysFires(t *testing.T) {
	r := DateReminder{}
	// DateReminder fires on every real user message, regardless of IsFirstMessage or LastMessageDate.
	lines := r.Generate(t.Context(), Context{IsFirstMessage: false})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line (always fires), got %d", len(lines))
	}
}

func TestDateReminder_SkipsOnToolResult(t *testing.T) {
	r := DateReminder{}
	lines := r.Generate(t.Context(), Context{
		IsFirstMessage: false,
		Now:            time.Date(2025, 7, 15, 14, 30, 45, 0, time.UTC),
		IsToolResult:   true,
	})
	if len(lines) != 0 {
		t.Errorf("expected no output when IsToolResult is true, got: %v", lines)
	}
}

func TestDateReminder_AlwaysFiresWithDateChanged(t *testing.T) {
	r := DateReminder{}
	lines := r.Generate(t.Context(), Context{
		IsFirstMessage:  false,
		LastMessageDate: "2025-07-14",
		Now:             time.Date(2025, 7, 15, 14, 30, 45, 0, time.UTC),
	})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line (always fires), got %d", len(lines))
	}
	if !strings.Contains(lines[0], "Tuesday, July 15, 2025") {
		t.Errorf("expected date in output, got: %s", lines[0])
	}
}

func TestCollector_Empty(t *testing.T) {
	c := NewCollector()
	result := c.Collect(t.Context(), Context{
		IsFirstMessage: false,
	})
	if result != "" {
		t.Errorf("expected empty, got: %s", result)
	}
}

func TestCollector_FirstMessage(t *testing.T) {
	c := NewCollector(DateReminder{})
	result := c.Collect(t.Context(), Context{
		IsFirstMessage: true,
		Now:            time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if !strings.Contains(result, "<system-reminder>") {
		t.Errorf("expected <system-reminder> tag, got: %s", result)
	}
	if !strings.Contains(result, "</system-reminder>") {
		t.Errorf("expected </system-reminder> tag, got: %s", result)
	}
	if !strings.Contains(result, "Wednesday, January 1, 2025") {
		t.Errorf("expected date, got: %s", result)
	}
}

func TestCollector_MultipleReminders(t *testing.T) {
	c := NewCollector(
		DateReminder{},
		GitReminder{},
	)
	result := c.Collect(t.Context(), Context{
		IsFirstMessage: true,
		Now:            time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if !strings.Contains(result, "Sunday, June 1, 2025") {
		t.Errorf("expected date, got: %s", result)
	}
	if !strings.Contains(result, "Git") {
		t.Errorf("expected git info, got: %s", result)
	}
	if strings.Count(result, "<system-reminder>") != 1 {
		t.Errorf("expected one opening tag, got: %s", result)
	}
}

func TestWrapUserMessage_NoReminders(t *testing.T) {
	c := NewCollector()
	result := c.WrapUserMessage(t.Context(), "hello", Context{})
	if result != "hello" {
		t.Errorf("expected unchanged message, got: %s", result)
	}
}

func TestWrapUserMessage_WithReminders(t *testing.T) {
	c := NewCollector(DateReminder{})
	result := c.WrapUserMessage(t.Context(), "hello", Context{
		IsFirstMessage: true,
		Now:            time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if !strings.HasPrefix(result, "<system-reminder>") {
		t.Errorf("expected reminder at top, got: %s", result)
	}
	if !strings.HasSuffix(result, "hello") {
		t.Errorf("expected original message at end, got: %s", result)
	}
}

func TestGitReminder_FirstMessage(t *testing.T) {
	r := GitReminder{}
	lines := r.Generate(t.Context(), Context{
		IsFirstMessage: true,
		Now:            time.Date(2025, 7, 15, 0, 0, 0, 0, time.UTC),
	})
	if lines == nil {
		t.Fatal("expected lines in a git repo, got nil")
	}
	if len(lines) == 0 {
		t.Fatal("expected at least one line")
	}
	// First line should be branch or detached HEAD info
	if !strings.HasPrefix(lines[0], "Git branch:") && !strings.HasPrefix(lines[0], "Git HEAD:") {
		t.Errorf("expected git branch/HEAD line, got: %s", lines[0])
	}
	// There should be a git status line
	foundStatus := false
	for _, line := range lines {
		if strings.HasPrefix(line, "Git status:") {
			foundStatus = true
			break
		}
	}
	if !foundStatus {
		t.Errorf("expected 'Git status:' line, got: %v", lines)
	}
}

func TestGitReminder_NotFirstMessage(t *testing.T) {
	r := GitReminder{}
	lines := r.Generate(t.Context(), Context{IsFirstMessage: false})
	if lines != nil {
		t.Errorf("expected nil when not first message, got: %v", lines)
	}
}

func TestCollector_WithGitReminder(t *testing.T) {
	c := NewCollector(DateReminder{}, GitReminder{})
	result := c.Collect(t.Context(), Context{
		IsFirstMessage: true,
		Now:            time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if !strings.Contains(result, "<system-reminder>") {
		t.Errorf("expected <system-reminder> tag, got: %s", result)
	}
	if !strings.Contains(result, "Sunday, June 1, 2025") {
		t.Errorf("expected date, got: %s", result)
	}
	// Should contain git info since we're in a repo
	if !strings.Contains(result, "Git") {
		t.Errorf("expected git info, got: %s", result)
	}
	if strings.Count(result, "<system-reminder>") != 1 {
		t.Errorf("expected one opening tag, got: %s", result)
	}
}

// ---- SkillListReminder tests -----------------------------------------------

// mockSkillMetaProvider is a test stub that returns a fixed set of skills.
type mockSkillMetaProvider struct {
	metas []SkillMetaRecord
}

func (m *mockSkillMetaProvider) ListSkillMetas() []SkillMetaRecord {
	return m.metas
}

func TestSkillListReminder_FiresOnFirstMessage(t *testing.T) {
	r := NewSkillListReminder(&mockSkillMetaProvider{
		metas: []SkillMetaRecord{{Name: "test", Description: "a test skill"}},
	})
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line on first message, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `name="test"`) {
		t.Errorf("expected skill 'test' in output, got: %s", lines[0])
	}
	if r.dirty {
		t.Error("expected dirty to be false after firing")
	}
}

func TestSkillListReminder_SkipsOnNonFirstWhenClean(t *testing.T) {
	r := NewSkillListReminder(&mockSkillMetaProvider{
		metas: []SkillMetaRecord{{Name: "test", Description: "a test skill"}},
	})
	// First call: fires, clears dirty
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
	if len(lines) != 1 {
		t.Fatalf("first call should fire, got %d", len(lines))
	}
	// Second call: should skip
	lines = r.Generate(t.Context(), Context{IsFirstMessage: false})
	if len(lines) != 0 {
		t.Errorf("expected no output when not first message and clean, got %d lines", len(lines))
	}
}

func TestSkillListReminder_SkipsOnToolResult(t *testing.T) {
	r := NewSkillListReminder(&mockSkillMetaProvider{
		metas: []SkillMetaRecord{{Name: "test", Description: "a test skill"}},
	})
	lines := r.Generate(t.Context(), Context{IsFirstMessage: false, IsToolResult: true})
	if len(lines) != 0 {
		t.Errorf("expected no output on tool result, got %d lines", len(lines))
	}
}

func TestSkillListReminder_FiresWhenDirty(t *testing.T) {
	r := NewSkillListReminder(&mockSkillMetaProvider{
		metas: []SkillMetaRecord{{Name: "test", Description: "a test skill"}},
	})
	// First call clears dirty
	r.Generate(t.Context(), Context{IsFirstMessage: true})

	// Simulate skill_create: make dirty again
	r.dirty = true

	lines := r.Generate(t.Context(), Context{IsFirstMessage: false})
	if len(lines) != 1 {
		t.Fatalf("expected to fire when dirty, got %d lines", len(lines))
	}
	if !strings.Contains(lines[0], `name="test"`) {
		t.Errorf("expected skill 'test' in output, got: %s", lines[0])
	}
	if r.dirty {
		t.Error("expected dirty to be cleared after firing")
	}
}

func TestSkillListReminder_NilProvider(t *testing.T) {
	r := NewSkillListReminder(nil)
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
	if lines != nil {
		t.Errorf("expected nil from nil provider, got %v", lines)
	}
}

func TestSkillListReminder_EmptySkills(t *testing.T) {
	r := NewSkillListReminder(&mockSkillMetaProvider{})
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
	if lines != nil {
		t.Errorf("expected nil from empty skills, got %v", lines)
	}
}

// ---- All reminders share a single <system-reminder> block ---------------

// mockReminder is a test stub for exercising the collector.
type mockReminder struct {
	content []string
}

func (m *mockReminder) Generate(_ context.Context, _ Context) []string { return m.content }

func TestCollector_MergesAllRemindersIntoOneSystemReminderBlock(t *testing.T) {
	c := NewCollector(
		DateReminder{},
		&mockReminder{content: []string{"memory 1"}},
		&mockReminder{content: []string{"deferred tools hint"}},
	)
	result := c.Collect(t.Context(), Context{
		IsFirstMessage: true,
		Now:            time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	})

	// Exactly one wrapper block; every reminder's output lives inside it.
	if strings.Count(result, "<system-reminder>") != 1 {
		t.Errorf("expected exactly one <system-reminder> open tag, got: %s", result)
	}
	if strings.Count(result, "</system-reminder>") != 1 {
		t.Errorf("expected exactly one </system-reminder> close tag, got: %s", result)
	}
	for _, want := range []string{"Sunday, June 1, 2025", "memory 1", "deferred tools hint"} {
		if !strings.Contains(result, want) {
			t.Errorf("expected %q inside the block, got: %s", want, result)
		}
	}
	// No legacy per-reminder tags may leak out.
	for _, legacy := range []string{
		"<relevant-memories>", "<available-skills>",
		"<available-deferred-tools>", "<lsp-diagnostics>",
	} {
		if strings.Contains(result, legacy) {
			t.Errorf("legacy tag %q must not appear, got: %s", legacy, result)
		}
	}
}

func TestCollector_EmptyGenerate_NoBlock(t *testing.T) {
	c := NewCollector(
		&mockReminder{content: nil},
	)
	result := c.Collect(t.Context(), Context{IsFirstMessage: true})
	if result != "" {
		t.Errorf("expected empty when no reminders fire, got: %s", result)
	}
}
