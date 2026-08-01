package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/monsterxx03/tachi/agent/hooks"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
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

// ConfirmResponse is the user's answer to a tool confirmation request.
type ConfirmResponse int

const (
	// ConfirmDeny rejects the pending tool call.
	ConfirmDeny ConfirmResponse = iota
	// ConfirmAllowOnce approves this call only.
	ConfirmAllowOnce
	// ConfirmAllowAlways approves this call and remembers the exact command
	// for the rest of the session (only meaningful for Bash policy asks;
	// other tools treat it as AllowOnce).
	ConfirmAllowAlways
)

type AIAgent struct {
	// Config 是构造期/Configure 期初始化的配置聚合。多数字段只读；
	// 少数经对应 Setter 运行时修改（Logger、PermissionMode、PermissionPolicy、
	// SessionManager、ReminderCollector、ProcessManager、MCPManager、
	// CompactStrategy）。标注"构造输入"的字段（TitleGenEnabled、AutoApprove*、
	// ACPFileMode、PlanToolEnabled）在构造期被消费，运行时值分别存于
	// titleGenEnabled / PermState / Frontend——改 Config 里的这些字段无效。
	Config    AgentConfig
	Frontend  FrontendConfig   // 前端模式（纯只读）
	Channels  RuntimeChannels  // 通信通道（agent 生命周期，只读）
	PermState *PermissionState // 可变权限（session 级）

	// 运行时可变字段
	mode string // "auto" / "chat" / "plan"

	// 构造结果（Setup 解析后只读）
	titleModelProvider llm.Provider
	titleGenEnabled    bool

	// ⏳ 暂留（待后续步骤迁移）
	activeSkills         map[string]bool
	deferredToolReminder *systemreminder.DeferredToolReminder
	skillListReminder    *systemreminder.SkillListReminder
	lastOneoffPath       atomic.Pointer[string] // 由 run goroutine 写、前端并发读（oneoff 侧车路径）
	mcpInitErrors        []error
	mcpOwned             bool // Configure 前由 Config.MCPManager==nil 设定；Close 据此决定是否销毁

	conv       *convState   // 会话级滚动状态（token 估算、compact 冷却、消息日期）
	currentRun *RunState    // 当前运行的实时状态（loop 写，外部并发读）
	mu         sync.RWMutex // 保护 currentRun + mode
}

func NewAIAgent(provider llm.Provider, maxIterations int) *AIAgent {
	return &AIAgent{
		Config: AgentConfig{
			Provider:        provider,
			MaxIterations:   maxIterations,
			ToolRegistry:    tools.NewRegistry(),
			ProcessManager:  tools.NewProcessManager(),
			CompactStrategy: &llmCompactStrategy{provider: provider},
		},
		Channels: RuntimeChannels{
			ConfirmResp: make(chan ConfirmResponse, 1),
			AskUserResp: make(chan tools.AskUserResult, 1),
		},
		PermState:       &PermissionState{},
		conv:            newConvState(),
		titleGenEnabled: true,
		mode:            ModeAuto,
	}
}

