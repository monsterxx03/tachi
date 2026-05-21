# ACP Agent 实现方案

> 版本: 1.3 | 日期: 2026-05-21 | 状态: 实施就绪

## 一、概述

实现 ACP (Agent Client Protocol) 支持，使 Tachi 可作为编辑器的 AI Agent 后端（编辑器是 Client，Tachi 是 Server）。通信方式：JSON-RPC 2.0 over stdio。

**依赖**：`github.com/coder/acp-go-sdk v0.13.0`（Apache-2.0）

SDK 提供：
- `acp.Agent` 接口（我们实现）
- `acp.AgentSideConnection`（JSON-RPC dispatch + 向 Client 发通知/请求）
- 所有协议类型定义

我们需要写的桥接层：
- `Prompt()` ↔ `RunConversationStream()` 
- `AgentEvent` → `SessionUpdate` 流式转换
- `ConfirmationTool` → `RequestPermission`
- ACP Session → Tachi `session.Manager`
- ContentBlock ↔ 内部消息格式

**参考**：
- 协议规范: https://agentclientprotocol.com/protocol/overview
- SDK 仓库: https://github.com/coder/acp-go-sdk

---

## 二、新增代码结构

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

## 三、详细模块设计

### 3.1 agent.go — TachiAgent

核心：实现 `acp.Agent` 接口。

```go
import "github.com/coder/acp-go-sdk"

type TachiAgent struct {
    cfg      *config.Config
    sessions *ACPSessionManager
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
    // 详见 3.3
}

func (t *TachiAgent) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
    // 详见 3.2
}

func (t *TachiAgent) Cancel(ctx context.Context, req acp.CancelNotification) error {
    // 详见 3.7
}

// ... 其他方法（Authenticate, LoadSession, ResumeSession, ListSessions, CloseSession, SetSessionMode, SetSessionConfigOption）
```

**设计决策**：`TachiAgent` 不持有 `AIAgent` 实例——每个 ACP Session 独立拥有自己的 `AIAgent`（详见 3.3）。

### 3.2 Prompt 处理器（最核心）

这是 TachiAgent 中最核心的模块，桥接 SDK 和 Tachi agent loop：

```
session/prompt
  ↓
convertContentBlocks(req.Prompt) → Tachi 内部消息格式
  ↓
同步阻塞:
  ├── RunConversationStream(promptCtx, history, userMsg, systemPrompt, opts)
  │     → chan AgentEvent
  ├── StreamToACP: chan AgentEvent → acp.SessionUpdate 通知
  └── channel 关闭 → 返回 PromptResponse
```

关键设计：**`Prompt()` 是阻塞的**。SDK 内部会在 goroutine 里调用 `Prompt()`，返回后 SDK 自动将结果序列化为 JSON-RPC 响应。我们的 `Prompt()` 在整个 LLM 交互周期内阻塞，期间通过 `t.conn.SessionUpdate()` 发送流式更新。

```go
func (t *TachiAgent) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
    sess, ok := t.sessions.Get(string(req.SessionId))
    if !ok {
        return acp.PromptResponse{}, fmt.Errorf("session not found: %s", req.SessionId)
    }
    
    // 防御性：同一 session 内串行（ACP 协议保证，但防御并发 bug）
    sess.mu.Lock()
    defer sess.mu.Unlock()
    
    // 1. 构建可取消的 prompt context
    promptCtx, promptCancel := context.WithCancel(sess.ctx)
    defer promptCancel()
    sess.setPromptCancel(promptCancel)  // 暴露给 Cancel() 调用
    
    // 2. 转换输入
    userMsg, err := convertToTachiMessage(req.Prompt)
    if err != nil {
        return acp.PromptResponse{}, err
    }
    
    // 3. 构建历史（从 Tachi session 恢复）
    history := buildHistory(sess.SessMgr)
    
    // 4. 运行 agent loop（阻塞，同步drain）
    eventCh := sess.Agent.RunConversationStream(promptCtx, history, userMsg, systemPrompt, opts)
    
    // 5. 流式转换：AgentEvent → ACP 通知
    stopReason, err := StreamToACP(promptCtx, sess, t.conn, eventCh)
    if err != nil {
        return acp.PromptResponse{}, err
    }
    
    // 6. 清理
    sess.setPromptCancel(nil)
    
    return acp.PromptResponse{StopReason: stopReason}, nil
}
```

