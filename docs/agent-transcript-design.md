# Agent Transcript 机制设计文档

## 问题

当前 `tachi` 的 agent 执行追踪有两个不足：

1. **Subagent 执行过程不可见**：`SubagentExecutor.RunSubagent` 内部创建子 agent、执行多轮 tool call、产生 thinking/text 的全过程对父 agent 不可见。子 agent 只返回最终文本。
2. **Session 消息不能表达嵌套结构**：`session.Message` 是 flat 的 JSONL，只适合 LLM 会话重放（resume），无法表达 subagent 的树状执行结构。

## 解决方案

`agent/transcript` 包提供了独立的树状执行追踪数据结构，与 `session.Message`（flat JSONL）互补。

- **session.Message** → LLM 会话重放（resume）
- **transcript.Event** → 调试、分析、TUI 可视化

## 数据模型

### Event —— 原子事件

```go
type EventType string
const (
    EventThinking   EventType = "thinking"
    EventText       EventType = "text"
    EventToolCall   EventType = "tool_call"
    EventToolResult EventType = "tool_result"
)

type Event struct {
    Type      EventType `json:"type"`
    Timestamp time.Time `json:"ts"`
    Name      string    `json:"name,omitempty"`   // for tool_call/tool_result
    Content   string    `json:"content,omitempty"`
    Args      string    `json:"args,omitempty"`    // tool_call JSON
    IsError   bool      `json:"is_error,omitempty"`
    Children  []Event   `json:"children,omitempty"` // SubAgent sub-transcript
}
```

### Turn —— 一轮 LLM 调用

```go
type Turn struct {
    ID     int     `json:"id"`
    Events []Event `json:"events"`
}
```

### Transcript —— 完整执行记录

```go
type Transcript struct {
    SessionID string `json:"session_id,omitempty"`
    Turns     []Turn `json:"turns"`
}
```

## 核心组件

### Builder —— 流式构建器

```
agent/transcript/builder.go
```

- `StartTurn()` — 自动完成上一轮 turn，开始新一轮
- `RecordThinking(content)` — 记录 thinking block
- `RecordText(content)` — 记录聚合后的 text（per-turn，非 per-token）
- `RecordToolCall(name, args) → *ToolCallRecorder` — 记录工具调用
- `Build() → *Transcript` — 返回不可变快照

### ToolCallRecorder

```
rec := builder.RecordToolCall("ReadFile", `{"path":"foo"}`)
// ... 对于 SubAgent:
sub := rec.SubBuilder()
// ... 子在 sub 中记录事件 ...
rec.RecordToolResult("content", false)
```

### Context 传递

```go
// agent/transcript/context.go
ctx = transcript.WithBuilder(ctx, subBuilder)
subBuilder := transcript.BuilderFromContext(ctx)
```

## 集成架构

```
RunConversationStream                          (agent/agent.go)
  ├── 创建 transcript.NewBuilder()
  ├── runAgentLoop
  │   ├── StartTurn()                          ← 每轮 API 调用
  │   ├── consumeStream()
  │   │   ├── RecordThinking(tb.Thinking)      ← thinking blocks
  │   │   └── RecordText(acc.text.String())    ← 聚合后的 text
  │   └── executeToolCalls
  │       ├── RecordToolCall(name, args)       ← 所有工具调用
  │       ├── ctx = WithBuilder(ctx, sub)      ← SubAgent 路径
  │       ├── RecordToolResult(content, isErr)
  │       └── [SubagentExecutor]
  │           └── child.SetTranscriptBuilder(sub)
  │               └── child.runAgentLoop       ← 子 agent 写入 sub builder
  └── persistTranscript()                      ← 写入 transcript.json
```

## 持久化

```
~/.tachi/session/<session-id>/
├── meta.json
├── messages.jsonl      # LLM 会话消息（现有）
└── transcript.json     # 执行 transcript（新增）
```

- `session/manager.go` — `SaveTranscript()` / `LoadTranscript()`
- `session/store.go` — `FileStore.SaveTranscript()` / `LoadTranscript()`
- 跟随 session 目录一起被清理（`DeleteSession`）

## TUI 集成

新增 `AgentEvent` 类型：

- `AgentEventSubagentStart` — SubAgent 开始时发出，TUI 将 tool_call 行标记为 `⊡` 图标
- `AgentEventSubagentDone` — SubAgent 完成时发出

ChatView 渲染改动：
- 普通 tool call：`~ ReadFile(path.go)`
- SubAgent（运行中）：`⊡ SubAgent ⊞(prompt)`
- SubAgent（完成）：`v SubAgent ⊞(prompt)`

## 文件清单

| 文件 | 操作 | 说明 |
|------|------|------|
| `agent/transcript/transcript.go` | 新建 | Event, Turn, Transcript 类型 + 序列化 |
| `agent/transcript/transcript_test.go` | 新建 | 单元测试 |
| `agent/transcript/builder.go` | 新建 | Builder + ToolCallRecorder |
| `agent/transcript/builder_test.go` | 新建 | Builder 单元测试 |
| `agent/transcript/context.go` | 新建 | context key/value 传递 builder |
| `agent/transcript/context_test.go` | 新建 | Context 单元测试 |
| `agent/agent.go` | 修改 | AIAgent.transcriptBuilder, runAgentLoop 集成, RunConversationStream 初始化+持久化, 新增 AgentEvent 常量 |
| `agent/tool_executor.go` | 修改 | 所有工具执行路径集成 transcript 记录; SubAgent context 注入 |
| `agent/subagent.go` | 修改 | RunSubagent 从 context 获取并传播 sub-builder |
| `session/store.go` | 修改 | Store 接口 + FileStore 添加 SaveTranscript/LoadTranscript |
| `session/manager.go` | 修改 | Manager 添加 SaveTranscript/LoadTranscript |
| `tui/model.go` | 修改 | toolCallDisplay 添加 IsSubagent, 处理 SubagentStart/SubagentDone 事件 |
| `tui/chatview.go` | 修改 | MarkSubagent, renderToolCall 添加 Subagent 指示符 |
| `docs/agent-transcript-design.md` | 新建 | 本文档 |

## 向后兼容

- `transcriptBuilder` 为 nil 时不记录（默认行为不变）
- Channel 模式无感知变化
- `messages.jsonl` 保持原样