# Subagent 设计文档

> 版本: 1.1 | 日期: 2026-05-09 | 状态: 设计阶段

## 一、概述

Subagent（子代理）是 Tachi 的并行任务委派机制。主 agent 通过 `SubAgent` tool 将独立的子任务委派给一个临时的、隔离的子 agent 执行，子 agent 完成后返回结果给主 agent。这解决了 LLM 单线程处理复杂任务时上下文膨胀、注意力衰减的问题。

## 二、总体架构

```
                         ┌─────────────────────────────────┐
                         │       Main Agent Loop            │
                         │   user → LLM → tool calls → ...  │
                         └──────────────┬──────────────────┘
                                        │ LLM decides to call SubAgent tool
                                        ▼
                         ┌─────────────────────────────────┐
                         │         SubagentTool             │
                         │    agent/tools/subagent.go       │
                         │                                  │
                         │  schema:                         │
                         │    prompt (string, required)      │
                         │    allowed_tools ([]string)       │
                         │    max_iterations (int)           │
                         │  parallel: true                   │
                         └──────────────┬──────────────────┘
                                        │ ExecuteContext() delegates to
                                        ▼
                         ┌─────────────────────────────────┐
                         │       SubagentExecutor           │
                         │    agent/subagent.go             │
                         │                                  │
                         │  ┌──────────────────────────┐   │
                         │  │     child AIAgent         │   │
                         │  │                           │   │
                         │  │  provider: sub/default    │   │
                         │  │  model:    sub/default    │   │
                         │  │  tools:    filtered       │   │
                         │  │  budget:   50 (default)   │   │
                         │  │                           │   │
                         │  │  skipEditConfirm: true    │   │
                         │  │  askUser:         unreg   │   │
                         │  │  session:         none    │   │
                         │  │  system_reminders: none   │   │
                         │  └──────────────────────────┘   │
                         │                                  │
                         │  runs RunOneOffStream()           │
                         │  drains all events → final text   │
                         └──────────────┬──────────────────┘
                                        │ returns string (or partial + error)
                                        ▼
                         ┌─────────────────────────────────┐
                         │   tool_result back to main agent │
                         └─────────────────────────────────┘
```

## 三、核心设计决策

| 维度 | 决策 | 理由 |
|------|------|------|
| 触发方式 | 作为 Tool 注册，LLM 自主调用 | 自然融入 agent loop，无需新命令 |
| 工具权限 | 可配置白名单（`allowed_tools`），默认全部（含 MCP 工具） | 灵活性与安全性兼顾 |
| MCP 工具 | 默认继承，`allowed_tools` 可过滤 | 与内建工具一视同仁，保持一致性 |
| 模型配置 | 独立 `subagent.provider` / `subagent.model`，fallback 主模型 | 可用轻量模型节省成本 |
| Thinking | 默认禁用，通过 `subagent.thinking` 配置项开启 | 速度优先，复杂场景可按需启用 |
| 内部可见性 | 透明 —— 主 agent 只看最终文本 | 减少上下文膨胀 |
| System prompt | 简洁模板，强调专注和高效 | 子 agent 不需要 persona |
| 错误处理 | 返回部分结果 + 错误标记 | 让主 agent 决策是否重试 |
| 并行支持 | `Parallel() = true`，受 `max_concurrency` 限制 | 多个子任务可同时进行，防资源耗尽 |
| 输出截断 | 默认 16384 字符，可配置 | 防止子 agent 输出污染主 agent 上下文 |
| 编辑确认 | 自动跳过 | 子 agent 不应阻塞等确认 |
| AskUser | 不注册 | 子 agent 不应打断用户 |
| Session | 不记录 | 子 agent 对话是短暂的 |
| 迭代预算 | 默认 200，可配置 | 防失控，与主 agent 一致 |
| 递归防护 | 代码 + prompt 双重禁止 | 不注册 SubAgent 工具 + system prompt 明确禁止 |

## 四、模块设计

### 4.1 配置扩展 — `config/config.go`

在 `Config` struct 中新增：