**为什么 `Prompt()` 要同步阻塞？** ACP 协议设计如此——`session/prompt` 是请求-响应模式，工具循环期间通过 `session/update` 通知来流式输出。SDK 在 goroutine 里调用 `Prompt()`，不阻塞主 dispatch 循环。

**Steer 机制**：ACP 模式下**不设置 steer channel**（`steerRespCh == nil`）。编辑器中的后续输入以新的 `session/prompt` 请求发送，不需要 mid-turn injection。现有 agent_loop 中 `a.steerRespCh != nil` 的条件会自动跳过 steer check。

### 3.3 session.go — ACPSession 与 AIAgent 实例模型

**核心决策：每个 ACP Session 持有独立的 AIAgent 实例。**

原因：
- `AIAgent` 有实例级 channel（`confirmRespCh`），多 session 共享会串台
- MCP 连接建立有开销（OAuth），不应每次 Prompt 重建
- Session 历史需在同一 `session.Manager` 中累积
- 与 channel mode 的 "per-turn fresh agent" 不同——ACP session 生命周期跨多次 Prompt

```go
type ACPSession struct {
    ID     string
    Cwd    string
    Agent  *agent.AIAgent       // 独立 AIAgent 实例
    MCPMgr *mcp.Manager         // 独立 MCP manager（session 结束时关闭）
    SessMgr *session.Manager    // 独立 session manager（JSONL 持久化）

    ctx          context.Context      // session 级 context
    cancel       context.CancelFunc   // 关闭整个 session
    promptCancel context.CancelFunc   // 取消当前 prompt（Cancel 调用）
    mu           sync.Mutex           // 保护并发 Prompt（防御性）
}

func (s *ACPSession) setPromptCancel(fn context.CancelFunc) {
    // 由 Prompt() 设置，由 Cancel() 读取
    s.promptCancel = fn
}

type ACPSessionManager struct {
    mu       sync.RWMutex
    sessions map[string]*ACPSession
}

func (sm *ACPSessionManager) New(ctx context.Context, cwd string, cfg *config.Config, conn *acp.AgentSideConnection) (*ACPSession, error)
func (sm *ACPSessionManager) Get(id string) (*ACPSession, bool)
func (sm *ACPSessionManager) Delete(id string)
func (sm *ACPSessionManager) List() []*ACPSession
func (sm *ACPSessionManager) CloseAll()  // 进程退出时清理
```

**NewSession 实现**：

```go
func (t *TachiAgent) NewSession(ctx context.Context, req acp.NewSessionRequest) (acp.NewSessionResponse, error) {
    cwd := extractCwd(req)  // 从 session 参数获取工作目录
    
    // 1. 创建独立的 AIAgent
    provider, _ := llm.NewProvider(ctx, t.cfg.Provider, t.cfg.Model)
    aiAgent := agent.NewAIAgent(provider, t.cfg.Model, 0)  // 无 iteration 上限
    
    // 2. Configure（注册工具、连接 MCP、设置 subagent）
    mcpMgr, _ := aiAgent.Configure(ctx, t.cfg)
    
    // 3. ACP 特化配置
    aiAgent.SetPermissionMode(agent.PermissionModeExternal)
    aiAgent.SetPermissionHandler(buildPermissionHandler(t.conn, sessionID))
    aiAgent.UnregisterTool(tools.ToolNameAskUser)  // ACP 无交互式提问
    aiAgent.SetTitleGenEnabled(false)              // 编辑器管理标题
    // 不调用 SetSteerChannel — steerRespCh 保持 nil
    
    // 4. 预创建 Tachi session（避免 RunConversationStream 内部创建）
    sm := session.NewManager(session.DefaultStore(), cfg.MaxSessions)
    sm.New(provider.Name(), t.cfg.Model, cwd)
    sm.SetTitle("ACP Session")  // 占位标题
    aiAgent.SetSessionManager(sm)
    
    // 5. 设置工作目录（通过 wdctx）
    // wdctx.SetFallbackDir 在 ACP 模式下不适用（多 session），
    // 每次 Prompt() 调用时通过 wdctx.WithDir(promptCtx, cwd) 传递
    
    // 6. 合并编辑器传来的 MCP（如果有）
    if req.McpServers != nil {
        mergeMCPServers(mcpMgr, req.McpServers, t.cfg.ACP.MCPConflictPolicy)
    }
    
    // 7. 注册 session
    sessCtx, sessCancel := context.WithCancel(context.Background())
    sess := &ACPSession{
        ID: newID(), Cwd: cwd,
        Agent: aiAgent, MCPMgr: mcpMgr, SessMgr: sm,
        ctx: sessCtx, cancel: sessCancel,
    }
    t.sessions.Add(sess)
    
    return acp.NewSessionResponse{SessionId: acp.SessionId(sess.ID)}, nil
}
```

