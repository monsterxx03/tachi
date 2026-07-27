package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool name constant for the RecordMemory tool.
const ToolNameRecordMemory = "RecordMemory"

// MemoryRecorder is the interface that RecordMemoryTool uses to persist
// explicit LLM-initiated memories to the backend. Decouples the tool
// definition from the agent's memory and session management.
type MemoryRecorder interface {
	// RecordMemory stores a memory entry with the given content and tags.
	// The implementation is responsible for associating it with the current
	// session and passing it through the backend's filtering/upload pipeline.
	RecordMemory(ctx context.Context, content string, tags []string) error
}

// RecordMemoryTool allows the LLM to explicitly record important information
// to the memory backend. It is only registered when a memory
// backend is configured.
//
// The LLM should use this tool when it encounters information worth
// remembering across sessions: user preferences, project-specific conventions,
// important decisions, configuration details, key facts, etc. It should NOT
// be used for routine conversation content — only information with lasting
// significance.
type RecordMemoryTool struct {
	recorder MemoryRecorder
}

// NewRecordMemoryTool creates a RecordMemoryTool backed by the given recorder.
func NewRecordMemoryTool(recorder MemoryRecorder) *RecordMemoryTool {
	return &RecordMemoryTool{recorder: recorder}
}

func (t *RecordMemoryTool) Name() string { return ToolNameRecordMemory }

func (t *RecordMemoryTool) IsDestructive() bool { return true }
func (t *RecordMemoryTool) Description() string {
	return "Record important information to persistent memory. " +
		"Use when you encounter notable facts, user preferences, project conventions, " +
		"important decisions, or configuration details worth remembering across conversations. " +
		"Focus on lasting significance — routine details do not need to be recorded."
}

func (t *RecordMemoryTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"content": {
			Type:        "string",
			Description: "Required. The information to remember. Write as a clear, self-contained statement that will make sense when retrieved later. Be specific and include relevant context.",
		},
		"tags": {
			Type:        "array",
			Description: "Optional tags for categorization (e.g. [\"user-preference\", \"decision\", \"config\", \"project-convention\"]). Helps organize and filter memories.",
			Items:       map[string]string{"type": "string"},
		},
	}
}

func (t *RecordMemoryTool) Required() []string {
	return []string{"content"}
}

func (t *RecordMemoryTool) Parallel() bool { return true }

type recordMemoryResult struct {
	Success bool     `json:"success"`
	Content string   `json:"content"`
	Tags    []string `json:"tags,omitempty"`
	Message string   `json:"message,omitempty"`
}

func (t *RecordMemoryTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var params struct {
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Content == "" {
		return "", fmt.Errorf("content is required")
	}

	if t.recorder == nil {
		return "", fmt.Errorf("memory recorder not available")
	}

	if err := t.recorder.RecordMemory(ctx, params.Content, params.Tags); err != nil {
		return "", fmt.Errorf("failed to record memory: %w", err)
	}

	result := recordMemoryResult{
		Success: true,
		Content: params.Content,
		Tags:    params.Tags,
		Message: "Memory recorded successfully. It will be available for semantic recall in future conversations.",
	}

	b, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(b), nil
}
