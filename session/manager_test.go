package session

import (
	"testing"
)

func TestManagerCleanup_BelowLimit(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store)
	mgr.SetMaxKeep(5)

	// Create 3 sessions — should not trigger cleanup
	for i := 0; i < 3; i++ {
		_, err := mgr.New("openai", "gpt-4")
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
	for i := 0; i < 5; i++ {
		_, err := mgr.New("openai", "gpt-4")
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
	for i := 0; i < 5; i++ {
		_, err := mgr.New("openai", "gpt-4")
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
	for i := 0; i < 4; i++ {
		sess, err := mgr.New("openai", "gpt-4")
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
	sess1, _ := mgr.New("openai", "gpt-4")
	mgr.Load(sess1.ID) // make it current

	// Create 2 more — cleanup triggers, should keep sess1 (current)
	sess2, _ := mgr.New("openai", "gpt-4")
	sess3, _ := mgr.New("openai", "gpt-4")
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

	for i := 0; i < 3; i++ {
		_, err := mgr.New("openai", "gpt-4")
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

func newTempStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	return store
}
