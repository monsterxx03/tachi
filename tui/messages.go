package tui

import (
	"github.com/monsterxx03/tachi/agent"
)

// agentEventMsg wraps an agent event with a stream generation counter.
// The generation is checked on receipt to ignore events from stale streams.
type agentEventMsg struct {
	event agent.AgentEvent
	gen   int
}

// streamDoneMsg signals that a stream's event channel has closed.
// The generation is checked to avoid acting on a previous stream's close.
type streamDoneMsg struct {
	gen int
}

// mcpStatusMsg carries an async status message from MCP connect/reconnect operations
// to be displayed in the chat view from within the TUI update loop.
// When nextCh is non-nil, the handler returns a command to read the next message
// from it, allowing a goroutine to send multiple status updates over time.
type mcpStatusMsg struct {
	content string
	nextCh  <-chan string
}

// mcpProfileSwitchedMsg signals that an MCP profile switch goroutine has
// finished. The switch replaced config.MCPServers, so the Model must re-sync
// its mcpServers snapshot from the shared config — done here in the update
// loop (never from the goroutine) to avoid data races with rendering.
type mcpProfileSwitchedMsg struct{}

// dreamStatusMsg carries an async status message from AutoDream execution
// to be displayed in the chat view from within the TUI update loop.
// Mirrors mcpStatusMsg — the generic channel→chatview pattern.
type dreamStatusMsg struct {
	content string
	nextCh  <-chan string
}

// researchStatusMsg carries an async status/progress message from Deep
// Research execution to be displayed in the chat view. Follows the same
// generic channel→chatview pattern as dreamStatusMsg.
type researchStatusMsg struct {
	content string
	nextCh  <-chan string
}

// researchDoneMsg signals that deep research has completed, so the model
// can reset isResearching and allow user input again.
type researchDoneMsg struct{}
