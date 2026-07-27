// Package memory provides a session memory system for Tachi.
// The TopicBackend uses local Markdown topic files searched via ripgrep,
// produced offline by the AutoDream system.
package memory

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// DreamStateFile is the filename for dream state persistence in each memory domain.
const DreamStateFile = "last_dream.json"

// Entry is a memory record exchanged between backends and the agent.
type Entry struct {
	ID        string   // unique identifier (for Forget)
	SessionID string   // source session ID
	Summary   string   // one-line summary
	Tags      []string // keyword tags
	Content   string   // detailed content
	Timestamp int64    // unix timestamp
	Score     float64  // relevance score from backend recall
}

// FactState tracks decay information for a single fact in a topic file.
// Used by both the Dream orchestrator (scanning topics) and TopicBackend
// (applying decay multipliers during recall + reinforcing on hit).
type FactState struct {
	ID             string    `json:"id"`
	TopicFile      string    `json:"topic_file"`
	Decay          float64   `json:"decay"`
	Reinforcements int       `json:"reinforcements"`
	LastReinforced time.Time `json:"last_reinforced"`
	CreatedAt      time.Time `json:"created_at"`
	Superseded     bool      `json:"superseded"`
}

// FactID generates the stable fact identifier used for decay tracking.
// Format: "topic:<filename>:<sha256_hex8>"
func FactID(topicFile, content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("topic:%s:%x", topicFile, h[:4])
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

	// ReinforceFact strengthens a fact's decay state when it is recalled.
	// Called after MemoryRecall returns results — each matched fact gets
	// its reinforcement counter incremented, last_reinforced timestamp
	// updated, and decay reset to 1.0.
	//
	// Only TopicBackend implements this.
	ReinforceFact(ctx context.Context, entryID string) error
}

// Config is the configuration for the TopicBackend.
type Config struct {
	Type              string   // backend type (only "topic")
	BaseDir           string   // ~/.tachi/
	Timeout           time.Duration // context deadline for Store/Recall/Forget calls (default 10s)
	DecayHalfLifeDays int      // decay half-life in days (default 7); only used by TopicBackend
	ExcludeRepos      []string // git repo roots to skip memory writes
}

// New creates a backend by type. Only "topic" is supported.
func New(backendType string, cfg Config, logger *logger.Logger) (Backend, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	switch backendType {
	case "topic":
		return NewTopicBackend(cfg, logger)
	default:
		return nil, fmt.Errorf("unknown memory backend: %s", backendType)
	}
}
