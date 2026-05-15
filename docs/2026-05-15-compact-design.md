# /compact — 手动对话压缩设计

## 概述

`/compact` 是一个手动触发的对话压缩命令，适用于 TUI 和 channel 模式。
它将当前 session 的完整历史发送给 LLM 生成综合摘要，然后**创建新 session** 承载压缩后的上下文，
旧 session 完整保留为历史记录。两个 session 之间通过 `compacted_child_id` / `compacted_parent_id`
双向链接。

## 动机

当前系统在上下文管理方面有两个缺口：

1. **TokenWarningReminder** 仅提醒 LLM "要简洁"，不真正缩减历史
2. **无自动截断** — 全部历史每次 API 调用都会发送，超过上下文窗口时调用直接失败

`/compact` 提供一种用户可控的压缩手段：当用户觉得对话太长、Token 消耗太大、或即将超出窗口时，手动执行压缩。

## 核心设计：创建新 Session 而非原地覆盖

**设计决策：压缩 = 创建新 session + 双向链接，旧 session 原封不动保留。**

对比两种方案：

| 方案 | 优点 | 缺点 |
|------|------|------|
| A. 原地覆盖 `messages.jsonl` | 简单 | 历史丢失、回滚困难、并发不安全、审计缺失 |
| B. 创建新 session + 链接 | 安全、可审计、可回滚 | 多一个 session 目录 |

选择方案 B。理由：

- **不可逆操作太危险**：LLM 生成的摘要可能遗漏关键信息，原地覆盖后永久丢失
- **审计需求**：用户可能想回头看 "我是怎么走到这一步的"
- **自然对齐 `/new` 语义**：compact 本质上是 "结束当前对话，用摘要开始新对话"
- **并发安全**：不修改活跃 session 的 `messages.jsonl`

### Session 链模型

```
Session A (原始对话，200 条消息)
  meta.json:   {..., compacted_child_id: "B-id"}
  messages.jsonl: [200 条消息 — 永久保留，不可变]

Session B (压缩后)
  meta.json:   {..., compacted_parent_id: "A-id", compacted_parent_title: "帮我重构用户模块"}
  messages.jsonl: [2 条消息 — 摘要 + 确认]
  thread_id:   "wx_xxx"  ← 从 Session A 迁移过来

后续对话写入 Session B 的 messages.jsonl。
```

### meta.json 新增字段

**旧 session（被压缩方）：**

```json
{
  "id": "2026-05-15-140000-a1b2c3d4",
  "thread_id": "",                        // 被清空
  "compacted_child_id": "2026-05-15-170000-e5f6g7h8"
}
```

**新 session（压缩产物）：**

```json
{
  "id": "2026-05-15-170000-e5f6g7h8",
  "thread_id": "wx_xxx",                  // 从旧 session 继承
  "title": "帮我重构用户模块",
  "provider": "anthropic",
  "model": "claude-sonnet-4-20250514",
  "working_dir": "/Users/will/repos/tachi",
  "compacted_parent_id": "2026-05-15-140000-a1b2c3d4",
  "compacted_parent_title": "帮我重构用户模块"
}
```

新 session 的 `title` 直接继承旧 session 的 title（不额外标记 `[已压缩]` 之类的前缀 — 简洁为上）。

### messages.jsonl（新 session）

```jsonl
{"type":"assistant","content":"历史摘要：\n\n[LLM 生成的摘要]\n\n---\n以上是之前对话的压缩摘要。","timestamp":"2026-05-15T17:00:00Z"}
{"type":"user","content":"请基于以上摘要继续对话。","timestamp":"2026-05-15T17:00:00Z"}
```

**设计理由**：第一条是 assistant 消息（含摘要），第二条是 user 消息（触发继续）。
这样做的好处是 `ConvertSessionToLLMMessages` 重建 history 时，摘要以 assistant 消息出现，
后续 `RunConversationStream` 收到非空 history → 跳过 system prompt 注入（第一项已是 system prompt）。

### llm.Message[] 重建

压缩后返回给调用方的 history：

```go
[]llm.Message{
    {Role: "system",   Content: systemPrompt},
    {Role: "assistant", Content: "历史摘要：\n\n" + summary + "\n\n---\n以上是之前对话的压缩摘要。"},
    {Role: "user",     Content: "请基于以上摘要继续对话。"},
}
```

关键：**显式注入 system prompt 作为第一项**。因为 `RunConversationStream` 只在 `history` 为空时注入
system prompt——压缩后的 history 非空，必须手动放进去以保证 LLM 的行为约束不丢失。

