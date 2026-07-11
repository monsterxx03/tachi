# ACP 增强方案：提升 Zed 集成体验

> 版本: 1.1 | 日期: 2026-07-11 | 状态: 部分实施

## 一、概述

当前的 ACP 实现已覆盖了协议的核心能力——会话生命周期、模型切换、权限请求、工具流 streaming、Agent 跟跳、MCP 集成、Slash 命令等。本文档梳理了进一步提升 Zed 编辑器集成的体验的若干方向，涵盖从低成本快速见效的小改进到高投入高回报的架构级优化。

**参考**：
- ACP 协议规范: https://agentclientprotocol.com/protocol/overview
- Zed ACP 客户端: https://zed.dev/acp/editor/zed
- 现有 ACP 实现: `agent/acp/`
- ACP Elicitation RFD: https://agentclientprotocol.com/rfds/elicitation.md
- ACP 终端能力: https://agentclientprotocol.com/protocol/v1/terminals.md
- ACP Plan: https://agentclientprotocol.com/protocol/v1/agent-plan.md
- ACP Tool Calls (Diff): https://agentclientprotocol.com/protocol/v1/tool-calls.md

---

## 二、能力总览

### 2.1 当前状态

| 能力 | 状态 | 备注 |
|------|------|------|
| 会话生命周期 (New/Load/Resume/List/Close) | ✅ | 完整实现 |
| 模型切换 (Config Options) | ✅ | select 类型 |
| 模式切换 (auto/chat) | ✅ | 通过 Config Options |
| Permission 请求 | ✅ | External 模式 + allow_once/allow_all/reject |
| 工具调用 streaming (kind/title/status/rawInput) | ✅ | 含 Agent 跟跳 (locations) |
| Thinking streaming | ✅ | 通过 thinking delta |
| 文本 delta streaming | ✅ | 通过 text delta |
| 历史重放 (session/load) | ✅ | 完整 replay |
| Editor MCP 连接 | ✅ | 含 conflict policy |
| Slash 命令 | ✅ | 10 static + 动态 skill |
| Token 用量 (UsageUpdate) | ✅ | TurnComplete 时发送 |
| 会话标题更新 | ✅ | SessionInfoUpdate |
| Auto-compact 通知 | ✅ | 文本通知 |
| 文件 diff 显示 (permission 中) | ✅ | ToolDiffContent |
| Cancel | ✅ | context cancel |
| Session Delete | ✅ | 2026-07-11 实现 |
| Agent Plan 流 | ❌ | 未实现 |
| Diff 内嵌到 tool result | ✅ | 2026-07-11 实现：EditFile/WriteFile 完成时带 diff content |
| 终端委派 (ACP terminal) | ❌ | Bash 工具本地执行 |
| 文件系统委派 (ReadFile via ACP) | ✅ | 2026-07-11 实现：Write/Edit/Read 全部走 ACP FS |
| Steer 支持 | ⚠️ 部分 | Cancel 可中断, 但无 steer 通道 |
| Elicitation (交互式提问) | ❌ | AskUser 被注销 |
| Boolean Config Options | ❌ | 不支持 |
| 启发式进度更新 | ❌ | 纯文本 |
| Usage 频率/丰富度 | ⚠️ 基础 | 仅在 TurnComplete 发一次 |

### 2.2 增强方向一览

| # | 方向 | 工作量 | 体验提升 | 协议成熟度 | 状态 |
|---|------|--------|----------|-----------|------|
| 1 | Agent Plan 流 | 中 | ⭐⭐⭐⭐⭐ | Stable | ❌ |
| 2 | Diff 内嵌到 Tool Result | 低 | ⭐⭐⭐⭐ | Stable | ✅ 已实现 |
| 3 | ReadFile 走 ACP FS | 低 | ⭐⭐⭐ | Stable | ✅ 已实现 |
| 4 | Session Delete | 低 | ⭐⭐ | Stable | ✅ 已实现 |
| 5 | 终端委派 | 大 | ⭐⭐⭐⭐⭐ | Stable | ❌ |
| 6 | Steer 增强 | 中 | ⭐⭐⭐⭐ | Stable | ❌ |
| 7 | Slash 命令完善 | 低-中 | ⭐⭐⭐ | N/A | ❌ |
| 8 | Elicitation 准备 | 中 | ⭐⭐⭐（远期） | RFD (Preview) | ❌ |
| 9 | Usage 增强 | 低 | ⭐⭐ | RFD | ❌ |
| 10 | Boolean Config Options | 低 | ⭐⭐ | Stable | ❌ |

---

## 三、详细设计

### 3.1 Agent Plan 流（Tier 1）

