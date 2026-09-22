package main

import (
	"context"
	"slices"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// Deferred pushes: what a settings save does about a session whose turn is in flight.
//
// A push swaps things a RUNNING turn reads without a lock — the hook dispatcher, the memory
// backend's pointer, the subagent runner, the resolved provider. The window is narrow and
// pointer-sized, but it is a real data race (`go test -race` sees it), and the reminder setter
// that is swapped the same way had to be given a lock for exactly this reason.
//
// Rather than lock every field a turn reads, the save DEFERS: SaveSettings marks its push step
// (`pushing`), the fan-out queues a push for any session that is mid-turn instead of applying
// it, and `endTurn` — where the turn goroutine is done and no turn of that session can be in
// flight — runs the queue.
//
// What is NOT deferred: the process-wide and shared-layer rows (the log level, the prompt-cache
// clear, the MCP manager's own limits, a profile switch, the tool-list poller). None of them
// swaps a field a turn reads unlocked — the manager and the tool registry guard themselves — and
// their target is the shared layer, whose "session" is the one being displayed, not the one
// running.
//
// A queued push is neither a failure nor a lost change: the value is already in config.yaml (the
// write happens before any push), so the worst case for a process that dies before the queue
// drains is that the change takes effect at the next start.

// deferredPush is one push waiting for its session's turn to end: the row's own per-session work,
// exactly the closure the fan-out would have run.
type deferredPush struct {
	fn func(liveRun) error
}

// deferPush queues fn for the session's turn end, and records the session as deferred for the
// summary of the save that is running.
func (d *desktopApp) deferPush(id string, fn func(liveRun) error) {
	d.pushMu.Lock()
	defer d.pushMu.Unlock()
	if d.pendingPushes == nil {
		d.pendingPushes = make(map[string][]deferredPush)
	}
	d.pendingPushes[id] = append(d.pendingPushes[id], deferredPush{fn: fn})
	if !slices.Contains(d.deferredThisSave, id) {
		d.deferredThisSave = append(d.deferredThisSave, id)
	}
}

// flushDeferredPushes runs the pushes queued for a session whose turn has just ended. `to` is the
// session the conversation is in NOW: a compaction can move it mid-turn, and the queue was keyed
// by the id in force when the user saved, so both are drained.
//
// It must NOT be called while holding d.mu: the queued closures call back into the app (the
// fan-out takes d.mu itself). A queued push whose session is gone is dropped — the file already
// has the value, and the next start reads it.
func (d *desktopApp) flushDeferredPushes(from, to string) {
	d.pushMu.Lock()
	jobs := d.pendingPushes[from]
	delete(d.pendingPushes, from)
	if to != from {
		jobs = append(jobs, d.pendingPushes[to]...)
		delete(d.pendingPushes, to)
	}
	d.pushMu.Unlock()
	if len(jobs) == 0 {
		return
	}

	// The agent is re-fetched rather than captured at save time: a rebuilt or closed agent must
	// not be pushed to, and the run's agent IS the one a new turn would use.
	r := d.runOf(to)
	if r == nil || r.agent == nil {
		return
	}
	d.mu.Lock()
	running := r.running
	d.mu.Unlock()
	if running {
		// A new turn started before we got here (the composer drains its queue on agent:idle,
		// which is emitted a moment later). Keep the queue and let THAT turn's end flush it —
		// applying now would be exactly the race this queue exists to avoid.
		d.pushMu.Lock()
		d.pendingPushes[to] = append(jobs, d.pendingPushes[to]...)
		d.pushMu.Unlock()
		return
	}

	run := liveRun{ID: to, Agent: r.agent}
	for _, job := range jobs {
		if err := job.fn(run); err != nil {
			logger.New("desktop").Warn(context.Background(),
				"Settings: deferred push failed after the turn ended", "session", to, "err", err)
		}
	}
}
