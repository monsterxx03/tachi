package manager

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetThreadWorkDir_SessionFallback verifies that getThreadWorkDir falls
// back to the thread's persisted session WorkingDir when the agent cache has
// no entry for the thread (e.g. right after /new evicted the agent). State-
// less commands like /sh and announcement updates must observe the configured
// directory instead of the process CWD.
func TestGetThreadWorkDir_SessionFallback(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Type:          "openai",
		Model:         "test-model",
		ContextWindow: 128_000,
		MaxTokens:     4096,
	}
	mgr.defaultResolvedProvider.Provider = &mockProvider{name: "mock"}

	threadID := fmt.Sprintf("wd-%s-%d", t.Name(), time.Now().UnixNano())

	// No cache entry and no session yet → empty.
	assert.Equal(t, "", mgr.getThreadWorkDir(threadID))

	// Create a session for the thread and persist a working directory
	// (as /new does with announcement defaults).
	sm, _, err := mgr.loadThreadSession(threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)
	sess := sm.Current()
	require.NotNil(t, sess)
	sess.WorkingDir = "/configured/dir"
	require.NoError(t, sm.UpdateMeta(sess))

	// Cache still has no entry → fall back to the session's workdir.
	assert.Equal(t, "/configured/dir", mgr.getThreadWorkDir(threadID))

	// Once a cached agent exists, its workDir wins over the session.
	mgr.agentCacheMu.Lock()
	mgr.agentCache[threadID] = &cachedAgent{workDir: "/cached/dir"}
	mgr.agentCacheMu.Unlock()
	assert.Equal(t, "/cached/dir", mgr.getThreadWorkDir(threadID))
}

// mockReminderChannel embeds mockChannel and adds the ThreadReminderChannel
// methods with a canned reminder value (empty = nothing to inject).
type mockReminderChannel struct {
	mockChannel
	text string
}

func (m *mockReminderChannel) ThreadReminder(ctx context.Context, threadID string) (string, bool) {
	if m.text == "" {
		return "", false
	}
	return m.text, true
}

// TestBuildAgent_ThreadReminderChannelRegisters verifies that a channel
// implementing channel.ThreadReminderChannel gets its reminder registered
// into the thread agent's reminder collector, so it surfaces in the unified
// <system-reminder> block at the user-message boundary — and stays out of
// tool-result boundary collections.
func TestBuildAgent_ThreadReminderChannelRegisters(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)

	mgr.setThreadChannel(threadID, &mockReminderChannel{
		mockChannel: mockChannel{name: "wave"},
		text:        "Group announcement reminder: 群规：先搜后问",
	})

	a, err := mgr.buildAgent(context.Background(), threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)
	defer a.Close()

	require.NotNil(t, a.Config.ReminderCollector)

	block := a.Config.ReminderCollector.Collect(context.Background(), systemreminder.Context{})
	assert.Contains(t, block, "Group announcement reminder: 群规：先搜后问",
		"channel reminder should ride in the unified <system-reminder> block")
	assert.Contains(t, block, "<system-reminder>")

	toolBlock := a.Config.ReminderCollector.Collect(context.Background(), systemreminder.Context{IsToolResult: true})
	assert.NotContains(t, toolBlock, "Group announcement reminder",
		"channel reminder must not repeat at tool-result boundaries")
}

// TestBuildAgent_PlainChannelNoThreadReminder verifies channels that don't
// implement ThreadReminderChannel leave no channel reminder in the block.
func TestBuildAgent_PlainChannelNoThreadReminder(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)

	mgr.setThreadChannel(threadID, &mockChannel{name: "plain"})

	a, err := mgr.buildAgent(context.Background(), threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)
	defer a.Close()

	block := a.Config.ReminderCollector.Collect(context.Background(), systemreminder.Context{})
	assert.NotContains(t, block, "Group announcement reminder")
}
