package main

import (
	"testing"

	"github.com/monsterxx03/tachi/agent"
)

func TestFollowCompactionReHomesRun(t *testing.T) {
	d := newDesktopApp()
	run := &sessionRun{state: AgentState{Status: StatusIdle, Label: "空闲"}}
	d.runs["old"] = run
	d.activeID = "old"
	run.compaction = &compactionRecord{SessionID: "new", OldMsgCount: 42, Summary: "摘要"}

	if got := d.followCompaction("old"); got != "new" {
		t.Errorf("followCompaction() = %q, want the new session", got)
	}

	if _, stillThere := d.runs["old"]; stillThere {
		t.Error("the pre-compaction key is still holding the run")
	}
	if d.runs["new"] != run {
		t.Error("the run was not re-homed onto the compacted session")
	}
	if d.activeID != "new" {
		t.Errorf("activeID = %q, want the compacted session", d.activeID)
	}
	if run.compaction != nil {
		t.Error("the compaction record should be consumed once the run has moved")
	}
}

func TestFollowCompactionIsInertWithoutCompaction(t *testing.T) {
	d := newDesktopApp()
	run := &sessionRun{state: AgentState{Status: StatusIdle, Label: "空闲"}}
	d.runs["a"] = run
	d.activeID = "a"

	// A run that did not compact, a run that has no record, and an unknown id
	// are all no-ops (and must not panic or move anything).
	if got := d.followCompaction("a"); got != "a" {
		t.Errorf("followCompaction() = %q, want the unchanged id", got)
	}
	if got := d.followCompaction("missing"); got != "missing" {
		t.Errorf("followCompaction(unknown) = %q, want the given id", got)
	}

	// A record pointing at the run's own id is ignored too: re-homing onto
	// itself would delete the run's state under a live turn.
	run.compaction = &compactionRecord{SessionID: "a"}
	if got := d.followCompaction("a"); got != "a" {
		t.Errorf("self-referential record moved the run to %q", got)
	}
	run.compaction = &compactionRecord{SessionID: ""}
	if got := d.followCompaction("a"); got != "a" {
		t.Errorf("empty record moved the run to %q", got)
	}

	if len(d.runs) != 1 || d.runs["a"] != run {
		t.Errorf("runs = %v, want the original run under its own key", d.runs)
	}
	if d.activeID != "a" {
		t.Errorf("activeID = %q, want a", d.activeID)
	}
}

// ── the compaction window: between FinalizeCompact and the turn's end ──
//
// The conversation has already moved into the child while the run is still keyed to the parent.
// Every case below is one of the things that window used to get wrong: the child treated as an
// ordinary idle session, which is how one conversation came to be written by two turns at once
// (see ownerOfLocked).

// compactingApp returns an app with one running turn and the child it has compacted into.
func compactingApp(t *testing.T, state AgentState) (*desktopApp, *sessionRun) {
	t.Helper()
	d := newDesktopApp()
	run := &sessionRun{state: state, running: true}
	d.runs["parent"] = run
	d.activeID = "parent"
	run.compaction = &compactionRecord{SessionID: "child", OldMsgCount: 42}
	return d, run
}

func TestTheCompactionChildBelongsToTheRunProducingIntoIt(t *testing.T) {
	d, run := compactingApp(t, AgentState{Status: StatusThinking, Label: "思考"})

	for _, id := range []string{"child"} {
		if got := d.runOf(id); got != run {
			t.Errorf("runOf(%q) = %v, want the run producing into it", id, got)
		}
		if got := d.getRun(id); got != run {
			t.Errorf("getRun(%q) = %v, want the run producing into it", id, got)
		}
	}
	// The alias is a resolution rule, not an entry: nothing may end up filed under the child's
	// key while the turn is still running (that entry is what followCompaction has to be
	// careful about).
	if _, created := d.runs["child"]; created {
		t.Error("resolving the child left a run entry under its own key")
	}
	// The turn's own key still answers with it, and a session nobody owns is still nil.
	if got := d.runOf("parent"); got != run {
		t.Errorf("runOf(parent) = %v, want the same run", got)
	}
	if got := d.runOf("elsewhere"); got != nil {
		t.Errorf("runOf(elsewhere) = %v, want nil", got)
	}
}