// NewAIAgentWithConfig creates an AIAgent from a structured config.
// This is the recommended constructor — it replaces the pattern of
// NewAIAgent + multiple Set*/Setup* calls.
func NewAIAgentWithConfig(ctx context.Context, cfg AgentConfig) (*AIAgent, *mcp.Manager, error) {
	a := NewAIAgent(cfg.Provider, cfg.MaxIterations)

	// Adopt the caller's config wholesale, then restore NewAIAgent defaults
	// for fields the caller left nil. Wholesale assignment keeps this
	// maintenance-free: newly added AgentConfig fields need no copy here.
	a.Config = cfg
	if a.Config.ToolRegistry == nil {
		a.Config.ToolRegistry = tools.NewRegistry()
	}
	if a.Config.ProcessManager == nil {
		a.Config.ProcessManager = tools.NewProcessManager()
	}
	if a.Config.CompactStrategy == nil {
		a.Config.CompactStrategy = &llmCompactStrategy{provider: cfg.Provider}
	}

	// MCP ownership — an injected manager is owned elsewhere (e.g.
	// channel.Manager) and must not be torn down by Close.
	a.mcpOwned = cfg.MCPManager == nil

	// Dedicated providers (resolution is the caller's responsibility).
	// When a dedicated provider is nil but FullConfig is available, fall back
	// to the config-based Setup*Provider methods which resolve from provider names.
	hasCfg := cfg.FullConfig != nil

	if cfg.TitleProvider != nil {
		a.titleModelProvider = cfg.TitleProvider
	} else if hasCfg {
		a.SetupTitleProvider(cfg.FullConfig)
	}
	if cfg.CommitProvider != nil {
		a.Config.CommitProvider = cfg.CommitProvider
	} else if hasCfg {
		a.SetupCommitProvider(cfg.FullConfig)
	}
	if cfg.ReviewProvider != nil {
		a.Config.ReviewProvider = cfg.ReviewProvider
	} else if hasCfg {
		a.SetupReviewProvider(cfg.FullConfig)
	}
	// Adversarial review providers: models + judge are one Setup unit — the
	// caller either pre-resolves BOTH (config-based resolution is skipped,
	// matching the sibling providers' precedence) or neither (names are
	// resolved from FullConfig here). SetupAdversarialProviders resets first,
	// so an explicit re-setup can never accumulate duplicate entries.
	if cfg.AdversarialModels != nil {
		a.Config.AdversarialModels = cfg.AdversarialModels
		a.Config.AdversarialJudge = cfg.AdversarialJudge
	} else if hasCfg {
		a.SetupAdversarialProviders(cfg.FullConfig)
	}
	if cfg.RunProvider != nil {
		a.Config.RunProvider = cfg.RunProvider
	} else if hasCfg {
		a.SetupRunProvider(cfg.FullConfig)
	}
	if cfg.SubagentProvider != nil {
		a.Config.SubagentProvider = cfg.SubagentProvider
	} else if hasCfg {
		a.SetupSubagentProvider(cfg.FullConfig)
	}

	// TitleGenEnabled override: apply to a.titleGenEnabled (the field
	// generateTitle() actually checks). SetupTitleProvider may have already
	// set it from FullConfig; this takes final precedence when cfg provides
	// an explicit value.
	if cfg.TitleGenEnabled != nil {
		a.titleGenEnabled = *cfg.TitleGenEnabled
	}

	// PermState initialization
	a.PermState = &PermissionState{
		AutoApproveEdits:      cfg.AutoApproveEdits,
		AutoApprovePolicyAsks: cfg.AutoApprovePolicyAsks,
	}

	// Frontend config
	a.Frontend = FrontendConfig{
		ACPFileMode:     cfg.ACPFileMode,
		PlanToolEnabled: cfg.PlanToolEnabled,
	}

	// Store full config reference for subsystems that need it
	if cfg.FullConfig != nil {
		a.Config.FullConfig = cfg.FullConfig
	}

	// Configure with the extracted system config
	mcpMgr, err := a.configure(ctx, cfg.SystemConfig)
	if err != nil {
		a.Close()
		return nil, nil, err
	}

	// Resolve dedicated keyword provider (must be after configure, which creates a.Config.Memory)
	if cfg.FullConfig != nil {
		a.resolveKeywordProvider(cfg.FullConfig)
	}

	// Post-configure: SkipMemoryRecall must be set after configure
	// because configure initializes a.Config.Memory
	if cfg.SkipMemoryRecall {
		a.SetSkipMemoryRecall(true)
	}

	// Unregister AskUser when not interactive (channel/-p mode default)
	// Interactive modes (TUI, ACP with elicitation) keep it registered
	if cfg.PermissionMode != PermissionModeTUI {
		if cfg.PermissionMode == PermissionModeSkip || !cfg.ACPFileMode {
			a.UnregisterTool(tools.ToolNameAskUser)
		}
	}

	// Return mcpMgr only when we own it (caller should Close it)
	if a.mcpOwned {
		return a, mcpMgr, nil
	}
	return a, nil, nil
}