**问题**：当 LLM 开始响应时，Zed 用户只能看到零散流出的工具调用（ReadFile → Grep → EditFile...），缺少一个"大纲"来理解 Agent 的整体执行策略。相比之下，Zed 内置 Agent 可以展示计划步骤。

**ACP 规范**：协议已定义 `plan` 类型的 session update，包含 `PlanEntry[]` 数组，每个 entry 有 `content`、`priority`（high/medium/low）、`status`（pending/in_progress/completed）。

**方案**：

在 `streamToACP` 中，检测到首批工具调用时，从 thinking delta 或工具调用序列中提取计划，生成 plan update。后续随执行进度更新各 entry 的 status。

```go
// stream.go — 新增 plan 追踪
type planTracker struct {
    entries []acp.PlanEntry
    sent    bool
}

// 在每次 AgentEventTurnComplete 时重置
// 在收到首批 ThinkingDelta 或 ToolCallStart 时生成 plan

// 提取 plan 的策略（从易到难）：
// 1. 简单版：只发首批 tool calls 的执行计划
//    "The agent will: Read 3 files, Grep for pattern, Edit 1 file"
// 2. 进阶版：从 thinking delta 中提取更丰富的步骤描述
```

**实现路径**：

**Phase 1（简单版）**：在首个 `AgentEventToolCallStart` 或 `AgentEventThinkingDelta` 到来时，收集前 3-5 个工具调用的 title，生成 plan entries。

**Phase 2（进阶版）**：从 thinking delta 文本中解析出"步骤描述"。利用 LLM thinking 的结构化特性（通常以 `1.`, `2.`, `-` 等格式列出计划）。

```go
// 伪代码 — 在 streamToACP 中
type acpStreamState struct {
    plan          *planTracker
    bufferedCalls []agent.AgentEvent  // 缓冲首批工具调用用于构建 plan
}

func (s *acpStreamState) buildPlanFromThinking(delta string) {
    // 从 thinking delta 中提取结构化步骤
    lines := strings.Split(delta, "\n")
    var entries []acp.PlanEntry
    for _, line := range lines {
        line = strings.TrimSpace(line)
        if isStepLine(line) {
            entries = append(entries, acp.PlanEntry{
                Content:  cleanStepText(line),
                Priority: acp.PlanPriorityMedium,
                Status:   acp.PlanStatusPending,
            })
        }
    }
    if len(entries) > 0 {
        s.plan.entries = entries
    }
}

func (s *acpStreamState) flushPlan(ctx, conn, sessionID) {
    if s.plan.sent || len(s.plan.entries) == 0 {
        return
    }
    conn.SessionUpdate(ctx, acp.SessionNotification{
        SessionId: sessionID,
        Update: acp.SessionUpdate{
            PlanUpdate: &acp.SessionPlanUpdate{
                Entries: s.plan.entries,
            },
        },
    })
    s.plan.sent = true
}
```

**文件变更**：
- `agent/acp/stream.go` — 新增 `planTracker` + plan 发送逻辑
- 可能在 `streamToACP` 中需要重构为 `acpStreamState` 结构体（当前用局部变量 `toolArgs`/`pendingStarts`，可以统一管理）

---

### 3.2 Diff 内嵌到 Tool Result（Tier 1）✅ 已实现（2026-07-11）

**问题**：当 `EditFile` 执行完成时，tool result 以纯文本形式返回。Zed 只把它当作文本展示，无法渲染成漂亮的 diff 视图（绿色新增、红色删除）。

**现状**：`permission.go` 已经在 `RequestPermission` 中发送 `acp.ToolDiffContent`（path + oldText + newText），但 tool result 阶段没有。

**实现**：在 `streamToACP` 的 `AgentEventToolResult` 分支中，对 EditFile/WriteFile 工具，从已缓冲的工具参数中解析 `path`/`old_string`/`new_string`，附加 diff content block。

```go
// stream.go — AgentEventToolResult 分支
case agent.AgentEventToolResult:
    updateOpts := []acp.ToolCallUpdateOpt{
        acp.WithUpdateStatus(status),
    }
    // 对 EditFile/WriteFile 附加 diff content
    if event.ToolName == tools.ToolNameEdit || event.ToolName == tools.ToolNameWrite {
        if diffContent := buildDiffFromArgs(event.ToolName, toolArgs[event.ToolID]); diffContent != nil {
            updateOpts = append(updateOpts, acp.WithUpdateContent([]acp.ToolCallContent{*diffContent}))
        }
    }
    update := acp.UpdateToolCall(acp.ToolCallId(event.ToolID), updateOpts...)
    conn.SessionUpdate(ctx, ..., update)
}

// 新增 helper: buildDiffFromArgs
func buildDiffFromArgs(toolName string, argsJSON string) *acp.ToolCallContent {
    switch toolName {
    case tools.ToolNameEdit:
        // 提取 path, old_string, new_string → 含旧/新文本的 diff
    case tools.ToolNameWrite:
        // 提取 path, content → 仅新文本的 diff（旧文本留空表示新建）
    }
}
```

