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
	delete(d.runs, id)
	d.runs[next] = r
	if d.activeID == id {
		d.activeID = next
	}
	d.mu.Unlock()

	if d.app != nil {
		d.app.Event.Emit("agent:session_switched", map[string]any{
			"previousId": id,
			"sessionId":  next,
		})
	}
	return next
}