## 压缩流程

### 完整时序

```
用户输入 /compact
     │
     ▼
[前置检查] session 存在 + 消息数 ≥ 2
     │
     ▼
构建压缩 prompt（含完整对话历史作为 context）
     │
     ▼
RunOneOffStream(ctx, compactionPrompt) → 阻塞等待 LLM 生成摘要
  ├─ 不限制迭代次数（maxIterations = 0）
  ├─ 工具集完整（LLM 可能需要 ReadFile 确认上下文）
  └─ 不记录到任何 session
     │
     ▼
[成功] → 创建新 session → 写入 compact messages → 链接旧 session
        → 迁移 ThreadID → 返回 newHistory + summary
[失败] → 什么都不改，返回 error
     │
     ▼
TUI: 更新 m.history + 刷新 chatview + 更新 statusbar
Channel: 返回摘要文本给用户
```

### 迭代预算

**不限制 `max_iterations`**。理由：

- 压缩不是时间敏感操作，用户主动触发，愿意等待
- LLM 可能需要 ReadFile 查看被修改的文件来写更准确的摘要
- 绝大多数情况下 1-3 轮就完成，不会失控
- 自然安全网：LLM 最终一定会停止调用工具并输出摘要文本

TUI 模式下 agent 本来就是 `maxIterations = 0`，无需额外处理。
Channel 模式下为 compact 创建独立 agent 实例，显式传 `0`。

### 压缩 Prompt 模板

```text
你是一个对话压缩助手。请将以下对话历史压缩成一份简洁但全面的综合摘要。

摘要必须包含：
1. 用户的主要目标和问题
2. 已完成的关键操作和修改（具体文件名、命令、配置变更）
3. 重要发现和结论
4. 当前状态和待解决问题
5. 工作环境（目录、分支等）

约束：
- 用中文输出
- 保持信息密度，删除重复和无关内容
- 保留具体的文件名、命令、配置值等关键细节
- 不要添加新的分析或建议——只总结已经发生的事情

---对话历史---

[完整的对话历史，角色标记格式]
用户: ...
助手: ...
[工具调用: ReadFile(path="...")]
[工具结果: ...]
...

---
请输出压缩摘要：
```

Prompt 构建函数：

```go
// BuildCompactPrompt builds the compaction prompt from in-memory llm.Message history.
// Tool results longer than 500 chars are truncated in the prompt.
func BuildCompactPrompt(history []llm.Message) string
```

对于 channel 模式，先将 `session.Message[]` 转为 `llm.Message[]`（调用 `ConvertSessionToLLMMessages`），
再传入 `BuildCompactPrompt`。

## Agent 层

### 新文件：`agent/compact.go`

三个公开函数：

```go
// BuildCompactPrompt builds the prompt asking the LLM to produce a conversation summary.
// history is the in-memory conversation. System messages are skipped.
// Tool result content exceeding 500 chars is truncated in the prompt.
func BuildCompactPrompt(history []llm.Message) string

// FinalizeCompact creates a new session containing the compacted summary,
// links it bidirectionally to the old session, and returns the new conversation
// history (with system prompt prepended) ready for RunConversationStream.
//
// The old session's meta is updated with compacted_child_id.
// ThreadID migration is handled by the caller if needed.
//
// Parameters:
//   - sm: session manager with the OLD session loaded as current
//   - systemPrompt: prepended as the first message in the returned history
//   - summary: the LLM-generated summary text
//
// Returns:
//   - newHistory: []llm.Message ready for RunConversationStream
//   - err: any error during session creation or persistence
func FinalizeCompact(sm *session.Manager, systemPrompt string, summary string) ([]llm.Message, error)

// drainCompactEvents collects the final assistant response from a RunOneOffStream
// event channel. Returns the full response text or an error.
func drainCompactEvents(ch <-chan AgentEvent) (string, error)
```

### `FinalizeCompact` 实现细节

```
1. 从 sm.Current() 获取旧 session 信息 (title, provider, model, workingDir)
2. sm.New(provider, model, workingDir) → 创建新 session，自动设为 current
3. 写入 compact messages 到新 session (AppendMessage × 2)
4. 更新新 session title（继承旧 title）、compacted_parent_id、compacted_parent_title
5. 更新旧 session meta：compacted_child_id = 新 session ID
6. 返回 buildCompactHistory(systemPrompt, summary)
```

