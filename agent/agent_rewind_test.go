package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agent_checkpoint "github.com/monsterxx03/tachi/agent/checkpoint"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	a.beginCheckpointTurn(ctx, rs, "把导出改成流式", 2, 1)
	require.NoError(t, a.snapshotBeforeWrite(ctx, rs, "WriteFile"))

	// The turn works: it edits that file and creates another one.
	require.NoError(t, os.WriteFile(filepath.Join(work, "keep.txt"), []byte("modified"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(work, "new.txt"), []byte("created"), 0o644))
	for i := 0; i < 3; i++ {
		require.NoError(t, fake.AppendMessage(&session.Message{Type: session.MessageTypeAssistant, Content: "后来的记录"}))
	}

	// Turn 2: its snapshot captures the state as it stands now, which is what
	// makes a rewind to turn 1 a real change rather than a no-op.
	a.beginCheckpointTurn(ctx, &RunState{}, "再改一处", 5, 2)

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
	a.beginCheckpointTurn(ctx, &RunState{}, "第一轮", 1, 0)

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

// TestRewindReportsABlockedTargetInsteadOfFailing: a turn whose file state is
// unknown (a guard refused the snapshot) is not an error to retry, it is an
// answer to show — and nothing may be touched.
func TestRewindReportsABlockedTargetInsteadOfFailing(t *testing.T) {
	a, work, ctx, _ := rewindTestAgent(t, 1)
	// The byte guard refuses this turn's snapshot, so its file state is unknown.
	a.Config.FullConfig.Checkpoints.MaxBytes = 4
	require.NoError(t, os.WriteFile(filepath.Join(work, "big.bin"), []byte("bigger than four bytes"), 0o644))
	a.beginCheckpointTurn(ctx, &RunState{}, "改大文件", 1, 0)
	require.NoError(t, a.snapshotBeforeWrite(ctx, turnRun(1), "WriteFile"))

	p, err := a.PreviewRewind(ctx, 1)
	require.NoError(t, err)
	assert.NotEmpty(t, p.Blocked, "a skipped snapshot must block the rewind, not be silently skipped")

	res, err := a.Rewind(ctx, 1)
	require.NoError(t, err, "a blocked rewind is an answer, not a failure")
	assert.Equal(t, p.Blocked, res.Preview.Blocked)
	assert.Nil(t, res.History)
	assert.Equal(t, "bigger than four bytes", readWorkFile(t, work, "big.bin"), "nothing may be touched")
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
	a.beginCheckpointTurn(ctx, &RunState{}, "第一轮", 1, 0)
	a.beginCheckpointTurn(ctx, &RunState{}, "第二轮", 2, 0)

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
