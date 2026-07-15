package agent

import (
	"testing"

	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCompactHistory_Structure(t *testing.T) {
	systemPrompt := "You are Tachi."
	summary := "用户希望重构用户模块。已完成注册功能迁移。"

	history := buildCompactHistory(systemPrompt, summary)
	require.Len(t, history, 3)

	assert.Equal(t, "system", history[0].Role)
	assert.Equal(t, systemPrompt, history[0].Content)

	assert.Equal(t, "assistant", history[1].Role)
	assert.Contains(t, history[1].Content, summary)
	assert.Contains(t, history[1].Content, "历史摘要")

	assert.Equal(t, "user", history[2].Role)
	assert.Contains(t, history[2].Content, "请基于以上摘要继续对话")
}

func TestFinalizeCompact_NoActiveSession(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	_, err = FinalizeCompact(sm, "system prompt", "summary")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no active session")
}

func TestFinalizeCompact_CreatesNewSession(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	// Create an old session
	oldSess, err := sm.New("anthropic", "/test/dir")
	require.NoError(t, err)
	oldSess.Title = "帮我重构用户模块"
	require.NoError(t, sm.UpdateMeta(oldSess))

	// Store old session ID for later comparison
	oldID := oldSess.ID

	// Finalize compact
	systemPrompt := "You are Tachi."
	summary := "用户希望重构用户模块。已完成注册功能迁移。"

	newHistory, err := FinalizeCompact(sm, systemPrompt, summary)
	require.NoError(t, err)

	// Verify new session exists and has correct fields
	newSess := sm.Current()
	require.NotNil(t, newSess)
	assert.NotEqual(t, oldID, newSess.ID, "new session should have different ID")
	assert.Equal(t, oldID, newSess.CompactedParentID)
	assert.Equal(t, oldSess.Title, newSess.CompactedParentTitle)
	assert.Equal(t, oldSess.Title, newSess.Title, "title should be inherited")

	// Verify old session was updated
	loadedOld, err := store.LoadMeta(oldID)
	require.NoError(t, err)
	assert.Equal(t, newSess.ID, loadedOld.CompactedChildID)

	// Verify messages were written to new session
	msgs, err := store.LoadMessages(newSess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, session.MessageTypeAssistant, msgs[0].Type)
	assert.Contains(t, msgs[0].Content, summary)
	assert.Equal(t, session.MessageTypeUser, msgs[1].Type)
	assert.Contains(t, msgs[1].Content, "请基于以上摘要继续对话")

	// Verify returned history
	require.Len(t, newHistory, 3)
	assert.Equal(t, "system", newHistory[0].Role)
	assert.Equal(t, systemPrompt, newHistory[0].Content)
	assert.Equal(t, "assistant", newHistory[1].Role)
	assert.Contains(t, newHistory[1].Content, summary)
	assert.Equal(t, "user", newHistory[2].Role)
	assert.Contains(t, newHistory[2].Content, "请基于以上摘要继续对话")
}

func TestFinalizeCompact_PreservesProviderModelWorkingDir(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	oldSess, err := sm.New("openai", "/my/project")
	require.NoError(t, err)

	_, err = FinalizeCompact(sm, "prompt", "summary")
	require.NoError(t, err)

	newSess := sm.Current()
	assert.Equal(t, oldSess.ProviderName, newSess.ProviderName)
	assert.Equal(t, oldSess.WorkingDir, newSess.WorkingDir)
}

func TestDrainCompactEvents_ReturnsResponseText(t *testing.T) {
	ch := make(chan AgentEvent, 10)

	ch <- AgentEvent{Type: AgentEventTextDelta, TextDelta: "Hello "}
	ch <- AgentEvent{Type: AgentEventTextDelta, TextDelta: "World"}
	ch <- AgentEvent{
		Type: AgentEventTurnComplete,
		Result: &RunResult{
			Response: "Hello World",
		},
	}
	close(ch)

	result, err := DrainCompactEvents(ch)
	assert.NoError(t, err)
	assert.Equal(t, "Hello World", result)
}

func TestDrainCompactEvents_ReturnsError(t *testing.T) {
	ch := make(chan AgentEvent, 10)

	ch <- AgentEvent{Type: AgentEventTextDelta, TextDelta: "Partial "}
	ch <- AgentEvent{
		Type: AgentEventError,
		Result: &RunResult{
			Response: "Partial ",
			Error:    assert.AnError,
		},
	}
	close(ch)

	// When there's partial text AND an error, DrainCompactEvents returns the
	// text preferentially (preserving useful output) and drops the error.
	result, err := DrainCompactEvents(ch)
	assert.NoError(t, err)
	assert.Equal(t, "Partial", result)
}

func TestDrainCompactEvents_ErrorOnly(t *testing.T) {
	ch := make(chan AgentEvent, 10)

	ch <- AgentEvent{
		Type: AgentEventError,
		Result: &RunResult{
			Error: assert.AnError,
		},
	}
	close(ch)

	result, err := DrainCompactEvents(ch)
	assert.Error(t, err)
	assert.Equal(t, "", result)
}

func TestDrainCompactEvents_IgnoresToolAndThinkingEvents(t *testing.T) {
	ch := make(chan AgentEvent, 10)

	ch <- AgentEvent{Type: AgentEventThinkingDelta, ThinkingDelta: "thinking..."}
	ch <- AgentEvent{Type: AgentEventToolCallStart, ToolName: "ReadFile", ToolID: "tc1"}
	ch <- AgentEvent{Type: AgentEventToolCallArgs, ToolName: "ReadFile", ToolArgs: `{"path":"test"}`}
	ch <- AgentEvent{Type: AgentEventToolResult, ToolName: "ReadFile", ToolResult: "file content", ToolID: "tc1"}
	ch <- AgentEvent{
		Type: AgentEventTurnComplete,
		Result: &RunResult{
			Response: "Final result",
		},
	}
	close(ch)

	result, err := DrainCompactEvents(ch)
	assert.NoError(t, err)
	assert.Equal(t, "Final result", result)
}

func TestDrainCompactEvents_Empty(t *testing.T) {
	ch := make(chan AgentEvent)
	close(ch)

	result, err := DrainCompactEvents(ch)
	assert.NoError(t, err)
	assert.Equal(t, "", result)
}
