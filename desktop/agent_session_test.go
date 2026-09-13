package main

import "testing"

// TestDeleteSessionRefusesARunningTurn pins the one rule that keeps a delete from racing the turn
// it is deleting: a running conversation is still writing into its own directory (messages, usage
// rows, tool results), and its events are addressed to a conversation that would no longer exist.
//
// The refusal lives in AgentService and not only in the sidebar menu: a menu opened before the
// turn started is stale by the time it is clicked, which is exactly when the rule has to hold.
func TestDeleteSessionRefusesARunningTurn(t *testing.T) {
	d, svc, sid := newRootsApp(t, t.TempDir())
	if d.sm == nil {
		t.Fatal("fixture: the session manager must exist")
	}

	// Running: refused, and the session survives.
	d.getRun(sid).running = true
	if got := svc.DeleteSession(sid); got != "会话正在运行，先停止再删除" {
		t.Errorf("a running session must be refused, got %q", got)
	}
	if _, err := d.sm.Load(sid); err != nil {
		t.Errorf("the refused delete must not have touched the session: %v", err)
	}

	// Idle: the same call goes through — which is what makes the refusal above about RUNNING
	// rather than about deleting in general.
	d.getRun(sid).running = false
	if got := svc.DeleteSession(sid); got != "ok" {
		t.Fatalf("an idle session must be deletable, got %q", got)
	}
	if _, err := d.sm.Load(sid); err == nil {
		t.Error("the session should be gone")
	}
}
