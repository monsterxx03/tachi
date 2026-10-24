package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveUploadedFileKeepsTheSendersName pins the whole point of a separate uploads/ directory:
// an upload arrives WITH a name, and that name is what the phone shows back and what the
// @-reference reads as. A generated name (paste-) would lose it.
func TestSaveUploadedFileKeepsTheSendersName(t *testing.T) {
	root := t.TempDir()
	const id = "2026-10-24-210908-6a039f44"

	got, err := SaveUploadedFile(root, id, "发票 10月.pdf", []byte("%PDF-1.4 fake"))
	if err != nil {
		t.Fatalf("SaveUploadedFile: %v", err)
	}
	want := filepath.Join(root, id, uploadsDirName, "发票 10月.pdf")
	if got != want {
		t.Errorf("stored %q, want %q", got, want)
	}
	if raw, err := os.ReadFile(got); err != nil || string(raw) != "%PDF-1.4 fake" {
		t.Errorf("stored bytes = %q, %v", raw, err)
	}
	if st, err := os.Stat(got); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, %v; want 600", st.Mode().Perm(), err)
	}
	if st, err := os.Stat(filepath.Dir(got)); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("uploads dir mode = %v, %v; want 700", st.Mode().Perm(), err)
	}
}

// TestSaveUploadedFileDoesNotOverwrite: two photos off a phone are routinely called IMG_0001.jpg,
// and the second one silently replacing the first is a failure nobody could notice.
func TestSaveUploadedFileDoesNotOverwrite(t *testing.T) {
	root := t.TempDir()
	const id = "s1"

	first, err := SaveUploadedFile(root, id, "IMG_0001.jpg", []byte("one"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := SaveUploadedFile(root, id, "IMG_0001.jpg", []byte("two"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first == second {
		t.Fatalf("both uploads landed in %q", first)
	}
	if filepath.Base(second) != "IMG_0001-1.jpg" {
		t.Errorf("second name = %q, want IMG_0001-1.jpg", filepath.Base(second))
	}
	if raw, _ := os.ReadFile(first); string(raw) != "one" {
		t.Errorf("first file now reads %q — it was overwritten", raw)
	}
}

// TestUploadFileNameDropsDirectories: a picker hands over a path, and the base name is what the
// sender means. Everything that would escape the uploads directory is refused outright, because
// sanitising ".." into something else would be a name nobody sent.
func TestUploadFileNameDropsDirectories(t *testing.T) {
	ok := map[string]string{
		"/sdcard/DCIM/Camera/IMG_0001.jpg": "IMG_0001.jpg",
		`C:\Users\me\photo.png`:            "photo.png",
		"  log.txt  ":                      "log.txt",
	}
	for in, want := range ok {
		got, err := UploadFileName(in)
		if err != nil || got != want {
			t.Errorf("UploadFileName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", ".", "..", "/", "a/b/.."} {
		if got, err := UploadFileName(bad); err == nil {
			t.Errorf("UploadFileName(%q) = %q, want an error", bad, got)
		}
	}
}

// TestUploadFileNameKeepsTheExtensionWhenShortening: the extension is what tells a later reader
// (and @-expansion) what kind of file this is, so a long name must lose stem, not suffix.
func TestUploadFileNameKeepsTheExtensionWhenShortening(t *testing.T) {
	long := strings.Repeat("截图", 60) + ".png"
	got, err := UploadFileName(long)
	if err != nil {
		t.Fatalf("UploadFileName: %v", err)
	}
	if !strings.HasSuffix(got, ".png") {
		t.Errorf("%q lost its extension", got)
	}
	if n := len([]rune(got)); n > maxUploadNameRunes {
		t.Errorf("name is %d runes, want <= %d", n, maxUploadNameRunes)
	}
}

// TestUploadDirRefusesTraversal is DirFor's rule, reached through the other door: a session id that
// is not one directory name must not become one when the uploads directory is created.
func TestUploadDirRefusesTraversal(t *testing.T) {
	if _, err := UploadDir(t.TempDir(), "../etc"); err == nil {
		t.Fatal("UploadDir accepted a traversal id")
	}
}