func TestTheCompactionChildRefusesANewTurn(t *testing.T) {
	d, run := compactingApp(t, AgentState{Status: StatusToolRunning, Label: "执行"})

	if got := d.startTurn("child", "再发一条"); got != refuseTurnRunning {
		t.Errorf("startTurn(child) = %q, want the running refusal", got)
	}
	// The window closes when the turn settles: the conversation is the child's from then on, and
	// it takes a turn like any other session (nothing about it is special any more).
	run.running = false
	if _, _, _, _, ok := d.beginTurn("child", d.runOf("child")); !ok {
		t.Error("the child should accept a turn once its turn has settled")
	}
}

func TestSteerReachesTheTurnProducingIntoTheChild(t *testing.T) {
	d, run := compactingApp(t, AgentState{Status: StatusThinking, Label: "思考"})
	run.steerCh = make(chan agent.SteerInput, 1)
	svc := &AgentService{desk: d}

	// Read off the map by the child's id alone this answered "not running": the frontend had
	// already shown the text in the transcript, the loop's steer_check timed out, and the
	// message was lost.
	if got := svc.Steer("child", "把这条接上"); got != "ok" {
		t.Fatalf("Steer(child) = %q, want ok", got)
	}
	select {
	case in := <-run.steerCh:
		if in.Text != "把这条接上" {
			t.Errorf("steered text = %q, want the message", in.Text)
		}
	default:
		t.Error("the steer did not reach the turn producing into the child")
	}
}

func TestRunningSessionsMarksBothRowsOfTheChain(t *testing.T) {
	d, _ := compactingApp(t, AgentState{Status: StatusThinking, Label: "思考"})
	svc := &AgentService{desk: d}

	// BOTH ids, because the sidebar renders whichever of the two its own session list holds —
	// nothing refreshes that list mid-turn, so the child's row may not exist yet, and the reader
	// is watching the parent's live transcript either way. They are two rows of ONE conversation
	// (collapsed, the parent hangs under the child as 「压缩前」).
	got := svc.RunningSessions()
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if len(got) != 2 || !seen["parent"] || !seen["child"] {
		t.Errorf("RunningSessions() = %v, want the parent's key and the child", got)
	}

	// The alias is gone with the pending record: once the turn settles, the child is the
	// conversation's own key and there is nothing left to report twice.
	d.runs["child"] = d.runs["parent"]
	delete(d.runs, "parent")
	d.runs["child"].compaction = nil
	got = svc.RunningSessions()
	if len(got) != 1 || got[0] != "child" {
		t.Errorf("RunningSessions() = %v, want [child] after the move", got)
	}
}

func TestDeletingTheCompactionChildIsRefusedWhileItsTurnRuns(t *testing.T) {
	sm := newSessionManagerForTest(t, t.TempDir())
	parentID := sm.Current().ID
	child, err := sm.New("", t.TempDir())
	if err != nil {
		t.Fatalf("new child session: %v", err)
	}

	d := newDesktopApp()
	d.sm = sm
	run := &sessionRun{state: AgentState{Status: StatusThinking, Label: "思考"}, running: true, sm: sm}
	d.runs[parentID] = run
	run.compaction = &compactionRecord{SessionID: child.ID}
	svc := &AgentService{desk: d}

	// The turn is appending to the child right now: deleting it would lose the conversation
	// under a turn whose writes are keyed by that very id.
	if got := svc.DeleteSession(child.ID); got != refuseDeleteRunning {
		t.Errorf("DeleteSession(child) = %q, want the running refusal", got)
	}
	if _, err := sm.Load(child.ID); err != nil {
		t.Errorf("the child session was deleted: %v", err)
	}
}

