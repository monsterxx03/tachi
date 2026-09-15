package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkpointManifest mirrors the parts of the on-disk manifest these tests
// assert. It is deliberately a local struct rather than the checkpoint
// package's: the point is to pin the FILE contract the frontends and a rewind
// read, not the package's internals.
type checkpointManifest struct {
	Checkpoints []struct {
		Turn       int    `json:"turn"`
		Records    int    `json:"records"`
		APIRecords int    `json:"api_records"`
		UserText   string `json:"user_text"`
		Roots      []struct {
			RootIndex int    `json:"root_index"`
			Root      string `json:"root"`
			Tree      string `json:"tree"`
		} `json:"roots"`
	} `json:"checkpoints"`
}

// checkpointTestAgent returns an agent whose session lives in a temp dir, with
// checkpointing on and the turn's working directory bound in the context.
func checkpointTestAgent(t *testing.T) (*AIAgent, string, context.Context) {
	t.Helper()
	oldBase := config.BaseDir()
	t.Cleanup(func() { config.SetBaseDir(oldBase) })
	config.SetBaseDir(t.TempDir())

	work := t.TempDir()
	enabled := true
	// A provider is required: rebuilding a session's history converts the records
	// with the provider's type (see LoadSessionHistory), which a rewind does.
	a := newTestAgent(t, &mockStreamProvider{name: "anthropic"}, withFakeSession())
	a.Config.FullConfig = &config.Config{
		Checkpoints: config.CheckpointConfig{Enabled: &enabled},
	}
	_, err := a.Config.SessionManager.New("test", work)
	require.NoError(t, err)
	return a, work, wdctx.WithDir(context.Background(), work)
}

// manifestPath is where a session's checkpoint index lives: beside the messages
// and the one-off sidecars, under the session's own directory.
func manifestPath(t *testing.T, a *AIAgent) string {
	t.Helper()
	root, err := config.SessionDir()
	require.NoError(t, err)
	return filepath.Join(root, a.Config.SessionManager.Current().ID, "checkpoints", "manifest.json")
}

