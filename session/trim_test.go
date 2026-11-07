package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The lines below are written by hand rather than marshaled: the point of most of
// these tests is what the trim does NOT touch, and a re-marshaled fixture would make
// every assertion about "the untouched lines are byte-identical" vacuous.
const (
	trimFixtureUser    = `{"type":"user","content":"帮我看看这个仓库","timestamp":"2026-01-01T00:00:00Z"}`
	trimFixtureCall    = `{"type":"tool_call","name":"ReadFile","args":{"path":"a.go"},"tool_call_id":"c1","timestamp":"2026-01-01T00:00:01Z"}`
	trimFixtureResult  = `{"type":"tool_result","name":"ReadFile","result":"LONG OUTPUT","tool_call_id":"c1","timestamp":"2026-01-01T00:00:02Z"}`
	trimFixtureCall2   = `{"type":"tool_call","name":"Bash","args":{"command":"ls"},"tool_call_id":"c2","timestamp":"2026-01-01T00:00:03Z"}`
	trimFixtureResult2 = `{"type":"tool_result","name":"Bash","result":"a.go b.go","tool_call_id":"c2","timestamp":"2026-01-01T00:00:04Z"}`
	// Field order and number formatting deliberately unlike what a re-marshal would
	// produce — this line is the witness that untouched records are copied verbatim.
	trimFixtureAssistant = `{"content":"好","type":"assistant","usage":{"input_tokens":12.0},"timestamp":"2026-01-01T00:00:05Z"}`
)

func TestTrimToolResultsRewritesOnlyTheNamedResults(t *testing.T) {
	store, id := storeForTest(t, trimFixtureUser, trimFixtureCall, trimFixtureResult,
		trimFixtureCall2, trimFixtureResult2, trimFixtureAssistant)

	res, err := store.TrimToolResults(id, map[string]string{"c1": "[输出已裁剪]"}, "t1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Trimmed)
	require.NotEmpty(t, res.Sidecar)

	data := string(readLines(t, store.messagesPath(id)))
	// Only c1's result changed.
	assert.Contains(t, data, `"result":"[输出已裁剪]"`)
	assert.Contains(t, data, trimFixtureResult2, "a result that was not named must be untouched")
	// Everything else is byte-identical — the assistant line proves it, because a
	// re-marshal would have rewritten its key order and its float.
	assert.Contains(t, data, trimFixtureAssistant)
	assert.Contains(t, data, trimFixtureUser)
	assert.Contains(t, data, trimFixtureCall)

	// The original file is kept whole, beside the session.
	sidecar, err := os.ReadFile(res.Sidecar)
	require.NoError(t, err)
	assert.Equal(t, trimFixtureUser+"\n"+trimFixtureCall+"\n"+trimFixtureResult+"\n"+
		trimFixtureCall2+"\n"+trimFixtureResult2+"\n"+trimFixtureAssistant+"\n", string(sidecar))
	assert.Equal(t, filepath.Join(store.sessionDir(id), "trimmed", "messages-t1.jsonl"), res.Sidecar)
}

func TestTrimToolResultsDropsTheImagesTheResultCarried(t *testing.T) {
	line := `{"type":"tool_result","name":"ReadFile","result":"[Image: a.png]","tool_call_id":"c9","parts":[{"type":"image","ref":"abc","media_type":"image/png"}]}`
	store, id := storeForTest(t, line)

	res, err := store.TrimToolResults(id, map[string]string{"c9": "note"}, "t")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Trimmed)

	var msg Message
	require.NoError(t, json.Unmarshal([]byte(readLines(t, store.messagesPath(id))), &msg))
	assert.Equal(t, "note", msg.Result)
	assert.Empty(t, msg.Parts, "an image is part of what a trim exists to stop sending")
	assert.Equal(t, "c9", msg.ToolCallID, "the pair with its tool_call must survive")
}

func TestTrimToolResultsIsANoOpWhenNothingMatches(t *testing.T) {
	store, id := storeForTest(t, trimFixtureUser, trimFixtureResult)

	before := readLines(t, store.messagesPath(id))
	res, err := store.TrimToolResults(id, map[string]string{"nope": "note"}, "t")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Trimmed)
	assert.Empty(t, res.Sidecar)
	assert.Equal(t, before, readLines(t, store.messagesPath(id)))
	_, err = os.Stat(filepath.Join(store.sessionDir(id), "trimmed"))
	assert.True(t, os.IsNotExist(err), "a trim that changed nothing must not leave a directory behind")
}

func TestTrimToolResultsWithNoReplacementsTouchesNothing(t *testing.T) {
	store, id := storeForTest(t, trimFixtureResult)
	before := readLines(t, store.messagesPath(id))

	res, err := store.TrimToolResults(id, nil, "t")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Trimmed)
	assert.Equal(t, before, readLines(t, store.messagesPath(id)))
}

func TestTrimToolResultsPreservesALastLineWithoutNewline(t *testing.T) {
	store, id := storeForTest(t, trimFixtureUser, trimFixtureResult)
	path := store.messagesPath(id)
	require.NoError(t, os.WriteFile(path, []byte(trimFixtureUser+"\n"+trimFixtureResult), 0o600))

	res, err := store.TrimToolResults(id, map[string]string{"c1": "note"}, "t")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Trimmed)

	data := string(readLines(t, path))
	assert.True(t, len(data) > 0)
	assert.Equal(t, byte('}'), data[len(data)-1], "no newline is invented at the end of the file")
	assert.Contains(t, data, trimFixtureUser+"\n")
}

func TestTrimToolResultsWithoutAFileIsANoOp(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(&Session{ID: "empty"}))

	res, err := store.TrimToolResults("empty", map[string]string{"c1": "note"}, "t")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Trimmed)
}

// A line the store cannot parse is left exactly as it is: the trim is not a
// validator, and a record it cannot read is not one it may drop.
func TestTrimToolResultsLeavesUnparsableLinesAlone(t *testing.T) {
	store, id := storeForTest(t, `{not json`, trimFixtureResult)

	res, err := store.TrimToolResults(id, map[string]string{"c1": "note"}, "t")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Trimmed)
	assert.Contains(t, string(readLines(t, store.messagesPath(id))), `{not json`)
}
