package tui

import (
	"github.com/monsterxx03/tachi/agent"
)

type agentEventMsg agent.AgentEvent

type streamDoneMsg struct{}

// mcpStatusMsg carries an async status message from MCP connect/reconnect operations
// to be displayed in the chat view from within the TUI update loop.
// When nextCh is non-nil, the handler returns a command to read the next message
// from it, allowing a goroutine to send multiple status updates over time.
type mcpStatusMsg struct {
	content string
	nextCh  <-chan string
}
