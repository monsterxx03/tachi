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

// TestFinalizeCompact_PreservesAdditionalRoots: sm.New carries only the provider and
// the PRIMARY directory, so every other workspace field is copied by hand in
// FinalizeCompact — and a field missing from that block is silently zeroed rather than
// reported. Additional roots are extra writable trees the user added; dropped here,
// the compacted session keeps working in them (its tools and @-references do not know
// they were forgotten) while its own record no longer admits they exist, and a
// checkpoint no longer covers them.
func TestFinalizeCompact_PreservesAdditionalRoots(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	oldSess, err := sm.New("anthropic", "/my/project")
	require.NoError(t, err)
	extra := []string{"/my/shared-lib", "/my/other-tree"}
	oldSess.AdditionalDirs = append([]string(nil), extra...)
	require.NoError(t, sm.UpdateMeta(oldSess))

	_, err = FinalizeCompact(sm, "prompt", "summary")
	require.NoError(t, err)

	newSess := sm.Current()
	assert.Equal(t, extra, newSess.AdditionalDirs)

	// And the copy is on disk too, not only on the struct the manager handed back.
	loaded, err := store.LoadMeta(newSess.ID)
	require.NoError(t, err)
	assert.Equal(t, extra, loaded.AdditionalDirs)

	// The compacted session owns its own slice: mutating it must not reach back into
	// the parent's record.
	newSess.AdditionalDirs[0] = "/tmp/mutated"
	assert.Equal(t, extra[0], oldSess.AdditionalDirs[0])
}

// TestFinalizeCompact_MigratesThreadID guards against repeated auto-compact
// on channel threads: after a compaction the new session must own the thread
// binding and the old session must release it, so FindByThreadID (channel
// mode's session lookup) resolves to the compacted session — not the
// pre-compact one with its full history. Without this migration, a thread's
// next turn reloads the full history and auto-compacts again right after a
// compaction reset the context.
func TestFinalizeCompact_MigratesThreadID(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	oldSess, err := sm.New("openai", "/my/project")
	require.NoError(t, err)
	sm.SetThreadID("discord:12345")

	_, err = FinalizeCompact(sm, "prompt", "summary")
	require.NoError(t, err)

	// The NEW session owns the thread binding.
	newSess := sm.Current()
	require.NotNil(t, newSess)
	assert.NotEqual(t, oldSess.ID, newSess.ID)
	assert.Equal(t, "discord:12345", newSess.ThreadID, "new session should inherit the thread binding")

	// FindByThreadID must resolve to the new session (the compacted one).
	found, err := sm.FindByThreadID("discord:12345")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, newSess.ID, found.ID, "FindByThreadID should hit the new session")

	// The old session releases the binding, so cleanup can reclaim it and
	// lookups are unambiguous even if list ordering ever changes.
	oldLoaded, err := store.LoadMeta(oldSess.ID)
	require.NoError(t, err)
	assert.Equal(t, "", oldLoaded.ThreadID, "old session should release the thread binding")
	assert.Equal(t, newSess.ID, oldLoaded.CompactedChildID)
}

// TestFinalizeCompact_NoThreadIDNoop ensures the migration is a no-op when the
// old session has no thread binding (TUI/one-off sessions).
func TestFinalizeCompact_NoThreadIDNoop(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	oldSess, err := sm.New("openai", "/my/project")
	require.NoError(t, err)

	_, err = FinalizeCompact(sm, "prompt", "summary")
	require.NoError(t, err)

	newSess := sm.Current()
	assert.Equal(t, "", newSess.ThreadID)
	oldLoaded, err := store.LoadMeta(oldSess.ID)
	require.NoError(t, err)
	assert.Equal(t, "", oldLoaded.ThreadID)
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
