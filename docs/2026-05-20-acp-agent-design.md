# ACP Agent 实现方案

> 版本: 1.1 | 日期: 2026-05-20 | 状态: 设计阶段

## 一、背景

### 什么是 ACP

**Agent Client Protocol (ACP)** 是 Zed 于 2025 年 8 月提出、现已由社区治理的开放标准协议，定义**编辑器 ↔ 编码 Agent** 之间的通信。定位相当于"LSP for AI Coding Agents"。

| 协议 | 解决的问题 | 角色 |
|------|-----------|------|
| MCP | Agent ↔ 工具 | Agent 是 MCP Client，工具是 Server |
| **ACP** | **编辑器 ↔ Agent** | **编辑器是 Client，Agent 是 Server** |
| A2A | Agent ↔ Agent | 双端平等 |

---

## 二、外部 SDK 评估：用还是不用？

结论：**用 `github.com/coder/acp-go-sdk`**。

### SDK 管了什么

`coder/acp-go-sdk` 提供了一个 `acp.Agent` 接口：

```go
type Agent interface {
    Initialize(context.Context, InitializeRequest) (InitializeResponse, error)
    Authenticate(context.Context, AuthenticateRequest) (AuthenticateResponse, error)
    NewSession(context.Context, NewSessionRequest) (NewSessionResponse, error)
    LoadSession(context.Context, LoadSessionRequest) (LoadSessionResponse, error)
    ResumeSession(context.Context, ResumeSessionRequest) (ResumeSessionResponse, error)
    ListSessions(context.Context, ListSessionsRequest) (ListSessionsResponse, error)
    CloseSession(context.Context, CloseSessionRequest) (CloseSessionResponse, error)
    Prompt(context.Context, PromptRequest) (PromptResponse, error)
    Cancel(context.Context, CancelNotification) error
    SetSessionMode(context.Context, SetSessionModeRequest) (SetSessionModeResponse, error)
    SetSessionConfigOption(context.Context, SetSessionConfigOptionRequest) (SetSessionConfigOptionResponse, error)
}
```

和一个 `AgentSideConnection`，封装了**从 Agent 端发送请求到 Client**：

```go
asc := acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)
asc.SessionUpdate(ctx, notification)     // 发 session/update 通知
asc.RequestPermission(ctx, req)          // 发权限请求，等响应
asc.ReadTextFile(ctx, req)               // 读编辑器端文件
asc.WriteTextFile(ctx, req)              // 写编辑器端文件
asc.CallExtension(ctx, method, params)   // 调用扩展方法
asc.Done()                               // 等待连接关闭
```

SDK 自动处理：
- stdin/stdout 的 JSON-RPC 2.0 消息读写 ✅（~200 行）
- 所有协议类型定义（`InitializeRequest`、`PromptResponse`、`SessionUpdate` 等）✅（~500 行）
- 请求-响应关联和 dispatch ✅（~150 行）
- 错误码和 validation ✅（~100 行）

### SDK 管不了什么

SDK 不知道 Tachi 内部如何运行，以下**必须我们自己写**：

| 模块 | 为什么 SDK 管不了 |
|------|-----------------|
| `Prompt()` ↔ `RunConversationStream()` 桥接 | SDK 不知道 agent loop 的存在 |
| `ConfirmationTool` → `RequestPermission` | SDK 不知道 `NeedsConfirmation()`/`GetDiff()` |
| `session.Manager` → ACP Session 生命周期 | SDK 不知道 `session.Manager` 的 JSONL 持久化 |
| MCP server 合并（config.yaml + 编辑器传入） | SDK 不知道 Tachi 的 MCP 配置模型 |
| `AgentEvent` → `SessionUpdate` 流式转换 | SDK 不知道 `chan AgentEvent` |
| ContentBlock ↔ 内部消息格式 | SDK 不知道 Tachi 的 `llm.Message` |

### 对比：手写 vs 用 SDK

