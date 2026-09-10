package atfile

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireRipgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not installed")
	}
}

// write creates a file under dir and returns its slash-separated relative path.
func write(t *testing.T, dir, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, data, 0o644))
	return rel
}

func TestExpandInlinesTextFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "foo.go", []byte("package main\n"))

	res := Expand(dir, "看一下 @foo.go 谢谢")

	assert.Equal(t, "看一下 @foo.go\n\n--- BEGIN UNTRUSTED FILE CONTENT: foo.go ---\npackage main\n\n--- END UNTRUSTED FILE CONTENT: foo.go --- 谢谢", res.Text)
	assert.Equal(t, 1, res.Refs)
	assert.Empty(t, res.Images)
}

// TestExpandRefBoundaries pins the reference trigger rule: a reference starts at
// the beginning of the message or after any whitespace byte (not only after a
// plain space, which used to leave "@foo" after a newline unexpanded).
func TestExpandRefBoundaries(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "foo.go", []byte("x"))

	cases := []struct {
		name     string
		message  string
		wantRefs int
	}{
		{"message start", "@foo.go", 1},
		{"after space", "see @foo.go", 1},
		{"after newline", "see\n@foo.go", 1},
		{"after tab", "see\t@foo.go", 1},
		{"after carriage return", "see\r@foo.go", 1},
		{"after full-width space", "see\u3000@foo.go", 0},
		{"attached to a word", "see@foo.go", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Expand(dir, tc.message)
			assert.Equal(t, tc.wantRefs, res.Refs)
			if tc.wantRefs == 0 {
				assert.Equal(t, tc.message, res.Text, "unexpanded message must stay verbatim")
			} else {
				assert.Contains(t, res.Text, "BEGIN UNTRUSTED FILE CONTENT: foo.go")
			}
		})
	}
}

func TestExpandLeavesMissingRefVerbatim(t *testing.T) {
	dir := t.TempDir()

	res := Expand(dir, "看下 @nope.go")

	assert.Equal(t, "看下 @nope.go", res.Text)
	assert.Equal(t, 0, res.Refs)
}

func TestExpandBareAtStaysVerbatim(t *testing.T) {
	res := Expand(t.TempDir(), "what about @ alone?")
	assert.Equal(t, "what about @ alone?", res.Text)
	assert.Equal(t, 0, res.Refs)
}

func TestExpandDirectoryListing(t *testing.T) {
	requireRipgrep(t)
	dir := t.TempDir()
	write(t, dir, "pkg/a.go", []byte("a"))
	write(t, dir, "pkg/b.go", []byte("b"))

	res := Expand(dir, "@pkg")

	assert.Equal(t, 1, res.Refs)
	assert.Contains(t, res.Text, "--- BEGIN UNTRUSTED FILE CONTENT: pkg ---")
	assert.Contains(t, res.Text, "Directory contains 2 files:")
	// Entries are relative to the referenced directory (rg runs with it as cwd).
	assert.Contains(t, res.Text, "  a.go")
	assert.Contains(t, res.Text, "  b.go")
}

func TestExpandImageAsContentPart(t *testing.T) {
	dir := t.TempDir()
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x01}
	write(t, dir, "pic.png", png)

	res := Expand(dir, "看这个 @pic.png")

	assert.Equal(t, "看这个 [图片: pic.png]", res.Text)
	assert.Equal(t, 1, res.Refs)
	require.Len(t, res.Images, 1)
	assert.Equal(t, llm.ContentPartImage, res.Images[0].Type)
	assert.Equal(t, "image/png", res.Images[0].MediaType)
	assert.NotEmpty(t, res.Images[0].Data)
}

func TestExpandBinaryAnnotated(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "bin.dat", []byte{0x00, 0x01, 0x02})

	res := Expand(dir, "@bin.dat")

	assert.Equal(t, "[@file: bin.dat]", res.Text)
	assert.Equal(t, 1, res.Refs)
}

func TestExpandOversizedTextAnnotated(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "big.txt", []byte(strings.Repeat("a", MaxInlineSize+1)))

	res := Expand(dir, "@big.txt")

	assert.Equal(t, "[@file: big.txt]", res.Text)
	assert.Equal(t, 0, len(res.Images))
}

func TestExpandMultipleRefsKeepOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.txt", []byte("A"))
	write(t, dir, "pic.png", []byte{0x89, 0x50, 0x4E, 0x47})

	res := Expand(dir, "@a.txt @pic.png @missing.txt")

	assert.Equal(t, 2, res.Refs)
	assert.Contains(t, res.Text, "BEGIN UNTRUSTED FILE CONTENT: a.txt ---\nA")
	assert.Contains(t, res.Text, "[图片: pic.png]")
	assert.Contains(t, res.Text, "@missing.txt", "unresolvable ref stays verbatim")
	assert.True(t, strings.Index(res.Text, "a.txt ---\nA") < strings.Index(res.Text, "[图片: pic.png]"),
		"expansions keep their original order")
	require.Len(t, res.Images, 1)
}

