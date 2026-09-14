package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	agent_checkpoint "github.com/monsterxx03/tachi/agent/checkpoint"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boundary is the turn boundary the loop hands to beginCheckpointTurn: explicit
// counts, and Known set the way the loop sets it when both history files read.
func boundary(records, apiRecords int) turnBoundary {
	return turnBoundary{Records: records, APIRecords: apiRecords, Known: true}
}

// rewindTestAgent is checkpointTestAgent plus some records already in the
// session, so a truncation has something to cut.
func rewindTestAgent(t *testing.T, records int) (*AIAgent, string, context.Context, *fakeSessionManager) {
	t.Helper()
	a, work, ctx := checkpointTestAgent(t)
	fake := a.Config.SessionManager.(*fakeSessionManager)
	for i := 0; i < records; i++ {
		require.NoError(t, fake.AppendMessage(&session.Message{Type: session.MessageTypeUser, Content: "旧记录"}))
	}
	return a, work, ctx, fake
}

// turnRun returns the RunState the loop would hand to the tool executor for a
// checkpointed turn.
func turnRun(turn int) *RunState {
	rs := &RunState{}
	rs.setCheckpointTurn(turn)
	return rs
}

// TestRewindRestoresFilesAndCutsTheConversation is the whole feature in one
// test: the files go back, the conversation goes back to the same point (its
// user message removed, handed back for editing), the discarded tail is kept in
// sidecars, and the state that described the old history is dropped.
func TestRewindRestoresFilesAndCutsTheConversation(t *testing.T) {
	a, work, ctx, fake := rewindTestAgent(t, 4)

	// A file that exists before the turn, so the rewind has a MODIFICATION to
	// undo as well as a creation.
	require.NoError(t, os.WriteFile(filepath.Join(work, "keep.txt"), []byte("original"), 0o644))

	// Turn 1: the boundary is at record 2, and the snapshot is taken before
	// anything writes — so it records the tree as it stands before the work.
	rs := &RunState{}
	a.beginCheckpointTurn(ctx, rs, "把导出改成流式", boundary(2, 1))
	require.NoError(t, a.snapshotBeforeWrite(ctx, rs, "WriteFile"))

	// The turn works: it edits that file and creates another one.
	require.NoError(t, os.WriteFile(filepath.Join(work, "keep.txt"), []byte("modified"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(work, "new.txt"), []byte("created"), 0o644))
	for i := 0; i < 3; i++ {
		require.NoError(t, fake.AppendMessage(&session.Message{Type: session.MessageTypeAssistant, Content: "后来的记录"}))
	}

	// Turn 2: its snapshot captures the state as it stands now, which is what
	// makes a rewind to turn 1 a real change rather than a no-op.
	a.beginCheckpointTurn(ctx, &RunState{}, "再改一处", boundary(5, 2))

	// Two recorded requests, so the cut has something to cut there too.
	require.NoError(t, fake.AppendAPIRequest(&session.APIRequest{Seq: 1, SystemPrompt: "sys"}))
	require.NoError(t, fake.AppendAPIRequest(&session.APIRequest{Seq: 2, SystemPrompt: "sys"}))

	// A live anchor, of the shape a completed call would leave: the rewind must
	// drop it, because it is the real prompt size of a call whose prompt is
	// about to lose messages.
	a.conv.setPromptAnchor(9999, 1)

	p, err := a.PreviewRewind(ctx, 1)
	require.NoError(t, err)
	assert.Empty(t, p.Blocked)
	assert.Equal(t, "把导出改成流式", p.UserText)
	assert.Equal(t, 2, p.Records, "the cut point is where turn 1's user message sat")
	require.Len(t, p.Roots, 1)
	assert.Equal(t, []string{"new.txt"}, p.Roots[0].Added)
	assert.Equal(t, []string{"keep.txt"}, p.Roots[0].Changed)

	res, err := a.Rewind(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, "original", readWorkFile(t, work, "keep.txt"))
	assert.False(t, workFileExists(work, "new.txt"), "a file created after the turn must be removed")
	// The cut the rewind chose: the conversation goes back to turn 1's boundary,
	// the request log with it (the fake keeps no files — the sidecars a real
	// session writes are covered in the session package).
	require.Len(t, fake.truncations, 1)
	assert.Equal(t, 2, fake.truncations[0].KeepMessages)
	assert.Equal(t, 1, fake.truncations[0].KeepAPI)
	assert.Contains(t, fake.truncations[0].Tag, "turn-1")

	// The conversation came back with it: two records left, and turn 1's user
	// message is OUT of it so the caller can hand it back for editing.
	msgs, err := fake.LoadMessages()
	require.NoError(t, err)
	assert.Len(t, msgs, 2)
	reqs, err := fake.LoadAPIRequests("")
	require.NoError(t, err)
	assert.Len(t, reqs, 1, "the request log is cut with the conversation")

	// The history a frontend adopts is the rebuilt one, not the pre-rewind one.
	assert.Len(t, res.History, 2)

	// And the state that described the discarded history is gone: the anchor is
	// dropped (it belonged to a call whose prompt lost messages) and the estimate
	// is recomputed for what remains.
	assert.Zero(t, a.conv.lastPromptReal, "the anchor must not outlive the history it measured")
	assert.Zero(t, a.conv.lastPromptEstimate)
	assert.Zero(t, a.conv.compactEstimate, "the compaction baseline may refer to a discarded compaction")
	assert.Greater(t, a.conv.tokens(), int64(0), "the remaining history still has a size")
}

// TestRewindRefusesWhileATurnIsRunning pins the one race that cannot be made
// safe: truncating the conversation under a live turn.
func TestRewindRefusesWhileATurnIsRunning(t *testing.T) {
	a, _, ctx, _ := rewindTestAgent(t, 2)
	a.beginCheckpointTurn(ctx, &RunState{}, "第一轮", boundary(1, 0))

	live := &RunState{}
	a.mu.Lock()
	a.currentRun = live
	a.mu.Unlock()

	_, err := a.Rewind(ctx, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "正在运行")

	live.markFinished()
	_, err = a.Rewind(ctx, 1)
	assert.NoError(t, err, "a finished turn must not block a rewind")
}

// TestRewindWithAnUnknownFileStateSaysSoInsteadOfBlocking: a turn whose file state
// was never recorded (a guard refused the snapshot) cannot have its files
// restored — but its CONVERSATION can still be rewound, and that is the half the
// reader asked for. Blocking the whole rewind would be a silent loss of a
// working feature (and an unreadable card, since the reason is a Go-internal
// string); the contract is "rewind, and say plainly that no file was restored",
// which is what the design and Claude Code's "No files were restored" both do.
func TestRewindWithAnUnknownFileStateSaysSoInsteadOfBlocking(t *testing.T) {
	a, work, ctx, fake := rewindTestAgent(t, 1)
	// The byte guard refuses this turn's snapshot, so its file state is unknown.
	a.Config.FullConfig.Checkpoints.MaxBytes = 4
	require.NoError(t, os.WriteFile(filepath.Join(work, "big.bin"), []byte("bigger than four bytes"), 0o644))
	a.beginCheckpointTurn(ctx, &RunState{}, "改大文件", boundary(1, 0))
	require.NoError(t, a.snapshotBeforeWrite(ctx, turnRun(1), "WriteFile"))

	p, err := a.PreviewRewind(ctx, 1)
	require.NoError(t, err)
	assert.Empty(t, p.Blocked, "an unrecorded file state must not block the rewind")
	assert.NotEmpty(t, p.NoFiles, "…but it has to be said out loud")
	assert.Contains(t, p.NoFiles, "超过单文件上限", "the reason is written for the reader, not for a log")
	assert.False(t, p.FilesUnchanged, "unknown is not the same as 'nothing to do'")

	res, err := a.Rewind(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, p.NoFiles, res.Preview.NoFiles)
	assert.Equal(t, "bigger than four bytes", readWorkFile(t, work, "big.bin"), "no file may be restored")
	// The conversation did move: the cut point is the turn's own boundary.
	require.Len(t, fake.truncations, 1)
	assert.Equal(t, 1, fake.truncations[0].KeepMessages)
	assert.NotNil(t, res.History)
}

// TestRewindOfAReadOnlyTurnNeedsNoFiles covers both shapes of "there is nothing to
// restore": a turn that wrote nothing because a LATER turn wrote (so the state at
// its start is that later turn's starting state), and a session that never wrote
// at all. Both are a fact about the workspace, not a refused snapshot — and
// treating them as "unknown" locked every read-only session out of rewind
// entirely, which is the common case ("that answer was wrong, let me ask again").
func TestRewindOfAReadOnlyTurnNeedsNoFiles(t *testing.T) {
	t.Run("整段会话只读", func(t *testing.T) {
		a, _, ctx, fake := rewindTestAgent(t, 2)
		a.beginCheckpointTurn(ctx, &RunState{}, "第一轮", boundary(0, 0))
		a.beginCheckpointTurn(ctx, &RunState{}, "第二轮", boundary(2, 0))

		p, err := a.PreviewRewind(ctx, 1)
		require.NoError(t, err)
		assert.Empty(t, p.Blocked)
		assert.Empty(t, p.NoFiles)
		assert.True(t, p.FilesUnchanged, "nothing ever wrote, so the workspace already is the state to return to")
		assert.Equal(t, 0, p.Records, "the cut point is where turn 1 began: the very start of the session")

		res, err := a.Rewind(ctx, 1)
		require.NoError(t, err)
		require.Len(t, fake.truncations, 1, "the conversation must still go back")
		assert.Equal(t, 0, fake.truncations[0].KeepMessages)
		assert.Len(t, res.History, 0)
	})

	t.Run("只读的末尾一轮", func(t *testing.T) {
		a, work, ctx, fake := rewindTestAgent(t, 1)
		// Turn 1 writes, turn 2 only reads — so turn 2's starting state is the state
		// that turn 1 changed the workspace INTO, and nothing wrote after it either.
		a.beginCheckpointTurn(ctx, &RunState{}, "第一轮", boundary(1, 0))
		require.NoError(t, a.snapshotBeforeWrite(ctx, turnRun(1), "WriteFile"))
		require.NoError(t, os.WriteFile(filepath.Join(work, "keep.txt"), []byte("changed"), 0o644))
		fake.AppendMessage(&session.Message{Type: session.MessageTypeAssistant, Content: "答"})
		a.beginCheckpointTurn(ctx, &RunState{}, "第二轮", boundary(2, 0))

		p, err := a.PreviewRewind(ctx, 2)
		require.NoError(t, err)
		assert.Empty(t, p.Blocked)
		assert.True(t, p.FilesUnchanged, "no turn at or after this one wrote, so there is nothing to undo")

		_, err = a.Rewind(ctx, 2)
		require.NoError(t, err)
		require.Len(t, fake.truncations, 1)
		assert.Equal(t, 2, fake.truncations[0].KeepMessages)
		// The workspace is left exactly as it stands — that IS the correct answer to
		// "back to the start of turn 2", which began after this write.
		assert.Equal(t, "changed", readWorkFile(t, work, "keep.txt"))
	})
}

// TestRewindDropsTheCheckpointsOfTheTurnsItUndid: after a rewind, every later
// checkpoint indexes a conversation that no longer exists — its cut point sits
// past the new end of messages.jsonl, and its ref holds the branch that was just
// abandoned. Leaving them lets a second rewind into one of them report success
// while the conversation cut is a no-op AND the files jump to the discarded
// branch's point in time (measured exactly that, before this existed).
func TestRewindDropsTheCheckpointsOfTheTurnsItUndid(t *testing.T) {
	a, work, ctx, fake := rewindTestAgent(t, 0)

	// Turn 1 writes a file; turn 2 writes another one, so returning to turn 1 is a
	// real change and turn 2's snapshot is a real abandoned state.
	a.beginCheckpointTurn(ctx, &RunState{}, "第一轮", boundary(0, 0))
	require.NoError(t, a.snapshotBeforeWrite(ctx, turnRun(1), "WriteFile"))
	require.NoError(t, os.WriteFile(filepath.Join(work, "a.txt"), []byte("1"), 0o644))
	for i := 0; i < 3; i++ {
		fake.AppendMessage(&session.Message{Type: session.MessageTypeAssistant, Content: "一"})
	}
	a.beginCheckpointTurn(ctx, &RunState{}, "第二轮", boundary(3, 0))
	require.NoError(t, a.snapshotBeforeWrite(ctx, turnRun(2), "WriteFile"))
	require.NoError(t, os.WriteFile(filepath.Join(work, "b.txt"), []byte("2"), 0o644))
	for i := 0; i < 3; i++ {
		fake.AppendMessage(&session.Message{Type: session.MessageTypeAssistant, Content: "二"})
	}

	_, err := a.Rewind(ctx, 1)
	require.NoError(t, err)
	assert.False(t, workFileExists(work, "b.txt"), "the abandoned turn's file is gone with it")

	turns, err := a.RewindTurns(ctx)
	require.NoError(t, err)
	require.Len(t, turns, 1, "the turns the rewind undid must not stay in the index")
	assert.Equal(t, 1, turns[0].Turn)
	assert.Equal(t, 0, turns[0].Records)

	// Turn 2 is gone as a target: not a rewind that reports success while the
	// conversation stays put.
	p, err := a.PreviewRewind(ctx, 2)
	require.NoError(t, err)
	assert.NotEmpty(t, p.Blocked)
	assert.Zero(t, p.Records, "a refused preview must not carry a cut point the caller could apply")

	// And the numbering continues from the cut, so the next turn is 2 again rather
	// than a number nobody can see the gap before.
	next := &RunState{}
	a.beginCheckpointTurn(ctx, next, "再来一轮", boundary(2, 0))
	assert.Equal(t, 2, next.CheckpointTurn())
}

// TestUnreadableHistoryRecordsNoCheckpoint: the boundary a checkpoint stores is a
// CUT POINT into messages.jsonl, so a scan that failed must not be recorded as
// one — and failing OPEN is not a missing checkpoint, it is a destructive one.
// LoadMessages returns nothing at all for a single unreadable line (a crash
// mid-append leaves a torn one), which made the count 0; a rewind to such a turn
// then moved the WHOLE conversation into a sidecar while reporting a normal
// rewind. Failing closed costs that one turn's rewindability.
func TestUnreadableHistoryRecordsNoCheckpoint(t *testing.T) {
	a, _, ctx, fake := rewindTestAgent(t, 3)
	fake.loadErr = errors.New("messages.jsonl: unexpected end of JSON input")

	b := a.sessionBoundary()
	require.False(t, b.Known, "an unreadable history is not a usable cut point")
	assert.Zero(t, b.Records, "…and the count it produced must not be taken as one")

	rs := &RunState{}
	a.beginCheckpointTurn(ctx, rs, "这一轮", b)
	assert.Zero(t, rs.CheckpointTurn(), "no checkpoint may be recorded from an unknown boundary")
	assert.Zero(t, a.snapshotBeforeWrite(ctx, rs, "WriteFile"), "and no file snapshot is taken for a turn without one")

	turns, err := a.RewindTurns(ctx)
	require.NoError(t, err)
	assert.Empty(t, turns)

	// The turn is refused rather than applied with a cut point of 0 — which is
	// what "no checkpoint" means everywhere else in this feature.
	p, err := a.PreviewRewind(ctx, 1)
	require.NoError(t, err)
	assert.NotEmpty(t, p.Blocked)
	assert.Zero(t, p.Records)
}

// TestRewindWithCheckpointsOffSaysSo: with the feature off there is nothing to
// go back to, and the caller needs a reason rather than an empty result.
func TestRewindWithCheckpointsOffSaysSo(t *testing.T) {
	a, _, ctx, _ := rewindTestAgent(t, 1)
	off := false
	a.Config.FullConfig.Checkpoints.Enabled = &off

	_, err := a.Rewind(ctx, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "检查点")
}

// TestRewindTurnsListsTheBoundariesForAPicker keeps the UI's input honest: the
// list carries what a picker needs (the prompt text, the cut point, and whether
// the turn even has file state).
func TestRewindTurnsListsTheBoundariesForAPicker(t *testing.T) {
	a, _, ctx, _ := rewindTestAgent(t, 2)
	a.beginCheckpointTurn(ctx, &RunState{}, "第一轮", boundary(1, 0))
	a.beginCheckpointTurn(ctx, &RunState{}, "第二轮", boundary(2, 0))

	turns, err := a.RewindTurns(ctx)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	assert.Equal(t, 1, turns[0].Turn)
	assert.Equal(t, "第一轮", turns[0].UserText)
	assert.Equal(t, 1, turns[0].Records)
	assert.True(t, turns[0].NoFiles, "a turn that only read has no file state of its own")
	assert.Equal(t, 2, turns[1].Turn)
}

// TestCheckpointConfigDefaultsMatchThePackage keeps the two places that define
// the limits from drifting apart: the yaml defaults a user reads and the
// fallbacks the package uses when Options is zero.
func TestCheckpointConfigDefaultsMatchThePackage(t *testing.T) {
	cfg := config.DefaultConfig()
	assert.Equal(t, agent_checkpoint.DefaultMaxFiles, cfg.Checkpoints.MaxFiles)
	assert.Equal(t, agent_checkpoint.DefaultMaxBytes, cfg.Checkpoints.MaxBytes)
	assert.Equal(t, agent_checkpoint.DefaultRetain, cfg.Checkpoints.Retain)
	require.NotNil(t, cfg.Checkpoints.Enabled)
	assert.True(t, *cfg.Checkpoints.Enabled, "a loaded config has checkpoints on")
}

func readWorkFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	require.NoError(t, err)
	return string(data)
}

