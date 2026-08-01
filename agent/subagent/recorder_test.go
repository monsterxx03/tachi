package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorder_NewRecorder_CreatesFile(t *testing.T) {
	tmpDir := t.TempDir()
	// Override SessionDir so newRecorder writes to tmpDir
	origSessionDirFn := sessionDirFn
	sessionDirFn = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDirFn = origSessionDirFn }()

	rec, err := newRecorder("sess-abc", "sub-001", nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	defer rec.close()

	expectedPath := filepath.Join(tmpDir, "sess-abc", "subagent", "sub-001.jsonl")
	assert.FileExists(t, expectedPath)
}

func TestRecorder_Record_WritesJSONL(t *testing.T) {
	tmpDir := t.TempDir()
	origSessionDirFn := sessionDirFn
	sessionDirFn = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDirFn = origSessionDirFn }()

	rec, err := newRecorder("sess-def", "sub-002", nil)
	require.NoError(t, err)
	defer rec.close()

	// Record a user message
	msg := &session.Message{
		Type:    session.MessageTypeUser,
		Content: "find all TODO comments",
	}
	err = rec.record(msg)
	require.NoError(t, err)

	// Record a tool call
	err = rec.record(&session.Message{
		Type:       session.MessageTypeToolCall,
		Name:       "Grep",
		Args:       `{"pattern":"TODO"}`,
		ToolCallID: "tool_001",
	})
	require.NoError(t, err)
}

func TestRecorder_MultipleRecords(t *testing.T) {
	tmpDir := t.TempDir()
	origSessionDirFn := sessionDirFn
	sessionDirFn = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDirFn = origSessionDirFn }()

	rec, err := newRecorder("sess-ghi", "sub-003", nil)
	require.NoError(t, err)
	defer rec.close()

	messages := []session.Message{
		{Type: session.MessageTypeUser, Content: "hello"},
		{Type: session.MessageTypeAssistant, Content: "world"},
		{Type: session.MessageTypeThinking, Content: "let me think..."},
		{Type: session.MessageTypeToolCall, Name: "ReadFile", Args: `{"path":"foo.go"}`, ToolCallID: "tc1"},
		{Type: session.MessageTypeToolResult, Name: "ReadFile", Result: "package main...", ToolCallID: "tc1"},
	}

	for _, msg := range messages {
		err := rec.record(&msg)
		require.NoError(t, err)
	}

	rec.close()

	// Read back and verify
	content, err := os.ReadFile(filepath.Join(tmpDir, "sess-ghi", "subagent", "sub-003.jsonl"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	assert.Len(t, lines, 5)

	// Each line should be valid JSON
	for _, line := range lines {
		var m session.Message
		err := json.Unmarshal([]byte(line), &m)
		assert.NoError(t, err, "line should be valid JSON: %s", line)
	}
}

func TestRecorder_RecordMessageTypes(t *testing.T) {
	tmpDir := t.TempDir()
	origSessionDirFn := sessionDirFn
	sessionDirFn = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDirFn = origSessionDirFn }()

	rec, err := newRecorder("sess-jkl", "sub-004", nil)
	require.NoError(t, err)
	defer rec.close()

	// Test all message types
	types := []session.MessageType{
		session.MessageTypeUser,
		session.MessageTypeAssistant,
		session.MessageTypeThinking,
		session.MessageTypeToolCall,
		session.MessageTypeToolResult,
	}

	for i, typ := range types {
		err := rec.record(&session.Message{
			Type:       typ,
			Content:    "test content",
			Name:       "test-tool",
			Args:       `{"key":"val"}`,
			ToolCallID: "tc_" + string(rune('0'+i)),
		})
		require.NoError(t, err)
	}

	rec.close()

	content, err := os.ReadFile(filepath.Join(tmpDir, "sess-jkl", "subagent", "sub-004.jsonl"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	assert.Len(t, lines, 5)

	for i, line := range lines {
		var m session.Message
		json.Unmarshal([]byte(line), &m)
		assert.Equal(t, types[i], m.Type)
		assert.NotZero(t, m.Timestamp)
	}
}

func TestRecorder_ErrorResult(t *testing.T) {
	tmpDir := t.TempDir()
	origSessionDirFn := sessionDirFn
	sessionDirFn = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDirFn = origSessionDirFn }()

	rec, err := newRecorder("sess-mno", "sub-005", nil)
	require.NoError(t, err)
	defer rec.close()

	err = rec.record(&session.Message{
		Type:       session.MessageTypeToolResult,
		Name:       "Bash",
		Result:     "command not found",
		IsError:    true,
		ToolCallID: "tc_error",
	})
	require.NoError(t, err)

	rec.close()

	content, _ := os.ReadFile(filepath.Join(tmpDir, "sess-mno", "subagent", "sub-005.jsonl"))
	var m session.Message
	json.Unmarshal([]byte(strings.TrimSpace(string(content))), &m)
	assert.True(t, m.IsError)
	assert.Equal(t, "command not found", m.Result)
}

func TestRecorder_CloseClosesFile(t *testing.T) {
	tmpDir := t.TempDir()
	origSessionDirFn := sessionDirFn
	sessionDirFn = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDirFn = origSessionDirFn }()

	rec, err := newRecorder("sess-pqr", "sub-006", nil)
	require.NoError(t, err)

	// Write something
	rec.record(&session.Message{Type: session.MessageTypeUser, Content: "x"})

	err = rec.close()
	assert.NoError(t, err)

	// Write after close should error
	err = rec.record(&session.Message{Type: session.MessageTypeUser, Content: "y"})
	assert.Error(t, err)
}