| 维度 | 手写 | 用 SDK |
|------|------|--------|
| 代码量 | ~1200-1500 行 | **~700-900 行** |
| JSON-RPC 框架 | 自己实现 stdin 读写 + dispatch | 直接实现 `acp.Agent` 接口 |
| 类型定义 | 手写所有 request/response 结构体 | SDK 已生成 |
| 协议兼容性 | 自己踩坑 | SDK 已通过兼容性测试 |
| 版本依赖 | 无 | 添加入口依赖（v0.13.0，Apache 2.0） |
| 自定义灵活性 | 完全控制 | 核心 PO 路径符合，边缘场景可扩展 |
| 学习成本 | 逐字读 ACP 规范 | 看 SDK 示例即可上手 |

### 版本锁定策略

`go.mod` 中固定 v0.13.0（当前最新稳定版），不自动升级 major 版本。协议升级时按需评估。

---

## 三、总体架构

```
┌──────────────────────────────────────────────────────────────────┐
│                    编辑器 (ACP Client)                            │
│  Zed / JetBrains / VS Code / Neovim / Emacs / Obsidian ...      │
│                                                                  │
│  ┌──────────────────── ACP over stdio ─────────────────────┐    │
│  │  stdin → JSON-RPC requests                              │    │
│  │  stdout ← JSON-RPC responses + notifications            │    │
│  └─────────────────────────────────────────────────────────┘    │
│         ↑                                                        │
│         ∣ acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)│
│         ↓                                                        │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │  coder/acp-go-sdk (JSON-RPC 2.0 框架)                   │    │
│  │  ├── dispatch: 调用 Agent 接口方法                       │    │
│  │  └── AgentSideConnection: 发通知/权限请求                │    │
│  │                                                          │    │
│  │  TachiAgent (实现 acp.Agent)                              │    │
│  │  ├── Initialize, NewSession, Prompt, Cancel, ...         │    │
│  │  ├── Bridge: Prompt → RunConversationStream              │    │
│  │  └── Bridge: ConfirmationTool → RequestPermission        │    │
│  │                                                          │    │
│  │  ┌────────────────────────────────────────────────┐     │    │
│  │  │ Tachi Agent Engine (agent/, tool/, mcp/, ...)   │     │    │
│  │  └────────────────────────────────────────────────┘     │    │
│  └─────────────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────────────┘
```

---

## 四、新增代码结构

```
agent/acp/
  agent.go            ← TachiAgent: 实现 acp.Agent 接口
  session.go          ← ACPSession + SessionManager
  stream.go           ← AgentEvent → acp.SessionUpdate 转换
  permission.go       ← EditFile 确认 → acp.RequestPermission
  convert.go          ← ContentBlock ↔ Tachi 内部消息
```

不再需要手写 `server.go`——SDK 的 `AgentSideConnection` 就是服务器。

---

## 五、详细模块设计

### 5.1 agent.go — TachiAgent

核心：实现 `acp.Agent` 接口。

```go
import "github.com/coder/acp-go-sdk"

type TachiAgent struct {
    agent    *agent.AIAgent
    sessions *SessionManager
    conn     *acp.AgentSideConnection  // SDK 连接（设后用于发通知）
}

// acp.Agent 必须实现的方法：
func (t *TachiAgent) SetAgentConnection(c *acp.AgentSideConnection) { t.conn = c }

func (t *TachiAgent) Initialize(ctx context.Context, req acp.InitializeRequest) (acp.InitializeResponse, error) {
    return acp.InitializeResponse{
        ProtocolVersion: acp.ProtocolVersionNumber,
        AgentCapabilities: acp.AgentCapabilities{
            LoadSession: true,
            SessionCapabilities: &acp.SessionCapabilities{
                List:  &acp.SessionListCapabilities{},
                Close: &acp.SessionCloseCapabilities{},
                Resume: &acp.SessionCloseCapabilities{},
            },
            McpCapabilities: &acp.McpCapabilities{
                HTTP: true,
                SSE:  false,
            },
            PromptCapabilities: &acp.PromptCapabilities{
                Image:           false,
                Audio:           false,
                EmbeddedContext: true,
            },
        },
        AgentInfo: &acp.Implementation{
            Name:    "tachi",
            Title:   "Tachi",
            Version: version,
        },
    }, nil
}

func (t *TachiAgent) NewSession(ctx context.Context, req acp.NewSessionRequest) (acp.NewSessionResponse, error) {
    // 1. 创建 Tachi session
    // 2. 设置 wdctx
    // 3. 连接客户端传来的 MCP（与 config.yaml 合并）
    // 4. 注册 ACP session → Tachi session 映射
    // 5. 返回 sessionId
}

func (t *TachiAgent) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
    // 最核心的方法，见 5.2
}

func (t *TachiAgent) Cancel(ctx context.Context, req acp.CancelNotification) error {
    // 取消当前 prompt goroutine
}

// ... 其他方法（Authenticate, LoadSession, ResumeSession, ListSessions, CloseSession, SetSessionMode, SetSessionConfigOption）
```

