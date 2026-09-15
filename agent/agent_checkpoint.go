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

// rebindCheckpointTurn starts a fresh checkpoint turn for the session the run is on NOW, after
// a compaction moved the conversation into a new one.
//
// The binding is session-scoped twice over — the turn's number lives in the OLD session's
// manifest, and the manager resolves by session id — so after the swap every write-capable
// tool call for the REST of the turn is refused ("turn N was never begun": the new session's
// manifest has no such turn). That refusal is the reported failure, and it lasts until the
// turn ends, because only the next turn begins a new one.
//
// The boundary is taken FRESH rather than inherited: a checkpoint's cut point has to name a
// position in the files it will be rewound against, and the new session's records are the
// compaction's. The price is bounded and honest: the new session's first turn starts at the
// compaction, so rewinding to it does not undo what this turn wrote BEFORE the move — those
// writes belong to the parent's turns, which the parent's manifest still describes.
//
// A failure is logged, not fatal (beginCheckpointTurn's contract): the turn keeps running.
func (a *AIAgent) rebindCheckpointTurn(ctx context.Context, rs *RunState, userText string) {
	if rs == nil || rs.SkipSessionWrites {
		return
	}
	rs.setCheckpointTurn(0)
	a.beginCheckpointTurn(ctx, rs, userText, a.sessionBoundary())
}

// endCheckpointTurn records where the turn's writes LEFT the workspace (see
// checkpoint.Manager.SnapshotEnd), which is what makes the turn's changes readable
// exactly — from its own two trees rather than from what the tool calls declared, so a
// shell command's writes count too.
//
// Called on every way a turn can finish, and never fatal: the work is done and the turn is
// over, so a failure is logged and recorded as a reason for the reader, not turned into a
// failed turn.
//
// The context is DETACHED from the turn's cancellation, which is not a detail: the loop
// exits on a cancelled context precisely when the user stopped the turn (Stop, StopAndSend,
// the TUI's Ctrl+C — all cancel, they do not set a flag), and this is the cleanup that runs
// AFTER it. Running git on that dead context fails immediately, so every stopped turn would
// report "no numbers" instead of what it changed — and its end snapshot, being refused,
// pays the refused-end path for nothing. The work to record is already on disk; only the
// cancellation is meaningless here. Same reasoning as dropStates, which cleans up on a
// background context.
func (a *AIAgent) endCheckpointTurn(ctx context.Context, rs *RunState) {
	if rs == nil || rs.SkipSessionWrites {
		return
	}
	turn := rs.CheckpointTurn()
	if turn == 0 {
		return
	}
	m := a.checkpointManager(ctx)
	if m == nil {
		return
	}
	if err := m.SnapshotEnd(context.WithoutCancel(ctx), turn); err != nil {
		a.Config.Logger.Warn(ctx, "Agent: checkpoint end failed", "turn", turn, "err", err)
	}
}

// TurnSummary returns what a turn changed, counted from the checkpoint's own two trees, or
// nil when there are no numbers for it: no checkpoint, the turn wrote nothing, or its end
// state was refused. A caller that gets nil falls back to what the tool calls declared —
// which is what every reader did before the checkpoints existed, and is still the only
// answer when the feature is off.
func (a *AIAgent) TurnSummary(ctx context.Context, turn int) *checkpoint.TurnDiff {
	m := a.checkpointManager(ctx)
	if m == nil {
		return nil
	}
	rec, ok, err := m.Record(turn)
	if err != nil || !ok {
		return nil
	}
	return rec.Diff
}

// TurnDiff returns the frozen unified diff of a turn (its start tree against its end tree),
// per workspace root, and whether there is such a pair at all. Both sides are recorded, so the
// answer does not move when the working tree does — asking after a commit gives the same diff
// the turn produced. Per root because the paths inside are relative to it: a caller that
// merges them cannot tell two roots' same-named files apart.
func (a *AIAgent) TurnDiff(ctx context.Context, turn int) ([]checkpoint.RootDiff, bool, error) {
	m := a.checkpointManager(ctx)
	if m == nil {
		return nil, false, nil
	}
	return m.TurnDiff(ctx, turn)
}

// TurnDiffCommand returns a shell command that prints exactly what a turn changed, or ""
// when there is no pair of trees to compare.
//
// It is for the review fork, which runs git ITSELF rather than being handed a diff (a diff
// inlined into a prompt would have to be capped, and could not be re-read if the reviewer
// wanted a second look at one file). The fork cannot name the checkpoint's trees on its
// own — they live in the session's shadow store — so the command is spelled out for it.
func (a *AIAgent) TurnDiffCommand(ctx context.Context, turn int) string {
	m := a.checkpointManager(ctx)
	if m == nil {
		return ""
	}
	return m.TurnDiffCommand(turn)
}

// TurnChangedSince reports which paths the working tree no longer has where the turn left
// them, per root, for a diff panel that must say 「之后又改过」 rather than imply it shows the
// disk. Per root for the usual reason: the paths are relative to it.
func (a *AIAgent) TurnChangedSince(ctx context.Context, turn int) []checkpoint.RootChanged {
	m := a.checkpointManager(ctx)
	if m == nil {
		return nil
	}
	changed, err := m.ChangedSinceTurn(ctx, turn)
	if err != nil {
		a.Config.Logger.Warn(ctx, "Agent: checkpoint changed-since failed", "turn", turn, "err", err)
		return nil
	}
	return changed
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
//
// A frontend whose session RECORD is not the authority on the workspace supplies
// RootsFunc (see AgentConfig.RootsFunc) — the desktop, where a project owns the roots and
// the record is a snapshot. Resolving there is not a refinement: a checkpoint that covered
// the wrong tree would restore the wrong tree, silently and with a success message.
func (a *AIAgent) checkpointRoots(sess *session.Session) []string {
	if sess == nil {
		return nil
	}
	if resolve := a.Config.RootsFunc; resolve != nil {
		return resolve(sess)
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
