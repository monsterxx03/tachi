package hashline

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- ComputeTag ---

func TestComputeTag(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"short", "hello"},
		{"multiline", "line1\nline2\nline3"},
		{"unicode", "你好，世界"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tag := ComputeTag(tt.content)
			if len(tag) != 4 {
				t.Fatalf("ComputeTag() returned %q (len=%d), want 4 chars", tag, len(tag))
			}
			for _, c := range tag {
				if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
					t.Fatalf("ComputeTag() returned %q, want hex chars only", tag)
				}
			}
		})
	}
}

func TestComputeTagDeterministic(t *testing.T) {
	tag1 := ComputeTag("hello world")
	tag2 := ComputeTag("hello world")
	if tag1 != tag2 {
		t.Fatalf("ComputeTag() not deterministic: %s vs %s", tag1, tag2)
	}
}

// --- SnapshotStore ---

func TestSnapshotStoreRecordAndVerify(t *testing.T) {
	s := NewSnapshotStore()
	path := "/tmp/test.go"
	content := "package main\n"
	tag := s.Record(path, content)

	if len(tag) != 4 {
		t.Fatalf("Record() returned tag %q, want 4 chars", tag)
	}

	if err := s.Verify(path, tag, content); err != nil {
		t.Fatalf("Verify() failed: %v", err)
	}
}

func TestSnapshotStoreVerifyWrongTag(t *testing.T) {
	s := NewSnapshotStore()
	s.Record("/tmp/test.go", "package main\n")
	err := s.Verify("/tmp/test.go", "xyz", "package main\n")
	if err == nil {
		t.Fatal("expected error for wrong tag, got nil")
	}
}

func TestSnapshotStoreVerifyNoSnapshot(t *testing.T) {
	s := NewSnapshotStore()
	err := s.Verify("/tmp/unknown.go", "abc", "")
	if err == nil {
		t.Fatal("expected error for unknown path, got nil")
	}
}

func TestSnapshotStoreRecordDedup(t *testing.T) {
	s := NewSnapshotStore()
	tag1 := s.Record("/tmp/test.go", "hello")
	tag2 := s.Record("/tmp/test.go", "hello")
	if tag1 != tag2 {
		t.Fatalf("Record() should return same tag for same content: %s vs %s", tag1, tag2)
	}
}

func TestSnapshotStoreInvalidate(t *testing.T) {
	s := NewSnapshotStore()
	path := "/tmp/test.go"
	s.Record(path, "hello")
	s.Invalidate(path)
	err := s.Verify(path, "abc", "")
	if err == nil {
		t.Fatal("expected error after Invalidate(), got nil")
	}
}

func TestSnapshotStoreGetTag(t *testing.T) {
	s := NewSnapshotStore()
	tag := s.Record("/tmp/test.go", "content")
	got := s.GetTag("/tmp/test.go")
	if got != tag {
		t.Fatalf("GetTag() = %q, want %q", got, tag)
	}
	if got := s.GetTag("/nonexistent"); got != "" {
		t.Fatalf("GetTag() for unknown = %q, want empty", got)
	}
}

func TestSnapshotStoreConcurrent(t *testing.T) {
	s := NewSnapshotStore()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			path := filepath.Join("/tmp", "test.go")
			content := "content"
			tag := s.Record(path, content)
			s.Verify(path, tag, content)
			s.GetTag(path)
			s.Invalidate(path)
		}(i)
	}
	wg.Wait()
}

// --- Parser ---

func TestParseReplace(t *testing.T) {
	input := "¶file.go#a1f0\nreplace 2..2:\n+new line 2\n"
	sections, err := Parse(input, "/test")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if len(sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(sections))
	}
	sec := sections[0]
	if !strings.HasSuffix(sec.Path, "file.go") {
		t.Fatalf("unexpected path: %s", sec.Path)
	}
	if sec.Tag != "a1f0" {
		t.Fatalf("unexpected tag: %s", sec.Tag)
	}
	if len(sec.Operations) != 1 {
		t.Fatalf("expected 1 operation, got %d", len(sec.Operations))
	}
	op := sec.Operations[0]
	if op.Type != OpReplace || op.Start != 2 || op.End != 2 {
		t.Fatalf("operation: type=%d start=%d end=%d, want replace(2,2)", op.Type, op.Start, op.End)
	}
	if len(op.Body) != 1 || op.Body[0] != "new line 2" {
		t.Fatalf("body = %q, want [\"new line 2\"]", op.Body)
	}
}

