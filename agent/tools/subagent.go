package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SubagentRunner is the interface SubagentTool uses to delegate execution.
// This decouples the tool definition from the execution logic, making it
// testable and allowing different executor implementations.
type SubagentRunner interface {
	// RunSubagent executes a sub-agent task. Returns the output text,
	// the sub-agent's shortID, execution statistics, and any error.
	RunSubagent(ctx context.Context, args SubagentArgs) (string, string, *SubagentResult, error)
	// AvailableToolNames returns the list of tool names available to sub-agents,
	// used to populate the tool description dynamically so LLM knows valid values
	// for the allowed_tools parameter.
	AvailableToolNames() []string
	// MaxOutputChars returns the configured output truncation threshold.
	MaxOutputChars() int
}

// SubagentArgs holds the parsed arguments for the SubAgent tool.
type SubagentArgs struct {
	Prompt         string   `json:"prompt"`
	AllowedTools   []string `json:"allowed_tools"`
	WorktreeBranch string   `json:"worktree_branch"` // Optional: git branch for worktree isolation
}

const subagentBaseDescription = `Delegate a self-contained task to an isolated sub-agent with its own context window and tool set. The sub-agent works independently and returns a single summary result.

When to use:
- Complex multi-step tasks that would bloat the main conversation context
- Self-contained research, analysis, or code exploration that doesn't need user interaction
- Tasks that can run in PARALLEL with other SubAgent calls or tool calls
- Refactoring or bulk operations across many files where intermediate thinking isn't valuable to the main conversation

Do NOT use for:
- Simple single-tool operations (just call the tool directly)
- Tasks requiring user confirmation or input
- Trivial questions answerable in a single sentence

Tips for effective delegation:
- Write detailed prompts — include file paths, patterns, specific questions
- Use allowed_tools to restrict the sub-agent to only the tools it needs (e.g. ["ReadFile", "Grep", "Glob"] for search-only tasks)
- Multiple SubAgent calls run in parallel when submitted together — partition large tasks into independent sub-tasks`

// SubagentTool delegates tasks to isolated sub-agents.
type SubagentTool struct {
	runner         SubagentRunner
	mu             sync.Mutex
	lastSubagentID string
	lastIterCount  int
	lastDuration   time.Duration
}

// NewSubagentTool creates a new SubagentTool with the given runner.
func NewSubagentTool(runner SubagentRunner) *SubagentTool {
	return &SubagentTool{runner: runner}
}

func (t *SubagentTool) Name() string { return ToolNameSubAgent }

func (t *SubagentTool) IsDestructive() bool { return true }
func (t *SubagentTool) Description() string {
	names := t.runner.AvailableToolNames()
	return subagentBaseDescription + "\n\nAvailable tools for allowed_tools: " + strings.Join(names, ", ")
}

func (t *SubagentTool) Properties() map[string]PropertySchema {
	// Dynamic items.enum: the model gets the exact tool names it may pass in
	// allowed_tools, so it cannot misspell them (e.g. "ReadFileTool").
	// Only set when non-empty — an empty enum would be invalid JSON Schema
	// (it means "no legal values").
	toolNames := t.runner.AvailableToolNames()
	items := map[string]any{"type": "string"}
	if len(toolNames) > 0 {
		enum := make([]any, len(toolNames))
		for i, n := range toolNames {
			enum[i] = n
		}
		items["enum"] = enum
	}

	return map[string]PropertySchema{
		"prompt": {
			Type:        "string",
			Description: "Detailed task description for the sub-agent. Include file paths, patterns, specific questions — the more detail, the better.",
		},
		"allowed_tools": {
			Type:        "array",
			Description: "Optional list of tool names the sub-agent is allowed to use. When empty, all available tools are inherited. Use this to restrict the sub-agent (e.g. [\"ReadFile\", \"Grep\", \"Glob\"] for read-only tasks).",
			Items:       items,
		},
		"worktree_branch": {
			Type: "string",
			Description: "Optional: git branch to checkout in the sub-agent's isolated worktree. " +
				"When empty, the worktree starts at detached HEAD (current commit). " +
				"Only meaningful when worktree mode is enabled. " +
				"Use this when the sub-agent needs to work on a specific branch " +
				"(e.g., cross-branch analysis, parallel PR development).",
		},
	}
}

func (t *SubagentTool) Required() []string { return []string{"prompt"} }

func (t *SubagentTool) Parallel() bool { return true }

func (t *SubagentTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var sa SubagentArgs
	if err := json.Unmarshal([]byte(args), &sa); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	result, subagentID, stats, err := t.runner.RunSubagent(ctx, sa)

	t.mu.Lock()
	t.lastSubagentID = subagentID
	if stats != nil {
		t.lastIterCount = stats.IterCount
		t.lastDuration = stats.Duration
	} else {
		t.lastIterCount = 0
		t.lastDuration = 0
	}
	t.mu.Unlock()

	// Output truncation protection — reserve ~80 chars for stats footer
	reserveForStats := 120
	if stats != nil && stats.IterCount > 0 {
		result = t.truncateOutput(result, reserveForStats)
	} else {
		result = t.truncateOutput(result, 0)
	}

	// Append execution stats to the result for visibility everywhere (TUI, history, LLM).
	if stats != nil && stats.IterCount > 0 {
		summary := ""
		if stats.ToolCallSummary != nil {
			summary = stats.ToolCallSummary.String()
		}
		result += FormatSubagentStats(stats.Duration, stats.IterCount, summary)
	}

	if err != nil {
		// Return partial result if available
		if result != "" {
			return fmt.Sprintf("Sub-agent completed with errors:\n\n%s\n\n⚠️ Error: %v", result, err), nil
		}
		return "", fmt.Errorf("sub-agent failed: %w", err)
	}
	return result, nil
}

// LastSubagentID returns the shortID of the most recently completed sub-agent
// invocation. Safe for concurrent use.
func (t *SubagentTool) LastSubagentID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastSubagentID
}

// LastSubagentStats returns the iteration count and duration of the most recent
// sub-agent invocation. Safe for concurrent use.
// Implements SubagentStatsCarrier.
func (t *SubagentTool) LastSubagentStats() (int, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastIterCount, t.lastDuration
}

func (t *SubagentTool) truncateOutput(s string, reserve int) string {
	maxChars := t.runner.MaxOutputChars()
	if maxChars > 0 && len(s) > maxChars-reserve {
		if reserve > maxChars {
			reserve = 0
		}
		return s[:maxChars-reserve] + "\n\n⚠️ [Output truncated at " + strconv.Itoa(maxChars-reserve) + " chars]"
	}
	return s
}
