package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monsterxx03/tachi/agent/hooks"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
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

	// Vision fallback（懒构建，mutex 保护）：当当前模型不支持图片时，用
	// 配置中第一个支持图片的 provider 描述图片。Once 保证每个 agent 只
	// 解析一次；err 为 nil 时 delegate 才可用（无可用 provider 也会缓存）。
	visionDelegateOnce sync.Once
	visionDelegate     llm.Provider
	visionDelegateErr  error
	// visionDelegateOverride（仅测试）直接替换 config 解析的 delegate，
	// 用于单测描述转换逻辑而不触发真实 provider 构建。
	visionDelegateOverride llm.Provider
}

// NewAIAgentWithConfig creates an AIAgent from a structured config.
// This is the recommended constructor — it replaces the pattern of
// NewAIAgent + multiple Set*/Setup* calls.
func NewAIAgentWithConfig(ctx context.Context, cfg AgentConfig) (*AIAgent, *mcp.Manager, error) {
	// The agent always owns a non-nil Resolved (the "no nil Resolved"
	// invariant — reads can dereference it directly):
	//   - caller-provided: copied here so SetResolvedProvider / SetThinking /
	//     SetContextWindow mutate a private object (the caller's original
	//     *ResolvedProvider neither sees those writes nor leaks its own
	//     post-construction mutations into the agent);
	//   - nil: resolved from FullConfig's default provider, so callers that
	//     only need the default provider skip the build dance entirely;
	//   - nil + no FullConfig: an empty ResolvedProvider (no main provider —
	//     bare agents like Fork / github / dream always pass one explicitly).
	if cfg.Resolved == nil {
		if cfg.FullConfig != nil {
			rp, err := llm.DefaultProvider(cfg.FullConfig)
			if err != nil {
				return nil, nil, err
			}
			cfg.Resolved = rp
		} else {
			cfg.Resolved = &llm.ResolvedProvider{}
		}
	} else {
		r := *cfg.Resolved
		cfg.Resolved = &r
	}

	a := &AIAgent{
		Config: AgentConfig{
			Resolved:        cfg.Resolved,
			MaxIterations:   cfg.MaxIterations,
			ToolRegistry:    tools.NewRegistry(),
			ProcessManager:  tools.NewProcessManager(),
			CompactStrategy: &llmCompactStrategy{provider: cfg.Resolved.Provider},
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

	// Usage billing: wrap every provider so all LLM calls are recorded into
	// the ledger. MUST run before a.Config = cfg so derived providers
	// (CompactStrategy, Setup*Provider, keyword extractor) all receive the
	// wrapped instances — wrapping after would silently miss call sites.
	rec := cfg.UsageRecorder
	if rec == nil {
		rec = getGlobalUsageRecorder()
	}
	wrapUsageProviders(&cfg, rec)

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
		a.Config.CompactStrategy = &llmCompactStrategy{provider: cfg.Resolved.Provider}
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
	// Adversarial providers — whether caller-pre-resolved or resolved from
	// config here — are wrapped AFTER the Setup section, so the pre-resolved
	// pointers survive construction (see wrapUsageProviders) while both paths
	// still get /review billing.
	wrapResolvedAdversarial(&a.Config, rec, cfg.FullConfig)
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
		AutoApprovePolicyAsks: cfg.AutoApprovePolicyAsks,
	}

	// Frontend config
	a.Frontend = FrontendConfig{
		ACPFileMode:     cfg.ACPFileMode,
		ACPReadMode:     cfg.ACPReadMode,
		ACPTerminalBash: cfg.ACPTerminalBash,
		PlanToolEnabled: cfg.PlanToolEnabled,
	}

	// Store full config reference for subsystems that need it
	if cfg.FullConfig != nil {
		a.Config.FullConfig = cfg.FullConfig
	}

	// Configure with the extracted system config. SkipConfigure keeps the
	// agent "bare" (no built-in tools / skills / reminder collector / subagent
	// tool — see AgentConfig.SkipConfigure); NewAIAgentWithConfig never
	// returns an error on that path.
	var mcpMgr *mcp.Manager
	var err error
	if !cfg.SkipConfigure {
		mcpMgr, err = a.configure(ctx, cfg.SystemConfig)
		if err != nil {
			a.Close()
			return nil, nil, err
		}
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

	// Unregister AskUser when not interactive (channel/-p mode default).
	// Interactive modes (TUI, ACP) keep it registered; in ACP sessions the
	// elicitation capability check (supportsElicitation) decides separately
	// whether the tool stays usable.
	if cfg.PermissionMode == PermissionModeSkip {
		a.UnregisterTool(tools.ToolNameAskUser)
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

// SetResolvedProvider switches the main provider wholesale to the named provider's
// full resolved config (provider instance, context window, thinking
// defaults) in one step. The name is resolved through the agent's FullConfig
// (config.BuildProvider; empty name = the default provider), so callers pass
// a provider config name instead of a pre-built *llm.ResolvedProvider.
//
// Unlike SetContextWindow + SetThinking — which only change the dimensions a
// caller explicitly passes, leaving the rest from the old provider —
// SetResolvedProvider replaces every dimension together, so a wholesale
// provider switch (e.g. tachi -p with a configured run provider) picks up
// the target's resolved ContextWindow / Thinking / ThinkingEffort without
// per-field overrides. The provider is re-wrapped for usage billing (so
// post-switch calls stay on the ledger; the ledger row's provider name comes
// from the provider itself — config-resolved providers carry it via
// NewNamedProvider). The applied ResolvedProvider is returned for callers
// that need display metadata (Type / Model / MaxTokens); it is the caller's
// copy — the agent owns a detached one.
func (a *AIAgent) SetResolvedProvider(providerName string) (*llm.ResolvedProvider, error) {
	if a.Config.FullConfig == nil {
		return nil, errors.New("SetResolvedProvider: no FullConfig to resolve provider from")
	}
	rp, err := llm.BuildProvider(a.Config.FullConfig, providerName)
	if err != nil {
		return nil, err
	}
	c := *rp // detach: the agent owns its Resolved (see AgentConfig.Resolved)
	c.Provider = wrapForUsage(c.Provider, a.usageRecorder(), a.Config.FullConfig)
	a.Config.Resolved = &c
	return rp, nil
}

// SetThinking updates the agent-level default thinking config (switch +
// effort). Used when switching models at runtime (/model): the target
// provider's thinking_level may differ from the current one, and without
// this the runLoop would keep applying the old model's defaults.
func (a *AIAgent) SetThinking(thinking *bool, effort string) {
	a.Config.Resolved.Thinking = thinking
	a.Config.Resolved.ThinkingEffort = effort
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
	if a.Config.Resolved.Provider == nil {
		return ""
	}
	return a.Config.Resolved.Provider.Model()
}

// Provider returns the main LLM provider for conversation turns.
// Resolved is never nil on a constructed agent (see AgentConfig.Resolved);
// Provider itself may be nil on bare agents with no main provider.
func (a *AIAgent) Provider() llm.Provider {
	return a.Config.Resolved.Provider
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
					text := strutil.TruncateFitted(msgs[j].Content, maxMsgLength)
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
	a.Config.Resolved.ContextWindow = window
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
	return a.Config.Resolved.ContextWindow
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

// sessionSeqBase returns the highest request Seq recorded so far in the
// current session (scanning both messages and api_requests). A new turn's
// requests continue numbering from here, keeping Seq monotonic across turns
// and process restarts (it is derived from disk, not in-memory state).
// Returns 0 when no session is active or nothing is recorded yet.
// Best-effort: a read failure is logged and treated as 0.
func (a *AIAgent) sessionSeqBase() int {
	sm := a.Config.SessionManager
	if sm == nil {
		return 0
	}
	cur := sm.Current()
	if cur == nil {
		return 0
	}

	base := 0
	if msgs, err := sm.LoadMessages(); err == nil {
		for i := range msgs {
			if msgs[i].Seq > base {
				base = msgs[i].Seq
			}
		}
	} else {
		a.Config.Logger.Warn(context.Background(), "Agent: sessionSeqBase: load messages failed", err)
	}
	if reqs, err := sm.LoadAPIRequests(cur.ID); err == nil {
		for i := range reqs {
			if reqs[i].Seq > base {
				base = reqs[i].Seq
			}
		}
	} else {
		a.Config.Logger.Warn(context.Background(), "Agent: sessionSeqBase: load api requests failed", err)
	}
	return base
}

// --- Tool Registry ---

func (a *AIAgent) RegisterTools() {
	// ReadFile/WriteFile/EditFile route through the ACP client's file system
	// (fs/read_text_file, fs/write_text_file) when the client declares the
	// corresponding capability; Bash routes through the client's terminal API
	// when it declares the terminal capability. Other frontends (tui/channel/
	// -p) never set these flags and keep the fully local implementations.
	readTool := tools.NewReadTool()
	if a.Frontend.ACPReadMode {
		readTool.SetACPMode(true)
	}
	a.Config.ToolRegistry.Register(readTool)

	writeTool := tools.NewWriteTool()
	if a.Frontend.ACPFileMode {
		writeTool.SetACPMode(true)
	}
	a.Config.ToolRegistry.Register(writeTool)

	editTool := tools.NewEditTool()
	if a.Frontend.ACPFileMode {
		editTool.SetACPMode(true)
	}
	a.Config.ToolRegistry.Register(editTool)

	a.Config.ToolRegistry.Register(tools.GlobTool{})
	a.Config.ToolRegistry.Register(tools.GrepTool{})
	// Bash — spill oversized outputs to disk via the shared tool_result
	// config (same policy as WebFetch). FullConfig may be nil in bare
	// agents; those keep the plain 1MB-buffer behavior (no spill).
	bashCfg := tools.BashToolConfig{ProcessManager: a.Config.ProcessManager}
	if a.Config.FullConfig != nil {
		bashCfg.ResultBaseDir = a.Config.FullConfig.ToolResult.ResultFileDir()
		bashCfg.MaxResultChars = a.Config.FullConfig.ToolResult.MaxResultChars()
	}
	bashTool := tools.NewBashTool(bashCfg)
	if a.Frontend.ACPTerminalBash {
		bashTool.SetACPMode(true)
	}
	a.Config.ToolRegistry.Register(bashTool)
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

		// WebFetch — always registered; the built-in native backend needs no
		// API key (firecrawl providers fall back to native on reserved
		// targets or quota/rate-limit errors).
		a.Config.ToolRegistry.Register(tools.NewWebFetchTool(tools.WebFetchToolConfig{
			Providers:      a.Config.FullConfig.WebFetch.Providers,
			Timeout:        a.Config.FullConfig.WebFetch.Timeout,
			Proxy:          a.Config.FullConfig.WebFetch.Proxy,
			ResultBaseDir:  a.Config.FullConfig.ToolResult.ResultFileDir(),
			MaxReturnChars: a.Config.FullConfig.ToolResult.MaxResultChars(),
		}))
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

	// 2. Remove from deferred pool
	if pool != nil {
		removed := pool.RemoveByServer(serverName)
		if removed > 0 {
			a.Config.Logger.Info(context.Background(), "MCP: removed tools from deferred pool for server", "count", removed, "server", serverName)
		}
	}

	// 3. Remove from every per-session discovered set — disabling a server
	// is a global fact, so no session should keep its tools marked as loaded.
	if a.Config.MCPManager != nil {
		a.Config.MCPManager.EachDiscoveredSet(func(set *mcp.DiscoveredSet) {
			for _, name := range set.List() {
				if strings.HasPrefix(name, prefix) {
					set.Remove(name)
					a.Config.Logger.Info(context.Background(), "MCP: removed tool from discovered set", "tool", name)
				}
			}
		})
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

// discoveredSetFor returns the MCP discovered set for the given session,
// lazily creating and restoring it via the manager. Returns nil when MCP is
// disabled or the session ID is empty. Internal helper.
func (a *AIAgent) discoveredSetFor(sessionID string) *mcp.DiscoveredSet {
	if a.Config.MCPManager == nil || sessionID == "" {
		return nil
	}
	return a.Config.MCPManager.SetFor(sessionID)
}

// currentSessionID returns the ID of the currently active session, or ""
// when no session manager/current session exists.
func (a *AIAgent) currentSessionID() string {
	if a.Config.SessionManager == nil {
		return ""
	}
	cur := a.Config.SessionManager.Current()
	if cur == nil {
		return ""
	}
	return cur.ID
}

// currentDiscoveredSet returns the discovered set of the currently active
// session, or nil when there is none. Internal helper.
func (a *AIAgent) currentDiscoveredSet() *mcp.DiscoveredSet {
	return a.discoveredSetFor(a.currentSessionID())
}

// AddDeferredMCPTools adds MCP tools to the deferred pool and marks the
// DeferredToolReminder as dirty so it fires on the next user message.
// This is used when a user manually enables an MCP server mid-session —
// tools are deferred (not immediately visible to the LLM) and hinted via
// the <system-reminder> deferred-tools block.
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

// sessionAwareDeferredTracker adapts the per-session discovered sets to the
// DeferredToolTracker interface. Contains checks the currently active
// session's set, so the deferred-tool reminder always reflects the session
// the user is talking to (TUI: the one session; channel: the per-thread
// session active when the reminder fires).
type sessionAwareDeferredTracker struct {
	a *AIAgent
}

func (t *sessionAwareDeferredTracker) Contains(name string) bool {
	set := t.a.currentDiscoveredSet()
	return set != nil && set.Contains(name)
}

// sessionAwareTracker returns a DeferredToolTracker backed by the current
// session's discovered set.
func (a *AIAgent) sessionAwareTracker() *sessionAwareDeferredTracker {
	return &sessionAwareDeferredTracker{a: a}
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
			Tracker:  a.sessionAwareTracker(),
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

// --- Usage billing ledger wiring ---
// See docs/2026-08-05-usage-billing.md. Every LLM call (loop turns, one-off
// runs, direct CreateChat calls) is recorded by a RecordingProvider wrapped
// around each provider at construction time.

// usageDirFn returns the ledger directory; injectable for tests (mirrors the
// oneoffSessionDirFn pattern in oneoff_recorder.go).
var usageDirFn = config.UsageDir

var (
	globalUsageRecorderOnce sync.Once
	globalUsageRecorder     *llm.UsageRecorder
)

// getGlobalUsageRecorder returns the process-wide usage ledger recorder,
// created lazily from <home>/usage/. Constructing it has no side effects —
// the directory is created only on the first Record. AgentConfig
// UsageRecorder injection (tests, per-process managers) takes precedence.
func getGlobalUsageRecorder() *llm.UsageRecorder {
	globalUsageRecorderOnce.Do(func() {
		globalUsageRecorder = llm.NewUsageRecorder(usageDirFn())
	})
	return globalUsageRecorder
}

// usageRecorder returns the agent's ledger recorder: the injected one wins,
// else the process-wide singleton.
func (a *AIAgent) usageRecorder() *llm.UsageRecorder {
	if a.Config.UsageRecorder != nil {
		return a.Config.UsageRecorder
	}
	return getGlobalUsageRecorder()
}

// UsageRecorder returns the agent's usage billing ledger recorder (the
// injected one, or the process-wide singleton). Frontends pass it to
// ComputeSessionUsage for /usage. Always non-nil in practice — the recorder
// is constructed without side effects and cannot fail; ComputeSessionUsage
// still tolerates nil defensively.
func (a *AIAgent) UsageRecorder() *llm.UsageRecorder {
	return a.usageRecorder()
}

// WrapProviderForUsage wraps p with the process-wide usage ledger recorder,
// resolving price snapshots via cfg. Idempotent — an already-wrapped provider
// passes through unchanged. This is the escape hatch for bare-agent call
// sites (dream runner, github bot, channel /model compact) that construct
// providers outside NewAIAgentWithConfig: wrapping here keeps their LLM calls
// billed even though kind/session tagging happens at RunOneOffStream. The
// ledger row's provider name comes from the provider itself.
func WrapProviderForUsage(p llm.Provider, cfg *config.Config) llm.Provider {
	return wrapForUsage(p, getGlobalUsageRecorder(), cfg)
}

// GlobalUsageRecorder returns the process-wide usage ledger recorder (the
// same instance NewAIAgentWithConfig uses when no recorder is injected).
// Channel/ACP/bot frontends use it for /usage and one-off provider wrapping.
func GlobalUsageRecorder() *llm.UsageRecorder {
	return getGlobalUsageRecorder()
}

// wrapForUsage wraps p with rec for usage billing, resolving price snapshots
// via cfg. Idempotent (WrapRecordingProvider); nil p passes through
// untouched.
//
// The ledger row's provider name is taken from the provider itself
// (Provider.ProviderName): config-resolved providers carry their config name
// via NewNamedProvider (through the decorator chain), so callers never pass
// or think about names. Bare providers (test mocks, ad-hoc construction)
// return "" and fall back to the type name.
//
// rec is always non-nil at every call site (injected recorder, or
// usageRecorder/getGlobalUsageRecorder which never return nil); WrapRecording
// Provider still guards nil defensively.
func wrapForUsage(p llm.Provider, rec *llm.UsageRecorder, cfg *config.Config) llm.Provider {
	if p == nil {
		return p
	}
	return llm.WrapRecordingProvider(p, rec, func(provider llm.Provider, model string) llm.ResolvedPrice {
		return llm.ResolveModelPriceAt(cfg, provider.ProviderName(), model, time.Now())
	})
}

// wrapUsageProviders wraps every provider in cfg with usage-ledger recording.
// Must run BEFORE the config is adopted so all derived providers
// (CompactStrategy, Setup*Provider, keyword extractor) see wrapped instances.
// rec == nil disables wrapping (no recording, no overhead).
func wrapUsageProviders(cfg *AgentConfig, rec *llm.UsageRecorder) {
	if rec == nil {
		return
	}
	// Provider names come from the providers themselves (NewNamedProvider);
	// nothing to thread through here.
	cfg.Resolved.Provider = wrapForUsage(cfg.Resolved.Provider, rec, cfg.FullConfig)
	cfg.TitleProvider = wrapForUsage(cfg.TitleProvider, rec, cfg.FullConfig)
	cfg.CommitProvider = wrapForUsage(cfg.CommitProvider, rec, cfg.FullConfig)
	cfg.ReviewProvider = wrapForUsage(cfg.ReviewProvider, rec, cfg.FullConfig)
	cfg.RunProvider = wrapForUsage(cfg.RunProvider, rec, cfg.FullConfig)
	cfg.SubagentProvider = wrapForUsage(cfg.SubagentProvider, rec, cfg.FullConfig)
	// Adversarial providers are intentionally NOT wrapped here: pre-resolved
	// pointers must survive construction untouched (tests pin this contract).
	// They are wrapped once after the Setup section via wrapResolvedAdversarial.
}

// wrapResolvedAdversarial wraps the adversarial providers (whether
// caller-pre-resolved in cfg or resolved by SetupAdversarialProviders from
// config) with usage billing. It runs after the Setup section — before that,
// the pre-resolved pointers must stay untouched (see wrapUsageProviders) and
// the config-resolved ones do not exist yet. WrapRecordingProvider's
// idempotence makes this safe even if a provider was already wrapped.
//
// The loop iterates the actual list (not the config), so caller-pre-resolved
// providers are covered even when config resolution has nothing to wrap;
// names come from the providers themselves, falling back to the type name.
func wrapResolvedAdversarial(cfg *AgentConfig, rec *llm.UsageRecorder, full *config.Config) {
	if rec == nil || cfg == nil {
		return
	}
	for i := range cfg.AdversarialModels {
		cfg.AdversarialModels[i] = wrapForUsage(cfg.AdversarialModels[i], rec, full)
	}
	cfg.AdversarialJudge = wrapForUsage(cfg.AdversarialJudge, rec, full)
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
