# API 请求记录（System Prompt + Tool Schema）设计

## 背景

`/transcript` 报告此前只展示会话消息流（user / thinking / text / tool_call / tool_result），
但**模型实际看到什么**——system prompt 和随请求发送的工具定义（含完整 JSON schema）——
完全没有留痕。排查"模型为什么这么回答"时，只能靠猜测。

## 目标

1. 每次 LLM API 调用时，把请求携带的 **system prompt** 和 **tool list（含 schema）** 持久化。
2. 在 `/transcript` HTML 报告中按 turn 展示：模型在这一轮看到了什么。

## 数据模型

`session.APIRequest` 记录一次 API 调用的请求负载，主会话落盘为 `<session>/api_requests.jsonl`
（每行一条，随 session 目录一起清理）：

```go
type APITool struct {
    Name        string          `json:"name"`
    Description string          `json:"description"`
    Parameters  json.RawMessage `json:"parameters,omitempty"` // 完整 JSON schema
}

type APIRequest struct {
    Timestamp time.Time `json:"timestamp"`
    // Iteration 是会话内 1-based API 调用序号；tool_call / tool_result 消息
    // 携带同一序号，把每个工具执行链接回产生它的请求。
    Iteration    int       `json:"iteration,omitempty"`
    SystemPrompt string    `json:"system_prompt"`
    // UserPrompt 是该请求回答的最新 user（或 steer）消息内容。
    UserPrompt string `json:"user_prompt,omitempty"`
    // Model 是该请求实际发送的模型名（如 "deepseek-v4-flash"）。
    Model string `json:"model,omitempty"`
    // Provider 是支撑该模型的 config provider 名（未知时为空）。
    Provider string `json:"provider,omitempty"`
    // Thinking 是该请求实际使用的思考模式："none"（关闭）、reasoning effort
    // （"low"/"high"/...）、或 ""（provider 默认）。
    Thinking string    `json:"thinking,omitempty"`
    Tools    []APITool `json:"tools,omitempty"`
}
```

`session.Message` 增加 `Iteration` 字段（产生该消息的 API 调用序号），
one-off sidecar 文件（`oneoff/<kind>-<ts>.jsonl`）以 `api_request` 类型行记录同一结构
（`oneoffAPIRequestLine{Type:"api_request"; session.APIRequest}`，JSON 平铺）。

## 记录链路

`runLoop`（agent/agent_loop.go）是四种入口（TUI / ACP / channel / `-p`）共享的核心循环，
在每次 `Provider.CreateChatStream` 之前调用 `a.recordAPIRequest(ctx, rs, llmTools)`：

- system prompt 取自 `rs.Messages[0]`（role=system），即模型实际收到的那条。
- user prompt 取自 `rs.Messages` 最后一条 user/steer 消息（`extractUserPrompt`）。
- iteration 取自 `rs.APICalls`（本次调用序号）。
- model / provider 取自实际调用的 `in.Provider`（`Model()` / `ProviderName()`）。
- thinking 取自实际使用的 `opts`（`requestThinking`：Thinking=false→"none"，
  ThinkingEffort→effort，默认→""）。
- tools 取自本次调用实际构建的 `llmTools`（含模式过滤后的子集）。
- **主会话**：写入 session 的 `api_requests.jsonl`。
- **one-off / ambient**（`rs.SkipSessionWrites`）：写入 one-off sidecar 的 `api_request` 行。
- **子 agent**：走 `RunOneOffStream` 且无 one-off recorder（OneoffRec 为 nil），不记录。
- **best-effort**：写入失败仅 Warn 日志，绝不阻断 agent 循环。

工具执行（`executeToolCallsParallel` / `executeToolCallsSequential`）与
`recordAssistantTurn` 记录的 tool_call / tool_result / thinking / assistant 消息
均填 `Iteration: rs.APICalls`，实现"工具 ↔ 请求"双向关联。