```go
// SubagentConfig holds configuration for sub-agent execution.
// When Provider/Model are empty, the main provider/model is used.
// Note: Go struct tags like `default:"50"` are documentation only — zero-value
// fallback is handled explicitly in code (see SubagentMaxIterations()).
type SubagentConfig struct {
    Provider       string `yaml:"provider"`        // provider name, empty → use main
    Model          string `yaml:"model"`           // model name, empty → use main
    MaxIterations  int    `yaml:"max_iterations"`  // default: 200 (hardcoded fallback)
    MaxConcurrency int    `yaml:"max_concurrency"` // default: 10 (hardcoded fallback)
    MaxOutputChars int    `yaml:"max_output_chars"`// default: 16384 (hardcoded fallback)
    Thinking       bool   `yaml:"thinking"`        // default: false
}
```

`Config` 新增字段：

```go
Subagent SubagentConfig `yaml:"subagent"`
```

**硬编码 fallback 常量**（在 `agent/subagent.go` 中定义）：

```go
const (
    defaultSubagentMaxIterations  = 50
    defaultSubagentMaxConcurrency = 4
    defaultSubagentMaxOutputChars = 16384
)
```

**YAML 示例**：

```yaml
subagent:
  provider: "minimax-anthropic"
  model: "MiniMax-M2.7"
  max_iterations: 20
  max_concurrency: 10
  max_output_chars: 16384
  thinking: false
```

### 4.2 AIAgent 扩展 — `agent/agent.go`

#### 新增字段

```go
type AIAgent struct {
    // ... 现有字段 ...
    
    subagentProvider      llm.Provider // 子 agent 专用 provider（nil = fallback）
    subagentModel         string       // 子 agent 专用 model（"" = fallback）
    subagentMaxIterations int          // 子 agent 默认迭代上限
    subagentMaxConcurrency int         // 子 agent 最大并发数
    subagentMaxOutputChars int         // 子 agent 输出截断阈值
    subagentThinking      bool         // 子 agent 是否启用 thinking
}
```

#### 新增方法

```go
// SetupSubagentProvider resolves and creates a dedicated LLM provider for
// sub-agent execution from config. Falls back to main provider when not set.
func (a *AIAgent) SetupSubagentProvider(cfg *config.Config)

// SubagentProvider returns the sub-agent provider or falls back to main.
func (a *AIAgent) SubagentProvider() llm.Provider

// SubagentModel returns the sub-agent model or falls back to main.
func (a *AIAgent) SubagentModel() string

// SubagentMaxIterations returns the configured max iterations for sub-agents.
// Returns hardcoded default (50) when config value is 0.
func (a *AIAgent) SubagentMaxIterations() int

// SubagentMaxConcurrency returns the concurrency semaphore limit.
// Returns hardcoded default (4) when config value is 0.
func (a *AIAgent) SubagentMaxConcurrency() int

// SubagentMaxOutputChars returns the output truncation threshold.
// Returns hardcoded default (16384) when config value is 0.
func (a *AIAgent) SubagentMaxOutputChars() int

// SubagentThinking returns whether sub-agents should enable thinking.
func (a *AIAgent) SubagentThinking() bool
```

#### Configure() 修改

在 `Configure()` 末尾注册 `SubagentTool`：

```go
// 在 Configure() 中，注册完 MCP 工具后：
subTool := tools.NewSubagentTool(a)
a.RegisterTool(subTool)
```

### 4.3 SubagentTool — `agent/tools/subagent.go`

#### 接口定义

```go
// SubagentRunner is the interface SubagentTool uses to delegate execution.
// This decouples the tool definition from the execution logic, making it
// testable and allowing different executor implementations.
type SubagentRunner interface {
    RunSubagent(ctx context.Context, prompt string, allowedTools []string, maxIterations int) (string, error)
    // AvailableToolNames returns the list of tool names available to sub-agents,
    // used to populate the tool description dynamically so LLM knows valid values
    // for the allowed_tools parameter.
    AvailableToolNames() []string
    // MaxOutputChars returns the configured output truncation threshold.
    MaxOutputChars() int
}
```

#### Tool 实现

```go
type SubagentTool struct {
    runner SubagentRunner
}

func NewSubagentTool(runner SubagentRunner) *SubagentTool
```

**Schema**:

