package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// pngBytes is a small valid PNG header — the storage layer never inspects the bytes, it only has to
// keep them, so the exact content does not matter here.
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x01, 0x02, 0x03}

// TestDirFor pins the layout AND the untrusted-input rule together: the id can arrive from a
// webview, so anything that is not a single directory name must be refused before it is joined onto
// the store path.
func TestDirFor(t *testing.T) {
	root := t.TempDir()

	const id = "2026-09-13-103926-92db8c33"
	if got, err := DirFor(root, id); err != nil || got != filepath.Join(root, id) {
		t.Errorf("DirFor(%q) = %q, %v; want %q", id, got, err, filepath.Join(root, id))
	}

	for _, bad := range []string{"", ".", "..", "../other-session", "a/b", `/abs`, `a\b`} {
		if got, err := DirFor(root, bad); err == nil {
			t.Errorf("DirFor(%q) = %q, want an error", bad, got)
		}
	}
	if _, err := DirFor(root, ""); err != ErrNoSessionID {
		t.Errorf("DirFor(\"\") err = %v, want ErrNoSessionID (a frontend says this in its own words)", err)
	}
}

// TestSavePastedImageLandsInTheSessionDirectory pins where a paste goes — the same place for both
// frontends, and the reason this is in the store rather than in either one of them.
func TestSavePastedImageLandsInTheSessionDirectory(t *testing.T) {
	root := t.TempDir()
	const id = "2026-09-13-103926-92db8c33"

	path, err := SavePastedImage(root, id, ".png", pngBytes)
	if err != nil {
		t.Fatalf("SavePastedImage: %v", err)
	}
	if want := filepath.Join(root, id, "pasted") + string(filepath.Separator); !strings.HasPrefix(path, want) {
		t.Errorf("pasted file %q is not under %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, pngBytes) {
		t.Errorf("stored file = %q, %v; want the pasted bytes", got, err)
	}

	// The bytes are whatever was on the reader's screen, so they are not world-readable — and the
	// directory holding them is not either.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("pasted file mode = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("pasted dir mode = %o, want 700", perm)
	}
}

// TestSavePastedImageNamesAreUnique: two pastes in the same second must not land in one file. The
// timestamp alone collides (it did — the desktop's own test caught a lost screenshot that way), so
// the name carries a uuid too.
func TestSavePastedImageNamesAreUnique(t *testing.T) {
	root := t.TempDir()

	first, err := SavePastedImage(root, "id", ".png", pngBytes)
	if err != nil {
		t.Fatalf("first paste: %v", err)
	}
	second, err := SavePastedImage(root, "id", ".png", pngBytes)
	if err != nil {
		t.Fatalf("second paste: %v", err)
	}
	if first == second {
		t.Fatalf("both pastes went to %q, want a name of their own each", first)
	}
	for _, p := range []string{first, second} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("paste %q is missing: %v", p, err)
		}
	}
}

// TestSavePastedImageWithoutASessionUsesTemp: a frontend can hold a paste before any conversation
// exists (a fresh TUI opens one with its first turn). The file is only the carrier for the
// @-reference of that turn — the bytes themselves are recorded with the message — so it goes to the
// temp directory rather than into the store, where no session can own it.
func TestSavePastedImageWithoutASessionUsesTemp(t *testing.T) {
	root := t.TempDir()

	path, err := SavePastedImage(root, "", ".png", pngBytes)
	if err != nil {
		t.Fatalf("SavePastedImage: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })
	if strings.HasPrefix(path, root) {
		t.Errorf("pasted file %q landed in the session store, want the temp directory", path)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, pngBytes) {
		t.Errorf("stored file = %q, %v; want the pasted bytes", got, err)
	}
}

// TestSavePastedImageRefusesTraversal: the same rule as DirFor, at the write itself — a name like
// "../../etc" must not put a file outside the store.
func TestSavePastedImageRefusesTraversal(t *testing.T) {
	base := t.TempDir()
	// The global base dir is moved onto `base` as well, so the assertion below catches a refusal
	// that writes under EITHER root: the one it was given, or the process-global one it must not
	// consult.
	config.SetBaseDir(base)

	if _, err := SavePastedImage(base, "../../etc", ".png", pngBytes); err == nil {
		t.Fatal("SavePastedImage accepted a traversal id")
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("something was written under the base dir: %v", entries)
	}
}

// TestSavePastedImageRefusesAnOversizedImage mirrors the check the callers do first (so they can say
// so while the screenshot is still on the clipboard): a file the model will never be shown would sit
// in the session forever.
func TestSavePastedImageRefusesAnOversizedImage(t *testing.T) {
	root := t.TempDir()
	const id = "2026-09-13-103926-92db8c33"

	_, err := SavePastedImage(root, id, ".png", make([]byte, llm.MaxImageSize+1))
	if err == nil {
		t.Fatal("SavePastedImage accepted an image over the model's limit")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("err = %v, want it to name the limit", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, id)); !os.IsNotExist(statErr) {
		t.Errorf("a refused paste still created the session's directory")
	}
}
