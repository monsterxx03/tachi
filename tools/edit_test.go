package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEditTool_BasicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world\nfoo bar\nbaz qux\n"), 0644)

	tool := EditTool{}
	result, err := tool.Execute(`{"file_path":"` + path + `","old_string":"foo bar","new_string":"replaced"}`)
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
	result, err := tool.Execute(`{"file_path":"` + path + `","old_string":"aaa","new_string":"xxx","replace_all":true}`)
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
	_, err := tool.Execute(`{"file_path":"` + path + `","old_string":"nonexistent","new_string":"xxx"}`)
	if err == nil {
		t.Fatal("expected error for missing old_string")
	}
}

func TestEditTool_IdenticalStrings(t *testing.T) {
	tool := EditTool{}
	_, err := tool.Execute(`{"file_path":"/tmp/x","old_string":"same","new_string":"same"}`)
	if err == nil {
		t.Fatal("expected error when old_string == new_string")
	}
}

func TestEditTool_MultipleMatchesNoReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("aaa bbb aaa ccc aaa\n"), 0644)

	tool := EditTool{}
	_, err := tool.Execute(`{"file_path":"` + path + `","old_string":"aaa","new_string":"xxx"}`)
	if err == nil {
		t.Fatal("expected error for multiple matches without replace_all")
	}
}

func TestEditTool_CreateNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	tool := EditTool{}
	result, err := tool.Execute(`{"file_path":"` + path + `","old_string":"","new_string":"new content\n"}`)
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
	_, err := tool.Execute(`{"file_path":"` + path + `","old_string":"","new_string":"new"}`)
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
	result, err := tool.Execute(`{"file_path":"` + path + `","old_string":"\"Hello\"","new_string":"\"Hi\""}`)
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
	_, err := tool.Execute(`{"file_path":"` + path + `","old_string":"hello","new_string":"xxx"}`)
	if err == nil {
		t.Fatal("expected error for binary file")
	}
}

func TestEditTool_FileNotFound(t *testing.T) {
	tool := EditTool{}
	_, err := tool.Execute(`{"file_path":"/tmp/nonexistent_edit_test_file","old_string":"x","new_string":"y"}`)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEditTool_PreservesFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.txt")
	os.WriteFile(path, []byte("old content"), 0755)

	tool := EditTool{}
	_, err := tool.Execute(`{"file_path":"` + path + `","old_string":"old","new_string":"new"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0755 {
		t.Fatalf("expected permissions 0755, got %v", info.Mode().Perm())
	}
}
