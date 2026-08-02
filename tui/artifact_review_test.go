package tui

import (
	"os"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/llm"
)

// TestSpliceReviewReminderIntoHistory guards the J1/#1 ordering fix: the
// review artifact reminder is stashed by appendReviewArtifact and spliced
// into m.history only AFTER the one-off savedHistory restore (which replaces
// m.history wholesale). Splicing earlier would be wiped by the restore.
func TestSpliceReviewReminderIntoHistory(t *testing.T) {
	m := testModel()
	m.history = []llm.Message{
		{Role: "user", Content: "帮我 review"},
		{Role: "assistant", Content: "好"},
	}

	// Simulate a completed review: appendReviewArtifact stashes the
	// reminder (disk write is skipped — no real agent), then the one-off
	// restore wipes m.history back to the saved snapshot.
	reportPath := writeTempReport(t)
	m.appendReviewArtifact(3, reportPath)
	m.history = m.savedHistory // savedHistory is nil in testModel → history becomes nil

	// The stash must survive the restore and be spliced afterwards.
	m.spliceReviewReminderIntoHistory()
	if len(m.history) != 1 {
		t.Fatalf("expected 1 spliced message after restore, got %d: %+v", len(m.history), m.history)
	}
	last := m.history[0]
	if last.Role != "user" || !strings.Contains(last.Content, "round-3-judge-x.md") {
		t.Errorf("spliced reminder mismatch: %+v", last)
	}
	// The stash must be consumed.
	if m.reviewReminder != "" {
		t.Errorf("reviewReminder not consumed after splice")
	}
}

// TestSpliceReviewReminderIntoHistory_NoopWhenEmpty ensures an empty stash
// is a no-op (no phantom user message).
func TestSpliceReviewReminderIntoHistory_NoopWhenEmpty(t *testing.T) {
	m := testModel()
	m.spliceReviewReminderIntoHistory()
	if len(m.history) != 0 {
		t.Errorf("expected no-op with empty stash, got %d messages", len(m.history))
	}
}

// TestAppendReviewArtifact_StashesReminderWithoutAgent verifies the stash
// happens even when there is no session manager (fresh window) — the current
// window must still be able to follow up (J6).
func TestAppendReviewArtifact_StashesReminderWithoutAgent(t *testing.T) {
	m := testModel() // no agent → SessionManager() == nil
	reportPath := writeTempReport(t)
	m.appendReviewArtifact(2, reportPath)
	if m.reviewReminder == "" {
		t.Fatal("expected reminder stashed even without a session manager")
	}
	if !strings.Contains(m.reviewReminder, reportPath) {
		t.Errorf("stashed reminder missing path: %q", m.reviewReminder)
	}
}

// writeTempReport creates a temp file to satisfy the os.Stat check in
// appendReviewArtifact (J7) and returns its path.
func writeTempReport(t *testing.T) string {
	t.Helper()
	p := t.TempDir() + "/round-3-judge-x.md"
	if err := os.WriteFile(p, []byte("report"), 0o600); err != nil {
		t.Fatalf("write temp report: %v", err)
	}
	return p
}
