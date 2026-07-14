package agent

import (
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// forkTool creates a suitable copy of a tool for a child agent.
// Most tools can be shared by pointer, but tools with per-instance
// mutable state (like ReadTool's file cache) need a fresh instance
// to prevent subagent reads from polluting the parent's cache.
func forkTool(t tools.Tool) tools.Tool {
	if rt, ok := t.(*tools.ReadTool); ok {
		_ = rt // we just need a fresh instance with empty cache
		return tools.NewReadTool()
	}
	return t
}

// ForkConfig controls child agent creation from a parent AIAgent.
type ForkConfig struct {
	Provider      llm.Provider     // required — LLM provider
	MaxIterations int              // 0 = unlimited
	MaxTokens     int              // 0 = default (4096)
	AllowedTools  []string         // empty = copy all parent tools
	NoMCP         bool             // true = don't inherit shared MCP Manager
	Logger        *debuglog.Logger // nil = use parent logger
	SessionID     string           // logging hint
}

// ForkedAgent wraps a restricted child AIAgent created by Fork().
// Close() only cleans up the child's own resources — it does NOT
// kill shared ProcessManager or close shared MCP Manager, which
// remain owned by the parent.
type ForkedAgent struct {
	agent     *AIAgent
	sharedPM  *tools.ProcessManager
	sharedMCP *mcp.Manager
}

// Agent returns the internal AIAgent for configuration before running.
func (f *ForkedAgent) Agent() *AIAgent {
	return f.agent
}

// Close cleans up the child agent's own resources. It does NOT kill
// the shared ProcessManager or close the shared MCP Manager.
// Safe to call multiple times.
func (f *ForkedAgent) Close() {
	if f.agent == nil {
		return
	}
	f.agent.ClearToolRegistry()
	f.agent = nil
}

// Fork creates a restricted child AIAgent from the parent.
//
// The child shares the parent's MCP Manager and ProcessManager
// (Close on the child skips them), has its own tool registry
// filtered by AllowedTools, and does NOT inherit session manager,
// memory, LSP manager, or reminder collector.
func (a *AIAgent) Fork(cfg ForkConfig) *ForkedAgent {
	child := NewAIAgent(cfg.Provider, cfg.MaxIterations)

	if cfg.Logger != nil {
		child.SetLogger(cfg.Logger)
	} else {
		child.SetLogger(a.logger)
	}

	// Register allowed tools from parent.
	if len(cfg.AllowedTools) == 0 {
		// No whitelist — copy all parent tools.
		for _, name := range a.toolRegistry.GetToolNames() {
			if tool := a.toolRegistry.GetTool(name); tool != nil {
				child.toolRegistry.Register(forkTool(tool))
			}
		}
	} else {
		// Whitelist mode.
		allowSet := make(map[string]bool, len(cfg.AllowedTools))
		for _, name := range cfg.AllowedTools {
			allowSet[name] = true
		}
		for _, name := range a.toolRegistry.GetToolNames() {
			if allowSet[name] {
				if tool := a.toolRegistry.GetTool(name); tool != nil {
					child.toolRegistry.Register(forkTool(tool))
				}
			}
		}
	}

	child.SetSkipEditConfirm(true)
	child.SetReminderCollector(nil)

	// Share heavy resources with parent — Close won't tear them down.
	// Skip MCP sharing when NoMCP is set (e.g. ambient turns that
	// should only use the whitelisted tools without MCP).
	child.SetProcessManager(a.processManager)
	if a.mcpManager != nil && !cfg.NoMCP {
		child.SetSharedMCP(a.mcpManager)
	}

	return &ForkedAgent{
		agent:     child,
		sharedPM:  a.processManager,
		sharedMCP: a.mcpManager,
	}
}

// LoadSessionHistory loads all messages from the current session and
// converts them to LLM message format. Returns nil history when there
// is no active session or the session has no messages.
func (a *AIAgent) LoadSessionHistory() ([]llm.Message, error) {
	sm := a.SessionManager()
	if sm == nil {
		return nil, nil
	}

	sess := sm.Current()
	if sess == nil {
		return nil, nil
	}

	msgs, err := sm.LoadMessages()
	if err != nil {
		return nil, err
	}

	if len(msgs) == 0 {
		return nil, nil
	}

	return ConvertSessionToLLMMessages(msgs, a.Provider().Name())
}
