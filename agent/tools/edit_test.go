package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEditTool_BasicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world\nfoo bar\nbaz qux\n"), 0644)

	tool := EditTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"foo bar","new_string":"replaced"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result == "" {
		t.Fatal("expected non-empty result")
	}

	content, _ := os.ReadFile(path)
	expected := "hello world\nreplaced\nbaz qux\n"
	if string(content) != expected {
		t.Fatalf("expected %q, got %q", expected, string(content))
	}
}

func TestEditTool_ReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("aaa bbb aaa ccc aaa\n"), 0644)

	tool := EditTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"aaa","new_string":"xxx","replace_all":true}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	content, _ := os.ReadFile(path)
	expected := "xxx bbb xxx ccc xxx\n"
	if string(content) != expected {
		t.Fatalf("expected %q, got %q", expected, string(content))
	}
}

func TestEditTool_OldStringNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world\n"), 0644)

	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"nonexistent","new_string":"xxx"}`)
	if err == nil {
		t.Fatal("expected error for missing old_string")
	}
}

func TestEditTool_IdenticalStrings(t *testing.T) {
	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(), `{"path":"/tmp/x","old_string":"same","new_string":"same"}`)
	if err == nil {
		t.Fatal("expected error when old_string == new_string")
	}
}

func TestEditTool_MultipleMatchesNoReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("aaa bbb aaa ccc aaa\n"), 0644)

	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"aaa","new_string":"xxx"}`)
	if err == nil {
		t.Fatal("expected error for multiple matches without replace_all")
	}
}

func TestEditTool_CreateNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	tool := EditTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"","new_string":"new content\n"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	content, _ := os.ReadFile(path)
	if string(content) != "new content\n" {
		t.Fatalf("expected %q, got %q", "new content\n", string(content))
	}
}

func TestEditTool_CreateExistingFileError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	os.WriteFile(path, []byte("existing"), 0644)

	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"","new_string":"new"}`)
	if err == nil {
		t.Fatal("expected error when creating file that already exists")
	}
}

func TestEditTool_CurlyQuoteNormalization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quotes.txt")
	// File contains curly quotes
	os.WriteFile(path, []byte("He said \u201CHello\u201D and \u2018world\u2019\n"), 0644)

	tool := EditTool{}
	// Search with straight quotes (as LLM would produce)
	result, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"\"Hello\"","new_string":"\"Hi\""}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	content, _ := os.ReadFile(path)
	// The curly quotes in the matched portion should be replaced with the literal new_string
	got := string(content)
	if got != "He said \"Hi\" and \u2018world\u2019\n" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestEditTool_BinaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binary.bin")
	data := []byte("hello\x00world")
	os.WriteFile(path, data, 0644)

	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"hello","new_string":"xxx"}`)
	if err == nil {
		t.Fatal("expected error for binary file")
	}
}

func TestEditTool_FileNotFound(t *testing.T) {
	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(), `{"path":"/tmp/nonexistent_edit_test_file","old_string":"x","new_string":"y"}`)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEditTool_PreservesFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.txt")
	os.WriteFile(path, []byte("old content"), 0755)

	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"old","new_string":"new"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0755 {
		t.Fatalf("expected permissions 0755, got %v", info.Mode().Perm())
	}
}

// ============================================================================
// GetDiff tests — Legacy mode
// ============================================================================