// SetLogger overrides the agent's logger. Channel callers use this to inject
// a channel-specific logger so debug output is tagged with the correct source.
func (a *AIAgent) SetLogger(l *logger.Logger) {
	a.Config.Logger = l
}

// Logger returns the agent's debug logger.
func (a *AIAgent) Logger() *logger.Logger {
	return a.Config.Logger
}

// RespondToAskUser is called by TUI to respond to an AskUserQuestion request
func (a *AIAgent) RespondToAskUser(answers map[string]string, annotations map[string]string) {
	select {
	case a.Channels.AskUserResp <- tools.AskUserResult{Answers: answers, Annotations: annotations}:
	default:
		// Channel already has a value or is not waiting
	}
}

// ConfirmTool is called by TUI to respond to a confirmation request
func (a *AIAgent) ConfirmTool(resp ConfirmResponse) {
	select {
	case a.Channels.ConfirmResp <- resp:
	default:
		// Channel already has a value or is not waiting
	}
}

func (a *AIAgent) SetProvider(provider llm.Provider) {
	a.Config.Provider = provider
}

// SetThinking updates the agent-level default thinking config (switch +
// effort). Used when switching models at runtime (/model): the target
// provider's thinking_level may differ from the current one, and without
// this the runLoop would keep applying the old model's defaults.
func (a *AIAgent) SetThinking(thinking *bool, effort string) {
	a.Config.Thinking = thinking
	a.Config.ThinkingEffort = effort
}

// SetPendingSessionThinking records a per-session thinking override to apply
// to the session that will be created on the next turn (used by /thinking
// when no session is active yet, e.g. right after startup). Empty clears it.
// The override is written into the new session's meta on first use (see
// ensureSessionAndRecordUser) and only affects that session.
func (a *AIAgent) SetPendingSessionThinking(level string) {
	a.Config.PendingSessionThinking = level
}

// PendingSessionThinking returns the pending per-session thinking override,
// or "" when none is set.
func (a *AIAgent) PendingSessionThinking() string {
	return a.Config.PendingSessionThinking
}

// Model returns the current model name.
func (a *AIAgent) Model() string {
	return a.Config.Provider.Model()
}

// Provider returns the main LLM provider for conversation turns.
func (a *AIAgent) Provider() llm.Provider {
	return a.Config.Provider
}

// SetPermissionMode sets how tool confirmation requests are handled.
func (a *AIAgent) SetPermissionMode(mode PermissionMode) {
	a.Config.PermissionMode = mode
}

// SetPermissionHandler sets the external permission handler for PermissionModeExternal.
func (a *AIAgent) SetPermissionHandler(h PermissionHandler) {
	a.PermState.PermissionHandler = h
}

// SetPermissionPolicy installs the bash permission policy (allow/ask/deny
// rules). nil disables policy checks (everything allowed, pre-feature behavior).
func (a *AIAgent) SetPermissionPolicy(p *permission.Policy) {
	a.Config.PermissionPolicy = p
}

// PermissionPolicy returns the installed policy, or nil.
func (a *AIAgent) PermissionPolicy() *permission.Policy {
	return a.Config.PermissionPolicy
}

// SetAutoApprovePolicyAsks controls how PermissionModeSkip handles policy
// "ask" decisions: true = execute anyway (used after an explicit "allow all"
// choice, e.g. ACP); false (default) = deny with an explanatory error.
func (a *AIAgent) SetAutoApprovePolicyAsks(v bool) {
	a.PermState.AutoApprovePolicyAsks = v
}

// SetAutoApproveEdits skips EditFile confirmation prompts (TUI-oriented).
// Unlike PermissionModeSkip, it affects only EditFile — bash policy asks
// and any other confirmations still prompt.
func (a *AIAgent) SetAutoApproveEdits(v bool) {
	a.PermState.AutoApproveEdits = v
}