**CloseSession 实现**：

```go
func (t *TachiAgent) CloseSession(ctx context.Context, req acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
    sess, ok := t.sessions.Get(string(req.SessionId))
    if !ok {
        return acp.CloseSessionResponse{}, nil
    }
    
    // 取消进行中的 prompt
    sess.cancel()
    // 关闭 MCP 连接
    if sess.MCPMgr != nil {
        sess.MCPMgr.Close()
    }
    // 结束 Tachi session
    sess.SessMgr.EndCurrent()
    // 从 manager 移除
    t.sessions.Delete(sess.ID)
    
    return acp.CloseSessionResponse{}, nil
}
```

**ACP Session 与 Tachi Session 的映射**：

| ACP 方法 | Tachi 操作 |
|---------|-----------|
| `session/new` | `NewAIAgent` + `Configure` + `session.Manager.New()` |
| `session/load` | 从磁盘加载 session → 重建 AIAgent + 重放历史 |
| `session/resume` | 同 load，但保留进行中的状态 |
| `session/close` | `cancel()` + `MCPMgr.Close()` + `SessMgr.EndCurrent()` |
| `session/list` | 扫描 `~/.tachi/session/` 目录 |

### 3.4 stream.go — 事件流转换

```go
// StreamToACP 消费 AgentEvent channel，转为 ACP session/update 通知。
// 阻塞直到 channel 关闭或 ctx 取消。返回最终 stopReason。
func StreamToACP(
    ctx context.Context,
    sess *ACPSession,
    conn *acp.AgentSideConnection,
    events <-chan agent.AgentEvent,
) (acp.StopReason, error) {
    var stopReason acp.StopReason = acp.StopReasonEndTurn
    
    for {
        select {
        case event, ok := <-events:
            if !ok {
                return stopReason, nil
            }
            
            switch event.Type {
            case agent.AgentEventTextDelta:
                conn.SessionUpdate(ctx, acp.SessionNotification{
                    SessionId: acp.SessionId(sess.ID),
                    Update: acp.SessionUpdate{
                        AgentMessageChunk: &acp.ContentChunk{
                            Content: []acp.ContentBlock{{Text: &acp.TextBlock{Text: event.TextDelta}}},
                        },
                    },
                })
                
            case agent.AgentEventToolCallStart:
                conn.SessionUpdate(ctx, acp.SessionNotification{
                    SessionId: acp.SessionId(sess.ID),
                    Update: acp.SessionUpdate{
                        ToolCall: &acp.ToolCall{
                            ToolCallId: acp.ToolCallId(event.ToolID),
                            Title:      event.ToolName,
                            Kind:       mapToolKind(event.ToolName),
                            Status:     acp.ToolCallStatusRunning,
                        },
                    },
                })
                
            case agent.AgentEventToolResult:
                status := acp.ToolCallStatusCompleted
                if event.ToolIsError {
                    status = acp.ToolCallStatusFailed
                }
                conn.SessionUpdate(ctx, acp.SessionNotification{
                    SessionId: acp.SessionId(sess.ID),
                    Update: acp.SessionUpdate{
                        ToolCallUpdate: &acp.ToolCallUpdate{
                            ToolCallId: acp.ToolCallId(event.ToolID),
                            Status:     &status,
                        },
                    },
                })
                
            case agent.AgentEventTurnComplete:
                if event.Result != nil {
                    stopReason = mapStopReason(event.Result.ExitReason)
                }
                // channel 即将关闭，下一次 <-events 会 return
                
            case agent.AgentEventError:
                stopReason = acp.StopReasonEndTurn  // 或自定义 error reason
                // 将错误作为 agent message 发送
                if event.Result != nil && event.Result.Error != nil {
                    conn.SessionUpdate(ctx, acp.SessionNotification{
                        SessionId: acp.SessionId(sess.ID),
                        Update: acp.SessionUpdate{
                            AgentMessageChunk: &acp.ContentChunk{
                                Content: []acp.ContentBlock{{Text: &acp.TextBlock{
                                    Text: "Error: " + event.Result.Error.Error(),
                                }}},
                            },
                        },
                    })
                }
                
            // 以下事件在 ACP 模式下忽略：
            // AgentEventThinkingDelta — 编辑器不展示 thinking
            // AgentEventToolCallArgs — 增量 args，ACP 不需要
            // AgentEventSteerCheck — ACP 不用 steer
            // AgentEventSessionTitle — ACP 不用 title
            // AgentEventSubagentStart/Done — 内部实现细节
            // AgentEventUsage — 内部统计
            // AgentEventAskUser — ACP 已 unregister AskUser tool
            }
            
        case <-ctx.Done():
            return acp.StopReasonCancelled, nil
        }
    }
}

// mapToolKind 将 Tachi 工具名映射到 ACP ToolKind
func mapToolKind(toolName string) *acp.ToolKind {
    switch toolName {
    case tools.ToolNameRead:
        k := acp.ToolKindRead; return &k
    case tools.ToolNameWrite, tools.ToolNameEdit:
        k := acp.ToolKindEdit; return &k
    case tools.ToolNameBash:
        k := acp.ToolKindCommand; return &k
    default:
        return nil
    }
}

// mapStopReason 将 Tachi ExitReason 映射到 ACP StopReason
func mapStopReason(exitReason string) acp.StopReason {
    switch exitReason {
    case "stop":
        return acp.StopReasonEndTurn
    case "cancelled":
        return acp.StopReasonCancelled
    default:
        return acp.StopReasonEndTurn
    }
}
```

