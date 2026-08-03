package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestBashTool_Execute(t *testing.T) {
	tool := BashTool{}

	t.Run("simple echo", func(t *testing.T) {
		result, err := tool.ExecuteContext(context.TODO(), `{"command": "echo hello"}`)
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
		result, err := tool.ExecuteContext(context.TODO(), `{"command": "echo err >&2"}`)
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
		result, err := tool.ExecuteContext(context.TODO(), `{"command": "exit 42"}`)
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
		result, err := tool.ExecuteContext(context.TODO(), `{"command": "sleep 10", "timeout": 500}`)
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
		_, err := tool.ExecuteContext(context.TODO(), `{"command": ""}`)
		if err == nil {
			t.Error("expected error for empty command")
		}
	})

	t.Run("multiline output", func(t *testing.T) {
		result, err := tool.ExecuteContext(context.TODO(), `{"command": "printf 'a\nb\nc'"}`)
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
		result, err := tool.ExecuteContext(context.TODO(), `{"command": "pwd"}`)
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

func TestBashTool_SpillOversizedOutput(t *testing.T) {
	tmpDir := t.TempDir()
	tool := NewBashTool(BashToolConfig{
		ResultBaseDir:  tmpDir,
		MaxResultChars: 100,
	})

	// ~5KB of output — far beyond the 100-char limit but well under the 1MB buffer cap.
	result, err := tool.ExecuteContext(context.TODO(), `{"command": "seq 1 1000"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var r BashResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if !strings.Contains(r.Stdout, "[BASH OUTPUT TOO LARGE]") {
		t.Fatalf("expected TOO LARGE marker, got stdout:\n%.300s", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "bash_out_") {
		t.Fatalf("expected spill file path in message, got:\n%.300s", r.Stdout)
	}
	if !r.Truncated {
		t.Error("expected Truncated=true after spill")
	}

	// The spill file must exist on disk and hold the full output.
	path := tmpDir + "/" + spillFileNameFrom(r.Stdout)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill file not readable: %v", err)
	}
	if !strings.Contains(string(data), "1000") {
		t.Errorf("spill file should contain the full output end; got %.200s", data)
	}
}

func TestBashTool_NoSpillWhenDisabled(t *testing.T) {
	// Default tool (no ResultBaseDir / MaxResultChars): output returns inline,
	// no spill file is written.
	tool := BashTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"command": "seq 1 1000"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var r BashResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if strings.Contains(r.Stdout, "[BASH OUTPUT TOO LARGE]") {
		t.Error("unexpected spill marker with spill disabled")
	}
	if !strings.Contains(r.Stdout, "1000") {
		t.Errorf("expected inline stdout, got %.200s", r.Stdout)
	}
}

// spillFileNameFrom extracts the spill filename from a TOO LARGE message.
func spillFileNameFrom(msg string) string {
	const marker = "bash_out_"
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	end := strings.IndexAny(msg[i:], " \n")
	if end < 0 {
		return msg[i:]
	}
	return msg[i : i+end]
}
