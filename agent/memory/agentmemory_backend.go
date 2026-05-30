package memory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/memory/agentmemory"
)

// AgentMemoryBackend implements the Backend interface by storing memories
// in an agentmemory server via HTTP.
type AgentMemoryBackend struct {
	client  *agentmemory.Client
	timeout time.Duration
}

// NewAgentMemoryBackend creates a new AgentMemoryBackend.
func NewAgentMemoryBackend(cfg Config) (*AgentMemoryBackend, error) {
	return &AgentMemoryBackend{
		client:  agentmemory.NewClient(cfg.AgentMemory.APIURL, cfg.Timeout),
		timeout: cfg.Timeout,
	}, nil
}

// AgentMemoryConfig holds agentmemory-specific backend configuration.
type AgentMemoryConfig struct {
	APIURL string // agentmemory server URL (default: http://localhost:3111)
}

// Store writes memory to agentmemory. It delegates to the appropriate
// agentmemory API based on the StoreScope:
//   - StoreScopeStart:                   POST /agentmemory/session/start
//   - StoreScopeTurn/StoreScopeCompact: POST /agentmemory/remember
//   - StoreScopeSession:                 POST /agentmemory/session/end
func (b *AgentMemoryBackend) Store(ctx context.Context, opts StoreOptions) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	switch opts.Scope {
	case StoreScopeStart:
		return b.client.StartSession(ctx, opts.SessionID, resolveProject(), resolveCWD())

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
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
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
			Summary:   truncateStr(m.Title, 80),
			Content:   m.Title,
			Score:     m.Score,
			Timestamp: parseTimestamp(m.Timestamp),
		})
	}
	return entries, nil
}

// Forget deletes a memory entry from agentmemory by its ID.
func (b *AgentMemoryBackend) Forget(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	return b.client.Forget(ctx, id)
}

// formatMessages formats StoreOptions into a plain-text representation
// suitable for storing in agentmemory. It strips noise blocks and memory
// tags from content before formatting.
func (b *AgentMemoryBackend) formatMessages(opts StoreOptions) string {
	// Direct content takes priority
	if opts.DirectContent != "" {
		return stripNoiseTags(opts.DirectContent)
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
		content := stripNoiseTags(m.Content)
		if content == "" {
			continue
		}
		prefix := "User: "
		if m.Role == "assistant" {
			prefix = "Assistant: "
		}
		sb.WriteString(prefix + content + "\n")
	}
	return strings.TrimSpace(sb.String())
}

// parseTimestamp parses an ISO 8601 timestamp string from agentmemory
// (e.g. "2026-05-30T00:32:12.533Z") into a Unix timestamp in seconds.
// Returns 0 if parsing fails.
func parseTimestamp(ts string) int64 {
	if ts == "" {
		return 0
	}
	// Try ISO 8601 (RFC3339) — with and without fractional seconds
	t, err := time.Parse(time.RFC3339, ts)
	if err == nil {
		return t.Unix()
	}
	t, err = time.Parse("2006-01-02T15:04:05Z", ts)
	if err == nil {
		return t.Unix()
	}
	t, err = time.Parse("2006-01-02T15:04:05.000Z", ts)
	if err == nil {
		return t.Unix()
	}
	// Try Unix timestamp as string (seconds or milliseconds)
	n, err := strconv.ParseInt(ts, 10, 64)
	if err == nil {
		if n > 1e12 { // milliseconds → seconds
			return n / 1000
		}
		return n
	}
	return 0
}

// resolveProject returns the project identifier for agentmemory's session/start
// endpoint. It uses the git repo root if available, falling back to the hostname.
func resolveProject() string {
	if name := projectFromGit(); name != "" {
		return name
	}
	return "unknown"
}

// resolveCWD returns the current working directory.
func resolveCWD() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// projectFromGit returns the git repo root basename (e.g. "tachi").
func projectFromGit() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(out)))
}