func TestParseDelete(t *testing.T) {
	input := "¶file.go#a1f0\ndelete 3..5\n"
	sections, err := Parse(input, "/test")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	op := sections[0].Operations[0]
	if op.Type != OpDelete || op.Start != 3 || op.End != 5 {
		t.Fatalf("operation: type=%d start=%d end=%d", op.Type, op.Start, op.End)
	}
}

func TestParseInsertBefore(t *testing.T) {
	input := "¶file.go#a1f0\ninsert before 2:\n+header line\n"
	sections, err := Parse(input, "/test")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	op := sections[0].Operations[0]
	if op.Type != OpInsertBefore || op.Start != 2 {
		t.Fatalf("operation: type=%d start=%d", op.Type, op.Start)
	}
	if len(op.Body) != 1 || op.Body[0] != "header line" {
		t.Fatalf("body = %q", op.Body)
	}
}

func TestParseInsertAfter(t *testing.T) {
	input := "¶file.go#a1f0\ninsert after 3:\n+  new line\n"
	sections, err := Parse(input, "/test")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	op := sections[0].Operations[0]
	if op.Type != OpInsertAfter || op.Start != 3 {
		t.Fatalf("operation: type=%d start=%d", op.Type, op.Start)
	}
	if op.Body[0] != "  new line" {
		t.Fatalf("body[0] = %q, want \"  new line\"", op.Body[0])
	}
}

func TestParseInsertHead(t *testing.T) {
	input := "¶file.go#a1f0\ninsert head:\n+// generated\n+\n+package main\n"
	sections, err := Parse(input, "/test")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	op := sections[0].Operations[0]
	if op.Type != OpInsertHead {
		t.Fatalf("operation type = %d, want OpInsertHead", op.Type)
	}
	if len(op.Body) != 3 || op.Body[0] != "// generated" || op.Body[1] != "" || op.Body[2] != "package main" {
		t.Fatalf("body = %q, want [\"// generated\" \"\" \"package main\"]", op.Body)
	}
}

func TestParseInsertTail(t *testing.T) {
	input := "¶file.go#a1f0\ninsert tail:\n+end\n"
	sections, err := Parse(input, "/test")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if sections[0].Operations[0].Type != OpInsertTail {
		t.Fatal("expected OpInsertTail")
	}
}

func TestParseMultipleSections(t *testing.T) {
	input := "¶main.go#a1f0\nreplace 5..5:\n+new\n\n¶util.go#b2c0\ninsert after 10:\n+extra\n"
	sections, err := Parse(input, "/test")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if len(sections) != 2 {
		t.Fatalf("expected 2 sections, got %d", len(sections))
	}
}

func TestParseMultipleOperations(t *testing.T) {
	input := "¶file.go#a1f0\nreplace 3..3:\n+changed\ninsert tail:\n+end\n"
	sections, err := Parse(input, "/test")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if len(sections[0].Operations) != 2 {
		t.Fatalf("expected 2 operations, got %d", len(sections[0].Operations))
	}
}

func TestParseEmptyInput(t *testing.T) {
	if _, err := Parse("", "/test"); err == nil {
		t.Fatal("expected error for empty input")
	}
	if _, err := Parse("  \n", "/test"); err == nil {
		t.Fatal("expected error for whitespace input")
	}
}

