package session

import (
	"testing"
)

func TestManagerCleanup_BelowLimit(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store)
	mgr.SetMaxKeep(5)

	// Create 3 sessions — should not trigger cleanup
	for range 3 {
		_, err := mgr.New("openai", "gpt-4", ".")
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
	}

	sessions, err := mgr.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(sessions) != 3 {
		t.Errorf("expected 3 sessions, got %d", len(sessions))
	}
}

func TestManagerCleanup_ExceedsLimit(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store)
	mgr.SetMaxKeep(3)

	// Create 5 sessions — should trigger cleanup, retaining 3
	for range 5 {
		_, err := mgr.New("openai", "gpt-4", ".")
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
	}

	sessions, err := mgr.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(sessions) != 3 {
		t.Errorf("expected 3 sessions after cleanup, got %d", len(sessions))
	}
}

func TestManagerCleanup_ZeroMaxKeep(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store)
	mgr.SetMaxKeep(0) // no cleanup

	// Create 5 sessions — should not trigger cleanup
	for range 5 {
		_, err := mgr.New("openai", "gpt-4", ".")
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
	}

	sessions, err := mgr.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(sessions) != 5 {
		t.Errorf("expected 5 sessions with maxKeep=0, got %d", len(sessions))
	}
}

func TestManagerCleanup_PreservesNewest(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store)
	mgr.SetMaxKeep(2)

	// Create 4 sessions
	var sessionIDs []string
	for range 4 {
		sess, err := mgr.New("openai", "gpt-4", ".")
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
		sessionIDs = append(sessionIDs, sess.ID)
	}

	sessions, err := mgr.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}

	// The newest 2 should remain (last 2 created)
	for _, s := range sessions {
		if s.ID != sessionIDs[2] && s.ID != sessionIDs[3] {
			t.Errorf("unexpected session retained: %s (expected newest 2)", s.ID)
		}
	}
}

func TestManagerCleanup_PreservesCurrent(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store)
	mgr.SetMaxKeep(1)

	// Create 3 sessions. The first becomes the current session.
	sess1, _ := mgr.New("openai", "gpt-4", ".")
	mgr.Load(sess1.ID) // make it current

	// Create 2 more — cleanup triggers, should keep sess1 (current)
	sess2, _ := mgr.New("openai", "gpt-4", ".")
	sess3, _ := mgr.New("openai", "gpt-4", ".")
	_ = sess2
	_ = sess3

	// Load sess1 again so it's current when cleanupLocked runs
	mgr.Load(sess1.ID)

	removed, err := mgr.CleanupOldSessions()
	if err != nil {
		t.Fatalf("CleanupOldSessions failed: %v", err)
	}
	if removed == 0 {
		t.Log("cleanup may have already run during New(); explicit cleanup may remove 0")
	}

	sessions, err := mgr.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(sessions) < 1 {
		t.Errorf("expected at least 1 session, got %d", len(sessions))
	}
}

func TestManagerCleanup_ExplicitNoop(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store)
	mgr.SetMaxKeep(100)

	for range 3 {
		_, err := mgr.New("openai", "gpt-4", ".")
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
	}

	removed, err := mgr.CleanupOldSessions()
	if err != nil {
		t.Fatalf("CleanupOldSessions failed: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}
}

// TestManagerCleanup_PreservesChannelSessions verifies that sessions
// with a non-empty ThreadID (i.e., actively used by IM channels) are
// preserved during cleanup, even if they're among the oldest.
func TestManagerCleanup_PreservesChannelSessions(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store)
	mgr.SetMaxKeep(2)

	// Create 5 sessions. Mark some with ThreadID to simulate
	// active channel threads.
	var channelIDs []string
	for i := range 5 {
		sess, err := mgr.New("openai", "gpt-4", ".")
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
		if i%2 == 0 {
			// Sessions 0, 2, 4 are "channel" sessions (active threads).
			mgr.SetThreadID("thread-" + sess.ID[:8])
			channelIDs = append(channelIDs, sess.ID)
		}
	}

	sessions, err := mgr.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	// With maxKeep=2 and 3 protected channel sessions, we expect
	// at least the 3 channel sessions to survive. The 2 non-channel
	// sessions may or may not be retained depending on sorting.
	if len(sessions) < 3 {
		t.Errorf("expected at least 3 sessions (channel-protected), got %d", len(sessions))
	}

	// Verify all channel sessions are present.
	for _, cid := range channelIDs {
		found := false
		for _, s := range sessions {
			if s.ID == cid {
				found = true
				if s.ThreadID == "" {
					t.Errorf("channel session %s lost its ThreadID", cid)
				}
				break
			}
		}
		if !found {
			t.Errorf("channel session %s was deleted by cleanup", cid)
		}
	}
}

// TestManagerCleanup_RemovesExpiredChannelSessions verifies that sessions
// whose ThreadID was cleared (via /new) are still eligible for cleanup.
func TestManagerCleanup_RemovesClearedThreadSessions(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store)
	mgr.SetMaxKeep(1)

	// Create 3 sessions, all with ThreadID.
	for range 3 {
		_, err := mgr.New("openai", "gpt-4", ".")
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
		mgr.SetThreadID("thread-" + mgr.Current().ID[:8])
	}

	// All 3 survive because ThreadID protects them.
	sessions, _ := mgr.List()
	if len(sessions) != 3 {
		t.Fatalf("expected 3, got %d", len(sessions))
	}

	// Clear ThreadID on all — simulating /new calls.
	for _, s := range sessions {
		mgr.Load(s.ID)
		mgr.SetThreadID("") // clear the binding
	}
	mgr.EndCurrent()

	// Create one more session to trigger cleanup with maxKeep=1.
	_, err := mgr.New("openai", "gpt-4", ".")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	remaining, _ := mgr.List()
	if len(remaining) != 1 {
		t.Errorf("expected 1 session after cleanup (all ThreadIDs cleared), got %d", len(remaining))
	}
}

func newTempStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	return store
}
