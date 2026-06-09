package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/tools/hashline"
)

func TestEditTool_BasicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world\nfoo bar\nbaz qux\n"), 0644)

	tool := EditTool{}
	result, err := tool.ExecuteContext(context.TODO(),`{"path":"` + path + `","old_string":"foo bar","new_string":"replaced"}`)
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
	result, err := tool.ExecuteContext(context.TODO(),`{"path":"` + path + `","old_string":"aaa","new_string":"xxx","replace_all":true}`)
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
	_, err := tool.ExecuteContext(context.TODO(),`{"path":"` + path + `","old_string":"nonexistent","new_string":"xxx"}`)
	if err == nil {
		t.Fatal("expected error for missing old_string")
	}
}

func TestEditTool_IdenticalStrings(t *testing.T) {
	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(),`{"path":"/tmp/x","old_string":"same","new_string":"same"}`)
	if err == nil {
		t.Fatal("expected error when old_string == new_string")
	}
}

func TestEditTool_MultipleMatchesNoReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("aaa bbb aaa ccc aaa\n"), 0644)

	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(),`{"path":"` + path + `","old_string":"aaa","new_string":"xxx"}`)
	if err == nil {
		t.Fatal("expected error for multiple matches without replace_all")
	}
}

func TestEditTool_CreateNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	tool := EditTool{}
	result, err := tool.ExecuteContext(context.TODO(),`{"path":"` + path + `","old_string":"","new_string":"new content\n"}`)
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
	_, err := tool.ExecuteContext(context.TODO(),`{"path":"` + path + `","old_string":"","new_string":"new"}`)
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
	result, err := tool.ExecuteContext(context.TODO(),`{"path":"` + path + `","old_string":"\"Hello\"","new_string":"\"Hi\""}`)
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
	_, err := tool.ExecuteContext(context.TODO(),`{"path":"` + path + `","old_string":"hello","new_string":"xxx"}`)
	if err == nil {
		t.Fatal("expected error for binary file")
	}
}

func TestEditTool_FileNotFound(t *testing.T) {
	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(),`{"path":"/tmp/nonexistent_edit_test_file","old_string":"x","new_string":"y"}`)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEditTool_PreservesFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.txt")
	os.WriteFile(path, []byte("old content"), 0755)

	tool := EditTool{}
	_, err := tool.ExecuteContext(context.TODO(),`{"path":"` + path + `","old_string":"old","new_string":"new"}`)
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
// GetDiff tests — Hashline mode
// ============================================================================