func TestRecorder_TimestampIsSet(t *testing.T) {
	tmpDir := t.TempDir()
	origSessionDirFn := sessionDirFn
	sessionDirFn = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDirFn = origSessionDirFn }()

	rec, err := newRecorder("sess-stu", "sub-007", nil)
	require.NoError(t, err)
	defer rec.close()

	before := time.Now()
	rec.record(&session.Message{Type: session.MessageTypeUser, Content: "with timestamp"})
	after := time.Now()

	rec.close()

	content, _ := os.ReadFile(filepath.Join(tmpDir, "sess-stu", "subagent", "sub-007.jsonl"))
	var m session.Message
	json.Unmarshal([]byte(strings.TrimSpace(string(content))), &m)

	assert.True(t, !m.Timestamp.Before(before), "timestamp should be >= before")
	assert.True(t, !m.Timestamp.After(after.Add(time.Second)), "timestamp should be <= after+1s")
}

func TestRecorder_UsageMessage(t *testing.T) {
	tmpDir := t.TempDir()
	origSessionDirFn := sessionDirFn
	sessionDirFn = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDirFn = origSessionDirFn }()

	rec, err := newRecorder("sess-usage", "sub-usage", nil)
	require.NoError(t, err)
	defer rec.close()

	err = rec.record(&session.Message{
		Type:    session.MessageTypeAssistant,
		Content: "final output",
		Usage: &session.Usage{
			InputTokens:  500,
			OutputTokens: 200,
		},
	})
	require.NoError(t, err)

	rec.close()

	content, _ := os.ReadFile(filepath.Join(tmpDir, "sess-usage", "subagent", "sub-usage.jsonl"))
	var m session.Message
	json.Unmarshal([]byte(strings.TrimSpace(string(content))), &m)

	assert.NotNil(t, m.Usage)
	assert.Equal(t, int64(500), m.Usage.InputTokens)
	assert.Equal(t, int64(200), m.Usage.OutputTokens)
}

func TestRecorder_AppendsToExistingFile(t *testing.T) {
	tmpDir := t.TempDir()
	origSessionDirFn := sessionDirFn
	sessionDirFn = func() (string, error) { return tmpDir, nil }
	defer func() { sessionDirFn = origSessionDirFn }()

	// Create first recorder, write and close
	rec1, err := newRecorder("sess-vwx", "sub-append", nil)
	require.NoError(t, err)
	rec1.record(&session.Message{Type: session.MessageTypeUser, Content: "msg1"})
	rec1.close()

	// Create second recorder, append and close
	rec2, err := newRecorder("sess-vwx", "sub-append", nil)
	require.NoError(t, err)
	rec2.record(&session.Message{Type: session.MessageTypeAssistant, Content: "msg2"})
	rec2.close()

	// Read and verify two lines
	content, err := os.ReadFile(filepath.Join(tmpDir, "sess-vwx", "subagent", "sub-append.jsonl"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	assert.Len(t, lines, 2, "should have two lines after append")
}
