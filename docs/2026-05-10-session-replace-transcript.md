# Session 取代 Transcript 方案

## 背景

当前 tachi 有两套并行的执行记录系统：

1. **Session (`messages.jsonl`)** — 扁平 JSONL，用于 LLM 会话重放（resume）
2. **Transcript (`transcript.jsonl`)** — 树状 JSONL，用于调试分析和 HTML report

两者在主 agent 层高度重叠——同一个 thinking、同一段 text、同一个 tool_call 被各写一次。Transcript 唯一比 Session 多提供的价值是 **SubAgent 内部执行过程的嵌套记录**（`Children []Event`）。

### 当前存储布局

```
~/.tachi/session/<session-id>/
├── meta.json
├── messages.jsonl      # 扁平，LLM resume
└── transcript.jsonl    # 树状，含 SubAgent Children
```

## 目标

- **删除 `agent/transcript/` 整个包**，消除双写
- **主 `messages.jsonl` 保持扁平不变**，不影响 resume 路径
- **SubAgent 内部执行过程** 写入独立文件 `subagent/<shortID>.jsonl`
- **HTML Report** 从 Session + SubAgent 文件重建，保持现有展示效果

## 新存储布局

```
~/.tachi/session/<session-id>/
├── meta.json
├── messages.jsonl           # 主 agent 扁平对话（不变）
└── subagent/
    ├── a1b2c3d4.jsonl      # SubAgent 1 完整执行记录
    └── e5f6g7h8.jsonl      # SubAgent 2 完整执行记录
```

每个 `subagent/<shortID>.jsonl` 就是一个标准 session 消息文件，复用 `session.Message` 类型：

```jsonl
{"type":"user","content":"Explore the codebase structure...","timestamp":"..."}
{"type":"thinking","content":"Let me start by...","signature":"...","timestamp":"..."}
{"type":"assistant","content":"I'll search for...","timestamp":"..."}
{"type":"tool_call","name":"Glob","args":"{\"pattern\":\"**/*.go\"}","tool_call_id":"toolu_xxx","timestamp":"..."}
{"type":"tool_result","name":"Glob","result":"Found 42 files...","tool_call_id":"toolu_xxx","timestamp":"..."}
{"type":"thinking","content":"Good, now let me...","signature":"...","timestamp":"..."}
{"type":"assistant","content":"Here's a summary...","timestamp":"..."}
```

## 改造清单

### 1. `session.Message` 加 `SubagentID` 字段

```diff
// session/session.go
type Message struct {
    Type       MessageType `json:"type"`
    Content    string      `json:"content,omitempty"`
    Name       string      `json:"name,omitempty"`
    Signature  string      `json:"signature,omitempty"`
    Args       any         `json:"args,omitempty"`
    Result     string      `json:"result,omitempty"`
    IsError    bool        `json:"is_error,omitempty"`
    Diff       string      `json:"diff,omitempty"`
    ToolCallID string      `json:"tool_call_id,omitempty"`
+   SubagentID string      `json:"subagent_id,omitempty"`
    Timestamp  time.Time   `json:"timestamp"`
}
```

`omitempty` 确保非 SubAgent 消息不受影响。仅在主 session 的 SubAgent `tool_result` 消息中设置此字段，用于关联 `subagent/<SubagentID>.jsonl` 文件。

### 2. `tools.ToolResult` 加 `SubagentID` 字段

```diff
// agent/tools/tool.go
type ToolResult struct {
    Status      ToolResultStatus
    Output      string
    Err         error
    Name        string
    Args        string
    Diff        string
    Questions   []Question
+   SubagentID  string
}
```

`SubagentExecutor.RunSubagent` 在返回前设置 `tr.SubagentID = shortID`。

### 3. SubAgent 执行过程写入 `subagent/<shortID>.jsonl`

#### 3.1 新增 `agent/subagent_recorder.go`

提取一个轻量的 `SubagentRecorder`，负责在子 agent 运行时将消息逐条 append 到文件：

```go
// subagent_recorder.go
type subagentRecorder struct {
    file *os.File
}

func newSubagentRecorder(sessionID, shortID string) (*subagentRecorder, error) {
    dir := filepath.Join(config.SessionDir(), sessionID, "subagent")
    os.MkdirAll(dir, 0700)
    f, err := os.OpenFile(filepath.Join(dir, shortID+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
    if err != nil {
        return nil, err
    }
    return &subagentRecorder{file: f}, nil
}

func (r *subagentRecorder) record(msg *session.Message) error {
    data, err := json.Marshal(msg)
    if err != nil {
        return err
    }
    _, err = r.file.Write(append(data, '\n'))
    return err
}

func (r *subagentRecorder) close() error {
    return r.file.Close()
}
```

#### 3.2 改 `runChildAgent`

在子 agent 流式消费循环中同步写 JSONL：