// SetACPFileMode enables ACP file I/O for the EditFile tool.
func (a *AIAgent) SetACPFileMode() {
	a.Frontend.ACPFileMode = true
}

// EnablePlanTool enables registration of the SavePlan tool. Must be called
// before Configure()/RegisterTools(). Currently only ACP sessions enable it,
// since only ACP clients render the structured plan card UI.
func (a *AIAgent) EnablePlanTool() {
	a.Frontend.PlanToolEnabled = true
}

// SetSkipMemoryRecall suppresses memory recall for non-interactive modes like "tachi run".
func (a *AIAgent) SetSkipMemoryRecall(skip bool) {
	if a.Config.Memory != nil {
		a.Config.Memory.SkipRecall = skip
	}
}

func (a *AIAgent) SetSessionManager(sm SessionManager) {
	a.Config.SessionManager = sm
	// Wire session provider into TopicBackend for temporal query fallback.
	// This enables queries like "我们最近聊过什么" where keyword-based grep
	// cannot match — the backend falls back to recent session summaries.
	if a.Config.Memory != nil {
		if tb, ok := a.Config.Memory.Backend.(*memory.TopicBackend); ok {
			tb.SetSessionProvider(&topicSessionProvider{manager: sm})
			a.Config.Logger.Info(context.Background(), "Memory: session provider wired for topic backend")
		}
	}
}

// topicSessionProvider adapts SessionManager to memory.SessionProvider,
// allowing TopicBackend to fall back to recent session summaries when
// keyword-based topic search yields no results (temporal queries).
type topicSessionProvider struct {
	manager SessionManager
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
	a.Config.ContextWindow = window
}

// LastInputEstimate returns the local token estimate for the most recent
// API call, computed by estimateAndUpdateTokens before the call. This is
// deliberately conservative (overestimates) and is used for both
// token-warning reminders and the TUI statusbar context fraction.
func (a *AIAgent) LastInputEstimate() int64 {
	return a.conv.tokens()
}

// isCompactCooldown returns true if the token estimate has not grown
// significantly (>= 20%) since the last auto-compact. This prevents
// repeated compaction on the same session within a single conversation.
func (a *AIAgent) isCompactCooldown() bool {
	return a.conv.compactCooldown()
}

// setCompactCooldown records the current token estimate so that
// isCompactCooldown can prevent immediate re-compaction.
func (a *AIAgent) setCompactCooldown() {
	a.conv.setCompactEstimate(a.conv.tokens())
}

// ContextWindow returns the model's context window size.
func (a *AIAgent) ContextWindow() int64 {
	return a.Config.ContextWindow
}

// SetReminderCollector replaces the default reminder collector. Useful for
// tests or when callers want full control over which reminders fire.
func (a *AIAgent) SetReminderCollector(c ReminderCollector) {
	a.Config.ReminderCollector = c
}

// SessionManager returns the session manager, or nil if none is set.
func (a *AIAgent) SessionManager() SessionManager {
	return a.Config.SessionManager
}

// ClearSession ends the current session so a new one will be created on the next message.
// Used by /new command to start a fresh session.
func (a *AIAgent) ClearSession() {
	if a.Config.SessionManager != nil {
		a.Config.SessionManager.EndCurrent()
	}
	// Clear skill activation state so the same skills can be re-activated.
	a.activeSkills = nil
}

// GetTool retrieves a tool from the agent's registry by name.
func (a *AIAgent) GetTool(name string) tools.Tool {
	return a.Config.ToolRegistry.GetTool(name)
}

// recordSession persists a message to the session store. rs is the owning
// run's state: runs flagged SkipSessionWrites (one-off tasks like /commit,
// /review, ambient, dream) keep their messages out of the main session
// history, leaving a trail in the run's one-off transcript instead.
func (a *AIAgent) recordSession(rs *RunState, msg *session.Message) {
	if a.Config.SessionManager == nil || rs.SkipSessionWrites {
		// Side-channel execution: keep it out of the main session history,
		// but leave a trail in the one-off transcript if one is attached.
		if rs.OneoffRec != nil {
			rs.OneoffRec.record(msg)
		}
		return
	}
	if err := a.Config.SessionManager.AppendMessage(msg); err != nil {
		a.Config.Logger.Error(context.Background(), "Agent: failed to record session message", err)
	}
}

