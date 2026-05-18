package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/creasty/defaults"
	"github.com/monsterxx03/tachi/llm"
	"gopkg.in/yaml.v3"
)

const (
	DefaultMaxTokens         = 128000
	DefaultMaxIterations     = 50
	DefaultMCPConnectTimeout = 5 * time.Second
	configDirName            = ".tachi"
	configFileName           = "config.yaml"
	inputHistoryFileName     = "input_history"
	sessionDirName           = "session"
	logsDirName              = "logs"
	mcpTokensDirName         = "mcp_tokens"
	skillsDirName            = "skills"
	weixinStateDirName       = "weixin"
	cronStoreFileName        = "crons.json"
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
	ClientID              string   `yaml:"client_id,omitempty"`
	ClientSecret          string   `yaml:"client_secret,omitempty"`
	ClientURI             string   `yaml:"client_uri,omitempty"`
	Scopes                []string `yaml:"scopes,omitempty"`
	AuthServerMetadataURL string   `yaml:"auth_server_metadata_url,omitempty"`          // Override auto-discovery
	CallbackHost          string   `yaml:"callback_host,omitempty" default:"127.0.0.1"` // OAuth callback host
	CallbackPort          int      `yaml:"callback_port,omitempty"`                     // OAuth callback port (default: auto)
}

