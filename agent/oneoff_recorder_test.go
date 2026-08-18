package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// ---- helpers ----

// overrideOneoffDirs points both one-off dir resolvers at tmp dirs for the
// duration of a test.
func overrideOneoffDirs(t *testing.T) (sessionDir, homeDir string) {
	t.Helper()
	sessionDir = t.TempDir()
	homeDir = t.TempDir()
	origS, origH := oneoffSessionDirFn, oneoffHomeDirFn
	oneoffSessionDirFn = func() (string, error) { return sessionDir, nil }
	oneoffHomeDirFn = func() string { return homeDir }
	t.Cleanup(func() { oneoffSessionDirFn, oneoffHomeDirFn = origS, origH })
	return sessionDir, homeDir
}

// readJSONLLines parses every line of a JSONL file into generic maps.
func readJSONLLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var lines []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(ln), &m))
		lines = append(lines, m)
	}
	return lines
}

// lineTypes extracts the "type" field of each JSONL line, in order.
func lineTypes(lines []map[string]any) []string {
	types := make([]string, 0, len(lines))
	for _, ln := range lines {
		types = append(types, ln["type"].(string))
	}
	return types
}

// ---- recorder unit tests ----

func TestOneoffRecorder_PerSessionDir(t *testing.T) {
	sessionDir, _ := overrideOneoffDirs(t)

	rec, err := newOneoffRecorder(OneOffMeta{
		Kind:         "review",
		SystemPrompt: "you are a reviewer",
		Extra:        map[string]string{"thread": "t-1"},
	}, "sess-abc", nil, "/repo", 30)
	require.NoError(t, err)

	rec.record(&session.Message{Type: session.MessageTypeUser, Content: "review this"})
	rec.record(&session.Message{Type: session.MessageTypeAssistant, Content: "lgtm"})
	path, size, _ := rec.close()

	// File lands under <sessionDir>/sess-abc/oneoff/
	require.Equal(t, filepath.Join(sessionDir, "sess-abc", "oneoff", filepath.Base(path)), path)
	require.FileExists(t, path)
	assert.Greater(t, size, int64(0))

	lines := readJSONLLines(t, path)
	require.Len(t, lines, 3)
	assert.Equal(t, []string{"meta", "user", "assistant"}, lineTypes(lines))

	// Meta header fields
	meta := lines[0]
	assert.Equal(t, "review", meta["kind"])
	assert.Equal(t, "sess-abc", meta["session_id"])
	assert.Equal(t, "you are a reviewer", meta["system_prompt"])
	assert.Equal(t, "/repo", meta["cwd"])
	assert.Equal(t, map[string]any{"thread": "t-1"}, meta["extra"])
	assert.NotEmpty(t, meta["started_at"])
}

func TestOneoffRecorder_GlobalDir(t *testing.T) {
	_, homeDir := overrideOneoffDirs(t)

	rec, err := newOneoffRecorder(OneOffMeta{Kind: "dream"}, "", nil, "", 30)
	require.NoError(t, err)
	path, _, _ := rec.close()

	// No session → global <home>/oneoff/<kind>/
	require.Equal(t, filepath.Join(homeDir, "dream", filepath.Base(path)), path)
	require.FileExists(t, path)

	lines := readJSONLLines(t, path)
	require.Len(t, lines, 1)
	assert.Equal(t, "meta", lines[0]["type"])
	assert.Empty(t, lines[0]["session_id"])
}

func TestOneoffRecorder_ProviderInMeta(t *testing.T) {
	overrideOneoffDirs(t)
	prov := &mockStreamProvider{name: "deepseek"}

	rec, err := newOneoffRecorder(OneOffMeta{Kind: "commit"}, "", prov, "", 30)
	require.NoError(t, err)
	path, _, _ := rec.close()

	lines := readJSONLLines(t, path)
	assert.Equal(t, "deepseek", lines[0]["provider"])
	assert.NotEmpty(t, lines[0]["model"])
}

