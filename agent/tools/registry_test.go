package tools

import (
	"context"
	"os"
	"testing"
)

func TestWriteTool(t *testing.T) {
	tool := WriteTool{}
	content := "Test content"
	_, err := tool.ExecuteContext(nil,`{"path": "/tmp/test_write.txt", "content": "Test content"}`)
	if err != nil {
		t.Fatalf("WriteTool.Execute failed: %v", err)
	}
	defer os.Remove("/tmp/test_write.txt")

	// Verify file was written
	data, err := os.ReadFile("/tmp/test_write.txt")
	if err != nil {
		t.Fatalf("Failed to read written file: %v", err)
	}
	if string(data) != content {
		t.Errorf("Expected %q, got %q", content, string(data))
	}
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubTool{
		name:        "test",
		desc: "A test tool",
		props: map[string]PropertySchema{
			"arg1": {Type: "string", Description: "First argument"},
		},
		required: []string{"arg1"},
		executeFn: func(ctx context.Context, args string) (string, error) {
			return "success", nil
		},
	})

	// Test invoking registered tool
	tr := reg.Invoke(nil, "test", `{"arg1": "value1"}`)
	if tr.Status != ToolResultSuccess {
		t.Errorf("Invoke failed: %v", tr.Err)
	}
	if tr.Output != "success" {
		t.Errorf("Expected 'success', got %q", tr.Output)
	}

	// Test unknown tool
	tr = reg.Invoke(nil, "unknown", "{}")
	if tr.Status != ToolResultError {
		t.Error("Expected error for unknown tool")
	}

	// Test missing required argument
	tr = reg.Invoke(nil, "test", "{}")
	if tr.Status != ToolResultError {
		t.Error("Expected error for missing required argument")
	}

	// Test Unregister
	if !reg.Unregister("test") {
		t.Error("Expected Unregister to return true for registered tool")
	}
	tr = reg.Invoke(nil, "test", `{"arg1": "value1"}`)
	if tr.Status != ToolResultError {
		t.Error("Expected error after unregistering tool")
	}

	// Test Unregister non-existent tool
	if reg.Unregister("nonexistent") {
		t.Error("Expected Unregister to return false for unknown tool")
	}
}

func TestRegistryIsParallel(t *testing.T) {
	reg := NewRegistry()

	// Unknown tool returns false
	if reg.IsParallel("nonexistent") {
		t.Error("Expected IsParallel to return false for unknown tool")
	}

	// Register a parallel tool
	reg.Register(&stubTool{
		name:      "parallel_tool",
		parallel:  true,
		executeFn: func(ctx context.Context, args string) (string, error) { return "", nil },
	})
	if !reg.IsParallel("parallel_tool") {
		t.Error("Expected IsParallel to return true for parallel tool")
	}

	// Register a non-parallel tool
	reg.Register(&stubTool{name: "serial_tool", parallel: false})
	if reg.IsParallel("serial_tool") {
		t.Error("Expected IsParallel to return false for non-parallel tool")
	}
}
