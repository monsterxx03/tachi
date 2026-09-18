package main

import (
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
)

// newPinApp returns an app holding one on-disk session, plus the service and the session id:
// the shape SetSessionPinned needs (a real store, so the write is observable where it matters
// — meta.json, which is what makes a pin survive a restart).
func newPinApp(t *testing.T) (*desktopApp, *AgentService, string) {
	t.Helper()
	sm := newSessionManagerForTest(t, t.TempDir())
	sid := sm.Current().ID
	d := newTestApp()
	d.sm = sm
	return d, &AgentService{desk: d}, sid
}

// TestSetSessionPinnedPersistsAndLists pins the round trip the sidebar depends on: the flag
// reaches meta.json (the order has to survive a restart) and ListSessions carries it back out
// (the frontend sorts on it). Unpinning has to work the same way — a 置顶 the user cannot undo
// is worse than none, and a toggle that only ever writes `true` would pass a one-way test.
func TestSetSessionPinnedPersistsAndLists(t *testing.T) {
	d, svc, sid := newPinApp(t)

	if got := svc.SetSessionPinned(sid, true); got != "ok" {
		t.Fatalf("SetSessionPinned(true) = %q, want ok", got)
	}
	if sess := mustLoad(t, d.sm, sid); !sess.Pinned {
		t.Error("the pin did not reach the session record")
	}
	if list := svc.ListSessions(); len(list) != 1 || !list[0].Pinned {
		t.Errorf("ListSessions did not carry the pin back: %+v", list)
	}

	if got := svc.SetSessionPinned(sid, false); got != "ok" {
		t.Fatalf("SetSessionPinned(false) = %q, want ok", got)
	}
	if sess := mustLoad(t, d.sm, sid); sess.Pinned {
		t.Error("unpinning did not clear the flag")
	}
	if list := svc.ListSessions(); len(list) != 1 || list[0].Pinned {
		t.Errorf("ListSessions still reports a pin: %+v", list)
	}
}

// TestSetSessionPinnedKeepsTheOtherMeta is the reason the write goes through
// updateSessionMeta rather than a load-mutate-save of its own: UpdateMeta rewrites the WHOLE
// meta.json, so a stale read clobbers everything that moved in between. Here the title and the
// workspace are on the record and have to survive a pin — the two fields a running turn (title
// generation) and the folder picker write behind the user's back.
func TestSetSessionPinnedKeepsTheOtherMeta(t *testing.T) {
	d, svc, sid := newPinApp(t)

	sess := mustLoad(t, d.sm, sid)
	sess.Title = "置顶前的标题"
	sess.WorkingDir = "/my/project"
	if err := d.sm.UpdateMeta(sess); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := svc.SetSessionPinned(sid, true); got != "ok" {
		t.Fatalf("SetSessionPinned = %q, want ok", got)
	}

	after := mustLoad(t, d.sm, sid)
	if !after.Pinned || after.Title != "置顶前的标题" || after.WorkingDir != "/my/project" {
		t.Errorf("the pin clobbered neighbouring metadata: %+v", after)
	}
}

// TestSetSessionPinnedDuringARunningTurn: pinning is offered on every row, including one whose
// conversation is working, and the desktop keeps a RUNNING session in its own per-run manager
// whose record the turn is mutating. The flag therefore has to be written through that record,
// not through a fresh load: a load-mutate-save would rewrite meta.json from a copy taken before
// the turn's own writes and silently undo them — here, the title the turn just produced.
func TestSetSessionPinnedDuringARunningTurn(t *testing.T) {
	d, svc, sid := newPinApp(t)
	run := d.getRun(sid)
	run.sm = d.sm
	run.running = true
	// A title this turn has already generated, living on the bound record only — NOT on disk
	// until the write below carries it there.
	run.sm.Current().Title = "这一轮刚生成的标题"

	if got := svc.SetSessionPinned(sid, true); got != "ok" {
		t.Fatalf("SetSessionPinned = %q, want ok", got)
	}

	onDisk := mustLoad(t, d.sm, sid)
	if !onDisk.Pinned {
		t.Error("the pin did not reach meta.json")
	}
	if onDisk.Title != "这一轮刚生成的标题" {
		t.Errorf("the pin was written from a stale copy — the turn's title is gone: %q", onDisk.Title)
	}
}

// TestSetSessionPinnedRefusesWithoutAStore: the one failure the call has. The frontend rolls its
// optimistic row back on anything that is not "ok", so what matters is that this is not "ok".
func TestSetSessionPinnedRefusesWithoutAStore(t *testing.T) {
	svc := &AgentService{desk: newTestApp()}
	if got := svc.SetSessionPinned("any", true); got == "ok" || got == "" {
		t.Fatalf("SetSessionPinned with no session manager = %q, want a refusal", got)
	}
}

// TestSessionInfoCarriesThePin: toSessionInfo is the one hop between the record and the
// sidebar's rows, and a field it forgets is a feature that silently does nothing — every pin
// would be written, listed and ignored. It is checked against a record read back from the
// store, so it pins the mapping (Pinned → JSON `pinned` → the frontend's sort), not a struct
// this process happens to still be holding.
func TestSessionInfoCarriesThePin(t *testing.T) {
	_, svc, sid := newPinApp(t)
	if got := svc.SetSessionPinned(sid, true); got != "ok" {
		t.Fatalf("SetSessionPinned = %q, want ok", got)
	}
	// A second store over the same directory — exactly what a fresh process builds, so nothing
	// below can come from this process's memory.
	dir, err := config.SessionDir()
	if err != nil {
		t.Fatalf("session dir: %v", err)
	}
	other, err := session.NewFileStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sess, err := other.LoadMeta(sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !toSessionInfo(sess).Pinned {
		t.Error("toSessionInfo dropped the pin passed to it (its JSON field is what the sidebar reads)")
	}
}

func mustLoad(t *testing.T, sm *session.Manager, id string) *session.Session {
	t.Helper()
	sess, err := sm.Load(id)
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	return sess
}
