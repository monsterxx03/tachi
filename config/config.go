package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/creasty/defaults"
	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"gopkg.in/yaml.v3"
)

const (
	DefaultMaxTokens          = 128000
	DefaultMaxIterations      = 50
	configDirName             = ".tachi"
	configFileName            = "config.yaml"
	inputHistoryFileName      = "input_history"
	sessionDirName            = "session"
	logsDirName               = "logs"
	mcpTokensDirName          = "mcp_tokens"
	skillsDirName             = "skills"
	weixinStateDirName        = "weixin"
	researchDirName           = "research"
	cronStoreFileName         = "crons.json"
	toolResultsDirName        = "tool_results"
	oneoffDirName             = "oneoff"
	defaultToolResultMaxChars = 50000
)

// baseDir is the base directory for all tachi state. Default: $HOME/.tachi.
// Can be overridden via SetBaseDir() before any other config functions are called.
var baseDir string

// SetBaseDir overrides the default base directory. Must be called before
// any other config functions (e.g., from main with a --home CLI flag).
func SetBaseDir(dir string) {
	baseDir = dir
}

// BaseDir returns the resolved base directory. If SetBaseDir was called,
// returns that value. Otherwise falls back to $HOME/.tachi.
func BaseDir() string {
	if baseDir != "" {
		return baseDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Best-effort fallback: return relative ".tachi"
		return configDirName
	}
	return filepath.Join(home, configDirName)
}

// FindProjectRoot walks up from the current working directory to find the
// nearest git repository root (directory containing .git). If found, returns
// that directory. Otherwise returns the current working directory as-is.
// Returns "" if os.Getwd() fails.
func FindProjectRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := cwd
	for {
		gitPath := filepath.Join(dir, ".git")
		if _, err := os.Stat(gitPath); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root — stop
			break
		}
		dir = parent
	}
	return cwd
}

// ModelPricing 定价覆盖（CNY / 1M tokens）。
// 设置后覆盖内置价格表；设为 0 禁用该项成本计算；留空（nil）时使用内置价格表。
type ModelPricing struct {
	InputPrice              *float64 `yaml:"input_price,omitempty"`
	OutputPrice             *float64 `yaml:"output_price,omitempty"`
	CacheReadInputPrice     *float64 `yaml:"cache_read_input_price,omitempty"`
	CacheCreationInputPrice *float64 `yaml:"cache_creation_input_price,omitempty"`
}

// ModelSpec 汇总模型级运行时属性：上下文窗口、定价、思考级别。
// 通过 ProviderConfig.Spec 嵌套配置（不向前兼容旧版平铺字段）。
type ModelSpec struct {
	// ContextWindow 手动覆盖模型上下文窗口（tokens）。
	ContextWindow *int64 `yaml:"context_window,omitempty"`

	// ThinkingLevel 控制思考模式强度。不同模型支持不同级别：
	//   - "none"             关闭思考模式
	//   - "low"/"medium"/"high"/"xhigh"/"max"  设置思考强度
	// 空 = 使用模型内置默认（DeepSeek: 思考开启、effort high；Anthropic: adaptive）。
	// 请求的级别会原样透传给 API，由服务端映射到模型实际的推理强度
	// （如 DeepSeek thinking_mode 文档的 effort 映射表）。
	ThinkingLevel string `yaml:"thinking_level,omitempty"`

	// Pricing 定价覆盖（可选，覆盖内置价格表）。
	Pricing *ModelPricing `yaml:"pricing,omitempty"`
}

type ProviderConfig struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"`
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`

	Spec ModelSpec `yaml:"spec"`
}

type WebSearchConfig struct {
	Type       string        `yaml:"type" default:"brave"` // brave, serper, serpapi
	Key        string        `yaml:"key"`
	Timeout    time.Duration `yaml:"timeout" default:"30s"`
	MaxResults int           `yaml:"max_results" default:"10"`
	Proxy      string        `yaml:"proxy"` // Optional proxy URL (e.g. socks5://127.0.0.1:1080, http://127.0.0.1:8080)
}

type WebFetchConfig struct {
	Timeout time.Duration `yaml:"timeout" default:"60s"` // HTTP request timeout
	Proxy   string        `yaml:"proxy"`                 // Optional proxy URL (e.g. socks5://127.0.0.1:1080)
}

// MCPTransportType represents the type of MCP transport protocol
type MCPTransportType string

const (
	MCPTransportStdio MCPTransportType = "stdio"
	MCPTransportHTTP  MCPTransportType = "http"
)

// MCPOAuthConfig holds OAuth2 configuration for HTTP MCP servers.
// If ClientID is empty, dynamic client registration (DCR) is attempted
// automatically on the first OAuth flow.
type MCPOAuthConfig struct {
	ClientID              string   `json:"client_id,omitempty"`
	ClientSecret          string   `json:"client_secret,omitempty"`
	ClientURI             string   `json:"client_uri,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	AuthServerMetadataURL string   `json:"auth_server_metadata_url,omitempty"`          // Override auto-discovery
	CallbackHost          string   `json:"callback_host,omitempty" default:"127.0.0.1"` // OAuth callback host
	CallbackPort          int      `json:"callback_port,omitempty"`                     // OAuth callback port (default: auto)
}

