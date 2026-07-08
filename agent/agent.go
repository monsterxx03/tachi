package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/lsp"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
)

// DefaultMaxTokens is the fallback max_tokens value for LLM API calls
// when no explicit value is configured.
const DefaultMaxTokens = 4096

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

// NewIterationBudget creates a new iteration budget. When maxIterations is 0,
// the budget is unlimited (conversation mode). Otherwise it has a fixed cap.
func NewIterationBudget(maxIterations int) *IterationBudget {
	if maxIterations == 0 {
		return &IterationBudget{Unlimited: true}
	}
	return &IterationBudget{Remaining: maxIterations}
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
	lastInputTokens    int64                    // local token estimate (conservative), set by estimateAndUpdateTokens
	lastTokenBreakdown tokenbreakdown.Breakdown // categorized breakdown of last estimate, set alongside lastInputTokens
	lastMessageDate    string                   // calendar date (2006-01-02) of last processed user message; empty initially
	titleModelProvider llm.Provider             // optional: dedicated provider for title generation
	titleGenEnabled    bool                     // whether LLM-based title generation is active
	commitProvider     llm.Provider             // optional: dedicated provider for /commit messages
	reviewProvider     llm.Provider             // optional: dedicated provider for /review code review
	logger             *debuglog.Logger

	// Skill-related fields
	skillStore   *skill.Store
	activeSkills map[string]bool // skills activated in current session

	// Subagent-related fields (implements subagent.Agent interface)
	subagentProvider llm.Provider // sub-agent dedicated provider (nil = fallback to main)
	subagentRunner   tools.SubagentRunner

	// Memory-related fields
	memory *MemoryState // nil = memory not enabled

	// Config reference (set by Configure — used by RegisterTools and sub-systems)
	cfg *config.Config

	// MCP ToolSearch fields are owned by mcpManager. The agent reads them via
	// mcpManager.Pool() / mcpManager.DiscoveredSet() rather than holding its
	// own references — there is one source of truth for ToolSearch state per
	// manager, which can be shared across agents (e.g. channel mode).

	// DeferredToolReminder reference — allows mid-session reset when user
	// manually enables an MCP server (tools go into the manager's pool, reminder
	// fires again to hint LLM about them).
	deferredToolReminder *systemreminder.DeferredToolReminder

	// skillListReminder is the live SkillListReminder in the reminderCollector.
	// Stored so we can mutate it (MarkDirty / SetProvider) without rebuilding
	// the entire collector.
	skillListReminder *systemreminder.SkillListReminder

	// MCP async init
	mcpManager *mcp.Manager // MCP connection manager (also owns pool/set/initDone)

	// sharedMCP is true when mcpManager was injected via SetSharedMCP and
	// should not be re-created or torn down by Configure/Close. Used by
	// channel.Manager to share one MCP backend
	// across many cached AIAgent instances.
	sharedMCP bool

	// processManager manages background processes started by BashTool.
	// Tied to the agent lifecycle — Close() kills all tracked processes.
	processManager *tools.ProcessManager

	// lspManager manages LSP server connections.
	lspManager *lsp.LSPManager

	// pendingImages holds image content parts to attach to the next user message.
	// Set via SetPendingImages, consumed (and cleared) by RunConversationStream.
	pendingImages []llm.ContentPart

	// lastMessages is the final LLM message slice after a RunConversationStream
	// or RunOneOffStream call completes. It includes all messages sent to the
	// LLM during that turn (history + current user + assistant + tool results).
	// Channel mode reads this via GetLastMessages() to maintain an in-memory
	// history cache on the cachedAgent, avoiding repeated disk reloads.
	lastMessages []llm.Message

	// lastCompactTokenEstimate tracks the token estimate at the time of the
	// most recent auto-compact. Used by shouldAutoCompact's cooldown logic:
	// auto-compact won't retrigger until token estimate grows by >20%.
	lastCompactTokenEstimate int64

	// turnStart records the wall-clock time when the current turn's agent
	// loop began. Set at the top of runAgentLoop and used by finish handlers
	// to compute the turn's total duration for the RunResult.
	turnStart time.Time
}