旧 session 的 `compacted_child_id` 更新：

```go
// 需要先加载旧 session，更新字段，再持久化
oldSess := sm.Current() // 但 sm.Current 现在指向新 session...
```

这里有个坑：`sm.New()` 会改变 `sm.Current()`。所以需要在 `sm.New()` 之前保存旧 session 引用：

```go
oldSess := sm.Current()

newSess, err := sm.New(oldSess.Provider, oldSess.Model, oldSess.WorkingDir)
// newSess 现在是 sm.Current()

// 更新新 session
newSess.Title = oldSess.Title
newSess.CompactedParentID = oldSess.ID
newSess.CompactedParentTitle = oldSess.Title
sm.UpdateMeta(newSess)

// 更新旧 session
oldSess.CompactedChildID = newSess.ID
// 但 oldSess 不是 current，需要直接操作 Store
```

当前 `session.Manager` 没有直接更新非 current session 的方法。
`UpdateMeta` 只更新 current session。需要新增或直接操作 `Store.UpdateMeta`。

**方案**：`FinalizeCompact` 需要直接使用 `session.Store` 接口。Manager 可以暴露 `Store()` 方法，
或者 `FinalizeCompact` 通过内部 helper 走 `sm.Store`（如果 Manager 暴露）。

更简单的方案：`FinalizeCompact` 中使用 `sm.UpdateMeta(oldSess)` — 但 `UpdateMeta` 内部用 `m.current`...

查看当前 `session.Manager.UpdateMeta`：
```go
func (m *Manager) UpdateMeta(session *Session) error {
    return m.store.UpdateMeta(session)
}
```

它接受 `*Session` 参数，直接调 `store.UpdateMeta(session)`，不依赖 `m.current`。
所以可以直接用！

流程修正：

```go
func FinalizeCompact(sm *session.Manager, systemPrompt string, summary string) ([]llm.Message, error) {
    oldSess := sm.Current()
    if oldSess == nil {
        return nil, fmt.Errorf("no active session to compact")
    }

    // 1. Create new session (becomes current)
    newSess, err := sm.New(oldSess.Provider, oldSess.Model, oldSess.WorkingDir)
    if err != nil {
        return nil, fmt.Errorf("create compact session: %w", err)
    }

    // 2. Write compact messages
    now := time.Now()
    sm.AppendMessage(&session.Message{
        Type: session.MessageTypeAssistant, Timestamp: now,
        Content: fmt.Sprintf("历史摘要：\n\n%s\n\n---\n以上是之前对话的压缩摘要。", summary),
    })
    sm.AppendMessage(&session.Message{
        Type: session.MessageTypeUser, Timestamp: now,
        Content: "请基于以上摘要继续对话。",
    })

    // 3. Update new session meta
    newSess.Title = oldSess.Title
    newSess.CompactedParentID = oldSess.ID
    newSess.CompactedParentTitle = oldSess.Title
    sm.UpdateMeta(newSess)

    // 4. Update old session meta
    oldSess.CompactedChildID = newSess.ID
    sm.UpdateMeta(oldSess)

    // 5. Build and return history
    return buildCompactHistory(systemPrompt, summary), nil
}
```

### Session Manager 需要暴露 Store

`sm.UpdateMeta(oldSess)` 直接可用（它接受参数不走 `m.current`）。
但有一个问题：`sm.New()` 会改变 `sm.current`，然后 `sm.UpdateMeta(oldSess)` 更新的是非 current session。
当前 `UpdateMeta` 实现直接调 `m.store.UpdateMeta(session)`，不依赖 `m.current`，所以没问题。

✅ 确认无问题。

### `buildCompactHistory`

```go
func buildCompactHistory(systemPrompt, summary string) []llm.Message {
    return []llm.Message{
        {Role: "system", Content: systemPrompt},
        {Role: "assistant", Content: fmt.Sprintf("历史摘要：\n\n%s\n\n---\n以上是之前对话的压缩摘要。", summary)},
        {Role: "user", Content: "请基于以上摘要继续对话。"},
    }
}
```

## TUI 实现

### 命令注册

`tui/commands.go` 新增：

```go
{
    Name:        "/compact",
    Description: "Compress conversation history into a summary and start a fresh session",
    handler:     (*Model).handleCompactCommand,
}
```

### Model 新增字段

```go
type Model struct {
    // ... existing fields ...
    isCompacting bool // true during compact LLM call (distinct from /commit's savedHistory)
}
```

