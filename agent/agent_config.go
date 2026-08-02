package agent

import (
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent/hooks"
	"github.com/monsterxx03/tachi/agent/lsp"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// AgentSystemConfig 是 Configure 实际需要的配置子集。
// 替代原来的 *config.Config 参数，让 Configure 的依赖显式化。
type AgentSystemConfig struct {
	Memory         config.MemoryConfig
	MCPServers     []config.MCPServerConfig
	MCPToolRefresh config.MCPToolRefreshConfig
	WebSearch      config.WebSearchConfig
	WebFetch       config.WebFetchConfig
	ToolResult     config.ToolResultConfig
	LSP            config.LSPConfig
	Hooks          config.HooksConfig
	Herdr          config.HerdrConfig
	Subagent       config.SubagentConfig
	SystemReminder config.SystemReminderConfig
	Debug          config.DebugConfig
}

// SystemConfigFromConfig 从完整的 config.Config 提取 AgentSystemConfig。
// 入口点经过 Bootstrap 拿到 cfg 后，用这个函数快速构建系统配置。
func SystemConfigFromConfig(cfg *config.Config) AgentSystemConfig {
	return AgentSystemConfig{
		Memory:         cfg.Memory,
		MCPServers:     cfg.MCPServers,
		MCPToolRefresh: cfg.MCPToolRefresh,
		WebSearch:      cfg.WebSearch,
		WebFetch:       cfg.WebFetch,
		ToolResult:     cfg.ToolResult,
		LSP:            cfg.LSP,
		Hooks:          cfg.Hooks,
		Herdr:          cfg.Herdr,
		Subagent:       cfg.Subagent,
		SystemReminder: cfg.SystemReminder,
		Debug:          cfg.Debug,
	}
}

// SystemReminderConfig holds the subset of reminder configuration used by
// buildReminderCollectorFrom. Extracted from AgentSystemConfig to avoid
// passing the full struct through the agent's internal interfaces.
type SystemReminderConfig struct {
	GitReminder         *bool
	MemoryRecallLimit   int
	MemoryRecallTimeout time.Duration
	Pprof               *config.PprofConfig // nil or !Enabled → no pprof reminder
}

// AgentConfig 封装构造 AIAgent 所需的所有参数。
// 替代原来 NewAIAgent + 20 个 Set*/Setup* 调用的模式。
//
// 字段分类：
//   - "构造输入"：构造时被消费，派生到运行时字段（titleGenEnabled /
//     PermState / Frontend）。构造完成后这些字段不再被读取——改它们无效，
//     要改运行时行为请用对应 Setter（SetAutoApproveEdits 等）。
//   - "只读配置"：构造后不变，通过 Config 引用
//   - "回填"：由 Configure() 初始化后回填
type AgentConfig struct {
	// --- 必填 ---
	Provider      llm.Provider
	ContextWindow int64

	// --- 基础行为 ---
	MaxIterations  int
	PermissionMode PermissionMode

	// Logger — 一般只读，但在 session 创建时会被附加 session_id
	//（ensureSessionAndRecordUser 在 session 创建时注入）。非 nil。
	Logger *logger.Logger

	// --- 功能开关（构造输入）---
	ACPFileMode           bool  // 构造输入 → 派生 FrontendConfig
	PlanToolEnabled       bool  // 构造输入 → 派生 FrontendConfig
	TitleGenEnabled       *bool // 构造输入（nil = config-based）
	SkipMemoryRecall      bool  // 只读配置
	AutoApproveEdits      bool  // 构造输入 → 初始化 PermState
	AutoApprovePolicyAsks bool  // 构造输入 → 初始化 PermState

	// --- 独立 Provider（构造输入；nil = fallback 到主 provider）---
	TitleProvider    llm.Provider
	CommitProvider   llm.Provider
	ReviewProvider   llm.Provider
	RunProvider      llm.Provider
	SubagentProvider llm.Provider

	// --- 思考模式默认值（构造输入；nil/空 = provider/模型默认）---
	// 由配置 ProviderConfig.ThinkingLevel 解析而来（resolved.Provider.Thinking /
	// ThinkingEffort）。runLoop 在 ChatOptions 未显式指定时填充这两个值，
	// 因此所有前端（TUI / channel / ACP / one-off）自动继承模型级思考配置，
	// 而 /commit 等显式覆盖的调用不受影响。
	Thinking       *bool  // 默认思考开关（nil = provider 默认；false = 关闭）
	ThinkingEffort string // 默认思考强度（已按模型归一化；空 = 模型默认）

	// PendingSessionThinking 是无活跃 session 时由前端（TUI /thinking）设置的
	// 待定 per-session thinking override。ensureSessionAndRecordUser 在首次
	// 创建 session 时写入新 session 的 ThinkingLevel 并清空。空 = 无待定 override。
	PendingSessionThinking string

	// --- 对抗式审查 Provider（由 SetupAdversarialProviders 在构造期回填；
	//     空切片 = 未配置；nil 条目 = 配置了但解析失败，/review 启动前 fail fast）---
	AdversarialModels []llm.Provider // review.adversarial.models 的解析结果（按配置顺序）
	AdversarialJudge  llm.Provider   // review.adversarial.judge_model 的解析结果（nil = 未配置或失败）

	// --- 权限策略 ---
	PermissionPolicy *permission.Policy

	// --- 工具与基础设施 ---
	ToolRegistry      *tools.Registry   // 由 NewAIAgent 创建
	SessionManager    SessionManager    // 新增
	ReminderCollector ReminderCollector // 新增（Configure 回填）
	MCPManager        *mcp.Manager      // 由 SharedMCP 更名；Configure 回填
	ProcessManager    *tools.ProcessManager
	LSPManager        *lsp.LSPManager      // 新增（Configure 回填）
	SubagentRunner    tools.SubagentRunner // 新增（Configure 回填）
	SkillStore        *skill.Store         // 新增
	Memory            *MemoryState         // 新增（Configure 回填）

	// --- 钩子系统（由 Configure 初始化，构造期回填）---
	HookDispatcher *hooks.Dispatcher

	// --- 压缩策略（测试可 mock）---
	CompactStrategy CompactStrategy

	// --- 系统配置 ---
	SystemConfig AgentSystemConfig
	FullConfig   *config.Config
}

// ---------------------------------------------------------------------------
// FrontendConfig — 前端模式配置（纯只读）
// ---------------------------------------------------------------------------

// FrontendConfig holds frontend-mode configuration for an AIAgent.
// It is set once at construction time and is read-only afterwards.
//
// Fields are derived from construct inputs in AgentConfig (ACPFileMode,
// PlanToolEnabled) and stored here for streamlined access. The mode
// itself (auto/chat/plan) is not part of FrontendConfig — it is a
// runtime-mutable field on AIAgent, changed via SetMode().
type FrontendConfig struct {
	// ACPFileMode enables ACP file I/O for the EditFile tool. When true,
	// NeedsConfirmation returns false (Zed handles review) and ExecuteContext
	// routes writes through conn.WriteTextFile for inline diffs.
	ACPFileMode bool

	// PlanToolEnabled gates registration of the SavePlan tool. Only ACP
	// sessions enable it — ACP clients (e.g. Zed) render a structured plan
	// card from SavePlan calls, while the TUI and channel frontends have no
	// corresponding plan card UI.
	PlanToolEnabled bool
}

// ---------------------------------------------------------------------------
// RuntimeChannels — 通信通道（agent 生命周期，只读）
// ---------------------------------------------------------------------------

// RuntimeChannels holds the communication channels that live for the
// agent's entire lifetime. These are created once in NewAIAgent and
// never replaced.
//
// steer channel is NOT included here — it is per-run state, created
// and destroyed with each run, passed via RunOption.
type RuntimeChannels struct {
	// ConfirmResp receives tool confirmation responses from the
	// frontend (TUI/ACP). Buffered with capacity 1.
	ConfirmResp chan ConfirmResponse

	// AskUserResp receives AskUserQuestion responses from the
	// frontend (TUI). Buffered with capacity 1.
	AskUserResp chan tools.AskUserResult
}

// ---------------------------------------------------------------------------
// PermissionState — 运行时可变权限（session 级）
// ---------------------------------------------------------------------------

// PermissionState holds the runtime-variable permission state of an
// AIAgent. Unlike AgentConfig (read-only after construction), these
// fields are modified during a session by user actions:
//   - PermissionHandler: set when PermissionModeExternal is active
//   - AutoApprovePolicyAsks: set when user chooses "allow all"
//   - AutoApproveEdits: set from config or when user picks "always"
//     on an edit confirmation (session-scoped)
//
// Initial values come from AgentConfig.AutoApprove* construct inputs.
type PermissionState struct {
	// PermissionHandler is called by PermissionModeExternal to decide
	// tool execution. It receives the tool name, tool call ID, diff
	// preview, and raw args. Returns (approved, error).
	PermissionHandler PermissionHandler

	// AutoApprovePolicyAsks makes PermissionModeSkip approve policy
	// "ask" decisions instead of denying them. Set only when a user
	// explicitly chose "allow all" (ACP); channel/subagent/one-off
	// runs leave it false.
	AutoApprovePolicyAsks bool

	// AutoApproveEdits skips EditFile confirmation prompts. Set from
	// config tui.auto_approve_edits, or at runtime when the user picks
	// "always" on an edit confirmation (session-scoped). Affects only
	// EditFile — unlike PermissionModeSkip, bash policy asks still prompt.
	AutoApproveEdits bool
}

// ---------------------------------------------------------------------------
// RunState — 实时运行状态（per-run）
// ---------------------------------------------------------------------------

// RunState holds the per-run mutable state of an AIAgent.
//
// It replaces turnState + loopState: the message slice (which previously
// existed in both with a deferred sync) lives here under one mutex, so
// slash-command handlers and the TUI can read it while the loop writes.
// Conversation-scoped rolling state (token estimate, compact cooldown,
// last message date) lives on AIAgent.conv instead — see convState
// for why those are NOT per-run.
//
// Fields are classified into two visibility tiers:
//
//  1. Concurrent access (mu guards): Messages — read by slash-command
//     handlers and the TUI while the loop appends. Note the loop goroutine
//     itself is the sole writer, so it may read Messages WITHOUT holding
//     mu (its own writes are already visible to it); mu exists purely to
//     synchronize cross-goroutine readers via snapshotMessages.
//
//  2. Loop goroutine only (no guard): StartTime, TraceID, APICalls,
//     LengthRetries, Budget, SkipSessionWrites, OneoffRec — written at run
//     start or by the loop itself; every access happens on the run goroutine.
//
// Lifecycle:
//   - Created at the start of RunConversationStream / RunOneOffStream
//   - RunConversationStream publishes it to AIAgent.currentRun for concurrent
//     readers; RunOneOffStream keeps it local (one-off runs are invisible to
//     GetLastMessages & friends)
//   - All writes happen-before eventCh is closed
//   - Pointer retained in currentRun until the next run replaces it
//     (never set to nil — channel mode reads currentRun between turns)
type RunState struct {
	mu sync.RWMutex

	// ── 并发读写（loop 写、slash-command/TUI 读）──
	Messages []llm.Message

	// ── 仅 run goroutine 访问（无需锁，归属此处仅为聚合）──
	StartTime         time.Time
	TraceID           string
	APICalls          int
	LengthRetries     int
	Budget            *IterationBudget
	SkipSessionWrites bool
	OneoffRec         *oneoffRecorder
}

// snapshotMessages returns a shallow copy of the stored message slice.
func (rs *RunState) snapshotMessages() []llm.Message {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	if rs.Messages == nil {
		return nil
	}
	out := make([]llm.Message, len(rs.Messages))
	copy(out, rs.Messages)
	return out
}

// begin records the start time and trace ID for a new run.
func (rs *RunState) begin(traceID string) {
	rs.StartTime = time.Now()
	rs.TraceID = traceID
}

// elapsed returns the duration since the current run began.
func (rs *RunState) elapsed() time.Duration {
	return time.Since(rs.StartTime)
}

func (rs *RunState) trace() string {
	return rs.TraceID
}

// append appends messages to the stored slice under the mutex.
func (rs *RunState) append(msgs ...llm.Message) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.Messages = append(rs.Messages, msgs...)
}

