package manager

import (
	"context"
	"testing"

	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockInteractiveChannel embeds mockChannel (satisfying channel.Channel) and
// adds the InteractiveChannel methods, simulating an interactive channel
// (e.g. Discord with native AskUserQuestion UI).
type mockInteractiveChannel struct {
	mockChannel
}

func (m *mockInteractiveChannel) Interactive() bool { return true }

func (m *mockInteractiveChannel) PresentQuestions(ctx context.Context, threadID, replyID string, questions []channel.Question) error {
	return nil
}

// TestBuildAgent_InteractiveChannelRegistersAskUser guards against the
// regression where channel-mode agents had AskUserQuestion unregistered at
// construction (PermissionModeSkip) and the interactive branch only logged
// "thread is interactive" without re-registering the tool — leaving the LLM
// unable to call it even on interactive channels.
func TestBuildAgent_InteractiveChannelRegistersAskUser(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)

	mgr.setThreadChannel(threadID, &mockInteractiveChannel{})

	a, err := mgr.buildAgent(context.Background(), threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)
	defer a.Close()

	assert.Contains(t, a.ToolNames(), agenttools.ToolNameAskUser,
		"interactive channel should keep AskUserQuestion registered")
}

// TestBuildAgent_NonInteractiveChannelUnregistersAskUser verifies the
// default behavior for plain channels (weixin/github): AskUserQuestion is
// unregistered so the LLM never tries to use it.
func TestBuildAgent_NonInteractiveChannelUnregistersAskUser(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)

	mgr.setThreadChannel(threadID, &mockChannel{name: "plain"})

	a, err := mgr.buildAgent(context.Background(), threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)
	defer a.Close()

	assert.NotContains(t, a.ToolNames(), agenttools.ToolNameAskUser)
}

// TestBuildAgent_UnknownThreadUnregistersAskUser covers the safety default:
// a thread with no registered channel must not accidentally keep AskUser
// registered (it would render no UI and hang the turn).
func TestBuildAgent_UnknownThreadUnregistersAskUser(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)
	// No setThreadChannel on purpose.

	a, err := mgr.buildAgent(context.Background(), threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)
	defer a.Close()

	assert.NotContains(t, a.ToolNames(), agenttools.ToolNameAskUser)
}