| 属性 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `Name()` | — | — | `"SubAgent"` |
| `Description()` | — | — | 详细描述何时使用 SubAgent（见下方） |
| `Parallel()` | — | — | `true` |
| `prompt` | string | yes | 给子 agent 的任务描述，越详细越好 |
| `allowed_tools` | []string | no | 允许的工具名称列表，为空则继承全部 |
| `max_iterations` | int | no | 覆盖默认迭代上限，0=使用默认值 |

**Description 文本（核心 —— 这是 LLM 的调用指南）**：

Description 通过 `SubagentTool.Description()` 方法动态生成，末尾附带可用工具列表：

```go
func (t *SubagentTool) Description() string {
    names := t.runner.AvailableToolNames()
    return baseDescription + "\n\nAvailable tools for allowed_tools: " + strings.Join(names, ", ")
}
```

**baseDescription 模板**：

```
Delegate a self-contained task to an isolated sub-agent with its own context window and tool set. The sub-agent works independently and returns a single summary result.

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
- Multiple SubAgent calls run in parallel when submitted together — partition large tasks into independent sub-tasks
```

**ExecuteContext 实现**：

```go
func (t *SubagentTool) ExecuteContext(ctx context.Context, args string) (string, error) {
    var sa SubagentArgs
    json.Unmarshal([]byte(args), &sa)

    maxIters := sa.MaxIterations
    if maxIters <= 0 {
        maxIters = 0 // will use default in executor
    }

    result, err := t.runner.RunSubagent(ctx, sa.Prompt, sa.AllowedTools, maxIters)
    
    // 输出截断保护
    result = t.truncateOutput(result)

    if err != nil {
        // Return partial result if available
        if result != "" {
            return fmt.Sprintf("Sub-agent completed with errors:\n\n%s\n\n⚠️ Error: %v", result, err), nil
        }
        return "", fmt.Errorf("sub-agent failed: %w", err)
    }
    return result, nil
}

func (t *SubagentTool) truncateOutput(s string) string {
    maxChars := t.runner.MaxOutputChars() // 从 config 获取，默认 16384
    if maxChars > 0 && len(s) > maxChars {
        return s[:maxChars] + "\n\n⚠️ [Output truncated at " + strconv.Itoa(maxChars) + " chars]"
    }
    return s
}
```

### 4.4 SubagentExecutor — `agent/subagent.go`

#### 核心类型

```go
// SubagentExecutor implements SubagentRunner by creating and running child
// AIAgent instances. It also manages the concurrency semaphore for parallel
// sub-agent execution.
type SubagentExecutor struct {
    parentAgent *AIAgent
    logger      *debuglog.Logger
    sem         chan struct{} // concurrency semaphore, size = MaxConcurrency
}

func NewSubagentExecutor(parent *AIAgent, logger *debuglog.Logger) *SubagentExecutor {
    maxConc := parent.SubagentMaxConcurrency()
    return &SubagentExecutor{
        parentAgent: parent,
        logger:      logger,
        sem:         make(chan struct{}, maxConc),
    }
}

// AvailableToolNames returns all tool names the sub-agent can use (for description).
func (e *SubagentExecutor) AvailableToolNames() []string {
    // 从 parentAgent 获取全部注册工具名，排除 AskUserQuestion 和 SubAgent
    // ...
}

// MaxOutputChars returns the configured output truncation threshold.
func (e *SubagentExecutor) MaxOutputChars() int {
    return e.parentAgent.SubagentMaxOutputChars()
}
```

#### System Prompt

```go
const subagentSystemPrompt = `You are a focused sub-agent of Tachi, an AI coding assistant. Complete the delegated task efficiently and return a clear summary.

Rules:
- Stay strictly on-task. Do not explore tangents or make unrelated changes.
- Use tools aggressively — read files, search code, run commands as needed.
- DO NOT ask the user questions. If you need input, explain what's missing in your summary.
- DO NOT attempt to delegate to sub-agents — you cannot spawn further sub-agents.
- File edits are auto-confirmed. Be careful — double-check before writing.
- If the task is too large for your budget, return your best partial results with a note about what remains.
- Format your output for the main agent to read: structured, concise, actionable.

Your output goes directly back to the main agent — no preamble, no closing remarks like "I've completed the task". Just the findings.`
```