// MCPServerConfig represents a single MCP server connection configuration
type MCPServerConfig struct {
	Name    string            `yaml:"name"`
	Type    MCPTransportType  `yaml:"type"` // "stdio" or "http"
	Command string            `yaml:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	URL     string            `yaml:"url,omitempty"`                    // For http transport
	Headers map[string]string `yaml:"headers,omitempty"`                // For http transport
	Proxy   string            `yaml:"proxy,omitempty"`                  // Optional proxy URL (only for http transport; e.g. socks5://127.0.0.1:1080)
	Timeout *time.Duration    `yaml:"timeout,omitempty"`                // Connect timeout (default: 5s)
	Enabled *bool             `yaml:"enabled,omitempty" default:"true"` // Whether to load this server
	OAuth   *MCPOAuthConfig   `yaml:"oauth,omitempty"`                  // OAuth2 configuration (http transport only)

	// Profile is the MCP profile this server originates from.
	// Empty string means it came from mcp_servers (always loaded).
	// Set internally during config expansion; not serialized to YAML.
	Profile string `yaml:"-"`
}

// HasOAuth returns true if the server has OAuth2 configured.
func (srv *MCPServerConfig) HasOAuth() bool {
	return srv.OAuth != nil
}

// TokenStorageName returns the key used for on-disk token storage.
// For always-loaded servers this is the server name. For profile servers,
// the profile name is appended to avoid collisions across environments.
func (srv *MCPServerConfig) TokenStorageName() string {
	if srv.Profile == "" {
		return srv.Name
	}
	return srv.Name + "_" + srv.Profile
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

// CompactTimeoutDefault is the default timeout for /compact operations.
const CompactTimeoutDefault = 5 * time.Minute

// CompactConfig holds configuration for the /compact command.
type CompactConfig struct {
	Timeout time.Duration `yaml:"timeout" default:"5m"` // Timeout for the compaction LLM call
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
	Weixin   WeixinConfig               `yaml:"weixin"`
	Channels map[string]map[string]any  `yaml:"channels"`
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
// Type selects the backend: "" (disabled), "native", or "mem9".
type MemoryConfig struct {
	Type    string        `yaml:"type"`    // "native", "mem9", or "" (disabled)
	Timeout string        `yaml:"timeout"` // context deadline for Store/Recall/Forget (default "10s")
	Mem9    Mem9SubConfig `yaml:"mem9"`
}

// Mem9SubConfig holds mem9-specific memory configuration.
type Mem9SubConfig struct {
	APIURL         string `yaml:"api_url"`         // default: https://api.mem9.ai
	APIKey         string `yaml:"api_key"`
	AgentID        string `yaml:"agent_id"`        // default: "tachi"
	Mode           string `yaml:"mode"`            // default: "smart"
	RequestTimeout string `yaml:"request_timeout"` // HTTP request timeout (default "15s")
	Proxy          string `yaml:"proxy"`           // Optional proxy URL (e.g. socks5://127.0.0.1:1080, http://127.0.0.1:8080)
}

type Config struct {
	Provider               string                       `yaml:"provider"`
	MaxTokens              int                          `yaml:"max_tokens" default:"128000"`
	MaxIterations          *int                         `yaml:"max_iterations" default:"50"`             // nil = default; 0 = unlimited; >0 = explicit limit
	SessionCleanupMaxCount int                          `yaml:"session_cleanup_max_count" default:"100"` // max sessions to retain
	Providers              []ProviderConfig             `yaml:"providers"`
	WebSearch              WebSearchConfig              `yaml:"web_search"`
	WebFetch               WebFetchConfig               `yaml:"web_fetch"`
	MCPServers             []MCPServerConfig            `yaml:"mcp_servers"`
	MCPProfiles            map[string][]MCPServerConfig `yaml:"mcp_profiles"`       // Profile name -> servers
	ActiveMCPProfile       string                       `yaml:"active_mcp_profile"` // Which profile to load (empty = none)
	TUI                    TUIConfig                    `yaml:"tui"`
	SystemReminder         SystemReminderConfig         `yaml:"system_reminder"`
	Language               string                       `yaml:"language" default:"English"`      // Reply language for LLM
	TitleGeneration        *bool                        `yaml:"title_generation" default:"true"` // set false to use truncation
	TitleProvider          string                       `yaml:"title_provider"`                  // optional: provider name for title generation (defaults to main provider)
	CommitProvider         string                       `yaml:"commit_provider"`                 // optional: provider name for /commit (defaults to main provider)
	Memory                 MemoryConfig                 `yaml:"memory"`                          // pluggable memory backend
	Channel                ChannelConfig                `yaml:"channel"`                         // IM channel backends
	Subagent               SubagentConfig               `yaml:"subagent"`                        // Sub-agent configuration
	Compact                CompactConfig                `yaml:"compact"`                         // /compact command configuration
	Cron                   CronConfig                   `yaml:"cron"`                            // Cron scheduler (channel mode)
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

func configPath() (string, error) {
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

// CronStorePath returns the default path for crons.json.
func CronStorePath() string {
	return filepath.Join(BaseDir(), cronStoreFileName)
}

func Load() (*Config, error) {
	path, err := configPath()
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

	// Expand MCP profile into MCPServers.
	if err := cfg.ExpandMCPProfiles(); err != nil {
		return nil, fmt.Errorf("mcp profiles: %w", err)
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
	path, err := configPath()
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
	cfg.Provider = "minimax-anthropic"
	cfg.Providers = []ProviderConfig{
		{
			Name:    "minimax-anthropic",
			Type:    llm.ProviderTypeAnthropic,
			Model:   "MiniMax-M2.7",
			BaseURL: "https://api.minimaxi.com/anthropic",
			APIKey:  "<your-api-key>",
		},
		{
			Name:    "minimax-openai",
			Type:    llm.ProviderTypeOpenAI,
			Model:   "MiniMax-M2.7",
			BaseURL: "https://api.minimaxi.com/v1",
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

// ExpandMCPProfiles merges the active MCP profile's servers into MCPServers.
// Each server from the profile gets its Profile field set to the profile name.
// Returns an error if a profile server name conflicts with an mcp_servers entry
// or with another server in the same profile.
func (c *Config) ExpandMCPProfiles() error {
	if c.ActiveMCPProfile == "" {
		return nil
	}

	profileServers, ok := c.MCPProfiles[c.ActiveMCPProfile]
	if !ok {
		return fmt.Errorf("active_mcp_profile %q not found in mcp_profiles", c.ActiveMCPProfile)
	}

	// Build a lookup of existing mcp_servers names for conflict detection.
	existing := make(map[string]bool, len(c.MCPServers))
	for _, srv := range c.MCPServers {
		existing[srv.Name] = true
	}

	// Check conflicts within the profile itself.
	seenInProfile := make(map[string]bool, len(profileServers))
	for i := range profileServers {
		srv := &profileServers[i]
		if srv.Name == "" {
			return fmt.Errorf("profile %q: server at index %d has no name", c.ActiveMCPProfile, i)
		}
		if existing[srv.Name] {
			return fmt.Errorf(
				"server name conflict: %q in profile %q collides with an mcp_servers entry of the same name",
				srv.Name, c.ActiveMCPProfile,
			)
		}
		if seenInProfile[srv.Name] {
			return fmt.Errorf(
				"server name conflict: duplicate name %q in profile %q",
				srv.Name, c.ActiveMCPProfile,
			)
		}
		seenInProfile[srv.Name] = true
	}

	// Stamp profile origin and append.
	for i := range profileServers {
		profileServers[i].Profile = c.ActiveMCPProfile
		if err := defaults.Set(&profileServers[i]); err != nil {
			return fmt.Errorf("profile %q server %q: defaults: %w", c.ActiveMCPProfile, profileServers[i].Name, err)
		}
	}

	c.MCPServers = append(c.MCPServers, profileServers...)
	return nil
}

// MCPTimeout returns the timeout for connecting to an MCP server.
func (srv *MCPServerConfig) MCPTimeout() time.Duration {
	if srv.Timeout != nil && *srv.Timeout > 0 {
		return *srv.Timeout
	}
	return DefaultMCPConnectTimeout
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

// intPtr returns a pointer to the given int. Helper for config initialization.
func intPtr(v int) *int {
	return &v
}