### 5.2 Prompt 处理器（最核心）

这是 TachiAgent 中最核心的模块，桥接 SDK 和 Tachi agent loop：

```
session/prompt
  ↓
convertContentBlocks(req.Prompt) → Tachi 内部消息格式
  ↓
启动 goroutine:
  ├── RunConversationStream(ctx, history, userMsg, systemPrompt, opts)
  │     → chan AgentEvent
  ├── streamLoop: chan AgentEvent → acp.SessionUpdate 通知
  └── 同步等待 stream 完成 → 返回 PromptResponse
```

关键设计：**`Prompt()` 是阻塞的**。SDK 内部会在 goroutine 里调用 `Prompt()`，返回后 SDK 自动将结果序列化为 JSON-RPC 响应。我们的 `Prompt()` 在整个 LLM 交互周期内阻塞，期间通过 `t.conn.SessionUpdate()` 发送流式更新。

```go
func (t *TachiAgent) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
    session, _ := t.sessions.Get(string(req.SessionId))
    
    // 1. 转换输入
    userMsg, err := convertToTachiMessage(req.Prompt)
    
    // 2. 构建历史（从 session 恢复）
    history := t.buildHistory(session)
    
    // 3. 运行 agent loop（阻塞，同步）
    eventCh, err := t.agent.RunConversationStream(ctx, history, userMsg, systemPrompt, opts)
    
    // 4. 流式转换：AgentEvent → ACP 通知
    streamErr := StreamToACP(ctx, session, t.conn, eventCh)
    
    // 5. 返回 stopReason
    return acp.PromptResponse{StopReason: stopReason}, nil
}
```

**为什么 `Prompt()` 要同步阻塞？** ACP 协议设计如此——`session/prompt` 是请求-响应模式，工具循环期间通过 `session/update` 通知来流式输出。SDK 在 goroutine 里调用 `Prompt()`，不阻塞主 dispatch 循环。

### 5.3 session.go — Session 管理器

```go
type ACPSession struct {
    ID          string
    Cwd         string
    Cancel      context.CancelFunc  // 取消 prompt goroutine
    Active      bool
}

type SessionManager struct {
    mu       sync.RWMutex
    sessions map[string]*ACPSession
}

func (sm *SessionManager) New(cwd string) *ACPSession
func (sm *SessionManager) Get(id string) (*ACPSession, bool)
func (sm *SessionManager) Delete(id string)
func (sm *SessionManager) List() []*ACPSession
```

ACP Session 和 Tachi Session 的映射：

| ACP 方法 | Tachi 组件 |
|---------|-----------|
| `session/new` | `session.Manager.StartNew()` |
| `session/load` | `session.Manager.LoadSession(id)` + 重放历史 |
| `session/close` | `session.EndCurrent()` + MCP 断开 |

**注意**：ACP 支持**多并发 Session**（同一个 Agent 进程可以服务多个独立对话）。当前 Tachi 的 `session.Manager` 是单 current session 模型。ACP 模式下需要 `ACPSessionManager` 来管理多 Session，`session.Manager` 仍用于持久化。

