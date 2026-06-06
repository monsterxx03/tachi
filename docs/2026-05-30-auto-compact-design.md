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
| `DrainCompactEvents(ch)` | 收集 compact 结果（仅手动 /compact 用） |

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

> 注意：`estimateInputTokens()` 的性能已经过优化——直接接受 `[]tools.Schema` 避免 `buildLLMTools` 的全量转换开销（`token_estimate.go:26`）。

## 3. 设计

### 3.1 核心思路

在 agent loop 中，**每次 LLM 调用前**检查 token 水位。如果预估 token 数超过 `context_window × threshold` 且未处于冷却期，自动触发压缩。

与手动 `/compact` 的关键区别：
- 自动压缩在 agent loop **内部同步完成**，不经过 TUI 编排
- 使用 `provider.CreateChat`（非流式）直接调 LLM，不走 `RunOneOffStream`
- 压缩成功后替换 `messages` 指针，下一轮 loop 用新 history 继续

整个流程对用户透明——进度反馈通过事件发送到 TUI 状态栏。

### 3.2 配置

在 `config/config.go` 的 `CompactConfig` 中扩展：

```go
type CompactConfig struct {
    Timeout    time.Duration `yaml:"timeout" default:"5m"`     // 压缩 LLM 调用超时
    MaxTokens  int           `yaml:"max_tokens" default:"4096"` // 压缩 LLM 响应的 max_tokens
    Auto       bool          `yaml:"auto" default:"true"`       // 是否启用自动压缩
    Threshold  float64       `yaml:"threshold" default:"0.75"`  // 触发阈值 (lastInputTokens / contextWindow)
}
```

字段说明：
- `Timeout`: 压缩 LLM 调用的独立超时（使用 `context.Background()` + `WithTimeout`，不依赖 conversation 的 ctx）
- `MaxTokens`: 压缩响应的最大 token 数。摘要不需要太长，4096 足够
- `Auto`: 默认启用，用户可设 `false` 关闭
- `Threshold`: `lastInputTokens / contextWindow >= threshold` 时触发。默认 0.75（75%）而非 0.8，因为压缩 LLM 调用本身也需要 token 预算（headroom）

触发条件（同时满足）：
- `Auto == true && a.cfg != nil`
- `a.contextWindow > 0`
- `lastInputTokens >= contextWindow * Threshold`
- 不处于冷却期（见 §3.5）

### 3.3 Agent 侧改动

#### 3.3.1 `agent/token_estimate.go` — 增加 compact 判定

```go
// shouldAutoCompact checks whether the current context is large enough
// to warrant automatic compaction. Returns false if:
// - auto-compact is disabled in config
// - context window is unknown (<= 0)
// - token estimate is below the threshold
// - we're still in the cooldown period after a previous compact
func (a *AIAgent) shouldAutoCompact() bool {
    if a.cfg == nil || !a.cfg.Compact.Auto {
        return false
    }
    if a.contextWindow <= 0 {
        return false
    }
    if a.isCompactCooldown() {
        return false
    }

    pct := float64(a.lastInputTokens) / float64(a.contextWindow)
    threshold := a.cfg.Compact.Threshold
    return pct >= threshold
}
```

> 注意：不引入 `Reserved` 字段。纯百分比判断更简单，`Threshold` 的默认值 0.75 已隐含 headroom。

#### 3.3.2 `agent/agent_loop.go` — 插入自动压缩点

检查点放在 `runAgentLoop` 的 **loop 顶部，每次 LLM 调用之前**：

```go
func (a *AIAgent) runAgentLoop(
    ctx context.Context,
    provider llm.Provider,
    messages []llm.Message,
    opts llm.ChatOptions,
    ch chan<- AgentEvent,
) {
    // ...
    for {
        if !a.iterationBudget.consume() {
            // ... budget exhausted
            return
        }

        select {
        case <-ctx.Done():
            // ... interrupted
            return
        default:
        }

        // ── Auto-compact check (before LLM call) ──
        if a.shouldAutoCompact() {
            ch <- AgentEvent{Type: AgentEventAutoCompactStart}
            newHistory, err := a.doCompact(ctx, messages)
            if err != nil {
                a.logger.Log("Auto compact failed: %v", err)
                // 压缩失败不中断对话，继续用原来的 history
                // （但会标明当前仍处于高水位）
            } else {
                messages = newHistory
                a.setCompactCooldown()
                a.logger.Log("Auto compact completed, new history has %d messages", len(messages))
            }
            continue // 下一轮 loop 用新 history（或原 history）
        }

        // ── 原有的 LLM 调用逻辑 ──
        apiCallCount++
        llmTools := buildLLMTools(a.filterActiveSchemas(a.toolRegistry.GetSchemas()))
        streamCh, err := provider.CreateChatStream(ctx, messages, llmTools, opts)
        // ...
    }
}
```

