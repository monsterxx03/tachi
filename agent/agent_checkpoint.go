package agent

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/monsterxx03/tachi/agent/checkpoint"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
)

// Session checkpoints: a per-turn file snapshot a rewind can restore, together
// with the conversation. Design:
// docs/2026-09-14-session-checkpoint-rewind-design.md.
//
// Two call sites make it work, and their ORDER is the whole design:
//
//   - beginCheckpointTurn at the turn start records where the conversation
//     stood. Cheap: a manifest entry, no filesystem work.
//   - snapshotBeforeWrite right before a tool that could write takes the file
//     half. Lazy so a turn that only reads costs nothing, idempotent so the
//     second write in a turn is a lookup — and always BEFORE the write, which
//     is what makes the recorded state the turn's start rather than its middle.

// beginCheckpointTurn records the turn's boundary and stores its number on the
// run. records/apiRecords come from the boundary scan taken before this turn's
// user message was appended, so a rewind to this turn removes that message and
// hands it back to the reader.
//
// A failure is logged, not fatal: the turn still runs, with no file snapshot of
// its own (rs.CheckpointTurn stays 0), and a rewind to it reports that nothing
// was snapshotted rather than pretending otherwise.
func (a *AIAgent) beginCheckpointTurn(ctx context.Context, rs *RunState, userMessage string, records, apiRecords int) {
	// One-off runs never write the main session, so they get no checkpoints:
	// their file changes are the caller's to manage, and a rewind of the main
	// conversation must not depend on them.
	if rs == nil || rs.SkipSessionWrites {
		return
	}
	m := a.checkpointManager(ctx)
	if m == nil {
		return
	}
	turn, err := m.Begin(ctx, checkpoint.Boundary{
		Records:    records,
		APIRecords: apiRecords,
		UserText:   userMessage,
	})
	if err != nil {
		a.Config.Logger.Warn(ctx, "Agent: checkpoint begin failed", "err", err)
		return
	}
	rs.setCheckpointTurn(turn)
}

// snapshotBeforeWrite takes the file half of the current turn's checkpoint when
// a write-capable tool is about to run. Callers invoke it before EVERY such tool
// call; after the first it is a manifest lookup.
//
// The error is the caller's to act on by REFUSING the write: a change that
// happened without a checkpoint is a change a rewind cannot take back, and
// letting it through would make the rewind report a success it cannot deliver.
func (a *AIAgent) snapshotBeforeWrite(ctx context.Context, rs *RunState, toolName string) error {
	if rs == nil || rs.SkipSessionWrites || !checkpoint.CouldWrite(toolName) {
		return nil
	}
	turn := rs.CheckpointTurn()
	if turn == 0 {
		return nil
	}
	m := a.checkpointManager(ctx)
	if m == nil {
		return nil
	}
	return m.Snapshot(ctx, turn)
}

// checkpointsEnabled reports whether this agent checkpoints at all. A nil
// Enabled means off — see config.CheckpointConfig.Enabled for why that is the
// opposite of the Compact.Auto idiom.
func (a *AIAgent) checkpointsEnabled() bool {
	cfg := a.Config.FullConfig
	return cfg != nil && cfg.Checkpoints.Enabled != nil && *cfg.Checkpoints.Enabled
}

// checkpointManager returns the manager for the current session, building it on
// first use and rebuilding it when the session or its root set changed. It
// returns nil when checkpoints are off or there is no session to bind to.
//
// The roots are resolved per turn rather than once: the working directory can
// move (`/cd`) and a session can gain roots, and a checkpoint recorded against
// the old root set would restore the wrong directories.
func (a *AIAgent) checkpointManager(ctx context.Context) *checkpoint.Manager {
	if !a.checkpointsEnabled() {
		return nil
	}
	cfg := a.Config.FullConfig
	sm := a.Config.SessionManager
	if sm == nil || !sm.HasCurrent() {
		return nil // no session to bind a checkpoint store to
	}
	sess := sm.Current()
	roots := a.checkpointRoots(ctx, sess)
	key := sess.ID + "\x00" + strings.Join(roots, "\x00")

	a.ckptMu.Lock()
	defer a.ckptMu.Unlock()
	if a.ckpt != nil && a.ckptKey == key {
		return a.ckpt
	}
	sessionDirRoot, err := config.SessionDir()
	if err != nil {
		a.Config.Logger.Warn(ctx, "Agent: checkpoint disabled, session dir unavailable", "err", err)
		return nil
	}
	a.ckpt = checkpoint.NewManager(filepath.Join(sessionDirRoot, sess.ID), roots, checkpoint.Options{
		MaxFiles: cfg.Checkpoints.MaxFiles,
		MaxBytes: cfg.Checkpoints.MaxBytes,
		Retain:   cfg.Checkpoints.Retain,
		Logger:   a.Config.Logger.With("component", "checkpoint"),
	})
	a.ckptKey = key
	return a.ckpt
}

// checkpointRoots is the root set a turn's snapshot covers: the turn's working
// directory plus whatever extra roots the session carries (session.AdditionalDirs).
func (a *AIAgent) checkpointRoots(ctx context.Context, sess *session.Session) []string {
	roots := []string{wdctx.Dir(ctx)}
	if sess != nil {
		roots = append(roots, sess.AdditionalDirs...)
	}
	return roots
}

// sessionBoundary returns where the session's recorded history currently ends:
// the highest request Seq (what the next turn's numbering continues from) and
// the number of records in messages.jsonl and api_requests.jsonl (which a
// checkpoint stores as its cut point).
//
// Derived from disk, like Seq itself, so a process restart renumbers nothing.
// Best-effort: a read failure leaves that part at 0, which the callers treat as
// "unknown" rather than as a real boundary.
func (a *AIAgent) sessionBoundary() (seq, records, apiRecords int) {
	sm := a.Config.SessionManager
	if sm == nil {
		return 0, 0, 0
	}
	cur := sm.Current()
	if cur == nil {
		return 0, 0, 0
	}
	if msgs, err := sm.LoadMessages(); err == nil {
		records = len(msgs)
		for i := range msgs {
			if msgs[i].Seq > seq {
				seq = msgs[i].Seq
			}
		}
	} else {
		a.Config.Logger.Warn(context.Background(), "Agent: sessionBoundary: load messages failed", err)
	}
	if reqs, err := sm.LoadAPIRequests(cur.ID); err == nil {
		apiRecords = len(reqs)
		for i := range reqs {
			if reqs[i].Seq > seq {
				seq = reqs[i].Seq
			}
		}
	} else {
		a.Config.Logger.Warn(context.Background(), "Agent: sessionBoundary: load api requests failed", err)
	}
	return seq, records, apiRecords
}
