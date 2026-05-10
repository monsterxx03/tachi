# Steer 机制：工具调用边界处的待发送输入注入

## 问题

当前 pending input 机制在 `AgentEventTurnComplete`（回合结束）时才将队列中的用户消息发送给 LLM。在涉及多轮工具调用的场景中，用户可能已经知道 LLM 走错了方向，但必须等整个 turn 结束才能纠正。

**例子**：
```
用户: 找出所有 Go 文件并逐个分析
LLM:  tool_call Glob("*.go") → 100 个文件
      tool_call ReadFile("file1.go") → ...
      tool_call ReadFile("file2.go") → ...
      [用户此时已输入 "只看 agent 目录下的文件"]
      ... LLM 继续读不需要的文件 ...
      TurnComplete → pending 消息才发送
```

## 目标

在每次工具调用执行完毕、LLM 准备继续下一轮 API 调用之前（以下称 **Steer Point**），检查是否有待发送的用户输入。如果有，立即注入到消息历史中，让 LLM 在下一轮 API 调用中就能看到并调整方向。

这样用户可以在 LLM "思考间隙" 实时介入，而不是等整个回合结束。

## Provider 差异：Anthropic 的 alternating role 约束

这是实现 Steer 机制的**关键约束**，需要首先理解。

### 背景：tool result 的表示方式

在 tachi 内部，tool result 统一以 `llm.Message{Role: "tool", ToolCallID: ..., Content: ...}` 表示。但在发送给不同 provider 时，转换逻辑不同：

| Provider | Tool result 的 API 表示 |
|----------|----------------------|
| **OpenAI** | `role: "tool"` — 独立角色，不同于 `user` |
| **Anthropic** | `role: "user"` + `content: [{type: "tool_result", ...}]` — 合并为 user 消息 |

### Anthropic 的 strict alternating 要求

Anthropic API 严格要求消息的 `user` / `assistant` 角色必须交替出现。连续两个 `user` 消息会返回 400 错误：

```
"messages: roles must alternate between 'user' and 'assistant',
 but found multiple 'user' roles in a row"
```

### 对 Steer 的影响

**OpenAI**（无问题）：
```
user: "找到所有 Go 文件"
assistant: [tool_calls: Glob]
tool: glob results           ← tool 角色，非 user
user: "只看 agent 目录"       ← steer，合法
assistant: ...
```
✅ `tool` 穿插在 `user`/`assistant` 之间，不违反交替规则。

**Anthropic**（有问题）：
```
user: "找到所有 Go 文件"
assistant: [tool_use: Glob]
user: [{tool_result: ...}]   ← tool result 已经是 user 角色
user: "只看 agent 目录"       ← steer，又一个 user → 连续两个 user！
assistant: ...               ← 报错
```
❌ 两个连续的 `user` 消息违反 Anthropic 的交替规则。

### 解决方案

Anthropic 的 user message 支持混合 content blocks。把 steer 文本作为 `text` block 合并到 tool_result 所在的同一个 user message 中：

```json
// Anthropic — 合法的消息格式
{
  "role": "user",
  "content": [
    {"type": "tool_result", "tool_use_id": "tool_001", "content": "glob results..."},
    {"type": "text", "text": "只看 agent 目录下的文件"}
  ]
}
```

为实现这一点，引入新的内部消息角色 `"steer"`，由各 provider converter 按自身协议处理。

### 仅 tool_calls 时触发 Steer

Steer 只在 `finish_reason == "tool_calls"` / `"tool_use"` 时触发。`max_tokens` / `length` 连续续写场景不触发 steer，避免与内部续写 user message 形成连续 user 消息。

## 架构设计

### Steer Point 的时机

Steer Point 位于 `runAgentLoop` 中 `handleFinishReason` 返回 `true`（即 finish_reason 为 `tool_calls`，工具已执行完毕）之后、下一轮 API 调用之前。

