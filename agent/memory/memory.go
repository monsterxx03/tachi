// Package memory provides a session memory system for Tachi.
// The TopicBackend uses local Markdown topic files searched via ripgrep,
// produced offline by the AutoDream system.
package memory

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// DreamStateFile is the filename for dream state persistence in each memory domain.
const DreamStateFile = "last_dream.json"

// InboxFile is where direct writes (the MemoryRecord tool) land, and
// InboxProcessingFile is where a RUNNING dream moves it (see dream/inbox.go).
//
// The name lives here because recall searches both: while a dream is running
// the memories it is consolidating must not disappear from recall.
const (
	InboxFile           = "inbox.md"
	InboxProcessingFile = "inbox.processing.md"
)

// Entry is a memory record exchanged between backends and the agent.
type Entry struct {
	ID        string   // unique identifier (the decay-state key for topic facts)
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
//
// The hash covers the fact's IDENTITY, which the heading is preferred for (see
// factIdentity): the whole text would make every clarification the dream writes
// a brand-new fact, resetting how often it was recalled and how old it is —
// exactly the two numbers the decay system exists to track.
func FactID(topicFile, content string) string {
	h := sha256.Sum256([]byte(factIdentity(content)))
	return fmt.Sprintf("topic:%s:%x", topicFile, h[:4])
}

// factIdentity is what a fact is "the same as" across dream rewrites: its
// entry heading when it has one, and the whole block otherwise (inbox entries
// have a "## <timestamp>" heading, which is also a stable identity).
//
// Two blocks in one file sharing a heading would share a decay state; the dream
// prompt forbids duplicate facts, and a shared state is a milder failure than
// every rewrite being treated as new.
func factIdentity(block string) string {
	// Prefer "## " by HEADING LEVEL, not position: the first block of every
	// topic file also carries the file's own "# <title>", and the entry's
	// heading is what identifies the fact (mirrors extractTitle).
	for _, prefix := range []string{"## ", "# "} {
		for line := range strings.SplitSeq(block, "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, prefix) {
				return trimmed
			}
		}
	}
	return block
}

// StoreScope selects which memory domain a direct write lands in.
//
// The domain is the difference between a fact that follows the user
// ("prefers terse answers") and one that only makes sense in one repository
// ("deploy runs make deploy"). Before scope existed every write landed in the
// project domain whenever the process happened to be inside a git repository,
// so a preference learned in project A was invisible in project B.
type StoreScope string

const (
	// ScopeAuto keeps the old behaviour: project when a project domain exists,
	// global otherwise. Correct for "facts about what we are working on".
	ScopeAuto StoreScope = ""
	// ScopeGlobal applies in every project and outside one.
	ScopeGlobal StoreScope = "global"
	// ScopeProject is knowledge about one repository. It falls back to global
	// when there is no project domain — a misplaced memory can still be found;
	// a dropped one cannot.
	ScopeProject StoreScope = "project"
)

// ParseStoreScope validates a scope from the tool layer ("" = auto).
func ParseStoreScope(s string) (StoreScope, error) {
	switch StoreScope(s) {
	case ScopeAuto, ScopeGlobal, ScopeProject:
		return StoreScope(s), nil
	default:
		return "", fmt.Errorf("unknown memory scope %q (want global, project, or empty)", s)
	}
}

// Backend is the abstract interface for memory storage.
//
// Memory has two paths: recall (MemoryRecall tool + system reminders) and
// direct writes (MemoryRecord tool). Turn/compact/session-scoped ingestion
// used to go through Store as well, but with TopicBackend the production of
// memory is asynchronous (the Dream pipeline writes topic files offline),
// so Store now carries direct writes only.
type Backend interface {
	// Store appends a direct memory entry (from the MemoryRecord tool) to the
	// backend's inbox. Tags are written into the entry's 关键词 line so recall
	// can match them; scope selects the domain.
	Store(ctx context.Context, content string, tags []string, scope StoreScope) error

	// Recall searches relevant memories by query. Called on every user message.
	// limit controls max returned entries.
	Recall(ctx context.Context, query string, limit int) ([]Entry, error)

	// ReinforceFacts strengthens the decay state of the facts a recall
	// returned — each one gets its reinforcement counter incremented, its
	// last_reinforced timestamp updated, and its decay reset to 1.0.
	//
	// BATCHED because one recall returns up to five entries, and the state file
	// is a whole JSON document: a per-entry call rewrote it five times on the
	// user's critical path. Implementations ignore ids they do not know
	// (session fallback entries are not facts).
	//
	// Only TopicBackend implements this.
	ReinforceFacts(ctx context.Context, entryIDs []string) error
}

// New creates a backend by type. Only "topic" is supported.
func New(backendType string, cfg config.MemoryConfig, logger *logger.Logger) (Backend, error) {
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