#### 核心方法

```go
func (e *SubagentExecutor) RunSubagent(
    ctx context.Context,
    prompt string,
    allowedTools []string,
    maxIterations int,
) (string, error) {
    // 0. 获取并发信号量（阻塞直到有空位或 ctx 取消）
    select {
    case e.sem <- struct{}{}:
        defer func() { <-e.sem }()
    case <-ctx.Done():
        return "", ctx.Err()
    }

    // 1. 确定 provider 和 model（fallback 逻辑）
    provider := e.parentAgent.SubagentProvider()
    model := e.parentAgent.SubagentModel()

    // 2. 确定迭代预算
    if maxIterations <= 0 {
        maxIterations = e.parentAgent.SubagentMaxIterations()
    }

    // 3. 构建 child agent（带唯一标识的 logger 用于调试）
    shortID := uuid.New().String()[:8]
    childLogger := e.logger.WithPrefix(fmt.Sprintf("[subagent:%s]", shortID))
    
    child := agent.NewAIAgent(provider, model, maxIterations)
    child.SetSkipEditConfirm(true)
    child.SetLogger(childLogger)
    child.SetReminderCollector(nil) // 禁用所有 system reminders
    // 不需要 session manager
    // 不需要 AskUser tool

    // 4. 注册工具（按 allowedTools 过滤）
    child.RegisterToolsForSubagent(e.parentAgent, allowedTools)

    // 5. 确定 thinking 配置
    thinking := e.parentAgent.SubagentThinking()

    // 6. 通过 RunOneOffStream 执行
    ch := child.RunOneOffStream(ctx, provider, subagentSystemPrompt, prompt, llm.ChatOptions{
        MaxTokens: defaultMaxTokens,
        Thinking:  &thinking,
    })

    // 7. 消费事件，收集结果 + 统计
    var sb strings.Builder
    startTime := time.Now()
    iterCount := 0
    
    for event := range ch {
        switch event.Type {
        case agent.AgentEventTextDelta:
            sb.WriteString(event.TextDelta)
        case agent.AgentEventToolResult:
            iterCount++
        case agent.AgentEventError:
            // 错误时返回部分结果
            duration := time.Since(startTime)
            childLogger.Log("completed with error | iters=%d duration=%s output_len=%d",
                iterCount, duration, sb.Len())
            if event.Result != nil && event.Result.Error != nil {
                return sb.String(), event.Result.Error
            }
            return sb.String(), fmt.Errorf("sub-agent error")
        }
    }

    // 8. 记录统计信息
    duration := time.Since(startTime)
    childLogger.Log("completed | iters=%d duration=%s output_len=%d",
        iterCount, duration, sb.Len())

    return sb.String(), nil
}
```

#### 工具过滤逻辑

```go
// RegisterToolsForSubagent registers a filtered subset of the parent's tools
// on the child agent. If allowedTools is empty, all parent tools are registered
// EXCEPT AskUser (which sub-agents should never have).
func (a *AIAgent) RegisterToolsForSubagent(parent *AIAgent, allowedTools []string) {
    // ...
}
```

**过滤规则**：

1. `allowedTools` 为空 → 继承父 agent 全部工具（排除 `AskUserQuestion` 和 `SubAgent`）
2. `allowedTools` 非空 → 只注册白名单中的工具
3. `AskUserQuestion` 永远不注册（子 agent 不应打断用户）
4. `SubAgent` 永远不注册（禁止递归，双重保险：代码层 + system prompt）
5. `EditFile` 保留（子 agent 需要写入能力），但 `skipEditConfirm=true` 确保不阻塞
6. MCP 工具（`mcp__*`）默认继承，`allowed_tools` 可按名称过滤

> **MCP 线程安全说明**：MCP tool 底层通过 `mcp.Manager` 维护连接池，`CallTool` 调用是并发安全的。多个子 agent 并发调用同一 MCP server 不会产生竞态问题。

#### 并行执行

`SubagentTool.Parallel() = true` 意味着：