func readCheckpointManifest(t *testing.T, a *AIAgent) checkpointManifest {
	t.Helper()
	data, err := os.ReadFile(manifestPath(t, a))
	require.NoError(t, err, "the manifest must exist once a turn has begun")
	var m checkpointManifest
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

// TestCheckpointWiringRecordsBoundaryThenSnapshot pins the two call sites'
// contract: the boundary is recorded at the turn start from the counts the
// caller scanned (which exclude this turn's user message, so a rewind hands it
// back), and the file half is taken only for a tool that could write — and only
// once in the turn, because a second snapshot would record the middle of a turn
// instead of its start.
func TestCheckpointWiringRecordsBoundaryThenSnapshot(t *testing.T) {
	a, work, ctx := checkpointTestAgent(t)
	rs := &RunState{}

	a.beginCheckpointTurn(ctx, rs, "把导出改成流式", boundary(3, 2))
	require.Equal(t, 1, rs.CheckpointTurn())

	m := readCheckpointManifest(t, a)
	require.Len(t, m.Checkpoints, 1)
	rec := m.Checkpoints[0]
	assert.Equal(t, 3, rec.Records, "the cut point is where the conversation stood BEFORE the user message")
	assert.Equal(t, 2, rec.APIRecords)
	assert.Equal(t, "把导出改成流式", rec.UserText)

	// A read-only tool must not pay for a walk of the tree.
	require.NoError(t, a.snapshotBeforeWrite(ctx, rs, tools.ToolNameRead))
	assert.Empty(t, readCheckpointManifest(t, a).Checkpoints[0].Roots,
		"a read-only tool must not trigger a snapshot")

	// A write-capable one must, and must record the tree as it is right now.
	require.NoError(t, os.WriteFile(filepath.Join(work, "a.txt"), []byte("v1"), 0o644))
	require.NoError(t, a.snapshotBeforeWrite(ctx, rs, tools.ToolNameWrite))
	rec = readCheckpointManifest(t, a).Checkpoints[0]
	require.Len(t, rec.Roots, 1)
	assert.Equal(t, work, rec.Roots[0].Root)
	require.NotEmpty(t, rec.Roots[0].Tree)

	// A second write in the same turn is a lookup, not a new snapshot: the state
	// recorded must still be the one from before the first write.
	require.NoError(t, os.WriteFile(filepath.Join(work, "a.txt"), []byte("v2"), 0o644))
	require.NoError(t, a.snapshotBeforeWrite(ctx, rs, tools.ToolNameBash))
	assert.Equal(t, rec.Roots[0].Tree, readCheckpointManifest(t, a).Checkpoints[0].Roots[0].Tree,
		"the snapshot must describe the turn's start, not its middle")
	assert.Equal(t, 1, len(readCheckpointManifest(t, a).Checkpoints))
}

// TestCheckpointRebindsAfterCompaction pins the fix for a refusal the reader reports as
// "bash 工具会报错: turn 29 was never begun" right after a session was compacted and they
// kept working in the new one.
//
// Both halves of the checkpoint binding are session-scoped — the turn's NUMBER lives in the
// old session's manifest, and the manager resolves by session id — so a compaction that
// swaps the session mid-turn leaves the rest of that turn with a turn number the new
// session has never heard of. Every write-capable tool then refuses its write (by design:
// a change a rewind cannot take back must not happen), until the turn ends and the next one
// begins a fresh checkpoint. Rebinding right after the swap is what keeps the turn usable.
func TestCheckpointRebindsAfterCompaction(t *testing.T) {
	trueVal := true
	a, work, ctx := checkpointTestAgent(t)
	// The auto-compact trigger, and a strategy that produces a summary without an LLM.
	a.SetCompactStrategy(&fakeCompactStrategy{summary: "摘要"})
	a.Config.FullConfig = &config.Config{
		Checkpoints: config.CheckpointConfig{Enabled: boolPtr(true)},
		Compact: config.CompactConfig{
			Auto: &trueVal, Threshold: 0.5, MaxTokens: 1024, Timeout: time.Minute,
		},
	}
	a.SetContextWindow(1000)
	a.conv.setEstimate(600, tokenbreakdown.Breakdown{}) // 60% > the 50% threshold

	rs := &RunState{
		Messages: []llm.Message{
			{Role: "system", Content: "You are Tachi."},
			{Role: "user", Content: "把导出改成流式"},
			{Role: "assistant", Content: "好"},
		},
		Budget: NewIterationBudget(0),
	}

	// The turn starts in the parent session and records its boundary there — the state the
	// whole problem comes from.
	parentID := a.Config.SessionManager.Current().ID
	a.beginCheckpointTurn(ctx, rs, "把导出改成流式", boundary(3, 2))
	require.Equal(t, 1, rs.CheckpointTurn())
	require.NoError(t, a.snapshotBeforeWrite(ctx, rs, tools.ToolNameWrite))

	ch := make(chan AgentEvent, 16)
	_, compacted := a.maybeAutoCompact(ctx, rs, &runInput{UserText: "把导出改成流式"}, &llm.ChatOptions{}, ch)
	close(ch)
	require.True(t, compacted, "the fixture must actually compact, or this proves nothing")
	require.NotEqual(t, parentID, a.Config.SessionManager.Current().ID,
		"compaction must have moved the conversation into a new session")
	// Re-bound in the new session — and that session numbers its turns from 1. (The NUMBER alone
	// does not tell the two apart; the manifest below is what does: without the rebind the new
	// session has no record at all, and the write below is refused with "turn 1 was never begun".)
	require.Equal(t, 1, rs.CheckpointTurn())

	// The write the reader was refused: same turn, new session.
	require.NoError(t, os.WriteFile(filepath.Join(work, "b.txt"), []byte("v1"), 0o644))
	require.NoError(t, a.snapshotBeforeWrite(ctx, rs, tools.ToolNameBash),
		"a write after the compaction must not be refused")

	// The checkpoint lives in the NEW session's store, labelled with this turn's prompt.
	m := readCheckpointManifest(t, a)
	require.Len(t, m.Checkpoints, 1, "one turn in the new session")
	assert.Equal(t, 1, m.Checkpoints[0].Turn)
	assert.Equal(t, "把导出改成流式", m.Checkpoints[0].UserText)
	require.Len(t, m.Checkpoints[0].Roots, 1)
	assert.Equal(t, work, m.Checkpoints[0].Roots[0].Root)
}

// TestCheckpointWiringEndsTheTurnWithItsChanges: the turn end records where the writes LEFT
// the workspace, and the summary that comes out of it covers what NO tool declared — two
// files written by a shell command — because it is read from the trees, not from the calls.
// A turn that only read gets no summary at all (and paid nothing for one).
func TestCheckpointWiringEndsTheTurnWithItsChanges(t *testing.T) {
	a, work, ctx, _ := rewindTestAgent(t, 0)

	rs := &RunState{}
	a.beginCheckpointTurn(ctx, rs, "用 shell 写两个文件", boundary(0, 0))
	require.NoError(t, a.snapshotBeforeWrite(ctx, rs, tools.ToolNameBash))
	// The turn's work: a shell command's writes, which no tool argument describes.
	require.NoError(t, os.WriteFile(filepath.Join(work, "made.txt"), []byte("a\nb\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(work, "other.txt"), []byte("c\n"), 0o644))
	a.endCheckpointTurn(ctx, rs)

	d := a.TurnSummary(ctx, rs.CheckpointTurn())
	require.NotNil(t, d, "a turn that wrote has a summary")
	assert.Empty(t, d.Skipped)
	assert.Equal(t, 2, d.Files)
	assert.Equal(t, 3, d.Added)
	assert.Equal(t, 0, d.Removed)

	diffs, ok, err := a.TurnDiff(ctx, rs.CheckpointTurn())
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, diffs, 1)
	assert.Equal(t, work, diffs[0].Root, "the diff says which root it came from")
	assert.Contains(t, diffs[0].Text, "made.txt")
	assert.Contains(t, diffs[0].Text, "+a")

	// A turn that only reads: no end state, no summary, and the readers are told there is
	// no pair instead of being handed zeroes.
	read := &RunState{}
	a.beginCheckpointTurn(ctx, read, "只是看看", boundary(0, 0))
	a.endCheckpointTurn(ctx, read)
	assert.Nil(t, a.TurnSummary(ctx, read.CheckpointTurn()))
	_, ok, err = a.TurnDiff(ctx, read.CheckpointTurn())
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestCheckpointWiringEndsAStoppedTurn is the path a stopped turn takes: the loop returns on
// a CANCELLED context (Stop cancels it, it does not set a flag) and endCheckpointTurn is the
// cleanup that runs afterwards. The turn's writes are on disk either way, so the numbers must
// come out of it exactly as they do on a normal exit — running git on the dead context would
// record "no numbers" for the very turns a rewind is most often aimed at.
func TestCheckpointWiringEndsAStoppedTurn(t *testing.T) {
	a, work, ctx, _ := rewindTestAgent(t, 0)

	rs := &RunState{}
	a.beginCheckpointTurn(ctx, rs, "改一半我就停了", boundary(0, 0))
	require.NoError(t, a.snapshotBeforeWrite(ctx, rs, tools.ToolNameBash))
	require.NoError(t, os.WriteFile(filepath.Join(work, "half-done.txt"), []byte("写了一半\n"), 0o644))

	stopped, cancel := context.WithCancel(ctx)
	cancel() // exactly what the desktop's Stop does to the turn's context
	a.endCheckpointTurn(stopped, rs)

	d := a.TurnSummary(ctx, rs.CheckpointTurn())
	require.NotNil(t, d, "a stopped turn still has its changes counted")
	assert.Empty(t, d.Skipped)
	assert.Equal(t, 1, d.Files)

	// The stop must not cost the turn its way back: a rewind to it restores the file half.
	p, err := a.PreviewRewind(ctx, rs.CheckpointTurn())
	require.NoError(t, err)
	assert.Equal(t, 1, len(p.Roots), "the stopped turn is still restorable")
}

// TestCheckpointWiringSkipsOneOffRuns pins that a side channel gets no
// checkpoint: one-off runs never write the main session, so a rewind of the
// main conversation must not depend on them.
func TestCheckpointWiringSkipsOneOffRuns(t *testing.T) {
	a, _, ctx := checkpointTestAgent(t)
	rs := &RunState{SkipSessionWrites: true}

	a.beginCheckpointTurn(ctx, rs, "评审一下", boundary(0, 0))

	assert.Equal(t, 0, rs.CheckpointTurn())
	_, err := os.Stat(manifestPath(t, a))
	assert.True(t, os.IsNotExist(err), "a one-off run must not create a checkpoint")
}

// TestCheckpointWiringDisabledByConfig pins the switch: with checkpoints off the
// agent does no work at all, and the turn simply has no checkpoint of its own.
//
// The nil case is the one that matters. A config built in code leaves Enabled
// unset, and unset must mean OFF: an agent that wrote snapshots every turn into
// a store nobody configured is exactly the surprise this default prevents (and
// in a test that means writing outside t.TempDir()).
// TestCheckpointRootsUsesRootsFunc pins the seam the desktop projects need: a frontend
// whose session RECORD is not the workspace authority supplies the resolver, and the
// checkpoint then covers exactly the trees its tools worked in. Without this the desktop
// would keep snapshotting the snapshot — the tree the session was CREATED in — while the
// agent wrote somewhere else, and a rewind would "restore" files nobody had touched.
//
// nil keeps every other entry point on the record, byte for byte.
func TestCheckpointRootsUsesRootsFunc(t *testing.T) {
	sess := &session.Session{
		ID:             "s1",
		WorkingDir:     "/from/record",
		AdditionalDirs: []string{"/record/extra"},
	}

	a := &AIAgent{}
	assert.Equal(t, []string{"/from/record", "/record/extra"}, a.checkpointRoots(sess))

	// An empty primary is passed through as-is: the manager's root set for a session with
	// no workspace must stay exactly what it was (one empty entry, which it drops).
	assert.Equal(t, []string{""}, a.checkpointRoots(&session.Session{ID: "s2"}))

	projected := []string{"/project/primary", "/project/extra"}
	var seen *session.Session
	a.Config.RootsFunc = func(s *session.Session) []string {
		seen = s
		return projected
	}
	assert.Equal(t, projected, a.checkpointRoots(sess))
	assert.Same(t, sess, seen, "the hook resolves for the session the turn belongs to")

	assert.Nil(t, a.checkpointRoots(nil), "no session, no roots")
}

func TestCheckpointWiringDisabledByConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled *bool
	}{
		{"explicitly off", boolPtr(false)},
		{"unset", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, ctx := checkpointTestAgent(t)
			a.Config.FullConfig.Checkpoints.Enabled = tc.enabled
			rs := &RunState{}

			a.beginCheckpointTurn(ctx, rs, "你好", boundary(0, 0))

			assert.Equal(t, 0, rs.CheckpointTurn())
			_, err := os.Stat(manifestPath(t, a))
			assert.True(t, os.IsNotExist(err))
		})
	}
}

func boolPtr(v bool) *bool { return &v }