```go
// agent/subagent.go — runChildAgent
recorder, err := newSubagentRecorder(parentSessionID, shortID)
if err != nil {
    childLogger.Log("failed to create subagent recorder: %v", err)
}
defer recorder.close()

// 写第一条 user message
recorder.record(&session.Message{
    Type:      session.MessageTypeUser,
    Content:   args.Prompt,
    Timestamp: time.Now(),
})

// 消费 AgentEvent，旁路写文件
for event := range ch {
    switch event.Type {
    case AgentEventThinkingDelta:
        // 聚合后写
    case AgentEventTextDelta:
        sb.WriteString(event.TextDelta)
    case AgentEventToolCallArgs:
        recorder.record(&session.Message{
            Type:       session.MessageTypeToolCall,
            Name:       event.ToolName,
            Args:       event.ToolArgs,
            ToolCallID: event.ToolID,
            Timestamp:  time.Now(),
        })
    case AgentEventToolResult:
        recorder.record(&session.Message{
            Type:       session.MessageTypeToolResult,
            Name:       event.ToolName,
            Result:     event.ToolResult,
            IsError:    event.ToolIsError,
            ToolCallID: event.ToolID,
            Timestamp:  time.Now(),
        })
        iterCount++
    // ... 其他事件
    }
}
```

**关于 thinking 和 text 的聚合**：子 agent 的 `RunOneOffStream` 目前只累加 text 到 `sb`，没有保留 thinking。需要改为在消费 stream 时也跟踪 thinking blocks，聚合后写入。这与主 agent 的 `handleFinishReason` 逻辑类似——每轮 API 调用结束后，把已聚合的 thinking + text 写入。

**实现策略**：在 `runChildAgent` 的事件循环中增加 thinking 聚合逻辑。当前 `runChildAgent` 只消费 `AgentEventTextDelta` 和 `AgentEventToolResult`。需要扩展为也处理 `AgentEventThinkingDelta`，并在每轮 API 调用边界（即出现 tool calls 或 turn complete 时）将聚合的 thinking + text flush 到 recorder。

**关键**：`RunOneOffStream` 内部调用了 `runAgentLoop`，后者已经 emit `AgentEventThinkingDelta`。`runChildAgent` 的消费循环需要新增对这些事件的响应。

### 4. `tool_executor.go` 记录 SubagentID 到主 session

```diff
// agent/tool_executor.go — executeToolCallsSequential & executeToolCallsParallel
a.recordSession(&session.Message{
    Type:       session.MessageTypeToolResult,
    Name:       tc.Function.Name,
    Result:     toolMsg.Content,
    IsError:    toolMsg.IsError,
    ToolCallID: tc.ID,
+   SubagentID: tr.SubagentID,
})
```

### 5. 删除 Transcript 整个体系

| 操作 | 位置 |
|------|------|
| 删除 `agent/transcript/` 包 | transcript.go, builder.go, context.go, context_test.go, builder_test.go, transcript_test.go |
| 删除 `agent/transcript/render/` 包 | html.go, templates/report.html |
| 删除 `AIAgent.transcriptBuilder` 字段 | `agent/agent.go` 结构体 |
| 删除 `recordTranscriptUser/Thinking/Text/ToolCall` 方法 | `agent/agent.go` |
| 删除 `flushTranscriptTurns` / `persistTranscript` 方法 + 调用 | `agent/agent.go` |
| 删除 `runAgentLoop` 中的 `StartTurn` / `RecordThinking` / `RecordText` 调用 | `agent/agent.go` |
| 删除 `executeToolCalls*` 中所有 `tcRecorder` / `recordTranscriptToolCall` 相关代码 | `agent/tool_executor.go` |
| 删除 `transcript.WithBuilder` context 注入 | `agent/tool_executor.go` |
| 删除 `runChildAgent` 中 `transcript.BuilderFromContext` + `SetTranscriptBuilder` | `agent/subagent.go` |
| 删除 `Store` 接口的 `AppendTranscriptTurn` / `LoadTranscript` / `transcriptPath` | `session/store.go` |
| 删除 `Manager` 的 `AppendTranscriptTurn` / `LoadTranscript` | `session/manager.go` |
| 更新设计文档 | `docs/2026-05-09-agent-transcript-design.md` 标注已废弃 |

### 6. HTML Report 重写

#### 6.1 新增 `agent/report/` 包

将原来 `agent/transcript/render/` 的逻辑迁移到 `agent/report/`，数据源切换为 Session + SubAgent 文件。

```go
// agent/report/report.go
func BuildReportData(
    s *session.Session,
    msgs []session.Message,                // messages.jsonl
    subagents map[string][]session.Message, // shortID → 子 agent 消息
) *ReportData
```

`ReportData` 结构调整：

```go
type ReportData struct {
    Session    *SessionView
    Turns      []TurnView
    Stats      StatsView
}

type TurnView struct {
    ID       int
    Events   []EventView      // 从 session.Message 转换
}

type EventView struct {
    Type        string
    Timestamp   string
    Name        string
    Content     string
    ArgsJSON    string
    IsError     bool
    HasChildren bool
    Children    []EventView   // 从 subagents[SubagentID] 构建
    Icon        string
    CSSClass    string
}
```

