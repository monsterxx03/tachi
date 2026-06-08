package lsp

import "time"

// Config is the runtime LSP configuration for the agent/lsp package.
// Converted from config.LSPConfig during agent initialization.
type Config struct {
	Enabled          bool
	MaxRestarts      int
	MaxFileSize      int64
	MaxResults       int
	ConcurrencyLimit int
	RequestTimeout   time.Duration
	StartupTimeout   time.Duration
	Servers          []ServerConfig
}

// ServerConfig describes a single LSP server to manage.
type ServerConfig struct {
	Name               string
	Command            string
	Args               []string
	Extensions         []string
	Languages          []string
	InitializationOpts map[string]any
	Settings           map[string]any
	Env                map[string]string
	WorkspaceFolder    string
	StartupTimeout     time.Duration
	ConcurrencyLimit   int
}