func NewAIAgent(provider llm.Provider, maxIterations int) *AIAgent {
	return &AIAgent{
		provider:        provider,
		maxIterations:   maxIterations,
		titleGenEnabled: true,
		toolRegistry:    tools.NewRegistry(),
		processManager:  tools.NewProcessManager(),
		confirmRespCh:   make(chan bool, 1),
		askUserRespCh:   make(chan tools.AskUserResult, 1),
		logger:          debuglog.DefaultLogger,
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

func (a *AIAgent) SetProvider(provider llm.Provider) {
	a.provider = provider
}

// Model returns the current model name.
func (a *AIAgent) Model() string {
	return a.provider.Model()
}

// Provider returns the main LLM provider for conversation turns.
func (a *AIAgent) Provider() llm.Provider {
	return a.provider
}

// SetPermissionMode sets how tool confirmation requests are handled.
func (a *AIAgent) SetPermissionMode(mode PermissionMode) {
	a.permissionMode = mode
}

// SetPermissionHandler sets the external permission handler for PermissionModeExternal.
func (a *AIAgent) SetPermissionHandler(h PermissionHandler) {
	a.permissionHandler = h
}

// SetACPFileMode enables ACP file I/O for the EditFile tool. In ACP mode,
// NeedsConfirmation returns false (Zed handles review) and ExecuteContext
// routes through conn.WriteTextFile for inline diffs.
func (a *AIAgent) SetACPFileMode() {
	if t, ok := a.toolRegistry.GetTool(tools.ToolNameEdit).(*tools.EditTool); ok {
		t.SetACPMode(true)
	}
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
	if a.memory != nil {
		a.memory.SkipRecall = skip
	}
}

func (a *AIAgent) SetSessionManager(sm *session.Manager) {
	a.sessionManager = sm
	// Wire session provider into TopicBackend for temporal query fallback.
	// This enables queries like "我们最近聊过什么" where keyword-based grep
	// cannot match — the backend falls back to recent session summaries.
	if a.memory != nil {
		if tb, ok := a.memory.Backend.(*memory.TopicBackend); ok {
			tb.SetSessionProvider(&topicSessionProvider{manager: sm})
			a.logger.Log("Memory: session provider wired for topic backend")
		}
	}
}

// topicSessionProvider adapts *session.Manager to memory.SessionProvider,
// allowing TopicBackend to fall back to recent session summaries when
// keyword-based topic search yields no results (temporal queries).
type topicSessionProvider struct {
	manager *session.Manager
}

// recentUserMsgCount is how many recent user messages to include per session
// in the temporal query fallback. Two messages gives enough context to answer
// "what did we talk about recently" without being overly verbose.
const recentUserMsgCount = 2

// maxMsgLength caps individual user messages in session summaries to keep
// the total recall output bounded (200 chars is enough to convey topic).
const maxMsgLength = 200

func (p *topicSessionProvider) RecentSessions(ctx context.Context, limit int) ([]memory.RecentSession, error) {
	sessions, err := p.manager.List()
	if err != nil {
		return nil, err
	}
	if len(sessions) > limit {
		sessions = sessions[:limit]
	}
	result := make([]memory.RecentSession, len(sessions))
	for i, s := range sessions {
		msgs, err := p.manager.LoadSessionMessages(s.ID)
		recent := []string{}
		if err == nil {
			// Collect last N user messages in reverse order (most recent first).
			for j := len(msgs) - 1; j >= 0 && len(recent) < recentUserMsgCount; j-- {
				if msgs[j].Type == session.MessageTypeUser {
					text := msgs[j].Content
					runes := []rune(text)
					if len(runes) > maxMsgLength {
						text = string(runes[:maxMsgLength-1]) + "…"
					}
					recent = append(recent, text)
				}
			}
		}
		result[i] = memory.RecentSession{
			ID:             s.ID,
			Title:          s.Title,
			RecentMessages: recent,
			CreatedAt:      s.CreatedAt,
			UpdatedAt:      s.UpdatedAt,
		}
	}
	return result, nil
}

// SetContextWindow sets the model's context window size for token-warning reminders.
func (a *AIAgent) SetContextWindow(window int64) {
	a.contextWindow = window
}

// LastInputEstimate returns the local token estimate for the most recent
// API call, computed by estimateAndUpdateTokens before the call. This is
// deliberately conservative (overestimates) and is used for both
// token-warning reminders and the TUI statusbar context fraction.
func (a *AIAgent) LastInputEstimate() int64 {
	return a.lastInputTokens
}

// isCompactCooldown returns true if the token estimate has not grown
// significantly (>= 20%) since the last auto-compact. This prevents
// repeated compaction on the same session within a single conversation.
func (a *AIAgent) isCompactCooldown() bool {
	if a.lastCompactTokenEstimate == 0 {
		return false // never compacted
	}
	growth := float64(a.lastInputTokens) / float64(a.lastCompactTokenEstimate)
	return growth < 1.2
}

// setCompactCooldown records the current token estimate so that
// isCompactCooldown can prevent immediate re-compaction.
func (a *AIAgent) setCompactCooldown() {
	a.lastCompactTokenEstimate = a.lastInputTokens
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
	a.toolRegistry.Register(tools.NewEditTool())

	a.toolRegistry.Register(tools.GlobTool{})
	a.toolRegistry.Register(tools.GrepTool{})
	a.toolRegistry.Register(tools.NewBashTool(a.processManager))
	a.toolRegistry.Register(tools.AskUserTool{})

	// WebSearch — only register if provider + key are configured
	if a.cfg != nil {
		ws := tools.WebSearchTool{
			ProviderType: a.cfg.WebSearch.Type,
			APIKey:       a.cfg.WebSearch.Key,
			Timeout:      a.cfg.WebSearch.Timeout,
			MaxResults:   a.cfg.WebSearch.MaxResults,
			Proxy:        a.cfg.WebSearch.Proxy,
		}
		if _, key := ws.ResolveProvider(); key != "" {
			a.toolRegistry.Register(&ws)
		}

		// WebFetch — always registered, no API key needed.
		a.toolRegistry.Register(&tools.WebFetchTool{
			Timeout:        a.cfg.WebFetch.Timeout,
			Proxy:          a.cfg.WebFetch.Proxy,
			ResultBaseDir:  a.cfg.ToolResult.ResultFileDir(),
			MaxReturnChars: a.cfg.ToolResult.MaxResultChars(),
		})
	}

	// RecordMemory / MemoryRecall — only when memory backend is configured
	if a.memory != nil {
		a.toolRegistry.Register(tools.NewRecordMemoryTool(a))
		a.toolRegistry.Register(tools.NewMemoryRecallTool(a.memory.Backend))
	}
}

func (a *AIAgent) RegisterTool(tool tools.Tool) {
	a.toolRegistry.Register(tool)
}

// UnregisterTool removes a tool from the agent's registry by name.
func (a *AIAgent) UnregisterTool(name string) {
	a.toolRegistry.Unregister(name)
}

// UnregisterMCPServer removes all tools belonging to an MCP server from
// every data structure: the active tool registry, the deferred pool, and
// the discovered set. This ensures that disabling a server via /mcp toggle
// fully cleans up — no stale tool references remain in DeferredToolReminder
// or MCPSearchTools.
func (a *AIAgent) UnregisterMCPServer(serverName string) {
	prefix := fmt.Sprintf("mcp__%s__", serverName)

	// 1. Unregister from active tool registry
	for _, name := range a.toolRegistry.GetToolNames() {
		if strings.HasPrefix(name, prefix) {
			a.toolRegistry.Unregister(name)
			a.logger.Log("MCP: unregistered tool %s from registry", name)
		}
	}

	pool := a.DeferredPool()
	set := a.discoveredSet()

	// 2. Remove from deferred pool
	if pool != nil {
		removed := pool.RemoveByServer(serverName)
		if removed > 0 {
			a.logger.Log("MCP: removed %d tools from deferred pool for server %s", removed, serverName)
		}
	}

	// 3. Remove from discovered set
	if set != nil {
		// Collect tool names with this prefix from the discovered set
		for _, name := range set.List() {
			if strings.HasPrefix(name, prefix) {
				set.Remove(name)
				a.logger.Log("MCP: removed tool %s from discovered set", name)
			}
		}
	}
}

// DeferredPool returns the MCP deferred pool owned by the agent's
// MCP manager, or nil if no manager is configured (i.e. MCP disabled).
func (a *AIAgent) DeferredPool() *mcp.DeferredPool {
	if a.mcpManager == nil {
		return nil
	}
	return a.mcpManager.Pool()
}

// discoveredSet returns the MCP discovered set owned by the agent's
// MCP manager, or nil if no manager is configured. Internal helper.
func (a *AIAgent) discoveredSet() *mcp.DiscoveredSet {
	if a.mcpManager == nil {
		return nil
	}
	return a.mcpManager.DiscoveredSet()
}

// AddDeferredMCPTools adds MCP tools to the deferred pool and marks the
// DeferredToolReminder as dirty so it fires on the next user message.
// This is used when a user manually enables an MCP server mid-session —
// tools are deferred (not immediately visible to the LLM) and hinted via
// the <available-deferred-tools> system reminder.
// Returns the number of tools added.
func (a *AIAgent) AddDeferredMCPTools(tools []mcp.MCPTool) int {
	pool := a.DeferredPool()
	if pool == nil {
		return 0
	}
	count := 0
	for _, t := range tools {
		dt := mcp.NewDeferredToolFromMCPTool(t, "")
		pool.Add(dt)
		count++
		a.logger.Log("MCP: deferred tool %s (user toggle)", t.Name())
	}
	a.NotifyDeferredToolsAdded()
	a.logger.Log("MCP: added %d tools to deferred pool from toggle", count)
	return count
}

// NotifyDeferredToolsAdded marks the DeferredToolReminder as dirty and ensures
// it's registered in the reminder collector so the LLM is notified of newly
// available deferred tools on the next user message. Safe to call even when
// the reminder hasn't been set up yet.
func (a *AIAgent) NotifyDeferredToolsAdded() {
	pool := a.DeferredPool()
	if a.deferredToolReminder == nil {
		// DeferredToolReminder hasn't been created yet (e.g., ToolSearch
		// was disabled during init). Create one now.
		if pool == nil {
			return
		}
		a.deferredToolReminder = &systemreminder.DeferredToolReminder{
			Provider: &deferredToolProviderAdapter{pool: pool},
			Tracker:  a.discoveredSet(),
			Dirty:    true,
		}
	} else {
		a.deferredToolReminder.Dirty = true
	}

	// Ensure it's registered in the reminder collector
	a.reminderCollector.AddReminder(a.deferredToolReminder)

	a.logger.Log("MCP: DeferredToolReminder marked dirty")
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

// RestrictTools unregisters tools based on allowed/disallowed lists.
// When allowed is non-empty, only tools in the whitelist are kept (disallowed ignored).
// When only disallowed is non-empty, those tools are removed and everything else kept.
func (a *AIAgent) RestrictTools(allowed, disallowed []string) {
	if len(allowed) == 0 && len(disallowed) == 0 {
		return
	}

	if len(allowed) > 0 {
		// Whitelist mode: keep only tools in the set.
		keep := make(map[string]bool)
		for _, name := range allowed {
			keep[name] = true
		}
		for _, name := range a.ToolNames() {
			if !keep[name] {
				a.UnregisterTool(name)
			}
		}
		return
	}

	// Blacklist mode: remove only tools in the set.
	remove := make(map[string]bool)
	for _, name := range disallowed {
		remove[name] = true
	}
	for _, name := range a.ToolNames() {
		if remove[name] {
			a.UnregisterTool(name)
		}
	}
}

// SetProcessManager injects a ProcessManager for background process tracking.
// Used by channel Manager to share a single PM across per-turn AIAgent instances.
// Has no effect after RegisterTools() has already been called, so call it before
// Configure().
func (a *AIAgent) SetProcessManager(pm *tools.ProcessManager) {
	a.processManager = pm
}

// SetSharedMCP injects a pre-built MCP manager to be shared across multiple
// AIAgent instances (e.g. per-thread cached agents in channel mode). When
// called BEFORE Configure(), the InitMCPAsync step is skipped — the agent
// reuses the provided manager (which carries pool, discovered set, and
// initDone channel) instead of creating its own.
//
// Close() will not tear down shared MCP — the owner (channel.Manager) is
// responsible for closing the manager when the process exits.
func (a *AIAgent) SetSharedMCP(mgr *mcp.Manager) {
	a.mcpManager = mgr
	a.sharedMCP = true
}

// SetPendingImages sets image content parts to attach to the next user message
// sent via RunConversationStream. The images are consumed (cleared) after use.
// Call this before RunConversationStream when the user message includes images.
func (a *AIAgent) SetPendingImages(images []llm.ContentPart) {
	a.pendingImages = images
}

// GetLastMessages returns the final LLM message slice from the most recent
// RunConversationStream call. The slice includes every message exchanged
// during that turn: the prior history, the (wrapped) user message, and all
// assistant + tool-call + tool-result messages produced by the agent loop.
// It is safe to read only after the event channel returned by
// RunConversationStream has been fully drained (channel closed).
// Returns nil if no turn has completed yet.
func (a *AIAgent) GetLastMessages() []llm.Message {
	return a.lastMessages
}

// Close releases resources held by the agent, including killing all tracked
// background processes. Safe to call on a nil agent.
func (a *AIAgent) Close() {
	if a.processManager != nil {
		a.processManager.KillAll()
	}
	if a.lspManager != nil {
		a.lspManager.Shutdown(context.Background())
	}
}