```
Main LLM output:
  tool_call[0]: SubAgent("搜索所有 error 处理模式", allowed_tools=["Grep","Glob","ReadFile"])
  tool_call[1]: SubAgent("搜索最佳实践文章", allowed_tools=["WebSearch","WebFetch"])

→ SubagentExecutor 并行创建两个 child agent
→ 受 max_concurrency 信号量控制（默认 10），超限时排队等待
→ 两个 RunOneOffStream 并发执行
→ 两个结果同时返回给 tool_executor
→ 两个 tool_result message 追加到主 agent 消息历史
```

由于 `groupToolCalls()` 将相邻的 `parallel=true` 工具合并为同一组，且 SubagentTool 的 `Parallel()=true`，所以同一个 assistant turn 中的多个 `SubAgent` 调用会同组并行执行。

**并发数控制**：`SubagentExecutor` 内的 `sem` channel 充当信号量，确保全局不超过 `max_concurrency`（默认 10）个子 agent 同时运行。超额的调用会在 `select` 处阻塞等待，直到有空位或 context 取消。


## 五、执行流程详解

### 5.1 主 Agent 循环视角

```
1. User sends message
2. Main LLM receives [system, history..., user_message]
3. Main LLM decides to call SubAgent tool(s):
   → assistant message with tool_calls
4. executeToolCalls() groups calls:
   → SubAgent("task A") and SubAgent("task B") same group (Parallel=true)
5. executeToolCallsParallel():
   a. emit ToolCallArgs events (TUI shows "🔄 SubAgent [task A]..." spinner)
   b. acquire semaphore slots (up to max_concurrency)
   c. run SubAgent calls concurrently via goroutines
   d. each goroutine:
      - creates child AIAgent with unique ID
      - calls RunOneOffStream()
      - drains events, collects text
      - emits AgentEventSubagentProgress periodically (TUI updates iteration count)
   e. emit ToolResult events (TUI shows results)
6. Tool result messages appended to main agent history
7. Main LLM receives updated history → continues
```

### 5.2 子 Agent 内部视角

```
1. RunOneOffStream() is called with:
   - system: subagentSystemPrompt
   - user:   wrapped prompt (no system reminders)
2. runAgentLoop() starts:
   a. LLM API call with sub-agent messages + filtered tools
   b. stream consumer processes deltas
   c. handleFinishReason():
      - "stop" → returns final text ✓
      - "tool_calls" → executeToolCalls() → auto-confirm edits → loop
      - "length" → continue up to 3 retries → then returns partial
      - budget exhausted → returns partial + error
3. All text_delta events concatenated into final string
4. Final string wrapped in SubagentTool's ExecuteContext → tool_result
```

### 5.3 错误处理路径

| 场景 | 主 Agent 收到的 tool_result |
|------|---------------------------|
| 正常完成 | 子 agent 完整输出文本（受 `max_output_chars` 截断保护） |
| 输出超长 | 截断至 16384 字符 + `⚠️ [Output truncated at 16384 chars]` |
| 迭代预算耗尽 | 部分输出 + `⚠️ Error: iteration budget exhausted` |
| LLM API 报错 | 部分输出（如有）+ `⚠️ Error: API call failed: ...` |
| Context 取消 | 部分输出（如有）+ `⚠️ Error: context cancelled` |
| 并发信号量超时 | 无输出 + `⚠️ Error: context cancelled`（ctx 被取消时） |
| 工具执行报错 | 正常工具错误被 `tool_result(is_error=true)` 处理，子 agent 自行恢复或停止 |

设计原则：**永远返回尽可能多的有用信息 + 错误标记**，让主 agent 判断下一步。

### 5.4 TUI 进度展示

子 agent 通过 `AgentEventSubagentProgress` 事件向 TUI 报告迭代进度：

```go
// 新增事件类型
AgentEventSubagentProgress AgentEventType = "subagent_progress"

// 事件内容
type SubagentProgressData struct {
    ToolCallID string // 对应的 tool_call ID，用于定位 TUI 中的展示位置
    Iteration  int    // 当前迭代次数
    MaxIter    int    // 最大迭代次数
    ToolName   string // 当前正在执行的工具名（可选）
}
```

**TUI 展示效果**：

```
⏳ SubAgent ["搜索错误处理模式"] — iter 3/50
⏳ SubAgent ["搜索最佳实践"] — iter 5/50
```

