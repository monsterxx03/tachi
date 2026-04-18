package tools

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestReadTool(t *testing.T) {
	tool := ReadTool{}

	// Create a test file
	content := "Hello, World!"
	err := os.WriteFile("/tmp/test_read.txt", []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove("/tmp/test_read.txt")

	// Test Read
	result, err := tool.Execute(`{"path": "/tmp/test_read.txt"}`)
	if err != nil {
		t.Fatalf("ReadTool.Execute failed: %v", err)
	}
	if result != content {
		t.Errorf("Expected %q, got %q", content, result)
	}
}

func TestReadToolNotFound(t *testing.T) {
	tool := ReadTool{}
	_, err := tool.Execute(`{"path": "/nonexistent/file.txt"}`)
	if err == nil {
		t.Error("Expected error for nonexistent file")
	}
}

func TestReadToolWithOffsetAndLimit(t *testing.T) {
	tool := ReadTool{}

	// Create a test file with 10 lines
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	content := strings.Join(lines, "\n")
	err := os.WriteFile("/tmp/test_read_offset.txt", []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove("/tmp/test_read_offset.txt")

	tests := []struct {
		name   string
		offset int
		limit  int
		want   string
	}{
		{"default (no offset/limit)", 0, 0, content},
		{"offset only", 3, 0, "line3\nline4\nline5\nline6\nline7\nline8\nline9\nline10"},
		{"limit only", 0, 5, "line1\nline2\nline3\nline4\nline5"},
		{"offset and limit", 3, 4, "line3\nline4\nline5\nline6"},
		{"offset 1 (1-indexed)", 1, 3, "line1\nline2\nline3"},
		{"offset beyond end", 20, 0, ""},
		{"limit exceeds remaining", 8, 10, "line8\nline9\nline10"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args string
			if tt.offset > 0 && tt.limit > 0 {
				args = fmt.Sprintf(`{"path": "/tmp/test_read_offset.txt", "offset": %d, "limit": %d}`, tt.offset, tt.limit)
			} else if tt.offset > 0 {
				args = fmt.Sprintf(`{"path": "/tmp/test_read_offset.txt", "offset": %d}`, tt.offset)
			} else if tt.limit > 0 {
				args = fmt.Sprintf(`{"path": "/tmp/test_read_offset.txt", "limit": %d}`, tt.limit)
			} else {
				args = `{"path": "/tmp/test_read_offset.txt"}`
			}

			result, err := tool.Execute(args)
			if err != nil {
				t.Fatalf("ReadTool.Execute failed: %v", err)
			}
			if result != tt.want {
				t.Errorf("Expected %q, got %q", tt.want, result)
			}
		})
	}
}

func TestReadToolBinary(t *testing.T) {
	tool := ReadTool{}

	// Create a binary file with null bytes
	binaryContent := []byte{0x00, 0x01, 0x02, 'h', 'e', 'l', 'l', 'o'}
	err := os.WriteFile("/tmp/test_binary.bin", binaryContent, 0644)
	if err != nil {
		t.Fatalf("Failed to create binary test file: %v", err)
	}
	defer os.Remove("/tmp/test_binary.bin")

	_, err = tool.Execute(`{"path": "/tmp/test_binary.bin"}`)
	if err == nil {
		t.Error("Expected error for binary file")
	}
	if !strings.Contains(err.Error(), "binary file") {
		t.Errorf("Expected binary file error, got: %v", err)
	}
}

func TestReadToolTooLarge(t *testing.T) {
	tool := ReadTool{}

	// Create a file larger than 256KB
	largeContent := make([]byte, 257*1024) // 257KB
	for i := range largeContent {
		largeContent[i] = 'a'
	}
	err := os.WriteFile("/tmp/test_large.txt", largeContent, 0644)
	if err != nil {
		t.Fatalf("Failed to create large test file: %v", err)
	}
	defer os.Remove("/tmp/test_large.txt")

	_, err = tool.Execute(`{"path": "/tmp/test_large.txt"}`)
	if err == nil {
		t.Error("Expected error for large file")
	}
	if !strings.Contains(err.Error(), "file too large") {
		t.Errorf("Expected 'file too large' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "263168") {
		t.Errorf("Expected actual size in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "262144") {
		t.Errorf("Expected limit size in error, got: %v", err)
	}
}

func TestReadToolAtLimit(t *testing.T) {
	tool := ReadTool{}

	// Create a file exactly at 256KB
	limitContent := make([]byte, 256*1024) // exactly 256KB
	for i := range limitContent {
		limitContent[i] = 'a'
	}
	err := os.WriteFile("/tmp/test_at_limit.txt", limitContent, 0644)
	if err != nil {
		t.Fatalf("Failed to create limit test file: %v", err)
	}
	defer os.Remove("/tmp/test_at_limit.txt")

	// Should not error - exactly at limit
	_, err = tool.Execute(`{"path": "/tmp/test_at_limit.txt"}`)
	if err != nil {
		t.Errorf("Should not error at exactly 256KB, got: %v", err)
	}
}
