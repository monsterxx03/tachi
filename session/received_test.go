package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file that arrived from another machine lands in the session's own directory, under received/ —
// the same place for both directions the node crosses (a peer's attachment, and a file this
// machine's delegation read back), because both are bytes from outside that belong to this
// conversation.
func TestSaveReceivedFileLandsInTheSessionDirectory(t *testing.T) {
	root := t.TempDir()
	const id = "2026-09-13-103926-92db8c33"

	path, err := SaveReceivedFile(root, id, "report.pdf", []byte("hello from the far side"))
	if err != nil {
		t.Fatalf("SaveReceivedFile: %v", err)
	}
	if want := filepath.Join(root, id, "received") + string(filepath.Separator); !strings.HasPrefix(path, want) {
		t.Errorf("received file %q is not under %q", path, want)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "hello from the far side" {
		t.Errorf("read back %q, %v", got, err)
	}
}

// The name is the OTHER machine's word for it, so it is input, not a path. A separator (or a name
// that resolves upwards) must be refused rather than sanitised into something that writes somewhere
// else: this is the difference between a name inside the session and a path on this machine.
func TestSaveReceivedFileRefusesANameThatIsNotASingleElement(t *testing.T) {
	root := t.TempDir()
	const id = "2026-09-13-103926-92db8c33"

	for _, bad := range []string{"", ".", "..", "../evil.txt", "a/b.txt", `/etc/passwd`, `a\b.txt`} {
		path, err := SaveReceivedFile(root, id, bad, []byte("x"))
		if err == nil {
			t.Errorf("SaveReceivedFile(%q) = %q, want an error", bad, path)
		}
	}
	if _, err := SaveReceivedFile(root, id, "fine.txt", nil); err == nil {
		t.Error("a zero-byte payload is a broken transfer, not a file")
	}
}

// Two peers sending the same name are two files. Overwriting would be silent data loss — the reader
// gets a valid path either way, so nobody would ever see it.
func TestSaveReceivedFileDoesNotOverwriteAnExistingName(t *testing.T) {
	root := t.TempDir()
	const id = "2026-09-13-103926-92db8c33"

	first, err := SaveReceivedFile(root, id, "report.pdf", []byte("first"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := SaveReceivedFile(root, id, "report.pdf", []byte("second"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first == second {
		t.Fatal("the second file replaced the first")
	}
	if got, err := os.ReadFile(first); err != nil || string(got) != "first" {
		t.Errorf("the first file is gone or changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(second); err != nil || string(got) != "second" {
		t.Errorf("the second file did not land: %q, %v", got, err)
	}
	// The reader still recognises what the other machine called it, extension last.
	if !strings.HasSuffix(second, ".pdf") || !strings.Contains(filepath.Base(second), "report") {
		t.Errorf("the suffix lost the name or the extension: %s", second)
	}
}