### handleCompactCommand

```go
func (m *Model) handleCompactCommand() tea.Cmd {
    // 1. Pre-checks
    sm := m.agent.SessionManager()
    if sm == nil || !sm.HasCurrent() {
        m.chatview.AddMessage(chatMessage{Role: "assistant", Content: "没有活跃的 session 可以压缩"})
        return nil
    }
    if len(m.history) == 0 {
        m.chatview.AddMessage(chatMessage{Role: "assistant", Content: "对话历史为空，无需压缩"})
        return nil
    }

    // 2. Show user intent
    m.chatview.AddMessage(chatMessage{Role: "user", Content: "/compact"})
    m.setState(stateWaiting)
    m.chatview.ResetStreaming()
    m.thinkingView.Reset()
    m.thinkingMode = false

    // 3. Save state for rollback
    m.savedHistory = make([]llm.Message, len(m.history))
    copy(m.savedHistory, m.history)
    m.isCompacting = true

    // 4. Build compact prompt and run one-off
    prompt := agent.BuildCompactPrompt(m.history)

    ctx, cancel := context.WithCancel(context.Background())
    m.cancelFunc = cancel

    m.streamGen++
    m.eventCh = m.agent.RunOneOffStream(ctx, nil, m.systemPrompt, prompt, m.chatOpts)

    return tea.Batch(m.statusbar.Tick(), m.nextEvent())
}
```

### TurnComplete 处理

在 `handleAgentEvent` → `AgentEventTurnComplete` 中，**在 `isOneOff` 检查之前**插入 compact 处理：

```go
case agent.AgentEventTurnComplete:
    m.steerRespCh = nil
    if event.Messages != nil {
        m.history = event.Messages
    }

    // Compact handling — before one-off restore
    if m.isCompacting {
        m.isCompacting = false
        if event.Result != nil && event.Result.Error != nil {
            // Failed — restore saved history
            m.history = m.savedHistory
            m.savedHistory = nil
            m.chatview.AddMessage(chatMessage{
                Role: "error",
                Content: "压缩失败: " + event.Result.Error.Error(),
            })
            m.chatview.FinishStreaming()
            m.setState(stateIdle)
            m.cancelFunc = nil
            m.eventCh = nil
            return nil
        }

        summary := event.Result.Response
        sm := m.agent.SessionManager()

        // Save old ThreadID before FinalizeCompact (sm.New changes current)
        oldThreadID := ""
        if oldSess := sm.Current(); oldSess != nil {
            oldThreadID = oldSess.ThreadID
        }

        newHistory, err := agent.FinalizeCompact(sm, m.systemPrompt, summary)
        if err != nil {
            m.history = m.savedHistory
            m.savedHistory = nil
            m.chatview.AddMessage(chatMessage{
                Role: "error",
                Content: "压缩失败: " + err.Error(),
            })
            m.chatview.FinishStreaming()
            m.setState(stateIdle)
            m.cancelFunc = nil
            m.eventCh = nil
            return nil
        }

        // Migrate ThreadID
        if oldThreadID != "" {
            sm.SetThreadID(oldThreadID)
        }

        m.history = newHistory
        m.savedHistory = nil

        // Update usage (compact LLM call's tokens count toward the session)
        if event.Usage != nil {
            m.totalUsage.InputTokens = event.Usage.InputTokens
            m.totalUsage.OutputTokens += event.Usage.OutputTokens
            m.totalUsage.CacheCreationInputTokens += event.Usage.CacheCreationInputTokens
            m.totalUsage.CacheReadInputTokens += event.Usage.CacheReadInputTokens
            m.statusbar.SetUsage(&m.totalUsage)
            m.refreshSessionCost()
        }

        // Rebuild chatview for the new session
        m.chatview.Clear()
        m.chatview.AddMessage(chatMessage{
            Role:    "assistant",
            Content: formatCompactSummary(summary, len(m.savedHistory)),
        })
        m.chatview.FinishStreaming()
        m.syncSessionInfo()
        m.setState(stateIdle)
        m.cancelFunc = nil
        m.eventCh = nil
        return nil
    }

    isOneOff := m.savedHistory != nil
    if isOneOff {
        m.history = m.savedHistory
        m.savedHistory = nil
    } else if event.Usage != nil {
        // ... existing code unchanged ...
    }
```

### ChatView 展示

压缩成功后在 chatview 显示：

