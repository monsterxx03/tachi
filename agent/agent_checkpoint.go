package agent

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/monsterxx03/tachi/agent/checkpoint"
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

// turnBoundary is where the conversation stood when a turn started: the request
// Seq the turn's numbering continues from, and the two record counts a checkpoint
// stores as its cut point.
type turnBoundary struct {
	Seq        int
	Records    int
	APIRecords int
	// Known is false when either history file could not be read, which makes the
	// counts unusable as a CUT POINT — and unusable means DANGEROUS, not merely
	// imprecise: LoadMessages returns nothing at all for a single unreadable line
	// (a crash mid-append leaves a torn one), so Records would be 0 and a rewind
	// to such a turn would move the WHOLE conversation into a sidecar. A turn with
	// Known=false records no checkpoint: failing closed costs one turn of
	// rewindability, failing open costs the session.
	Known bool
}

// beginCheckpointTurn records the turn's boundary and stores its number on the
// run. b comes from the boundary scan taken before this turn's user message was
// appended, so a rewind to this turn removes that message and hands it back to
// the reader.
//
// A failure is logged, not fatal: the turn still runs, with no file snapshot of
// its own (rs.CheckpointTurn stays 0), and a rewind to it reports that nothing
// was snapshotted rather than pretending otherwise. An UNREADABLE boundary is
// treated the same way for the same reason — see turnBoundary.Known.
func (a *AIAgent) beginCheckpointTurn(ctx context.Context, rs *RunState, userMessage string, b turnBoundary) {
	// One-off runs never write the main session, so they get no checkpoints:
	// their file changes are the caller's to manage, and a rewind of the main
	// conversation must not depend on them.
	if rs == nil || rs.SkipSessionWrites {
		return
	}
	if !b.Known {
		a.Config.Logger.Warn(ctx, "Agent: checkpoint skipped, session history unreadable",
			"messages_records", b.Records, "api_records", b.APIRecords)
		return
	}
	m := a.checkpointManager(ctx)
	if m == nil {
		return
	}
	turn, err := m.Begin(ctx, checkpoint.Boundary{
		Records:    b.Records,
		APIRecords: b.APIRecords,
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
	roots := a.checkpointRoots(sess)
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

// checkpointRoots is the root set a turn's snapshot covers: the SESSION's own
// working directory plus whatever extra roots it carries.
//
// It deliberately does not read the working directory out of the context.
// wdctx.Dir falls back to the process's CWD when the context carries none, and a
// GUI app's CWD is "/": a snapshot manager built from that runs
// `git add -A --work-tree=/`, which walks the entire filesystem and never
// returns (measured — it pinned a rewind behind a child process that could not
// finish). The session's WorkingDir is the authoritative root anyway: it is what
// /cd updates and what survives a reload.
func (a *AIAgent) checkpointRoots(sess *session.Session) []string {
	if sess == nil {
		return nil
	}
	roots := make([]string, 0, 1+len(sess.AdditionalDirs))
	roots = append(roots, sess.WorkingDir)
	return append(roots, sess.AdditionalDirs...)
}

// sessionBoundary returns where the session's recorded history currently ends:
// the highest request Seq (what the next turn's numbering continues from) and
// the number of records in messages.jsonl and api_requests.jsonl (which a
// checkpoint stores as its cut point).
//
// Derived from disk, like Seq itself, so a process restart renumbers nothing.
// A read failure leaves Known false, which the caller treats as "no cut point"
// rather than as 0 — see turnBoundary.Known. The Seq stays best-effort: it only
// numbers requests, and a file that read fine still tells us where to continue.
func (a *AIAgent) sessionBoundary() turnBoundary {
	var b turnBoundary
	sm := a.Config.SessionManager
	if sm == nil {
		return b
	}
	cur := sm.Current()
	if cur == nil {
		return b
	}
	msgs, msgErr := sm.LoadMessages()
	if msgErr != nil {
		a.Config.Logger.Warn(context.Background(),
			"Agent: sessionBoundary: messages unreadable, this turn gets no checkpoint", msgErr)
	} else {
		b.Records = len(msgs)
		for i := range msgs {
			if msgs[i].Seq > b.Seq {
				b.Seq = msgs[i].Seq
			}
		}
	}
	reqs, reqErr := sm.LoadAPIRequests(cur.ID)
	if reqErr != nil {
		a.Config.Logger.Warn(context.Background(),
			"Agent: sessionBoundary: request log unreadable, this turn gets no checkpoint", reqErr)
	} else {
		b.APIRecords = len(reqs)
		for i := range reqs {
			if reqs[i].Seq > b.Seq {
				b.Seq = reqs[i].Seq
			}
		}
	}
	b.Known = msgErr == nil && reqErr == nil
	return b
}
