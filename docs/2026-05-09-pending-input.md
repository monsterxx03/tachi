# 流式输出期间的待发送输入（Pending Input）

## 问题

当前 TUI 在 LLM 流式输出期间（`stateStreaming`）输入区域被禁用，用户无法打字或排队消息，只能等待当前回合完全结束。这在涉及长工具调用链的场景下尤为痛苦：LLM 可能在执行多个工具，而用户只能干等。

## 目标行为

输入在 `stateStreaming` 期间保持启用，用户可以自由输入。在流式期间按 Enter 提交的消息不会立即发送，而是进入待发送队列（pending queue），当前回合结束后自动发送。

## 详细行为规格

### 1. 排队（Queueing）

- **触发时机**：当前状态为 `stateStreaming`，用户在输入框中按下 Enter
- **行为**：消息文本被追加到 `Model.pendingQueue []string`，输入框清空，用户可以继续输入下一条
- **斜杠命令**：`/new` 仍然生效（清空队列并结束当前流），其他斜杠命令（`/model`、`/commit` 等）在流式期间应被忽略或给出提示
- **空消息**：空消息（`strings.TrimSpace(text) == ""`）不进入队列，行为同 `stateIdle`

### 2. 待发送指示器（Pending Indicators）

- 每条进入队列的消息立即在聊天视图中渲染为一条**半透明斜体**占位行
- 格式：`[待发送] 你的消息内容`
- 占位行插入在当前所有真实消息之后、流式输出块之前
- 多条待发送消息按入队顺序从上到下排列
- 当队列被消费或清空时，这些占位行从视图中移除

### 3. 自动排空（Auto-drain）

- **触发时机**：收到 `AgentEventTurnComplete` 事件
- **前提条件**：`m.pendingQueue` 非空，且不是在 `/commit` 或 `/init` 这类临时上下文中（即 `m.savedHistory == nil`）
- **操作**：
  1. 将 `pendingQueue` 中所有条目用 `"\n\n"` 连接为一条消息文本
  2. 清空 `pendingQueue`
  3. 从 chatview 中移除所有 `Role == "pending"` 的条目
  4. 对该消息文本执行 `ExpandAtReferences()`（和正常提交一样）
  5. 调用 `m.sendMessage(text)` 自动发送
- **注意**：如果 `AgentEventTurnComplete` 触发时 `savedHistory` 非空（说明是 `/commit` 或 `/init` 这类一次性操作刚结束），则**丢弃** pending queue 而不发送。因为这之后会恢复对话历史，pending 消息的上下文不匹配。

### 4. Ctrl+C 中断

- 用户按 Ctrl+C 时，除了取消当前流（`m.cancelFunc()`），还要**清空 pending queue**
- 理由：用户中断说明不想继续当前对话方向，pending 消息也随之作废
- 实现位置：`Model.handleCtrlC()` 中，在调用 `cancelFunc` 的同时 `m.pendingQueue = nil`，并从 chatview 移除 pending 条目

### 5. 确认 / 提问状态（Confirmation / AskUser）

- `stateAwaitingConfirmation` 和 `stateAskUserQuestion` 期间输入**保持禁用**
- 这是同步交互点：用户必须先回复 y/n 或填写表单，不能让 pending 消息插队
- pending queue 中的消息在这些状态期间**保留不动**——用户完成确认/提问后，流继续，pending queue 在下一个 TurnComplete 正常排空

### 6. `/new` 命令

- `/new` 处理中增加：
  1. 清空 `pendingQueue`
  2. 从 chatview 移除所有 pending 占位消息
- 现有逻辑：`/new` 调用 `m.agent.ClearSession()` 结束当前 session，下一条消息创建新 session

### 7. 流式期间的输入编辑

- 用户在流式期间可以正常编辑输入框中的草稿内容（和 `stateIdle` 完全一样）
- 粘贴处理：`Model.Update` 中的 `tea.PasteMsg` 处理需要从仅 `state == stateIdle` 扩展到 `state == stateIdle || state == stateStreaming`
- 历史导航：Ctrl+P / Ctrl+N 正常可用
- @-文件补全：正常可用
- `Ctrl+S`（复制模式）、`Ctrl+M`（MCP 管理）在流式期间不可用（它们只在 `handleKeyIdle` 中处理）

### 8. 流式期间按 Enter 处理 `/new`

- 如果用户输入 `/new` 并在流式期间按 Enter，直接处理（清空队列 + 清空对话），不走排队逻辑
- 其他斜杠命令在流式期间按 Enter 时，显示提示（如 "请等待当前回合完成"），不进入队列

## 数据结构变更

### Model 新增字段

```go
type Model struct {
    // ... 现有字段 ...
    pendingQueue []string // 流式期间排队的待发送消息
}
```

### chatMessage 新增角色

已有 `chatMessage` 结构体不需要修改，只需在 `renderMessageContent()` 中新增 `"pending"` 角色的渲染分支：

```go
case "pending":
    return pendingMsgStyle.Width(inner).Render("[待发送] " + msg.Content)
```

### 新增样式

```go
var pendingMsgStyle = lipgloss.NewStyle().
    Foreground(lipgloss.Color("#6E738D")).
    Italic(true)
```

复用已有的 `dimStyle` 或者新建一个更明确的样式。推荐新建 `pendingMsgStyle`，语义更清晰。