// --- Tool Registry ---

func (a *AIAgent) RegisterTools() {
	a.Config.ToolRegistry.Register(tools.NewReadTool())
	a.Config.ToolRegistry.Register(tools.WriteTool{})
	editTool := tools.NewEditTool()
	if a.Frontend.ACPFileMode {
		editTool.SetACPMode(true)
	}
	a.Config.ToolRegistry.Register(editTool)

	a.Config.ToolRegistry.Register(tools.GlobTool{})
	a.Config.ToolRegistry.Register(tools.GrepTool{})
	a.Config.ToolRegistry.Register(tools.NewBashTool(a.Config.ProcessManager))
	a.Config.ToolRegistry.Register(tools.AskUserTool{})

	// WebSearch — only register if at least one provider + key is configured
	if a.Config.FullConfig != nil {
		ws := tools.NewWebSearchTool(tools.WebSearchToolConfig{
			Providers:  a.Config.FullConfig.WebSearch.Providers,
			Timeout:    a.Config.FullConfig.WebSearch.Timeout,
			MaxResults: a.Config.FullConfig.WebSearch.MaxResults,
			Proxy:      a.Config.FullConfig.WebSearch.Proxy,
		})
		if ws.Configured() {
			a.Config.ToolRegistry.Register(ws)
		}

		// WebFetch — always registered, no API key needed (firecrawl type
		// needs key; reserved targets fall back to native regardless).
		a.Config.ToolRegistry.Register(&tools.WebFetchTool{
			Type:           a.Config.FullConfig.WebFetch.Type,
			Key:            a.Config.FullConfig.WebFetch.Key,
			BaseURL:        a.Config.FullConfig.WebFetch.BaseURL,
			Timeout:        a.Config.FullConfig.WebFetch.Timeout,
			Proxy:          a.Config.FullConfig.WebFetch.Proxy,
			ResultBaseDir:  a.Config.FullConfig.ToolResult.ResultFileDir(),
			MaxReturnChars: a.Config.FullConfig.ToolResult.MaxResultChars(),
		})
	}

	// RecordMemory / MemoryRecall — only when memory backend is configured
	if a.Config.Memory != nil {
		a.Config.ToolRegistry.Register(tools.NewRecordMemoryTool(a))
		a.Config.ToolRegistry.Register(tools.NewMemoryRecallTool(a.Config.Memory.Backend))
	}

	// SavePlan — only registered for frontends with a plan card UI (ACP).
	// TUI/channel have no way to render the structured plan, so the tool
	// would just produce unseen JSON files.
	if a.Frontend.PlanToolEnabled {
		a.Config.ToolRegistry.Register(tools.SavePlanTool{})
	}
}

func (a *AIAgent) RegisterTool(tool tools.Tool) {
	a.Config.ToolRegistry.Register(tool)
}

// UnregisterTool removes a tool from the agent's registry by name.
func (a *AIAgent) UnregisterTool(name string) {
	a.Config.ToolRegistry.Unregister(name)
}

