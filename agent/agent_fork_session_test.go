package agent

import (
	"os"
	"testing"

	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestForkSessionCutsAtTheTurnsBoundary is the whole agent half in one test: the
// cut a fork asks for is the boundary the CHECKPOINT recorded for the chosen turn
// — both record counts, in one call — and the conversation being forked stays
// where it is (a fork copies, it does not truncate).
//
// The numbers are the point. `Records` is where the turn's records BEGIN, which
// includes the turn's reminder wrapper: cutting at the selected message's own index
// would leave that reminder behind as a dangling block of its own, and the next
// reload would send it as a stray user message.
func TestForkSessionCutsAtTheTurnsBoundary(t *testing.T) {
	a, _, ctx, fake := rewindTestAgent(t, 4)

	// Turn 1 began at record 2 with one recorded request.
	a.beginCheckpointTurn(ctx, &RunState{}, "把导出改成流式", boundary(2, 1))
	require.NoError(t, fake.AppendMessage(&session.Message{Type: session.MessageTypeAssistant, Content: "第一轮的回答"}))
	require.NoError(t, fake.AppendAPIRequest(&session.APIRequest{Seq: 1}))
	// Turn 2 began later: the fork asked for turn 1 must stop at 2, not here.
	a.beginCheckpointTurn(ctx, &RunState{}, "再改一处", boundary(5, 2))
	require.NoError(t, fake.AppendAPIRequest(&session.APIRequest{Seq: 2}))

	fork, err := a.ForkSession(ctx, 1, "把导出改成流式 · 分支")
	require.NoError(t, err)

	require.Len(t, fake.forks, 1)
	assert.Equal(t, 2, fake.forks[0].KeepRecords, "the cut is turn 1's boundary")
	assert.Equal(t, 1, fake.forks[0].KeepAPI)
	assert.Equal(t, "把导出改成流式 · 分支", fake.forks[0].Title)
	assert.Equal(t, "fork-1", fork.ID)
	assert.Len(t, fake.forked, 2, "the fork holds exactly the head the store was asked for")
	assert.Len(t, fake.messages, 5, "the source conversation is not truncated")
}

// TestForkSessionRefusesATurnWithNoCheckpoint: the manifest is the only place a
// turn's cut point is written down, so a turn it no longer holds (pruned past
// retain, or a number that was never one) has no boundary to fork at. The refusal
// names the turn and says why.
func TestForkSessionRefusesATurnWithNoCheckpoint(t *testing.T) {
	a, _, ctx, fake := rewindTestAgent(t, 4)
	a.beginCheckpointTurn(ctx, &RunState{}, "第一轮", boundary(2, 1))

	_, err := a.ForkSession(ctx, 9, "分支")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "第 9 轮")
	assert.Contains(t, err.Error(), "不能作为分支点")
	assert.Empty(t, fake.forks, "a refused fork asks the store for nothing")
}

// TestForkSessionRefusesWhileATurnIsRunning: a fork only READS the history, so it
// cannot corrupt it — but the copy would be a conversation whose last turn is
// amputated (the tool call recorded, its result not yet), which is not a history
// anything should start from.
func TestForkSessionRefusesWhileATurnIsRunning(t *testing.T) {
	a, _, ctx, _ := rewindTestAgent(t, 2)
	a.beginCheckpointTurn(ctx, &RunState{}, "第一轮", boundary(1, 0))

	live := &RunState{}
	a.mu.Lock()
	a.currentRun = live
	a.mu.Unlock()

	_, err := a.ForkSession(ctx, 1, "分支")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "正在运行")

	live.markFinished()
	_, err = a.ForkSession(ctx, 1, "分支")
	assert.NoError(t, err, "a finished turn must not block a fork")
}

// TestForkSessionRefusesWithoutCheckpoints: turn numbering IS the manifest, so
// there is no second way to answer "where does turn N start" — inventing one here
// would be a second definition of a boundary the transcript resolves through
// RewindTurns.
func TestForkSessionRefusesWithoutCheckpoints(t *testing.T) {
	a, _, ctx, _ := rewindTestAgent(t, 2)
	disabled := false
	a.Config.FullConfig.Checkpoints.Enabled = &disabled

	_, err := a.ForkSession(ctx, 1, "分支")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "检查点未启用")
}

// TestForkSessionDoesNotNeedTheWorkspaceRootsToExist: a fork never touches the
// files, so a session whose directory is gone still forks — its conversation is
// what is being copied, and refusing would take the reader's history away for a
// reason that has nothing to do with it.
func TestForkSessionDoesNotNeedTheWorkspaceRootsToExist(t *testing.T) {
	a, work, ctx, fake := rewindTestAgent(t, 4)
	a.beginCheckpointTurn(ctx, &RunState{}, "第一轮", boundary(2, 1))
	require.NoError(t, os.RemoveAll(work))

	_, err := a.ForkSession(ctx, 1, "分支")
	require.NoError(t, err)
	assert.Len(t, fake.forks, 1)
}