#### 6.2 Turn 分组逻辑

`messages.jsonl` 是扁平的，需要根据消息序列重建 Turn 分组：

- **user message** → 新 Turn 开始
- **thinking + assistant + tool_call** → 同一 Turn 内
- **tool_result** → 当前 Turn 的结束边界（下一轮 API 调用开始前）

分组算法：
```
遍历 messages:
  if type == "user":
    flush 当前 turn → start new turn (user 作为 turn 的第一个 event)
  elif type in {thinking, assistant, tool_call}:
    追加到当前 turn
  elif type == "tool_result":
    追加到当前 turn
    # tool_result 不结束 turn，因为可能有多个 tool_result 属于同一轮
    # turn 的真正边界是下一个 user message 或流结束
```

实际上更简单：由于 `messages.jsonl` 中 user message 总是标记新轮次，直接把两个 user message 之间的事件归为一个 Turn 即可。

#### 6.3 SubAgent 内联展示

遍历到 `SubagentID != ""` 的 tool_result 时，从 `subagents` map 取对应子 agent 的 `[]Message`，递归构建 `EventView.Children`。递归结构与原来 transcript 的 `Children []Event` 一致，模板无需大改。

#### 6.4 模板

`templates/report.html` 基本可复用，`EventView` 结构调整后模板中访问 `.Children` 的方式不变。

### 7. 测试

| 测试范围 | 内容 |
|----------|------|
| `SubagentRecorder` 单元测试 | 创建文件、写入、关闭、文件内容验证 |
| `runChildAgent` 集成测试 | 子 agent 执行后 `subagent/<id>.jsonl` 包含正确消息 |
| `tool_executor` 测试 | SubagentID 正确传递到 session.Message |
| `BuildReportData` 测试 | 从 session messages + subagent 文件正确构建 ReportData |
| Resume 回归测试 | messages.jsonl 不变，resume 路径不受影响 |
| Session 清理测试 | DeleteSession 一并删除 subagent/ 目录 |

## 不变部分

- `messages.jsonl` 格式和内容完全不变
- `ConvertSessionToLLMMessages`（resume 路径）不变
- TUI 的 `AgentEventSubagentStart` / `AgentEventSubagentDone` 事件不变
- `tools.SubagentTool` / `SubagentArgs` / `SubagentRunner` 接口不变
- Session 清理逻辑不变 — `DeleteSession` 通过 `os.RemoveAll(sessionDir)` 自然删除 `subagent/` 目录
- Channel 模式不受影响

## 收益

| 维度 | 效果 |
|------|------|
| 双写消除 | thinking/text/tool_call 不再同时写入 session 和 transcript |
| 主 JSONL 纯净 | SubAgent 内部过程不进 messages.jsonl，resume 不受任何影响 |
| SubAgent 可查 | `subagent/<id>.jsonl` 是标准 JSONL，可用 `cat`/`jq` 等工具直接分析 |
| 存储分离 | 每个 SubAgent 独立文件，不像 transcript.jsonl 所有嵌套摊在一个巨大树中 |
| Session 清理自然 | 删 session 目录即删所有 subagent 文件，无需额外逻辑 |

## 文件清单

| 文件 | 操作 | 说明 |
|------|------|------|
| `session/session.go` | 修改 | Message 加 SubagentID |
| `agent/tools/tool.go` | 修改 | ToolResult 加 SubagentID |
| `agent/subagent.go` | 修改 | runChildAgent 加 recorder 写入逻辑 + consuming stream 扩展 |
| `agent/subagent_recorder.go` | 新建 | SubagentRecorder 实现 |
| `agent/subagent_recorder_test.go` | 新建 | 单元测试 |
| `agent/tool_executor.go` | 修改 | tool_result recordSession 传 SubagentID |
| `agent/agent.go` | 修改 | 删除 transcriptBuilder 相关字段、方法、调用 |
| `agent/transcript/` | 删除 | 整个包 |
| `agent/report/report.go` | 新建 | HTML Report 构建（替代 agent/transcript/render/） |
| `agent/report/templates/report.html` | 迁移 | 从 agent/transcript/render/templates/ 迁移，微调 |
| `session/store.go` | 修改 | 删除 AppendTranscriptTurn/LoadTranscript/transcriptPath |
| `session/manager.go` | 修改 | 删除 AppendTranscriptTurn/LoadTranscript |
| `docs/2026-05-09-agent-transcript-design.md` | 修改 | 标注已废弃 |

## 向后兼容

- 旧 session 目录中的 `transcript.jsonl` 成为孤儿文件，不再被读取
- 旧 session 的 resume 不受影响（`messages.jsonl` 未变）
- 可选：提供一次性迁移脚本，将旧 `transcript.jsonl` 中的 SubAgent Children 拆分为 `subagent/<id>.jsonl`
