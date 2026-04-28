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

// testTool is a simple tool implementation for testing
type testTool struct {
	name        string
	description string
	properties  map[string]PropertySchema
	required    []string
	fn          func(args string) (string, error)
}

func (t testTool) Name() string        { return t.name }
func (t testTool) Description() string { return t.description }
func (t testTool) Properties() map[string]PropertySchema {
	return t.properties
}
func (t testTool) Required() []string { return t.required }
func (t testTool) Parallel() bool    { return true }
func (t testTool) Execute(args string) (string, error) {
	return t.fn(args)
}
func (t testTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	return t.fn(args)
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry()
	reg.Register(testTool{
		name:        "test",
		description: "A test tool",
		properties: map[string]PropertySchema{
			"arg1": {Type: "string", Description: "First argument"},
		},
		required: []string{"arg1"},
		fn: func(args string) (string, error) {
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
