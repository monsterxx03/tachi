package systemreminder

import (
	"strings"
	"testing"
)

type stubRunningTasks struct {
	items []RunningTask
}

func (s *stubRunningTasks) RunningTasks(string) []RunningTask { return s.items }

// The set is told once and stays silent until it changes: a process that runs
// for an hour must not cost a block on every turn.
func TestRunningTasksSpeaksOnlyOnChange(t *testing.T) {
	p := &stubRunningTasks{items: []RunningTask{{Name: "build", Command: "make build"}}}
	r := &RunningTasksReminder{Provider: p}

	first := r.Generate(t.Context(), Context{LogicalConversation: "conv"})
	if len(first) != 1 {
		t.Fatalf("first telling = %d lines, want 1", len(first))
	}
	if !strings.Contains(first[0], `"build"`) || !strings.Contains(first[0], "wait_name") {
		t.Errorf("the block must name the process and how to act on it, got:\n%s", first[0])
	}

	if again := r.Generate(t.Context(), Context{LogicalConversation: "conv"}); len(again) != 0 {
		t.Errorf("an unchanged set must stay silent, got %v", again)
	}

	// A second process is a change.
	p.items = append(p.items, RunningTask{Name: "server", Command: "./dev-server"})
	changed := r.Generate(t.Context(), Context{LogicalConversation: "conv"})
	if len(changed) != 1 || !strings.Contains(changed[0], `"server"`) {
		t.Fatalf("a changed set must be told, got %v", changed)
	}

	// Nothing running: silent, and the memory is cleared so the next start speaks.
	p.items = nil
	if got := r.Generate(t.Context(), Context{LogicalConversation: "conv"}); len(got) != 0 {
		t.Errorf("an empty set must be silent, got %v", got)
	}
	p.items = []RunningTask{{Name: "again", Command: "true"}}
	if got := r.Generate(t.Context(), Context{LogicalConversation: "conv"}); len(got) != 1 {
		t.Errorf("a restart after an empty set must be told, got %v", got)
	}
}

// Two conversations do not share the memory: one conversation's silence must not
// hide another's first telling.
func TestRunningTasksAreScopedToTheConversation(t *testing.T) {
	p := &stubRunningTasks{items: []RunningTask{{Name: "build", Command: "make"}}}
	r := &RunningTasksReminder{Provider: p}

	if got := r.Generate(t.Context(), Context{LogicalConversation: "a"}); len(got) != 1 {
		t.Fatalf("conversation a should be told, got %v", got)
	}
	if got := r.Generate(t.Context(), Context{LogicalConversation: "b"}); len(got) != 1 {
		t.Fatalf("conversation b has its own first telling, got %v", got)
	}
}

func TestRunningTasksNilProvider(t *testing.T) {
	r := &RunningTasksReminder{}
	if got := r.Generate(t.Context(), Context{}); len(got) != 0 {
		t.Errorf("nil provider must be silent, got %v", got)
	}
}