**注意**：`AgentEvent` 是 flat struct（`Type string` + 多字段），不是 interface。用 `switch event.Type` 分发。参考 `agent/agent_loop.go` 中的事件类型常量。

### 3.5 permission.go — 权限桥接（PermissionMode 三态）

#### 对 agent_loop 的改造

在 `tool_executor.go` 中，将 `skipEditConfirm bool` 替换为 `PermissionMode` 三态枚举：

```go
// agent/agent.go — 新增
type PermissionMode int

const (
    PermissionModeTUI      PermissionMode = iota // TUI: emit event → block on confirmRespCh
    PermissionModeSkip                           // 自动通过 (subagent, tachi run, channel)
    PermissionModeExternal                       // 外部 handler (ACP)
)

// 外部权限处理器签名
type PermissionHandler func(ctx context.Context, toolName, toolID, diff, args string) (bool, error)

// AIAgent 字段变更：
// - skipEditConfirm bool          → permissionMode PermissionMode
// + permissionHandler PermissionHandler

func (a *AIAgent) SetPermissionMode(mode PermissionMode)
func (a *AIAgent) SetPermissionHandler(h PermissionHandler)
```

#### tool_executor.go 改造（唯一的 agent_loop 变更）

```go
// 原来：if a.skipEditConfirm { ... } else { ... }
// 改为：
if tr.Status == tools.ToolResultPendingConfirm {
    switch a.permissionMode {
    case PermissionModeSkip:
        // 直接执行（原 skipEditConfirm=true 逻辑不变）
        output, err := a.toolRegistry.ExecuteConfirmed(ctx, tc.Function.Name, tr.Args)
        // ...

    case PermissionModeExternal:
        // 调用外部 handler（ACP 的 RequestPermission）
        approved, err := a.permissionHandler(ctx, tc.Function.Name, tc.ID, tr.Diff, tr.Args)
        if err != nil {
            tr = tools.ToolResult{Status: tools.ToolResultError, Err: err}
        } else if !approved {
            tr = tools.ToolResult{Status: tools.ToolResultError, Err: errors.New("permission denied by client")}
        } else {
            output, err := a.toolRegistry.ExecuteConfirmed(ctx, tc.Function.Name, tr.Args)
            // ...
        }

    default: // PermissionModeTUI
        // 原 skipEditConfirm=false 逻辑完全不变
        ch <- AgentEvent{Type: AgentEventToolConfirmation, ...}
        select {
        case confirmed := <-a.confirmRespCh:
            // ...
        case <-ctx.Done():
            // ...
        }
    }
}
```