**文件变更**：
- `agent/acp/stream.go` — `AgentEventToolResult` 分支增加 diff content

---

### 3.3 ReadFile 走 ACP FS（Tier 1）✅ 已实现（2026-07-11）

**问题**：ACP 模式下，`WriteFile` 和 `EditFile` 已经可以通过 `conn.WriteTextFile` / `conn.ReadTextFile` 路由到 Zed 的文件系统。但 `ReadFile` 仍然通过本地文件系统读取，Zed 无法获知 Agent 正在读哪个文件。

**现状**：
- `agent.go` 中 `aiAgent.SetACPFileMode()` 设置了 ACP 文件模式
- `agent/tools/write.go` 检查 `acpctx.Conn(ctx)` → 有则走 `conn.WriteTextFile`
- `agent/tools/edit.go` 有 `SetACPMode(bool)` → 路由到 `conn.WriteTextFile`
- `agent/tools/read.go` **之前没有** ACP 模式检测 → 总是本地读取

**实现**：在 `ReadFile` 工具中增加了 ACP 模式检测。如果 context 中有 `acpctx.Conn`，优先通过 `conn.ReadTextFile` 读取。

```go
// agent/tools/read.go — 修改 ExecuteContext
func (t *ReadFileTool) ExecuteContext(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
    // ... 参数解析 ...

    // 新增：ACP 模式 — 通过客户端读取
    if conn := acpctx.Conn(ctx); conn != nil {
        content, err := conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
            Path: filePath,
            // 可选: Offset, Limit
        })
        if err != nil {
            return tools.ToolResult{Status: tools.ToolResultError, Err: err}, nil
        }
        // 处理 returned content...
        return tools.ToolResult{
            Status: tools.ToolResultSuccess,
            Output: content.Text,
        }, nil
    }

    // 原有逻辑：本地读取
    // ...
}
```

**文件变更**：
- `agent/tools/read.go` — `ExecuteContext` 增加 ACP 模式分支
- `agent/acp/acpctx/acpctx.go` — 可能不需要变更（已有 `Conn(ctx)` 和 `WithConn`）

---

### 3.4 Session Delete（Tier 1）✅ 已实现（2026-07-11）

**问题**：ACP 协议定义了 `session/delete` 方法，但 Tachi 未实现。Zed 用户无法从编辑器侧清理不需要的会话。

**实现**：
1. `Initialize` 响应中声明 `Delete: &acp.SessionDeleteCapabilities{}`
2. 新增 `UnstableDeleteSession` 方法（SDK 通过 optional interface 发现）
3. 内存 session 关闭 + 磁盘 JSONL 清理

```go
// agent/acp/agent.go — Initialize 中声明能力
SessionCapabilities: acp.SessionCapabilities{
    List:   &acp.SessionListCapabilities{},
    Close:  &acp.SessionCloseCapabilities{},
    Resume: &acp.SessionResumeCapabilities{},
    Delete: &acp.SessionDeleteCapabilities{},  // 新增
},

// agent/acp/agent.go — 新增方法
func (t *TachiAgent) UnstableDeleteSession(_ context.Context, req acp.UnstableDeleteSessionRequest) (acp.UnstableDeleteSessionResponse, error) {
    sessionID := string(req.SessionId)
    // 1. 关闭内存 session
    if sess, ok := t.sessions.Get(sessionID); ok {
        sess.Close()
        t.sessions.Delete(sessionID)
    }
    // 2. 删除磁盘文件
    sm, err := session.NewManager()
    if err != nil { ... }
    if err := sm.Delete(sessionID); err != nil { ... }
    return acp.UnstableDeleteSessionResponse{}, nil
}
```

---

### 3.5 终端委派（Tier 2）

**问题**：Tachi 的 `Bash` 工具在本地运行命令。Zed 用户看不到命令执行过程，只能看到最终结果。而 Zed 内置 Agent 可以在终端面板中实时显示命令输出。

**ACP 协议**：已完整定义终端 API（`terminal/create`、`terminal/output`、`terminal/wait_for_exit`、`terminal/kill`、`terminal/release`），支持：
- 实时输出流
- 输出截断（outputByteLimit）
- 环境变量、工作目录
- Tool call 内嵌 terminal（type: "terminal" + terminalId）
- 超时控制（combine with kill）

**方案**：在 `NewSession`/`LoadSession` 时检测客户端 `clientCapabilities.terminal` 字段。如果客户端支持终端，将 `Bash` 工具替换为使用 ACP 终端 API 的变体。