func TestEditTool_GetDiff_Hashline_Replace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0644)

	store := hashline.NewSnapshotStore()
	tag := store.Record(path, "line1\nline2\nline3\n")

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	input := "¶" + path + "#" + tag + "\nreplace 2..2:\n+modified\n"
	diff, err := tool.GetDiff(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "-line2") || !strings.Contains(diff, "+modified") {
		t.Fatalf("diff should show replacement, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Hashline_Delete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("a\nb\nc\nd\ne\n"), 0644)

	store := hashline.NewSnapshotStore()
	tag := store.Record(path, "a\nb\nc\nd\ne\n")

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	input := "¶" + path + "#" + tag + "\ndelete 2..4\n"
	diff, err := tool.GetDiff(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "-b") || !strings.Contains(diff, "-c") || !strings.Contains(diff, "-d") {
		t.Fatalf("diff should show deleted lines, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Hashline_InsertBefore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("line1\nline2\n"), 0644)

	store := hashline.NewSnapshotStore()
	tag := store.Record(path, "line1\nline2\n")

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	input := "¶" + path + "#" + tag + "\ninsert before 2:\n+before\n"
	diff, err := tool.GetDiff(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "+before") {
		t.Fatalf("diff should show inserted line, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Hashline_InsertAfter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("line1\nline2\n"), 0644)

	store := hashline.NewSnapshotStore()
	tag := store.Record(path, "line1\nline2\n")

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	input := "¶" + path + "#" + tag + "\ninsert after 1:\n+inserted\n"
	diff, err := tool.GetDiff(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "+inserted") {
		t.Fatalf("diff should show inserted line, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Hashline_InsertHead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("body\n"), 0644)

	store := hashline.NewSnapshotStore()
	tag := store.Record(path, "body\n")

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	input := "¶" + path + "#" + tag + "\ninsert head:\n+header\n"
	diff, err := tool.GetDiff(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "+header") {
		t.Fatalf("diff should show header line, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Hashline_InsertTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("body\n"), 0644)

	store := hashline.NewSnapshotStore()
	tag := store.Record(path, "body\n")

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	input := "¶" + path + "#" + tag + "\ninsert tail:\n+footer\n"
	diff, err := tool.GetDiff(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "+footer") {
		t.Fatalf("diff should show footer line, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Hashline_MultipleOps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("a\nb\nc\n"), 0644)

	store := hashline.NewSnapshotStore()
	tag := store.Record(path, "a\nb\nc\n")

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	input := "¶" + path + "#" + tag + "\nreplace 1..1:\n+x\ninsert after 3:\n+y\n"
	diff, err := tool.GetDiff(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "-a") || !strings.Contains(diff, "+x") || !strings.Contains(diff, "+y") {
		t.Fatalf("diff should show both operations, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Hashline_MultipleSections(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	os.WriteFile(pathA, []byte("line1\nline2\n"), 0644)
	os.WriteFile(pathB, []byte("hello\nworld\n"), 0644)

	store := hashline.NewSnapshotStore()
	tagA := store.Record(pathA, "line1\nline2\n")
	tagB := store.Record(pathB, "hello\nworld\n")

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	input := "¶" + pathA + "#" + tagA + "\nreplace 2..2:\n+modified\n\n¶" + pathB + "#" + tagB + "\ninsert head:\n+greeting\n"
	diff, err := tool.GetDiff(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "-line2") || !strings.Contains(diff, "+modified") || !strings.Contains(diff, "+greeting") {
		t.Fatalf("diff should show changes in both files, got:\n%s", diff)
	}
	if !strings.Contains(diff, "a.txt") || !strings.Contains(diff, "b.txt") {
		t.Fatalf("diff should mention both files, got:\n%s", diff)
	}
}

func TestEditTool_GetDiff_Hashline_EmptyInput(t *testing.T) {
	store := hashline.NewSnapshotStore()

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	_, err := tool.GetDiff(context.TODO(), `{"input":""}`)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "input is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEditTool_GetDiff_Hashline_NoSnapshotStore(t *testing.T) {
	tool := &EditTool{}
	tool.SetHashlineMode(true, nil, 0)

	_, err := tool.GetDiff(context.TODO(), `{"input":"¶test.txt#abcd\nreplace 1..1:\n+x\n"}`)
	if err == nil {
		t.Fatal("expected error for missing snapshot store")
	}
	if !strings.Contains(err.Error(), "snapshot store") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEditTool_GetDiff_Hashline_StaleTag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello\n"), 0644)

	store := hashline.NewSnapshotStore()
	store.Record(path, "hello\n")

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	// Use a tag that doesn't exist — must be absolute path
	input := "¶" + path + "#ffff\nreplace 1..1:\n+x\n"
	_, err := tool.GetDiff(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err == nil {
		t.Fatal("expected error for stale tag")
	}
}

func TestEditTool_GetDiff_Hashline_NoSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello\n"), 0644)

	store := hashline.NewSnapshotStore()
	// Don't record the file in the store

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	input := "¶" + path + "#abcd\nreplace 1..1:\n+x\n"
	_, err := tool.GetDiff(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err == nil {
		t.Fatal("expected error for missing snapshot")
	}
	if !strings.Contains(err.Error(), "no snapshot") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEditTool_GetDiff_Hashline_InvalidInput(t *testing.T) {
	store := hashline.NewSnapshotStore()

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	_, err := tool.GetDiff(context.TODO(), `not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON args")
	}
}

func TestEditTool_GetDiff_Hashline_ParseError(t *testing.T) {
	store := hashline.NewSnapshotStore()

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	// Missing paragraph prefix — must be absolute path for the parse error test
	_, err := tool.GetDiff(context.TODO(), `{"input":"test.txt#abcd\nreplace 1..1:\n+x\n"}`)
	if err == nil {
		t.Fatal("expected parse error for invalid hashline")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEditTool_GetDiff_Hashline_HeadTailOnlyStaleTag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("body\n"), 0644)

	store := hashline.NewSnapshotStore()
	store.Record(path, "body\n")

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	// Head/tail-only operations with a stale tag should succeed (with warning)
	input := "¶" + path + "#ffff\ninsert head:\n+header\n"
	diff, err := tool.GetDiff(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err != nil {
		t.Fatalf("head/tail-only with stale tag should succeed (non-fatal): %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff, "+header") {
		t.Fatalf("diff should show inserted header, got:\n%s", diff)
	}
}

// ============================================================================
// GetDiff tests — Auto-detect mode (hashline input in ExecuteContext)
// ============================================================================

func TestEditTool_ExecuteContext_AutoHashline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello\n"), 0644)

	store := hashline.NewSnapshotStore()
	tag := store.Record(path, "hello\n")

	tool := &EditTool{}
	tool.SetHashlineMode(true, store, 0)

	input := "¶" + path + "#" + tag + "\nreplace 1..1:\n+world\n"
	result, err := tool.ExecuteContext(context.TODO(), `{"input":"`+escapeJSON(input)+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}
	if !strings.Contains(result, "Edited") {
		t.Fatalf("result should mention edited file, got: %s", result)
	}

	content, _ := os.ReadFile(path)
	if string(content) != "world\n" {
		t.Fatalf("expected %q, got %q", "world\n", string(content))
	}
}

func TestEditTool_ExecuteContext_AutoHashline_RequiresHashlineMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello\n"), 0644)

	// Hashline mode NOT enabled — should fall through to legacy mode
	// with path/old_string/new_string format
	tool := &EditTool{}
	result, err := tool.ExecuteContext(context.TODO(), `{"path":"`+path+`","old_string":"hello","new_string":"world"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	content, _ := os.ReadFile(path)
	if string(content) != "world\n" {
		t.Fatalf("expected %q, got %q", "world\n", string(content))
	}
}

// escapeJSON escapes a string for use in a JSON string value.
func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}