#### 迁移清单

| 调用侧 | 原代码 | 新代码 |
|--------|--------|--------|
| TUI | `SetSkipEditConfirm(false)` (默认) | `SetPermissionMode(PermissionModeTUI)` (默认) |
| `tachi run` | `SetSkipEditConfirm(true)` | `SetPermissionMode(PermissionModeSkip)` |
| channel mode | `SetSkipEditConfirm(true)` | `SetPermissionMode(PermissionModeSkip)` |
| subagent | `SetSkipEditConfirm(true)` | `SetPermissionMode(PermissionModeSkip)` |
| **ACP** | — | `SetPermissionMode(PermissionModeExternal)` + `SetPermissionHandler(...)` |

**TUI 路径零行为变更**——default case 保持原有 channel 通信逻辑。

#### ACP 侧 PermissionHandler 实现

```go
func buildPermissionHandler(conn *acp.AgentSideConnection, sessionID string) agent.PermissionHandler {
    return func(ctx context.Context, toolName, toolID, diff, args string) (bool, error) {
        resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
            SessionId: acp.SessionId(sessionID),
            ToolCall: acp.ToolCallUpdate{
                ToolCallId: acp.ToolCallId(toolID),
                Title:      &toolName,
                Kind:       ptr(acp.ToolKindEdit),
                Status:     ptr(acp.ToolCallStatusPending),
                // diff 通过 Content 传递给编辑器
                Content: &acp.ToolCallContent{Text: diff},
            },
            Options: []acp.PermissionOption{
                {Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: "allow"},
                {Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: "reject"},
                {Kind: acp.PermissionOptionKindAllowAll, Name: "Allow all edits", OptionId: "allow_all"},
            },
        })
        if err != nil {
            return false, err
        }
        
        if resp.Outcome.Selected == nil {
            return false, nil
        }
        
        // "allow_all" → 切换为 Skip 模式（后续不再询问）
        if resp.Outcome.Selected.OptionId == "allow_all" {
            // 注意：此处需要访问 AIAgent，通过闭包捕获
            return true, nil  // 同时外部设置 permissionMode = PermissionModeSkip
        }
        
        return resp.Outcome.Selected.OptionId == "allow", nil
    }
}
```

### 3.6 convert.go — 内容转换

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

### 3.7 Cancel 语义与 Context 传播

**Context 传播链**：

```
ACPSession.ctx (session 生命周期)
    │
    └── promptCtx = context.WithCancel(session.ctx)  (单次 Prompt 生命周期)
            │
            ├── RunConversationStream(promptCtx, ...)
            │       │
            │       ├── provider.CreateChatStream(promptCtx, ...)  → HTTP 请求中断
            │       └── tool.ExecuteContext(promptCtx, ...)        → bash killed
            │
            └── Cancel notification → sess.promptCancel()  → propagates
```

**实现**：

```go
func (t *TachiAgent) Cancel(ctx context.Context, req acp.CancelNotification) error {
    sess, ok := t.sessions.Get(string(req.SessionId))
    if !ok {
        return nil  // session 不存在，静默忽略
    }
    
    // promptCancel 由 Prompt() 设置，取消当前进行中的 prompt
    sess.mu.Lock()
    cancel := sess.promptCancel
    sess.mu.Unlock()
    
    if cancel != nil {
        cancel()
    }
    return nil
}
```

**Cancel 后的 Prompt() 返回**：
- `RunConversationStream` 内部 goroutine 检测到 `ctx.Done()`，关闭 event channel
- `StreamToACP` 从 `<-ctx.Done()` 分支退出，返回 `StopReasonCancelled`
- `Prompt()` 正常返回 `PromptResponse{StopReason: "cancelled"}`（不是 error）
- SDK 将此序列化为正常的 JSON-RPC 响应

**Session 级 Cancel**（CloseSession / 进程退出）：
- `sess.cancel()` 取消 session ctx → 自动传播到 promptCtx → 同上流程

### 3.8 错误处理策略