// MCPServerConfig represents a single MCP server connection configuration.
// Loaded from JSON files (mcp.json).
type MCPServerConfig struct {
	Name    string            `json:"name"`
	Type    MCPTransportType  `json:"type"` // "stdio" or "http"
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`                    // For http transport
	Headers map[string]string `json:"headers,omitempty"`                // For http transport
	Proxy   string            `json:"proxy,omitempty"`                  // Optional proxy URL (only for http transport; e.g. socks5://127.0.0.1:1080)
	Timeout Duration          `json:"timeout,omitempty"`                // Connect timeout (default: 10s)
	Enabled *bool             `json:"enabled,omitempty" default:"true"` // Whether to load this server
	OAuth   *MCPOAuthConfig   `json:"oauth,omitempty"`                  // OAuth2 configuration (http transport only)

	// ToolSearch-specific options
	AlwaysLoadTools []string          `json:"always_load_tools,omitempty"` // Tool names to always load (skip ToolSearch)
	SearchHints     map[string]string `json:"search_hints,omitempty"`      // Override search hints: tool_name -> hint
	Whitelist       []string          `json:"whitelist,omitempty"`         // If set, only these tools are loaded from the server; all others are ignored
	Blacklist       []string          `json:"blacklist,omitempty"`         // Tools to exclude. When both whitelist and blacklist are configured, blacklist filters from the whitelisted set (whitelist ∩ ¬blacklist)
	// Empty string means it came from a base file (always loaded).
	// Set internally during config loading; not serialized.
	Profile string `json:"-"`
}

// HasOAuth returns true if the server has OAuth2 configured.
func (srv *MCPServerConfig) HasOAuth() bool {
	return srv.OAuth != nil
}

// TokenStorageName returns the key used for on-disk token storage.
// For HTTP servers, the URL host is used so that servers behind the same
// gateway (same host) automatically share a single OAuth token.
// For stdio servers, the server name is used.
// Profile servers append the profile name to avoid cross-environment collisions.
func (srv *MCPServerConfig) TokenStorageName() string {
	base := srv.Name
	if srv.Type == MCPTransportHTTP && srv.URL != "" {
		if u, err := url.Parse(srv.URL); err == nil && u.Host != "" {
			// Sanitize host for use as filename (replace : with _)
			base = strings.ReplaceAll(u.Host, ":", "_")
		}
	}
	if srv.Profile == "" {
		return base
	}
	return base + "_" + srv.Profile
}

// IsEnabled returns whether the server should be loaded. Defaults to true.
func (srv *MCPServerConfig) IsEnabled() bool {
	return srv.Enabled == nil || *srv.Enabled
}

// TUIConfig 控制终端界面行为。
type TUIConfig struct {
	InputHistoryLimit int   `yaml:"input_history_limit" default:"10"`
	AutoApproveEdits  bool  `yaml:"auto_approve_edits"`                // true = EditFile 编辑不再弹确认（仅 TUI；不影响 Bash 权限规则的 ask 弹窗）
	NotifyOnComplete  *bool `yaml:"notify_on_complete" default:"true"` // 是否在 LLM 回合结束后发送终端通知
}

// NotifyEnabled 返回是否启用了回合结束通知。默认启用。
func (t *TUIConfig) NotifyEnabled() bool {
	return t.NotifyOnComplete == nil || *t.NotifyOnComplete
}

type SystemReminderConfig struct {
	GitReminder *bool `yaml:"git_reminder" default:"true"` // set false to disable
}

// WeixinConfig holds iLink Bot channel configuration.
type WeixinConfig struct {
	Enabled  bool   `yaml:"enabled"`
	StateDir string `yaml:"state_dir"` // State directory (default: ~/.tachi/weixin)
	RouteTag string `yaml:"route_tag"` // Optional SKRouteTag for routing
	Greeting string `yaml:"greeting"`  // Startup greeting sent to the admin user after login (default: "👋 你好！Tachi 已启动，随时可以开始工作～")
	BotAgent string `yaml:"bot_agent"` // v2.3.1+: bot_agent identity string (default: "Tachi")
}

// CronConfig holds cron scheduler configuration (only active in channel mode).
type CronConfig struct {
	Enabled          *bool         `yaml:"enabled" default:"true"` // whether to enable cron (default: true in channel mode)
	StorePath        string        `yaml:"store_path"`             // custom store path (default: ~/.tachi/crons.json)
	MaxConcurrent    int           `yaml:"max_concurrent" default:"3"`
	ExecutionTimeout time.Duration `yaml:"execution_timeout" default:"5m"`
}

// IsEnabled returns whether the cron scheduler is enabled. Defaults to true.
func (c *CronConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// DreamConfig holds configuration for AutoDream — the background memory
// consolidation system that runs via SystemScheduler in channel mode.
type DreamConfig struct {
	Enabled         bool          `yaml:"enabled" default:"false"`
	Schedule        string        `yaml:"schedule" default:"0 3 * * *"`     // cron expression (default: daily 3 AM)
	Provider        string        `yaml:"provider"`                         // provider name (empty → use main provider)
	MaxConcurrent   int           `yaml:"max_concurrent" default:"3"`       // max parallel dream sub-agents
	SubagentTimeout time.Duration `yaml:"subagent_timeout" default:"10m"`   // timeout for each dream sub-agent
	SubagentMaxIter int           `yaml:"subagent_max_iters" default:"30"`  // max iterations for dream sub-agent
	MaxMessageChars int           `yaml:"max_message_chars" default:"2000"` // max chars per message in dream prompt
}

// ACPConfig holds configuration for the ACP (Agent Client Protocol) agent mode.
type ACPConfig struct {
	// ConnectConfiguredMCP controls whether to connect MCP servers from mcp.json
	// when a new ACP session starts. Defaults to true.
	ConnectConfiguredMCP *bool `yaml:"connect_configured_mcp"`
	// MCPConflictPolicy determines what happens when an editor sends an MCP server
	// with the same name as one in mcp.json. "client_wins" (default) uses the
	// editor's version; "agent_wins" keeps the mcp.json version.
	MCPConflictPolicy string `yaml:"mcp_conflict_policy" default:"client_wins"`
}

// ShouldConnectConfiguredMCP returns whether to connect mcp.json MCP servers in ACP mode.
func (c *ACPConfig) ShouldConnectConfiguredMCP() bool {
	return c.ConnectConfiguredMCP == nil || *c.ConnectConfiguredMCP
}

// CompactConfig holds configuration for session compaction.
// Used by both the /compact command and auto-compact (when enabled).
type CompactConfig struct {
	Timeout   time.Duration `yaml:"timeout" default:"5m"`      // Timeout for the compaction LLM call
	MaxTokens int           `yaml:"max_tokens" default:"4096"` // Max tokens for the compact response (summary)
	Auto      *bool         `yaml:"auto" default:"true"`       // Enable automatic compaction when context is near limit
	Threshold float64       `yaml:"threshold" default:"0.8"`   // Trigger ratio: lastInputTokens / contextWindow >= threshold
}

// ToolResultConfig holds configuration for tool result size limits and
// file persistence of oversized results. When a tool result exceeds
// MaxChars, the full output is saved to disk and a truncated preview
// with file path is returned to the LLM instead.
type ToolResultConfig struct {
	MaxChars int    `yaml:"max_chars"` // max chars for tool result passed to LLM (default 50000, 0 = no limit)
	FileDir  string `yaml:"file_dir"`  // dir for storing oversized results (default: ~/.tachi/tool_results)
}

// MaxResultChars returns the effective max chars threshold. Falls back to
// defaultToolResultMaxChars (50000) when unconfigured. Returns 0 only when
// explicitly set to 0 (which means no limit).
func (c *ToolResultConfig) MaxResultChars() int {
	if c.MaxChars == 0 && c.FileDir == "" {
		return defaultToolResultMaxChars
	}
	return c.MaxChars
}

// ResultFileDir returns the effective directory for storing oversized results.
// Falls back to ~/.tachi/tool_results when unconfigured.
func (c *ToolResultConfig) ResultFileDir() string {
	if c.FileDir != "" {
		return c.FileDir
	}
	return ToolResultsDir()
}

// SubagentConfig holds configuration for sub-agent execution.
// When Provider is empty, the main provider is used.
type SubagentConfig struct {
	Provider       string `yaml:"provider"`         // provider name, empty → use main
	MaxIterations  int    `yaml:"max_iterations"`   // default: 50 (hardcoded fallback)
	MaxConcurrency int    `yaml:"max_concurrency"`  // default: 4 (hardcoded fallback)
	MaxOutputChars int    `yaml:"max_output_chars"` // default: 16384 (hardcoded fallback)
	Thinking       bool   `yaml:"thinking"`         // default: false

	// Worktree enables git worktree isolation for sub-agents (default: false).
	Worktree        bool   `yaml:"worktree"`
	WorktreeDir     string `yaml:"worktree_dir"`     // worktree storage directory (default: os.TempDir())
	WorktreeCleanup *bool  `yaml:"worktree_cleanup"` // clean up after completion (default: true)
	WorktreeBranch  string `yaml:"worktree_branch"`  // default branch for worktree checkout (empty = detached HEAD)
}

// AdversarialReviewConfig holds the per-round model configuration for the
// adversarial (multi-round) /review mode. Rounds are NOT configured here —
// multi-round review is entered explicitly via "/review N" (N ≥ 2); no
// argument means the single-round path. See
// docs/2026-07-30-adversarial-review-design.md.
//
// NOTE (creasty/defaults behavior): Adversarial is a *pointer* without a
// default tag, so defaults.Set() does NOT allocate it when the YAML omits the
// `adversarial:` key — callers must check `adv != nil` before touching
// Models/JudgeModel (SetupAdversarialProviders and CheckAdversarialProviders
// both gate on it). Old configs that still carry the removed `enabled:` /
// `rounds:` keys load fine: yaml.Unmarshal is non-strict and ignores unknown
// keys.
type AdversarialReviewConfig struct {
	Models     []string `yaml:"models"`      // per-round model names (empty → all rounds use review.provider)
	JudgeModel string   `yaml:"judge_model"` // fixed model for the final round (empty → modulo assignment)
}

// ReviewConfig holds configuration for the /review code review command.
// When Provider is empty, the main provider is used.
// AllowedTools defaults to [Bash, ReadFile, WriteFile, Glob, Grep] when empty (handled in code).
type ReviewConfig struct {
	Provider      string                   `yaml:"provider"`                     // provider name, empty → use main
	MaxIterations int                      `yaml:"max_iterations" default:"200"` // iteration budget
	AllowedTools  []string                 `yaml:"allowed_tools"`                // default: [Bash, ReadFile, Glob, Grep] (code-level fallback)
	Thinking      *bool                    `yaml:"thinking" default:"false"`     // enable extended thinking
	Adversarial   *AdversarialReviewConfig `yaml:"adversarial"`                  // multi-round mode (nil when unconfigured, see note above)
}

// ChannelConfig groups configuration for all IM channel backends.
//
// Legacy field: Weixin is a typed config for a built-in channel.
// For backward compatibility, if Weixin.Enabled is set
// but no matching entry exists in Channels, it's auto-activated.
//
// Generic field: Channels maps channel name to its raw config. Each entry
// must match the Name() of a registered Channel (via channel.Register).
// Private channel repositories can drop a single Go file with:
//
//	import _ "private-repo/tachi-channel-mybots"
//
// and then activate it via config.yaml:
//
//	channel:
//	  channels:
//	    mybots:
//	      enabled: true
//	      token: "xxx"
type ChannelConfig struct {
	Weixin   WeixinConfig              `yaml:"weixin"`
	Channels map[string]map[string]any `yaml:"channels"`
	Whisper  ChannelWhisperConfig      `yaml:"whisper"`
}

// ChannelWhisperConfig holds channel-mode whisper settings for group chat
// selective reply. When enabled, non-directed messages in group chats are
// batched and presented to the agent as ambient context rather than
// triggering individual agent turns.
type ChannelWhisperConfig struct {
	// Enabled controls whether the whisper pipeline is active (default: true).
	// Use pointer so false (explicit disable) can be distinguished from unset.
	Enabled *bool `yaml:"enabled"`

	// AmbientBatchWindow is the duration to buffer non-directed messages
	// before triggering an ambient turn (default: 30s).
	AmbientBatchWindow time.Duration `yaml:"ambient_batch_window" default:"30s"`

	// AmbientMaxIterations is the iteration budget for ambient turns (default: 5).
	AmbientMaxIterations int `yaml:"ambient_max_iterations" default:"5"`

	// AmbientMaxBuffer is the maximum number of messages buffered per thread.
	// When exceeded, oldest messages are dropped (FIFO). Default: 10.
	AmbientMaxBuffer int `yaml:"ambient_max_buffer" default:"10"`

	// AmbientMaxHistory is the maximum number of ambient conversation entries
	// retained in memory for cross-turn context. Oldest entries are dropped
	// (FIFO) when exceeded. Default: 50.
	AmbientMaxHistory int `yaml:"ambient_max_history" default:"50"`

	// AmbientCooldown is the minimum interval between two ambient turns
	// on the same thread (default: 0, no cooldown).
	AmbientCooldown time.Duration `yaml:"ambient_cooldown"`

	// SilenceMarker is the string the agent replies with to indicate
	// it has nothing to say. Matching is lenient (trim + case-insensitive).
	// Default: "[SILENT]".
	SilenceMarker string `yaml:"silence_marker" default:"[SILENT]"`

	// AmbientTools is the tool whitelist for ambient turns.
	// Empty (default) = [MemoryRecall, RecordMemory, WebFetch, WebSearch].
	AmbientTools []string `yaml:"ambient_tools"`

	// AmbientMaxTokens is the max_tokens budget for ambient turns.
	// Default: falls back to agent.DefaultMaxTokens.
	AmbientMaxTokens int `yaml:"ambient_max_tokens"`
}

// WhisperEnabled returns whether whisper is enabled, defaulting to true if unset.
func (c *ChannelWhisperConfig) WhisperEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ActiveChannels returns the raw configs for every enabled channel,
// merging the legacy typed Weixin config into the generic Channels
// map when needed.
//
// For backward compatibility: if the legacy Weixin typed config has
// Enabled set and there's no matching key in Channels, it's auto-included.
// If both exist, the generic Channels entry takes precedence.
func (cc *ChannelConfig) ActiveChannels() map[string]map[string]any {
	result := make(map[string]map[string]any, len(cc.Channels))

	// Copy generic channels that are enabled.
	for name, rawCfg := range cc.Channels {
		if isEnabled, ok := rawCfg["enabled"]; !ok || !toBool(isEnabled) {
			continue
		}
		result[name] = rawCfg
	}

	// Legacy weixin: include if enabled and not already present in Channels.
	if _, inChannels := result["weixin"]; !inChannels && cc.Weixin.Enabled {
		result["weixin"] = map[string]any{
			"enabled":   true,
			"state_dir": cc.Weixin.StateDir,
			"route_tag": cc.Weixin.RouteTag,
			"greeting":  cc.Weixin.Greeting,
		}
	}

	return result
}

// toBool converts a YAML-parsed value to a boolean. Handles both actual
// booleans (from yaml.v3) and numeric representations.
func toBool(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case int:
		return val != 0
	case float64:
		return val != 0
	default:
		return false
	}
}

// MemoryConfig holds configuration for the memory system.
// Type selects the backend: "" (disabled) or "topic".
type MemoryConfig struct {
	Type              string        `yaml:"type"`                             // "topic" or "" (disabled)
	KeywordProvider   string        `yaml:"keyword_provider"`                 // optional: provider name for keyword extraction (defaults to main provider)
	Timeout           time.Duration `yaml:"timeout" default:"10s"`            // context deadline for Store/Recall/Forget
	RecallLimit       int           `yaml:"recall_limit" default:"5"`         // max memories recalled per turn by automatic MemoryRecallReminder
	DecayHalfLifeDays int           `yaml:"decay_half_life_days" default:"7"` // decay half-life in days for TopicBackend (default 7)
}

// ToMemoryConfig converts the YAML-level MemoryConfig to the runtime
// memory.Config used by backends. Injects the base directory and
// applies a fallback timeout if the config value is zero.
func (mc *MemoryConfig) ToMemoryConfig() memory.Config {
	timeout := mc.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	return memory.Config{
		Type:              mc.Type,
		BaseDir:           BaseDir(),
		Timeout:           timeout,
		DecayHalfLifeDays: mc.DecayHalfLifeDays,
	}
}

// LSPConfig holds configuration for all LSP servers.
type LSPConfig struct {
	Enabled          *bool             `yaml:"enabled" default:"true"` // Enable LSP integration
	MaxRestarts      int               `yaml:"max_restarts" default:"3"`
	MaxFileSize      int64             `yaml:"max_file_size" default:"10485760"` // 10 MB
	MaxResults       int               `yaml:"max_results" default:"50"`         // per-operation result cap
	RequestTimeout   Duration          `yaml:"request_timeout" default:"15s"`    // per-request timeout
	ConcurrencyLimit int               `yaml:"concurrency_limit" default:"4"`    // per-server concurrency
	StartupTimeout   Duration          `yaml:"startup_timeout" default:"10s"`
	Servers          []LSPServerConfig `yaml:"servers"`
}

// IsEnabled returns whether LSP integration is enabled. Defaults to true.
func (c *LSPConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// LSPServerConfig describes a single LSP server to manage.
type LSPServerConfig struct {
	Name               string            `yaml:"name"`
	Command            string            `yaml:"command"`
	Args               []string          `yaml:"args"`
	Extensions         []string          `yaml:"extensions"`
	Languages          []string          `yaml:"languages"`
	InitializationOpts map[string]any    `yaml:"initialization_options"`
	Settings           map[string]any    `yaml:"settings"`
	Env                map[string]string `yaml:"env"`
	WorkspaceFolder    string            `yaml:"workspace_folder"`
	StartupTimeout     Duration          `yaml:"startup_timeout"`
	ConcurrencyLimit   int               `yaml:"concurrency_limit"`
}

// Default prompts for DeepResearch (built-in fallbacks when YAML doesn't specify).
const defaultQueryGeneratorPrompt = `You are a research query generator. Given a research topic and existing learnings, generate {breadth} specific, non-overlapping search engine queries. Each query should target a distinct aspect of the topic.

Research topic: {query}
Existing learnings: {learnings}

Generate {breadth} search queries with a "researchGoal" for each explaining what this query aims to discover.

Return your response as a JSON array of objects with "query" and "researchGoal" fields. Only return the JSON array, no other text.`

const defaultResearcherPrompt = `You are a research analyst. Your task:
1. Search for: "{query}"
2. Read the search results and linked pages
3. Extract key learnings (factual, detailed, with specific metrics and entities)
4. Suggest follow-up questions for deeper research

Research goal: {researchGoal}

Return your findings as a structured summary with:
- Key learnings (up to 3, concise and information-dense)
- Follow-up questions (up to 3, for deeper research)
- Source URLs visited

Output your findings in {language}.
`

const defaultReportWriterPrompt = `You are a research report writer. Write a comprehensive, well-structured report as a self-contained HTML document.

Research topic: {query}

Findings:
{learnings}

Source URLs:
{urls}

Create a beautiful, readable HTML page with:
- Modern, clean CSS design (embedded in a <style> tag)
- Good typography, readable font sizes and line spacing
- Professional color scheme with proper contrast
- Well-organized sections with clear heading hierarchy
- Code blocks with monospace font and subtle background if needed
- Links to sources properly formatted
- Responsive design that works on both desktop and mobile
- A table of contents at the top for navigation

The HTML should be a complete, valid HTML5 document with <!DOCTYPE html>, <html>, <head>, and <body> tags.
Include all CSS inline in a <style> tag within <head>. Do NOT use external CSS or JavaScript.
Write the report in {language}.

Use the WriteFile tool to save the HTML report to: {output_path}
Then return the complete HTML content of the report as your final output.`

type DeepResearchConfig struct {
	DefaultDepth   int           `yaml:"default_depth" default:"2"`
	DefaultBreadth int           `yaml:"default_breadth" default:"3"`
	MaxDepth       int           `yaml:"max_depth" default:"4"`
	MaxBreadth     int           `yaml:"max_breadth" default:"8"`
	Timeout        time.Duration `yaml:"timeout" default:"30m"`
	ReportTimeout  time.Duration `yaml:"report_timeout" default:"10m"`
	MaxLearnings   int           `yaml:"max_learnings" default:"200"`
	ReportLanguage string        `yaml:"language" default:"zh"`

	// QueryGeneratorProvider references a provider name from config's providers list.
	// When empty, the main (default) provider is used.
	QueryGeneratorProvider string `yaml:"query_generator_provider"`

	// Prompts contains all customizable prompt templates. When nil or empty string,
	// built-in Go defaults are used.
	Prompts *DeepResearchPrompts `yaml:"prompts,omitempty"`

	// Researcher controls how research sub-agents behave.
	Researcher *ResearcherConfig `yaml:"researcher,omitempty"`
}

// Language returns the report language, defaulting to "zh" when not set.
func (c *DeepResearchConfig) Language() string {
	if c.ReportLanguage != "" {
		return c.ReportLanguage
	}
	return "zh"
}

// QueryGeneratorPrompt returns the query generator prompt template, using
// the built-in default when config doesn't specify one.
func (c *DeepResearchConfig) QueryGeneratorPrompt() string {
	if c.Prompts != nil && c.Prompts.QueryGenerator != "" {
		return c.Prompts.QueryGenerator
	}
	return defaultQueryGeneratorPrompt
}

// ResearcherPrompt returns the researcher sub-agent prompt template, using
// the built-in default when config doesn't specify one.
func (c *DeepResearchConfig) ResearcherPrompt() string {
	if c.Prompts != nil && c.Prompts.Researcher != "" {
		return c.Prompts.Researcher
	}
	return defaultResearcherPrompt
}

// ReportWriterPrompt returns the report writer prompt template, using
// the built-in default when config doesn't specify one.
func (c *DeepResearchConfig) ReportWriterPrompt() string {
	if c.Prompts != nil && c.Prompts.ReportWriter != "" {
		return c.Prompts.ReportWriter
	}
	return defaultReportWriterPrompt
}

// ResearcherTools returns the allowed tools for research sub-agents.
func (c *DeepResearchConfig) ResearcherTools() []string {
	if c.Researcher != nil && len(c.Researcher.AllowedTools) > 0 {
		return c.Researcher.AllowedTools
	}
	return []string{"WebSearch", "WebFetch", "ReadFile", "Grep", "WriteFile"}
}

// ResearcherMaxIterations returns the max iterations per research sub-agent.
func (c *DeepResearchConfig) ResearcherMaxIterations() int {
	if c.Researcher != nil && c.Researcher.MaxIterations > 0 {
		return c.Researcher.MaxIterations
	}
	return 5
}

type DeepResearchPrompts struct {
	QueryGenerator string `yaml:"query_generator,omitempty"`
	Researcher     string `yaml:"researcher,omitempty"`
	ReportWriter   string `yaml:"report_writer,omitempty"`
}

type ResearcherConfig struct {
	AllowedTools  []string `yaml:"allowed_tools"`
	MaxIterations int      `yaml:"max_iterations" default:"5"`
}

// LogsConfig is a type alias for logger.Config, so that YAML tags and defaults
// are defined in one place (pkg/logger) and shared by the config package.
type LogsConfig = logger.Config

// DebugConfig holds debug/observability settings (pprof, etc.).
type DebugConfig struct {
	PPROF PprofConfig `yaml:"pprof"`
}

// PprofConfig controls the Go pprof HTTP server for live profiling.
// When enabled, pprof endpoints are served on 127.0.0.1:<port>:
//
//	/debug/pprof/          — index
//	/debug/pprof/profile   — CPU profile
//	/debug/pprof/heap      — heap profile
//	/debug/pprof/goroutine — goroutine dump
//	...and more
type PprofConfig struct {
	Enabled bool `yaml:"enabled" default:"false"`
	Port    int  `yaml:"port" default:"6060"`
}

// SystemPromptHint returns a system prompt snippet explaining how to use
// pprof for debugging Tachi itself. Returns empty string when pprof is
// not enabled or the port is 0.
func (c PprofConfig) SystemPromptHint() string {
	if !c.Enabled || c.Port == 0 {
		return ""
	}
	addr := fmt.Sprintf("127.0.0.1:%d", c.Port)
	return fmt.Sprintf(`- Pprof debug server: http://%s/debug/pprof/ — if the user asks you to debug
  Tachi's own performance issues (CPU, memory, goroutines), you can use Bash
  to run: go tool pprof http://%s/debug/pprof/profile?seconds=30 (CPU),
  go tool pprof http://%s/debug/pprof/heap (memory), or
  curl http://%s/debug/pprof/goroutine?debug=2 (goroutine dump)
`, addr, addr, addr, addr)
}

// Addr returns the listen address ("127.0.0.1:<port>") if pprof is enabled,
// or empty string otherwise.
func (c PprofConfig) Addr() string {
	if !c.Enabled || c.Port == 0 {
		return ""
	}
	return fmt.Sprintf("127.0.0.1:%d", c.Port)
}

type Config struct {
	Provider               string               `yaml:"provider"`
	MaxTokens              int                  `yaml:"max_tokens" default:"128000"`
	MaxIterations          *int                 `yaml:"max_iterations" default:"50"`             // nil = default; 0 = unlimited; >0 = explicit limit
	SessionCleanupMaxCount int                  `yaml:"session_cleanup_max_count" default:"100"` // max sessions to retain
	Providers              []ProviderConfig     `yaml:"providers"`
	WebSearch              WebSearchConfig      `yaml:"web_search"`
	WebFetch               WebFetchConfig       `yaml:"web_fetch"`
	MCPServers             []MCPServerConfig    `yaml:"-"`                  // Loaded from JSON files via LoadMCPServers(); not in YAML
	ActiveMCPProfile       string               `yaml:"active_mcp_profile"` // Which profile to load (empty = none)
	MCPToolSearch          MCPToolSearchConfig  `yaml:"mcp_tool_search"`
	MCPToolRefresh         MCPToolRefreshConfig `yaml:"mcp_tool_refresh"`
	TUI                    TUIConfig            `yaml:"tui"`
	SystemReminder         SystemReminderConfig `yaml:"system_reminder"`
	Language               string               `yaml:"language" default:"English"`      // Reply language for LLM
	TitleGeneration        *bool                `yaml:"title_generation" default:"true"` // set false to use truncation
	TitleProvider          string               `yaml:"title_provider"`                  // optional: provider name for title generation (defaults to main provider)
	CommitProvider         string               `yaml:"commit_provider"`                 // optional: provider name for /commit (defaults to main provider)
	RunProvider            string               `yaml:"run_provider"`                    // optional: provider name for tachi -p run mode (defaults to main provider)
	ProviderAliases        map[string]string    `yaml:"provider_aliases"`                // alias name → actual provider name
	Memory                 MemoryConfig         `yaml:"memory"`                          // pluggable memory backend
	Channel                ChannelConfig        `yaml:"channel"`                         // IM channel backends
	Subagent               SubagentConfig       `yaml:"subagent"`                        // Sub-agent configuration
	Review                 ReviewConfig         `yaml:"review"`                          // /review code review configuration
	Compact                CompactConfig        `yaml:"compact"`                         // /compact command configuration
	ToolResult             ToolResultConfig     `yaml:"tool_result"`                     // tool result size limits and file persistence
	Cron                   CronConfig           `yaml:"cron"`                            // Cron scheduler (channel mode)
	Dream                  DreamConfig          `yaml:"dream"`                           // AutoDream memory consolidation (channel mode)
	ACP                    ACPConfig            `yaml:"acp"`                             // ACP agent configuration
	LSP                    LSPConfig            `yaml:"lsp"`                             // LSP server configuration
	DeepResearch           DeepResearchConfig   `yaml:"deep_research"`                   // Deep Research engine configuration
	Logs                   LogsConfig           `yaml:"logs"`                            // Logger configuration
	Permissions            PermissionsConfig    `yaml:"permissions"`                     // Tool permission rules (bash allow/ask/deny)
	Oneoff                 OneoffConfig         `yaml:"oneoff"`                          // One-off transcript recording (/commit, /review, ambient, dream...)
	Hooks                  HooksConfig          `yaml:"hooks"`                           // Event hook system (user-defined commands)
	Herdr                  HerdrConfig          `yaml:"herdr"`                           // Herdr terminal multiplexer integration
	Debug                  DebugConfig          `yaml:"debug"`                           // Debug/observability settings (pprof, etc.)
}

// OneoffConfig controls one-off transcript recording: sidecar JSONL files
// capturing the full execution of side-channel LLM runs (/commit, /review,
// channel ambient, dream, github bot) without touching the main session
// history. See docs/2026-07-24-oneoff-transcript-design.md.
type OneoffConfig struct {
	Enabled       *bool `yaml:"enabled" default:"true"`      // master switch; false = restore old behavior (no recording)
	RetentionDays int   `yaml:"retention_days" default:"30"` // days to keep files under the global <home>/oneoff/ dir
}

// IsEnabled reports whether one-off transcript recording is on (default true).
func (c *OneoffConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// HooksConfig controls the event hook system. Events fire during the agent
// loop and can trigger external commands (user-defined scripts) or built-in
// Go callbacks (e.g. Herdr integration).
type HooksConfig struct {
	Enabled *bool                    `yaml:"enabled" default:"true"`
	Events  map[string][]HookCommand `yaml:"events"` // event name → commands
}

// IsEnabled reports whether the hook system is active (default true).
func (c *HooksConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// HookCommand defines an external command to run when an event fires.
// The command receives the event payload as JSON on stdin.
type HookCommand struct {
	Command string            `yaml:"command"`           // e.g. "bash {{HOOKS_DIR}}/notify.sh"
	Timeout string            `yaml:"timeout,omitempty"` // e.g. "5s", "3s"; default "5s"
	Async   *bool             `yaml:"async,omitempty"`   // default true; false = wait for completion
	Env     map[string]string `yaml:"env,omitempty"`     // extra env vars
}

// HerdrConfig controls the Herdr terminal multiplexer integration.
// When enabled and HERDR_ENV=1 is detected, Tachi automatically reports
// session identity and lifecycle state to the local Herdr server.
// Socket path and pane ID are always read from environment variables
// (HERDR_SOCKET_PATH, HERDR_PANE_ID).
type HerdrConfig struct {
	Enabled *bool `yaml:"enabled" default:"true"` // auto-detect from HERDR_ENV
}

// IsEnabled reports whether Herdr integration is allowed (default true).
func (c *HerdrConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// BashPermissions holds glob rules classifying bash commands.
// Only the '*' wildcard is supported. Per-command-segment precedence:
// deny > allow > ask > default(allow).
type BashPermissions struct {
	Deny  []string `yaml:"deny"`  // blocked outright; error returned to the LLM
	Ask   []string `yaml:"ask"`   // requires interactive approval (denied in non-interactive modes)
	Allow []string `yaml:"allow"` // exempts matching segments from ask rules
	// DisableBuiltinDeny turns off the built-in absolutely-dangerous deny
	// rules (permission.BuiltinDenyRules). Only honored from the GLOBAL
	// config — the same key in a project .tachi/permissions.yaml is ignored,
	// so a cloned repo cannot weaken the user's safety defaults.
	DisableBuiltinDeny bool `yaml:"disable_builtin_deny"`
}

// PermissionsConfig groups tool permission rule sets.
type PermissionsConfig struct {
	Bash BashPermissions `yaml:"bash"`
}

func DefaultConfig() *Config {
	cfg := &Config{}
	if err := defaults.Set(cfg); err != nil {
		panic(fmt.Sprintf("config: failed to apply defaults: %v", err))
	}
	return cfg
}

func configDir() string {
	return BaseDir()
}

func ConfigPath() (string, error) {
	return filepath.Join(configDir(), configFileName), nil
}

// InputHistoryPath 返回终端输入历史文件路径
func InputHistoryPath() (string, error) {
	return filepath.Join(configDir(), inputHistoryFileName), nil
}

// SessionDir 返回会话存储目录路径
func SessionDir() (string, error) {
	return filepath.Join(configDir(), sessionDirName), nil
}

// OneoffDir 返回旁路执行记录（one-off transcript）的全局存储目录。
// 仅用于无会话上下文的旁路执行（tachi -c、github bot、dream）；
// 有会话上下文的记录在 <SessionDir>/<id>/oneoff/ 下。
func OneoffDir() string {
	return filepath.Join(configDir(), oneoffDirName)
}

// LogsDir returns the path to the debug logs directory.
func LogsDir() string {
	return filepath.Join(BaseDir(), logsDirName)
}

// MCPTokensDir returns the path to the MCP OAuth tokens directory.
func MCPTokensDir() string {
	return filepath.Join(BaseDir(), mcpTokensDirName)
}

// GlobalSkillsDir returns the path to the global (user-level) skills directory.
func GlobalSkillsDir() string {
	return filepath.Join(BaseDir(), skillsDirName)
}

// WeixinStateDir returns the default path to the weixin state directory.
func WeixinStateDir() string {
	return filepath.Join(BaseDir(), weixinStateDirName)
}

// ResearchDir returns the path to the deep research reports directory.
func ResearchDir() string {
	return filepath.Join(BaseDir(), researchDirName)
}

// ToolResultsDir returns the path to the tool results storage directory.
func ToolResultsDir() string {
	return filepath.Join(BaseDir(), toolResultsDirName)
}

// CronStorePath returns the default path for crons.json.
func CronStorePath() string {
	return filepath.Join(BaseDir(), cronStoreFileName)
}

func Load() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	return LoadFrom(path)
}

func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultConfig(), nil
		}
		return nil, err
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// Apply defaults to any fields not set in the YAML.
	if err := defaults.Set(cfg); err != nil {
		return nil, fmt.Errorf("config defaults: %w", err)
	}

	return cfg, nil
}

func Save(cfg *Config) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	path := filepath.Join(dir, configFileName)
	return os.WriteFile(path, data, 0600)
}

func Init() (string, error) {
	path, err := ConfigPath()
	if err != nil {
		return "", err
	}

	dir := configDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	cfg := &Config{}
	if err := defaults.Set(cfg); err != nil {
		return "", fmt.Errorf("config defaults: %w", err)
	}

	// Init-specific overrides (not defaults — user-facing template values).
	cfg.Provider = "deepseek"
	cfg.Providers = []ProviderConfig{
		{
			Name:    "deepseek-v4-flash",
			Type:    llm.ProviderTypeAnthropic,
			Model:   "deepseek-v4-flash",
			BaseURL: "https://api.deepseek.com/anthropic",
			APIKey:  "<your-api-key>",
		},
		{
			Name:    "deepseek-v4-pro",
			Type:    llm.ProviderTypeAnthropic,
			Model:   "deepseek-v4-pro",
			BaseURL: "https://api.deepseek.com/anthropic",
			APIKey:  "<your-api-key>",
		},
	}
	cfg.WebSearch.Key = "<your-web-search-api-key>"

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return path, fmt.Errorf("config file already exists: %s", path)
		}
		return "", err
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return "", err
	}
	return path, nil
}

func (c *Config) FindProvider(name string) *ProviderConfig {
	// Resolve alias first: if name is an alias, use the target provider name.
	target := name
	if c.ProviderAliases != nil {
		if t, ok := c.ProviderAliases[name]; ok {
			target = t
		}
	}
	// Look up the (possibly resolved) name in providers.
	for j := range c.Providers {
		if c.Providers[j].Name == target {
			return &c.Providers[j]
		}
	}
	return nil
}

// ResolveAlias resolves an alias to the actual provider name.
// If the name is not an alias, it returns the name unchanged.
// This is used when storing provider names in session metadata,
// so that the stored value reflects the real provider config name
// rather than a potentially transient alias.
func (c *Config) ResolveAlias(name string) string {
	if c.ProviderAliases != nil {
		if t, ok := c.ProviderAliases[name]; ok {
			return t
		}
	}
	return name
}

// MCPEnabled returns true if at least one MCP server is configured.
func (c *Config) MCPEnabled() bool {
	return len(c.MCPServers) > 0
}

// LoadMCPServers loads MCP server config from JSON files (mcp.json).
// workDir is the project root directory (usually cwd); set to "" to skip
// project-level files and only load global.
//
// If JSON files are found, their servers replace whatever is currently in
// c.MCPServers. If no JSON files exist, c.MCPServers is left unchanged
// (typically empty, since YAML mcp_servers is no longer supported).
//
// Profile origin is tracked via the Profile field on each server:
// servers from base files (mcp.json) get empty string, servers from
// profile files (mcp.{profile}.json) get the profile name.
func (c *Config) LoadMCPServers(workDir string) error {
	servers, err := LoadMCPConfig(c.ActiveMCPProfile, workDir)
	if err != nil {
		return err
	}
	if servers != nil {
		c.MCPServers = servers
	}
	return nil
}

// MCPToolSearchConfig controls MCPSearchTools behavior.
type MCPToolSearchConfig struct {
	Enabled                *bool `yaml:"enabled" default:"true"`                    // false = load all tools directly (disable ToolSearch)
	MinToolsForSearch      int   `yaml:"min_tools_for_search" default:"5"`          // Auto-load all if total MCP tools <= this threshold
	MaxDeferredSchemaBytes int   `yaml:"max_deferred_schema_bytes" default:"50000"` // Max bytes per stored schema
}

// MCPToolRefreshConfig controls background tool list polling for HTTP MCP servers.
// When enabled, the agent periodically calls ListTools on connected HTTP servers
// and updates the deferred pool / discovered set if tools have changed.
type MCPToolRefreshConfig struct {
	Enabled  *bool         `yaml:"enabled" default:"true"` // false = disable background refresh
	Interval time.Duration `yaml:"interval" default:"1m"`  // polling interval (0 = disabled)
}

// IsEnabled returns whether tool refresh is active. Defaults to true.
func (c *MCPToolRefreshConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// RefreshInterval returns the polling interval. Returns 0 if refresh is disabled.
func (c *MCPToolRefreshConfig) RefreshInterval() time.Duration {
	if !c.IsEnabled() {
		return 0
	}
	return c.Interval
}

// IsEnabled returns whether ToolSearch is active. Defaults to true.
func (c *MCPToolSearchConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// GetMaxIterations returns the effective max iterations:
// - nil → DefaultMaxIterations (50)
// - 0 → 0 (unlimited)
// - >0 → explicit value
func (c *Config) GetMaxIterations() int {
	if c == nil || c.MaxIterations == nil {
		return DefaultMaxIterations
	}
	return *c.MaxIterations
}

// projectPermissionsFileName is the project-level permissions file, looked up
// under <project-root>/.tachi/.
const projectPermissionsFileName = "permissions.yaml"

// LoadProjectPermissions reads permission rules from
// <projectRoot>/.tachi/permissions.yaml. The file mirrors the global
// config.yaml "permissions:" section:
//
//	permissions:
//	  bash:
//	    deny: ["git push --force*"]
//	    ask:  ["rm *"]
//
// Project rules are merged with global rules such that a project can only
// tighten: deny/ask are unioned, but project-level allow is IGNORED at
// policy construction time (allow is a user-global privilege), and
// disable_builtin_deny has no effect here either.
// A missing file returns zero-value rules and nil error.
func LoadProjectPermissions(projectRoot string) (PermissionsConfig, error) {
	var out struct {
		Permissions PermissionsConfig `yaml:"permissions"`
	}
	if projectRoot == "" {
		return out.Permissions, nil
	}
	path := filepath.Join(projectRoot, ".tachi", projectPermissionsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out.Permissions, nil
		}
		return out.Permissions, fmt.Errorf("read project permissions: %w", err)
	}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return out.Permissions, fmt.Errorf("parse %s: %w", path, err)
	}
	return out.Permissions, nil
}