```
🔍 对话已压缩

旧会话: 帮我重构用户模块 (2026-05-15-140000-a1b2c)
旧消息数: 42 条
新会话: 帮我重构用户模块 (2026-05-15-170000-e5f6g)

摘要:
─────────────────────
[LLM 生成的摘要]
─────────────────────

使用 /sessions 可查看旧会话的完整历史。
```

`formatCompactSummary` helper：

```go
func formatCompactSummary(summary string, oldMsgCount int) string {
    var sb strings.Builder
    sb.WriteString("🔍 **对话已压缩**\n\n")
    sb.WriteString(fmt.Sprintf("旧消息数: %d 条\n", oldMsgCount))
    sb.WriteString("\n---\n\n")
    sb.WriteString(summary)
    sb.WriteString("\n\n---\n")
    sb.WriteString("💡 使用 `/sessions` 可查看旧会话的完整历史。\n")
    return sb.String()
}
```

## Channel 实现

### 命令分发

`handleSlashCommand()` 和 `executeSlashCommand()` 各加一个 case：

```go
case "/compact":
    return m.handleCompactCommand(msg.ThreadID)
```

### handleCompactCommand

```go
func (m *Manager) handleCompactCommand(threadID string) (string, error) {
    // 1. Load session
    sm := m.newSessionManager()
    sess, err := sm.FindByThreadID(threadID)
    if err != nil {
        return "", fmt.Errorf("加载 session 失败: %w", err)
    }
    if sess == nil || !sm.HasCurrent() {
        return "没有活跃的会话可以压缩。请先发送消息开始对话。", nil
    }

    sessionMsgs, err := sm.LoadMessages()
    if err != nil {
        return "", fmt.Errorf("加载消息失败: %w", err)
    }
    if len(sessionMsgs) < 2 {
        return "对话太短，无需压缩。", nil
    }

    // 2. Get provider
    prov, resolved := m.getProvider()
    if prov == nil || resolved == nil {
        return "", fmt.Errorf("provider 未初始化")
    }

    // 3. Convert to llm.Message for prompt building
    llmMsgs, err := agent.ConvertSessionToLLMMessages(sessionMsgs, resolved.Provider.Type)
    if err != nil {
        return "", fmt.Errorf("转换消息失败: %w", err)
    }
    compactPrompt := agent.BuildCompactPrompt(llmMsgs)

    // 4. Create agent (unlimited iterations)
    aiAgent := agent.NewAIAgent(prov, resolved.Provider.Model, 0)
    aiAgent.SetSkipEditConfirm(true)
    aiAgent.SetContextWindow(resolved.Provider.ContextWindow)
    mcpMgr, err := aiAgent.Configure(context.Background(), m.cfg)
    if err != nil {
        return "", fmt.Errorf("配置 agent 失败: %w", err)
    }
    if mcpMgr != nil {
        defer mcpMgr.Close()
    }
    aiAgent.UnregisterTool(tools.ToolNameAskUser)

    // 5. Run one-off
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
    defer cancel()

    eventCh := aiAgent.RunOneOffStream(ctx, nil, "", compactPrompt, llm.ChatOptions{
        MaxTokens: resolved.MaxTokens,
    })

    summary, err := drainCompactEvents(eventCh)
    if err != nil {
        return "", fmt.Errorf("压缩失败: %w", err)
    }

    // 6. Finalize
    newHistory, err := agent.FinalizeCompact(sm, m.systemPrompt, summary)
    if err != nil {
        return "", fmt.Errorf("创建压缩会话失败: %w", err)
    }
    _ = newHistory // not needed by channel — next runAgentTurn will load from session

    // 7. Migrate ThreadID to new session
    if err := sm.SetThreadID(threadID); err != nil {
        m.logger.Log("channel: /compact set thread_id: %v", err)
    }

    // 8. Format result
    return fmt.Sprintf(
        "🔍 对话已压缩\n\n原会话: %s (%s)\n消息数: %d\n\n摘要:\n%s",
        sess.Title, sess.ID[:8], len(sessionMsgs), summary,
    ), nil
}
```

**超时**：channel 的 compact 设置 5 分钟超时，防止 LLM 调用无限挂起。

**并发安全**：`/compact` 是同步命令（不进入 agent turn 循环），在处理期间该 thread 的新消息不会被处理
（因为 slash command 在 `buildHandler` 中提前返回，不进入 steer 路径）。这天然避免了并发问题。

## Session Store 变更

### `session/session.go`