```
runAgentLoop 迭代 N:
  API call → stream → tool_call_start events
  handleFinishReason("tool_calls"):
    executeToolCalls → tool_call_args, tool_result events
    tool results 已注入 messages
    return true (继续循环)
  ← 【Steer Point】检查 pending input
  inject reminders
迭代 N+1:
  API call → LLM 看到 tool results + steer message + reminders
```

### 通信机制

采用与 `confirmRespCh` / `askUserRespCh` 相同的 channel 模式：

- **Agent 侧**：`steerRespCh chan string` 字段，在 steer point 时发送 `AgentEventSteerCheck` 事件到 eventCh，然后阻塞读取 `steerRespCh`。
- **TUI 侧**：收到 `AgentEventSteerCheck` 后，检查 `pendingQueue`。如果有内容，join 后通过 `steerRespCh` 发回；否则发空字符串。

```
Agent goroutine                    TUI Update loop
     |                                  |
     |── AgentEventSteerCheck ──→       |
     |   (block on steerRespCh)         |
     |                              检查 pendingQueue
     |                              join pending 消息
     |                              更新 chatview (添加 user msg)
     |                              remove pending indicators
     |                          ←── steerRespCh <- combined
     |   收到 steerText                |
     |   append to messages           |
     |   recordSession                |
     |── next API call ──→            |
```

### Agent 端改动

#### 1. 新增 AgentEvent 类型

```go
const (
    // ... existing ...
    AgentEventSteerCheck = "steer_check" // agent 请求 TUI 检查 pending input
)
```

不需要新增 `AgentEventSteerInjected`——TUI 在发送 steer 文本前自行更新 chatview，agent 只需在内部 messages 中注入即可。

#### 2. AIAgent 新增字段

```go
type AIAgent struct {
    // ... existing ...
    steerRespCh chan string // TUI → agent: pending input to inject at steer point
}
```

#### 3. 新增 Setter 方法

```go
// SetSteerChannel sets the channel used for steer input injection.
func (a *AIAgent) SetSteerChannel(ch chan string) {
    a.steerRespCh = ch
}
```

#### 4. `runAgentLoop` 中注入 Steer Point

在 `handleFinishReason` 返回 `true` 之后、reminder 注入之前。**仅在 `finish_reason` 为 `tool_calls`/`tool_use` 时触发**（`length`/`max_tokens` 续写时不触发，避免形成连续 user 消息）：

```go
if !a.handleFinishReason(ctx, acc, &messages, ch, apiCallCount, &lengthContinueRetries) {
    return
}

// --- Steer Point: inject pending user input after tool results ---
// Only trigger after tool calls (not length continuation), to avoid
// consecutive user messages in providers that require alternating roles.
if (acc.finishReason == "tool_calls" || acc.finishReason == "tool_use") && a.steerRespCh != nil {
    ch <- AgentEvent{Type: AgentEventSteerCheck}
    select {
    case steerText := <-a.steerRespCh:
        if steerText != "" {
            // Use internal "steer" role — provider converters handle this
            // differently based on API protocol requirements.
            messages = append(messages, llm.Message{Role: llm.RoleSteer, Content: steerText})
            a.recordSession(&session.Message{
                Type:    session.MessageTypeUser,
                Content: steerText,
            })
        }
    case <-ctx.Done():
        return
    }
}

// After tool results, inject system-reminder warnings.
if a.shouldInjectLoopReminder() { ... }
```

#### 5. Channel 生命周期管理

在 `RunConversationStream` 的 goroutine 中，stream 结束时清空 steerRespCh：

```go
go func() {
    defer close(ch)
    defer func() { a.steerRespCh = nil }()
    // ... existing code ...
}()
```

`RunOneOffStream` 同理（但其 goroutine 也会清空，且 TUI 不会在 one-off 前设置 steerRespCh，所以 steer 在 one-off 中天然不生效）。

### TUI 端改动

#### 1. Model 新增字段

```go
type Model struct {
    // ... existing ...
    steerRespCh chan string // agent 的 steer 请求通过此 channel 获取 pending input
}
```

