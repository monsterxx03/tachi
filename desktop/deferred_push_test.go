package main

// The deferred-push rule (desktop/deferred_push.go): a settings save must not swap, under a
// running turn, the fields that turn reads unlocked — so it queues the push for that session and
// endTurn runs it.

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// pushFixture returns an app with ONE prepared session whose agent is a bare AIAgent, and a
// config.yaml that already carries `hooks.enabled` (SavePartial refuses a key the file does not
// have). The `hooks` row is the probe: its push is observable from here as
// `agent.Config.HookDispatcher`, and it is a per-session row, so it goes through the fan-out.
func pushFixture(t *testing.T) (*desktopApp, *AgentService, *sessionRun, string) {
	t.Helper()
	sm := newSessionManagerForTest(t, t.TempDir()) // sets the base dir; the fixture re-sets it
	sid := sm.Current().ID
	d, svc, _ := settingsFixture(t, `hooks:
  enabled: false
  events:
    tool_call:
      - command: echo hi
`)
	d.sm = sm
	r := d.getRun(sid)
	r.sm = sm
	r.agent = &agent.AIAgent{Config: agent.AgentConfig{Logger: logger.New("settings-test")}}
	return d, svc, r, sid
}

// TestSaveSettingsDefersAPushForARunningTurn is the whole rule end to end: the value reaches the
// file, the push does NOT reach the running agent, the summary says so (instead of counting the
// key as live), and the push lands as soon as the turn ends.
func TestSaveSettingsDefersAPushForARunningTurn(t *testing.T) {
	d, svc, r, sid := pushFixture(t)
	r.running = true

	res := svc.SaveSettings(map[string]any{"hooks.enabled": true})
	if !strings.Contains(res.Summary, "hooks.enabled") || !strings.Contains(res.Summary, "等这一轮结束生效") {
		t.Errorf("summary = %q, want it to say the key waits for the running turn", res.Summary)
	}
	if strings.Contains(res.Summary, "项已生效") {
		t.Errorf("summary = %q: a deferred key is not in effect yet", res.Summary)
	}
	if !strings.Contains(res.Summary, sid) {
		t.Errorf("summary = %q, want the running session named", res.Summary)
	}
	if r.agent.Config.HookDispatcher != nil {
		t.Error("the dispatcher was built under a running turn: the push was not deferred")
	}
	if n := len(d.pendingPushes[sid]); n != 1 {
		t.Fatalf("queued pushes for the session = %d, want 1", n)
	}

	// The turn ends: the queue runs, on the same agent.
	r.running = false
	d.endTurn(sid, "done")
	if r.agent.Config.HookDispatcher == nil {
		t.Error("the deferred push did not run at the turn end")
	}
	if n := len(d.pendingPushes); n != 0 {
		t.Errorf("the queue holds %d entries after the flush, want 0", n)
	}
}

// TestSaveSettingsPushesImmediatelyWhenNoTurnIsRunning is the other half of the rule: the
// deferral is about a RUNNING turn, not about saving. An idle session is updated on the spot.
func TestSaveSettingsPushesImmediatelyWhenNoTurnIsRunning(t *testing.T) {
	d, svc, r, _ := pushFixture(t)

	res := svc.SaveSettings(map[string]any{"hooks.enabled": true})
	if !strings.Contains(res.Summary, "1 项已生效") {
		t.Errorf("summary = %q, want the key reported live", res.Summary)
	}
	if r.agent.Config.HookDispatcher == nil {
		t.Error("an idle session must be pushed to at once")
	}
	if n := len(d.pendingPushes); n != 0 {
		t.Errorf("nothing should be queued, got %d entries", n)
	}
}

// TestSaveSettingsStillPushesTheOtherSessions: the deferral is per session. A busy session's push
// waits; an idle one's does not.
func TestSaveSettingsStillPushesTheOtherSessions(t *testing.T) {
	d, svc, busy, busyID := pushFixture(t)
	busy.running = true

	idle := d.getRun("idle-session")
	idle.agent = &agent.AIAgent{Config: agent.AgentConfig{Logger: logger.New("settings-test")}}

	res := svc.SaveSettings(map[string]any{"hooks.enabled": true})
	if idle.agent.Config.HookDispatcher == nil {
		t.Error("the idle session was not pushed to")
	}
	if busy.agent.Config.HookDispatcher != nil {
		t.Error("the busy session must wait for its turn to end")
	}
	if n := len(d.pendingPushes[busyID]); n != 1 {
		t.Errorf("queued pushes for the busy session = %d, want 1", n)
	}
	if strings.Contains(res.Summary, "项已生效") {
		t.Errorf("summary = %q: the key is not in effect for the running session yet", res.Summary)
	}
}

// TestDeferredPushFollowsACompactionMove: a compaction can move the conversation to a new session
// id mid-turn, and the queue was keyed by the id in force when the user saved. The flush at the
// turn end drains both, so a deferred change cannot be stranded under an id nobody uses again.
func TestDeferredPushFollowsACompactionMove(t *testing.T) {
	d, _, r, oldID := pushFixture(t)
	next := "compacted-session"

	ran := 0
	d.deferPush(oldID, func(liveRun) error { ran++; return nil })
	r.compaction = &compactionRecord{SessionID: next}
	r.running = false
	// The session is the displayed one, as it is during a real turn — followCompaction moves the
	// active id with the run, which is what the frontend follows.
	d.mu.Lock()
	d.activeID = oldID
	d.mu.Unlock()

	d.endTurn(oldID, "done")
	if ran != 1 {
		t.Errorf("the push queued under the pre-compaction id ran %d times, want 1", ran)
	}
	if n := len(d.pendingPushes); n != 0 {
		t.Errorf("the queue holds %d entries after the flush, want 0", n)
	}
	if current := d.currentID(); current != next {
		t.Errorf("currentID() = %q, want %q (the fixture should have moved the run)", current, next)
	}
}

// TestDeferredPushIsRequeuedWhenANewTurnAlreadyStarted: the composer drains its queued messages on
// agent:idle, so a new turn can be running by the time the flush gets there. Pushing then would be
// exactly the race the queue exists to avoid — the queue waits for the next turn's end instead.
func TestDeferredPushIsRequeuedWhenANewTurnAlreadyStarted(t *testing.T) {
	d, _, r, sid := pushFixture(t)

	ran := 0
	d.deferPush(sid, func(liveRun) error { ran++; return nil })
	r.running = true // the next turn started first

	d.flushDeferredPushes(sid, sid)
	if ran != 0 {
		t.Error("the push ran under a new turn")
	}
	if n := len(d.pendingPushes[sid]); n != 1 {
		t.Errorf("queued pushes = %d, want the push kept for the next turn end", n)
	}
}

// TestDeferredPushIsDroppedWhenTheSessionIsGone: a session closed before its turn ends leaves a
// queue entry behind; the flush drops it (the value is already in config.yaml, so the next start
// reads it) rather than pushing into a run that no longer exists.
func TestDeferredPushIsDroppedWhenTheSessionIsGone(t *testing.T) {
	d, _, _, sid := pushFixture(t)

	ran := 0
	d.deferPush(sid, func(liveRun) error { ran++; return nil })
	d.mu.Lock()
	delete(d.runs, sid)
	d.mu.Unlock()

	d.flushDeferredPushes(sid, sid)
	if ran != 0 {
		t.Error("a push ran for a session that no longer exists")
	}
	if n := len(d.pendingPushes); n != 0 {
		t.Errorf("the dropped queue lingers: %d entries", n)
	}
}