func TestFollowCompactionDoesNotDisplaceALiveRun(t *testing.T) {
	d, parent := compactingApp(t, AgentState{Status: StatusThinking, Label: "思考"})
	// The state this guards against: a second run already owns the child and is mid-turn (a
	// second turn started on the same conversation — what ownerOfLocked exists to prevent).
	other := &sessionRun{state: AgentState{Status: StatusThinking, Label: "思考"}, running: true}
	d.runs["child"] = other

	if got := d.followCompaction("parent"); got != "parent" {
		t.Errorf("followCompaction() = %q, want no move while the child has a live run", got)
	}
	if d.runs["child"] != other || d.runs["parent"] != parent {
		t.Error("a refused move must leave BOTH runs exactly where they were")
	}
	if parent.compaction != nil {
		t.Error("the pending record is consumed either way: the conversation is not moving")
	}
	if d.activeID != "parent" {
		t.Errorf("activeID = %q, want the parent (no switch was announced)", d.activeID)
	}
}

func TestFollowCompactionReplacesAnIdleRunOnTheChild(t *testing.T) {
	d, parent := compactingApp(t, AgentState{Status: StatusThinking, Label: "思考"})
	// An idle entry for a session that was created a moment ago is stale bookkeeping, not a use
	// of it: the conversation's own run must hold the key.
	stale := &sessionRun{state: AgentState{Status: StatusIdle, Label: "空闲"}}
	d.runs["child"] = stale

	if got := d.followCompaction("parent"); got != "child" {
		t.Errorf("followCompaction() = %q, want the child", got)
	}
	if d.runs["child"] != parent {
		t.Error("the conversation's run did not take the child's key")
	}
	if _, stillThere := d.runs["parent"]; stillThere {
		t.Error("the parent's key still holds the run")
	}
}

func TestEndTurnSettlesTheConversationOntoTheChild(t *testing.T) {
	d, run := compactingApp(t, AgentState{Status: StatusThinking, Label: "思考"})

	d.endTurn("parent", "complete")

	if d.runs["child"] != run {
		t.Fatalf("runs = %v, want the run re-homed onto the child", d.runs)
	}
	if run.running {
		t.Error("the turn must be over once endTurn has run")
	}
	if run.compaction != nil {
		t.Error("the pending record should be consumed by the move")
	}
	if d.activeID != "child" {
		t.Errorf("activeID = %q, want the child", d.activeID)
	}
	// And the alias is gone with it: the child is an ordinary session now, owned by its own key.
	if got := d.runOf("child"); got != run {
		t.Errorf("runOf(child) = %v, want the settled run", got)
	}
}

// TestFollowCompactionDropsTheApprovalMode — 审批模式属于**会话**，而搬家的那个 agent 是同一个对象。
//
// 新会话的记录是新的，而压缩的白名单**故意**不带走 skip_approval（agent/compact.go）：它只是不
// 带过去。agent 那一半不会自己知道这件事，于是一个「无需审批」的会话压缩之后，屏幕上那条新行顶着
// 一个「需要审批」的下拉框，而工具照旧不问 —— 控件和它的行为说两件事。
//
// 这里走的是真的 FinalizeCompact：白名单有没有带走这个字段，正是这条断言要问的事。
func TestFollowCompactionDropsTheApprovalMode(t *testing.T) {
	dir := t.TempDir()
	sm := newSessionManagerForTest(t, dir)
	sid := sm.Current().ID
	parent := mustLoad(t, sm, sid)
	parent.SkipApproval = true
	if err := sm.UpdateMeta(parent); err != nil {
		t.Fatalf("seed the parent's mode: %v", err)
	}
	a := newPermissionTestAgent(t)
	a.SetAllowAllAsksForSession(true) // 父会话答过「无需审批」

	d := newTestApp()
	d.runs[sid] = &sessionRun{sm: sm, agent: a}
	d.activeID = sid

	if _, err := agent.FinalizeCompact(sm, "sys", "摘要"); err != nil {
		t.Fatalf("FinalizeCompact: %v", err)
	}
	child := sm.Current()
	if child == nil || child.ID == sid {
		t.Fatal("compaction did not move the conversation into a child session")
	}
	if child.SkipApproval {
		t.Error("压缩出来的新会话不该继承这个模式")
	}

	d.runs[sid].compaction = &compactionRecord{SessionID: child.ID}
	if got := d.followCompaction(sid); got != child.ID {
		t.Fatalf("followCompaction() = %q, want the child", got)
	}
	if a.PermState.AllowAllAsks {
		t.Error("搬到子会话后 agent 还在放行：新会话说「需要审批」，行为却是不问")
	}
}