#### 2. `sendMessage` 中创建并设置 steer channel

```go
func (m *Model) sendMessage(text string) tea.Cmd {
    // ... existing ...
    m.steerRespCh = make(chan string)
    m.agent.SetSteerChannel(m.steerRespCh)
    m.eventCh = m.agent.RunConversationStream(...)
    // ...
}
```

#### 3. `handleAgentEvent` 新增 `AgentEventSteerCheck` 处理

```go
case agent.AgentEventSteerCheck:
    if len(m.pendingQueue) > 0 {
        combined := strings.Join(m.pendingQueue, "\n\n")
        m.pendingQueue = nil
        m.chatview.RemovePendingItems()
        m.statusbar.SetPendingCount(0)
        // Add as a normal user message in chatview for visual continuity.
        m.chatview.AddMessage(chatMessage{Role: "user", Content: combined})
        // Send steer text to agent (non-blocking with select).
        select {
        case m.steerRespCh <- combined:
        default:
        }
    } else {
        select {
        case m.steerRespCh <- "":
        default:
        }
    }
    return m.nextEvent()
```

#### 4. 清理 steerRespCh

在 `AgentEventTurnComplete` 和 `AgentEventError` 中清理：

```go
// TurnComplete / Error 处理中增加:
m.steerRespCh = nil
```

## 数据结构变更汇总

### `llm/provider.go`（新增）

| 位置 | 变更 |
|------|------|
| 新增常量 | `RoleSteer = "steer"` — 内部 steer 消息角色，provider converter 各自解释 |

### `llm/anthropic.go`

| 位置 | 变更 |
|------|------|
| `convertMessages()` | 处理 `Role == "tool"` 后，检查下一条是否为 `RoleSteer`。若是，将其内容作为 `text` content block 追加到同一 user message 中，并跳过 steer 消息 |

关键代码逻辑：

```go
if msg.Role == "tool" {
    var blocks []anthropic.ContentBlockParamUnion
    steerIdx := -1
    for ; i < len(messages) && messages[i].Role == "tool"; i++ {
        blocks = append(blocks, anthropic.NewToolResultBlock(
            messages[i].ToolCallID, messages[i].Content, messages[i].IsError,
        ))
    }
    // Check if next message is steer — merge as text block into same user message
    if i < len(messages) && messages[i].Role == llm.RoleSteer {
        blocks = append(blocks, anthropic.NewTextBlock(messages[i].Content))
        steerIdx = i
    }
    i-- // back up for outer loop increment
    anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
        Role:    anthropic.MessageParamRoleUser,
        Content: blocks,
    })
    if steerIdx >= 0 {
        i = steerIdx // skip the consumed steer message
    }
    continue
}
```

### `llm/openai.go`

| 位置 | 变更 |
|------|------|
| `convertMessages()` | `Role == "steer"` 映射为 `"user"`（OpenAI 的 `tool` 角色独立，可直接追加 user 消息） |

```go
role := msg.Role
if role == llm.RoleSteer {
    role = "user"
}
```

### `agent/agent.go`

| 位置 | 变更 |
|------|------|
| `AIAgent` struct | 新增 `steerRespCh chan string` |
| 新增方法 | `SetSteerChannel(ch chan string)` |
| `AgentEvent` 常量 | 新增 `AgentEventSteerCheck = "steer_check"` |
| `runAgentLoop()` | `handleFinishReason` 返回 true 后新增 steer point 逻辑 |
| `RunConversationStream` goroutine | defer 中 `a.steerRespCh = nil` |
| `RunOneOffStream` goroutine | defer 中 `a.steerRespCh = nil` |

### `tui/model.go`

| 位置 | 变更 |
|------|------|
| `Model` struct | 新增 `steerRespCh chan string` |
| `sendMessage()` | 创建 `steerRespCh`、调用 `m.agent.SetSteerChannel()` |
| `handleAgentEvent()` | 新增 `AgentEventSteerCheck` case |
| `AgentEventTurnComplete` | 增加 `m.steerRespCh = nil` |
| `AgentEventError` | 增加 `m.steerRespCh = nil` |