// ---------------------------------------------------------------------------
// convState — 会话级滚动状态（agent 生命周期）
// ---------------------------------------------------------------------------

// convState holds conversation-scoped mutable state that outlives any single
// run but is continuously updated BY runs. Unlike RunState (per-run, replaced
// every turn), these values carry meaning across turns:
//
//   - inputTokens / tokenBreakdown: the latest context estimate. Read between
//     turns (channel /usage, TUI statusbar) and before the first run (ACP
//     session/load primes the initial UsageUpdate with it).
//   - compactEstimate: the estimate at the last auto-compact. The cooldown
//     must survive across turns — its purpose is preventing repeated
//     compaction of the same session within one conversation.
//   - lastMessageDate: date of the most recent user message, so the date-time
//     reminder can fire when the calendar day changes between turns.
//
// It carries its own mutex because channel mode shares one cached agent
// between the turn goroutine (writer) and slash-command readers (the genuine
// data race that motivated the original turnState).
type convState struct {
	mu sync.RWMutex

	inputTokens     int64
	tokenBreakdown  tokenbreakdown.Breakdown
	compactEstimate int64
	lastMessageDate string
}

func newConvState() *convState { return &convState{} }

// tokens returns the current token estimate.
func (s *convState) tokens() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inputTokens
}

