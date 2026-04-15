package tools

import (
	"os"
	"testing"
)

func TestReadFile(t *testing.T) {
	// Create a test file
	content := "Hello, World!"
	err := os.WriteFile("/tmp/test_read.txt", []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove("/tmp/test_read.txt")

	// Test Read
	result, err := ReadFile(`{"path": "/tmp/test_read.txt"}`)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if result != content {
		t.Errorf("Expected %q, got %q", content, result)
	}
}

func TestReadFileNotFound(t *testing.T) {
	_, err := ReadFile(`{"path": "/nonexistent/file.txt"}`)
	if err == nil {
		t.Error("Expected error for nonexistent file")
	}
}

func TestWriteFile(t *testing.T) {
	content := "Test content"
	_, err := WriteFile(`{"path": "/tmp/test_write.txt", "content": "Test content"}`)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
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
	reg.Register("test", "A test tool", map[string]PropertySchema{
		"arg1": {Type: "string", Description: "First argument"},
	}, []string{"arg1"}, func(args string) (string, error) {
		return "success", nil
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
