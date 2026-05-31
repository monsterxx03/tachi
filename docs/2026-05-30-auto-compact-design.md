# 自动压缩（Auto Compact）设计

## 1. 动机

当前 Tachi 的 `/compact` 命令需要用户手动触发，在长会话中很容易被遗忘，导致：
- 上下文窗口接近上限时，LLM 回复质量下降
- 超出窗口上限导致 API 错误（`prompt token count exceeds limit`）
- 用户需要中断工作流手动执行压缩

本地 token 估算（`token_estimate.go`）已经就位，可以为自动压缩提供触发条件。

## 2. 已有基础

### 2.1 手动 `/compact` 的流程

```
用户输入 /compact
  → TUI 设置 isCompacting=true
  → 清除 tool registry（不让 LLM 调用工具）
  → RunOneOffStream(compactInstruction, history)
     ↓
  LLM 返回摘要文本
     ↓
  FinalizeCompact():
    1. sm.New() 创建新 session
    2. 写入摘要 + "继续"消息
    3. 双向链接 oldSession.CompactedChildID ↔ newSession.CompactedParentID
    4. 返回 newHistory
     ↓
  TUI:
    替换 history = newHistory
    重建 chatview，显示压缩摘要
    恢复 tool registry
```

### 2.2 关键函数

| 函数 | 作用 |
|------|------|
| `BuildCompactInstruction()` | 构造压缩 prompt（中文） |
| `FinalizeCompact(sm, systemPrompt, summary)` | 创建新 session，双向链接，返回 newHistory |
| `StoreCompactMemory()` | 压缩前持久化当前 session 到 memory backend |
| `DrainCompactEvents(ch)` | 收集 compact 结果 |

### 2.3 现有配置

```go
type CompactConfig struct {
    Timeout time.Duration `yaml:"timeout" default:"5m"`
}
```

### 2.4 本地估算就绪

`estimateAndUpdateTokens(messages)` 已在三个插入点调用：
- `RunConversationStream` 进入前
- `RunOneOffStream` 进入前
- `runAgentLoop` loop reminder 注入前

`a.lastInputTokens` 现在是**当前上下文**的预估值（不是上一轮的滞后值）。

## 3. 设计

### 3.1 核心思路

在 agent loop 中，每次 tool 执行结束（即 `estimateAndUpdateTokens` 刚更新完 `lastInputTokens` 之后），检查：**预估 token 数是否超过 context_window × threshold**，如果超过且未压缩过，自动触发压缩。

整个流程对用户透明——进度反馈显示在状态栏中。

### 3.2 配置

在 `config/config.go` 的 `CompactConfig` 中扩展：

```go
type CompactConfig struct {
    Timeout   time.Duration `yaml:"timeout" default:"5m"`    // 压缩 LLM 调用超时
    Auto      bool          `yaml:"auto" default:"true"`     // 是否启用自动压缩
    Threshold float64       `yaml:"threshold" default:"0.8"` // 触发阈值 (lastInputTokens / contextWindow)
}
```

解释：
- `Auto`: 默认启用，用户可设 `false` 关闭
- `Threshold`: `lastInputTokens / contextWindow >= threshold` 时触发（0.8 = 80%）

触发条件（同时满足）：
- `Auto == true`
- `lastInputTokens >= contextWindow * Threshold`

### 3.3 Agent 侧改动

#### 3.3.1 `agent/token_estimate.go` — 增加 compact 判定

新增 `shouldAutoCompact() bool` 方法：

```go
func (a *AIAgent) shouldAutoCompact() bool {
    if a.cfg == nil || !a.cfg.Compact.Auto {
        return false
    }
    if a.contextWindow <= 0 {
        return false
    }
    pct := float64(a.lastInputTokens) / float64(a.contextWindow)
    threshold := a.cfg.Compact.Threshold
    reserved := a.cfg.Compact.Reserved
    return pct >= threshold || (a.contextWindow-a.lastInputTokens) < reserved
}
```

#### 3.3.2 `agent/agent_loop.go` — 插入自动压缩点

在 `runAgentLoop` 中，每次 tool 执行完后（loop reminder 注入前），增加检查：

```go
// After tool results, inject system-reminder warnings.
if a.shouldInjectLoopReminder() {
    a.estimateAndUpdateTokens(messages)

    // 自动压缩检查
    if a.shouldAutoCompact() {
        // 1. 发送 AutoCompactStart 事件
        // 2. 执行压缩（类似 /compact 逻辑）
        // 3. 替换 messages 为压缩后的新 history
        // 4. 发送 AutoCompactComplete 事件
        // 5. 重置相关状态
        continue  // 跳过 loop reminder，下一轮用新 history
    }

    rctx := a.buildReminderContext(false, true)
    // ...
}
```