// UnregisterMCPServer removes all tools belonging to an MCP server from
// every data structure: the active tool registry, the deferred pool, and
// the discovered set. This ensures that disabling a server via /mcp toggle
// fully cleans up — no stale tool references remain in DeferredToolReminder
// or MCPSearchTools.
func (a *AIAgent) UnregisterMCPServer(serverName string) {
	prefix := fmt.Sprintf("mcp__%s__", serverName)

	// 1. Unregister from active tool registry
	for _, name := range a.Config.ToolRegistry.GetToolNames() {
		if strings.HasPrefix(name, prefix) {
			a.Config.ToolRegistry.Unregister(name)
			a.Config.Logger.Info(context.Background(), "MCP: unregistered tool from registry", "tool", name)
		}
	}

	pool := a.DeferredPool()
	set := a.discoveredSet()

	// 2. Remove from deferred pool
	if pool != nil {
		removed := pool.RemoveByServer(serverName)
		if removed > 0 {
			a.Config.Logger.Info(context.Background(), "MCP: removed tools from deferred pool for server", "count", removed, "server", serverName)
		}
	}

	// 3. Remove from discovered set
	if set != nil {
		// Collect tool names with this prefix from the discovered set
		for _, name := range set.List() {
			if strings.HasPrefix(name, prefix) {
				set.Remove(name)
				a.Config.Logger.Info(context.Background(), "MCP: removed tool from discovered set", "tool", name)
			}
		}
	}
}

// DeferredPool returns the MCP deferred pool owned by the agent's
// MCP manager, or nil if no manager is configured (i.e. MCP disabled).
func (a *AIAgent) DeferredPool() *mcp.DeferredPool {
	if a.Config.MCPManager == nil {
		return nil
	}
	return a.Config.MCPManager.Pool()
}

// SetMCPInitErrors stores per-server MCP connection errors from async init.
// Called by connectMCPBackground; read by the TUI after MCPReadyMsg.
func (a *AIAgent) SetMCPInitErrors(errs []error) {
	a.mcpInitErrors = errs
}

// MCPInitErrors returns per-server MCP connection errors, or nil if all
// servers connected successfully.
func (a *AIAgent) MCPInitErrors() []error {
	return a.mcpInitErrors
}

// discoveredSet returns the MCP discovered set owned by the agent's
// MCP manager, or nil if no manager is configured. Internal helper.
func (a *AIAgent) discoveredSet() *mcp.DiscoveredSet {
	if a.Config.MCPManager == nil {
		return nil
	}
	return a.Config.MCPManager.DiscoveredSet()
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
		a.Config.Logger.Info(context.Background(), "MCP: deferred tool (user toggle)", "tool", t.Name())
	}
	a.NotifyDeferredToolsAdded()
	a.Config.Logger.Info(context.Background(), "MCP: added tools to deferred pool from toggle", "count", count)
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
	a.Config.ReminderCollector.AddReminder(a.deferredToolReminder)

	a.Config.Logger.Info(context.Background(), "MCP: DeferredToolReminder marked dirty")
}

// ToolSchemas returns all tool schemas currently registered with the agent.
func (a *AIAgent) ToolSchemas() []tools.Schema {
	return a.Config.ToolRegistry.GetSchemas()
}

// ToolNames returns registered tool names without triggering Description() calls.
func (a *AIAgent) ToolNames() []string {
	return a.Config.ToolRegistry.GetToolNames()
}

// SaveToolRegistry returns a snapshot of all currently registered tools.
// Use RestoreToolRegistry to restore them later.
func (a *AIAgent) SaveToolRegistry() map[string]tools.Tool {
	saved := make(map[string]tools.Tool)
	for _, name := range a.Config.ToolRegistry.GetToolNames() {
		if tool := a.Config.ToolRegistry.GetTool(name); tool != nil {
			saved[name] = tool
		}
	}
	return saved
}

// RestoreToolRegistry clears the current tool registry and re-registers
// the tools from the given snapshot (typically obtained from SaveToolRegistry).
func (a *AIAgent) RestoreToolRegistry(saved map[string]tools.Tool) {
	// Remove all currently registered tools
	for _, name := range a.Config.ToolRegistry.GetToolNames() {
		a.Config.ToolRegistry.Unregister(name)
	}
	// Re-register from saved snapshot
	for _, tool := range saved {
		a.Config.ToolRegistry.Register(tool)
	}
}