func TestParseInvalidHeader(t *testing.T) {
	if _, err := Parse("file.go#a1f0\nreplace 1..1:\n+content\n", "/test"); err == nil {
		t.Fatal("expected error for missing ¶ prefix")
	}
	if _, err := Parse("¶file.go#a1f0\nreplace 1..1:\n+content\n", "/test"); err != nil {
		t.Fatal("4-char tag should be accepted but got error")
	}
	if _, err := Parse("¶file.go#abcde\nreplace 1..1:\n+content\n", "/test"); err != nil {
		t.Fatal("5-char tag should be accepted")
	}
	if _, err := Parse("¶file.go#a1f0xyz\nreplace 1..1:\n+content\n", "/test"); err == nil {
		t.Fatal("expected error for non-hex tag")
	}
}

func TestParseNoOperations(t *testing.T) {
	if _, err := Parse("¶file.go#a1f0\n", "/test"); err == nil {
		t.Fatal("expected error for no operations")
	}
}

// --- Patcher (with FakeFileSystem) ---

type fakeFileSystem struct {
	files map[string]string
}

func newFakeFS() *fakeFileSystem {
	return &fakeFileSystem{files: make(map[string]string)}
}

func (f *fakeFileSystem) Read(path string) (string, error) {
	content, ok := f.files[path]
	if !ok {
		return "", os.ErrNotExist
	}
	return content, nil
}

func (f *fakeFileSystem) Write(path string, content string) error {
	f.files[path] = content
	return nil
}

func (f *fakeFileSystem) Exists(path string) (bool, error) {
	_, ok := f.files[path]
	return ok, nil
}

func TestPatcherReplace(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "line1\nline2\nline3\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path: path,
		Tag:  tag,
		Operations: []Operation{
			{Type: OpReplace, Start: 2, End: 2, Body: []string{"modified"}},
		},
	}

	prepared, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}

	want := "line1\nmodified\nline3\n"
	if prepared.NewContent != want {
		t.Fatalf("NewContent = %q, want %q", prepared.NewContent, want)
	}
}

func TestPatcherReplaceMultipleLines(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "a\nb\nc\nd\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path: path,
		Tag:  tag,
		Operations: []Operation{
			{Type: OpReplace, Start: 2, End: 3, Body: []string{"x", "y", "z"}},
		},
	}

	prepared, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}

	want := "a\nx\ny\nz\nd\n"
	if prepared.NewContent != want {
		t.Fatalf("NewContent = %q, want %q", prepared.NewContent, want)
	}
}

func TestPatcherDelete(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "a\nb\nc\nd\ne\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path: path,
		Tag:  tag,
		Operations: []Operation{
			{Type: OpDelete, Start: 2, End: 4},
		},
	}

	prepared, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}

	want := "a\ne\n"
	if prepared.NewContent != want {
		t.Fatalf("NewContent = %q, want %q", prepared.NewContent, want)
	}
}

func TestPatcherInsertBefore(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "line1\nline2\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path: path,
		Tag:  tag,
		Operations: []Operation{
			{Type: OpInsertBefore, Start: 2, Body: []string{"before"}},
		},
	}

	prepared, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}

	want := "line1\nbefore\nline2\n"
	if prepared.NewContent != want {
		t.Fatalf("NewContent = %q, want %q", prepared.NewContent, want)
	}
}

func TestPatcherInsertAfter(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "line1\nline2\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path: path,
		Tag:  tag,
		Operations: []Operation{
			{Type: OpInsertAfter, Start: 1, Body: []string{"inserted"}},
		},
	}

	prepared, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}

	want := "line1\ninserted\nline2\n"
	if prepared.NewContent != want {
		t.Fatalf("NewContent = %q, want %q", prepared.NewContent, want)
	}
}

func TestPatcherInsertHead(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "body\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path: path,
		Tag:  tag,
		Operations: []Operation{
			{Type: OpInsertHead, Body: []string{"header"}},
		},
	}

	prepared, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}

	want := "header\nbody\n"
	if prepared.NewContent != want {
		t.Fatalf("NewContent = %q, want %q", prepared.NewContent, want)
	}
}