但这里有一个关键问题：压缩需要 LLM 调用，而 `runAgentLoop` 里已经有 LLM 流。最简单的做法是**发送事件让 TUI 侧触发压缩**，而不是在 agent loop 里嵌套压缩。

#### 3.3.3 事件驱动方案（推荐）

新增 AgentEvent 类型：

```go
const (
    AgentEventAutoCompactTriggered = "auto_compact_triggered"
    AgentEventAutoCompactStart     = "auto_compact_start"
    AgentEventAutoCompactDone      = "auto_compact_done"
)
```

流程：

```
Agent loop:
  1. Tool 执行完毕
  2. estimateAndUpdateTokens → 发现需要压缩
  3. 发送 AgentEventAutoCompactTriggered(estimatedTokens, contextWindow)
  4. 暂停当前 loop（等待 TUI 信号）
  5. 暂时 return（以特殊状态退出，不报错）

TUI (model_events.go):
  6. 收到 AutoCompactTriggered
  7. 保存当前状态（类似 /compact）
  8. 清除 tool registry
  9. 调用 RunOneOffStream(compactInstruction, history)
  10. 收到结果 → 调用 FinalizeCompact
  11. 更新 history = newHistory
  12. 恢复 tool registry
  13. 发送 AutoCompactDone 事件（或直接触发新一轮对话）

  ↓

Agent loop (新轮次):
  14. 用新 history 继续正常对话
```

但实际上，`RunOneOffStream` 是单独启动一个 goroutine，而 agent loop 也在 goroutine 里。两者之间的协调比较复杂。

#### 3.3.4 简化方案：Agent 内同步压缩

更简洁的方案：在 agent loop 内同步完成压缩，不依赖 TUI 事件。

```go
// runAgentLoop 内部
if a.shouldAutoCompact() {
    compactResult, err := a.doCompact(ctx, messages)
    if err != nil {
        // 压缩失败，继续（发 warning）
        a.logger.Log("Auto compact failed: %v", err)
    } else {
        // 替换 messages 为压缩后历史
        *messages = compactResult
        // 重置迭代计数器？不重置，算一次迭代
        continue // 直接下一轮 loop
    }
}
```

`doCompact()` 的实现：

```go
func (a *AIAgent) doCompact(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
    // 1. 保存当前 tools（怕被清掉）
    savedTools := a.SaveToolRegistry()
    a.ClearToolRegistry()

    // 2. 调用压缩 LLM（用 RunOneOffStream，不含工具）
    systemPrompt := ""
    if len(messages) > 0 && messages[0].Role == "system" {
        systemPrompt = messages[0].Content
    }
    ch := a.RunOneOffStream(ctx, messages, BuildCompactInstruction(), systemPrompt, llm.ChatOptions{
        MaxTokens: 4096,
    })
  
    summary, err := DrainCompactEvents(ch)
    if err != nil {
        a.RestoreToolRegistry(savedTools)
        return nil, fmt.Errorf("compact LLM call: %w", err)
    }

    // 3. 写 memory
    a.StoreCompactMemory()

    // 4. Finalize
    newHistory, err := FinalizeCompact(a.sessionManager, systemPrompt, summary)
    a.RestoreToolRegistry(savedTools)
    if err != nil {
        return nil, fmt.Errorf("finalize compact: %w", err)
    }
    return newHistory, nil
}
```

但这里有一个问题：`RunOneOffStream` 会修改 agent 的 `skipMemory` 或类似状态，而且在 `doCompact` 的 ctx 基础上，同时运行两个流可能冲突。

#### 3.3.5 推荐方案：专用 provider 调用

最干净的方案：不让 `doCompact` 走 agent loop，而是直接调 provider：

```go
func (a *AIAgent) doCompact(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
    // 1. 构建压缩 prompt
    compactPrompt := BuildCompactInstruction()
    
    // 2. 直接调 provider
    compactMsgs := append([]llm.Message(nil), messages...)
    compactMsgs = append(compactMsgs, llm.Message{Role: "user", Content: compactPrompt})
    
    resp, err := a.provider.CreateChat(ctx, compactMsgs, nil, llm.ChatOptions{
        MaxTokens: a.cfg.Compact.MaxTokens,
    })
    if err != nil {
        return nil, fmt.Errorf("compact: %w", err)
    }
  
    summary := resp.Content

    // 3. 写 memory
    a.StoreCompactMemory()

    // 4. Finalize
    systemPrompt := ""
    if len(messages) > 0 && messages[0].Role == "system" {
        systemPrompt = messages[0].Content
    }
    return FinalizeCompact(a.sessionManager, systemPrompt, summary)
}
```