这样选择 loop 顶部的原因：
1. **回合间压缩**：压缩发生在两次 LLM 调用之间，语义干净——不会在 tool 执行中途插入压缩
2. **覆盖所有 finish reason**：不管 `tool_calls`、`stop` 还是 `length`，下次 loop 迭代都会检查
3. **避免状态污染**：压缩替换 `messages` 后，新 history 立即生效，不影响当前轮的 tool 执行

#### 3.3.3 `doCompact` 实现（最终方案）

直接调 `provider.CreateChat`，不走 agent loop，不嵌套 stream：

```go
func (a *AIAgent) doCompact(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
    // 1. 独立超时：使用 Background() + Timeout 而不是 parent ctx，
    //    避免 conversation 取消时中断压缩（否则可能产生孤儿 session）
    compactCtx, cancel := context.WithTimeout(context.Background(), a.cfg.Compact.Timeout)
    defer cancel()

    // 转发 conversation 的取消信号——如果用户主动退出，也取消压缩
    go func() {
        select {
        case <-ctx.Done():
            cancel()
        case <-compactCtx.Done():
        }
    }()

    // 2. 构建压缩 prompt（不嵌入 history，history 作为结构化 context 传入）
    compactPrompt := commands.BuildCompactInstruction(a.cfg.Language)
    compactMsgs := make([]llm.Message, len(messages))
    copy(compactMsgs, messages)
    compactMsgs = append(compactMsgs, llm.Message{Role: "user", Content: compactPrompt})

    // 3. 直接调 provider（非流式）
    resp, err := a.provider.CreateChat(compactCtx, compactMsgs, nil, llm.ChatOptions{
        MaxTokens: a.cfg.Compact.MaxTokens,
    })
    if err != nil {
        return nil, fmt.Errorf("compact LLM call: %w", err)
    }

    summary := resp.Content

    // 4. 写 memory（压缩前持久化当前 session）
    a.StoreCompactMemory()

    // 5. FinalizeCompact 创建新 session
    systemPrompt := ""
    if len(messages) > 0 && messages[0].Role == "system" {
        systemPrompt = messages[0].Content
    }
    newHistory, err := FinalizeCompact(a.sessionManager, systemPrompt, summary)
    if err != nil {
        return nil, fmt.Errorf("finalize compact: %w", err)
    }

    // 6. 通知 memory backend 新 session 已启动
    a.StartSessionMemory()

    return newHistory, nil
}
```

关键设计决策：
- **用 `context.Background()`** 而不是传入的 `ctx`：防止 conversation 取消导致压缩中断后产生孤儿 session
- **转发取消信号**：用户主动退出时仍然取消压缩，避免不必要的 API 调用
- **没有保存/恢复 tool registry**：因为 `CreateChat` 不走 agent loop，tools 参数传 `nil` 即可，不影响 registry
- **`StartSessionMemory()`**：`FinalizeCompact` 创建新 session 后需要通知 memory backend（手动 /compact 时由后续 `RunConversationStream` 隐式触发，但自动压缩不经过那个路径）

#### 3.3.4 事件通知

两个事件足够：

```go
const (
    AgentEventAutoCompactStart = "auto_compact_start" // 开始压缩（agent loop 即将阻塞）
    AgentEventAutoCompactDone  = "auto_compact_done"  // 压缩完成
)
```

压缩完成后，agent loop 继续运行，用新 history 调用 LLM，最终产出一个正常的 `TurnComplete`。

### 3.4 TUI 侧改动

#### 3.4.1 状态栏显示

收到 `AutoCompactStart` 时状态栏显示 `[compacting...]`，收到 `AutoCompactDone` 时恢复。

由于压缩是同步 `CreateChat`（非流式），不显示实时进度。初版不做流式进度。

#### 3.4.2 事件处理

```go
// model_events.go
case AgentEventAutoCompactStart:
    m.isCompacting = true  // 复用现有字段
    // 状态栏自动显示 [compacting...]

case AgentEventAutoCompactDone:
    m.isCompacting = false
    // 新 history 已经在 agent loop 里生效
    // 下一条 TurnComplete 会显示压缩摘要
```

