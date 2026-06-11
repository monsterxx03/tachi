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
	cronStoreFileName         = "crons.json"
	toolResultsDirName        = "tool_results"
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

type ProviderConfig struct {
	Name          string `yaml:"name"`
	Type          string `yaml:"type"`
	Model         string `yaml:"model"`
	BaseURL       string `yaml:"base_url"`
	APIKey        string `yaml:"api_key"`
	ContextWindow *int64 `yaml:"context_window"` // Manual override for model context window (tokens)

	// Pricing overrides (CNY per 1M tokens). When set, override built-in pricing.
	// Leave nil to use built-in prices (if available). Set 0 to disable cost calculation.
	InputPrice              *float64 `yaml:"input_price"`
	OutputPrice             *float64 `yaml:"output_price"`
	CacheReadInputPrice     *float64 `yaml:"cache_read_input_price"`
	CacheCreationInputPrice *float64 `yaml:"cache_creation_input_price"`
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

	// Profile is the MCP profile this server originates from.
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
	SkipEditConfirm   bool  `yaml:"skip_edit_confirm"`
	NotifyOnComplete  *bool `yaml:"notify_on_complete" default:"true"` // 是否在 LLM 回合结束后发送终端通知
}

// NotifyEnabled 返回是否启用了回合结束通知。默认启用。
func (t *TUIConfig) NotifyEnabled() bool {
	return t.NotifyOnComplete == nil || *t.NotifyOnComplete
}

type SystemReminderConfig struct {
	IterationWarningThreshold int   `yaml:"iteration_warning_threshold" default:"5"`
	TokenWarningThresholdPct  int   `yaml:"token_warning_threshold_pct" default:"80"`
	GitReminder               *bool `yaml:"git_reminder" default:"true"` // set false to disable
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

// EditConfig holds configuration for the edit mode.
type EditConfig struct {
	Mode           string  `yaml:"mode" default:"replace"`         // replace | hashline
	FuzzyThreshold float64 `yaml:"fuzzy_threshold" default:"0.95"` // 0.0-1.0, line content fuzzy matching tolerance
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
	Auto      bool          `yaml:"auto" default:"true"`       // Enable automatic compaction when context is near limit
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
// When Provider/Model are empty, the main provider/model is used.
type SubagentConfig struct {
	Provider       string `yaml:"provider"`         // provider name, empty → use main
	Model          string `yaml:"model"`            // model name, empty → use main
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

// ChannelConfig groups configuration for all IM channel backends.
//
// Legacy field: Weixin is the typed config for the built-in weixin channel.
// For backward compatibility, if the Weixin.Enabled flag is set but no
// "weixin" entry exists in Channels, weixin is auto-activated.
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
}

// ActiveChannels returns the raw configs for every enabled channel,
// merging legacy Weixin config into the generic Channels map when needed.
//
// For backward compatibility: if the legacy weixin.enabled flag is set
// and there's no "weixin" key in Channels, weixin is included by
// converting its typed config. If both exist, Channels takes precedence.
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

// MemoryConfig holds configuration for the pluggable memory system.
// Type selects the backend: "" (disabled), "mem9", or "agentmemory".
type MemoryConfig struct {
	Type             string               `yaml:"type"`                  // "mem9" or "agentmemory" or "" (disabled)
	Timeout          time.Duration        `yaml:"timeout" default:"10s"` // context deadline for Store/Recall/Forget
	ToolResultMaxLen int                  `yaml:"tool_result_max_len"`   // max chars for tool result in memory (default 8000, 0 = no limit)
	Mem9             Mem9SubConfig        `yaml:"mem9"`
	AgentMemory      AgentMemorySubConfig `yaml:"agentmemory"`
	ExcludeRepos     []string             `yaml:"exclude_repos"` // git repo roots to skip memory writes
}

// AgentMemorySubConfig holds agentmemory-specific configuration.
type AgentMemorySubConfig struct {
	APIURL string `yaml:"api_url"` // agentmemory server URL (default: http://localhost:3111)
}

// Mem9SubConfig holds mem9-specific memory configuration.
type Mem9SubConfig struct {
	APIURL  string `yaml:"api_url"` // default: https://api.mem9.ai
	APIKey  string `yaml:"api_key"`
	AgentID string `yaml:"agent_id"` // default: "tachi"
	Mode    string `yaml:"mode"`     // default: "smart"
	Proxy   string `yaml:"proxy"`    // Optional proxy URL (e.g. socks5://127.0.0.1:1080, http://127.0.0.1:8080)
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
		Type:         mc.Type,
		BaseDir:      BaseDir(),
		Timeout:      timeout,
		ExcludeRepos: mc.ExcludeRepos,
		Mem9: memory.Mem9Config{
			APIURL:  mc.Mem9.APIURL,
			APIKey:  mc.Mem9.APIKey,
			AgentID: mc.Mem9.AgentID,
			Mode:    mc.Mem9.Mode,
			Proxy:   mc.Mem9.Proxy,
		},
		AgentMemory: memory.AgentMemoryConfig{
			APIURL: mc.AgentMemory.APIURL,
		},
	}
}

// LSPConfig holds configuration for all LSP servers.
type LSPConfig struct {
	Enabled          bool              `yaml:"enabled" default:"true"`
	MaxRestarts      int               `yaml:"max_restarts" default:"3"`
	MaxFileSize      int64             `yaml:"max_file_size" default:"10485760"`             // 10 MB
	MaxResults       int               `yaml:"max_results" default:"50"`                      // per-operation result cap
	RequestTimeout   Duration          `yaml:"request_timeout" default:"15s"`                 // per-request timeout
	ConcurrencyLimit int               `yaml:"concurrency_limit" default:"4"`                 // per-server concurrency
	StartupTimeout   Duration          `yaml:"startup_timeout" default:"10s"`
	Servers          []LSPServerConfig `yaml:"servers"`
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
	Memory                 MemoryConfig         `yaml:"memory"`                          // pluggable memory backend
	Channel                ChannelConfig        `yaml:"channel"`                         // IM channel backends
	Subagent               SubagentConfig       `yaml:"subagent"`                        // Sub-agent configuration
	Compact                CompactConfig        `yaml:"compact"`                         // /compact command configuration
	ToolResult             ToolResultConfig     `yaml:"tool_result"`                     // tool result size limits and file persistence
	Cron                   CronConfig           `yaml:"cron"`                            // Cron scheduler (channel mode)
	ACP                    ACPConfig            `yaml:"acp"`                             // ACP agent configuration
	Edit                   EditConfig           `yaml:"edit"`                            // Edit mode configuration
	LSP                    LSPConfig            `yaml:"lsp"`                             // LSP server configuration
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
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
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