`CreateChat`（非流式）是 `Provider` 接口已支持的方法，不需要 stream，简单可靠。

### 3.4 TUI 侧改动

#### 3.4.1 状态栏显示

状态栏新增一个状态指示：当正在自动压缩时显示 `[compacting...]`。

新增 `stateAutoCompacting` 状态（或复用现有状态机制）。

#### 3.4.2 事件处理

新增事件类型：

```go
const (
    AgentEventAutoCompactStart = "auto_compact_start" // 开始压缩
    AgentEventAutoCompactDone  = "auto_compact_done"  // 压缩完成，带 newHistory
)
```

`TurnComplete` 事件正常发出——压缩成功后，agent loop 的后续对话会产出一个新的 TurnComplete，包含压缩摘要。

### 3.5 压缩安全

1. **防重复压缩**：每轮 tool 执行后只检查一次，成功压缩后设置 `a.compactedThisTurn = true`，在下一轮新 user message 时复位。或者记录已压缩时的消息数/轮数。

2. **最小间隔**：压缩后设置 `compactCooldown` 标记，在下一轮新 user message 到达时复位。防止同一轮连续触发。

3. **回滚**：复用 `/compact` 的 `rollbackCompact` 机制。

4. **并发安全**：压缩在 agent loop goroutine 内同步执行，无并发问题。

### 3.6 与 `/compact` 的差异

| 方面 | `/compact`（手动） | 自动压缩 |
|------|-------------------|---------|
| 触发 | 用户输入命令 | 自动检测 context 水位 |
| 调用方式 | TUI → `RunOneOffStream` | Agent → `provider.CreateChat` |
| LLM 工具 | 清除 tool registry | 清除 tool registry |
| session 处理 | `FinalizeCompact` | 相同 |
| TUI 通知 | `TurnComplete` + compact 处理 | `AutoCompactStart/Done` |
| 回滚 | `rollbackCompact` | 同 |

## 4. 实现步骤

### Step 1: 配置扩展

`config/config.go` — `CompactConfig` 新增字段和默认值。

### Step 2: Agent 判定方法

`agent/token_estimate.go` — 新增 `shouldAutoCompact()`。

`agent/agent.go` — 新增 `compactedThisTurn bool` 或 `turnCountSinceCompact int` 字段。

### Step 3: Agent 同步压缩

`agent/compact.go` — 新增 `doCompact(ctx, messages) ([]llm.Message, error)`。

### Step 4: Agent loop 插入

`agent/agent_loop.go` — `runAgentLoop` 中调用 `shouldAutoCompact()`，触发后调用 `doCompact()`。

### Step 5: 事件通知

`agent/agent_loop.go` — 新增事件类型，压缩完成时发 `AutoCompactDone`。

### Step 6: TUI 状态栏

`tui/statusbar.go` — 显示 auto compact 进度。

### Step 7: TUI 事件处理

`tui/model_events.go` — 处理 `AutoCompactStart/Done`，更新 chatview。

## 5. 未解决问题

1. **`provider.CreateChat` 超时处理** — 压缩 LLM 调用可能耗时较长，需要独立的 context/timeout。现有 `CompactConfig.Timeout`（5m）可用。

2. **多模型场景** — compact 调用是否使用主模型？目前 `/compact` 用的是主模型，但压缩理论上可以用更便宜的模型。初版复用主模型。

3. **压缩精度** — 现有 `BuildCompactInstruction` 是中文 prompt，对于英文对话可能不够优雅。初版不改变。

4. **频率控制** — 如果每次 tool 执行后都检查，成功压缩后设置冷却标记，在下一轮新 user message 时复位，不会在同一轮内重复压缩。

5. **Channel 模式兼容** — channel 模式下也需要自动压缩。逻辑相同，只是 TUI 事件替换为 channel 通知。

6. **流式压缩进度** — 压缩 LLM 调用如果用 `CreateChatStream`（非 `CreateChat`），可以在状态栏显示流式输出。初版用 `CreateChat`（简单），后续可升级。
