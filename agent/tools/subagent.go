package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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

// subagentBaseDescription is the whole description — deliberately no list of
// tool names: allowed_tools carries them as an items.enum, and prose that
// repeats an enum is a fact stated twice (this description used to end with the
// runner's names spelled out).
const subagentBaseDescription = `Delegate a self-contained, multi-step task to an isolated sub-agent with its own context window and tool set; it returns one summary. Give it a detailed prompt — file paths, patterns, specific questions — and restrict allowed_tools to what the task needs. Calls submitted together run in parallel, so split independent work into separate calls. Not for a single tool call, nor for anything needing the user.`

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
func (t *SubagentTool) Description() string { return subagentBaseDescription }

func (t *SubagentTool) Properties() map[string]PropertySchema {
	// Dynamic items.enum: the model gets the exact tool names it may pass in
	// allowed_tools, so it cannot misspell them (e.g. "ReadFileTool"). The enum
	// is also the ONLY place they are listed — the description no longer repeats
	// them in prose.
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
			Description: "Detailed task description: file paths, patterns, specific questions.",
		},
		"allowed_tools": {
			Type:        "array",
			Description: "Optional. Tool names the sub-agent may use; empty inherits all available tools.",
			Items:       items,
		},
		"worktree_branch": {
			Type: "string",
			Description: "Optional git branch to check out in the sub-agent's isolated worktree " +
				"(default: detached HEAD at the current commit). Only meaningful when worktree mode " +
				"is enabled.",
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
