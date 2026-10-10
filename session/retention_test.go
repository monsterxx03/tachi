package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The retention record: the reason a session must survive a cleanup, kept where every process
// sharing the data dir can see it rather than in one manager's memory.

// The helper-process pattern, the same one node/crossprocess_test.go uses: this test binary
// re-executes itself with an env var set, so "a hold written by another process still
// protects" is verified with a real second process rather than asserted about one.
const (
	holdHelperMode = "TACHI_SESSION_HOLD_HELPER"
	holdHelperDir  = "TACHI_SESSION_HOLD_DIR"
)

func TestMain(m *testing.M) {
	if sessionID := os.Getenv(holdHelperMode); sessionID != "" {
		helperWritesHold(sessionID, os.Getenv(holdHelperDir))
		return
	}
	os.Exit(m.Run())
}

// helperWritesHold is the second process: it takes one hold and exits, leaving nothing behind
// but the file. It deliberately does NOT clean up on the way out — the whole question is
// whether a hold outlives the thing that wrote it.
func helperWritesHold(sessionID, dir string) {
	retention, err := NewRetention(dir, nil)
	if err != nil {
		fmt.Printf("ERROR %v\n", err)
		os.Exit(3)
	}
	if err := retention.Acquire(Hold{
		SessionID: sessionID, Owner: "helper", Reason: ReasonTask, Ref: "task-from-another-process",
	}); err != nil {
		fmt.Printf("ERROR %v\n", err)
		os.Exit(3)
	}
	fmt.Println("HELD")
}

func newRetentionFixture(t *testing.T) (string, *FileStore, *Manager, *Retention) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	mgr := NewManagerWithStore(store, nil)
	retention, err := NewRetention(dir, nil)
	if err != nil {
		t.Fatalf("NewRetention: %v", err)
	}
	return dir, store, mgr, retention
}

// fillTo makes the manager hold n sessions, oldest first, and returns them in creation order.
func fillTo(t *testing.T, mgr *Manager, n int) []*Session {
	t.Helper()
	out := make([]*Session, 0, n)
	for i := range n {
		s, err := mgr.New("openai", ".")
		if err != nil {
			t.Fatalf("New #%d: %v", i, err)
		}
		out = append(out, s)
	}
	return out
}

