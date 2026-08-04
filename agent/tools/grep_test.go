package tools

import (
	"context"
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
		"main.go":           "package main\n\nfunc main() {\n\tfmt.Println(\"hello world\")\n}\n",
		"lib.go":            "package main\n\nfunc helper() string {\n\treturn \"hello\"\n}\n",
		"src/utils.go":      "package src\n\nfunc Uppercase(s string) string {\n\treturn strings.ToUpper(s)\n}\n",
		"src/utils_test.go": "package src\n\nfunc TestUppercase(t *testing.T) {\n\tresult := Uppercase(\"hello\")\n}\n",
		"docs/readme.md":    "# Project\n\nThis is a test project.\nIt contains hello world examples.\n",
		"data.json":         `{"key": "hello", "value": 42}` + "\n",
		".hidden/secret.go": "package hidden\n\nvar secret = \"hello hidden\"\n",
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

// assertFiles parses the plain-text "files_with_matches" output and checks
// that it contains at least minFiles relative paths (one per line).
func assertFiles(t *testing.T, raw string, minFiles int) []string {
	t.Helper()
	lines := splitNonEmpty(raw)
	if len(lines) < minFiles {
		t.Errorf("expected at least %d files, got %d. Output:\n%s", minFiles, len(lines), raw)
	}
	for _, f := range lines {
		if strings.HasPrefix(f, "/") {
			t.Errorf("expected relative path, got %q", f)
		}
	}
	return lines
}

// assertLines parses the plain-text "content" output and checks that it
// contains at least minLines of matching lines (skipping empty/separator lines).
func assertLines(t *testing.T, raw string, minLines int) {
	t.Helper()
	contentLines := strings.Split(raw, "\n")
	nonEmpty := 0
	for _, l := range contentLines {
		if strings.TrimSpace(l) != "" {
			nonEmpty++
		}
	}
	if nonEmpty < minLines {
		t.Errorf("expected at least %d content lines, got %d. Output:\n%s", minLines, nonEmpty, raw)
	}
}

// splitNonEmpty splits raw by newlines and returns non-empty trimmed lines.
func splitNonEmpty(raw string) []string {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	var result []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "...") && !strings.HasPrefix(l, "(total:") && !strings.HasPrefix(l, "(no ") {
			result = append(result, l)
		}
	}
	return result
}

