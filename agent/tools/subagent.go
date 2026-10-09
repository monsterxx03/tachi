package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/pkg/fileutil"
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
	// resultBaseDir 非空时，超限输出落盘到该目录、只回一个指针（与 Bash / WebFetch 同一套
	// tool_result 策略）；为空则退回原地截断。只在注册前设置一次，之后只读。
	resultBaseDir string
}

// NewSubagentTool creates a new SubagentTool with the given runner.
func NewSubagentTool(runner SubagentRunner) *SubagentTool {
	return &SubagentTool{runner: runner}
}

// SetResultBaseDir enables spilling an oversized sub-agent result to disk instead of
// truncating it in place. Empty (the default) keeps the old truncate behavior.
//
// 为什么值得落盘：截断丢掉的不是"一条命令的输出"，而是已经花掉时间与 token 换来的分析
// 结果 —— 要取回只能重跑整个 sub-agent，而落盘只需调用方一次 ReadFile。sub-agent 的
// system prompt 还要求它在预算不够时"返回部分结果并说明剩下什么"，那份说明同样会被截断
// 掉，于是连"什么没做完"都一起丢了。
//
// 应在注册前调用（ReloadSubagent）；运行期改它会与并行的 sub-agent 读该字段竞争。
func (t *SubagentTool) SetResultBaseDir(dir string) { t.resultBaseDir = dir }

func (t *SubagentTool) Name() string        { return ToolNameSubAgent }
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
	if maxChars <= 0 {
		return s
	}
	// 按**字符**计数，与 Bash / WebFetch 的 spill 同一口径。用 len(s)（字节）会让中文内容
	// 提前到 1/3 处就触发，且按字节切会把一个汉字切成半个 → 乱码。
	total := utf8.RuneCountInString(s)
	if total <= maxChars-reserve {
		return s
	}
	if reserve > maxChars {
		reserve = 0
	}
	limit := maxChars - reserve

	// 先试落盘：完整结果能被 ReadFile 取回，比"砍掉即永久丢失"好得多。落盘失败（磁盘满、
	// 无权限）才退回截断 —— 那种情况下丢尾部仍好过整个工具失败。
	if t.resultBaseDir != "" {
		if p, err := t.spillOutput(s); err == nil {
			return formatSubagentSpill(total, maxChars, p)
		}
	}
	return truncateRunes(s, limit) + "\n\n⚠️ [Output truncated at " + strconv.Itoa(limit) + " chars]"
}

// truncateRunes 返回 s 的前 n 个字符（按 UTF-8 边界切，不会留下半截多字节字符）。
//
// 逐个解码到第 n 个边界即停，不把整个字符串转成 []rune —— 超限输出可能很大，全量转换
// 要额外分配一份 4 倍大小的切片。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i, count := 0, 0
	for count < n && i < len(s) {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s[:i]
}

// spillSeq 让同一纳秒内的两次落盘也不会撞名（sub-agent 是并行跑的）。
var spillSeq atomic.Uint64

// spillOutput 把完整结果写到 resultBaseDir 下的时间戳文件，返回路径。
//
// 与 Bash 的 spill 有一处不同：这里落的是**完整内容** —— sub-agent 的结果本就是内存里的
// 单个字符串，不存在 Bash 那种"ring buffer 只留尾部、头部已被丢弃"的问题。
func (t *SubagentTool) spillOutput(content string) (string, error) {
	filename := fmt.Sprintf("subagent_%d_%d.txt", time.Now().UnixNano(), spillSeq.Add(1))
	fp := filepath.Join(t.resultBaseDir, filename)
	if err := fileutil.WriteFilePrivate(fp, []byte(content)); err != nil {
		return "", err
	}
	return fp, nil
}

// formatSubagentSpill builds the compact pointer returned in place of an oversized
// result. 措辞与 formatBashSpill 一致，便于主 agent 用同一套方式处理。
func formatSubagentSpill(chars, limit int, path string) string {
	return fmt.Sprintf(
		"[SUBAGENT OUTPUT TOO LARGE] %d chars exceeds limit (%d chars).\n"+
			"Full output saved to:\n  %s\n"+
			"Use ReadFile with limit=500 (and offset to page) to read the full output.",
		chars, limit, path)
}
