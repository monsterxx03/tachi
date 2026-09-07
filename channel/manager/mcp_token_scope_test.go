package manager

import (
	"context"
	"sync"
	"testing"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockMCPTokenChannel embeds mockChannel (satisfying channel.Channel) and
// implements channel.MCPTokenUserChannel with canned key/ok values.
type mockMCPTokenChannel struct {
	mockChannel
	key string
	ok  bool
}

func (c *mockMCPTokenChannel) MCPTokenKey(msg channel.IncomingMessage) (string, bool) {
	return c.key, c.ok
}

// newScopedManager returns a Manager with just enough state for
// buildHandlerForChannel (thread-channel tracking map + lock).
func newScopedManager() *Manager {
	return &Manager{
		threadChannels:  make(map[string]channel.Channel),
		threadChannelMu: sync.RWMutex{},
	}
}

func TestBuildHandlerForChannel_InjectsUserTokenKey(t *testing.T) {
	m := newScopedManager()
	ch := &mockMCPTokenChannel{key: "san.zhang", ok: true}

	var gotKey string
	base := func(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		gotKey = mcp.MCPTokenUserFromCtx(ctx)
		return channel.HandlerResult{}
	}

	handler := m.buildHandlerForChannel(ch, base)
	handler(context.Background(), channel.IncomingMessage{ThreadID: "t1"})

	assert.Equal(t, "san.zhang", gotKey, "per-user MCP token key must be tagged onto the turn ctx")
}

func TestBuildHandlerForChannel_NoKeyWithoutUser(t *testing.T) {
	m := newScopedManager()
	ch := &mockMCPTokenChannel{key: "", ok: false}

	base := func(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		assert.Equal(t, "", mcp.MCPTokenUserFromCtx(ctx), "ctx must stay untagged when the channel reports no user")
		return channel.HandlerResult{}
	}

	handler := m.buildHandlerForChannel(ch, base)
	handler(context.Background(), channel.IncomingMessage{ThreadID: "t1"})
}

func TestBuildHandlerForChannel_UntaggedForPlainChannel(t *testing.T) {
	m := newScopedManager()
	ch := &mockChannel{name: "plain"} // does NOT implement MCPTokenUserChannel

	base := func(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		assert.Equal(t, "", mcp.MCPTokenUserFromCtx(ctx), "legacy channels must keep untagged ctx")
		return channel.HandlerResult{}
	}

	handler := m.buildHandlerForChannel(ch, base)
	result := handler(context.Background(), channel.IncomingMessage{ThreadID: "t1"})
	require.False(t, result.Steered) // sanity: base ran to completion
}