func TestPatcherInsertTail(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "body\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path: path,
		Tag:  tag,
		Operations: []Operation{
			{Type: OpInsertTail, Body: []string{"footer"}},
		},
	}

	prepared, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}

	want := "body\nfooter\n"
	if prepared.NewContent != want {
		t.Fatalf("NewContent = %q, want %q", prepared.NewContent, want)
	}
}

func TestPatcherMultipleOperationsSameSection(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "a\nb\nc\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path: path,
		Tag:  tag,
		Operations: []Operation{
			{Type: OpReplace, Start: 1, End: 1, Body: []string{"x"}},
			{Type: OpInsertAfter, Start: 3, Body: []string{"y"}},
		},
	}

	prepared, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}

	want := "x\nb\nc\ny\n"
	if prepared.NewContent != want {
		t.Fatalf("NewContent = %q, want %q", prepared.NewContent, want)
	}
}

func TestPatcherTagMismatch(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "content\n"
	snapshots.Record(path, "content\n")

	section := Section{
		Path: path,
		Tag:  "xyz",
		Operations: []Operation{
			{Type: OpReplace, Start: 1, End: 1, Body: []string{"new"}},
		},
	}

	_, err := patcher.Prepare(section)
	if err == nil {
		t.Fatal("expected error for tag mismatch")
	}
}

func TestPatcherApply(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "line1\nline2\nline3\n"
	tag := snapshots.Record(path, fs.files[path])

	input := "¶file.go#" + tag + "\nreplace 2..2:\n+modified\n"
	results, err := patcher.Apply(input, "/test")
	if err != nil {
		t.Fatalf("Apply() failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	saved, _ := fs.Read(path)
	if !strings.Contains(saved, "modified") {
		t.Fatalf("Apply() didn't update content:\n%s", saved)
	}
}

func TestPatcherApplyMultipleFiles(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	fs.files["/test/a.go"] = "a\nb\n"
	fs.files["/test/b.go"] = "c\nd\n"
	tagA := snapshots.Record("/test/a.go", fs.files["/test/a.go"])
	tagB := snapshots.Record("/test/b.go", fs.files["/test/b.go"])

	input := "¶a.go#" + tagA + "\nreplace 2..2:\n+x\n\n¶b.go#" + tagB + "\ninsert tail:\n+y\n"

	results, err := patcher.Apply(input, "/test")
	if err != nil {
		t.Fatalf("Apply() failed: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if !strings.Contains(fs.files["/test/a.go"], "\nx\n") {
		t.Fatal("a.go not updated")
	}
	if !strings.Contains(fs.files["/test/b.go"], "y\n") {
		t.Fatal("b.go not updated")
	}
}

func TestPatcherNoTrailingNewline(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	content := "a\nb\nc"
	fs.files[path] = content
	tag := snapshots.Record(path, content)

	section := Section{
		Path: path,
		Tag:  tag,
		Operations: []Operation{
			{Type: OpInsertTail, Body: []string{"d"}},
		},
	}

	prepared, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}

	want := "a\nb\nc\nd"
	if prepared.NewContent != want {
		t.Fatalf("NewContent = %q, want %q", prepared.NewContent, want)
	}
}

func TestPatcherEditThenCommit(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	content := "line1\nline2\n"
	fs.files[path] = content
	tag := snapshots.Record(path, content)

	section := Section{
		Path: path,
		Tag:  tag,
		Operations: []Operation{
			{Type: OpReplace, Start: 2, End: 2, Body: []string{"modified"}},
		},
	}

	prepared, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}

	result, err := patcher.Commit(prepared)
	if err != nil {
		t.Fatalf("Commit() failed: %v", err)
	}

	if result.Path != path {
		t.Fatalf("result path = %q, want %q", result.Path, path)
	}
	if result.Tag == "" {
		t.Fatal("result tag is empty")
	}

	saved, _ := fs.Read(path)
	if !strings.Contains(saved, "modified") {
		t.Fatalf("committed content missing update:\n%s", saved)
	}
}

func TestPatcherExternalModification(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "original\n"
	tag := snapshots.Record(path, "original\n")

	// External modification
	fs.files[path] = "modified\n"

	section := Section{
		Path: path,
		Tag:  tag,
		Operations: []Operation{
			{Type: OpReplace, Start: 1, End: 1, Body: []string{"new"}},
		},
	}

	_, err := patcher.Prepare(section)
	if err == nil {
		t.Fatal("expected error after external modification")
	}
}

func TestOSFileSystem(t *testing.T) {
	fs := OSFileSystem{}
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	err := fs.Write(path, "hello\nworld\n")
	if err != nil {
		t.Fatalf("Write() failed: %v", err)
	}

	exists, err := fs.Exists(path)
	if err != nil {
		t.Fatalf("Exists() failed: %v", err)
	}
	if !exists {
		t.Fatal("Exists() returned false after write")
	}

	content, err := fs.Read(path)
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}
	if content != "hello\nworld\n" {
		t.Fatalf("Read() = %q, want %q", content, "hello\nworld\n")
	}

	exists, err = fs.Exists("/nonexistent/path")
	if err != nil {
		t.Fatalf("Exists() for nonexistent failed: %v", err)
	}
	if exists {
		t.Fatal("Exists() returned true for nonexistent")
	}
}