func TestSweepOneoffDir(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "dream-20200101-000000-abcd.jsonl")
	fresh := filepath.Join(dir, "dream-20990101-000000-ef01.jsonl")
	other := filepath.Join(dir, "notes.txt")
	for _, p := range []string{old, fresh, other} {
		require.NoError(t, os.WriteFile(p, []byte("{}"), 0600))
	}
	// Age the old file beyond the 30-day retention.
	past := time.Now().Add(-40 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(old, past, past))

	sweepOneoffDir(dir, 30)

	assert.NoFileExists(t, old)
	assert.FileExists(t, fresh)
	assert.FileExists(t, other)

	// retentionDays <= 0 → no-op sweep
	past2 := time.Now().Add(-365 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(fresh, past2, past2))
	sweepOneoffDir(dir, 0)
	assert.FileExists(t, fresh)
}

func TestOneoffRecorder_GlobalCreationSweeps(t *testing.T) {
	_, homeDir := overrideOneoffDirs(t)
	kindDir := filepath.Join(homeDir, "ambient")
	require.NoError(t, os.MkdirAll(kindDir, 0700))
	stale := filepath.Join(kindDir, "ambient-20200101-000000-abcd.jsonl")
	require.NoError(t, os.WriteFile(stale, []byte("{}"), 0600))
	past := time.Now().Add(-60 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(stale, past, past))

	rec, err := newOneoffRecorder(OneOffMeta{Kind: "ambient"}, "", nil, "", 30)
	require.NoError(t, err)
	rec.close()

	assert.NoFileExists(t, stale, "global recorder creation should sweep stale files")
}

// ---- recordSession redirect ----

func TestRecordSession_RedirectsToSidecar(t *testing.T) {
	sessionDir, _ := overrideOneoffDirs(t)
	a := newTestAgent(t, &mockStreamProvider{name: "mock"})

	rec, err := newOneoffRecorder(OneOffMeta{Kind: "commit"}, "sess-x", nil, "", 30)
	require.NoError(t, err)
	rs := &RunState{SkipSessionWrites: true, OneoffRec: rec} // one-off mode

	a.recordSession(rs, &session.Message{Type: session.MessageTypeUser, Content: "hi"})
	path, _, _ := rec.close()

	lines := readJSONLLines(t, path)
	require.Len(t, lines, 2) // meta + user
	assert.Equal(t, "user", lines[1]["type"])
	assert.Equal(t, "hi", lines[1]["content"])
	require.Equal(t, filepath.Join(sessionDir, "sess-x", "oneoff", filepath.Base(path)), path)
}

func TestRecordSession_NoRecorderNoPanic(t *testing.T) {
	a := newTestAgent(t, &mockStreamProvider{name: "mock"})
	rs := &RunState{SkipSessionWrites: true}
	// No session manager, no recorder — must be a silent no-op.
	a.recordSession(rs, &session.Message{Type: session.MessageTypeUser, Content: "hi"})
}

// ---- RunOneOffStream end-to-end ----

