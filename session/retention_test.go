package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
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
		SessionID: held.ID, Owner: "node", Reason: ReasonTask, Ref: "task-1",
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

// One hold kind, one answer. A running execution refuses every deletion path — the owner's click
// and the count-based sweep alike — and releasing it lets the session go. Both halves are here
// because the pair IS the rule: the sweep and the click cannot disagree while there is a single
// reason a session can be held for.
func TestARunningExecutionRefusesEveryDeletionAndReleasingItLetsTheSessionGo(t *testing.T) {
	_, store, mgr, retention := newRetentionFixture(t)
	sess := fillTo(t, mgr, 1)[0]

	if err := retention.Acquire(Hold{
		SessionID: sess.ID, Owner: "node", Reason: ReasonTask, Ref: "task-42",
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// A turn is writing into this session right now, so the delete is refused — and the refusal
	// names WHAT to end, because the reader's next move is to end that task (or to stop it, when
	// it will not end by itself: node's `session:end`, the desktop's banner button).
	err := mgr.Delete(sess.ID)
	var refusal *SessionHeldError
	if !errors.As(err, &refusal) {
		t.Fatalf("deleting a session a task is running in must be refused with the reason, got %v", err)
	}
	if len(refusal.Holds) != 1 || refusal.Holds[0].Ref != "task-42" {
		t.Errorf("the refusal must name the running task, got %v", refusal.Holds)
	}
	if !strings.Contains(err.Error(), "结束对应任务") {
		t.Errorf("the refusal has to say what the reader can do: %q", err.Error())
	}
	if _, err := store.LoadMeta(sess.ID); err != nil {
		t.Errorf("a refused delete must not touch the session: %v", err)
	}

	// The execution ends. Nothing holds the session any more, so the ordinary lifecycle owns it
	// again: the count-based sweep may take it, and so may the reader.
	if err := retention.Release(sess.ID, "node", "task-42"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := mgr.Delete(sess.ID); err != nil {
		t.Fatalf("with no execution running, the owner's delete must go through: %v", err)
	}
	if _, err := store.LoadMeta(sess.ID); err == nil {
		t.Error("the session should be gone")
	}
}

// A reason this version RETIRED is neither honoured nor forgotten.
//
// Not honoured: the record a previous version left names a promise this version does not make
// ("keep this conversation's session for a window"), so the two questions this layer answers must
// ignore it. A reader whose only hold is that one has to be able to delete the conversation — and
// the sidebar must not offer to stop an execution that does not exist, which is what a refusal
// reading "请先结束对应任务" about a conversation with no task in it would amount to.
//
// Not forgotten: the FILE stays until its owner migrates it (node/runner.go's reconcile), which is
// the one place that knows what the reason meant. What this test also pins is that the delete
// itself is that migration's second half: with the session gone, the record has no subject left.
func TestARetiredReasonNeitherRefusesNorSurvivesItsSession(t *testing.T) {
	_, store, mgr, retention := newRetentionFixture(t)
	sess := fillTo(t, mgr, 1)[0]

	if err := retention.Acquire(Hold{
		SessionID: sess.ID, Owner: "node", Reason: ReasonContextRetired, Ref: "context-1",
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	busy, err := retention.BusySessions()
	if err != nil {
		t.Fatalf("BusySessions: %v", err)
	}
	if busy[sess.ID] {
		t.Error("a retired reason is not an execution running in a session")
	}
	if holds, err := retention.Held(sess.ID); err != nil || len(holds) != 1 {
		t.Fatalf("the record is the owner's to migrate, so it has to be readable: %v (%v)", holds, err)
	}

	if err := mgr.Delete(sess.ID); err != nil {
		t.Fatalf("a retired reason must not refuse the owner's delete: %v", err)
	}
	if _, err := store.LoadMeta(sess.ID); err == nil {
		t.Error("the session should be gone")
	}
	if holds, err := retention.Held(sess.ID); err != nil || len(holds) != 0 {
		t.Errorf("a deleted session must not leave a record naming it: %v (%v)", holds, err)
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

func TestHoldsByOwnerReportsOnlyThatOwnersHolds(t *testing.T) {
	_, _, mgr, retention := newRetentionFixture(t)
	created := fillTo(t, mgr, 2)

	holds := []Hold{
		{SessionID: created[0].ID, Owner: "node", Reason: ReasonTask, Ref: "t1"},
		{SessionID: created[0].ID, Owner: "other", Reason: ReasonTask, Ref: "t2"},
		{SessionID: created[1].ID, Owner: "node", Reason: ReasonTask, Ref: "t3"},
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
	if got := mine[created[1].ID]; len(got) != 1 || got[0].Reason != ReasonTask {
		t.Errorf("session %s: got %v", created[1].ID, got)
	}
}

func TestReacquiringTheSameRefReplacesItRatherThanAdding(t *testing.T) {
	_, _, mgr, retention := newRetentionFixture(t)
	sess := fillTo(t, mgr, 1)[0]

	// The same holder keeps refreshing ONE record rather than piling up: a grown list would make
	// the refusal unreadable and the file unbounded.
	for range 3 {
		if err := retention.Acquire(Hold{
			SessionID: sess.ID, Owner: "node", Reason: ReasonTask, Ref: "task-1",
		}); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}

	holds, err := retention.Held(sess.ID)
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	if len(holds) != 1 {
		t.Fatalf("refreshing a hold must not add a second one: %v", holds)
	}
	if holds[0].Ref != "task-1" {
		t.Errorf("the kept record should be the one just written, got %+v", holds[0])
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