### 不变更的文件

`tui/chatview.go`、`tui/styles.go`、`tui/commands.go` 无需修改——pending 消息的入队、显示、移除逻辑已完整，steer 只是多了一个排空时机。

## 渲染 / 视觉表现

Steer 注入后，chatview 的视觉表现为：

```
[user] 找出所有 Go 文件并逐个分析
[assistant] 好的，先搜索 Go 文件...
  ~ Glob(**/*.go) → 100 files
  ~ ReadFile(agent/agent.go) → ...
[user] 只看 agent 目录下的文件              ← steer 注入，显示为普通 user 消息
[assistant] 收到，调整为只看 agent 目录...   ← LLM 下一轮 API 调用的输出
  ~ Glob(agent/*.go) → 5 files
  ~ ReadFile(agent/agent.go) → ...
...
```

注意：
- Pending 指示器 `[待发送] ...` 在 steer 注入时被移除
- 注入的消息以普通 `[user]` 角色显示（和正常发送的消息外观一致）
- 由于 steer 发生在工具结果之后、下一轮 LLM 响应之前，消息时间线是连续的

## 与现有 Pending 机制的关系

Steer 是 TurnComplete 排空的**提前变体**，两者形成分层排空策略：

```
pendingQueue 中有消息
  ├── 有 Steer Point（工具调用完成后）
  │     └── Steer 注入 → pendingQueue 清空 ✓
  └── 无 Steer Point（纯文本回复，无工具调用）
        └── TurnComplete 排空 → pendingQueue 清空 ✓
```

**不变式**：pendingQueue 中的消息最终都会被排空（或被显式清空，如 Ctrl+C、/new）。

**互斥性**：如果 Steer 已排空队列，TurnComplete 看到空队列，不做任何操作。

## 边界情况

### 1. Steer 时无 pending 消息

Agent 发出 `AgentEventSteerCheck`，TUI 检查 `pendingQueue` 为空，返回空字符串。Agent 继续正常循环。延迟仅为一个 channel 往返（微秒级）。

### 2. 连续多层 Steer

用户在第 N 轮工具调用后 steer 注入消息 → LLM 看到新消息，产生新工具调用 → 第 N+1 轮工具调用后再次 Steer Point → 如果用户在此期间又输入了新消息，再次注入。

这是预期行为——用户可以在任何时候继续"预输入"。

### 3. `length`/`max_tokens` 续写时不触发 Steer

Steer 仅在 `finish_reason == "tool_calls"` / `"tool_use"` 时触发。`length`/`max_tokens` 续写场景中，`handleFinishReason` 内部已注入了一条 user 续写消息（"Please continue where you left off..."），此时再注入 steer 会在 Anthropic 侧形成连续 user 消息，导致 API 400 错误。

### 4. Steer 与 /commit、/init 等 one-off 操作

`/commit` 和 `/init` 使用 `RunOneOffStream`，TUI 不为其设置 `steerRespCh`。即使 agent 的 `steerRespCh` 未清空（理论上），新 stream 创建时会覆盖。且 `RunOneOffStream` 的 goroutine defer 也会清空 `steerRespCh`。**Steer 在 one-off 中不生效**——符合预期，因为 one-off 期间 pending 消息在 TurnComplete 时被丢弃。

### 5. Steer 与确认弹窗 / AskUser

确认弹窗（`stateAwaitingConfirmation`）和 AskUser（`stateAskUserQuestion`）发生在 `executeToolCalls` 内部。此时 Steer Point 尚未到达（`handleFinishReason` 还未返回）。**Steer 不会在确认/提问期间触发**——用户必须先完成交互，工具执行完毕后 Steer Point 才会到达。

### 6. Steer 与 Ctrl+C 中断

