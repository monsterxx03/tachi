package tools

import (
	"os"
	"testing"
)

func TestWriteTool(t *testing.T) {
	tool := WriteTool{}
	content := "Test content"
	_, err := tool.Execute(`{"path": "/tmp/test_write.txt", "content": "Test content"}`)
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
	result, err := reg.Invoke("test", `{"arg1": "value1"}`)
	if err != nil {
		t.Errorf("Invoke failed: %v", err)
	}
	if result != "success" {
		t.Errorf("Expected 'success', got %q", result)
	}

	// Test unknown tool
	_, err = reg.Invoke("unknown", "{}")
	if err == nil {
		t.Error("Expected error for unknown tool")
	}

	// Test missing required argument
	_, err = reg.Invoke("test", "{}")
	if err == nil {
		t.Error("Expected error for missing required argument")
	}
}
