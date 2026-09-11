package main

import "testing"

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
