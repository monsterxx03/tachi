package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBashTool_Execute(t *testing.T) {
	tool := BashTool{}

	t.Run("simple echo", func(t *testing.T) {
		result, err := tool.Execute(`{"command": "echo hello"}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var r BashResult
		if err := json.Unmarshal([]byte(result), &r); err != nil {
			t.Fatalf("failed to parse result: %v", err)
		}
		if r.Stdout != "hello" {
			t.Errorf("expected stdout 'hello', got %q", r.Stdout)
		}
		if r.ExitCode != 0 {
			t.Errorf("expected exit code 0, got %d", r.ExitCode)
		}
	})

	t.Run("command with stderr", func(t *testing.T) {
		result, err := tool.Execute(`{"command": "echo err >&2"}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var r BashResult
		json.Unmarshal([]byte(result), &r)
		if r.Stderr != "err" {
			t.Errorf("expected stderr 'err', got %q", r.Stderr)
		}
	})

	t.Run("non-zero exit code", func(t *testing.T) {
		result, err := tool.Execute(`{"command": "exit 42"}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var r BashResult
		json.Unmarshal([]byte(result), &r)
		if r.ExitCode != 42 {
			t.Errorf("expected exit code 42, got %d", r.ExitCode)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		result, err := tool.Execute(`{"command": "sleep 10", "timeout": 500}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var r BashResult
		json.Unmarshal([]byte(result), &r)
		if !r.Interrupted {
			t.Error("expected command to be interrupted")
		}
		if r.ExitCode != -1 {
			t.Errorf("expected exit code -1, got %d", r.ExitCode)
		}
	})

	t.Run("empty command", func(t *testing.T) {
		_, err := tool.Execute(`{"command": ""}`)
		if err == nil {
			t.Error("expected error for empty command")
		}
	})

	t.Run("multiline output", func(t *testing.T) {
		result, err := tool.Execute(`{"command": "printf 'a\nb\nc'"}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var r BashResult
		json.Unmarshal([]byte(result), &r)
		lines := strings.Split(r.Stdout, "\n")
		if len(lines) != 3 {
			t.Errorf("expected 3 lines, got %d", len(lines))
		}
	})

	t.Run("working directory", func(t *testing.T) {
		result, err := tool.Execute(`{"command": "pwd"}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var r BashResult
		json.Unmarshal([]byte(result), &r)
		if r.Stdout == "" {
			t.Error("expected non-empty pwd output")
		}
	})
}