```go
// ACP 模式下:
// - ACPSessionManager 管理活跃中的 ACP session（允许多个）
// - 每个 ACP session 对应一个独立的 Tachi session（持久化到 JSONL）
// - 每个 ACP session 有独立的 cancel context
```

### 5.4 stream.go — 事件流转换

```go
// StreamToACP 转换 AgentEvent → ACP session/update 通知。
// 返回最终 stopReason。
func StreamToACP(
    ctx context.Context,
    session *ACPSession,
    conn *acp.AgentSideConnection,
    events <-chan agent.AgentEvent,
) (acp.StopReason, error) {
    
    for {
        select {
        case event, ok := <-events:
            if !ok {
                return acp.StopReasonEndTurn, nil
            }
            
            update := convertEvent(event)
            if update == nil {
                continue // 某些事件不需要发送通知
            }
            
            conn.SessionUpdate(ctx, acp.SessionNotification{
                SessionId: acp.SessionId(session.ID),
                Update:    *update,
            })
            
        case <-ctx.Done():
            return acp.StopReasonCancelled, nil
        }
    }
}

func convertEvent(event agent.AgentEvent) *acp.SessionUpdate {
    switch e := event.(type) {
    case agent.EventTextChunk:
        return &acp.SessionUpdate{
            AgentMessageChunk: &acp.ContentChunk{
                Content: acp.TextBlock(e.Text),
            },
        }
    case agent.EventToolCallStart:
        return &acp.SessionUpdate{
            ToolCall: &acp.ToolCall{
                ToolCallId: acp.ToolCallId(e.ToolCallID),
                Title:      e.ToolName,
                Kind:       toolKindPtr(e.ToolName),
                Status:     acp.ToolCallStatusPending,
                ...
            },
        }
    case agent.EventToolCallResult:
        status := acp.ToolCallStatusCompleted
        if e.Err != nil {
            status = acp.ToolCallStatusFailed
        }
        return &acp.SessionUpdate{
            ToolCallUpdate: &acp.ToolCallUpdate{
                ToolCallId: acp.ToolCallId(e.ToolCallID),
                Status:     &status,
                ...
            },
        }
    default:
        return nil
    }
}
```

SDK 提供了 `SessionUpdate` 的便捷构建器：

```go
// 直接用 SDK 的 helper
conn.SessionUpdate(ctx, acp.SessionNotification{
    SessionId: id,
    Update: acp.UpdateAgentMessageText("Hello!"),
})

// 工具调用
conn.SessionUpdate(ctx, acp.SessionNotification{
    SessionId: id,
    Update: acp.StartToolCall(callID, "Reading file",
        acp.WithStartKind(acp.ToolKindRead),
        acp.WithStartStatus(acp.ToolCallStatusPending),
    ),
})
```

### 5.5 permission.go — 权限桥接

在 ACP 模式下，Tachi 的工具执行流程需要拦截 `ConfirmationTool` 确认，转为走 ACP 通道。

**改造策略**：在 `tool_executor.go` 中增加一个 `permissionHandler` 钩子：

```go
// ACP 模式下注入
agent.toolExecutor.SetPermissionHandler(func(ctx context.Context, result tools.ToolResult) (bool, error) {
    // 发送 ACP 权限请求
    resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
        SessionId: sessionID,
        ToolCall: acp.ToolCallUpdate{
            ToolCallId: callID,
            Title:      &result.Name,
            Kind:       ptr(acp.ToolKindEdit),
            Status:     ptr(acp.ToolCallStatusPending),
        },
        Options: []acp.PermissionOption{
            {Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow once", OptionId: "allow"},
            {Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: "reject"},
        },
    })
    
    if resp.Outcome.Selected != nil && resp.Outcome.Selected.OptionId == "allow" {
        return true, nil
    }
    return false, nil
})
```

### 5.6 convert.go — 内容转换