func TestRunOneOffStream_RecordsSidecar(t *testing.T) {
	sessionDir, _ := overrideOneoffDirs(t)

	// Agent with a real session rooted at the overridden session dir.
	// Stream delay + a slow tool make the api_request and tool_result
	// durations measurably non-zero against the fast in-memory mock.
	prov := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{
		toolCallSeq("Bash", "call_1", `{"command":"git status"}`),
		textSeq("review done"),
	}, streamDelay: 5 * time.Millisecond}
	a := newTestAgent(t, prov)
	a.RegisterTool(&stubTool{
		name: "Bash",
		desc: "Run a command",
		executeFn: func(ctx context.Context, args string) (string, error) {
			time.Sleep(5 * time.Millisecond) // make tool_result duration measurable
			var m map[string]any
			json.Unmarshal([]byte(args), &m)
			cmd, _ := m["command"].(string)
			return "executed: " + cmd, nil
		},
	})

	store, err := session.NewFileStore(sessionDir)
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)
	_, err = sm.New("mock", t.TempDir())
	require.NoError(t, err)
	a.SetSessionManager(sm)

	sessID := sm.Current().ID
	result, _ := drainAgentEvents(a.RunOneOffStream(
		t.Context(), prov, "system prompt", "review the diff",
		llm.ChatOptions{MaxTokens: 1024}, WithOneOffMeta(&OneOffMeta{Kind: "review"})))

	require.NotNil(t, result)
	assert.Equal(t, ExitReasonStop, result.ExitReason)

	// Sidecar file under the session's oneoff/ dir, path surfaced via accessor.
	path := a.LastOneoffTranscriptPath()
	require.Equal(t, filepath.Join(sessionDir, sessID, "oneoff", filepath.Base(path)), path)

	lines := readJSONLLines(t, path)
	// The intermediate assistant line (empty text + usage, from the tool_calls
	// step) mirrors what recordAssistantTurn writes to a normal session — the
	// sidecar is a faithful mirror of messages.jsonl semantics. Each API call
	// additionally records an "api_request" line (system prompt + tools).
	assert.Equal(t,
		[]string{"meta", "user", "api_request", "assistant", "tool_call", "tool_result", "api_request", "assistant"},
		lineTypes(lines))
	assert.Equal(t, "review", lines[0]["kind"])
	assert.Equal(t, "system prompt", lines[0]["system_prompt"])
	assert.Equal(t, "review the diff", lines[1]["content"])
	assert.Equal(t, "Bash", lines[4]["name"])
	assert.Contains(t, lines[5]["result"], "executed: git status")
	// The tool_result line carries the execution duration.
	assert.Greater(t, lines[5]["duration_ms"].(float64), float64(0), "tool_result records the tool execution duration")
	assert.Equal(t, "review done", lines[7]["content"])

	// api_request lines carry the system prompt and tool schemas of each call.
	req1 := lines[2]
	assert.Equal(t, "api_request", req1["type"])
	assert.Equal(t, "system prompt", req1["system_prompt"])
	assert.Greater(t, req1["duration_ms"].(float64), float64(0), "api_request carries the LLM call duration")
	req1Tools, ok := req1["tools"].([]any)
	require.True(t, ok, "api_request tools should be a list")
	require.NotEmpty(t, req1Tools)
	firstTool, ok := req1Tools[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Bash", firstTool["name"])
	assert.Contains(t, firstTool, "parameters", "tool schema should be recorded")

	req2 := lines[6]
	assert.Equal(t, "api_request", req2["type"])
	assert.Equal(t, "system prompt", req2["system_prompt"])
	assert.Greater(t, req2["duration_ms"].(float64), float64(0), "api_request carries the LLM call duration")
	req2Tools, ok := req2["tools"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, req2Tools)

	// Main session history stays untouched — isolation preserved.
	msgs, err := sm.LoadMessages()
	require.NoError(t, err)
	assert.Empty(t, msgs, "one-off run must not write to messages.jsonl")
}

func TestRunOneOffStream_EmptyKindNoRecording(t *testing.T) {
	sessionDir, homeDir := overrideOneoffDirs(t)
	prov := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{textSeq("ok")}}
	a := newTestAgent(t, prov)

	result, _ := drainAgentEvents(a.RunOneOffStream(
		t.Context(), prov, "sys", "hi", llm.ChatOptions{MaxTokens: 1024}))
	require.NotNil(t, result)

	assert.Empty(t, a.LastOneoffTranscriptPath())
	// Neither dir tree was created.
	_, errS := os.ReadDir(sessionDir)
	_, errH := os.ReadDir(homeDir)
	require.NoError(t, errS)
	require.NoError(t, errH)
	assert.Empty(t, mustReadDir(t, sessionDir))
	assert.Empty(t, mustReadDir(t, homeDir))
}

func TestRunOneOffStream_DisabledByConfig(t *testing.T) {
	sessionDir, homeDir := overrideOneoffDirs(t)
	prov := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{textSeq("ok")}}
	a := newTestAgent(t, prov)
	disabled := false
	a.Config.FullConfig = &config.Config{Oneoff: config.OneoffConfig{Enabled: &disabled}}

	result, _ := drainAgentEvents(a.RunOneOffStream(
		t.Context(), prov, "sys", "hi", llm.ChatOptions{MaxTokens: 1024},
		WithOneOffMeta(&OneOffMeta{Kind: "commit"})))
	require.NotNil(t, result)

	assert.Empty(t, a.LastOneoffTranscriptPath())
	assert.Empty(t, mustReadDir(t, sessionDir))
	assert.Empty(t, mustReadDir(t, homeDir))
}

