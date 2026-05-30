package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/agent/memory"
)

// Tool name constant for the MemoryRecall tool.
const ToolNameMemoryRecall = "MemoryRecall"

// MemoryRecallTool allows the LLM to search past memories for information
// relevant to the current task. It complements RecordMemory — while the
// system automatically recalls relevant memories on each turn, this tool
// gives the LLM explicit control to dig deeper when needed.
//
// The LLM should use this tool when it needs to recall specific information
// from past sessions: user preferences, project conventions, previous
// decisions, configuration details, or anything else worth remembering.
// It supplements the automatic semantic recall that happens on every turn.
type MemoryRecallTool struct {
	backend memory.Backend
}

// NewMemoryRecallTool creates a MemoryRecallTool backed by the given backend.
func NewMemoryRecallTool(backend memory.Backend) *MemoryRecallTool {
	return &MemoryRecallTool{backend: backend}
}

func (t *MemoryRecallTool) Name() string { return ToolNameMemoryRecall }

func (t *MemoryRecallTool) Description() string {
	return "Search persistent memory for relevant past information. " +
		"Use this when you need to recall specific information from past " +
		"sessions: user preferences, project-specific conventions, previous " +
		"decisions, configuration details, or any other notable facts. " +
		"Memories are automatically recalled when relevant — use this tool " +
		"for explicit deeper searches when the automatic recall doesn't " +
		"surface enough context. " +
		"Think about what exact query would find the memory you need."
}

func (t *MemoryRecallTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"query": {
			Type:        "string",
			Description: "Required. The search query to find relevant memories. Be specific and use keywords that would appear in the memory you're looking for (e.g. \"user prefers dark theme\", \"database connection config\", \"project build instructions\").",
		},
		"limit": {
			Type:        "integer",
			Description: "Optional. Maximum number of memories to return (default: 5, max: 20).",
		},
	}
}

func (t *MemoryRecallTool) Required() []string {
	return []string{"query"}
}

func (t *MemoryRecallTool) Parallel() bool { return true }

func (t *MemoryRecallTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var params struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if strings.TrimSpace(params.Query) == "" {
		return "", fmt.Errorf("query is required")
	}

	if t.backend == nil {
		return "", fmt.Errorf("memory backend not configured")
	}

	if params.Limit <= 0 {
		params.Limit = 5
	}
	if params.Limit > 20 {
		params.Limit = 20
	}

	entries, err := t.backend.Recall(ctx, params.Query, params.Limit)
	if err != nil {
		return "", fmt.Errorf("failed to recall memories: %w", err)
	}

	if len(entries) == 0 {
		result := map[string]any{
			"query":   params.Query,
			"limit":   params.Limit,
			"results": []any{},
			"message": "No relevant memories found.",
		}
		b, _ := json.Marshal(result)
		return string(b), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d relevant memories:\n\n", len(entries)))
	for i, e := range entries {
		sb.WriteString(fmt.Sprintf("--- Memory %d (relevance: %.2f) ---\n", i+1, e.Score))
		if e.SessionID != "" {
			sb.WriteString(fmt.Sprintf("Session: %s\n", e.SessionID))
		}
		sb.WriteString(e.Content)
		sb.WriteString("\n")
	}

	return sb.String(), nil
}