// setEstimate records a new token estimate and its breakdown together, so
// readers never observe one without the other.
func (s *convState) setEstimate(total int64, tb tokenbreakdown.Breakdown) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputTokens = total
	s.tokenBreakdown = tb
}

// estimateSnapshot returns the token total and its breakdown read under a
// single lock.
func (s *convState) estimateSnapshot() (int64, tokenbreakdown.Breakdown) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inputTokens, s.tokenBreakdown
}

// snapshotBreakdown returns a copy of the current breakdown.
func (s *convState) snapshotBreakdown() tokenbreakdown.Breakdown {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tokenBreakdown
}

// compactCooldown reports whether the estimate has grown less than 20% since
// the last compaction. Current estimate and compact baseline are read under
// one lock so the comparison never mixes two estimates.
func (s *convState) compactCooldown() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.compactEstimate == 0 {
		return false
	}
	return float64(s.inputTokens)/float64(s.compactEstimate) < 1.2
}

// setCompactEstimate records the token estimate at compaction time.
func (s *convState) setCompactEstimate(v int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compactEstimate = v
}

// messageDate returns the recorded date of the last user message.
func (s *convState) messageDate() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastMessageDate
}

// setMessageDate records the date of the last processed user message.
func (s *convState) setMessageDate(d string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastMessageDate = d
}