```go
// agent/acp/agent.go — NewSession 中
// 检测客户端终端能力（需从 Initialize 响应中获取并传递）
// 当前实现是 agent 端 initialize，client 端会响应 clientCapabilities

// 需要：将 clientCapabilities 从 Initialize 响应传递到 NewSession
// 方法一：TachiAgent 存储 clientCapabilities
// 方法二：NewSession req 中带 clientCapabilities（需要 SDK 支持）

// 假设存储在 TachiAgent 中
func (t *TachiAgent) Initialize(ctx context.Context, req acp.InitializeRequest) (acp.InitializeResponse, error) {
    // 存储 client capabilities
    t.clientCapabilities = req.ClientCapabilities
    // ... 原有 initialize 逻辑 ...
}

// 在 NewSession 中：
if t.clientCapabilities.Terminal {
    // 注册 ACP 终端模式的 Bash 替代工具
    aiAgent.RegisterTool(newTerminalBashTool(t.conn, sess.ID))
    aiAgent.UnregisterTool(tools.ToolNameBash)  // 替换原生 Bash
}
```

**终端 Bash 工具设计**：

```go
// agent/acp/terminal_tool.go (新增)
type terminalBashTool struct {
    conn      *acp.AgentSideConnection
    sessionID string
}

func (t *terminalBashTool) Name() string     { return "bash" }
func (t *terminalBashTool) Description() string { return "Execute shell commands" }
// ... Properties, Required, Parallel ...

func (t *terminalBashTool) ExecuteContext(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
    command := args["command"].(string)

    // 1. terminal/create
    termResp, err := t.conn.TerminalCreate(ctx, acp.TerminalCreateRequest{
        SessionId: acp.SessionId(t.sessionID),
        Command:   parseCommand(command),
        Args:      parseArgs(command),
        // 可选的 cwd, env, outputByteLimit
    })
    if err != nil {
        return tools.ToolResult{Status: tools.ToolResultError, Err: err}, nil
    }
    terminalID := termResp.TerminalId

    // 2. 在 tool call 中嵌入 terminal，让 Zed 实时显示输出
    t.conn.SessionUpdate(ctx, acp.SessionNotification{
        SessionId: acp.SessionId(t.sessionID),
        Update:    acp.EmbedTerminalInToolCall(toolCallID, terminalID),
    })

    // 3. 等待完成（带超时）
    select {
    case <-waitTerminal(ctx, t.conn, sessionID, terminalID):
        // 获取最终输出
        outputResp, _ := t.conn.TerminalOutput(ctx, ...)
        // terminal/release
        t.conn.TerminalRelease(ctx, ...)
        return tools.ToolResult{...}, nil
    case <-time.After(timeout):
        t.conn.TerminalKill(ctx, ...)
        // 获取截断输出
        // terminal/release
        return tools.ToolResult{...}, nil
    case <-ctx.Done():
        t.conn.TerminalKill(ctx, ...)
        t.conn.TerminalRelease(ctx, ...)
        return tools.ToolResult{Status: tools.ToolResultCancelled}, nil
    }
}
```

**注意事项**：
- ACP 终端是异步的——`terminal/create` 立即返回 terminalId，命令在后台运行
- 当前 `Bash` 工具是同步的（等待命令完成再返回）
- 需要决定：是保持同步语义（内部 wait_for_exit）还是改为异步（返回 terminalId 让 LLM 后续查询）
- **推荐**：保持同步语义，内部使用 `terminal/wait_for_exit` 等待完成

**文件变更**：
- `agent/acp/agent.go` — 存储 `clientCapabilities`、按需替换 Bash 工具
- `agent/acp/terminal_tool.go` — 新增：终端 Bash 工具实现
- `agent/acp/stream.go` — 可能需要支持 terminal 类 tool call content

---

### 3.6 Steer 增强（Tier 2）

**问题**：Zed Agent 面板支持"Steer"功能——用户可以在 Agent 工作过程中，在下个消息框键入指令并切换"Steer"开关，将消息排队并在下一个工具调用边界中断 Agent。当前 Tachi 实现：
1. `Cancel` 方法已实现，可以中断当前 prompt
2. 但中断后**没有保存部分结果**到缓存历史
3. 客户端需要手动发新 prompt 来"纠正"Agent

**当前流程**：
```
用户发 Prompt A → Agent 开始工作（工具调用中...）
    → 用户切换 Steer，发 Prompt B
    → Zed Cancel 当前 Prompt
    → Agent 立即中断，PromptA 返回 Cancelled
    → Zed 发 PromptB，包含: [历史消息 + PromptB 指令]
    → Agent 重新开始
```