func TestAHoldKeepsASessionFromThisManagersCleanup(t *testing.T) {
	_, store, mgr, retention := newRetentionFixture(t)

	// Created with no cap, so the sweep's candidates are the ones THIS test chooses: a manager
	// that cleans up while you are still making sessions would take the first one before the
	// hold below is even possible.
	created := fillTo(t, mgr, 4)
	mgr.SetMaxKeep(2)
	oldest, second := created[0], created[1]

	if err := retention.Acquire(Hold{
		SessionID: oldest.ID, Owner: "node", Reason: ReasonTask, Ref: "task-1",
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	removed := mgr.CleanupOldSessions()
	if removed != 1 {
		t.Fatalf("expected exactly the unheld candidate to go, removed=%d", removed)
	}
	if _, err := store.LoadMeta(oldest.ID); err != nil {
		t.Errorf("the held session was deleted by its own manager's cleanup: %v", err)
	}
	if _, err := store.LoadMeta(second.ID); err == nil {
		t.Error("the unheld sibling should have been swept")
	}
}

// The other half of the same promise, and the one a node actually depends on: the protection
// is not in the manager that took it. A cleanup run by ANY manager over this data dir — the
// desktop's, another TUI's, a manager built for this call — sees the same record.
func TestAHoldIsVisibleToEveryManagerOverTheSameDataDir(t *testing.T) {
	dir, _, node, retention := newRetentionFixture(t)
	node.SetMaxKeep(0) // the node's own manager never sweeps; the desktop's does

	held := fillTo(t, node, 1)[0]
	if err := retention.Acquire(Hold{
		SessionID: held.ID, Owner: "node", Reason: ReasonContext, Ref: "ctx-1",
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Somebody else's manager, capped at one session, with newer sessions of its own.
	other, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	desktop := NewManagerWithStore(other, nil)
	desktop.SetMaxKeep(1)
	fillTo(t, desktop, 3)

	if removed := desktop.CleanupOldSessions(); removed != 0 {
		t.Fatalf("a sweep that knows nothing about the node deleted %d session(s)", removed)
	}
	if _, err := other.LoadMeta(held.ID); err != nil {
		t.Errorf("the node's session was swept by another manager: %v", err)
	}
}

func TestDeletingAHeldSessionIsRefusedWithItsReason(t *testing.T) {
	_, store, mgr, retention := newRetentionFixture(t)
	sess := fillTo(t, mgr, 1)[0]

	for _, h := range []Hold{
		{SessionID: sess.ID, Owner: "node", Reason: ReasonTask, Ref: "task-42"},
		{SessionID: sess.ID, Owner: "node", Reason: ReasonContext, Ref: "ctx-7"},
	} {
		if err := retention.Acquire(h); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}

	err := mgr.Delete(sess.ID)
	var refusal *SessionHeldError
	if !errors.As(err, &refusal) {
		t.Fatalf("deleting a held session must be refused with the reason, got %v", err)
	}
	if len(refusal.Holds) != 2 {
		t.Errorf("the refusal must name every hold, got %v", refusal.Holds)
	}
	// The reader's next action is to end that task or close that conversation, so the refusal
	// has to say which ones.
	for _, want := range []string{"task-42", "ctx-7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %s", err.Error(), want)
		}
	}
	if _, err := store.LoadMeta(sess.ID); err != nil {
		t.Errorf("a refused delete must not touch the session: %v", err)
	}

	// Releasing ONE reason is not releasing the other: a task ending says nothing about a
	// conversation that can still be continued.
	if err := retention.Release(sess.ID, "node", "task-42"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := mgr.Delete(sess.ID); err == nil {
		t.Fatal("the conversation's hold still stands, so the delete must still be refused")
	}

	if err := retention.Release(sess.ID, "node", "ctx-7"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := mgr.Delete(sess.ID); err != nil {
		t.Fatalf("with nothing holding it the session must delete: %v", err)
	}
}

func TestAHoldCannotBeTakenOnASessionThatIsGone(t *testing.T) {
	_, _, mgr, retention := newRetentionFixture(t)
	fillTo(t, mgr, 1)

	// The race the design closes by verifying inside the lock: create the session, protect it,
	// and hear it if the session lost that race — rather than working in a directory nobody
	// owns.
	err := retention.Acquire(Hold{SessionID: "2020-01-01-000000-deadbeef", Owner: "node", Reason: ReasonTask, Ref: "t"})
	if err == nil {
		t.Fatal("protecting a session that does not exist must fail")
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("the refusal should say the session is gone, got %v", err)
	}
}

func TestAnExpiredHoldStopsProtectingAndIsSwept(t *testing.T) {
	_, store, mgr, retention := newRetentionFixture(t)
	sess := fillTo(t, mgr, 1)[0]

	if err := retention.Acquire(Hold{
		SessionID: sess.ID, Owner: "node", Reason: ReasonContext, Ref: "ctx-1",
		ExpiresAt: time.Now().Add(20 * time.Millisecond),
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := mgr.Delete(sess.ID); err == nil {
		t.Fatal("a hold within its window must refuse the delete")
	}

	time.Sleep(40 * time.Millisecond)

	// Expiry is decided by the clock, not by whoever reads the record: a node that never runs
	// again must not leave a session protected forever.
	if err := mgr.Delete(sess.ID); err != nil {
		t.Fatalf("an expired hold must not keep refusing: %v", err)
	}
	if _, err := store.LoadMeta(sess.ID); err == nil {
		t.Fatal("the session should be gone")
	}

	// And the expired record itself is cleaned up by the sweep rather than lingering.
	// Sweep is the clock's half of a hold's lifetime: what has passed is dropped from the
	// record, and what has not is left alone.
	live := fillTo(t, mgr, 1)[0]
	for _, h := range []Hold{
		{SessionID: live.ID, Owner: "node", Reason: ReasonContext, Ref: "ctx-expired", ExpiresAt: time.Now().Add(-time.Hour)},
		{SessionID: live.ID, Owner: "node", Reason: ReasonContext, Ref: "ctx-live", ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if err := retention.Acquire(h); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}
	dropped, err := retention.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if dropped == 0 {
		t.Error("an expired hold must be dropped, not kept as a record nobody will ever clear")
	}
	holds, err := retention.Held(live.ID)
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	if len(holds) != 1 || holds[0].Ref != "ctx-live" {
		t.Errorf("the sweep should have kept exactly the live hold, got %v", holds)
	}
}

func TestHoldsByOwnerReportsOnlyThatOwnersHolds(t *testing.T) {
	_, _, mgr, retention := newRetentionFixture(t)
	created := fillTo(t, mgr, 2)

	holds := []Hold{
		{SessionID: created[0].ID, Owner: "node", Reason: ReasonTask, Ref: "t1"},
		{SessionID: created[0].ID, Owner: "other", Reason: ReasonTask, Ref: "t2"},
		{SessionID: created[1].ID, Owner: "node", Reason: ReasonContext, Ref: "c1"},
	}
	for _, h := range holds {
		if err := retention.Acquire(h); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}

	mine, err := retention.HoldsByOwner("node")
	if err != nil {
		t.Fatalf("HoldsByOwner: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("expected two sessions with this owner's holds, got %v", mine)
	}
	if got := mine[created[0].ID]; len(got) != 1 || got[0].Ref != "t1" {
		t.Errorf("session %s: got %v", created[0].ID, got)
	}
	if got := mine[created[1].ID]; len(got) != 1 || got[0].Reason != ReasonContext {
		t.Errorf("session %s: got %v", created[1].ID, got)
	}
}

func TestReacquiringTheSameRefReplacesItRatherThanAdding(t *testing.T) {
	_, _, mgr, retention := newRetentionFixture(t)
	sess := fillTo(t, mgr, 1)[0]

	// A context that keeps being used keeps refreshing ONE record: a grown list would make the
	// refusal unreadable and the file unbounded.
	for i := range 3 {
		if err := retention.Acquire(Hold{
			SessionID: sess.ID, Owner: "node", Reason: ReasonContext, Ref: "ctx-1",
			ExpiresAt: time.Now().Add(time.Duration(i+1) * time.Hour),
		}); err != nil {
			t.Fatalf("Acquire #%d: %v", i, err)
		}
	}

	holds, err := retention.Held(sess.ID)
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	if len(holds) != 1 {
		t.Fatalf("refreshing a hold must not add a second one: %v", holds)
	}
	if time.Until(holds[0].ExpiresAt) < 2*time.Hour {
		t.Errorf("the record should carry the latest deadline, got %s", holds[0].ExpiresAt)
	}
}

// The reason this record is on DISK. A process that wrote a hold and went away (a crash, a
// restart, a node whose task is still out there) must still keep its session: the hold is the
// promise, and the process is not.
func TestAHoldWrittenByAnotherProcessStillProtects(t *testing.T) {
	dir, store, mgr, retention := newRetentionFixture(t)
	sess := fillTo(t, mgr, 1)[0]

	cmd := exec.Command(os.Args[0]) // this test binary, re-executed
	cmd.Env = append(os.Environ(), holdHelperMode+"="+sess.ID, holdHelperDir+"="+dir)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper failed: %v (output %q)", err, out)
	}
	if !strings.Contains(string(out), "HELD") {
		t.Fatalf("helper did not take the hold: %q", out)
	}

	// The helper is gone. Its hold is not.
	if err := mgr.Delete(sess.ID); err == nil {
		t.Fatal("a session held by a process that exited must still be refused")
	}
	if _, err := store.LoadMeta(sess.ID); err != nil {
		t.Errorf("session gone: %v", err)
	}

	// And the new process can read exactly what the old one wrote.
	holds, err := retention.Held(sess.ID)
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	if len(holds) != 1 || holds[0].Owner != "helper" || holds[0].Ref != "task-from-another-process" {
		t.Fatalf("unexpected holds: %v", holds)
	}
}

func TestTheRetentionDirIsNotASession(t *testing.T) {
	// The records live inside the session data dir, so a scan must not mistake them for
	// sessions (or worse, try to read a meta.json that is not there and abort the scan).
	_, _, mgr, retention := newRetentionFixture(t)
	sess := fillTo(t, mgr, 1)[0]
	if err := retention.Acquire(Hold{SessionID: sess.ID, Owner: "node", Reason: ReasonTask, Ref: "t"}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	sessions, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected exactly the one session, got %d", len(sessions))
	}
	if sessions[0].ID != sess.ID {
		t.Errorf("listed %s, wanted %s", sessions[0].ID, sess.ID)
	}
}