func TestRunOneOffStream_RecorderFailureStillRuns(t *testing.T) {
	// Point the global oneoff dir at an existing FILE — MkdirAll must fail.
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0600))
	origH := oneoffHomeDirFn
	oneoffHomeDirFn = func() string { return blocker }
	t.Cleanup(func() { oneoffHomeDirFn = origH })

	prov := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{textSeq("still works")}}
	a := newTestAgent(t, prov)

	result, _ := drainAgentEvents(a.RunOneOffStream(
		t.Context(), prov, "sys", "hi", llm.ChatOptions{MaxTokens: 1024},
		WithOneOffMeta(&OneOffMeta{Kind: "commit"})))

	// Recording failure is a Warn, never fatal — the run completes normally.
	require.NotNil(t, result)
	assert.Equal(t, ExitReasonStop, result.ExitReason)
	assert.Empty(t, a.LastOneoffTranscriptPath())
}

// mustReadDir returns entry names (fails the test on error).
func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestOneoffRecorder_RecordAPIRequest(t *testing.T) {
	_, homeDir := overrideOneoffDirs(t)

	rec, err := newOneoffRecorder(OneOffMeta{Kind: "commit"}, "", nil, "/repo", 30)
	require.NoError(t, err)

	rec.recordAPIRequest(&session.APIRequest{
		SystemPrompt: "system prompt",
		UserPrompt:   "review the diff",
		Iteration:    2,
		Tools: []session.APITool{{
			Name:        "ReadFile",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path"}},"required":["path"]}`),
		}},
	})
	path, _, _ := rec.close()

	lines := readJSONLLines(t, path)
	require.Len(t, lines, 2) // meta + api_request
	assert.Equal(t, []string{"meta", "api_request"}, lineTypes(lines))

	req := lines[1]
	assert.Equal(t, "api_request", req["type"])
	assert.Equal(t, "system prompt", req["system_prompt"])
	assert.Equal(t, "review the diff", req["user_prompt"])
	assert.Equal(t, float64(2), req["iteration"])
	tools, ok := req["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)
	tool, ok := tools[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "ReadFile", tool["name"])
	assert.Equal(t, "Read a file", tool["description"])
	params, ok := tool["parameters"].(map[string]any)
	require.True(t, ok, "tool schema should be an object")
	assert.Equal(t, "object", params["type"])
	// Global dir layout unchanged.
	require.True(t, strings.Contains(path, filepath.Join(homeDir, "commit")))
}

func TestOneoffRecorder_RecordAPIRequestWithDuration(t *testing.T) {
	_, _ = overrideOneoffDirs(t)

	rec, err := newOneoffRecorder(OneOffMeta{Kind: "commit"}, "", nil, "/repo", 30)
	require.NoError(t, err)

	// The run loop writes the api_request line once, AFTER the call completes,
	// already carrying its start time and wall-clock duration — no in-place
	// patching. Interleaved messages preserve the sidecar's row order.
	start := time.Now().Add(-2 * time.Second)
	rec.recordAPIRequest(&session.APIRequest{Seq: 1, Timestamp: start, SystemPrompt: "p1", DurationMs: 2000})
	rec.record(&session.Message{Type: session.MessageTypeAssistant, Content: "a1"})
	rec.record(&session.Message{Type: session.MessageTypeToolResult, Name: "Bash", Content: "r1", DurationMs: 7})
	rec.recordAPIRequest(&session.APIRequest{Seq: 2, Timestamp: start, SystemPrompt: "p2", DurationMs: 3000})

	path, _, _ := rec.close()
	lines := readJSONLLines(t, path)
	require.Equal(t, []string{"meta", "api_request", "assistant", "tool_result", "api_request"}, lineTypes(lines))
	// api_request lines carry their duration (written once, at record time).
	assert.Equal(t, float64(2000), lines[1]["duration_ms"])
	// Interleaved messages preserved, tool_result keeps its own duration.
	assert.Equal(t, "a1", lines[2]["content"])
	assert.Equal(t, float64(7), lines[3]["duration_ms"])
	assert.Equal(t, float64(3000), lines[4]["duration_ms"])
}
