package systemreminder

import (
	"strings"
	"testing"
	"time"
)

func TestDateReminder_Fires(t *testing.T) {
	r := DateReminder{}
	lines := r.Generate(Context{
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
	lines := r.Generate(Context{IsFirstMessage: false})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line (always fires), got %d", len(lines))
	}
}

func TestDateReminder_SkipsOnToolResult(t *testing.T) {
	r := DateReminder{}
	lines := r.Generate(Context{
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
	lines := r.Generate(Context{
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

func TestIterationWarningReminder_Fires(t *testing.T) {
	r := IterationWarningReminder{Threshold: 5}
	lines := r.Generate(Context{
		IterationsLeft: 5,
		MaxIterations:  10,
	})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "5 of 10") {
		t.Errorf("expected budget info, got: %s", lines[0])
	}
}

func TestIterationWarningReminder_AboveThreshold(t *testing.T) {
	r := IterationWarningReminder{Threshold: 5}
	lines := r.Generate(Context{
		IterationsLeft: 6,
		MaxIterations:  10,
	})
	if len(lines) != 0 {
		t.Errorf("expected no warning when above threshold, got: %v", lines)
	}
}

func TestIterationWarningReminder_ZeroLeft(t *testing.T) {
	r := IterationWarningReminder{Threshold: 5}
	lines := r.Generate(Context{
		IterationsLeft: 0,
		MaxIterations:  10,
	})
	if len(lines) != 0 {
		t.Errorf("expected no warning when 0 left, got: %v", lines)
	}
}

func TestIterationWarningReminder_ZeroThreshold(t *testing.T) {
	r := IterationWarningReminder{Threshold: 0}
	lines := r.Generate(Context{
		IterationsLeft: 1,
		MaxIterations:  10,
	})
	if len(lines) != 0 {
		t.Errorf("expected no warning when threshold is 0, got: %v", lines)
	}
}

func TestTokenWarningReminder_Fires(t *testing.T) {
	r := TokenWarningReminder{ThresholdPct: 80}
	lines := r.Generate(Context{
		InputTokens:   110000,
		ContextWindow: 128000,
	})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "86%") || !strings.Contains(lines[0], "110000") {
		t.Errorf("unexpected output: %s", lines[0])
	}
}

func TestTokenWarningReminder_BelowThreshold(t *testing.T) {
	r := TokenWarningReminder{ThresholdPct: 80}
	lines := r.Generate(Context{
		InputTokens:   100000,
		ContextWindow: 128000,
	})
	if len(lines) != 0 {
		t.Errorf("expected no warning under threshold, got: %v", lines)
	}
}

func TestTokenWarningReminder_ZeroThreshold(t *testing.T) {
	r := TokenWarningReminder{ThresholdPct: 0}
	lines := r.Generate(Context{
		InputTokens:   110000,
		ContextWindow: 128000,
	})
	if len(lines) != 0 {
		t.Errorf("expected no warning when threshold is 0, got: %v", lines)
	}
}

func TestCollector_Empty(t *testing.T) {
	c := NewCollector(
		IterationWarningReminder{Threshold: 5},
		TokenWarningReminder{ThresholdPct: 80},
	)
	result := c.Collect(Context{
		IsFirstMessage:  false,
		IterationsLeft:  10,
		MaxIterations:   10,
		InputTokens:     1000,
		ContextWindow:   128000,
	})
	if result != "" {
		t.Errorf("expected empty, got: %s", result)
	}
}

func TestCollector_FirstMessage(t *testing.T) {
	c := NewCollector(DateReminder{})
	result := c.Collect(Context{
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
		IterationWarningReminder{Threshold: 5},
	)
	result := c.Collect(Context{
		IsFirstMessage:  true,
		IterationsLeft:  5,
		MaxIterations:   10,
		Now:              time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if !strings.Contains(result, "Sunday, June 1, 2025") {
		t.Errorf("expected date, got: %s", result)
	}
	if !strings.Contains(result, "5 of 10") {
		t.Errorf("expected iteration warning, got: %s", result)
	}
	if strings.Count(result, "<system-reminder>") != 1 {
		t.Errorf("expected one opening tag, got: %s", result)
	}
}

func TestWrapUserMessage_NoReminders(t *testing.T) {
	c := NewCollector()
	result := c.WrapUserMessage("hello", Context{})
	if result != "hello" {
		t.Errorf("expected unchanged message, got: %s", result)
	}
}

func TestWrapUserMessage_WithReminders(t *testing.T) {
	c := NewCollector(DateReminder{})
	result := c.WrapUserMessage("hello", Context{
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
	lines := r.Generate(Context{
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
	lines := r.Generate(Context{IsFirstMessage: false})
	if lines != nil {
		t.Errorf("expected nil when not first message, got: %v", lines)
	}
}

func TestCollector_WithGitReminder(t *testing.T) {
	c := NewCollector(DateReminder{}, GitReminder{})
	result := c.Collect(Context{
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
	lines := r.Generate(Context{IsFirstMessage: true})
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
	lines := r.Generate(Context{IsFirstMessage: true})
	if len(lines) != 1 {
		t.Fatalf("first call should fire, got %d", len(lines))
	}
	// Second call: should skip
	lines = r.Generate(Context{IsFirstMessage: false})
	if len(lines) != 0 {
		t.Errorf("expected no output when not first message and clean, got %d lines", len(lines))
	}
}

func TestSkillListReminder_SkipsOnToolResult(t *testing.T) {
	r := NewSkillListReminder(&mockSkillMetaProvider{
		metas: []SkillMetaRecord{{Name: "test", Description: "a test skill"}},
	})
	lines := r.Generate(Context{IsFirstMessage: false, IsToolResult: true})
	if len(lines) != 0 {
		t.Errorf("expected no output on tool result, got %d lines", len(lines))
	}
}

func TestSkillListReminder_FiresWhenDirty(t *testing.T) {
	r := NewSkillListReminder(&mockSkillMetaProvider{
		metas: []SkillMetaRecord{{Name: "test", Description: "a test skill"}},
	})
	// First call clears dirty
	r.Generate(Context{IsFirstMessage: true})

	// Simulate skill_create: make dirty again
	r.dirty = true

	lines := r.Generate(Context{IsFirstMessage: false})
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
	lines := r.Generate(Context{IsFirstMessage: true})
	if lines != nil {
		t.Errorf("expected nil from nil provider, got %v", lines)
	}
}

func TestSkillListReminder_EmptySkills(t *testing.T) {
	r := NewSkillListReminder(&mockSkillMetaProvider{})
	lines := r.Generate(Context{IsFirstMessage: true})
	if lines != nil {
		t.Errorf("expected nil from empty skills, got %v", lines)
	}
}

// ---- TaggedReminder tests ---------------------------------------------------

// mockTaggedReminder is a test stub that implements both Reminder and TaggedReminder.
type mockTaggedReminder struct {
	tag     string
	content []string
}

func (m *mockTaggedReminder) Generate(_ Context) []string { return m.content }
func (m *mockTaggedReminder) WrapperTag() string          { return m.tag }

func TestTaggedReminder_WrappedInOwnTag(t *testing.T) {
	c := NewCollector(
		&mockTaggedReminder{tag: "relevant-memories", content: []string{"memory 1", "memory 2"}},
	)
	result := c.Collect(Context{IsFirstMessage: true})
	if !strings.Contains(result, "<relevant-memories>") {
		t.Errorf("expected <relevant-memories> tag, got: %s", result)
	}
	if !strings.Contains(result, "</relevant-memories>") {
		t.Errorf("expected </relevant-memories> tag, got: %s", result)
	}
	if strings.Contains(result, "<system-reminder>") {
		t.Errorf("expected no <system-reminder> tag for tagged-only reminders, got: %s", result)
	}
	if !strings.Contains(result, "memory 1") || !strings.Contains(result, "memory 2") {
		t.Errorf("expected memory content, got: %s", result)
	}
}

func TestTaggedReminder_MixedWithDefault(t *testing.T) {
	c := NewCollector(
		DateReminder{},
		&mockTaggedReminder{tag: "relevant-memories", content: []string{"memory 1"}},
	)
	result := c.Collect(Context{
		IsFirstMessage:  true,
		Now:              time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	// Should have both blocks
	if !strings.Contains(result, "<system-reminder>") {
		t.Errorf("expected <system-reminder> tag, got: %s", result)
	}
	if !strings.Contains(result, "<relevant-memories>") {
		t.Errorf("expected <relevant-memories> tag, got: %s", result)
	}
	// <relevant-memories> should come after <system-reminder>
	sysIdx := strings.Index(result, "<system-reminder>")
	memIdx := strings.Index(result, "<relevant-memories>")
	if sysIdx < 0 || memIdx < 0 || memIdx <= sysIdx {
		t.Errorf("expected <system-reminder> before <relevant-memories>, got: %s", result)
	}
	if !strings.Contains(result, "Sunday, June 1, 2025") {
		t.Errorf("expected date in system-reminder, got: %s", result)
	}
	if !strings.Contains(result, "memory 1") {
		t.Errorf("expected memory in relevant-memories, got: %s", result)
	}
}

func TestTaggedReminder_EmptyGenerate_NoBlock(t *testing.T) {
	c := NewCollector(
		&mockTaggedReminder{tag: "relevant-memories", content: nil},
	)
	result := c.Collect(Context{IsFirstMessage: true})
	if result != "" {
		t.Errorf("expected empty when no reminders fire, got: %s", result)
	}
}
