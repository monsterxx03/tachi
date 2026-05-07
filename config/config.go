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
	DefaultMaxTokens                        = 32000
	MaxAllowedTokens                        = 4096
	DefaultMaxIterations                    = 50
	DefaultMCPConnectTimeout                = 5 * time.Second
	configDirName                           = ".tachi"
	configFileName                          = "config.yaml"
	inputHistoryFileName                    = "input_history"
	sessionDirName                          = "session"
)

type ProviderConfig struct {
	Name          string `yaml:"name"`
	Type          string `yaml:"type"`
	Model         string `yaml:"model"`
	BaseURL       string `yaml:"base_url"`
	APIKey        string `yaml:"api_key"`
	ContextWindow *int64 `yaml:"context_window"` // Manual override for model context window (tokens)
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
	AuthServerMetadataURL string   `yaml:"auth_server_metadata_url,omitempty"` // Override auto-discovery
	CallbackHost          string   `yaml:"callback_host,omitempty" default:"127.0.0.1"` // OAuth callback host
	CallbackPort          int      `yaml:"callback_port,omitempty"`                      // OAuth callback port (default: auto)
}

// MCPServerConfig represents a single MCP server connection configuration
type MCPServerConfig struct {
	Name    string            `yaml:"name"`
	Type    MCPTransportType  `yaml:"type"` // "stdio" or "http"
	Command string            `yaml:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	URL     string            `yaml:"url,omitempty"`     // For http transport
	Headers map[string]string `yaml:"headers,omitempty"` // For http transport
	Proxy   string            `yaml:"proxy,omitempty"`   // Optional proxy URL (only for http transport; e.g. socks5://127.0.0.1:1080)
	Timeout *time.Duration    `yaml:"timeout,omitempty"` // Connect timeout (default: 5s)
	Enabled *bool             `yaml:"enabled,omitempty" default:"true"` // Whether to load this server
	OAuth   *MCPOAuthConfig   `yaml:"oauth,omitempty"`   // OAuth2 configuration (http transport only)
}

// HasOAuth returns true if the server has OAuth2 configured.
func (srv *MCPServerConfig) HasOAuth() bool {
	return srv.OAuth != nil
}

// IsEnabled returns whether the server should be loaded. Defaults to true.
func (srv *MCPServerConfig) IsEnabled() bool {
	return srv.Enabled == nil || *srv.Enabled
}

// TUIConfig 控制终端界面行为。
type TUIConfig struct {
	InputHistoryLimit int  `yaml:"input_history_limit" default:"10"`
	SkipEditConfirm   bool `yaml:"skip_edit_confirm"`
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
}

// ChannelConfig groups configuration for all IM channel backends.
type ChannelConfig struct {
	Weixin WeixinConfig `yaml:"weixin"`
}

type Config struct {
	Provider               string                `yaml:"provider"`
	MaxTokens              int                   `yaml:"max_tokens" default:"32000"`
	MaxIterations          *int                  `yaml:"max_iterations" default:"50"`          // nil = default; 0 = unlimited; >0 = explicit limit
	SessionCleanupMaxCount int                   `yaml:"session_cleanup_max_count" default:"100"` // max sessions to retain
	Providers              []ProviderConfig      `yaml:"providers"`
	WebSearch              WebSearchConfig       `yaml:"web_search"`
	WebFetch               WebFetchConfig        `yaml:"web_fetch"`
	MCPServers             []MCPServerConfig     `yaml:"mcp_servers"`
	TUI                    TUIConfig             `yaml:"tui"`
	SystemReminder         SystemReminderConfig  `yaml:"system_reminder"`
	Language               string                `yaml:"language" default:"English"` // Reply language for LLM
	TitleGeneration        *bool                 `yaml:"title_generation" default:"true"` // set false to use truncation
	TitleProvider          string                `yaml:"title_provider"`   // optional: provider name for title generation (defaults to main provider)
	CommitProvider         string                `yaml:"commit_provider"`  // optional: provider name for /commit (defaults to main provider)
	Channel                ChannelConfig         `yaml:"channel"`          // IM channel backends
}

func DefaultConfig() *Config {
	cfg := &Config{}
	if err := defaults.Set(cfg); err != nil {
		panic(fmt.Sprintf("config: failed to apply defaults: %v", err))
	}
	return cfg
}

func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, configDirName), nil
}

func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

// InputHistoryPath 返回终端输入历史文件路径：~/.tachi/input_history
func InputHistoryPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, inputHistoryFileName), nil
}

// SessionDir 返回会话存储目录路径：~/.tachi/session
func SessionDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionDirName), nil
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

	return cfg, nil
}

func Save(cfg *Config) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
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

	dir, err := configDir()
	if err != nil {
		return "", err
	}
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