不需要像手动 `/compact` 那样保存/恢复 history、清除 tool registry——这些都由 agent 内部完成了。

### 3.5 压缩安全

#### 3.5.1 冷却机制

压缩成功后设置冷却标记，避免连续触发：

```go
// agent.go — 新增字段
lastCompactTokenEstimate int64   // 上次压缩时的 token 估计值
compactCooldownUntil     int64   // 冷却到期时的消息数
```

冷却条件：**token 增长小于 20%** 时不重新触发。

```go
func (a *AIAgent) shouldAutoCompact() bool {
    // ...基础检查...
    
    if a.isCompactCooldown() {
        return false
    }
    
    pct := float64(a.lastInputTokens) / float64(a.contextWindow)
    return pct >= a.cfg.Compact.Threshold
}

func (a *AIAgent) isCompactCooldown() bool {
    if a.lastCompactTokenEstimate == 0 {
        return false // 从未压缩过
    }
    // 只有 token 增长超过 20% 才重新触发
    growth := float64(a.lastInputTokens) / float64(a.lastCompactTokenEstimate)
    return growth < 1.2
}

func (a *AIAgent) setCompactCooldown() {
    a.lastCompactTokenEstimate = a.lastInputTokens
}
```

这样设计的好处：
- 不需要依赖消息数，不受消息类型影响
- 用户继续对话后 token 自然增长，到 20% 阈值后自动解除冷却
- 如果用户发了大量内容，token 陡增，冷却会更快解除

#### 3.5.2 孤儿 session 防护

`doCompact` 的压缩失败路径需要清理孤儿 session：

```go
func (a *AIAgent) doCompact(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
    // ...
    newHistory, err := FinalizeCompact(a.sessionManager, systemPrompt, summary)
    if err != nil {
        // FinalizeCompact 可能已创建新 session（sm.New 成功但后续步骤失败）
        // 尝试删除孤儿 session
        if cur := a.sessionManager.Current(); cur != nil {
            // 检查是否是新创建的 session（没有实际对话内容）
            if cur.CompactedParentID != "" {
                _ = a.sessionManager.Delete(cur.ID) // best-effort
            }
        }
        return nil, fmt.Errorf("finalize compact: %w", err)
    }
    // ...
}
```

> 需要 `session.Manager` 暴露 `Delete(id)` 方法，或使用 `sm.DeleteCurrent()`。

#### 3.5.3 回滚

如果用户对压缩结果不满意，可以使用手动的 `/compact rollback` 机制（复用现有的 `rollbackCompact`）。

自动压缩不会自动回滚——它和手动压缩创建相同结构的 session（带 `CompactedParentID` 链接），用户可以在 session 列表中看到父子关系，随时回退。

#### 3.5.4 并发安全

压缩在 agent loop goroutine 内同步执行，无并发问题。唯一的外部影响是通过 `ch` 发送事件——`ch` 是带缓冲的 channel（cap=64），不会阻塞。

### 3.6 与 `/compact` 的差异

| 方面 | `/compact`（手动） | 自动压缩 |
|------|-------------------|---------|
| 触发 | 用户输入命令 | 自动检测 context 水位 |
| LLM 调用方式 | TUI → `RunOneOffStream`（走 agent loop） | Agent → `provider.CreateChat`（直调） |
| LLM 工具 | 清除 tool registry（双保险） | 不传 tools（`CreateChat` 的 tools=ni） |
| session 处理 | `FinalizeCompact`（TUI 侧调用） | 相同（agent 侧调用） |
| TUI 变动 | 保存/恢复 history + tools | `AutoCompactStart/Done` 两个事件 |
| 冷却 | 无（用户手动触发） | 20% token 增长冷却 |
| 孤儿 session | 极少（用户手动操作） | `doCompact` 失败时清理 |

## 4. 实现步骤

### Step 1: 配置扩展 ← P0

`config/config.go` — `CompactConfig` 新增 `Auto`、`Threshold`、`MaxTokens` 字段和默认值。

```go
type CompactConfig struct {
    Timeout   time.Duration `yaml:"timeout" default:"5m"`
    MaxTokens int           `yaml:"max_tokens" default:"4096"`
    Auto      bool          `yaml:"auto" default:"true"`
    Threshold float64       `yaml:"threshold" default:"0.75"`
}
```

