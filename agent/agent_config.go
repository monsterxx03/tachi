package agent

import (
	"time"

	"github.com/monsterxx03/tachi/agent/mcp"
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
	}
}

// SystemReminderConfig holds the subset of reminder configuration used by
// buildReminderCollectorFrom. Extracted from AgentSystemConfig to avoid
// passing the full struct through the agent's internal interfaces.
type SystemReminderConfig struct {
	GitReminder         *bool
	MemoryRecallLimit   int
	MemoryRecallTimeout time.Duration
}

// AgentConfig 封装构造 AIAgent 所需的所有参数。
// 替代原来 NewAIAgent + 20 个 Set*/Setup* 调用的模式。
type AgentConfig struct {
	// --- 必填 ---
	Provider      llm.Provider
	ContextWindow int64

	// --- 基础行为 ---
	MaxIterations  int
	Logger         *logger.Logger
	PermissionMode PermissionMode

	// --- 功能开关 ---
	ACPFileMode           bool
	PlanToolEnabled       bool
	TitleGenEnabled       *bool  // nil = use SetupTitleProvider (config-based); false = disable
	SkipMemoryRecall      bool
	AutoApproveEdits      bool
	AutoApprovePolicyAsks bool

	// --- 独立 Provider（nil = fallback 到主 provider）---
	TitleProvider    llm.Provider
	CommitProvider   llm.Provider
	ReviewProvider   llm.Provider
	RunProvider      llm.Provider
	SubagentProvider llm.Provider

	// --- 共享依赖 ---
	ProcessManager *tools.ProcessManager
	SharedMCP      *mcp.Manager

	// --- 系统配置（传给 Configure）---
	SystemConfig AgentSystemConfig
	// FullConfig 当系统配置是从完整的 Config 提取时，保留引用以便内部使用。
	// 使用 SystemConfigFromConfig 时会自动设置此字段。
	FullConfig *config.Config
}
