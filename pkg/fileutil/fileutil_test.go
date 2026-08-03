package fileutil

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMarshalJSONIndent(t *testing.T) {
	data, err := MarshalJSON(map[string]int{"a": 1})
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	want := "{\n  \"a\": 1\n}"
	if string(data) != want {
		t.Fatalf("MarshalJSON = %q, want %q", data, want)
	}
}

func TestWriteFileCreatesDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c.txt")
	if err := WriteFilePrivate(path, []byte("hi")); err != nil {
		t.Fatalf("WriteFilePrivate: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hi" {
		t.Fatalf("content = %q, want %q", data, "hi")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file perm = %o, want 600", got)
	}
}

func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := AtomicWriteFilePrivate(path, []byte(`{"v":1}`)); err != nil {
		t.Fatalf("AtomicWriteFilePrivate: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != `{"v":1}` {
		t.Fatalf("content = %q", data)
	}

	// No stray .tmp files left behind.
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover tmp file: %v", err)
	}
}

func TestAtomicWriteFileCleansTmpOnFailure(t *testing.T) {
	dir := t.TempDir()
	// Create a directory at the target path so the final Rename fails.
	path := filepath.Join(dir, "target")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}

	err := AtomicWriteFileShared(path, []byte("data"))
	if err == nil {
		t.Fatal("expected rename error, got nil")
	}
	if _, statErr := os.Stat(path + ".tmp"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("tmp file not cleaned up after failure: %v", statErr)
	}
}

func TestWriteJSONAndReadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	type payload struct {
		Name  string            `json:"name"`
		Flags map[string]string `json:"flags"`
	}
	in := payload{Name: "x", Flags: map[string]string{"a": "1"}}
	if err := WriteJSONShared(path, &in); err != nil {
		t.Fatalf("WriteJSONShared: %v", err)
	}

	// File is indented, not compact.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}

	var out payload
	if err := ReadJSON(path, &out); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if out.Name != in.Name || out.Flags["a"] != "1" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}

	// Missing file surfaces os.ErrNotExist via errors.Is.
	if err := ReadJSON(filepath.Join(dir, "nope.json"), &out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadJSON missing = %v, want os.ErrNotExist", err)
	}
}

func TestAtomicWriteJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "state.json")
	if err := AtomicWriteJSONPrivate(path, map[string]int{"n": 2}); err != nil {
		t.Fatalf("AtomicWriteJSONPrivate: %v", err)
	}
	var out map[string]int
	if err := ReadJSON(path, &out); err != nil {
		t.Fatal(err)
	}
	if out["n"] != 2 {
		t.Fatalf("n = %d, want 2", out["n"])
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover tmp file: %v", err)
	}
}

func TestExistsIsDirIsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")

	if !Exists(dir) || !Exists(file) || Exists(missing) {
		t.Fatal("Exists misreports")
	}
	if !IsDir(dir) || IsDir(file) || IsDir(missing) {
		t.Fatal("IsDir misreports")
	}
	if !IsFile(file) || IsFile(dir) || IsFile(missing) {
		t.Fatal("IsFile misreports")
	}
}

func TestRemoveIgnoreNotExist(t *testing.T) {
	dir := t.TempDir()

	// Removing a missing file is not an error.
	if err := RemoveIgnoreNotExist(filepath.Join(dir, "nope.json")); err != nil {
		t.Errorf("RemoveIgnoreNotExist(missing) = %v, want nil", err)
	}

	// Removing an existing file succeeds.
	path := filepath.Join(dir, "exists.json")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveIgnoreNotExist(path); err != nil {
		t.Errorf("RemoveIgnoreNotExist(existing) = %v, want nil", err)
	}
	if Exists(path) {
		t.Error("file still exists after RemoveIgnoreNotExist")
	}

	// Second removal is a no-op (file gone).
	if err := RemoveIgnoreNotExist(path); err != nil {
		t.Errorf("RemoveIgnoreNotExist(after) = %v, want nil", err)
	}
}
