package agent

import (
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent/hooks"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
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
//     要改运行时行为请用对应 Setter（SetAutoApprovePolicyAsks、EnablePlanTool 等）。
//   - "只读配置"：构造后不变，通过 Config 引用
//   - "回填"：由 Configure() 初始化后回填
type AgentConfig struct {
	// --- 必填 ---
	// Resolved 是主 provider 及其解析结果——Provider / ContextWindow /
	// Thinking / ThinkingEffort 的单一来源。构造后永不为 nil（代码可直接
	// 解引用，无需 nil 防御）：
	//   - 调用方传入：NewAIAgentWithConfig 拷贝一份私有副本（调用方的
	//     *ResolvedProvider 不受 SetResolvedProvider / SetThinking /
	//     SetContextWindow 运行时改写的影响）；
	//   - nil：NewAIAgentWithConfig 经 FullConfig.DefaultProvider() 构造
	//     default provider（FullConfig 为 nil 时才退化为空 ResolvedProvider，
	//     即无主 provider——Fork / github / dream 等裸 agent 总是显式传入）。
	Resolved *llm.ResolvedProvider

	// --- 基础行为 ---
	MaxIterations  int
	PermissionMode PermissionMode

	// SkipConfigure keeps the agent "bare": NewAIAgentWithConfig skips the
	// configure step (built-in tool registration, skills, reminder collector,
	// subagent tool — everything AgentSystemConfig drives). Used by
	// Fork / dream / github agents that register explicit tool whitelists or
	// inherit tools from a parent, and by minimal test agents. With it set,
	// SystemConfig is ignored and NewAIAgentWithConfig never returns an error.
	SkipConfigure bool

	// Logger — 一般只读，但在 session 创建时会被附加 session_id
	//（ensureSessionAndRecordUser 在 session 创建时注入）。非 nil。
	Logger *logger.Logger

	// --- 功能开关（构造输入）---
	ACPFileMode            bool  // 构造输入 → 派生 FrontendConfig（路由 EditFile/WriteFile 写操作走客户端 fs/write_text_file）
	ACPReadMode            bool  // 构造输入 → 派生 FrontendConfig（路由 ReadFile 读操作走客户端 fs/read_text_file）
	ACPTerminalBash        bool  // 构造输入 → 派生 FrontendConfig（路由 Bash 执行走客户端 terminal API）
	PlanToolEnabled        bool  // 构造输入 → 派生 FrontendConfig
	TitleGenEnabled        *bool // 构造输入（nil = config-based）
	SkipMemoryRecall       bool  // 只读配置
	DisableSystemReminders bool  // 只读配置：禁用全部 system reminder（非交互模式如 `tachi -p`）
	DisableMCP             bool  // 只读配置：跳过 MCP server 连接与工具发现（非交互模式如 `tachi -p` / `tachi commit`）
	DisableSkills          bool  // 只读配置：跳过 skill store 扫描与 skill 工具注册（非交互模式如 `tachi -p` / `tachi commit`）
	AutoApprovePolicyAsks  bool  // 构造输入 → 初始化 PermState
	AskUserEnabled         bool  // 只读配置：即使 PermissionModeSkip 也保留 AskUserQuestion（desktop 等交互式前端）

	// --- 独立 Provider（构造输入；nil = fallback 到主 provider）---
	TitleProvider    llm.Provider
	CommitProvider   llm.Provider
	ReviewProvider   llm.Provider
	RunProvider      llm.Provider
	SubagentProvider llm.Provider

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
	// PermissionTrees 是这份策略读过的**所有**工作树（config.ProjectRootsFrom
	// 解析并去重，第一棵 = 主目录那棵）。多 workspace 会话（desktop 的额外 root、
	// ACP 的 additionalDirectories）里每棵树各带一份 .tachi/permissions.yaml，
	// 全部并集——只读主树会让第二棵树的绊线静默失效。
	PermissionTrees []string
	// PermissionRoot 是这份策略所依据的那棵树（FindProjectRootFrom 的结果）：
	// 项目级规则从 <root>/.tachi/permissions.yaml 读，判定闸门的 state 也要
	// 告诉模型"多外面才算外面"。空 = 没有项目（只有全局规则）。
	// 由 Configure（进程 cwd）或 SetPermissionRoot（会话自己的树）写入。
	PermissionRoot string

	// --- 工具与基础设施 ---
	ToolRegistry      *tools.Registry   // 由 NewAIAgent 创建
	SessionManager    SessionManager    // 新增
	ReminderCollector ReminderCollector // 新增（Configure 回填）
	MCPManager        *mcp.Manager      // 由 SharedMCP 更名；Configure 回填
	ProcessManager    *tools.ProcessManager
	SubagentRunner    tools.SubagentRunner // 新增（Configure 回填）
	SkillStore        *skill.Store         // 新增
	Memory            *MemoryState         // 新增（Configure 回填）

	// --- 钩子系统（由 Configure 初始化，构造期回填）---
	HookDispatcher *hooks.Dispatcher

	// --- 压缩策略（测试可 mock）---
	CompactStrategy CompactStrategy

	// --- usage 计费账本 ---
	// UsageRecorder 记录所有 LLM 调用的 token 用量（provider 层包装采集）。
	// nil = 使用进程级共享单例（<home>/usage/，见 getGlobalUsageRecorder）。
	// 测试注入自建 recorder 以隔离磁盘写入。
	UsageRecorder *llm.UsageRecorder

	// RootsFunc resolves the workspace roots a checkpoint should cover for a session.
	// nil = the record's own WorkingDir + AdditionalDirs, which is what every other entry
	// point wants (tui, acp, channel, -p leave it unset and are unaffected).
	//
	// The desktop sets it to the project-aware resolver: there the session RECORD is not the
	// authority on the workspace (a project owns the roots and the record holds a snapshot
	// taken when the session was created), so without this the agent would write in one tree
	// while its checkpoints kept recording another — a rewind would then restore files into a
	// directory nobody was working in, and report success. See
	// docs/2026-09-14-desktop-project-design.md §14.3.
	RootsFunc func(*session.Session) []string

	// MCPRootsFunc, when set, is what this agent's MCP connections answer the `roots`
	// capability with: the workspaces the servers may treat as their own.
	//
	// nil = declare nothing, which is the right default for a caller that cannot name its
	// workspace. A client that advertises the capability owes the server an answer, and a
	// roots-AWARE server confines file access to it (the official chrome-devtools server allows
	// only the OS temp dir when the client declares no roots at all — which is how a tool that
	// exports a file into the project ends up failing a path check).
	//
	// ACP sets it per session (its connections are per session, so the answer is that session's
	// cwd + additionalDirectories, and `session/resume` rewrites them in place);
	// the desktop sets it on the shared manager instead (one connection serves every session, so
	// the answer is the union of their workspaces — see desktopApp.mcpRoots).
	MCPRootsFunc mcp.RootsFunc

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
// PlanToolEnabled) and stored here for streamlined access.
type FrontendConfig struct {
	// ACPFileMode routes EditFile/WriteFile writes through
	// conn.WriteTextFile for Zed's inline diff UI. Edits never require
	// interactive confirmation in any mode (see EditTool); ACPFileMode only
	// changes the write channel. Set from clientCapabilities.fs.writeTextFile.
	ACPFileMode bool

	// ACPReadMode routes ReadFile through conn.ReadTextFile (client-side
	// file system) so the editor shows which file is being read. Set from
	// clientCapabilities.fs.readTextFile.
	ACPReadMode bool

	// ACPTerminalBash routes Bash execution through the client's terminal
	// API (terminal/create etc.) so the editor shows live command output.
	// Set from clientCapabilities.terminal. Only meaningful for ACP
	// sessions; other frontends never set it and keep the local Bash.
	ACPTerminalBash bool

	// PlanToolEnabled gates registration of the SavePlan tool. Only
	// frontends with a plan card UI enable it (ACP clients such as Zed,
	// and the desktop app), while the TUI and channel frontends have no
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
//
// Initial values come from AgentConfig construct inputs.
type PermissionState struct {
	// PermissionHandler is called by PermissionModeExternal to decide
	// tool execution. It receives the tool name, tool call ID, preview,
	// and raw args. Returns (approved, error).
	//
	// It is reached for a Bash `ask` (agent_permission.go) and for a file
	// tool's ask (agent_permission_files.go) — the latter is the file gate's
	// whole interactive path, and its refusal refuses THAT CALL rather than
	// the turn.
	PermissionHandler PermissionHandler

	// AutoApprovePolicyAsks makes PermissionModeSkip approve policy
	// "ask" decisions instead of denying them. Set only when a user
	// explicitly chose "allow all" (ACP); channel/subagent/one-off
	// runs leave it false.
	AutoApprovePolicyAsks bool

	// AllowAllAsks is 「本会话全部允许」 — the answer the interactive frontends give to a permission
	// card (the TUI's `a`, the desktop's 本会话全部允许 button). The policy keeps its own copy of this
	// flag (Policy.AllowAllAsksForSession), because the rules half is the policy's business and a
	// forked child shares the parent's policy; THIS copy is what the JUDGMENT GATES read, for two
	// reasons that have nothing to do with rules:
	//
	//   - the gates answer a different question ("should a human see this?") and run AFTER every
	//     rule has spoken, so a switch the rules own cannot reach them — the button said
	//     「本会话全部允许」 and the gate kept asking (the bug this field exists for);
	//   - a session with no policy at all (rules absent AND built-ins disabled) still has gates,
	//     and the approval has to have somewhere to live there too.
	//
	// Both flags are set together by AIAgent.AllowAllAsksForSession, and both are dropped when the
	// conversation ends — one answer, two readers.
	AllowAllAsks bool

	// MCPSearchAllowed / MCPSearchBlocked are the MCP search cost card's
	// session-wide answers (see mcp_budget.go). They are one-shot answers
	// recorded on the agent because that is what a session IS here: one agent
	// serves one conversation (channel caches one agent per thread, the desktop
	// builds one per session), and the bash policy's AllowAllAsksForSession is
	// scoped the same way.
	//
	//   - MCPSearchAllowed: the user chose 「本会话允许」 — no further cards.
	//   - MCPSearchBlocked: the user refused — the tool is not asked about again
	//     and not run again either, because a refusal that kept loading tools
	//     would be a button that cannot deliver.
	MCPSearchAllowed bool
	MCPSearchBlocked bool

	// askMu serializes interactive asks on this agent. Tool calls inside one
	// parallel group run concurrently and the TUI answers all of them through
	// ONE channel (Channels.ConfirmResp, capacity 1), so two cards waiting at
	// once would race for the same answer — a search could be approved by a
	// keystroke meant for its neighbour. Bash never needed this (its tool is
	// non-parallel, so its asks are already serial); MCPSearchTools is parallel.
	askMu sync.Mutex

	// askVerdicts memoizes the judgment gate's answer per (canonical command,
	// matched rule) for this session (agent_permission_jev.go). It exists for
	// cost, not for policy: a judgment is ~600 input tokens and ~0.5s, and a
	// build loop runs the SAME command text dozens of times — an agent's
	// commands rarely repeat ACROSS a session, but `go test ./...` repeats
	// within one. The key is the command text, so nothing is inferred about a
	// command that was not itself judged: this is not "this class is allowed".
	askVerdicts   map[string]askVerdict
	askVerdictsMu sync.Mutex
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
//  2. Loop goroutine only (no guard): StartTime, TraceID, APICalls, Seq,
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
	StartTime time.Time
	TraceID   string
	APICalls  int
	// Seq is the session-wide request sequence number. Initialized at turn
	// start from the current session's recorded maximum (sessionBoundary),
	// then incremented in lockstep with APICalls in runLoop — so Seq and
	// APICalls always stay 1:1. One-off runs (SkipSessionWrites) leave Seq
	// at 0: their sidecar transcripts start numbering from 1.
	Seq           int
	LengthRetries int
	// TurnCost / TurnCredit 累计本回合每次已完成 API 调用的花费（CNY）与
	// credit，计费口径与 usage 账本完全一致（RecordingProvider.record：
	// 逐次价格快照 + cache-miss 输入归一化 + credit_rate）。TurnComplete 时
	// 随 RunResult 带出，供各前端的回合 footer 展示。
	TurnCost          float64
	TurnCredit        float64
	Budget            *IterationBudget
	SkipSessionWrites bool
	OneoffRec         *oneoffRecorder
	// finished marks that this run's turn is over. currentRun is deliberately
	// never set to nil (channel mode reads it between turns), so "a run is in
	// flight" cannot be answered by that pointer alone — and a rewind must not
	// start while one is: truncating the conversation under a live turn is the
	// one thing that cannot be made safe here.
	finished bool
	// checkpointTurn is the checkpoint turn number this run belongs to, assigned
	// at the turn start (see beginCheckpointTurn). 0 means this run has no
	// checkpoint of its own — a one-off run, checkpoints disabled, or the record
	// failed — which disables the file half for the turn. Under mu because a
	// rewind command reads it from another goroutine.
	checkpointTurn int
	// produced records that the loop has put messages of its OWN into rs.Messages —
	// an assistant turn, its tool results, a steer, a loop reminder, or a compaction's
	// rewritten history. It is what separates a turn that failed before the model ever
	// answered from one that failed with work already behind it (see partialHistory),
	// which is the whole question at a failure site. Written together with Messages
	// under mu by append; read by the run goroutine only.
	produced bool

	// injectedReminders records what this TURN has already put in front of the model,
	// keyed by reminder piece name. The loop re-runs the collector after every tool
	// round, and an unchanged piece would otherwise be appended again — the same text
	// repeated once per iteration, paid for at the tail of a growing prompt (the one
	// place a prompt cache never helps). Only the run goroutine touches this.
	injectedReminders map[string]string
}

// seedReminders records the pieces a turn's first injection carried. Called once per
// turn, right where the reminder block is attached to the user message.
func (rs *RunState) seedReminders(pieces []systemreminder.Piece) {
	if rs.injectedReminders == nil {
		rs.injectedReminders = make(map[string]string, len(pieces))
	}
	for _, p := range pieces {
		rs.injectedReminders[p.Name] = systemreminder.RenderPieces([]systemreminder.Piece{p})
	}
}

// freshReminders drops the pieces this turn has already injected — the model is still
// carrying them in context, so repeating them costs tokens and says nothing new. What
// remains (a reminder that changed since it was last injected) is recorded as injected
// and returned for the caller to append.
func (rs *RunState) freshReminders(pieces []systemreminder.Piece) []systemreminder.Piece {
	var fresh []systemreminder.Piece
	for _, p := range pieces {
		rendered := systemreminder.RenderPieces([]systemreminder.Piece{p})
		if rs.injectedReminders[p.Name] == rendered {
			continue
		}
		if rs.injectedReminders == nil {
			rs.injectedReminders = make(map[string]string, 1)
		}
		rs.injectedReminders[p.Name] = rendered
		fresh = append(fresh, p)
	}
	return fresh
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

// markFinished records that this run's turn is over.
func (rs *RunState) markFinished() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.finished = true
}

// isFinished reports whether this run's turn is over.
func (rs *RunState) isFinished() bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.finished
}

// setCheckpointTurn records the checkpoint turn this run belongs to.
func (rs *RunState) setCheckpointTurn(turn int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.checkpointTurn = turn
}

// CheckpointTurn returns the checkpoint turn this run belongs to, or 0 when the
// run has none (see the field's comment).
func (rs *RunState) CheckpointTurn() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.checkpointTurn
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

// append appends messages to the stored slice under the mutex, recording that this turn has
// produced something of its own (see produced / partialHistory).
func (rs *RunState) append(msgs ...llm.Message) {
	if len(msgs) == 0 {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.Messages = append(rs.Messages, msgs...)
	rs.produced = true
}

// partialHistory is what a FAILED turn hands its frontend: the messages it produced, or nil when
// it produced none — rs.Messages is then still just the input, and a frontend that stored it
// would carry forward a question the model never answered.
//
// The line is drawn by the RUN rather than guessed from the failure, because it is not the same as
// "was this failure hard": a call that never opened a stream is the first-call case on iteration 1
// and a mid-turn failure on every later one, and an exhausted budget is always the latter. The
// exits that can land on either side of it ask here (agent_loop.go: the iteration budget, and the
// CreateChatStream error a rejected request arrives on).
func (rs *RunState) partialHistory() []llm.Message {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	if !rs.produced {
		return nil
	}
	return rs.Messages
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

	// lastPromptReal / lastPromptEstimate anchor the context size the frontends report: the prompt
	// size the provider BILLED for the most recent call (llm.PromptTokens), and the character
	// estimate that was made for that same call. The estimate alone is biased on mixed content —
	// measured 0.815x (≈18% low) on a long Chinese conversation with JSON tool results, the opposite
	// of the "conservative overestimate" the estimator's comment claimed — so the reported number is
	// the real anchor scaled by the estimate's movement since (a RATIO; see contextEstimate): the
	// error becomes one turn's delta instead of the whole prompt's bias. Zero means "no call yet in
	// this process".
	lastPromptReal     int64
	lastPromptEstimate int64
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

// setPromptAnchor records what the provider actually billed for the most recent call together
// with the estimate that was made for it (see the fields above).
func (s *convState) setPromptAnchor(real, estimate int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPromptReal = real
	s.lastPromptEstimate = estimate
}

// contextEstimate returns the context size to report: the last call's REAL prompt size, moved by
// how the estimate has changed since that call.
//
// The shape of the correction is a RATIO (real × current/atAnchor), not "real + delta", and that is
// deliberate: the estimate's bias is roughly proportional to the content, so scaling the anchor by
// the estimate's own movement keeps the correction working in BOTH directions. An additive delta
// with a floor at `real` looks right while the history only grows and is badly wrong the moment it
// does not: after a compaction the history is REPLACED by a summary, the estimate drops, and the
// ring would sit on the pre-compaction number until the next call — the very number compaction
// exists to bring down (`compact` asserts the meter falls). Scaling re-syncs it instead.
//
// With no anchor (nothing completed in this process yet) the plain estimate is all there is.
func (s *convState) contextEstimate() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastPromptReal <= 0 || s.lastPromptEstimate <= 0 || s.inputTokens <= 0 {
		return s.inputTokens
	}
	return s.lastPromptReal * s.inputTokens / s.lastPromptEstimate
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

// resetAfterRewind drops the parts of the conversation state that describe a
// history this session no longer has.
//
// The anchor goes because it is the REAL prompt size of the last call, and that
// call's prompt contained messages the rewind just removed: keeping it would
// report a size for a conversation that is not there (the estimate, which the
// caller recomputes right after, is the honest answer until the next call
// anchors again). The compaction baseline goes for the same reason — it may
// refer to a compaction that is now in the discarded future, which would hold
// the cooldown closed and block the compaction the shorter history may still
// need.
func (s *convState) resetAfterRewind() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPromptReal, s.lastPromptEstimate = 0, 0
	s.compactEstimate = 0
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