| 错误场景 | 处理方式 |
|---------|---------|
| LLM provider rate limit / auth error | `RunConversationStream` 通过 `AgentEventError` 上报 → 转为 `SessionUpdate` 中的 error message → `Prompt()` 返回正常 response（`StopReason: "error"`） |
| tool 执行 panic | `RunConversationStream` 内部 recover → `AgentEventError` |
| `RequestPermission` 超时 | `PermissionHandler` 返回 `(false, err)` → 工具标记为失败，agent loop 继续 |
| stdin EOF（编辑器退出） | SDK 检测到，触发 `conn.Done()` → `runACPAgent` 退出 → 清理所有 session |
| MCP server 连接断开 | 与 TUI 模式相同——工具调用返回错误，LLM 自行决定是否重试 |

**进程退出 cleanup**：

```go
// runACPAgent 中:
defer t.sessions.CloseAll()  // 关闭所有 session 的 MCP、cancel context

// CloseAll 实现:
func (sm *ACPSessionManager) CloseAll() {
    sm.mu.Lock()
    defer sm.mu.Unlock()
    for _, sess := range sm.sessions {
        sess.cancel()
        if sess.MCPMgr != nil {
            sess.MCPMgr.Close()
        }
    }
}
```

---

## 四、启动入口

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
    // stderr 启动日志（stdout 被 JSON-RPC 占用）
    fmt.Fprintf(os.Stderr, "tachi: ACP agent started (version %s)\n", version)
    
    // 1. 创建 TachiAgent（不在此处创建 AIAgent——推迟到 NewSession）
    sessMgr := NewACPSessionManager()
    tachiAgent := &TachiAgent{
        cfg:      cfg,
        sessions: sessMgr,
    }
    defer sessMgr.CloseAll()
    
    // 2. 启动 SDK 连接（阻塞，直到 stdin EOF）
    conn := acp.NewAgentSideConnection(tachiAgent, os.Stdout, os.Stdin)
    tachiAgent.conn = conn
    
    // 等待连接结束
    <-conn.Done()
    return nil
}
```

**启动入口极简**：不创建 AIAgent——推迟到 `NewSession`。

---

## 五、与现有系统的交互

ACP 模式和 TUI/Channel 模式共享同一个 tool/session/mcp 代码。差异点：

| 模式 | PermissionMode | AskUser | Steer | Title Gen |
|------|---------------|---------|-------|-----------|
| TUI | `PermissionModeTUI` | ✅ | ✅ | ✅ |
| `tachi run` | `PermissionModeSkip` | ✅ (auto-confirm) | ❌ | ❌ |
| Channel | `PermissionModeSkip` | ❌ | ✅ | ✅ |
| **ACP** | `PermissionModeExternal` | ❌ | ❌ | ❌ |

其他系统沿用不变：
- **Session 持久化**：每个 ACP Session 独立 `session.Manager`，写入 `~/.tachi/session/<id>/`
- **System Reminders**：正常工作（`Collector.Collect()` 在 `RunConversationStream` 内部注入）
- **Skill / Memory**：正常工作
- **SubAgent**：正常工作（子 agent 用 `PermissionModeSkip`，与当前行为一致）
- **Steer**：不设置 `steerRespCh`（现有代码 `a.steerRespCh != nil` 条件天然兼容）

---

## 六、配置

```yaml
# config.yaml 新增
acp_agent:
  connect_configured_mcp: true        # 在 NewSession 时连接 config.yaml 中的 MCP
  mcp_conflict_policy: "client_wins"  # 编辑器传入的同名 MCP 优先