Ctrl+C 调用 `cancelFunc()` 清空 `pendingQueue`。如果此时 agent 正阻塞在 `steerRespCh` 上：

```go
select {
case steerText := <-a.steerRespCh:
    // ...
case <-ctx.Done():
    return  // ← ctx 被取消，走这个分支
}
```

Agent 通过 `ctx.Done()` 退出，不会死锁。TUI 侧 steerRespCh 上的发送也会因为 `select default` 不阻塞。

### 7. Steer 与 `/new` 命令

`/new` 在处理时已清空 `pendingQueue` 并取消当前 stream。如果 agent 正阻塞在 `steerRespCh`，`ctx.Done()` 会让 agent 退出。TUI 在 `/new` handler 中也调用了 `cancelFunc()`。

### 8. Steer 消息的 @-文件引用

Steer 排空时在 TUI 侧调用 `ExpandAtReferences()`（和 TurnComplete 排空一致）。展开后的文本通过 steerRespCh 发送给 agent：

```go
combined := strings.Join(m.pendingQueue, "\n\n")
expanded := ExpandAtReferences(combined)
// send expanded via steerRespCh
```

### 9. 流式期间 Steer 导致的消息重复

当 Steer 注入一条 user 消息后，LLM 继续输出。如果此后又发生 TurnComplete，agent 的 `event.Messages` 中已包含 steer 消息。TUI 更新 `m.history = event.Messages`，不会重复。

但 chatview 中的 steer 消息是 TUI 在 `AgentEventSteerCheck` 处理中添加的，而 TurnComplete 时不会重建 chatview。这意味着 steer 消息只在 chatview 中出现一次（由 TUI 添加的那次），不会重复。✓

### 10. Session 持久化与恢复

Steer 消息以 `MessageTypeUser` 记录到 session（`a.recordSession`）。Resume 时通过 `session_convert.go` 转换为 `llm.Message{Role: "user", Content: ...}` — 与普通用户消息一致。**steer 消息在恢复后是标准的 user 消息**，不保留 `"steer"` 角色（因为 `"steer"` 只是运行时注入的临时标记，不需要跨会话持久化）。

## 测试要点

1. **基本 Steer 流程**：发送需要工具调用的消息 → 流式期间输入 steer 消息 → 工具调用完成后看到 steer 消息被注入 → LLM 在下一轮响应中体现 steer 内容
2. **Steer 后 pending 指示器消失**：验证 `[待发送]` 占位行在 steer 注入后被移除
3. **无 pending 时不阻塞**：工具调用完成后无 pending 消息 → Agent 正常继续（无额外延迟）
4. **多条 pending 合并注入**：流式期间输入 3 条消息 → steer 时合并为一条注入
5. **无工具调用时 fallback 到 TurnComplete**：纯文本回复场景 → steer point 不触发 → TurnComplete 正常排空
6. **连续多轮 Steer**：steer 注入 → LLM 产生新工具调用 → 流式期间再次输入 → 再次 steer
7. **Ctrl+C 中断 + Steer**：工具调用执行中按 Ctrl+C → pending 清空 → agent 退出
8. **确认弹窗不触发 Steer**：工具需要确认时 → 输入 pending 消息 → 确认弹窗期间 Steer 不触发 → 确认后工具执行完 → Steer 触发
9. **/commit 期间不 Steer**：one-off 操作期间 steer 不生效
10. **Steer 消息的 @-文件引用**：pending 消息包含 @path → steer 注入时正确展开文件内容
11. **Anthropic Provider**：验证 steer 文本被合并到 tool_result 的同一 user message 中（text content block），不产生连续 user 消息
12. **OpenAI Provider**：验证 steer 作为独立 `role: "user"` 消息追加在 tool 消息之后
13. **`length`/`max_tokens` 续写时不触发 Steer**：验证 steer 只在 `tool_calls` finish reason 时触发
14. **Session resume 后 steer 消息正常**：包含 steer 消息的 session 恢复后，steer 消息作为普通 user 消息呈现
