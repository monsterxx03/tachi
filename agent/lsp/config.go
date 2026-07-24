package lsp

import "time"

// Config is the runtime LSP configuration for the agent/lsp package.
// Converted from config.LSPConfig during agent initialization.
// The manager is only constructed when LSP is enabled, so Config carries
// no Enabled flag of its own.
type Config struct {
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