```

其余所有配置项（`provider`、`model`、`language`、`subagent`、`memory`、`mcp_servers`、`web_search`）在 ACP 模式下行为不变。

---

## 七、实施计划

### Phase 1：骨架 + PermissionMode 重构（2-3天）

- [ ] `agent/agent.go` — 引入 `PermissionMode` 枚举 + `PermissionHandler` 类型 + setter
- [ ] `agent/tool_executor.go` — `skipEditConfirm` → `switch a.permissionMode`（~20 行变更）
- [ ] 迁移调用侧：TUI / `tachi run` / channel / subagent（各 1 行）
- [ ] 添加依赖 `github.com/coder/acp-go-sdk v0.13.0`
- [ ] `agent/acp/agent.go` — 实现 `acp.Agent` 接口（`Initialize` + stubs）
- [ ] `agent/acp/session.go` — `ACPSessionManager` + `ACPSession` 结构
- [ ] CLI 入口 `tachi acp`（main.go）
- [ ] 验证：`go test ./agent/...` 通过（PermissionMode 不破坏现有）+ Zed 中 `tachi acp` 初始化成功

### Phase 2：核心会话（2-3天）

- [ ] `agent/acp/session.go` — `NewSession` 完整实现（per-session AIAgent）
- [ ] `agent/acp/convert.go` — ContentBlock ↔ 内部消息
- [ ] `agent/acp/stream.go` — `StreamToACP`: AgentEvent → ACP session/update
- [ ] `agent/acp/agent.go` — `Prompt()` 完整实现
- [ ] 验证：在编辑器发消息，看到 Tachi 流式回复 + 工具调用正常

### Phase 3：权限与取消（2天）

- [ ] `agent/acp/permission.go` — `buildPermissionHandler` 实现
- [ ] `agent/acp/agent.go` — `Cancel()` 实现 + context 传播验证
- [ ] 工具执行状态上报（`StartToolCall` → `ToolCallUpdate(completed/failed)`）
- [ ] 验证：EditFile 在编辑器里显示 diff 确认 + Cancel 中断正常

### Phase 4：会话管理 + 持久化（2天）

- [ ] `session/load` — 从磁盘重建 session + AIAgent + 重放历史
- [ ] `session/resume` — 恢复进行中的 session
- [ ] `session/list` — 扫描 `~/.tachi/session/` 目录
- [ ] `session/close` — 完整 cleanup（MCP close + session end）
- [ ] 验证：关闭编辑器再打开，能恢复对话

### Phase 5：打磨（1-2天）

- [ ] MCP 配置合并策略实现
- [ ] 进程退出 cleanup（`CloseAll`）
- [ ] 错误场景测试（provider error、MCP disconnect、permission timeout）
- [ ] `TestPromptRoundTrip`：mock stdin/stdout 跑完整 JSON-RPC 往返
- [ ] 文档：README 更新 + 编辑器配置指南

**总计：~9-12 天，900-1200 行新 Go 代码**（含 PermissionMode 重构 ~100 行）

---

## 八、注意事项

| 约束 | 说明 |
|------|------|
| `AgentEvent` 是 flat struct | `Type string` + 多字段混用。用 `switch event.Type` 分发，不是 type assertion |
| SDK 类型需要实际确认 | 实现前 `go doc` 确认 `acp.SessionUpdate`、`acp.ContentChunk` 等结构体字段名 |
| `session.Manager` 构造 | 查看 `session/manager.go` 的 `NewManager` 签名，确认 store 参数来源 |
| ACP 协议版本 | 固定返回 `acp.ProtocolVersionNumber`。编辑器发来更高版本时由编辑器决定降级 |
| MCP 冲突策略 | 同名 MCP server 与编辑器传入冲突时，编辑器优先（`client_wins`） |
| PermissionMode 重构 | 是 agent_loop 的唯一改动点。改完后必须跑 `go test ./agent/...` + `go test ./tui/...` |
| stdout 被占用 | ACP 模式下 stdout 是 JSON-RPC 通道。所有 debug 输出走 `debuglog`（写文件）或 stderr |

---

## 附录

### ACP 协议参考

- 协议规范: https://agentclientprotocol.com/protocol/overview
- 规范仓库: https://github.com/agentclientprotocol/agent-client-protocol

### Go SDK

- `github.com/coder/acp-go-sdk` — v0.13.0, Apache-2.0
- 实现模式: `agent.go` 实现 `acp.Agent` + `acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)` + `<-conn.Done()`

### 版本历史

| 版本 | 日期 | 变更 |
|------|------|------|
| 1.0 | 2026-05-20 | 初始设计 |
| 1.1 | 2026-05-20 | 改用 `coder/acp-go-sdk` |
| 1.2 | 2026-05-21 | 细化：PermissionMode、per-session AIAgent、Cancel、错误处理 |
| 1.3 | 2026-05-21 | 精简为实施文档：删除决策背景，修正 AgentEvent 代码错误，解决所有开放问题 |