```go
type Session struct {
    // ... existing fields ...
    CompactedChildID    string    `json:"compacted_child_id,omitempty"`
    CompactedParentID   string    `json:"compacted_parent_id,omitempty"`
    CompactedParentTitle string   `json:"compacted_parent_title,omitempty"`
}
```

### `session/store.go`

无需新增方法。`UpdateMeta` 已接受任意 `*Session`，可直接更新非 current session。

### `session/session.go` — Manager 方法

无需新增方法。`New`、`UpdateMeta`、`AppendMessage`、`SetThreadID` 已覆盖所有需求。

## 边界情况

### 空/短 Session

- 无消息 → "没有活跃的会话可以压缩"
- 只有 1 条用户消息 → "对话太短，无需压缩"
- 只有 1 个 turn → 同样报 "太短"

### Streaming 中

- TUI: `/compact` 在非 idle 状态 → "请等待当前回合完成后再执行命令"（与其他 slash command 一致）
- Channel: `/compact` 走 `handleSlashCommand`，此时 agent 可能正在运行，但 slash command 同步返回，
  不进入 steer 路径 — 安全

### 重复压缩

- 允许对已压缩的 session 再次压缩（此时 session B 被压缩成 session C）
- Session 链：A → B → C，通过 `compacted_child_id` 可追溯

### 压缩失败

- LLM API 错误 → **所有 session 数据不变**，返回错误。`FinalizeCompact` 在整个 LLM 调用完成之后才执行，
  LLM 失败时不会调用它
- `FinalizeCompact` 本身失败（文件系统错误等）→ 尽可能回滚（新 session 目录被创建但可以删除）

需要考虑 `FinalizeCompact` 的部分失败场景：

```
1. sm.New()          成功 ✓
2. AppendMessage #1  成功 ✓
3. AppendMessage #2  成功 ✓
4. UpdateMeta(new)   成功 ✓
5. UpdateMeta(old)   失败 ✗  ← 旧 session 没被标记，但新 session 已存在
```

处理：在第 5 步失败时，记录错误日志，但新 session 仍然可用（已写入消息）。
最坏情况：旧 session 的 `compacted_child_id` 缺失，但新 session 的 `compacted_parent_id` 正确。
这是可接受的不一致 — 单向链接仍可追溯。

### Token 计算

- 压缩 LLM 调用的 token 消耗**计入新 session 的总用量**
- TUI: `m.totalUsage` 在 TurnComplete 中更新（与普通 turn 一致）
- Channel: 压缩不经过 `runAgentTurn` → 不被记录。这是个已知限制，影响很小
  （压缩一次消耗的 token 相对整个 session 可忽略）

### 子 Agent 消息

- 子 agent 的执行记录在 `subagent/<id>.jsonl` 中
- 压缩 prompt 中的历史（`llm.Message[]`）已包含 tool_result 的摘要信息，子 agent 输出会被 LLM 看到
- 子 agent 的 `messages.jsonl` 本次不压缩（未来可扩展）

## 配置

`config.yaml` 可选新增（本次硬编码，后续扩展）：

```yaml
compact:
  max_summary_chars: 4000    # 摘要目标的字符数上限（软约束，由 prompt 传达）
  tool_result_truncate: 500  # prompt 中每条 tool result 的最大字符数
```

**本次范围**：所有参数硬编码（摘要目标 ~4000 字，tool result 截断 500 字符）。

## 文件变更清单

| 文件 | 变更 |
|------|------|
| `agent/compact.go` | **新增** — BuildCompactPrompt, FinalizeCompact, drainCompactEvents |
| `agent/compact_test.go` | **新增** — 单元测试 |
| `session/session.go` | Session 结构新增 3 个 compact 链字段 |
| `tui/commands.go` | 新增 /compact 命令注册 + handleCompactCommand |
| `tui/model.go` | Model 新增 isCompacting 字段；handleAgentEvent 新增 compact TurnComplete 分支 |
| `channel/manager/manager.go` | handleSlashCommand / executeSlashCommand 新增 /compact case；新增 handleCompactCommand |
| `docs/2026-05-15-compact-design.md` | **本文档** |

## 后续扩展（本次不做）

- `--backup` flag：压缩前额外备份 messages.jsonl（当前已通过新 session 保留完整历史，无需备份）
- 自动压缩：TokenWarningReminder 达到阈值时自动触发
- 增量压缩：只压缩最旧的 N 条消息
- `/sessions` 中展示 session 链（parent/child 关系）
- 压缩历史展示：`/compact history` 查看之前的压缩记录