**实现方式**：在 `SubagentExecutor.RunSubagent()` 的事件消费循环中，每收到一个 `AgentEventToolResult` 时，通过一个 progress callback（或 channel）将迭代计数推送给外层。外层的 `executeToolCallsParallel()` 将其转换为 `AgentEventSubagentProgress` 发送给 TUI。

## 六、与非 SubAgent 路径的对比

| 维度 | 直接 Tool Call | SubAgent |
|------|---------------|----------|
| 上下文 | 每一步 tool call/result 都在主对话中 | 子对话隔离，主对话只看到最终结果 |
| Thinking | 每步 thinking 可见 | thinking 不可见 |
| 并行 | 同类 tool 可并行 | 子 agent 间可并行，子 agent 内部也可并行 |
| 成本 | 零额外开销 | 一次额外的 API round-trip |
| Session | 记录进 session | 不记录 |
| 适用场景 | 简单操作、需要用户看到过程 | 复杂多步、上下文敏感、并行化 |

## 七、文件变更清单

| 文件 | 变更类型 | 说明 |
|------|---------|------|
| `config/config.go` | 修改 | 新增 `SubagentConfig` 结构体（含 MaxConcurrency/MaxOutputChars/Thinking）+ `Config.Subagent` 字段 |
| `agent/agent.go` | 修改 | 新增 6 个字段 + `SetupSubagentProvider()` + 6 个 getter + `RegisterToolsForSubagent()` + `Configure()` 末尾注册 SubagentTool |
| `agent/tools/subagent.go` | **新文件** | `SubagentTool` + `SubagentArgs` + `SubagentRunner` 接口（含 `AvailableToolNames()` + `MaxOutputChars()`）+ `truncateOutput()` |
| `agent/subagent.go` | **新文件** | `SubagentExecutor` + `NewSubagentExecutor()` + `subagentSystemPrompt` + `RunSubagent()` + hardcoded default 常量 + 信号量逻辑 |
| `agent/event.go` | 修改 | 新增 `AgentEventSubagentProgress` 事件类型（用于 TUI 进度展示） |
| `tui/view.go` | 修改 | 处理 `AgentEventSubagentProgress` 事件，展示子 agent 迭代进度 |
| `pkg/debuglog/` | 可能修改 | 新增 `WithPrefix()` 方法（如当前不存在） |
| `agent/tools/subagent_test.go` | **新文件** | SubagentTool 单元测试（schema、截断、description） |
| `agent/subagent_test.go` | **新文件** | SubagentExecutor 单元测试（信号量、工具过滤、结果收集） |
| `config/config_test.go` | 可能修改 | SubagentConfig 零值 fallback 测试 |

## 八、潜在问题和注意事项

### 8.1 递归 SubAgent

如果 LLM 在子 agent 中也触发 SubAgent 调用，会递归创建子 agent。

**解决方案（双重保险）**：
1. **代码层**：`RegisterToolsForSubagent()` 中**不注册 `SubAgent` 工具**，从根本上禁止
2. **Prompt 层**：子 agent 的 system prompt 明确写有 "DO NOT attempt to delegate to sub-agents"

两层防护确保即使未来重构工具注册逻辑，也不会意外引入递归。

### 8.2 Context Window 超限

子 agent 的 context window 可能因大量 tool result 而超限。

**缓解方案**：
- 默认仅 50 次迭代，自然限制上下文增长
- `allowed_tools` 白名单机制鼓励 LLM 只给必要的工具
- 子 agent 使用 token-warning reminder（可选，从主 agent 继承）

**不需要在主 agent 中处理**：因为子 agent 运行在独立的 provider 实例中，超限只会导致子 agent 报错，返回给主 agent 的是 tool_result error，主 agent 据此决定是否重试。

### 8.3 子 Agent 输出过长

如果子 agent 产生了很长的文本输出，tool_result 会直接进入主 agent 的上下文。

**解决方案**：
- **硬截断**：`SubagentTool.truncateOutput()` 在 `ExecuteContext` 返回前对结果进行截断，默认阈值 16384 字符（可通过 `subagent.max_output_chars` 配置）
- 截断时追加 `⚠️ [Output truncated at N chars]` 标记，让主 agent 知悉信息不完整
- 子 agent system prompt 也强调 "concise, actionable" 输出，从源头降低超长风险

