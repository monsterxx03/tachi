// Package subagent provides sub-agent execution, worktree isolation, and
// execution recording. It defines the Agent interface that the parent agent
// must implement, and the ChildAgent interface for running child agents.
package subagent

import (
	"context"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// Agent is the interface SubagentExecutor needs from its parent agent.
// AIAgent in the agent package implements this.
type Agent interface {
	// Subagent provider resolution (with fallback to main)
	SubagentProvider() llm.Provider

	// Shared services
	SessionManager() *session.Manager
	Logger() *logger.Logger

	// Tool registry — used to copy tools from parent to child agents
	ToolNames() []string
	GetTool(name string) tools.Tool

	// NewChildAgent creates a fully configured child agent with the given
	// logger, provider, iteration budget, and allowed tool set.
	NewChildAgent(logger *logger.Logger, provider llm.Provider,
		maxIterations int, allowedTools []string, subagentSessionID string) ChildAgent
}

// ChildAgent represents a configured but not-yet-started child agent.
type ChildAgent interface {
	// Run executes the child agent and returns a stream of events.
	Run(ctx context.Context, provider llm.Provider,
		systemPrompt, userPrompt string, opts llm.ChatOptions) <-chan StreamEvent
}

// StreamEventType mirrors agent.AgentEvent types for internal use within the
// subagent package. This avoids a circular dependency on the agent package.
type StreamEventType string

const (
	StreamEventTextDelta     StreamEventType = "text_delta"
	StreamEventThinkingDelta StreamEventType = "thinking_delta"
	StreamEventToolCallArgs  StreamEventType = "tool_call_args"
	StreamEventToolResult    StreamEventType = "tool_result"
	StreamEventTurnComplete  StreamEventType = "turn_complete"
	StreamEventError         StreamEventType = "error"
)

// StreamEvent is a local mirror of agent.AgentEvent, providing the subset of
// fields needed to consume child agent output within the subagent package.
type StreamEvent struct {
	Type          StreamEventType
	TextDelta     string
	ThinkingDelta string
	ToolName      string
	ToolArgs      string
	ToolID        string
	ToolResult    string
	ToolIsError   bool
	ToolDuration  time.Duration
	Error         error
	Usage         *llm.Usage
}