func TestEditTool_GetDiff_Legacy_BasicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world\nfoo bar\nbaz qux\n"), 0644)

	tool := &EditTool{}
	diff, err := tool.GetDiff(context.TODO(), `{"path":"`+path+`","old_string":"foo bar","new_string":"replaced"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	// Diff format: "-N | old_content" and "+N | new_content"
	if !strings.Contains(diff, "foo bar") || !strings.Contains(diff, "replaced") {
		t.Fatalf("diff should show replacement, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Legacy_ReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("aaa bbb aaa ccc aaa\n"), 0644)

	tool := &EditTool{}
	diff, err := tool.GetDiff(context.TODO(), `{"path":"`+path+`","old_string":"aaa","new_string":"xxx","replace_all":true}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "aaa") || !strings.Contains(diff, "xxx") {
		t.Fatalf("diff should show replacement, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Legacy_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	tool := &EditTool{}
	diff, err := tool.GetDiff(context.TODO(), `{"path":"`+path+`","old_string":"","new_string":"new content\n"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "new file") {
		t.Fatalf("diff should mention new file, got:\n%s", diff)
	}
	if !strings.Contains(diff, "new content") {
		t.Fatalf("diff should show content, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Legacy_OldStringNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world\n"), 0644)

	tool := &EditTool{}
	_, err := tool.GetDiff(context.TODO(), `{"path":"`+path+`","old_string":"nonexistent","new_string":"xxx"}`)
	if err == nil {
		t.Fatal("expected error for missing old_string")
	}
	if !strings.Contains(err.Error(), "old_string not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEditTool_GetDiff_Legacy_FileNotFound(t *testing.T) {
	tool := &EditTool{}
	_, err := tool.GetDiff(context.TODO(), `{"path":"/tmp/nonexistent_edit_test_file","old_string":"x","new_string":"y"}`)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEditTool_GetDiff_Legacy_BinaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binary.bin")
	os.WriteFile(path, []byte("hello\x00world"), 0644)

	tool := &EditTool{}
	_, err := tool.GetDiff(context.TODO(), `{"path":"`+path+`","old_string":"hello","new_string":"xxx"}`)
	if err == nil {
		t.Fatal("expected error for binary file")
	}
	if !strings.Contains(err.Error(), "binary") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEditTool_GetDiff_Legacy_FileTooLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")
	// Create a file larger than maxFileSize (256KB)
	large := make([]byte, maxFileSize+1)
	for i := range large {
		large[i] = 'x'
	}
	os.WriteFile(path, large, 0644)

	tool := &EditTool{}
	_, err := tool.GetDiff(context.TODO(), `{"path":"`+path+`","old_string":"xxx","new_string":"yyy"}`)
	if err == nil {
		t.Fatal("expected error for file too large")
	}
}

func TestEditTool_GetDiff_Legacy_CurlyQuoteNormalization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quotes.txt")
	// File contains curly quotes
	os.WriteFile(path, []byte("He said \u201CHello\u201D and \u2018world\u2019\n"), 0644)

	tool := &EditTool{}
	// Search with straight quotes
	diff, err := tool.GetDiff(context.TODO(), `{"path":"`+path+`","old_string":"\"Hello\"","new_string":"\"Hi\""}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "Hello") || !strings.Contains(diff, "Hi") {
		t.Fatalf("diff should show quote replacement, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Legacy_InvalidArgs(t *testing.T) {
	tool := &EditTool{}
	_, err := tool.GetDiff(context.TODO(), `not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON args")
	}
}

// ============================================================================

func TestEditTool_TrailingWhitespaceFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ws.txt")
	// "foo bar" is followed by " baz" on the same line — no "foo bar\n" in file.
	if err := os.WriteFile(path, []byte("hello world\nfoo bar baz\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	tool := EditTool{}
	// Model copies old_string with a trailing newline (thinks it's a whole
	// line); exact match fails, the trailing-whitespace fallback kicks in and
	// replaces only "foo bar", leaving " baz" intact.
	result, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"foo bar\n","new_string":"replaced"}`)
	if err != nil {
		t.Fatalf("trailing-whitespace fallback failed: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	content, _ := os.ReadFile(path)
	expected := "hello world\nreplaced baz\n"
	if string(content) != expected {
		t.Fatalf("expected %q, got %q", expected, string(content))
	}
}

func TestEditTool_ExactMatchWinsOverFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ws2.txt")
	// "foo" appears as a prefix of "foo bar"; searching "foo" must hit the
	// exact first occurrence, not some trimmed variant elsewhere.
	if err := os.WriteFile(path, []byte("foo bar\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	tool := EditTool{}
	// Exact match "foo" exists (inside "foo bar") — first occurrence replaced.
	_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"foo","new_string":"X"}`)
	if err != nil {
		t.Fatalf("exact match should succeed: %v", err)
	}

	content, _ := os.ReadFile(path)
	expected := "X bar\n"
	if string(content) != expected {
		t.Fatalf("expected %q, got %q", expected, string(content))
	}
}

func TestEditTool_FallbackAmbiguityCaughtByUniqueness(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ws3.txt")
	// "foo" occurs twice in the file (as a prefix of "foo bar" and "foo baz"),
	// but "foo\n" does not exist — the model's old_string fails exactly and
	// falls back to "foo", which is non-unique. The edit must refuse rather
	// than silently pick the first occurrence.
	if err := os.WriteFile(path, []byte("foo bar\nfoo baz\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"foo\n","new_string":"X"}`)
	if err == nil {
		t.Fatal("expected ambiguity error for non-unique fallback match")
	}
	if !strings.Contains(err.Error(), "matches 2 locations") {
		t.Errorf("expected uniqueness error, got: %v", err)
	}

	content, _ := os.ReadFile(path)
	if string(content) != "foo bar\nfoo baz\n" {
		t.Fatalf("file must be untouched after refused edit, got: %q", string(content))
	}
}

func TestEditTool_TolerantMatchAnnotated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tolerant.txt")
	if err := os.WriteFile(path, []byte("hello world\nfoo bar baz\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	tool := EditTool{}
	// Trailing-newline fallback triggers a tolerant match; the success message
	// must surface which actual string was matched so the model can verify.
	result, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"foo bar\n","new_string":"replaced"}`)
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	if !strings.Contains(result, "tolerant match") || !strings.Contains(result, "foo bar") {
		t.Errorf("expected tolerant-match annotation, got: %q", result)
	}
}

func TestEditTool_NotFoundGuidesReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.txt")
	os.WriteFile(path, []byte("hello world\n"), 0644)

	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"nope","new_string":"x"}`)
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "read tool to reload") {
		t.Errorf("expected reload guidance, got: %v", err)
	}
}

func TestEditTool_ParallelAlwaysTrue(t *testing.T) {
	tool := EditTool{}
	if !tool.Parallel() {
		t.Error("EditTool must declare parallel support (per-path locks handle conflicts)")
	}
	if tool.NeedsConfirmation() {
		t.Error("EditTool must never require interactive confirmation")
	}
}

func TestEditTool_ConcurrentSameFileSerialized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "same.txt")
	if err := os.WriteFile(path, []byte("line0\nline1\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	tool := EditTool{}
	var wg sync.WaitGroup
	errs := make(chan error, 2)

	// Two edits to DIFFERENT parts of the same file run concurrently; the
	// per-path lock serializes the read-modify-write cycles so both edits
	// land — without the lock, one edit's write would clobber the other.
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"line0","new_string":"a0"}`)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"line1","new_string":"a1"}`)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent edit failed: %v", err)
		}
	}

	content, _ := os.ReadFile(path)
	if got := string(content); got != "a0\na1\n" {
		t.Fatalf("expected both edits applied (%q), got %q", "a0\na1\n", got)
	}
}

