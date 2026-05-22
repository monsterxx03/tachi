package agent

import (
	"context"
	"time"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
)

const defaultMaxTokens = 4096

type IterationBudget struct {
	Remaining int
	Unlimited bool // When true, consume() always returns true.
	Parent    *IterationBudget
}

func (b *IterationBudget) consume() bool {
	if b.Unlimited {
		return true
	}
	if b.Remaining > 0 {
		b.Remaining--
		return true
	}
	if b.Parent != nil {
		return b.Parent.consume()
	}
	return false
}

// PermissionMode controls how tool confirmation requests are handled.
type PermissionMode int

const (
	// PermissionModeTUI emits events and blocks on confirmRespCh (interactive TUI).
	PermissionModeTUI PermissionMode = iota
	// PermissionModeSkip auto-approves all confirmations (subagent, channel, tachi run).
	PermissionModeSkip
	// PermissionModeExternal delegates to an external handler (ACP).
	PermissionModeExternal
)

// PermissionHandler is called by PermissionModeExternal to decide tool execution.
// It receives the tool name, tool call ID, diff preview, and raw args.
// Returns (approved, error).
type PermissionHandler func(ctx context.Context, toolName, toolID, diff, args string) (bool, error)

type AIAgent struct {
	model              string
	provider           llm.Provider
	maxIterations      int
	toolRegistry       *tools.Registry
	iterationBudget    *IterationBudget
	confirmRespCh      chan bool
	askUserRespCh      chan tools.AskUserResult
	steerRespCh        chan string // TUI → agent: pending input to inject at steer point
	permissionMode     PermissionMode
	permissionHandler  PermissionHandler
	sessionManager     *session.Manager
	reminderCollector  *systemreminder.Collector
	contextWindow      int64
	lastInputTokens    int64
	lastMessageDate    string       // calendar date (2006-01-02) of last processed user message; empty initially
	titleModelProvider llm.Provider // optional: dedicated provider for title generation
	titleGenEnabled    bool         // whether LLM-based title generation is active
	commitProvider     llm.Provider // optional: dedicated provider for /commit messages
	logger             *debuglog.Logger

	// Skill-related fields
	skillStore   *skill.Store
	activeSkills map[string]bool // skills activated in current session

	// Subagent-related fields (implements subagent.Agent interface)
	subagentProvider llm.Provider // sub-agent dedicated provider (nil = fallback to main)
	subagentModel    string       // sub-agent dedicated model ("" = fallback to main)

	// Memory-related fields
	memoryBackend    memory.Backend // nil = memory not enabled
	memoryTimeout    time.Duration  // context deadline for Store/Recall/Forget
	skipMemory       bool           // set by RunOneOffStream to suppress turn-level memory writes
	skipMemoryRecall bool           // set by main.go runAgent to suppress recall for "tachi run"
	excludeRepos     []string       // git repo roots to skip all memory writes

	// MCP ToolSearch fields
	deferredPool  *mcp.DeferredPool  // MCP tools available for search (nil = ToolSearch disabled)
	discoveredSet *mcp.DiscoveredSet // MCP tools discovered by LLM via MCPSearchTools

	// processManager manages background processes started by BashTool.
	// Tied to the agent lifecycle — Close() kills all tracked processes.
	processManager *tools.ProcessManager

	// baseReminders stores the non-skill reminders assembled during Configure.
	// rebuildSkillCollector uses this to re-apply SkillListReminder on reload.
	baseReminders []systemreminder.Reminder
}

func NewAIAgent(provider llm.Provider, model string, maxIterations int) *AIAgent {
	return &AIAgent{
		model:           model,
		provider:        provider,
		maxIterations:   maxIterations,
		titleGenEnabled: true,
		toolRegistry:    tools.NewRegistry(),
		processManager:  tools.NewProcessManager(),
		confirmRespCh:   make(chan bool, 1),
		askUserRespCh:   make(chan tools.AskUserResult, 1),
		logger:          debuglog.DefaultLogger,
		reminderCollector: systemreminder.NewCollector(
			systemreminder.DateReminder{},
			systemreminder.ProjectContextReminder{},
			systemreminder.GitReminder{},
			systemreminder.IterationWarningReminder{Threshold: 5},
			systemreminder.TokenWarningReminder{ThresholdPct: 80},
		),
	}
}

// SetLogger overrides the agent's logger. Channel callers use this to inject
// a channel-specific logger so debug output is tagged with the correct source.
func (a *AIAgent) SetLogger(l *debuglog.Logger) {
	a.logger = l
}

// Logger returns the agent's debug logger.
func (a *AIAgent) Logger() *debuglog.Logger {
	return a.logger
}

// RespondToAskUser is called by TUI to respond to an AskUserQuestion request
func (a *AIAgent) RespondToAskUser(answers map[string]string, annotations map[string]string) {
	select {
	case a.askUserRespCh <- tools.AskUserResult{Answers: answers, Annotations: annotations}:
	default:
		// Channel already has a value or is not waiting
	}
}

// ConfirmTool is called by TUI to respond to a confirmation request
func (a *AIAgent) ConfirmTool(confirmed bool) {
	select {
	case a.confirmRespCh <- confirmed:
	default:
		// Channel already has a value or is not waiting
	}
}

// SetSteerChannel sets the channel used for steer input injection.
// The TUI writes pending user input to this channel at steer points.
func (a *AIAgent) SetSteerChannel(ch chan string) {
	a.steerRespCh = ch
}

