package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteTool_Name(t *testing.T) {
	tool := WriteTool{}
	if tool.Name() != ToolNameWrite {
		t.Errorf("expected %s, got %s", ToolNameWrite, tool.Name())
	}
}

func TestWriteTool_Description(t *testing.T) {
	tool := WriteTool{}
	if tool.Description() == "" {
		t.Error("expected non-empty description")
	}
}

func TestWriteTool_Properties(t *testing.T) {
	tool := WriteTool{}
	props := tool.Properties()
	if _, ok := props["path"]; !ok {
		t.Error("expected path property")
	}
	if _, ok := props["content"]; !ok {
		t.Error("expected content property")
	}
}

func TestWriteTool_Required(t *testing.T) {
	tool := WriteTool{}
	required := tool.Required()
	if len(required) != 2 {
		t.Fatalf("expected 2 required fields, got %d", len(required))
	}
	if required[0] != "path" && required[1] != "path" {
		t.Error("expected path in required")
	}
	if required[0] != "content" && required[1] != "content" {
		t.Error("expected content in required")
	}
}

func TestWriteTool_WriteAbsolutePath(t *testing.T) {
	tool := WriteTool{}
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")

	output, err := tool.ExecuteContext(context.Background(),
		`{"path": "`+filePath+`", "content": "hello world"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify file was written
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(data))
	}

	// Verify output message
	if !containsStr(output, filePath) {
		t.Errorf("expected output to mention path, got: %s", output)
	}
}

func TestWriteTool_WriteUnicode(t *testing.T) {
	tool := WriteTool{}
	dir := t.TempDir()
	filePath := filepath.Join(dir, "unicode.txt")

	content := "你好，世界！🍣"
	_, err := tool.ExecuteContext(context.Background(),
		`{"path": "`+filePath+`", "content": "`+content+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(data) != content {
		t.Errorf("expected %q, got %q", content, string(data))
	}
}

func TestWriteTool_WriteEmptyContent(t *testing.T) {
	tool := WriteTool{}
	dir := t.TempDir()
	filePath := filepath.Join(dir, "empty.txt")

	_, err := tool.ExecuteContext(context.Background(),
		`{"path": "`+filePath+`", "content": ""}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(data))
	}
}

func TestWriteTool_WriteNewDirectory(t *testing.T) {
	tool := WriteTool{}
	dir := t.TempDir()
	// File in a subdirectory that doesn't exist yet — should auto-create
	filePath := filepath.Join(dir, "sub", "nested", "file.txt")

	_, err := tool.ExecuteContext(context.Background(),
		`{"path": "`+filePath+`", "content": "new dir content"}`)
	if err != nil {
		t.Fatalf("expected success with auto-created directories, got: %v", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(data) != "new dir content" {
		t.Errorf("expected %q, got %q", "new dir content", string(data))
	}
}

func TestWriteTool_WriteOverwrite(t *testing.T) {
	tool := WriteTool{}
	dir := t.TempDir()
	filePath := filepath.Join(dir, "overwrite.txt")

	// Write first file
	_, err := tool.ExecuteContext(context.Background(),
		`{"path": "`+filePath+`", "content": "first"}`)
	if err != nil {
		t.Fatalf("first write failed: %v", err)
	}

	// Overwrite with different content
	_, err = tool.ExecuteContext(context.Background(),
		`{"path": "`+filePath+`", "content": "second"}`)
	if err != nil {
		t.Fatalf("overwrite failed: %v", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(data) != "second" {
		t.Errorf("expected 'second', got %q", string(data))
	}
}

func TestWriteTool_InvalidJSON(t *testing.T) {
	tool := WriteTool{}
	_, err := tool.ExecuteContext(context.Background(), "not json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestWriteTool_FilePermissions(t *testing.T) {
	tool := WriteTool{}
	dir := t.TempDir()
	filePath := filepath.Join(dir, "perm.txt")

	_, err := tool.ExecuteContext(context.Background(),
		`{"path": "`+filePath+`", "content": "test"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("failed to stat file: %v", err)
	}
	// Should be readable by owner (0644)
	if info.Mode().Perm()&0400 == 0 {
		t.Error("expected file to be owner-readable")
	}
}

func TestWriteTool_NotParallel(t *testing.T) {
	tool := WriteTool{}
	if tool.Parallel() {
		t.Error("WriteTool should not be parallel")
	}
}

// containsStr reports whether substr is within s.
func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