**问题**：PromptA 取消时，当前轮次的消息（工具调用、LLM 部分输出）没有被保存到 `sess.history`（只在 `AgentEventTurnComplete` 时缓存）。PromptB 只能看到 PromptA 之前的历史。

**方案**：

在 `Cancel` 时，保存当前缓存的消息到历史：

```go
// agent/acp/stream.go — 新增
func (s *acpStreamState) snapshotPartialHistory(sess *ACPSession) {
    // 在当前 prompt 取消时，将已流出的消息保存为历史
    // 这允许后续 Prompt 看到之前的部分工作
    if s.partialMessages != nil && len(s.partialMessages) > 0 {
        sess.history = append(sess.history, s.partialMessages...)
    }
}

// 在 streamToACP 中持续收集部分消息：
case agent.AgentEventTextDelta:
    s.accumulatePartialMessage(event.TextDelta)
case agent.AgentEventToolCallStart:
    s.recordPartialToolCall(event)
// ...
```

**注意**：这需要 `streamToACP` 中持续构建"当前轮次的消息"。与 `session/load` 的重放不同——这是**正在进行的轮次**的部分快照。

**简化方案**：不用完整快照，而是在 Cancel 时：
1. 把当前已发送到客户端的消息 URL / 引用记录下来
2. 后续 Prompt 时，在系统提示中加入"上轮被中断时已做了 X、Y、Z"

**推荐**：先实现最简单的版本——**不保存部分结果**。因为：
- Zed 的 Steer 语义本来就是"中止当前操作，换一种方式"
- 用户看到 Agent 做了部分工作然后中断，通常会在 Steer 消息中说明"刚才 X 方向不对，现在做 Y"
- 保存部分结果增加了复杂度，且正确性难以保证（部分工具结果可能不完整）

**可能需要的改进**：
如果有用户在 Zed 中需要"继续之前被中断的工作"的场景，可以补充。但作为 v1 设计，**Cancel 清空当前轮次状态**是可以接受的。

**文件变更**：最小改动——只需确保 `Cancel` 后 `sess.history` 不会被破坏（当前实现已满足，因为只有 `TurnComplete` 会改 `sess.history`）。

---

### 3.7 Slash 命令完善（Tier 2）

**问题**：当前已有 10 个 slash 命令，但有几个可改进的点。

**改进清单**：

#### 3.7.1 `/mcp reconnect` 真正实现

**现状**：`/mcp reconnect <name>` 在 ACP 模式下是空操作，只输出"disconnected, configure in mcp.json"。

**改进**：实现真正的 reconnect：

```go
func handleACPMCPReconnect(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, name string) (acp.StopReason, error) {
    if name == "" {
        sendTextUpdate(ctx, conn, sessionID, "Usage: /mcp reconnect <name>")
        return acp.StopReasonEndTurn, nil
    }

    mgr := sess.mcpMgr
    if mgr == nil {
        sendTextUpdate(ctx, conn, sessionID, "No MCP manager available.")
        return acp.StopReasonEndTurn, nil
    }

    // 先断开（如果已连接）
    if mgr.IsConnected(name) {
        mgr.Disconnect(name)
    }

    // 重新连接
    serverCfg := findMCPServerConfig(sess.cfg, name)
    if serverCfg == nil {
        sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("MCP server %q not found in config.", name))
        return acp.StopReasonEndTurn, nil
    }

    tools, err := mgr.Connect(ctx, serverCfg)
    if err != nil {
        sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("Reconnect failed: %v", err))
        return acp.StopReasonEndTurn, nil
    }

    // 注册新工具
    for _, tool := range tools {
        sess.agent.RegisterTool(tool)
    }

    // 更新 available commands（如果 MCP 提供了新的 slash 命令）
    if t.conn != nil {
        acpCommands := buildACPAvailableCommands(sess.agent)
        _ = t.conn.SessionUpdate(...
            AvailableCommandsUpdate: ...)
    }

    sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("✅ MCP server **%s** reconnected.", name))
    return acp.StopReasonEndTurn, nil
}
```

**依赖**：需要 `mcp.Manager` 有 `Disconnect(name)` 和 `Connect(ctx, config)` 方法。

#### 3.7.2 `/research` 结构化进度

**现状**：`/research` 发送纯文本进度更新（"Searching...", "Analyzing..."）。

**改进**：使用更结构化的更新格式，或者在 plan 中展示研究步骤。可以复用 plan update 机制：

```go
// 研究开始前发送 plan
conn.SessionUpdate(ctx, acp.SessionNotification{
    SessionId: sessionID,
    Update: acp.SessionUpdate{
        PlanUpdate: &acp.SessionPlanUpdate{
            Entries: []acp.PlanEntry{
                {Content: "Phase 1: 广度搜索", Priority: "high", Status: "in_progress"},
                {Content: "Phase 2: 深度分析", Priority: "high", Status: "pending"},
                {Content: "Phase 3: 综合报告", Priority: "medium", Status: "pending"},
            },
        },
    },
})

// 每个阶段完成时更新 entry status
```

