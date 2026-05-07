package debuglog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriter(t *testing.T) {
	dir := t.TempDir()

	const chunkSize int64 = 128
	const keep = 3

	rw, err := newRotatingWriter(dir, "debug.log", chunkSize, keep)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer rw.Close()

	// Write enough to trigger rotation — chunkSize is 128, write 40 bytes each.
	// Each chunk can hold floor(128/40) = 3 writes.
	// To fill chunk 0-2 and have a bit of chunk 3, write N times.
	for i := range 20 {
		msg := strings.Repeat("a", 39) + "\n" // 40 bytes
		_, err := rw.Write([]byte(msg))
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Check that old chunks exist.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}

	t.Logf("files: %v", names)

	// We should have: debug.log, debug.log.1, debug.log.2
	// debug.log.3 should have been dropped (keep=3, oldest dropped)
	var gotLog, got1, got2, got3 bool
	for _, n := range names {
		switch n {
		case "debug.log":
			gotLog = true
		case "debug.log.1":
			got1 = true
		case "debug.log.2":
			got2 = true
		case "debug.log.3":
			got3 = true
		}
	}

	if !gotLog || !got1 || !got2 {
		t.Errorf("expected debug.log, .1, .2 to exist; got files: %v", names)
	}
	if got3 {
		t.Errorf("debug.log.3 should have been dropped (keep=%d)", keep)
	}

	// Quick sanity: the current debug.log should be non-empty.
	fi, err := os.Stat(filepath.Join(dir, "debug.log"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 {
		t.Error("debug.log should not be empty")
	}
}

func TestRotatingWriterNoRotate(t *testing.T) {
	dir := t.TempDir()

	rw, err := newRotatingWriter(dir, "debug.log", 1024*1024, 3)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer rw.Close()

	// Write a small amount, should not trigger rotation.
	_, err = rw.Write([]byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected 1 file, got %d", len(entries))
	}
	if entries[0].Name() != "debug.log" {
		t.Errorf("expected debug.log, got %s", entries[0].Name())
	}
}
