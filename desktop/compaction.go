package main

// Auto-compaction bookkeeping for the desktop.
//
// The agent loop compacts in-flight when the token estimate crosses the
// configured threshold (agent.maybeAutoCompact): it asks the model for a
// history summary and moves the conversation into a CHILD session linked to its
// parent (agent.FinalizeCompact). None of that is visible to a frontend on its
// own — the run would keep appending to the child while the UI still labelled
// the parent — so the desktop records the move while the turn runs (see the
// AgentEventAutoCompactDone case in handleEvent) and re-homes the run here, once
// the stream has settled.
//
// Between those two moments the conversation is the CHILD while the run is still keyed to the
// parent, and that window is not purely a presentation matter: the child must not be treated as
// an ordinary session, or a message sent in it starts a SECOND turn appending to the same
// session file (two rows spinning, one interleaved history). The record kept here is what the
// bookkeeping reads to prevent that — see ownerOfLocked, which resolves the child to this run.

import (
	"context"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// compactionRecord is one auto-compaction: the session the conversation moved
// into, and what the summary replaced.
type compactionRecord struct {
	SessionID   string
	OldMsgCount int
	Summary     string
}

// followCompaction re-homes a run onto the session auto-compaction created for
// it during the turn that just ended, and tells the frontend to move with it —
// its per-session state (transcript, pagination cursor, pending queue, pending
// question) is all keyed by session ID. It returns the id the caller should use
// from here on: the new session, or the original one when nothing moved.
//
// The old key is dropped rather than aliased: from here on the conversation
// lives in the child session, which is what every later turn, reload and usage
// report has to agree on. The parent stays on disk (and in the sidebar) as the
// pre-compaction history, exactly as in the TUI.
func (d *desktopApp) followCompaction(id string) string {
	d.mu.Lock()
	r := d.runs[id]
	if r == nil || r.compaction == nil {
		d.mu.Unlock()
		return id
	}
	moved := r.compaction
	r.compaction = nil
	next := moved.SessionID
	// Defensive: a zero or unchanged id would silently orphan the run's state,
	// so leave the run where it is and let the next turn write to the child.
	if next == "" || next == id {
		d.mu.Unlock()
		return id
	}
	// The child's key may not be TAKEN from a run that is using it. An entry here means a
	// second run owns the child — which ownerOfLocked exists to prevent, and which is how one
	// conversation comes to be written by two turns at once (see the note at the top). While
	// that run is mid-turn the move is refused outright: clobbering it would drop the run whose
	// turn is in flight (its state updates, its steer channel and its busy guard all live on
	// that entry) and leave the conversation looking idle under a turn that is still running.
	// Nothing else can reach here — a session that has just been created has no run of its own
	// — so this is the loud floor under that invariant, not a case with a friendly outcome.
	var loser *sessionRun
	if other := d.runs[next]; other != nil && other != r {
		if other.running {
			d.mu.Unlock()
			logger.New("desktop").Warn(context.Background(),
				"followCompaction: 子会话上挂着一个正在跑的 run，不搬（同一条对话被两个回合同时写）",
				"from", id, "to", next)
			return id
		}
		loser = other
	}
	delete(d.runs, id)
	d.runs[next] = r
	if d.activeID == id {
		d.activeID = next
	}
	d.mu.Unlock()

	if loser != nil {
		// An idle entry for a session created moments ago is stale bookkeeping (a LoadSession
		// that raced the compaction). The conversation's own run must hold the key; the loser's
		// agent — built for a session it never ran in — is closed rather than left to leak, the
		// same way prepareSession closes the agent that lost its build race.
		logger.New("desktop").Warn(context.Background(),
			"followCompaction: 子会话上原有的空 run 被替换，其 agent 已关闭", "from", id, "to", next)
		if loser.agent != nil {
			loser.agent.Close()
		}
	}

	// The approval mode is the SESSION's, and the agent carrying it is the same object across the
	// move — so a mode left in place would outlive the record it came from: the child starts at
	// 「需要审批」 (FinalizeCompact's whitelist does not carry skip_approval, and nobody was asked
	// whether the new conversation wants the asks off), while the agent would go on approving what
	// the child's record says must be asked about. Re-point it, exactly like the tree-scoped state
	// beginTurn re-points: the two halves describe ONE session, and here the session changed.
	if r.agent != nil && r.sm != nil {
		if cur := r.sm.Current(); cur != nil {
			r.agent.SetAllowAllAsksForSession(cur.SkipApproval)
		}
	}

	if d.hasUI() {
		d.emit("agent:session_switched", map[string]any{
			"previousId": id,
			"sessionId":  next,
		})
	}
	// 两块屏的通知不在这里发：它们要带上这一轮结束的原因（见 endTurn）。
	return next
}
