package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestTruncateToolOutput_UnderLimit(t *testing.T) {
	mgr := NewManager(t.Context(), 0, "", nil)
	result := strings.Repeat("hello", 100) // 500 chars
	output := mgr.truncateToolOutput(t.Context(), result, 1000, "", "mcp__test__echo")
	if output != result {
		t.Errorf("expected unmodified result, got %d chars (input %d)", len(output), len(result))
	}
}

func TestTruncateToolOutput_ZeroLimit(t *testing.T) {
	mgr := NewManager(t.Context(), 0, "", nil)
	result := strings.Repeat("hello", 1000) // 5000 chars
	output := mgr.truncateToolOutput(t.Context(), result, 0, "", "mcp__test__echo")
	if output != result {
		t.Error("zero limit should return result unchanged")
	}
}

func TestTruncateToolOutput_OverLimit_FilePersistence(t *testing.T) {
	mgr := NewManager(t.Context(), 0, "", nil)
	result := strings.Repeat("abcdefghij", 1000) // 10000 chars
	tmpDir := t.TempDir()

	output := mgr.truncateToolOutput(t.Context(), result, 5000, tmpDir, "mcp__server__tool")

	// Should be shorter than original.
	if len(output) >= len(result) {
		t.Errorf("expected truncated output, got %d chars (input %d)", len(output), len(result))
	}

	// Should contain the truncation marker and file path — but NOT preview content.
	if !strings.Contains(output, "[OUTPUT TOO LARGE]") {
		t.Error("output should contain [OUTPUT TOO LARGE] marker")
	}
	if !strings.Contains(output, "Use ReadFile") {
		t.Error("output should contain ReadFile instruction")
	}
	if !strings.Contains(output, tmpDir) {
		t.Error("output should contain the file directory path")
	}
	if strings.Contains(output, "Preview") {
		t.Error("output should NOT contain preview content")
	}

	// Verify the file was created.
	files, err := filepath.Glob(filepath.Join(tmpDir, "mcp__server__tool_*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 persisted file, got %d", len(files))
	}

	// Verify file content matches the original.
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != result {
		t.Errorf("file content mismatch: got %d chars, want %d", len(data), len(result))
	}
}

func TestTruncateToolOutput_FallbackOnBadDir(t *testing.T) {
	mgr := NewManager(t.Context(), 0, "", nil)
	result := strings.Repeat("x", 10000)
	// Use a path that can't be created (file where dir should be).
	badDir := filepath.Join(t.TempDir(), "notadir")
	// Create a file at the path to make mkdir fail.
	if err := os.WriteFile(badDir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	output := mgr.truncateToolOutput(t.Context(), result, 5000, badDir, "mcp__test__x")

	// Should fall back to hard truncation.
	if !strings.Contains(output, "[OUTPUT TRUNCATED") {
		t.Error("fallback should use [OUTPUT TRUNCATED] marker")
	}
	if len(output) > 6000 {
		t.Errorf("hard-truncated output too long: %d chars", len(output))
	}
}

func TestHardTruncate(t *testing.T) {
	result := strings.Repeat("abcdefghij", 1000) // 10000 chars
	output := hardTruncate(result, 5000, "mcp__test__x")

	if !strings.Contains(output, "[OUTPUT TRUNCATED") {
		t.Error("should contain [OUTPUT TRUNCATED]")
	}
	if len(output) < 5000 || len(output) > 6000 {
		t.Errorf("unexpected output length: %d", len(output))
	}
}

func TestSanitizeForFilename(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple", "hello", "hello"},
		{"spaces", "my tool name", "my_tool_name"},
		{"mcp_prefix", "mcp__server__tool", "mcp__server__tool"},
		{"special_chars", "tool:a/b?c<d>e", "tool_a_b_c_d_e"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeForFilename(tt.input)
			if got != tt.expected {
				t.Errorf("sanitizeForFilename(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestMCPTool_ExecuteContext_Truncation(t *testing.T) {
	// Build an MCPTool backed by a Manager with a stub client that returns
	// a large result.
	mgr := NewManager(t.Context(), 5000, t.TempDir(), nil)

	largeContent := strings.Repeat("abcdefghij", 1000) // 10000 chars
	addTestClient(mgr, "test-server", &stubMCPClient{
		callTool: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{Type: "text", Text: largeContent},
				},
			}, nil
		},
	})

	tool := MCPTool{
		serverName: "test-server",
		serverTool: &mcp.Tool{Name: "big_result"},
		manager:    mgr,
	}

	output, err := tool.ExecuteContext(t.Context(), "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should be truncated.
	if !strings.Contains(output, "[OUTPUT TOO LARGE]") {
		t.Error("expected truncated output")
	}
	if len(output) >= len(largeContent) {
		t.Errorf("output should be shorter than original (%d >= %d)", len(output), len(largeContent))
	}

	// Should contain file path + ReadFile instruction — but NOT preview content.
	if !strings.Contains(output, "Use ReadFile") {
		t.Error("should contain ReadFile instruction")
	}
	if strings.Contains(output, "Preview") {
		t.Error("should NOT contain preview content")
	}
}

func TestCleanupOldToolResults(t *testing.T) {
	mgr := NewManager(t.Context(), 0, "", nil)
	tmpDir := t.TempDir()

	// Create a fresh file (should survive cleanup).
	freshFile := filepath.Join(tmpDir, "fresh.txt")
	if err := os.WriteFile(freshFile, []byte("fresh"), 0600); err != nil {
		t.Fatal(err)
	}

	// Create an old file with mtime set to 48 hours ago.
	oldFile := filepath.Join(tmpDir, "old.txt")
	if err := os.WriteFile(oldFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Create another old file.
	oldFile2 := filepath.Join(tmpDir, "old2.txt")
	if err := os.WriteFile(oldFile2, []byte("old2"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime2 := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(oldFile2, oldTime2, oldTime2); err != nil {
		t.Fatal(err)
	}

	// Cleanup with 24h max age.
	mgr.cleanupOldToolResults(t.Context(), tmpDir, 24*time.Hour)

	// Fresh file should survive.
	if _, err := os.Stat(freshFile); os.IsNotExist(err) {
		t.Error("fresh file should survive cleanup")
	}

	// Old files should be removed.
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("old file should be removed")
	}
	if _, err := os.Stat(oldFile2); !os.IsNotExist(err) {
		t.Error("old file 2 should be removed")
	}
}

func TestCleanupOldToolResults_EmptyDir(t *testing.T) {
	mgr := NewManager(t.Context(), 0, "", nil)
	tmpDir := t.TempDir()
	// Should not panic or error on empty dir.
	mgr.cleanupOldToolResults(t.Context(), tmpDir, 24*time.Hour)
}

func TestCleanupOldToolResults_NonExistentDir(t *testing.T) {
	mgr := NewManager(t.Context(), 0, "", nil)
	// Should not panic on non-existent dir.
	mgr.cleanupOldToolResults(t.Context(), "/nonexistent/path/tool_results", 24*time.Hour)
}