func workFileExists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

// TestCheckpointRootsComeFromTheSessionNotTheAmbientCWD pins the trap that cost a
// debugging session. wdctx.Dir falls back to the process CWD when the context has
// none, and a GUI app's CWD is "/": a snapshot manager built from that runs
// `git add -A --work-tree=/`, which walks the whole filesystem and never returns
// (measured: it pinned the rewind behind a child process that could not finish).
func TestCheckpointRootsComeFromTheSessionNotTheAmbientCWD(t *testing.T) {
	a, work, _ := checkpointTestAgent(t)

	sess := &session.Session{WorkingDir: work, AdditionalDirs: []string{"/extra"}}
	assert.Equal(t, []string{work, "/extra"}, a.checkpointRoots(sess))
	assert.Nil(t, a.checkpointRoots(nil))
}

// TestRewindRefusesASessionCompactedOnwards is the decision 「压缩之后禁止跨边界回退」, and the
// shape of the crossing is worth spelling out: compaction does not rewrite the session it
// compacts — it starts a NEW one that continues from a summary (agent/compact.go) — so this
// session's checkpoints describe the state BEFORE that point while the conversation the reader
// is living in is the successor. A rewind here moves the workspace backwards under a successor
// that has kept writing since, and the two sessions end up describing inconsistent trees
// without either knowing.
//
// It is refused by name — the successor is pointed at — and it is refused by the PREVIEW too,
// so the confirmation card is never built around a file list nobody can act on.
func TestRewindRefusesASessionCompactedOnwards(t *testing.T) {
	a, work, ctx, fake := rewindTestAgent(t, 0)

	// A turn with a real checkpoint: the only thing that may block this rewind is the boundary.
	rs := &RunState{}
	a.beginCheckpointTurn(ctx, rs, "改一版", boundary(0, 0))
	require.NoError(t, a.snapshotBeforeWrite(ctx, rs, "Bash"))
	require.NoError(t, os.WriteFile(filepath.Join(work, "out.txt"), []byte("written\n"), 0o644))
	turn := rs.CheckpointTurn()
	require.NotZero(t, turn)

	// Control: the conversation has not moved on, so the rewind is offered as usual.
	p, err := a.PreviewRewind(ctx, turn)
	require.NoError(t, err)
	assert.Empty(t, p.Blocked, "an un-compacted session must still be rewindable")

	me := fake.Current()
	child := &session.Session{ID: "child-1", Title: "同一段对话", CompactedParentID: me.ID}
	fake.others = []*session.Session{child}
	me.CompactedChildID = child.ID

	p, err = a.PreviewRewind(ctx, turn)
	require.NoError(t, err)
	assert.Contains(t, p.Blocked, "压缩")
	assert.Contains(t, p.Blocked, "同一段对话", "the refusal names the successor, not just its id")
	assert.Empty(t, p.Roots, "a blocked preview must not offer a file list to confirm")

	_, err = a.Rewind(ctx, turn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "压缩")

	// Nothing moved: the file the turn wrote is still there and the conversation is intact.
	assert.True(t, workFileExists(work, "out.txt"))

	// The predecessor's side of the link is written BEST-EFFORT (compact.go logs and ignores a
	// failed UpdateMeta — the successor's own parent link is enough for the sidebar), so the
	// successor has to be found even when this session does not know about it.
	me.CompactedChildID = ""
	p, err = a.PreviewRewind(ctx, turn)
	require.NoError(t, err)
	assert.Contains(t, p.Blocked, "压缩",
		"a successor that only points BACK at us is still a boundary we must not cross")
}
