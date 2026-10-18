package main

import (
	"testing"

	"github.com/monsterxx03/tachi/session"
)

// The sidebar's 跳过记忆整理 action writes the flag the dream pipeline filters
// on, and it writes it through the same in-memory snapshot a running turn will
// save next — a plain file rewrite would be clobbered by that turn.
func TestSetSessionSkipDream(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sm := session.NewManagerWithStore(store, nil)
	sess, err := sm.New("alpha", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	d := newDesktopApp()
	d.sm = sm
	svc := &AgentService{desk: d}

	if got := svc.SetSessionSkipDream(sess.ID, true); got != "ok" {
		t.Fatalf("SetSessionSkipDream(true) = %q, want ok", got)
	}
	onDisk, err := store.LoadMeta(sess.ID)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if !onDisk.SkipDream {
		t.Fatal("skip_dream was not written to the session record")
	}

	if got := svc.SetSessionSkipDream(sess.ID, false); got != "ok" {
		t.Fatalf("SetSessionSkipDream(false) = %q, want ok", got)
	}
	onDisk, _ = store.LoadMeta(sess.ID)
	if onDisk.SkipDream {
		t.Fatal("restoring consolidation did not clear skip_dream")
	}
}

// A session a subsystem owns (a remote execution) is fixed at skipped: local
// memory must not digest somebody else's conversation, so the UI's "恢复记忆整理"
// is refused rather than silently ignored.
func TestSetSessionSkipDreamRefusesManagedSession(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sm := session.NewManagerWithStore(store, nil)
	sess, err := sm.New("alpha", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	sess.Kind = session.KindA2AExecution
	if err := sm.UpdateMeta(sess); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}

	d := newDesktopApp()
	d.sm = sm
	svc := &AgentService{desk: d}

	if got := svc.SetSessionSkipDream(sess.ID, false); got == "ok" {
		t.Fatal("a managed session must not be allowed back into memory consolidation")
	}
}