// TestExpandAbsoluteRef covers drag-and-drop from outside the working
// directory: such references are absolute and must resolve as-is.
func TestExpandAbsoluteRef(t *testing.T) {
	outside := t.TempDir()
	abs := filepath.Join(outside, "note.md")
	require.NoError(t, os.WriteFile(abs, []byte("hi"), 0o644))

	res := Expand(t.TempDir(), "看 @"+abs)

	assert.Equal(t, 1, res.Refs)
	assert.Contains(t, res.Text, "BEGIN UNTRUSTED FILE CONTENT: "+filepath.ToSlash(abs))
	assert.Contains(t, res.Text, "hi")
}

func TestClassify(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dir/file.txt", []byte("x"))
	write(t, dir, "text.txt", []byte("hello"))
	write(t, dir, "empty.txt", []byte{})
	write(t, dir, "bin.dat", []byte{0x00, 0x01})
	write(t, dir, "pic.jpeg", []byte{0xFF, 0xD8, 0xFF})
	write(t, dir, "huge.png", append([]byte{0x89, 0x50, 0x4E, 0x47}, bytes.Repeat([]byte{0}, int(llm.MaxImageSize))...))

	cases := []struct {
		path string
		want Kind
	}{
		{filepath.Join(dir, "dir"), KindDir},
		{filepath.Join(dir, "text.txt"), KindText},
		{filepath.Join(dir, "empty.txt"), KindText},
		{filepath.Join(dir, "bin.dat"), KindBinary},
		{filepath.Join(dir, "pic.jpeg"), KindImage},
		{filepath.Join(dir, "huge.png"), KindImageTooLarge},
		{filepath.Join(dir, "nope.txt"), KindMissing},
	}
	for _, tc := range cases {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			kind, _ := Classify(tc.path)
			assert.Equal(t, tc.want, kind)
		})
	}
}

func TestRefForPath(t *testing.T) {
	root := t.TempDir()
	write(t, root, "sub/a.go", []byte("x"))

	ref, ok := RefForPath(root, filepath.Join(root, "sub", "a.go"))
	require.True(t, ok)
	assert.Equal(t, "@sub/a.go", ref)

	outside := filepath.Join(t.TempDir(), "b.go")
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o644))
	ref, ok = RefForPath(root, outside)
	require.True(t, ok)
	assert.Equal(t, "@"+filepath.ToSlash(outside), ref, "outside the root keeps the absolute path")

	_, ok = RefForPath(root, filepath.Join(root, "missing.go"))
	assert.False(t, ok)
}

func TestRefsForPaths(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", []byte("x"))
	write(t, root, "b.go", []byte("x"))

	refs, ok := RefsForPaths(root, []string{filepath.Join(root, "a.go"), filepath.Join(root, "b.go")})
	require.True(t, ok)
	assert.Equal(t, "@a.go @b.go", refs)

	refs, ok = RefsForPaths(root, []string{filepath.Join(root, "missing.go")})
	assert.False(t, ok)
	assert.Empty(t, refs)

	refs, ok = RefsForPaths(root, []string{filepath.Join(root, "missing.go"), filepath.Join(root, "a.go")})
	require.True(t, ok)
	assert.Equal(t, "@a.go", refs, "missing paths are skipped")
}

func TestLastRefStart(t *testing.T) {
	assert.Equal(t, -1, LastRefStart("no refs here"))
	assert.Equal(t, 0, LastRefStart("@a.go"))
	assert.Equal(t, 4, LastRefStart("see @a.go"))
	assert.Equal(t, 4, LastRefStart("see\n@a.go"))
	assert.Equal(t, 10, LastRefStart("see @a.go @b"), "the last reference wins")
	assert.Equal(t, -1, LastRefStart("mail@example.com"))
}

func TestResolvePath(t *testing.T) {
	assert.Equal(t, "/root/sub/a.go", ResolvePath("/root", "sub/a.go"))
	assert.Equal(t, "/abs/a.go", ResolvePath("/root", "/abs/a.go"))
	assert.Equal(t, "/root/a.go", ResolvePath("/root", "./a.go"))

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "notes/a.md"), ResolvePath("/root", "~/notes/a.md"))
}

// TestExpandOversizedImageAnnotated pins the image budget shared with the Read
// tool (llm.MaxImageSize): past it, the reference is annotated with the reason,
// so the model can tell "image I cannot see, and why" from "binary file".
func TestExpandOversizedImageAnnotated(t *testing.T) {
	dir := t.TempDir()
	// Valid PNG signature, padded past the shared image budget.
	big := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
		bytes.Repeat([]byte{0x00}, int(llm.MaxImageSize))...)
	write(t, dir, "big.png", big)

	res := Expand(dir, "@big.png")

	assert.Equal(t, "[图片过大未内联: big.png（超过 5MB 上限）]", res.Text)
	assert.Empty(t, res.Images, "an oversized image must not be attached")
	assert.Equal(t, 1, res.Refs)
}