```go
// ACP ContentBlock → Tachi 内部消息
func convertContentBlocks(blocks []acp.ContentBlock) (string, error) {
    var sb strings.Builder
    for _, block := range blocks {
        switch {
        case block.Text != nil:
            sb.WriteString(block.Text.Text)
        case block.Resource != nil:
            // 同 Tachi 的 @-file 内联格式
            sb.WriteString("--- BEGIN UNTRUSTED FILE CONTENT: " + extractPath(block.Resource.Resource) + " ---\n")
            sb.WriteString(getContent(block.Resource.Resource))
            sb.WriteString("\n--- END UNTRUSTED FILE CONTENT ---\n")
        }
    }
    return sb.String(), nil
}
```

---

## 六、启动入口

```go
// main.go
app.Commands = append(app.Commands, &cli.Command{
    Name:  "acp",
    Usage: "Run as ACP agent (JSON-RPC over stdio)",
    Action: func(ctx context.Context, cmd *cli.Command) error {
        return runACPAgent(ctx, cfg)
    },
})

func runACPAgent(ctx context.Context, cfg *config.Config) error {
    // 1. 初始化 provider
    provider, err := llm.NewProvider(ctx, cfg.Provider, cfg.Model)
    
    // 2. 创建 AIAgent + 注册工具
    agent := agent.NewAIAgent(provider, cfg.Model, cfg.MaxIterations)
    agent.RegisterTools()
    agent.SetSkipEditConfirm(true) // ACP 模式下不走 TUI 确认
    
    // 3. 启动时连接 config.yaml 中的 MCP 服务器
    mcpManager := mcp.NewManager()
    if !cfg.ACP.ConnectConfiguredMCP {
        mcpTools, _ := mcpManager.ConnectAll(ctx, cfg.MCPServers)
        for _, t := range mcpTools { agent.RegisterTool(t) }
    }
    
    // 4. 创建 ACPSessionManager
    sessMgr := NewSessionManager(agent)
    
    // 5. 创建 TachiAgent（实现 acp.Agent）
    tachiAgent := &TachiAgent{
        agent:    agent,
        sessions: sessMgr,
    }
    
    // 6. 启动 SDK 连接（阻塞，直到 stdin EOF）
    conn := acp.NewAgentSideConnection(tachiAgent, os.Stdout, os.Stdin)
    tachiAgent.SetConnection(conn)
    
    // 等待连接结束
    <-conn.Done()
    return nil
}
```

---

## 七、与 Tachi 现有系统的交互

### 7.1 session 持久化

ACP Session 的数据复用 Tachi 现有的 `session.Manager`：

```
~/.tachi/session/<id>/
  ├── meta.json       ← 含 cwd 信息
  └── messages.jsonl  ← 消息日志
```

### 7.2 tool registry

ACP 模式和 TUI 模式共享同一个 `tool.Registry`。确认机制切换：

- **TUI 模式**: `SetSkipEditConfirm(false)` → TUI `stateAwaitingConfirmation`
- **ACP 模式**: `SetSkipEditConfirm(true)` → 工具执行前拦截，走 `session/request_permission`

### 7.3 steer 机制

ACP 模式下不适用。编辑器中的后续输入以新的 `session/prompt` 发送。

### 7.4 system reminders

所有 reminder 继续工作（DateReminder, GitReminder, IterationWarning 等），Tachi 的 agent loop 自己注入。

### 7.5 Skill 和 Memory

保持完整功能，无需适配。

---

## 八、配置

```yaml
# config.yaml 新增
acp_agent:
  # 启动时连接 config.yaml 中的 MCP（true），还是等编辑器 session/new 传（false）
  connect_configured_mcp: true
  # 同名 MCP 冲突策略
  mcp_conflict_policy: "client_wins"  # 或 "config_wins"
```

### 配置项在 ACP 模式下的行为

| 配置 | 行为 |
|------|------|
| `provider` / `model` | ACP 模式下沿用 |
| `language` | 沿用 |
| `subagent` | 沿用（子 Agent 在 ACP 模式下继续工作） |
| `memory` | 沿用 |
| `mcp_servers` | 与编辑器传入的合并，按 `mcp_conflict_policy` |
| `web_search` | 沿用 |

---

## 九、实施计划

### Phase 1：骨架（2天）