### 8.4 多个 SubAgent 并发时的资源竞争

多个子 agent 可能同时编辑同一文件。

**缓解方案**：
- `max_concurrency` 信号量（默认 10）限制并发子 agent 数量，降低冲突概率
- 目前主 agent 的工具系统也不处理并发写入冲突，子 agent 继承同样的行为
- 建议在 system prompt 中提示 LLM 避免给并行子 agent 分配写入相同文件的任务
- 长期可引入文件锁机制（超出本次设计范围）

### 8.5 子 Agent 的 System Reminder

子 agent 不需要 iteration-warning/token-warning/git-reminder/project-context。

**实现方式**：子 agent 创建时调用 `child.SetReminderCollector(nil)` 禁用所有 reminder。

### 8.6 调试日志辨识度

多个子 agent 并发时，日志会混在同一个 `debug.log` 文件中。

**解决方案**：每个子 agent 创建时生成 8 位短 UUID，logger 带 `[subagent:<id>]` 前缀。这样可以通过 grep 快速过滤特定子 agent 的日志。

```
[subagent:a3f2c8b1] completed | iters=7 duration=12.3s output_len=4210
[subagent:e9d1f4a7] completed with error | iters=2 duration=3.1s output_len=820
```

### 8.7 SubagentRunner 接口位置

`SubagentRunner` 接口定义在 `agent/tools/subagent.go` 中（工具包定义自己依赖的接口），由 `agent/subagent.go` 中的 `SubagentExecutor` 实现。

**注意**：这不会产生循环依赖，因为 `agent/tools` 包只定义接口，不引用 `agent` 包的具体类型。`agent` 包导入 `agent/tools`（注册工具），`agent/tools` 不反向依赖 `agent`。

## 九、测试策略

### 单元测试

| 测试对象 | 测试内容 |
|---------|---------|
| `SubagentTool` | schema 正确、参数验证、ExecuteContext 调用 runner 接口正确、输出截断 |
| `SubagentTool.truncateOutput` | 不截断、刚好截断边界、截断标记正确 |
| `SubagentTool.Description` | 动态工具列表正确拼接 |
| `SubagentExecutor` | 正确创建 child agent、工具过滤、结果收集、错误处理、并发信号量 |
| `SubagentExecutor` 并发 | 启动 N>max_concurrency 个 goroutine 验证排队行为 |
| `AIAgent.RegisterToolsForSubagent` | 白名单过滤、AskUser 排除、SubAgent 排除、空名单=全部、MCP 工具继承 |
| `SubagentConfig` defaults | 零值 fallback 到 hardcoded 常量 |

### 集成测试（可选）

- 真实 LLM 调用：给"找到所有 TODO 注释"的 prompt，验证子 agent 返回结果
- 并行测试：同时运行 2 个 SubAgent，验证结果独立
- 预算耗尽测试：设置 `maxIterations=2` 验证部分结果返回

### Mock 测试

`SubagentRunner` 接口使 `SubagentTool` 可以独立于 `SubagentExecutor` 测试：

```go
type mockRunner struct {
    result        string
    err           error
    toolNames     []string
    maxOutputChars int
}
func (m *mockRunner) RunSubagent(...) (string, error) { return m.result, m.err }
func (m *mockRunner) AvailableToolNames() []string    { return m.toolNames }
func (m *mockRunner) MaxOutputChars() int             { return m.maxOutputChars }
```

## 十、未来扩展方向

1. **SubAgent 输出结构化**：除了纯文本，可支持 JSON 格式（如文件列表、检查清单）
2. **SubAgent 缓存**：相同 prompt + 文件状态不变时缓存结果
3. **SubAgent 优先级**：后台低优先级子 agent（不阻塞主 agent）
4. **SubAgent 流式输出**：子 agent 边执行边向主 agent 流式报告（类似 gradio 的 streaming）
5. **TUI 进度增强**：展示子 agent 正在执行的具体工具名称、当前文件等
6. **Token 成本追踪**：累计所有子 agent 的 token 使用量，展示在 status bar 或 session 统计中
7. **文件锁机制**：防止并行子 agent 写入冲突（需要全局协调器）

