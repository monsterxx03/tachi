package agent

import (
	"context"
	"fmt"

	"github.com/monsterxx03/tachi/agent/subagent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// --- Subagent configuration accessors (implements subagent.Agent interface) ---

// SubagentRunner returns the tools.SubagentRunner used by the SubAgent tool.
func (a *AIAgent) SubagentRunner() tools.SubagentRunner {
	return a.Config.SubagentRunner
}

// SubagentProvider returns the sub-agent provider or falls back to main.
func (a *AIAgent) SubagentProvider() llm.Provider {
	if a.Config.SubagentProvider != nil {
		return a.Config.SubagentProvider
	}
	return a.Config.Provider
}

// NewChildAgent creates a fully configured child agent backed by RunOneOffStream.
// Implements the subagent.Agent interface.
func (a *AIAgent) NewChildAgent(
	logger *logger.Logger,
	provider llm.Provider,
	maxIterations int,
	allowedTools []string,
	subagentSessionID string,
) subagent.ChildAgent {
	return &childAdapter{
		parent:        a,
		childProvider: provider,
		maxIterations: maxIterations,
		allowedTools:  allowedTools,
		sessionID:     subagentSessionID,
		logger:        logger,
	}
}

// childAdapter implements subagent.ChildAgent by creating a child AIAgent
// and delegating to its RunOneOffStream. This keeps the subagent package
// independent of the agent package's internal types.
type childAdapter struct {
	parent        *AIAgent
	childProvider llm.Provider
	maxIterations int
	allowedTools  []string
	sessionID     string
	logger        *logger.Logger
}

func (c *childAdapter) Run(
	ctx context.Context,
	provider llm.Provider,
	systemPrompt, userPrompt string,
	opts llm.ChatOptions,
) <-chan subagent.StreamEvent {
	out := make(chan subagent.StreamEvent, 64)

	go func() {
		defer close(out)

		forked := c.parent.Fork(ForkConfig{
			Provider:      c.childProvider,
			MaxIterations: c.maxIterations,
			AllowedTools:  c.allowedTools,
			Logger:        c.logger,
			SessionID:     c.sessionID,
		})
		defer forked.Close()

		child := forked.Agent()

		if opts.MaxTokens <= 0 {
			opts.MaxTokens = DefaultMaxTokens
		}

		// Run via RunOneOffStream — we consume parent agent events and
		// translate them to the subagent-local StreamEvent type.
		// No one-off meta: sub-agents keep their own dedicated recorder
		// (agent/subagent/recorder.go); the one-off sidecar stays off here.
		ch := child.RunOneOffStream(ctx, provider, systemPrompt, userPrompt, opts)
		for event := range ch {
			out <- translateEvent(event)
		}
	}()

	return out
}

// translateEvent converts an AgentEvent to a subagent.StreamEvent.
func translateEvent(e AgentEvent) subagent.StreamEvent {
	se := subagent.StreamEvent{Usage: e.Usage}
	switch e.Type {
	case AgentEventTextDelta:
		se.Type = subagent.StreamEventTextDelta
		se.TextDelta = e.TextDelta
	case AgentEventThinkingDelta:
		se.Type = subagent.StreamEventThinkingDelta
		se.ThinkingDelta = e.ThinkingDelta
	case AgentEventToolCallArgs:
		se.Type = subagent.StreamEventToolCallArgs
		se.ToolName = e.ToolName
		se.ToolArgs = e.ToolArgs
		se.ToolID = e.ToolID
	case AgentEventToolResult:
		se.Type = subagent.StreamEventToolResult
		se.ToolName = e.ToolName
		se.ToolResult = e.ToolResult
		se.ToolIsError = e.ToolIsError
		se.ToolID = e.ToolID
		se.ToolDuration = e.ToolDuration
	case AgentEventTurnComplete:
		se.Type = subagent.StreamEventTurnComplete
	case AgentEventError:
		se.Type = subagent.StreamEventError
		se.Error = e.Result.Error
	}
	return se
}

// lazyRegisterMCPTool ensures an MCP tool is in the Registry before invocation.
// When ToolSearch is active, non-auto-loaded MCP tools stay only in the
// deferred pool until first use. This method bridges: if the tool is an MCP
// tool and not yet in the Registry, it registers the DeferredTool.Tool instance.
// Returns nil if the tool is already registered or is not an MCP tool.
func (a *AIAgent) lazyRegisterMCPTool(name string) error {
	pool := a.DeferredPool()
	if pool == nil || !tools.IsMCPSchema(name) {
		return nil
	}
	// Already registered
	if a.Config.ToolRegistry.GetTool(name) != nil {
		return nil
	}
	deferredTool := pool.Get(name)
	if deferredTool == nil || deferredTool.Tool == nil {
		return fmt.Errorf("deferred MCP tool %q not found", name)
	}
	a.Config.ToolRegistry.Register(deferredTool.Tool)
	return nil
}

// SetupSubagentProvider resolves and creates a dedicated LLM provider for
// sub-agent execution from config. Falls back to main provider when not set.
func (a *AIAgent) SetupSubagentProvider(cfg *config.Config) {
	// Resolve dedicated subagent provider if configured.
	a.setupDedicatedProvider(cfg, "subagent", cfg.Subagent.Provider, func(p llm.Provider) {
		a.Config.SubagentProvider = p
	})
}

// newSubagentEventSink creates a SubagentEventSink that forwards subagent
// internal tool call events to the parent agent's event channel, enabling
// real-time display in the TUI.
func (a *AIAgent) newSubagentEventSink(ch chan<- AgentEvent, parentToolID string) tools.SubagentEventSink {
	return &agentSubagentEventSink{ch: ch, parentToolID: parentToolID}
}

// agentSubagentEventSink implements tools.SubagentEventSink.
type agentSubagentEventSink struct {
	ch           chan<- AgentEvent
	parentToolID string
}

func (s *agentSubagentEventSink) SendToolCallEvent(toolName, args string) {
	select {
	case s.ch <- AgentEvent{
		Type:             AgentEventSubagentToolCall,
		ToolID:           s.parentToolID,
		ToolArgs:         args,
		SubagentToolName: toolName,
	}:
	default:
	}
}

func (s *agentSubagentEventSink) SendToolResultEvent(toolName, result string, isError bool) {
	select {
	case s.ch <- AgentEvent{
		Type:             AgentEventSubagentToolCall,
		ToolID:           s.parentToolID,
		ToolResult:       result,
		ToolIsError:      isError,
		SubagentToolName: toolName,
		SubagentToolDone: true,
	}:
	default:
	}
}