// ClearToolRegistry removes all registered tools. Use when the LLM should
// produce a response without invoking any tools (e.g. /compact summarization).
func (a *AIAgent) ClearToolRegistry() {
	for _, name := range a.Config.ToolRegistry.GetToolNames() {
		a.Config.ToolRegistry.Unregister(name)
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
	a.Config.ProcessManager = pm
}

// SetSharedMCP injects a pre-built MCP manager to be shared across multiple
// AIAgent instances (e.g. per-thread cached agents in channel mode). When
// called BEFORE Configure(), the MCP init step is skipped — the agent
// reuses the provided manager (which carries pool, discovered set, and
// initDone channel) instead of creating its own.
//
// Close() will not tear down shared MCP — the owner (channel.Manager) is
// responsible for closing the manager when the process exits.
func (a *AIAgent) SetSharedMCP(mgr *mcp.Manager) {
	a.Config.MCPManager = mgr
	a.mcpOwned = false
}

// GetLastMessages returns the final LLM message slice from the most recent
// RunConversationStream call. The slice includes every message exchanged
// during that turn: the prior history, the (wrapped) user message, and all
// assistant + tool-call + tool-result messages produced by the agent loop.
// It is safe to read only after the event channel returned by
// RunConversationStream has been fully drained (channel closed).
//
// RunOneOffStream does NOT publish its RunState to currentRun — messages
// from one-off runs (/commit, /review, ambient, dream) are NOT accessible
// via this method. Returns nil if no RunConversationStream has completed.
func (a *AIAgent) GetLastMessages() []llm.Message {
	a.mu.RLock()
	rs := a.currentRun
	a.mu.RUnlock()
	if rs != nil {
		return rs.snapshotMessages()
	}
	return nil
}

// dispatchEvent sends an event to the hook dispatcher, if initialised.
// It is a no-op when the hook system is disabled or has no handlers for
// the given event. The payload is populated with the current session ID
// and any extra fields passed via opts.
func (a *AIAgent) dispatchEvent(ctx context.Context, event string, opts hooks.Payload) {
	if a.Config.HookDispatcher == nil {
		return
	}
	if a.Config.SessionManager != nil && a.Config.SessionManager.Current() != nil {
		opts.SessionID = a.Config.SessionManager.Current().ID
	}
	if wd, err := os.Getwd(); err == nil && opts.WorkspaceDir == "" {
		opts.WorkspaceDir = wd
	}
	opts.Event = event
	a.Config.HookDispatcher.Dispatch(ctx, event, opts)
}

// dispatchPermissionResult reports a permission decision to the hook system.
// The only varying field is Approved, so the boilerplate lives here. Shared by
// the bash ask flow (agent_permission.go) and the ConfirmationTool flow
// (tool_executor.go).
func (a *AIAgent) dispatchPermissionResult(ctx context.Context, tc llm.ToolCall, approved bool) {
	a.dispatchEvent(ctx, hooks.EventPermissionResult, hooks.Payload{
		ToolName: tc.Function.Name,
		ToolID:   tc.ID,
		Approved: approved,
	})
}

// KillBackgroundProcesses terminates all tracked background processes
// (ProcessManager). Called when the user interrupts a turn with Ctrl+C so
// that long-running background tasks started during the turn (e.g. an http
// server launched with background=true) do not outlive the cancellation.
// Safe to call on a nil agent.
func (a *AIAgent) KillBackgroundProcesses() {
	if a == nil || a.Config.ProcessManager == nil {
		return
	}
	a.Config.ProcessManager.KillAll()
}

// Close releases resources held by the agent, including killing all tracked
// background processes. Safe to call on a nil agent.
func (a *AIAgent) Close() {
	// Fire session_end hook before tearing down, so integrations (e.g. Herdr)
	// can mark the agent as idle before the process exits.
	a.dispatchEvent(context.Background(), hooks.EventSessionEnd, hooks.Payload{})

	if a.Config.ProcessManager != nil {
		a.Config.ProcessManager.KillAll()
	}
	if a.Config.LSPManager != nil {
		a.Config.LSPManager.Shutdown(context.Background())
	}
}