func TestOSFileSystemLineEndings(t *testing.T) {
	fs := OSFileSystem{}
	dir := t.TempDir()
	path := filepath.Join(dir, "crlf.txt")

	if err := os.WriteFile(path, []byte("line1\r\nline2\r\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	content, err := fs.Read(path)
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}
	if strings.Contains(content, "\r\n") {
		t.Fatal("Read() didn't normalize CRLF to LF")
	}
}

func TestComputeTagBoundaries(t *testing.T) {
	tags := make(map[string]bool)
	for _, s := range []string{"", "\n", "\x00", strings.Repeat("x", 10000)} {
		tag := ComputeTag(s)
		if len(tag) != 4 {
			t.Fatalf("ComputeTag(%q) returned %q (len=%d)", s, tag, len(tag))
		}
		tags[tag] = true
	}
}

// --- Similarity / Fuzzy Matching ---

func TestLevenshteinDistance(t *testing.T) {
	tests := []struct {
		a, b     string
		expected int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"abc", "ab", 1},   // insertion
		{"abc", "abcd", 1}, // deletion
		{"abc", "xyz", 3},
		{"hello", "hallo", 1},
		{"你好", "你好", 0},
		{"你好", "你坏", 1},
	}

	for _, tt := range tests {
		got := levenshteinDistance(tt.a, tt.b)
		if got != tt.expected {
			t.Errorf("levenshteinDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.expected)
		}
		// Symmetry check
		gotRev := levenshteinDistance(tt.b, tt.a)
		if gotRev != tt.expected {
			t.Errorf("levenshteinDistance(%q, %q) = %d (reversed), want %d", tt.b, tt.a, gotRev, tt.expected)
		}
	}
}

func TestLevenshteinSimilarity(t *testing.T) {
	tests := []struct {
		a, b   string
		wantGE float64 // similarity should be >= this
	}{
		{"", "", 1.0},
		{"hello", "", 0.0},
		{"hello", "hello", 1.0},
		{"hello", "hallo", 0.8}, // 1/5 = 0.8
		{"abc", "abc ", 0.75},   // 1/4 = 0.75, trailing space
		{"def", "def", 1.0},
		{"你好世界", "你好世界", 1.0},
		{"你好世界", "你好世", 0.75}, // 1/4 = 0.75
	}

	for _, tt := range tests {
		got := levenshteinSimilarity(tt.a, tt.b)
		if got != tt.wantGE {
			t.Errorf("levenshteinSimilarity(%q, %q) = %.4f, want %.4f", tt.a, tt.b, got, tt.wantGE)
		}
	}
}

func TestFuzzyLineMatch(t *testing.T) {
	tests := []struct {
		actual    string
		expected  string
		threshold float64
		match     bool
	}{
		// Exact match always passes
		{"hello world", "hello world", 1.0, true},
		// Trailing whitespace is normalized
		{"hello world  ", "hello world", 1.0, true},
		{"hello world", "hello world  ", 1.0, true},
		// Minor difference within threshold
		{"hello world", "hello word", 0.9, true},   // 1/11 ≈ 0.909 > 0.9
		{"hello world", "hello word", 0.95, false}, // 1/11 ≈ 0.909 < 0.95
		// Completely different
		{"abc", "xyz", 0.5, false},
		// Empty strings
		{"", "", 1.0, true},
		{"", "a", 0.5, false},
		// Unicode
		{"你好世界", "你好世界", 1.0, true},
	}

	for _, tt := range tests {
		got := fuzzyLineMatch(tt.actual, tt.expected, tt.threshold)
		if got != tt.match {
			t.Errorf("fuzzyLineMatch(%q, %q, %.2f) = %v, want %v",
				tt.actual, tt.expected, tt.threshold, got, tt.match)
		}
	}
}

// --- Context Lines Parser ---

func TestParseWithContextLines(t *testing.T) {
	input := "¶file.go#a1f0\n1:package main\n2:\n3:import \"fmt\"\nreplace 3..3:\n+import \"os\"\n"
	sections, err := Parse(input, "/test")
	if err != nil {
		t.Fatalf("Parse() with context lines failed: %v", err)
	}
	if len(sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(sections))
	}

	sec := sections[0]
	if len(sec.ContextLines) != 3 {
		t.Fatalf("expected 3 context lines, got %d: %v", len(sec.ContextLines), sec.ContextLines)
	}
	if sec.ContextLines[1] != "package main" {
		t.Fatalf("context line 1 = %q, want %q", sec.ContextLines[1], "package main")
	}
	if sec.ContextLines[2] != "" {
		t.Fatalf("context line 2 = %q, want empty string", sec.ContextLines[2])
	}
	if sec.ContextLines[3] != "import \"fmt\"" {
		t.Fatalf("context line 3 = %q, want %q", sec.ContextLines[3], "import \"fmt\"")
	}

	if len(sec.Operations) != 1 {
		t.Fatalf("expected 1 operation, got %d", len(sec.Operations))
	}
	if sec.Operations[0].Type != OpReplace {
		t.Fatalf("expected replace operation, got %d", sec.Operations[0].Type)
	}
}