func TestEditTool_ConcurrentDifferentFiles(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	for _, p := range []string{pathA, pathB} {
		if err := os.WriteFile(p, []byte("hello\n"), 0644); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}
	}

	tool := EditTool{}
	var wg sync.WaitGroup
	errs := make(chan error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+pathA+`","old_string":"hello","new_string":"world"}`)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+pathB+`","old_string":"hello","new_string":"tachi"}`)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent edit failed: %v", err)
		}
	}

	contentA, _ := os.ReadFile(pathA)
	contentB, _ := os.ReadFile(pathB)
	if string(contentA) != "world\n" || string(contentB) != "tachi\n" {
		t.Fatalf("expected both edits applied, got %q and %q", string(contentA), string(contentB))
	}
}

func TestEditTool_ConcurrentCreateAndEditViaSymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatalf("failed to create real dir: %v", err)
	}
	linkDir := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	tool := EditTool{}
	var wg sync.WaitGroup
	errs := make(chan error, 2)

	// The same to-be-created file reached via two aliases (symlinked parent):
	// both calls must serialize on ONE lock key, so create + edit both land.
	realPath := filepath.Join(realDir, "new.txt")
	linkPath := filepath.Join(linkDir, "new.txt")
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+realPath+`","old_string":"","new_string":"base"}`)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := tool.ExecuteContext(context.TODO(), `{"path":"`+linkPath+`","old_string":"base","new_string":"edited"}`)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent alias edit failed: %v", err)
		}
	}

	content, _ := os.ReadFile(realPath)
	if got := string(content); got != "edited" {
		t.Fatalf("expected final content %q, got %q", "edited", got)
	}
}

// ============================================================================
