package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/memory/agentmemory"
)

// AgentMemoryBackend implements the Backend interface by storing memories
// in an agentmemory server via HTTP.
type AgentMemoryBackend struct {
	client *agentmemory.Client
}

// NewAgentMemoryBackend creates a new AgentMemoryBackend.
func NewAgentMemoryBackend(cfg Config) (*AgentMemoryBackend, error) {
	return &AgentMemoryBackend{
		client: agentmemory.NewClient(cfg.AgentMemory.APIURL, cfg.Timeout),
	}, nil
}

// AgentMemoryConfig holds agentmemory-specific backend configuration.
type AgentMemoryConfig struct {
	APIURL string // agentmemory server URL (default: http://localhost:3111)
}

// Store writes memory to agentmemory. It delegates to the appropriate
// agentmemory API based on the StoreScope:
//   - StoreScopeTurn/StoreScopeCompact: POST /agentmemory/remember
//   - StoreScopeSession:                 POST /agentmemory/session/end
func (b *AgentMemoryBackend) Store(ctx context.Context, opts StoreOptions) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	switch opts.Scope {
	case StoreScopeTurn, StoreScopeCompact:
		content := b.formatMessages(opts)
		if content == "" {
			return nil
		}
		return b.client.Remember(ctx, agentmemory.RememberPayload{
			Content:   content,
			Tags:      opts.Tags,
			SessionID: opts.SessionID,
		})

	case StoreScopeSession:
		return b.client.EndSession(ctx, opts.SessionID)

	default:
		return nil
	}
}

// Recall searches agentmemory for memories semantically relevant to the query.
// It uses agentmemory's hybrid retrieval (BM25 + vector + knowledge graph).
func (b *AgentMemoryBackend) Recall(ctx context.Context, query string, limit int) ([]Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if query == "" {
		return nil, nil
	}

	results, err := b.client.SmartSearch(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("agentmemory recall: %w", err)
	}

	entries := make([]Entry, 0, len(results))
	for _, m := range results {
		entries = append(entries, Entry{
			ID:        m.ID,
			SessionID: m.SessionID,
			Summary:   truncateStr(m.Content, 80),
			Content:   m.Content,
			Score:     m.Score,
			Timestamp: m.Timestamp,
		})
	}
	return entries, nil
}

// Forget deletes a memory entry from agentmemory by its ID.
func (b *AgentMemoryBackend) Forget(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return b.client.Forget(ctx, id)
}

// formatMessages formats StoreOptions into a plain-text representation
// suitable for storing in agentmemory.
func (b *AgentMemoryBackend) formatMessages(opts StoreOptions) string {
	// Direct content takes priority
	if opts.DirectContent != "" {
		return opts.DirectContent
	}

	// Format from turn or session messages
	messages := opts.TurnMessages
	if len(messages) == 0 {
		messages = opts.SessionMessages
	}
	if len(messages) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, m := range messages {
		prefix := "User: "
		if m.Role == "assistant" {
			prefix = "Assistant: "
		}
		sb.WriteString(prefix + m.Content + "\n")
	}
	return strings.TrimSpace(sb.String())
}