func TestParseContextLinesOnlyNoOps(t *testing.T) {
	// Context lines without operations should still fail with ErrSectionNoOps
	input := "¶file.go#a1f0\n1:package main\n2:\n"
	_, err := Parse(input, "/test")
	if err == nil {
		t.Fatal("expected error for context lines without operations")
	}
}

func TestParseContextLinesBetweenOps(t *testing.T) {
	// Context lines between operations should NOT be extracted (only before first op)
	input := "¶file.go#a1f0\nreplace 1..1:\n+new\n2:this should be skipped\nreplace 3..3:\n+other\n"
	sections, err := Parse(input, "/test")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if len(sections[0].ContextLines) != 0 {
		t.Fatalf("expected 0 context lines (context lines after ops), got %d", len(sections[0].ContextLines))
	}
	if len(sections[0].Operations) != 2 {
		t.Fatalf("expected 2 operations, got %d", len(sections[0].Operations))
	}
}

// --- Context Line Validation in Patcher ---

func TestPatcherContextLinesMatch(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcherWithThreshold(fs, snapshots, 0.9)

	path := "/test/file.go"
	fs.files[path] = "line1\nline2\nline3\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path: path,
		Tag:  tag,
		ContextLines: map[int]string{
			1: "line1",
			2: "line2",
		},
		Operations: []Operation{
			{Type: OpReplace, Start: 2, End: 2, Body: []string{"modified"}},
		},
	}

	_, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() with matching context lines failed: %v", err)
	}
}