- [ ] 添加依赖 `github.com/coder/acp-go-sdk v0.13.0`
- [ ] `agent/acp/agent.go` — 实现 `acp.Agent` 接口（`Initialize` + `NewSession` + `ListSessions` + `CloseSession` + stubs）
- [ ] `agent/acp/session.go` — ACPSessionManager
- [ ] CLI 入口 `tachi acp`
- [ ] 验证：在 Zed 中配置 `tachi acp`，看到初始化成功

### Phase 2：核心会话（2-3天）

- [ ] `agent/acp/convert.go` — ContentBlock ↔ 内部消息
- [ ] `agent/acp/stream.go` — AgentEvent → ACP session/update
- [ ] `Prompt()` — 完整 prompt turn 生命周期
- [ ] 验证：在编辑器发消息，看到 Tachi 回复，工具调用正常

### Phase 3：工具与确认（2天）

- [ ] `agent/acp/permission.go` — EditFile 确认桥接
- [ ] 工具执行状态上报（`StartToolCall` → `WithUpdateStatus(completed)`）
- [ ] 验证：文件编辑在编辑器里显示 diff 确认

### Phase 4：会话管理（2天）

- [ ] `session/load` — 重放历史
- [ ] `session/resume` — 直接恢复
- [ ] `session/cancel` — 中断
- [ ] 验证：关闭编辑器再打开，能恢复对话

### Phase 5：打磨（1天）

- [ ] MCP 配置合并策略
- [ ] 优雅降级与错误处理
- [ ] 文档

**总计：~9-10 天，700-900 行新 Go 代码**

---

## 十、风险与注意事项

### 已知风险

| 风险 | 影响 | 缓解 |
|------|------|------|
| `session/prompt` 阻塞调用 | 后续请求无法同步处理 | SDK 在 goroutine 中调用 `Prompt()`，主 dispatch 未被阻塞 |
| ACP 的 cwd 与 Tachi 全局工作目录冲突 | 文件操作路径混乱 | 通过 `wdctx.WithDir()` 隔离 |
| 多 Session 并发 | Tachi 的 session.Manager 是单 current | ACP 模式用 ACPSessionManager 管理多 Session，持久化复用 session.Manager |
| 编辑器发送不可信的 MCP 配置 | 恶意 MCP 工具 | 沿用 config 中的安全策略 |

### 开放讨论

1. **多 Session 并发** — ACP 支持同一 Agent 多个并发 Session。Tachi 当前 `session.Manager` 是单 current session 模型。ACP 模式下用 `ACPSessionManager` 管理多会话，每个独立 `cancel context`。
2. **MCP 冲突** — config.yaml 中同名 MCP server 与编辑器传来的冲突时谁优先？（暂定 `client_wins`）
3. **Skill 在编辑器里的激活方式** — 通过 ACP 的 slash command 能力暴露？还是用 ACP session config options？

---

## 十一、附录

### ACP 协议参考

- 官方网站: https://agentclientprotocol.com
- 协议规范: https://agentclientprotocol.com/protocol/overview
- 规范仓库: https://github.com/agentclientprotocol/agent-client-protocol

### Go SDK

- `github.com/coder/acp-go-sdk` — v0.13.0, Apache-2.0, 推荐
- 实现示例: `agent.go` 实现 `acp.Agent` + `acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)` + `<-conn.Done()`

### ACP 兼容编辑器

| 编辑器 | 方式 |
|-------|------|
| Zed | 原生内置 |
| JetBrains | 原生内置 (2025.3+) |
| VS Code | formulahendry.vscode-acp / strato-space.acp-plugin |
| Neovim | codecompanion.nvim / agentic.nvim / avante.nvim / acp.nvim |
| Emacs | agent-shell.el |
| Obsidian | obsidian-agent-client |

### 版本历史

| 版本 | 日期 | 变更 |
|------|------|------|
| 1.0 | 2026-05-20 | 初始设计 |
| 1.1 | 2026-05-20 | 改用 `coder/acp-go-sdk`，去掉价值说明 |