## 修改点清单

### `tui/model.go`

| 位置 | 修改内容 |
|------|---------|
| `setState()` | `m.input.SetEnabled(st == stateIdle \|\| st == stateStreaming)` |
| `AgentEventTurnComplete` 分支 | 增加 pending queue 排空逻辑 |
| `AgentEventError` 分支 | 清空 pending queue + 移除 pending chat items |
| `handleCtrlC()` | 调用 `cancelFunc` 时同时清空 pending queue + 移除 pending chat items |
| `tea.PasteMsg` 处理 | 条件从 `state == stateIdle` 改为 `state == stateIdle \|\| state == stateStreaming` |
| `InputSubmitMsg` 处理 | 增加流式期间的排队逻辑：非 `/new` 文本入队 + 显示 pending 指示器，`/new` 允许执行 |

### `tui/commands.go`

| 位置 | 修改内容 |
|------|---------|
| `/new` handler | 增加 `m.pendingQueue = nil` + 从 chatview 移除 pending 条目 |

### `tui/chatview.go`

| 位置 | 修改内容 |
|------|---------|
| `chatMessage` 结构体 | 不变（复用现有 `Role` 字段） |
| `renderMessageContent()` | 新增 `"pending"` case |
| 新增方法 | `RemovePendingItems()` — 从 `items` 中移除所有 `Role == "pending"` 的条目 |
| 新增方法 | `AddPendingItem(content string)` — 添加 pending 条目 |
| `ListLen()` | 已通过 `items` 长度隐式包含 pending 条目，无需修改 |
| `ListItem()` | 已通过 `renderItemCached` → `renderMessageContent` 渲染，无需修改 |

### `tui/styles.go`

| 位置 | 修改内容 |
|------|---------|
| 新增 | `pendingMsgStyle` 样式定义 |

## 渲染细节

Pending 消息在视图中位于**所有已完成的真实消息之后、当前流式输出块之前**。

`ChatView.ListLen()` 返回 `len(c.items) + (streamVisible ? 1 : 0)`，pending 条目就是普通 item，所以它们自然排在流式输出块之前。

视觉上：
```
[user] 之前的消息
[assistant] 之前的回复
[待发送] 等流结束就发这条       ← dim italic
[待发送] 然后发这条             ← dim italic
~ ReadFile(path/to/file)        ← 流式工具调用
  assistant streaming text...   ← 流式输出
```

当 pending queue 被排空时，调用 `RemovePendingItems()` 移除这些条目，视图回到正常状态。

## 边界情况

### 连续多次 TurnComplete

在 pending queue 排空后，`m.sendMessage()` 会启动新一轮流式对话。如果在此期间用户又输入了新消息，它们会再次进入 pending queue。这是预期行为——用户可以连续"预输入"多层对话。

### `/commit` 和 `/init` 期间

`/commit` 和 `/init` 是一次性操作，使用 `RunOneOffStream`。流结束后 `AgentEventTurnComplete` 中 `m.savedHistory != nil`，此时**丢弃** pending queue。不排空发送。因为这些操作会恢复原有对话历史和工具注册表，pending 消息的上下文语义不对。

### 流因错误结束

`AgentEventError` 分支中清空 pending queue。如果错误是 `interrupted`（Ctrl+C），pending queue 也在 `handleCtrlC` 中被清空；非 interrupted 错误（API 错误等）则在这里兜底清空。

### 空 pending queue

所有排空/清空逻辑在 pendingQueue 为空时是 no-op，不需要特殊处理。

### pending queue 中消息的 @-文件引用

排空时对连接后的文本调用 `ExpandAtReferences()`，和正常 `sendMessage` 流程一致。

### 输入框 placeholder

`SetEnabled(true)` 的 placeholder 是 `"Send a message... (Enter to send, Shift+Enter for newline; Ctrl+P/N history)"`。在流式期间这个文本也适用——用户确实可以"发送"（进入队列）。暂不需要区分流式/非流式的 placeholder。

## 不变式

1. `stateStreaming` 期间 `m.input.IsEnabled() == true`
2. `stateAwaitingConfirmation` 期间 `m.input.IsEnabled() == false`
3. `stateAskUserQuestion` 期间 `m.input.IsEnabled() == false`
4. pending queue 中每条消息在 chatview.items 中都有对应的 `Role == "pending"` 条目
5. `AgentEventTurnComplete` 后，pending queue 要么为空，要么已被排空（savedHistory 或 空队列时除外）

## 测试要点

1. 基本流程：流式期间输入消息 → 看到 `[待发送]` 指示器 → 流结束自动发送
2. 多条排队：连续输入 3 条 → 3 个指示器 → 流结束后合并为一条发送
3. Ctrl+C 清空：流式期间排队 2 条 → Ctrl+C → 队列清空、指示器消失
4. `/new` 清空：流式期间排队 2 条 → `/new` → 队列清空、对话清空
5. 确认弹窗：工具确认弹出期间输入保持禁用，队列保留
6. 提问表单：AskUser 弹出期间输入保持禁用，队列保留
7. `/commit` 期间排队被丢弃：发送 `/commit` → 流式期间排队 → commit 完成后队列被丢弃
8. 空消息不排队：流式期间按 Enter 空消息，不产生 pending 条目
9. 流式期间编辑正常：输入框草稿可正常编辑、粘贴、历史导航
