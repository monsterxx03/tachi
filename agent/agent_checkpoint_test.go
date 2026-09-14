package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
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

	a.beginCheckpointTurn(ctx, rs, "把导出改成流式", 3, 2)
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

// TestCheckpointWiringSkipsOneOffRuns pins that a side channel gets no
// checkpoint: one-off runs never write the main session, so a rewind of the
// main conversation must not depend on them.
func TestCheckpointWiringSkipsOneOffRuns(t *testing.T) {
	a, _, ctx := checkpointTestAgent(t)
	rs := &RunState{SkipSessionWrites: true}

	a.beginCheckpointTurn(ctx, rs, "评审一下", 0, 0)

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

			a.beginCheckpointTurn(ctx, rs, "你好", 0, 0)

			assert.Equal(t, 0, rs.CheckpointTurn())
			_, err := os.Stat(manifestPath(t, a))
			assert.True(t, os.IsNotExist(err))
		})
	}
}

func boolPtr(v bool) *bool { return &v }
