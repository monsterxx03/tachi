// Package memory provides a pluggable session memory system for Tachi.
// Backends implement the Backend interface with Store/Recall/Forget operations,
// allowing memory storage to be switched (mem9, etc.) without
// affecting the agent loop.
package memory

import (
	"context"
	"fmt"
	"time"
)

// Entry is a memory record exchanged between backends and the agent.
type Entry struct {
	ID        string   // unique identifier (for Forget)
	SessionID string   // source session ID
	Summary   string   // one-line summary
	Tags      []string // keyword tags
	Content   string   // detailed content (empty for native, conversation text for mem9)
	Timestamp int64    // unix timestamp
	Score     float64  // relevance score from backend recall
}

// StoreScope marks when Store is called, allowing backends to choose
// different upload strategies per timing.
type StoreScope string

const (
	StoreScopeTurn    StoreScope = "turn"    // after each conversation turn completes
	StoreScopeCompact StoreScope = "compact" // before context compaction
	StoreScopeSession StoreScope = "session" // at session end
	StoreScopeStart   StoreScope = "start"   // at session creation (notify backend)
)

// StoreOptions controls Store behavior.
type StoreOptions struct {
	Scope           StoreScope // call timing
	SessionID       string     // session ID (used for deduplication)
	SessionTitle    string     // session title
	Tags            []string   // keyword tags
	TurnMessages    []Message  // current turn messages (user + assistant)
	SessionMessages []Message  // all session messages (compact/session scopes)

	// DirectContent is a plain-text content string for direct memory writes
	// (not ingest-based). When set, it takes priority over TurnMessages and
	// the backend stores the content directly — no message filtering is applied.
	// Currently only supported by the mem9 backend.
	DirectContent string
}

// Message represents a single conversation message sent to memory backends.
type Message struct {
	Role    string `json:"role"`    // "user" or "assistant"
	Content string `json:"content"` // message body
}

// Backend is the abstract interface for memory storage.
// All backends must implement three operations: Store, Recall, Forget.
type Backend interface {
	// Store writes memory. Depending on scope, backend chooses granularity:
	//   - turn:    write current turn (last user+assistant pair)
	//   - compact: write larger window (recent turns)
	//   - session: write full session summary
	Store(ctx context.Context, opts StoreOptions) error

	// Recall searches relevant memories by query. Called on every user message.
	// limit controls max returned entries.
	Recall(ctx context.Context, query string, limit int) ([]Entry, error)

	// Forget deletes a memory by ID. Used by /forget command.
	Forget(ctx context.Context, id string) error

	// Observe records a contextual observation to the memory backend.
	// Used by the agent to log tool execution results as structured events
	// with a hook type indicating the nature of the observation.
	// The mem9 backend implements this as a no-op.
	Observe(ctx context.Context, opts ObserveOptions) error
}

// ObserveOptions controls Observe behavior.
type ObserveOptions struct {
	HookType    string // e.g. "post_tool_use", "post_tool_failure"
	SessionID   string // session identifier
	Project     string // project name (git repo root basename)
	CWD         string // current working directory
	ToolName    string // tool that was executed
	ToolInput   string // tool invocation arguments
	ToolOutput  string // tool result or error message
	IsError     bool   // whether the tool execution failed
	Timestamp   string // ISO 8601 timestamp
}

// Config is the common configuration for memory backends.
type Config struct {
	Type         string        // backend type (e.g., "mem9", "agentmemory")
	BaseDir      string        // ~/.tachi/
	Timeout      time.Duration // context deadline for Store/Recall/Forget calls (default 10s)
	Mem9         Mem9Config
	AgentMemory  AgentMemoryConfig // agentmemory-specific config
	ExcludeRepos []string // git repo roots to skip memory writes
}

// Mem9Config holds mem9-specific configuration.
type Mem9Config struct {
	APIURL         string        // default: https://api.mem9.ai
	APIKey         string
	AgentID        string        // default: "tachi"
	Mode           string        // "smart" or "raw"
	Proxy          string        // Optional proxy URL (e.g. socks5://127.0.0.1:1080)
	RequestTimeout time.Duration // HTTP request timeout (default 15s)
}

// New creates a backend by type.
func New(backendType string, cfg Config) (Backend, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Mem9.RequestTimeout <= 0 {
		cfg.Mem9.RequestTimeout = 15 * time.Second
	}
	switch backendType {
	case "mem9":
		return NewMem9Backend(cfg)
	case "agentmemory":
		return NewAgentMemoryBackend(cfg)
	default:
		return nil, fmt.Errorf("unknown memory backend: %s", backendType)
	}
}