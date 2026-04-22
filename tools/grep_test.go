package tools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func setupGrepTestDir(t *testing.T) string {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "grep_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	files := map[string]string{
		"main.go":            "package main\n\nfunc main() {\n\tfmt.Println(\"hello world\")\n}\n",
		"lib.go":             "package main\n\nfunc helper() string {\n\treturn \"hello\"\n}\n",
		"src/utils.go":       "package src\n\nfunc Uppercase(s string) string {\n\treturn strings.ToUpper(s)\n}\n",
		"src/utils_test.go":  "package src\n\nfunc TestUppercase(t *testing.T) {\n\tresult := Uppercase(\"hello\")\n}\n",
		"docs/readme.md":     "# Project\n\nThis is a test project.\nIt contains hello world examples.\n",
		"data.json":          "{\"key\": \"hello\", \"value\": 42}\n",
		".hidden/secret.go":  "package hidden\n\nvar secret = \"hello hidden\"\n",
	}

	for name, content := range files {
		path := filepath.Join(tmpDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("Failed to create dir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write %s: %v", name, err)
		}
	}

	return tmpDir
}

func skipWithoutRg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available, skipping test")
	}
}

func parseGrepResult(t *testing.T, raw string) GrepResult {
	t.Helper()
	var r GrepResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}
	return r
}

func TestGrep_FilesWithMatches(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "hello", "path": "` + tmpDir + `"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	if r.Mode != "files_with_matches" {
		t.Errorf("Expected mode files_with_matches, got %s", r.Mode)
	}
	if r.NumFiles < 4 {
		t.Errorf("Expected at least 4 files matching 'hello', got %d: %v", r.NumFiles, r.Filenames)
	}
	for _, f := range r.Filenames {
		if filepath.IsAbs(f) {
			t.Errorf("Expected relative path, got %q", f)
		}
	}
}

func TestGrep_ContentMode(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "hello", "path": "` + tmpDir + `", "output_mode": "content"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	if r.Mode != "content" {
		t.Errorf("Expected mode content, got %s", r.Mode)
	}
	if r.Content == "" {
		t.Error("Expected non-empty content")
	}
	if r.NumLines == 0 {
		t.Error("Expected non-zero NumLines")
	}
}

func TestGrep_ContentModeWithContext(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "hello", "path": "` + tmpDir + `", "output_mode": "content", "context_lines": 1}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	if r.NumLines <= 4 {
		t.Errorf("Expected more lines with context, got %d", r.NumLines)
	}
}

func TestGrep_CountMode(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "hello", "path": "` + tmpDir + `", "output_mode": "count"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	if r.Mode != "count" {
		t.Errorf("Expected mode count, got %s", r.Mode)
	}
	if r.NumMatches < 4 {
		t.Errorf("Expected at least 4 matches, got %d", r.NumMatches)
	}
	if r.NumFiles < 4 {
		t.Errorf("Expected at least 4 files, got %d", r.NumFiles)
	}
}

func TestGrep_GlobFilter(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "hello", "path": "` + tmpDir + `", "glob": "*.go"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	for _, f := range r.Filenames {
		if !strings.HasSuffix(f, ".go") {
			t.Errorf("Expected only .go files, got %q", f)
		}
	}
}

func TestGrep_TypeFilter(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "hello", "path": "` + tmpDir + `", "type": "go"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	for _, f := range r.Filenames {
		if !strings.HasSuffix(f, ".go") {
			t.Errorf("Expected only Go files, got %q", f)
		}
	}
}

func TestGrep_CaseInsensitive(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "HELLO", "path": "` + tmpDir + `", "case_insensitive": true}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	if r.NumFiles == 0 {
		t.Error("Expected matches with case insensitive search")
	}
}

func TestGrep_CaseSensitiveNoMatch(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "HELLO", "path": "` + tmpDir + `"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	if r.NumFiles != 0 {
		t.Errorf("Expected no matches for case sensitive 'HELLO', got %d files", r.NumFiles)
	}
}

func TestGrep_RegexPattern(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "func\\s+\\w+\\(", "path": "` + tmpDir + `", "type": "go"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	if r.NumFiles < 2 {
		t.Errorf("Expected at least 2 files with func declarations, got %d", r.NumFiles)
	}
}

func TestGrep_NoMatches(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "nonexistent_string_xyz", "path": "` + tmpDir + `"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	if r.NumFiles != 0 {
		t.Errorf("Expected 0 files, got %d", r.NumFiles)
	}
}

func TestGrep_EmptyPattern(t *testing.T) {
	tool := GrepTool{}
	_, err := tool.ExecuteContext(nil,`{"pattern": ""}`)
	if err == nil {
		t.Error("Expected error for empty pattern")
	}
}

func TestGrep_InvalidOutputMode(t *testing.T) {
	tool := GrepTool{}
	_, err := tool.ExecuteContext(nil,`{"pattern": "test", "output_mode": "invalid"}`)
	if err == nil {
		t.Error("Expected error for invalid output_mode")
	}
}

func TestGrep_HiddenFiles(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "hello hidden", "path": "` + tmpDir + `"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	if r.NumFiles != 1 {
		t.Errorf("Expected 1 file in hidden dir, got %d: %v", r.NumFiles, r.Filenames)
	}
}

func TestGrep_PatternStartingWithDash(t *testing.T) {
	skipWithoutRg(t)
	tmpDir, err := os.MkdirTemp("", "grep_dash_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	path := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(path, []byte("some -flag here\n"), 0644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "-flag", "path": "` + tmpDir + `"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	if r.NumFiles != 1 {
		t.Errorf("Expected 1 file, got %d", r.NumFiles)
	}
}

func TestGrep_RipgrepNotFound(t *testing.T) {
	origPath := os.Getenv("PATH")
	defer os.Setenv("PATH", origPath)
	os.Setenv("PATH", "/tmp")

	tool := GrepTool{}
	_, err := tool.ExecuteContext(nil,`{"pattern": "test"}`)
	if err == nil || !strings.Contains(err.Error(), "ripgrep") {
		t.Errorf("Expected error about ripgrep not found, got: %v", err)
	}
}

func TestGrep_Multiline(t *testing.T) {
	skipWithoutRg(t)
	tmpDir, err := os.MkdirTemp("", "grep_ml_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	path := filepath.Join(tmpDir, "multi.txt")
	if err := os.WriteFile(path, []byte("start\nmiddle\nend\n"), 0644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "start.middle", "path": "` + tmpDir + `", "multiline": true}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	if r.NumFiles != 1 {
		t.Errorf("Expected 1 file with multiline match, got %d", r.NumFiles)
	}
}

func TestGrep_RelativePaths(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(nil,`{"pattern": "hello", "path": "` + tmpDir + `", "output_mode": "content"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	r := parseGrepResult(t, raw)
	for _, line := range strings.Split(r.Content, "\n") {
		if line == "" || line == "--" {
			continue
		}
		if strings.HasPrefix(line, tmpDir) {
			t.Errorf("Expected relative paths in content, got absolute: %q", line)
		}
	}
}
