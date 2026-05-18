// Package memory provides a pluggable session memory system for Tachi.
// Backends implement the Backend interface with Store/Recall/Forget operations,
// allowing memory storage to be switched (native, mem9, etc.) without
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
)

// StoreOptions controls Store behavior.
type StoreOptions struct {
	Scope           StoreScope // call timing
	SessionID       string     // session ID (mem9 uses for dedup, native uses for index)
	SessionTitle    string     // session title (native backend writes to log at session scope)
	Tags            []string   // keyword tags (native backend writes to log at session scope)
	TurnMessages    []Message  // current turn messages (user + assistant)
	SessionMessages []Message  // all session messages (compact/session scopes)
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
	//   - turn:    write current turn (last user+assistant pair) — native ignores, mem9 writes
	//   - compact: write larger window (recent turns) — native ignores, mem9 writes
	//   - session: write full session summary — native writes log index, mem9 writes
	Store(ctx context.Context, opts StoreOptions) error

	// Recall searches relevant memories by query. Called on every user message.
	// limit controls max returned entries.
	// native backend returns nil (LLM uses GrepTool for better search).
	Recall(ctx context.Context, query string, limit int) ([]Entry, error)

	// Forget deletes a memory by ID. Used by /forget command.
	Forget(ctx context.Context, id string) error
}

// Config is the common configuration for memory backends.
type Config struct {
	Type    string // "native" or "mem9"
	BaseDir string // ~/.tachi/ (native uses this)
	Timeout time.Duration // context deadline for Store/Recall/Forget calls (default 10s)
	Mem9    Mem9Config
}

// Mem9Config holds mem9-specific configuration.
type Mem9Config struct {
	APIURL         string        // default: https://api.mem9.ai
	APIKey         string
	AgentID        string        // default: "tachi"
	Mode           string        // "smart" or "raw"
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
	case "native":
		return NewNativeBackend(cfg)
	case "mem9":
		return NewMem9Backend(cfg)
	default:
		return nil, fmt.Errorf("unknown memory backend: %s", backendType)
	}
}