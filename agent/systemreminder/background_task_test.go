package systemreminder

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/tasks"
)

func bgQueue(events ...tasks.Event) *tasks.Queue {
	q := tasks.New()
	for _, e := range events {
		q.Post(e)
	}
	return q
}

func bgEvent(name, command string, exitCode int, stdout, stderr string) tasks.Event {
	return tasks.Event{
		ID: name, Kind: tasks.KindBackground, Name: name, Command: command,
		Status: tasks.StatusExited, ExitCode: exitCode, Stdout: stdout, Stderr: stderr,
	}
}

func TestBackgroundTaskReminder_SkipsOnFirstMessage(t *testing.T) {
	q := bgQueue(bgEvent("build", "make build", 0, "", ""))
	lines := (&BackgroundTaskReminder{Queue: q}).Generate(t.Context(), Context{IsFirstMessage: true})
	if len(lines) != 0 {
		t.Errorf("expected empty output on first message, got %d lines", len(lines))
	}
	if q.Len() != 1 {
		t.Errorf("a skipped collection must not consume the event, pending = %d", q.Len())
	}
}

// Taking is destructive: the queue is the single copy of the news, and the two
// consumers (this reminder and the idle wake) must not both report it.
func TestBackgroundTaskReminder_TakesTheEventOnce(t *testing.T) {
	q := bgQueue(bgEvent("build", "make build", 0, "", ""))
	r := &BackgroundTaskReminder{Queue: q}
	if lines := r.Generate(t.Context(), Context{}); len(lines) != 1 {
		t.Fatalf("first collection should report the task, got %d lines", len(lines))
	}
	if lines := r.Generate(t.Context(), Context{}); len(lines) != 0 {
		t.Fatalf("second collection must be silent, got %d lines", len(lines))
	}
}

func TestBackgroundTaskReminder_NilQueue(t *testing.T) {
	lines := (&BackgroundTaskReminder{Queue: nil}).Generate(t.Context(), Context{IsFirstMessage: false})
	if len(lines) != 0 {
		t.Errorf("expected empty output with nil queue, got %d lines", len(lines))
	}
}

func TestBackgroundTaskReminder_NoCompletedTasks(t *testing.T) {
	lines := (&BackgroundTaskReminder{Queue: tasks.New()}).Generate(t.Context(), Context{IsFirstMessage: false})
	if len(lines) != 0 {
		t.Errorf("expected empty output when no tasks complete, got %d lines", len(lines))
	}
}

func TestBackgroundTaskReminder_SuccessfulTask(t *testing.T) {
	r := &BackgroundTaskReminder{Queue: bgQueue(
		bgEvent("build", "make build", 0, "", ""),
	)}
	lines := r.Generate(t.Context(), Context{IsFirstMessage: false})
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
	r := &BackgroundTaskReminder{Queue: bgQueue(
		bgEvent("build", "make build", 0, "compiled successfully\nbinary: ./tachi", ""),
	)}
	lines := r.Generate(t.Context(), Context{IsFirstMessage: false})
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
	r := &BackgroundTaskReminder{Queue: bgQueue(
		bgEvent("test", "go test ./...", 1, "", "TestFoo failed\npanic: oops"),
	)}
	lines := r.Generate(t.Context(), Context{IsFirstMessage: false})
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
	r := &BackgroundTaskReminder{Queue: bgQueue(
		bgEvent("lint", "golangci-lint run", 0, "", ""),
		bgEvent("test", "go test ./...", 1, "", ""),
	)}
	lines := r.Generate(t.Context(), Context{IsFirstMessage: false})
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
	r := &BackgroundTaskReminder{Queue: bgQueue(
		bgEvent("build", "make", 0, "", ""),
	)}
	lines := r.Generate(t.Context(), Context{IsFirstMessage: false, IsToolResult: true})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line on tool result, got %d", len(lines))
	}
}

func TestBackgroundTaskReminder_TailTruncation(t *testing.T) {
	// Generate content larger than maxOutputSnippet
	big := strings.Repeat("line of output\n", 800) // ~14KB
	r := &BackgroundTaskReminder{Queue: bgQueue(
		bgEvent("big-output", "make", 0, big, ""),
	)}
	lines := r.Generate(t.Context(), Context{IsFirstMessage: false})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "(truncated, showing tail 10.0 KB)") {
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
	if !strings.Contains(result, "(truncated, showing tail 20 B)") {
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

// A remote task parked on a question is not "finished", and the model must be
// told which question an answer has to name — otherwise the reply tool refuses it.
func TestTaskLines_RendersAWaitingRemoteTask(t *testing.T) {
	q := bgQueue(tasks.Event{
		ID: "d_1a2b", Kind: tasks.KindRemote, Name: "d_1a2b",
		Command: "迁移数据", Node: "nas", Status: tasks.StatusWaiting,
		QuestionID: "q_7", Stdout: "哪个目录？",
	})
	lines := (&BackgroundTaskReminder{Queue: q}).Generate(t.Context(), Context{})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	for _, want := range []string{
		"waiting for an answer", "question_id=q_7", `RemoteTask(action="reply"`,
		"Node: nas", "Task: 迁移数据", "哪个目录？",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("waiting remote task should mention %q, got:\n%s", want, lines[0])
		}
	}
}