实现位于 `agent/api_recorder.go`；`agent.SessionManager` 接口与
`session.Store` / `session.Manager` 各自新增 `AppendAPIRequest` / `LoadAPIRequests`。

## 展示（按 API 请求分组）

`render.BuildReportDataFromMessagesWithRequests(s, msgs, subagents, apiReqs)`
（旧 `BuildReportDataFromMessages` 委托之，apiReqs=nil）把事件流**按 API 请求（iteration）
分组**，每个请求是一个可视化单元：

- **请求组**（`RequestGroupView`）：一次 API 调用的完整视图 = 该请求的
  system prompt + tools + 它产生的所有事件（thinking / text / tool_call / tool_result）。
- **绑定 user prompt**：组标题显示触发它的用户输入（`«用户消息截断»`）。
- **工具分组**：同一次请求发出的多个 tool call 天然落在同一组内展示。
- **顺序保持**：iteration-0 的事件（user / reminder / steer）平铺在原位，
  请求组插在它们第一个事件的位置——steer / continuation 消息与它触发的下一组相邻。
- 组标题带 `🔁 API 请求 #N` + "与上一条相同"折叠标记；system prompt 与 tools
  为组内可折叠子区；整个组可折叠。
- **旧数据 fallback**：历史会话的消息没有 iteration 标记 → 无法分组，
  `HasRequestGroups=false` 时在 turn 顶部渲染扁平请求列表（v2 样式），不丢信息。
- turn 内无请求（或该会话没记录）则不渲染，报告不受影响。

HTML 报告：

- 请求组：`🔁 API 请求 #N · «user prompt»` 标题 + 模型/thinking meta
  （`model · thinking high`）+ `🔧 M` 徽标 + 组内事件流。
- 组内 `🧠 System Prompt` 与 `🔧 Tools（M）` 子区（工具含 name + description +
  格式化 schema），与上一条相同的标"同上"。
- 侧边栏统计 `API 调用` 数。

四个 `/transcript` 入口（TUI `tui/commands_misc.go`、CLI `main_channels.go`、
ACP `agent/acp/commands.go`、channel `channel/manager/commands_misc.go`）
均加载 `LoadAPIRequests` 并传入。

## 兼容性

- 旧会话的 `api_requests.jsonl`（无 Iteration/UserPrompt）照常渲染：归属按时间戳，
  请求块不显示 `#N`；tool_call 无 `⚙#N` 标记。
- `messages.jsonl` 与 LLM resume 逻辑不受影响（新字段可选、新文件独立）。
- one-off sidecar 的行序：`meta` → 消息行 → `api_request` 行交错出现。
  渲染端对未知类型走默认分支，向后兼容。

## 文件清单

| 文件 | 操作 | 说明 |
|------|------|------|
| `session/session.go` | 修改 | `APIRequest` / `APITool` 类型 |
| `session/store.go` | 修改 | Store 接口 + FileStore `AppendAPIRequest` / `LoadAPIRequests`（api_requests.jsonl） |
| `session/manager.go` | 修改 | Manager 对应方法 |
| `agent/api_recorder.go` | 新建 | `recordAPIRequest` / `extractSystemPrompt` / `toAPITools` |
| `agent/oneoff_recorder.go` | 修改 | `oneoffAPIRequestLine` + `recordAPIRequest`（sidecar `api_request` 行） |
| `agent/agent_loop.go` | 修改 | runLoop 每次 CreateChatStream 前记录 |
| `agent/session_manager.go` | 修改 | SessionManager 接口加 `AppendAPIRequest` / `LoadAPIRequests` |
| `agent/transcript/render/html.go` | 修改 | TurnView/APIRequestView、归属逻辑、新构造函数、APICallCount |
| `agent/transcript/render/templates/report.html` | 修改 | 请求卡片、徽标、CSS、JS |
| 4 个 `/transcript` 入口 | 修改 | 加载并传入 apiReqs |
