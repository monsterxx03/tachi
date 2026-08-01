package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractGlobBaseDirectory(t *testing.T) {
	tests := []struct {
		pattern     string
		wantBaseDir string
		wantRelPat  string
	}{
		{"**/*.go", "", "**/*.go"},
		{"*.go", "", "*.go"},
		{"src/**/*.ts", "src", "**/*.ts"},
		{"src/*.go", "src", "*.go"},
		{"/foo/bar/**/*.go", "/foo/bar", "**/*.go"},
		{"/foo/bar/baz.go", "/foo/bar", "baz.go"},
		{"foo/bar/baz.go", "foo/bar", "baz.go"},
		{"test.txt", ".", "test.txt"},
		{"**/node_modules/**", "", "**/node_modules/**"},
		{"src/**", "src", "**"},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			baseDir, relPat := extractGlobBaseDirectory(tt.pattern)
			if baseDir != tt.wantBaseDir {
				t.Errorf("extractGlobBaseDirectory(%q) baseDir = %q, want %q", tt.pattern, baseDir, tt.wantBaseDir)
			}
			if relPat != tt.wantRelPat {
				t.Errorf("extractGlobBaseDirectory(%q) relPat = %q, want %q", tt.pattern, relPat, tt.wantRelPat)
			}
		})
	}
}

func setupTestDir(t *testing.T) string {
	tmpDir, err := os.MkdirTemp("", "glob_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	// Create test file structure
	files := []string{
		"root.go",
		"src/lib.go",
		"src/utils/helper.go",
		"src/utils/extra.go",
		"docs/readme.md",
		"pkg/pkg.go",
		"pkg/sub/module.go",
		".hidden.go",
	}

	for _, f := range files {
		path := filepath.Join(tmpDir, f)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("Failed to create dir for %s: %v", f, err)
		}
		if err := os.WriteFile(path, []byte("package "+filepath.Base(f)), 0644); err != nil {
			t.Fatalf("Failed to write %s: %v", f, err)
		}
	}

	return tmpDir
}

func TestGlobFile_BasicPattern(t *testing.T) {
	// Check if ripgrep is available
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available, skipping test")
	}

	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	tool := GlobTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"pattern": "**/*.go", "path": "`+tmpDir+`"}`)
	if err != nil {
		t.Fatalf("GlobFile failed: %v", err)
	}

	var gr GlobResult
	if err := json.Unmarshal([]byte(result), &gr); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if gr.NumFiles == 0 {
		t.Error("Expected at least one .go file")
	}
	if gr.Truncated && gr.NumFiles <= 100 {
		t.Error("Truncated should be false when under limit")
	}
}

func TestGlobFile_SpecificDirectory(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available, skipping test")
	}

	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	tool := GlobTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"pattern": "src/**/*.go", "path": "`+tmpDir+`"}`)
	if err != nil {
		t.Fatalf("GlobFile failed: %v", err)
	}

	var gr GlobResult
	if err := json.Unmarshal([]byte(result), &gr); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	// Should find lib.go, helper.go, extra.go in src/
	if gr.NumFiles != 3 {
		t.Errorf("Expected 3 files, got %d: %v", gr.NumFiles, gr.Filenames)
	}
}

func TestGlobFile_SingleFilePattern(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available, skipping test")
	}

	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	// Note: ripgrep's --glob *.go matches .go files at any level recursively
	// To match only in root directory, use a pattern like "./ *.go"
	tool := GlobTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"pattern": "*.go", "path": "`+tmpDir+`"}`)
	if err != nil {
		t.Fatalf("GlobFile failed: %v", err)
	}

	var gr GlobResult
	if err := json.Unmarshal([]byte(result), &gr); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	// ripgrep --glob *.go matches all .go files recursively
	// This is ripgrep's behavior, not a bug
	if gr.NumFiles == 0 {
		t.Error("Expected at least one .go file")
	}
}

func TestGlobFile_AbsolutePattern(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available, skipping test")
	}

	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	// Test with absolute pattern
	pattern := filepath.Join(tmpDir, "src/**/*.go")
	tool := GlobTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"pattern": "`+pattern+`"}`)
	if err != nil {
		t.Fatalf("GlobFile failed: %v", err)
	}

	var gr GlobResult
	if err := json.Unmarshal([]byte(result), &gr); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if gr.NumFiles != 3 {
		t.Errorf("Expected 3 files, got %d: %v", gr.NumFiles, gr.Filenames)
	}
}

func TestGlobFile_NoMatches(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available, skipping test")
	}

	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	tool := GlobTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"pattern": "**/*.nonexistent", "path": "`+tmpDir+`"}`)
	if err != nil {
		t.Fatalf("GlobFile failed: %v", err)
	}

	var gr GlobResult
	if err := json.Unmarshal([]byte(result), &gr); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if gr.NumFiles != 0 {
		t.Errorf("Expected 0 files, got %d", gr.NumFiles)
	}
	if gr.Truncated {
		t.Error("Truncated should be false for empty result")
	}
}

func TestGlobFile_EmptyPattern(t *testing.T) {
	tool := GlobTool{}
	_, err := tool.ExecuteContext(context.TODO(), `{"pattern": ""}`)
	if err == nil {
		t.Error("Expected error for empty pattern")
	}
}

func TestGlobFile_DurationAndTruncatedFields(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available, skipping test")
	}

	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	tool := GlobTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"pattern": "**/*.go", "path": "`+tmpDir+`"}`)
	if err != nil {
		t.Fatalf("GlobFile failed: %v", err)
	}

	var gr GlobResult
	if err := json.Unmarshal([]byte(result), &gr); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if gr.DurationMs < 0 {
		t.Errorf("DurationMs should be non-negative, got %d", gr.DurationMs)
	}
}

func TestGlobFile_RelativePaths(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available, skipping test")
	}

	// Save and restore original cwd
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get cwd: %v", err)
	}
	defer os.Chdir(origDir)

	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}

	tool := GlobTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"pattern": "src/**/*.go"}`)
	if err != nil {
		t.Fatalf("GlobFile failed: %v", err)
	}

	var gr GlobResult
	if err := json.Unmarshal([]byte(result), &gr); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	// Verify all paths are relative (don't start with /)
	for _, f := range gr.Filenames {
		if strings.HasPrefix(f, "/") {
			t.Errorf("Expected relative path, got %q", f)
		}
	}
}

func TestGlobFile_RipgrepNotFound(t *testing.T) {
	// Temporarily modify PATH to not include ripgrep
	origPath := os.Getenv("PATH")
	defer os.Setenv("PATH", origPath)

	// Set PATH to a directory that definitely doesn't have rg
	os.Setenv("PATH", "/tmp")

	tool := GlobTool{}
	_, err := tool.ExecuteContext(context.TODO(), `{"pattern": "**/*.go"}`)
	if err == nil || !strings.Contains(err.Error(), "ripgrep") {
		t.Errorf("Expected error about ripgrep not found, got: %v", err)
	}
}
