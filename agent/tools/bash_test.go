package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
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

func TestBashTool_AutoBackground(t *testing.T) {
	pm := NewProcessManager()
	tool := BashTool{processManager: pm, foregroundWindow: 200 * time.Millisecond}

	result, err := tool.ExecuteContext(context.TODO(), `{"command": "sleep 1; echo done"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var r BashResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if !r.Backgrounded {
		t.Fatalf("expected backgrounded=true for slow command, got %+v", r)
	}
	if r.BgName == "" {
		t.Fatal("expected bgName to be set")
	}
	if !strings.Contains(r.Stderr, "continuing in background") {
		t.Errorf("expected background hint in stderr, got: %s", r.Stderr)
	}

	// The adopted process must be tracked and stoppable.
	infos := pm.List()
	if len(infos) != 1 || infos[0].Name != r.BgName {
		t.Fatalf("expected adopted process %q in manager, got %+v", r.BgName, infos)
	}

	if _, err := pm.Stop(r.BgName); err != nil {
		t.Fatalf("stop adopted process failed: %v", err)
	}
}

func TestBashTool_AutoBackgroundDisabledWithoutManager(t *testing.T) {
	// No ProcessManager: legacy timeout behavior (kill + interrupted).
	tool := BashTool{foregroundWindow: 200 * time.Millisecond}

	result, err := tool.ExecuteContext(context.TODO(), `{"command": "sleep 1; echo done"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var r BashResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if r.Backgrounded {
		t.Fatal("expected no backgrounding without a ProcessManager")
	}
	if !r.Interrupted || !strings.Contains(r.Stderr, "timed out") {
		t.Errorf("expected legacy timeout result, got %+v", r)
	}
}

func TestBashTool_FastCommandNotBackgrounded(t *testing.T) {
	tool := BashTool{processManager: NewProcessManager(), foregroundWindow: 5 * time.Second}

	result, err := tool.ExecuteContext(context.TODO(), `{"command": "echo hello"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var r BashResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if r.Backgrounded {
		t.Fatalf("fast command must not be backgrounded, got %+v", r)
	}
	if r.Stdout != "hello" || r.ExitCode != 0 {
		t.Errorf("expected stdout 'hello' exit 0, got %+v", r)
	}
}

// TestBashTool_RequiredFor pins the argument-dependent half of the schema (see
// ArgRequirements): `list_bg` and `stop_name` are CONTROL calls that carry no command, and the
// tool's own description is what tells the model to make them ("call this tool again with the
// list_bg parameter set to true"). A flat required list answered those calls with
// "missing required argument 'command'" — the model then burned a round trip re-sending one.
func TestBashTool_RequiredFor(t *testing.T) {
	tool := NewBashTool(BashToolConfig{ProcessManager: NewProcessManager()})

	for _, args := range []string{`{"list_bg": true}`, `{"stop_name": "dev-server"}`} {
		if err := validateArgs(tool, args); err != nil {
			t.Fatalf("expected the control call %s to validate, got %v", args, err)
		}
	}
	// Everything that STARTS or RUNS something still needs a command — background mode
	// included, which executeLocal checks again on its own.
	for _, args := range []string{`{}`, `{"timeout": 1000}`, `{"background": true, "bg_name": "x"}`} {
		var missing *MissingArgError
		if err := validateArgs(tool, args); !errors.As(err, &missing) || missing.Arg != "command" {
			t.Fatalf("expected a missing-command error for %s, got %v", args, err)
		}
	}
}

// The model-facing schema keeps advertising `command` on purpose: the tool list rides in every
// request, so changing it invalidates each session's prompt prefix. The relaxation belongs to
// the validator alone, which is why this pins the schema instead of the validator.
func TestBashTool_SchemaStillAdvertisesCommand(t *testing.T) {
	required := ToSchema(NewBashTool(BashToolConfig{ProcessManager: NewProcessManager()})).Parameters.Required
	if len(required) != 1 || required[0] != "command" {
		t.Fatalf("the model-facing schema must keep advertising exactly [command], got %v", required)
	}
}

// End to end through the registry — the path a model's call actually takes: the control call
// reaches the tool and is ANSWERED (an empty process list is the answer, not an error), rather
// than being refused at the door.
func TestBashTool_ListBgThroughRegistry(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewBashTool(BashToolConfig{ProcessManager: NewProcessManager()}))

	tr := reg.Invoke(context.TODO(), ToolNameBash, `{"list_bg": true}`)
	if tr.Status != ToolResultSuccess {
		t.Fatalf("expected list_bg to succeed, got %v (%v)", tr.Status, tr.Err)
	}
}