func TestPatcherContextLinesMismatch(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcherWithThreshold(fs, snapshots, 0.95)

	path := "/test/file.go"
	fs.files[path] = "line1\nline2\nline3\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path: path,
		Tag:  tag,
		ContextLines: map[int]string{
			2: "WRONG CONTENT",
		},
		Operations: []Operation{
			{Type: OpReplace, Start: 2, End: 2, Body: []string{"modified"}},
		},
	}

	_, err := patcher.Prepare(section)
	if err == nil {
		t.Fatal("expected error for context line mismatch")
	}
}

func TestPatcherContextLinesOutOfRange(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcherWithThreshold(fs, snapshots, 0.9)

	path := "/test/file.go"
	fs.files[path] = "line1\nline2\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path: path,
		Tag:  tag,
		ContextLines: map[int]string{
			10: "beyond file end",
		},
		Operations: []Operation{
			{Type: OpReplace, Start: 1, End: 1, Body: []string{"modified"}},
		},
	}

	_, err := patcher.Prepare(section)
	if err == nil {
		t.Fatal("expected error for context line out of range")
	}
}

func TestPatcherContextLinesFuzzyTolerance(t *testing.T) {
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	// Use exact-match mode (1.0)
	patcher := NewPatcherWithThreshold(fs, snapshots, 1.0)

	path := "/test/file.go"
	fs.files[path] = "func main() {\n\tfmt.Println(\"hello\")\n}\n"
	tag := snapshots.Record(path, fs.files[path])

	// Slight difference: "hello" vs "helloo" — exact match (1.0) should reject
	section := Section{
		Path: path,
		Tag:  tag,
		ContextLines: map[int]string{
			2: "\tfmt.Println(\"helloo\")",
		},
		Operations: []Operation{
			{Type: OpReplace, Start: 1, End: 1, Body: []string{"func main() {"}},
		},
	}

	_, err := patcher.Prepare(section)
	if err == nil {
		t.Fatal("expected error for context line mismatch with exact threshold")
	}

	// Now use lenient threshold — should pass
	patcher2 := NewPatcherWithThreshold(fs, snapshots, 0.8)
	section2 := Section{
		Path: path,
		Tag:  tag,
		ContextLines: map[int]string{
			2: "\tfmt.Println(\"helloo\")",
		},
		Operations: []Operation{
			{Type: OpReplace, Start: 1, End: 1, Body: []string{"func main() {"}},
		},
	}

	_, err = patcher2.Prepare(section2)
	if err != nil {
		t.Fatalf("Prepare() with lenient threshold should pass, got: %v", err)
	}
}

func TestPatcherContextLinesEmptyMap(t *testing.T) {
	// Empty context lines map should not affect normal operation
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcher(fs, snapshots)

	path := "/test/file.go"
	fs.files[path] = "a\nb\nc\n"
	tag := snapshots.Record(path, fs.files[path])

	section := Section{
		Path:         path,
		Tag:          tag,
		ContextLines: map[int]string{},
		Operations: []Operation{
			{Type: OpReplace, Start: 2, End: 2, Body: []string{"x"}},
		},
	}

	_, err := patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() with empty context lines should work: %v", err)
	}

	// Nil context lines should also work
	section.ContextLines = nil
	_, err = patcher.Prepare(section)
	if err != nil {
		t.Fatalf("Prepare() with nil context lines should work: %v", err)
	}
}

func TestPatcherApplyWithContextLines(t *testing.T) {
	// End-to-end: parse input with context lines + apply
	fs := newFakeFS()
	snapshots := NewSnapshotStore()
	patcher := NewPatcherWithThreshold(fs, snapshots, 0.9)

	path := "/test/file.go"
	fs.files[path] = "line1\nline2\nline3\n"
	tag := snapshots.Record(path, fs.files[path])

	// Include context lines before the operation
	input := "¶file.go#" + tag + "\n1:line1\n2:line2\nreplace 2..2:\n+modified\n"
	results, err := patcher.Apply(input, "/test")
	if err != nil {
		t.Fatalf("Apply() with matching context lines failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	saved, _ := fs.Read(path)
	if !strings.Contains(saved, "modified") {
		t.Fatalf("content was not updated:\n%s", saved)
	}
}