#### 3.7.3 `/model` Tab 补全提示

**现状**：`/model` 的 `InputHint` 未设置，Zed 无法给用户提示。

**改进**：在命令注册中增加 input hint：

```go
{
    Name:        "model",
    Description: "Switch LLM model/provider",
    InputHint:   "model name (e.g. gpt-4o, claude-sonnet-4)",
    Handler:     handleACPModel,
}
```

#### 3.7.4 新增 `/ask` 命令（临时替代 Elicitation）

**现状**：`AskUserQuestion` 被注销，Agent 无法向用户提问。

**改进**：新增 `/ask` slash 命令，让用户可以主动提问题给 Agent。这不是交互式提问的替代品，但提供了一种单向通道让用户表达疑问：

```go
{
    Name:        "ask",
    Description: "Ask a question or provide feedback to the agent",
    InputHint:   "your question or feedback",
    Handler:     handleACPAsk,
}
```

实现：将用户的提问注入到当前会话的历史中，作为一条 user message，然后触发 LLM 回复。

**文件变更**：
- `agent/acp/commands.go` — 修改 handler，增强 `/mcp reconnect`、添加 hints、新增 `/ask`
- 可能需要检查 `mcp.Manager` 接口能力

---

### 3.8 Elicitation 准备（Tier 3）

