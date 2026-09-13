package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// newDeleteApp returns an app holding one on-disk session, with the service that
// deletes it. The session's directory is real (a temp store), because the whole
// point of the refusal is that the DIRECTORY survives it.
func newDeleteApp(t *testing.T) (*desktopApp, *AgentService, string, string) {
	t.Helper()
	sm := newSessionManagerForTest(t, t.TempDir())
	sid := sm.Current().ID
	dir, err := config.SessionDir()
	if err != nil {
		t.Fatalf("session dir: %v", err)
	}
	d := newTestApp()
	d.sm = sm
	d.getRun(sid).sm = sm
	return d, &AgentService{desk: d}, sid, filepath.Join(dir, sid)
}

// TestDeleteSessionRefusesARunningSession pins the rule the delete path exists to
// enforce: a session with a turn in flight cannot be deleted. Deleting underneath a
// live turn loses the transcript silently (its AppendMessage has no directory left to
// append to) and the turn keeps calling the model and running tools while the user
// believes it is gone — so the refusal has to be the backend's, not only the UI's.
//
// It also pins the two things the refusal must NOT do: touch the directory, and drop
// the run (which would leave the still-running turn unreachable and unclosable).
func TestDeleteSessionRefusesARunningSession(t *testing.T) {
	d, svc, sid, dir := newDeleteApp(t)
	d.getRun(sid).running = true

	if got := svc.DeleteSession(sid); got != refuseDeleteRunning {
		t.Fatalf("DeleteSession = %q, want %q", got, refuseDeleteRunning)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the session directory was removed despite the refusal: %v", err)
	}
	if d.runs[sid] == nil {
		t.Error("the run was dropped, leaving the still-running turn unclosable")
	}
	if !d.runs[sid].running {
		t.Error("the refusal must not clear the run's busy flag")
	}
}

// TestDeleteSessionRefusesWithoutSideEffects is the same rule stated as "nothing the
// delete touches may move": a refused call must not delete the run, must not clear
// activeID, and must not unlist the session. Each of those is something a naive guard
// placed after the mutation would have done already.
func TestDeleteSessionRefusesWithoutSideEffects(t *testing.T) {
	d, svc, sid, _ := newDeleteApp(t)
	d.activeID = sid
	d.getRun(sid).running = true

	if got := svc.DeleteSession(sid); got != refuseDeleteRunning {
		t.Fatalf("DeleteSession = %q, want %q", got, refuseDeleteRunning)
	}
	if d.activeID != sid {
		t.Errorf("activeID = %q, want the session to stay displayed", d.activeID)
	}
	list, err := d.sm.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != sid {
		t.Errorf("the refused session is no longer listed: %v", list)
	}
}

// TestDeleteSessionDeletesAnIdleSession is the other half of the contract — the refusal
// must not cost the feature. An idle session still deletes, and clearing activeID for
// the displayed one is what makes the UI fall back to picking another.
func TestDeleteSessionDeletesAnIdleSession(t *testing.T) {
	d, svc, sid, dir := newDeleteApp(t)
	d.activeID = sid

	if got := svc.DeleteSession(sid); got != "ok" {
		t.Fatalf("DeleteSession = %q, want ok", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the session directory still exists (err = %v)", err)
	}
	if d.runs[sid] != nil {
		t.Error("the run was not dropped with the session")
	}
	if d.activeID != "" {
		t.Errorf("activeID = %q, want it cleared so the UI picks another session", d.activeID)
	}
}

// TestDeleteSessionRefusalNamesTheWayOut: the string is rendered verbatim in the
// confirmation box that raised the delete, so it has to say what to do next — "cannot
// delete" alone leaves the user pressing the same button.
func TestDeleteSessionRefusalNamesTheWayOut(t *testing.T) {
	if !strings.Contains(refuseDeleteRunning, "停止") {
		t.Errorf("refuseDeleteRunning = %q, want it to name the way out (停止)", refuseDeleteRunning)
	}
}