func TestGrep_FilesWithMatches(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(context.TODO(), `{"pattern": "hello", "path": "`+tmpDir+`"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	files := assertFiles(t, raw, 4)
	for _, f := range files {
		if filepath.IsAbs(f) {
			t.Errorf("Expected relative path, got %q", f)
		}
	}
}

func TestGrep_ContentMode(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(context.TODO(), `{"pattern": "hello", "path": "`+tmpDir+`", "output_mode": "content"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	assertLines(t, raw, 4)
}

func TestGrep_ContentModeWithContext(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(context.TODO(), `{"pattern": "hello", "path": "`+tmpDir+`", "output_mode": "content", "context_lines": 1}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	assertLines(t, raw, 8)
}

func TestGrep_CountMode(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(context.TODO(), `{"pattern": "hello", "path": "`+tmpDir+`", "output_mode": "count"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	files := splitNonEmpty(raw)
	if len(files) < 4 {
		t.Errorf("expected at least 4 file counts, got %d. Output:\n%s", len(files), raw)
	}
	// Each line should have format "path: N" for count mode
	for _, f := range files {
		if !strings.Contains(f, ":") {
			t.Errorf("count line should contain ':file_count', got %q", f)
		}
	}
	// Should have a total summary
	if !strings.Contains(raw, "(total:") {
		t.Errorf("expected total summary in count output, got:\n%s", raw)
	}
}

func TestGrep_GlobFilter(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(context.TODO(), `{"pattern": "hello", "path": "`+tmpDir+`", "glob": "*.go"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	files := assertFiles(t, raw, 1)
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			t.Errorf("Expected only .go files, got %q", f)
		}
	}
}

func TestGrep_TypeFilter(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(context.TODO(), `{"pattern": "hello", "path": "`+tmpDir+`", "type": "go"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	files := assertFiles(t, raw, 1)
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			t.Errorf("Expected only Go files, got %q", f)
		}
	}
}

func TestGrep_CaseInsensitive(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(context.TODO(), `{"pattern": "HELLO", "path": "`+tmpDir+`", "case_insensitive": true}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	files := splitNonEmpty(raw)
	if len(files) == 0 {
		t.Error("Expected matches with case insensitive search")
	}
}

func TestGrep_CaseSensitiveNoMatch(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(context.TODO(), `{"pattern": "HELLO", "path": "`+tmpDir+`"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	if !strings.Contains(raw, "(no matching files)") {
		t.Errorf("Expected no-match indicator, got:\n%s", raw)
	}
}

func TestGrep_FixedString(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	// Create a file with text containing regex special characters.
	content := `foo.Bar(ctx, "test")
another line with dots.here
plain text`
	if err := os.WriteFile(filepath.Join(tmpDir, "special.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tool := GrepTool{}

	// fixed_string=true: dots and parens are literal, not regex.
	raw, err := tool.ExecuteContext(context.TODO(),
		`{"pattern": "foo.Bar(ctx", "path": "`+tmpDir+`", "output_mode": "content", "fixed_string": true}`)
	if err != nil {
		t.Fatalf("fixed_string grep failed: %v", err)
	}
	if strings.Contains(raw, "(no matches)") {
		t.Errorf("fixed_string should find literal 'foo.Bar(ctx'. Output:\n%s", raw)
	}
	if !strings.Contains(raw, "special.go") {
		t.Errorf("expected result from special.go, got:\n%s", raw)
	}

	// Without fixed_string, the same pattern should fail regex (bogus group).
	raw, err = tool.ExecuteContext(context.TODO(),
		`{"pattern": "foo.Bar(ctx", "path": "`+tmpDir+`", "output_mode": "content"}`)
	if err == nil {
		t.Errorf("expected regex error for invalid pattern 'foo.Bar(ctx', got output:\n%s", raw)
	} else {
		t.Logf("regex correctly error'd: %v", err)
	}
}

func TestGrep_MaxResults(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	// "hello" matches in many files; limit to 2 results.
	tool := GrepTool{}
	raw, err := tool.ExecuteContext(context.TODO(), `{"pattern": "hello", "path": "`+tmpDir+`", "output_mode": "content", "max_results": 2}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(raw), "\n")
	contentLines := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "...") {
			contentLines++
		}
	}
	if contentLines != 2 {
		t.Errorf("expected exactly 2 content lines with max_results=2, got %d. Output:\n%s", contentLines, raw)
	}
	if !strings.Contains(raw, "... (showing 2 of") {
		t.Errorf("expected truncation notice, got:\n%s", raw)
	}
}

func TestGrep_NoMatchMultipleFiles(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	tool := GrepTool{}
	raw, err := tool.ExecuteContext(context.TODO(), `{"pattern": "zxcvbnm_nonexistent", "path": "`+tmpDir+`", "output_mode": "content"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}
	if !strings.Contains(raw, "(no matches)") {
		t.Errorf("expected '(no matches)' for nonexistent pattern, got:\n%s", raw)
	}
}

func TestGrepTool_MaxResultsClamped(t *testing.T) {
	skipWithoutRg(t)
	tmpDir := setupGrepTestDir(t)

	// Create a file with 1200 matching lines to exceed any sane limit.
	big := filepath.Join(tmpDir, "big.log")
	var sb strings.Builder
	for i := 0; i < 1200; i++ {
		sb.WriteString("hello line\n")
	}
	if err := os.WriteFile(big, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}

	// Pass a wildly out-of-range max_results; the runtime must clamp to the
	// schema-declared upper bound (maxGrepMaxResults = 1000), not honor it.
	tool := GrepTool{}
	raw, err := tool.ExecuteContext(context.TODO(), `{"pattern": "hello", "path": "`+tmpDir+`", "output_mode": "content", "max_results": 100000}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}

	if !strings.Contains(raw, "... (showing 1000 of") {
		t.Errorf("expected clamp to 1000 results, got output without truncation marker:\n%.500s", raw)
	}
}

func TestGrep_IncludeIgnored(t *testing.T) {
	skipWithoutRg(t)
	dir := setupGrepTestDir(t)

	// A file excluded by .ignore, with content unique to it. (.ignore is
	// honored by rg even outside git repos, unlike .gitignore.)
	ignoreFile := "ignored.txt\n"
	if err := os.WriteFile(filepath.Join(dir, ".ignore"), []byte(ignoreFile), 0644); err != nil {
		t.Fatalf("Failed to write .ignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("hello ignored\n"), 0644); err != nil {
		t.Fatalf("Failed to write ignored.txt: %v", err)
	}

	// Default: .ignore-excluded file must not appear.
	raw, err := (&GrepTool{}).ExecuteContext(context.TODO(), `{"pattern": "hello ignored", "path": "`+dir+`"}`)
	if err != nil {
		t.Fatalf("Grep failed: %v", err)
	}
	if strings.Contains(raw, "ignored.txt") {
		t.Errorf("ignored file should be excluded by default, got:\n%s", raw)
	}

	// include_ignored=true: the excluded file is searched.
	raw, err = (&GrepTool{}).ExecuteContext(context.TODO(), `{"pattern": "hello ignored", "path": "`+dir+`", "include_ignored": true}`)
	if err != nil {
		t.Fatalf("Grep with include_ignored failed: %v", err)
	}
	if !strings.Contains(raw, "ignored.txt") {
		t.Errorf("expected ignored.txt with include_ignored=true, got:\n%s", raw)
	}
}
