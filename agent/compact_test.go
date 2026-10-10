package agent

import (
	"testing"

	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompactRunOptions: both halves, because each covers a different failure. Hidden tools keep
// the reply a summary rather than a tool call; noAutoCompact is what stops the loop from compacting
// the compact run itself — which swallowed the instruction and left a frontend storing a
// continuation as "the summary" (see CompactRunOptions).
func TestCompactRunOptions(t *testing.T) {
	params := applyRunOptions(CompactRunOptions())
	require.NotNil(t, params)
	assert.True(t, params.restrict, "a compact run must not see tools")
	assert.Empty(t, params.allow, "a compact run must see no registry tools")
	assert.True(t, params.noAutoCompact, "a compact run must not be auto-compacted")

	// The option is reachable through the pair ONLY: a run that hides tools for its own reasons (the
	// node's managed session) must keep the auto-compaction it may still need.
	only := applyRunOptions([]RunOption{WithNoTools()})
	require.NotNil(t, only)
	assert.False(t, only.noAutoCompact, "WithNoTools alone must leave auto-compaction on")
}

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

func TestFinalizeCompactTellsTheHostTheConversationMoved(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewFileStore(dir)
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	oldSess, err := sm.New("anthropic", "/test/dir")
	require.NoError(t, err)
	oldSess.Title = "帮我重构用户模块"
	oldSess.ThreadID = "thread-1" // a binding the migration moves to the child
	require.NoError(t, sm.UpdateMeta(oldSess))

	// The host's view, as it is at the moment of the notification. What matters is the ORDER:
	// the new session is already the conversation (current, meta written) and the old one has
	// not been unwound yet, because the host protects the new session before anything releases
	// the old one (design §9.4).
	var notifiedOld, notifiedNew string
	var sawNewCurrent, sawNewMeta, oldStillBound bool
	sm.SetSessionSwitchHook(func(o, n *session.Session) {
		notifiedOld, notifiedNew = o.ID, n.ID
		sawNewCurrent = sm.Current() != nil && sm.Current().ID == n.ID
		if loaded, err := store.LoadMeta(n.ID); err == nil {
			sawNewMeta = loaded.Title == o.Title
		}
		if loaded, err := store.LoadMeta(o.ID); err == nil {
			oldStillBound = loaded.ThreadID == "thread-1"
		}
	})

	_, err = FinalizeCompact(sm, "system prompt", "summary")
	require.NoError(t, err)

	require.Equal(t, oldSess.ID, notifiedOld, "the host must be told which session was left")
	require.Equal(t, sm.Current().ID, notifiedNew, "…and which one it moved to")
	require.NotEqual(t, oldSess.ID, notifiedNew)
	require.True(t, sawNewCurrent, "the child has to be current when the host hears about it")
	require.True(t, sawNewMeta, "…and its meta written, or the host would protect a half-made session")
	require.True(t, oldStillBound,
		"the old session's bindings are released AFTER the notification, so the host can move "+
			"them rather than race their removal")

	// And a manager nobody installed a hook on simply says nothing (the ordinary case).
	plain := session.NewManagerWithStore(store, nil)
	require.NotPanics(t, func() { plain.NotifySessionSwitch(oldSess, oldSess) })
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

// TestFinalizeCompact_PreservesProjectID: the desktop resolves a member session's
// roots from its PROJECT, and the record's WorkingDir/AdditionalDirs are only the
// snapshot taken when the session was created. A compacted child that lost the id
// would therefore NOT look rootless: it would read that snapshot as its own roots,
// so tools, prompt and checkpoints would agree with each other in the tree the
// project has since moved away from — silently, with a normal-looking conversation.
func TestFinalizeCompact_PreservesProjectID(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	oldSess, err := sm.New("anthropic", "/my/snapshot")
	require.NoError(t, err)
	oldSess.ProjectID = "project-42"
	require.NoError(t, sm.UpdateMeta(oldSess))

	_, err = FinalizeCompact(sm, "prompt", "summary")
	require.NoError(t, err)

	newSess := sm.Current()
	assert.Equal(t, "project-42", newSess.ProjectID)

	// On disk too: the next process must resolve the same project from meta.json.
	loaded, err := store.LoadMeta(newSess.ID)
	require.NoError(t, err)
	assert.Equal(t, "project-42", loaded.ProjectID)
}

// TestFinalizeCompact_MovesThePin: a compacted conversation keeps its pin, and the record it
// came from gives it up. The sidebar's row for a chain is the CHILD (the parent folds away as
// 「压缩前」), so a pin copied onto the child while the parent kept its own would leave the
// user's 置顶 apparently working while a hidden ancestor stayed protected from cleanup
// forever — and unpinning the row would not lift it off the ancestor. One pin, one record:
// the conversation moved, so the pin moves with it.
func TestFinalizeCompact_MovesThePin(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	oldSess, err := sm.New("anthropic", "/my/project")
	require.NoError(t, err)
	oldSess.Pinned = true
	require.NoError(t, sm.UpdateMeta(oldSess))

	_, err = FinalizeCompact(sm, "prompt", "summary")
	require.NoError(t, err)

	newSess := sm.Current()
	require.NotEqual(t, oldSess.ID, newSess.ID)
	assert.True(t, newSess.Pinned, "the compacted child should carry the pin")

	// On disk, both halves: the child keeps it (the next launch reads meta.json, not memory)
	// and the parent has released it.
	loadedChild, err := store.LoadMeta(newSess.ID)
	require.NoError(t, err)
	assert.True(t, loadedChild.Pinned, "the child's pin was not persisted")
	loadedParent, err := store.LoadMeta(oldSess.ID)
	require.NoError(t, err)
	assert.False(t, loadedParent.Pinned, "the parent kept a pin it no longer means anything on")
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
