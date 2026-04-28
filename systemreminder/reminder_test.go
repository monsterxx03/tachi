package systemreminder

import (
	"strings"
	"testing"
	"time"
)

func TestDateReminder_FirstMessage(t *testing.T) {
	r := DateReminder{}
	lines := r.Generate(Context{
		IsFirstMessage: true,
		Now:            time.Date(2025, 7, 15, 0, 0, 0, 0, time.UTC),
	})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "Tuesday, July 15, 2025") {
		t.Errorf("expected date in output, got: %s", lines[0])
	}
}

func TestDateReminder_NotFirstMessage(t *testing.T) {
	r := DateReminder{}
	lines := r.Generate(Context{IsFirstMessage: false})
	if lines != nil {
		t.Errorf("expected nil, got %v", lines)
	}
}

func TestIterationWarningReminder_Low(t *testing.T) {
	r := IterationWarningReminder{}
	lines := r.Generate(Context{
		IterationsLeft: 2,
		MaxIterations:  10,
	})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "2 of 10") {
		t.Errorf("expected budget info, got: %s", lines[0])
	}
}

func TestIterationWarningReminder_High(t *testing.T) {
	r := IterationWarningReminder{}
	lines := r.Generate(Context{
		IterationsLeft: 5,
		MaxIterations:  10,
	})
	if len(lines) != 0 {
		t.Errorf("expected no warning when >2 iterations left, got: %v", lines)
	}
}

func TestIterationWarningReminder_ZeroLeft(t *testing.T) {
	r := IterationWarningReminder{}
	lines := r.Generate(Context{
		IterationsLeft: 0,
		MaxIterations:  10,
	})
	if len(lines) != 0 {
		t.Errorf("expected no warning when 0 left, got: %v", lines)
	}
}

func TestTokenWarningReminder_HighUsage(t *testing.T) {
	r := TokenWarningReminder{}
	lines := r.Generate(Context{
		InputTokens:   80000,
		ContextWindow: 128000,
	})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "62%") || !strings.Contains(lines[0], "80000") {
		t.Errorf("unexpected output: %s", lines[0])
	}
}

func TestTokenWarningReminder_LowUsage(t *testing.T) {
	r := TokenWarningReminder{}
	lines := r.Generate(Context{
		InputTokens:   60000,
		ContextWindow: 128000,
	})
	if len(lines) != 0 {
		t.Errorf("expected no warning under 60%%, got: %v", lines)
	}
}

func TestCollector_Empty(t *testing.T) {
	c := NewCollector(DateReminder{}, IterationWarningReminder{}, TokenWarningReminder{})
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
	c := NewCollector(DateReminder{}, IterationWarningReminder{})
	result := c.Collect(Context{
		IsFirstMessage:  true,
		IterationsLeft:  1,
		MaxIterations:   10,
		Now:             time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if !strings.Contains(result, "Sunday, June 1, 2025") {
		t.Errorf("expected date, got: %s", result)
	}
	if !strings.Contains(result, "1 of 10") {
		t.Errorf("expected iteration warning, got: %s", result)
	}
	// Verify the block is wrapped correctly
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