func (a *AIAgent) SetProvider(provider llm.Provider, model string) {
	a.provider = provider
	a.model = model
}

// Model returns the current model name.
func (a *AIAgent) Model() string {
	return a.model
}

// SetPermissionMode sets how tool confirmation requests are handled.
func (a *AIAgent) SetPermissionMode(mode PermissionMode) {
	a.permissionMode = mode
}

// SetPermissionHandler sets the external permission handler for PermissionModeExternal.
func (a *AIAgent) SetPermissionHandler(h PermissionHandler) {
	a.permissionHandler = h
}

// SetSkipEditConfirm is a backward-compatible helper that maps to PermissionMode.
// Deprecated: Use SetPermissionMode instead.
func (a *AIAgent) SetSkipEditConfirm(skip bool) {
	if skip {
		a.permissionMode = PermissionModeSkip
	} else {
		a.permissionMode = PermissionModeTUI
	}
}

// SetSkipMemoryRecall suppresses memory recall for non-interactive modes like "tachi run".
func (a *AIAgent) SetSkipMemoryRecall(skip bool) {
	a.skipMemoryRecall = skip
}

func (a *AIAgent) SetSessionManager(sm *session.Manager) {
	a.sessionManager = sm
}

// SetContextWindow sets the model's context window size for token-warning reminders.
func (a *AIAgent) SetContextWindow(window int64) {
	a.contextWindow = window
}

// ContextWindow returns the model's context window size.
func (a *AIAgent) ContextWindow() int64 {
	return a.contextWindow
}

// SetReminderCollector replaces the default reminder collector. Useful for
// tests or when callers want full control over which reminders fire.
func (a *AIAgent) SetReminderCollector(c *systemreminder.Collector) {
	a.reminderCollector = c
}

// SessionManager returns the session manager, or nil if none is set.
func (a *AIAgent) SessionManager() *session.Manager {
	return a.sessionManager
}

// ClearSession ends the current session so a new one will be created on the next message.
// Used by /new command to start a fresh session.
func (a *AIAgent) ClearSession() {
	if a.sessionManager != nil {
		a.sessionManager.EndCurrent()
	}
	// Clear skill activation state so the same skills can be re-activated.
	a.activeSkills = nil
}

// GetTool retrieves a tool from the agent's registry by name.
func (a *AIAgent) GetTool(name string) tools.Tool {
	return a.toolRegistry.GetTool(name)
}

func (a *AIAgent) recordSession(msg *session.Message) {
	if a.sessionManager == nil {
		return
	}
	if err := a.sessionManager.AppendMessage(msg); err != nil {
		a.logger.Log("Agent: failed to record session message: %v", err)
	}
}

// --- Tool Registry ---

func (a *AIAgent) RegisterTools() {
	a.toolRegistry.Register(tools.NewReadTool())
	a.toolRegistry.Register(tools.WriteTool{})
	a.toolRegistry.Register(tools.EditTool{})
	a.toolRegistry.Register(tools.GlobTool{})
	a.toolRegistry.Register(tools.GrepTool{})
	a.toolRegistry.Register(tools.NewBashTool(a.processManager))
	a.toolRegistry.Register(tools.AskUserTool{})
}

func (a *AIAgent) RegisterTool(tool tools.Tool) {
	a.toolRegistry.Register(tool)
}

// UnregisterTool removes a tool from the agent's registry by name.
func (a *AIAgent) UnregisterTool(name string) {
	a.toolRegistry.Unregister(name)
}

// ToolSchemas returns all tool schemas currently registered with the agent.
func (a *AIAgent) ToolSchemas() []tools.Schema {
	return a.toolRegistry.GetSchemas()
}

// ToolNames returns registered tool names without triggering Description() calls.
func (a *AIAgent) ToolNames() []string {
	return a.toolRegistry.GetToolNames()
}

// SaveToolRegistry returns a snapshot of all currently registered tools.
// Use RestoreToolRegistry to restore them later.
func (a *AIAgent) SaveToolRegistry() map[string]tools.Tool {
	saved := make(map[string]tools.Tool)
	for _, name := range a.toolRegistry.GetToolNames() {
		if tool := a.toolRegistry.GetTool(name); tool != nil {
			saved[name] = tool
		}
	}
	return saved
}

// RestoreToolRegistry clears the current tool registry and re-registers
// the tools from the given snapshot (typically obtained from SaveToolRegistry).
func (a *AIAgent) RestoreToolRegistry(saved map[string]tools.Tool) {
	// Remove all currently registered tools
	for _, name := range a.toolRegistry.GetToolNames() {
		a.toolRegistry.Unregister(name)
	}
	// Re-register from saved snapshot
	for _, tool := range saved {
		a.toolRegistry.Register(tool)
	}
}

// ClearToolRegistry removes all registered tools. Use when the LLM should
// produce a response without invoking any tools (e.g. /compact summarization).
func (a *AIAgent) ClearToolRegistry() {
	for _, name := range a.toolRegistry.GetToolNames() {
		a.toolRegistry.Unregister(name)
	}
}

// SetProcessManager injects a ProcessManager for background process tracking.
// Used by channel Manager to share a single PM across per-turn AIAgent instances.
// Has no effect after RegisterTools() has already been called, so call it before
// Configure().
func (a *AIAgent) SetProcessManager(pm *tools.ProcessManager) {
	a.processManager = pm
}

// Close releases resources held by the agent, including killing all tracked
// background processes. Safe to call on a nil agent.
func (a *AIAgent) Close() {
	if a.processManager != nil {
		a.processManager.KillAll()
	}
}