### Step 2: Agent 判定方法 ← P0

- `agent/token_estimate.go` — 新增 `shouldAutoCompact()`
- `agent/agent.go` — 新增 `lastCompactTokenEstimate int64` 字段

### Step 3: Agent 同步压缩 ← P0

`agent/compact.go` 或 `agent/agent_compact.go` — 新增 `doCompact(ctx, messages)` 实现。

### Step 4: Agent loop 插入 ← P0

`agent/agent_loop.go` — `runAgentLoop` loop 顶部插入检查点（§3.3.2）。

### Step 5: 冷却机制 ← P0

`agent/agent.go` — `isCompactCooldown()` / `setCompactCooldown()` 方法。

### Step 6: 孤儿 session 防护 ← P1

`session/manager.go` — 暴露 `Delete(id)` 方法（如尚不存在）；`doCompact` 失败时清理。

### Step 7: 事件类型 ← P1

`agent/agent_loop.go` — 新增 `AgentEventAutoCompactStart` / `AgentEventAutoCompactDone` 事件。

### Step 8: TUI 状态栏 ← P1

`tui/statusbar.go` — 收到 `AutoCompactStart` 时显示 `[compacting...]`。

### Step 9: TUI 事件处理 ← P1

`tui/model_events.go` — 处理 `AutoCompactStart/Done`，更新 `m.isCompacting`。

### Step 10: `language` 适配 ← P2

`agent/commands/compact.go` — `BuildCompactInstruction(language string)` 支持中/英文。

### Step 11: 测试 ← P2

- `agent/token_estimate_test.go` — `TestShouldAutoCompact` 单元测试
- `agent/agent_compact_test.go` — `TestDoCompact` 集成测试（mock provider）

## 5. 未解决问题

### 5.1 多模型场景

compact 调用是否使用主模型？目前 `/compact` 用的是主模型，但压缩理论上可以用更便宜的模型。初版复用主模型。后续可考虑 `CompactConfig.Provider` / `CompactConfig.Model` 字段。

### 5.2 压缩 LLM 超限风险

`doCompact` 把完整 history + compact prompt 发给 LLM。如果 history 本身就接近 context window 上限（比如 128K 用了 100K），压缩 LLM 调用也可能超限。

有两种应对思路：
1. **Threshold 默认 0.75**（而非 0.8）：预留 25% headroom（128K × 25% = 32K），足够容纳 compact prompt + 响应
2. **裁剪 history**：如果 history 超出预留空间，只发最近的 N 条消息（见 §5.6）

初版用方案 1，足够安全。

### 5.3 `BuildCompactInstruction` 语言适配

现有压缩 prompt 是中文。如果 `config.Language == "English"`，LLM 可能仍然用中文回复压缩摘要，导致用户看到中英混杂的界面。

解决方案：`BuildCompactInstruction(language string)` 根据 language 参数选择模板。

### 5.4 孤儿 session 清理

`doCompact` 失败时尝试删除孤儿 session（§3.5.2）。但更彻底的方案是定期后台清理：启动时扫描所有 session，删除 `len(messages) <= 2` 且 `CompactedParentID != ""` 的孤儿。

初版不做后台清理，只做 `doCompact` 失败时的即时清理。

### 5.5 Channel 模式兼容

Channel 模式下也需要自动压缩。逻辑与 TUI 模式相同：
- `doCompact` 不依赖 TUI，完全在 agent 内部完成
- `AutoCompactStart/Done` 事件可以忽略（channel 模式不渲染状态栏）
- 唯一区别：channel 模式下 `contextWindow` 需要从 provider config 获取

### 5.6 history 裁剪策略

如果 conversation 超长（比如 200 条消息），把完整 history 发给压缩 LLM 可能本身就很贵。可以考虑只发**最近 N 条消息 + 早期摘要**（如果已有）。

具体实现复杂，初版不做。`ContextWindow` 128K 以上的模型可以容纳大部分真实对话。

### 5.7 流式压缩进度

`CreateChat` 是非流式的，压缩期间用户看不到任何输出。如果压缩耗时较长（超过 10s），用户可能怀疑卡住了。

后续可升级为 `CreateChatStream`，在 agent loop 内消费 stream 并定期发送进度更新事件，让状态栏显示 `[compacting: 正在生成摘要...]`。

初版用 `CreateChat`（简单），且默认 5m 超时 + 75% threshold，正常对话的压缩通常在几秒内完成。