**问题**：ACP 协议的 `elicitation/create` RFD 在 2026-07-09 进入 Preview，定义了 Agent 以结构化方式向用户提问的标准机制。Zed 的交互式输入支持还在开发中（[讨论 #59828](https://github.com/zed-industries/zed/discussions/59828)），但这是协议的核心发展方向。

**现状**：`AskUserQuestion` 工具在 ACP 模式中被显式注销。工具本身是一个 `ConfirmationTool`（需要确认/输入），在 TUI 模式下会弹表单。

**方案**：分两阶段实现：

**Phase 1（独立于客户端支持）**：
1. 在 `Initialize` 响应中声明 `Elicitation` 能力（检测客户端是否支持）
2. 将 `AskUserQuestion` 工具内部实现改为：检测 ACP 模式且客户端支持 elicitation 时，通过 `conn.ElicitationCreate` 发送 form 模式的请求
3. 处理 `accept`/`decline`/`cancel` 响应

**Phase 2（当 Zed 支持后）**：
1. 自动生效，无需修改 Tachi 代码
2. Agent 可以在对话中间向用户提问（"你希望用哪种重构策略？"）

```go
// AskUserQuestion 工具的 ACP 分支（伪代码）
func (t *AskUserQuestionTool) ExecuteContext(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
    question := args["question"].(string)
    schema := extractSchema(args)  // JSON Schema from args

    if conn := acpctx.Conn(ctx); conn != nil {
        // ACP 模式：使用 elicitation
        resp, err := conn.ElicitationCreate(ctx, acp.ElicitationRequest{
            Mode: "form",
            Message: question,
            RequestedSchema: schema,
        })
        if err != nil {
            return tools.ToolResult{Err: err}, nil
        }
        switch resp.Action {
        case "accept":
            return tools.ToolResult{Output: formatResponse(resp.Content)}, nil
        case "decline":
            return tools.ToolResult{Output: "User declined to answer."}, nil
        case "cancel":
            return tools.ToolResult{Output: "User dismissed the question."}, nil
        }
    }

    // TUI 模式：原有逻辑（ConfirmationTool）
    return t.handleTUIQuestion(ctx, args)
}
```

**文件变更**：
- `agent/acp/agent.go` — `Initialize` 声明 elicitation 能力（如果 SDK 支持）
- `agent/acp/agent.go` — `NewSession` 按需注册 AskUserQuestion 工具
- `agent/tools/ask_user.go` — 增加 ACP 模式分支
- 依赖：需要 `acp-go-sdk` 支持 `ElicitationCreate` 方法

---

### 3.9 Usage 增强（Tier 3）

**问题**：当前 `UsageUpdate` 只在 `TurnComplete` 时发送一次，包含 `Size`（context window）和 `Used`（last input estimate）。

**改进**：

1. **发送时机增加**：在长时间运行的工具调用序列中，可以阶段性发送 usage update
2. **内容增加**：如果 ACP 客户端支持，可以发送更详细的成本信息

```go
// 当前实现
{
    "usageUpdate": {
        "size": 200000,
        "used": 45231
    }
}

// 可扩展
{
    "usageUpdate": {
        "size": 200000,
        "used": 45231,
        // 可选字段（需要检查 ACP 协议版本）
        "cost": 0.0023,
        "costCurrency": "CNY"
    }
}
```

**注意**：ACP 协议的 `UsageUpdate` 目前仅定义 `size` 和 `used` 字段。成本信息在 `session_usage` RFD 中讨论。扩展前需确认协议版本支持。

**文件变更**：
- `agent/acp/stream.go` — 在 `AgentEventTurnComplete` 分支中增加更多 usage 信息

---

### 3.10 Boolean Config Options（Tier 3）

**问题**：当前 `SetSessionConfigOption` 返回 "boolean config options are not supported" 错误。ACP 协议支持 `boolean` 类型的 config option（需要客户端在 `clientCapabilities.session.configOptions.boolean` 中声明）。

**方案**：在客户端支持的前提下，增加 boolean config option 的支持。适合的配置项：
- `compact_auto` — 是否启用自动 compact
- `thinking_visible` — 是否显示 thinking

```go
// agent/acp/model_config.go — 新增
func buildBooleanConfigOptions(clientSupportsBoolean bool) []acp.SessionConfigOption {
    if !clientSupportsBoolean {
        return nil
    }

    // 示例：auto-compact 开关
    return []acp.SessionConfigOption{
        {
            Boolean: &acp.SessionConfigOptionBoolean{
                Id:           "compact_auto",
                Name:         "Auto Compact",
                Description:  "Automatically compact conversation when approaching context limit",
                Category:     acp.Ptr(acp.SessionConfigOptionCategoryModelConfig),
                CurrentValue: true,
            },
        },
    }
}

// agent/acp/agent.go — SetSessionConfigOption
case "compact_auto", "thinking_visible":
    // 处理 boolean 值
    if req.Boolean == nil {
        return ..., fmt.Errorf("expected boolean value for %s", configID)
    }
    value := req.Boolean.Value
    // 应用配置变更
    return ..., nil
```

**文件变更**：
- `agent/acp/model_config.go` — 新增 boolean config option 构建函数
- `agent/acp/agent.go` — `SetSessionConfigOption` 处理 boolean 类型

---

## 四、各变更的影响范围

### 4.1 文件变更汇总

| 文件 | 变更类型 | 对应方向 | 状态 |
|------|----------|----------|------|
| `agent/acp/stream.go` | 修改 | Plan, Diff in Result, Usage, Steer | ✅ Diff in Result 已完成 |
| `agent/acp/agent.go` | 修改 | Session Delete, Terminal, Elicitation, Boolean | ✅ Session Delete 已完成 |
| `agent/acp/commands.go` | 修改 | Slash 完善 | ❌ |
| `agent/acp/model_config.go` | 修改 | Boolean Config | ❌ |
| `agent/tools/read.go` | 修改 | ACP FS delegation | ✅ 已完成 |
| `agent/tools/write.go` | 修改 | Bugfix: 补充缺失的 SessionId | ✅ 已完成 |
| `agent/acp/terminal_tool.go` | 新增 | Terminal delegation | ❌ |
| `session/manager.go` | 可能修改 | Session Delete（已有 `Delete` 方法） | ✅ 无需修改 |

### 4.2 代码量估算

| 方向 | 新增代码 | 修改代码 | 工作量 |
|------|----------|----------|--------|
| Agent Plan | ~80 行 | ~30 行 | 中 |
| Diff in Result | ~40 行 | ~15 行 | 低 |
| ReadFile ACP FS | ~20 行 | ~10 行 | 低 |
| Session Delete | ~30 行 | ~5 行 | 低 |
| Terminal delegation | ~200 行 | ~50 行 | 大 |
| Steer 增强 | ~30 行 | ~20 行 | 中 |
| Slash 完善 | ~60 行 | ~40 行 | 低-中 |
| Elicitation 准备 | ~100 行 | ~40 行 | 中 |
| Usage 增强 | ~20 行 | ~15 行 | 低 |
| Boolean Config | ~60 行 | ~30 行 | 低 |

### 4.3 与其他系统的交互

| 系统 | 影响 |
|------|------|
| `agent/agent_loop.go` | 无影响（所有变更在 ACP 层） |
| `agent/tools/` | ReadFile 增加 ACP 分支；其他工具不变 |
| `agent/acp/stream.go` | 核心修改点（Plan, Diff, Usage） |
| `agent/acp/permission.go` | 无影响（已有 diff 发送逻辑） |
| `agent/acp/convert.go` | 无影响 |
| `agent/acp/session.go` | 无影响 |
| `agent/mcp/` | 需要确认 `Disconnect`/`Connect` 接口 |
| `session/manager.go` | 可能需要新增 `Delete(id)` |

---

## 五、实施建议

### 5.1 分阶段实施路线

**Phase 0 — 基础设施（已完成）**
- Session Delete（低工作量，补齐协议覆盖）✅
- ReadFile ACP FS（低工作量，文件模式对齐）✅
- WriteFile bugfix: 补充缺失的 SessionId 字段 ✅

**Phase 1 — 核心体验提升（3-5天）**
- Agent Plan 流（中等，最有视觉冲击力的改进）
- Diff 内嵌到 Tool Result（低，改动小但效果明显）✅ 已完成
- Slash 命令完善（低-中，/mcp reconnect + /ask 等）

**Phase 2 — 进阶能力（5-8天）**
- 终端委派（大，需要完整的 terminal 适配器）
- Steer 增强（中，中断时保留下文）

**Phase 3 — 前瞻性（可并行于 Phase 1/2）**
- Elicitation 准备（中等，客户端就绪后自动生效）
- Boolean Config Options（低，协议对齐）
- Usage 增强（低，锦上添花）

### 5.2 优先级建议

**如果目标是"用最小投入让 Zed 体验明显提升"**：

1. **先做**：Agent Plan（最 visible）+ Diff in Result（最实用）+ ReadFile ACP FS（补缺口）→ 3-5 天
2. **接着做**：Terminal delegation（最大差异化竞争力）→ 5-8 天
3. **并行做**：Session Delete + Slash 完善 + 其他低工作量项目 → 2-3 天

### 5.3 前置依赖检查清单

- [ ] `session.Manager` 是否有 `Delete(id)` 方法？
- [ ] `mcp.Manager` 是否有 `Disconnect(name)` 和 `Connect(ctx, config)` 方法？
- [ ] `acp-go-sdk` 是否支持 `ElicitationCreate`？
- [ ] `acp-go-sdk` 的 `SessionUpdate` 是否已有 `PlanUpdate` 字段？
- [ ] `acp-go-sdk` 是否支持 `clientCapabilities` 读取？
- [ ] `acp-go-sdk` 的 `ToolCallContent` 是否已有 `ToolDiffContent` 构造器？
- [ ] `acp-go-sdk` 的 `TerminalCreate`/`TerminalOutput`/`TerminalWaitForExit` 等方法是否可用？

---

## 附录：关键协议参考

### ACP Plan Update 结构

```json
{
  "jsonrpc": "2.0",
  "method": "session/update",
  "params": {
    "sessionId": "sess_abc123",
    "update": {
      "sessionUpdate": "plan",
      "entries": [
        {
          "content": "Analyze the existing codebase structure",
          "priority": "high",
          "status": "pending"
        },
        {
          "content": "Identify components that need refactoring",
          "priority": "high",
          "status": "pending"
        }
      ]
    }
  }
}
```

### ACP Tool Call with Diff Content

```json
{
  "jsonrpc": "2.0",
  "method": "session/update",
  "params": {
    "sessionId": "sess_abc123",
    "update": {
      "sessionUpdate": "tool_call_update",
      "toolCallId": "call_001",
      "content": [
        {
          "type": "diff",
          "path": "/home/user/project/src/main.go",
          "oldText": "func old() {}",
          "newText": "func new() {}"
        }
      ]
    }
  }
}
```

### ACP Terminal in Tool Call

```json
{
  "jsonrpc": "2.0",
  "method": "session/update",
  "params": {
    "sessionId": "sess_abc123",
    "update": {
      "sessionUpdate": "tool_call",
      "toolCallId": "call_002",
      "title": "Running tests",
      "kind": "execute",
      "status": "in_progress",
      "content": [
        {
          "type": "terminal",
          "terminalId": "term_xyz789"
        }
      ]
    }
  }
}
```

### ACP Elicitation Request

```json
{
  "jsonrpc": "2.0",
  "id": 43,
  "method": "elicitation/create",
  "params": {
    "sessionId": "sess_abc123",
    "mode": "form",
    "message": "How would you like me to approach this refactoring?",
    "requestedSchema": {
      "type": "object",
      "properties": {
        "strategy": {
          "type": "string",
          "title": "Refactoring Strategy",
          "oneOf": [
            { "const": "conservative", "title": "Conservative" },
            { "const": "balanced", "title": "Balanced (Recommended)" },
            { "const": "aggressive", "title": "Aggressive" }
          ],
          "default": "balanced"
        }
      },
      "required": ["strategy"]
    }
  }
}
```

---

### 版本历史

| 版本 | 日期 | 变更 |
|------|------|------|
| 1.1 | 2026-07-11 | 实现 Session Delete、ReadFile ACP FS、Diff in Result、WriteFile SessionId bugfix；更新状态表 |
| 1.0 | 2026-07-11 | 初始设计 |
