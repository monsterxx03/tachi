package systemreminder

import (
	"strings"
	"testing"
)

func TestBackgroundTaskReminder_SkipsOnFirstMessage(t *testing.T) {
	r := &BackgroundTaskReminder{Provider: &mockBgProvider{nil}}
	lines := r.Generate(Context{IsFirstMessage: true})
	if len(lines) != 0 {
		t.Errorf("expected empty output on first message, got %d lines", len(lines))
	}
}

func TestBackgroundTaskReminder_NilProvider(t *testing.T) {
	r := &BackgroundTaskReminder{Provider: nil}
	lines := r.Generate(Context{IsFirstMessage: false})
	if len(lines) != 0 {
		t.Errorf("expected empty output with nil provider, got %d lines", len(lines))
	}
}

func TestBackgroundTaskReminder_NoCompletedTasks(t *testing.T) {
	r := &BackgroundTaskReminder{Provider: &mockBgProvider{nil}}
	lines := r.Generate(Context{IsFirstMessage: false})
	if len(lines) != 0 {
		t.Errorf("expected empty output when no tasks complete, got %d lines", len(lines))
	}
}

func TestBackgroundTaskReminder_SuccessfulTask(t *testing.T) {
	r := &BackgroundTaskReminder{Provider: &mockBgProvider{
		items: []BackgroundTaskInfo{
			{Name: "build", Command: "make build", ExitCode: 0, Status: "exited"},
		},
	}}
	lines := r.Generate(Context{IsFirstMessage: false})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"build"`) {
		t.Errorf("expected task name 'build', got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "successfully") {
		t.Errorf("expected success message, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "Command: make build") {
		t.Errorf("expected command line, got: %s", lines[0])
	}
}

func TestBackgroundTaskReminder_SuccessfulTaskWithOutput(t *testing.T) {
	r := &BackgroundTaskReminder{Provider: &mockBgProvider{
		items: []BackgroundTaskInfo{
			{Name: "build", Command: "make build", ExitCode: 0, Status: "exited",
				RecentStdout: "compiled successfully\nbinary: ./tachi"},
		},
	}}
	lines := r.Generate(Context{IsFirstMessage: false})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "Output:") {
		t.Errorf("expected output section, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "compiled successfully") {
		t.Errorf("expected stdout content, got: %s", lines[0])
	}
}

func TestBackgroundTaskReminder_FailedTask(t *testing.T) {
	r := &BackgroundTaskReminder{Provider: &mockBgProvider{
		items: []BackgroundTaskInfo{
			{Name: "test", Command: "go test ./...", ExitCode: 1, Status: "exited",
				RecentStderr: "TestFoo failed\npanic: oops"},
		},
	}}
	lines := r.Generate(Context{IsFirstMessage: false})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "exit code 1") {
		t.Errorf("expected exit code info, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "Output:") {
		t.Errorf("expected output section for stderr on failure, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "TestFoo failed") {
		t.Errorf("expected stderr content, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "Command: go test ./...") {
		t.Errorf("expected command line, got: %s", lines[0])
	}
}

func TestBackgroundTaskReminder_MultipleTasks(t *testing.T) {
	r := &BackgroundTaskReminder{Provider: &mockBgProvider{
		items: []BackgroundTaskInfo{
			{Name: "lint", Command: "golangci-lint run", ExitCode: 0, Status: "exited"},
			{Name: "test", Command: "go test ./...", ExitCode: 1, Status: "exited"},
		},
	}}
	lines := r.Generate(Context{IsFirstMessage: false})
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "lint") {
		t.Errorf("expected first task 'lint', got: %s", lines[0])
	}
	if !strings.Contains(lines[1], "test") {
		t.Errorf("expected second task 'test', got: %s", lines[1])
	}
}

func TestBackgroundTaskReminder_FiresOnToolResult(t *testing.T) {
	r := &BackgroundTaskReminder{Provider: &mockBgProvider{
		items: []BackgroundTaskInfo{
			{Name: "build", Command: "make", ExitCode: 0, Status: "exited"},
		},
	}}
	lines := r.Generate(Context{IsFirstMessage: false, IsToolResult: true})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line on tool result, got %d", len(lines))
	}
}

func TestBackgroundTaskReminder_TailTruncation(t *testing.T) {
	// Generate content larger than maxOutputSnippet
	big := strings.Repeat("line of output\n", 800) // ~14KB
	r := &BackgroundTaskReminder{Provider: &mockBgProvider{
		items: []BackgroundTaskInfo{
			{Name: "big-output", Command: "make", ExitCode: 0, Status: "exited",
				RecentStdout: big},
		},
	}}
	lines := r.Generate(Context{IsFirstMessage: false})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "(truncated, showing tail 10KB)") {
		t.Errorf("expected truncation notice, got: %s", lines[0])
	}
	// Should end with the last lines of output
	if !strings.Contains(lines[0], "line of output") {
		t.Errorf("expected output content after truncation, got: %s", lines[0])
	}
}

func TestTailSnippet_NoTruncation(t *testing.T) {
	s := "hello\nworld"
	result := tailSnippet(s, 100)
	if result != s {
		t.Errorf("expected unchanged string, got: %s", result)
	}
}

func TestTailSnippet_Truncation(t *testing.T) {
	s := "a\n" + strings.Repeat("x", 100) + "\nend"
	result := tailSnippet(s, 20)
	if !strings.Contains(result, "(truncated, showing tail 20B)") {
		t.Errorf("expected truncation notice, got: %s", result)
	}
	if !strings.HasSuffix(result, "end") {
		t.Errorf("expected tail to contain 'end', got: %s", result)
	}
}

func TestIndent(t *testing.T) {
	result := indent("hello\nworld", "  ")
	expected := "  hello\n  world"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestIndent_Empty(t *testing.T) {
	result := indent("", "  ")
	if result != "" {
		t.Errorf("expected empty, got: %q", result)
	}
}

// -- mocks --

type mockBgProvider struct {
	items []BackgroundTaskInfo
}

func (m *mockBgProvider) DrainCompleted() []BackgroundTaskInfo {
	if m.items == nil {
		return nil
	}
	return m.items
}
