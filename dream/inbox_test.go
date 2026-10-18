package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A write arriving while a dream runs must survive the dream's inbox cleanup.
// The old code truncated whatever was left in inbox.md after the agent ran, so
// anything recorded during those minutes was deleted with no trace.
func TestInboxHandoffKeepsWritesThatArriveDuringTheRun(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, inboxName)
	proc := filepath.Join(root, inboxProcessing)

	if err := os.WriteFile(inbox, []byte("old fact\n"), 0644); err != nil {
		t.Fatal(err)
	}

	taken, err := TakeInbox(root, nil)
	if err != nil || !taken {
		t.Fatalf("TakeInbox: taken=%v err=%v", taken, err)
	}
	if _, err := os.Stat(inbox); !os.IsNotExist(err) {
		t.Fatal("inbox should have been moved aside")
	}
	if _, err := os.Stat(proc); err != nil {
		t.Fatal("processing snapshot missing")
	}

	// A live conversation records something while the dream runs.
	if err := os.WriteFile(inbox, []byte("new fact\n"), 0644); err != nil {
		t.Fatal(err)
	}

	FinishInbox(root, true, nil)

	if _, err := os.Stat(proc); !os.IsNotExist(err) {
		t.Fatal("processing snapshot should be gone after a successful run")
	}
	body, _ := os.ReadFile(inbox)
	if string(body) != "new fact\n" {
		t.Fatalf("the write that arrived during the run was lost: %q", body)
	}
}

// A failed run hands the raw material back to the next one instead of throwing
// it away.
func TestInboxHandoffRestoresOnFailure(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, inboxName)
	proc := filepath.Join(root, inboxProcessing)

	os.WriteFile(inbox, []byte("unprocessed\n"), 0644)
	if _, err := TakeInbox(root, nil); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(inbox, []byte("arrived later\n"), 0644)

	FinishInbox(root, false, nil)

	if _, err := os.Stat(proc); !os.IsNotExist(err) {
		t.Fatal("snapshot should have been folded back, not left behind")
	}
	body, _ := os.ReadFile(inbox)
	text := string(body)
	if !strings.Contains(text, "unprocessed") || !strings.Contains(text, "arrived later") {
		t.Fatalf("both halves must survive a failed run, got %q", text)
	}
}

// A snapshot left by a crashed run was never integrated: the next run puts it
// back before taking its own, so it cannot be skipped forever.
func TestTakeInboxRecoversCrashedSnapshot(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, inboxProcessing)

	os.WriteFile(proc, []byte("from a crashed run\n"), 0644)

	taken, err := TakeInbox(root, nil)
	if err != nil {
		t.Fatalf("TakeInbox: %v", err)
	}
	if !taken {
		t.Fatal("recovered content is new work and must be consumed")
	}
	body, _ := os.ReadFile(proc)
	if !strings.Contains(string(body), "from a crashed run") {
		t.Fatalf("recovered content is not in the new snapshot: %q", body)
	}
}

// An empty (or missing) inbox is not work.
func TestTakeInboxEmpty(t *testing.T) {
	root := t.TempDir()
	if taken, err := TakeInbox(root, nil); err != nil || taken {
		t.Fatalf("missing inbox: taken=%v err=%v", taken, err)
	}
	os.WriteFile(filepath.Join(root, inboxName), nil, 0644)
	if taken, err := TakeInbox(root, nil); err != nil || taken {
		t.Fatalf("empty inbox: taken=%v err=%v", taken, err)
	}
}
