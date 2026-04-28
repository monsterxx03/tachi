package tui

import (
	"github.com/monsterxx03/tachi/agent"
)

type agentEventMsg agent.AgentEvent

type streamDoneMsg struct{}

// mcpStatusMsg carries an async status message from MCP connect/reconnect operations
// to be displayed in the chat view from within the TUI update loop.
type mcpStatusMsg struct {
	content string
}
