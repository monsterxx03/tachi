# Hook 系统与 Herdr 集成设计

日期: 2026-07-26
状态: 草稿

## 1. 动机

Tachi 目前没有任何生命周期事件暴露机制，无法与外部系统（如终端复用器、监控工具、CI 流水线）集成。

**目标：**
1. 为 Tachi 设计通用 hook 系统，支持在关键生命周期事件发生时执行外部脚本
2. 利用 hook 系统实现 Herdr 集成（检测在 Herdr pane 内运行时自动上报状态）
3. hook 系统本身是通用的——用户可配置任意脚本，不限于 Herdr

**非目标：**
- 不改变 Tachi 现有的 agent loop 核心逻辑
- 不引入新的第三方 Go 依赖（stdlib 足够：`net`, `os/exec`, `context`）

---

## 2. Event 模型

### 2.1 事件列表

| 事件 | 触发时机 | 附带数据（stdin JSON） | 对应 Herdr 状态 |
|---|---|---|---|
| `session_start` | 新 session 创建时 | session_id, workspace_dir, provider | — |
| `session_end` | Session 关闭/清理时 | session_id | idle |
| `turn_start` | 用户消息提交，进入 agent loop | session_id, user_message, turn_count | working |
| `turn_complete` | LLM 回复完成（stop） | session_id, response_text, usage, turn_count | idle |
| `turn_truncated` | LLM 回复被截断（length） | session_id, partial_text, usage, retry_count | working |
| `tool_call` | LLM 发起工具调用 | session_id, tool_name, tool_args, tool_id | working |
| `tool_result` | 工具执行完成 | session_id, tool_name, tool_id, is_error, duration_ms | working |
| `state_change` | Agent 状态切换 | session_id, from_state, to_state | 见下方映射 |
| `permission_request` | 需要用户确认（EditFile/Bash ask） | session_id, tool_name, tool_id, diff | blocked |
| `permission_result` | 用户响应确认 | session_id, tool_name, tool_id, approved | working |
| `ask_user_question` | LLM 向用户提问 | session_id, questions[] | blocked |
| `ask_user_response` | 用户回答了问题 | session_id, answers | working |
| `error` | Agent 遇到错误 | session_id, error_message | idle |

### 2.2 状态映射

Tachi 内部状态 → Herdr 状态语义：

| Tachi 状态 | Herdr 状态 | 说明 |
|---|---|---|
| `stateIdle` | `idle` | 等待用户输入 |
| `stateWaiting` | `working` | 正在等待 LLM 响应 |
| `stateStreaming` | `working` | LLM 正在流式输出 |
| `stateAwaitingConfirmation` | `blocked` | 等待用户确认 EditFile/Bash 操作 |
| `stateAskUserQuestion` | `blocked` | 等待用户回答 LLM 的问题 |

对应的事件触发：
- `stateIdle → stateWaiting` → `state_change: idle→working` + `turn_start`
- `stateWaiting → stateStreaming` → `state_change: working→working`（不触发，同状态不重复）
- `stateStreaming → stateIdle` → `state_change: working→idle` + `turn_complete`
- `stateStreaming → stateAwaitingConfirmation` → `state_change: working→blocked` + `permission_request`
- `stateAwaitingConfirmation → stateStreaming` → `state_change: blocked→working` + `permission_result`
- `stateIdle → stateAskUserQuestion` → `state_change: idle→blocked` + `ask_user_question`
- `stateAskUserQuestion → stateWaiting` → `state_change: blocked→working` + `ask_user_response`

### 2.3 事件去重

同一个事件在同一 turn 内可能多次触发（如多个工具调用连续执行），每条 hook 命令在同一次触发中只运行一次。同一种事件但不带状态变化时不重复通知（如 `working` → `working` 不触发 `state_change`）。

---

## 3. Hook 配置

### 3.1 YAML Schema

在 `config.yaml` 中新增 `hooks` 顶层字段：

```yaml
hooks:
  # 全局开关（默认 true）
  enabled: true

  # 事件 → hook 命令列表（用户自定义的外部脚本）
  events:
    turn_complete:
      - command: bash ~/.tachi/hooks/notify-done.sh
        timeout: 5s          # 默认 5s
        async: true           # 默认 true（异步执行，不阻塞 agent loop）
    tool_call:
      - command: bash /path/to/log-tool.sh
        timeout: 3s
        async: true
    session_start:
      - command: python3 ~/.tachi/hooks/my-integration.py
        timeout: 10s
        async: true
```

### 3.2 YAML 定义（Go 结构体）

```go
// config/config.go 新增

type HooksConfig struct {
    Enabled *bool             `yaml:"enabled" default:"true"`
    Events  map[string][]HookCommand `yaml:"events"` // key = event name
}

type HookCommand struct {
    Command string            `yaml:"command"`       // 外部命令
    Timeout string            `yaml:"timeout,omitempty"` // "5s", 默认 "5s"
    Async   *bool             `yaml:"async,omitempty"`   // 默认 true
    Env     map[string]string `yaml:"env,omitempty"`     // 额外环境变量
}
```

### 3.3 变量展开

Hook command 中支持以下模板变量，在执行时展开：

| 变量 | 展开为 |
|---|---|
| `{{HOOKS_DIR}}` | `~/.tachi/hooks/` 或 `config.BaseDir() + "/hooks"` |
| `{{SESSION_ID}}` | 当前 session ID |
| `{{WORKSPACE_DIR}}` | 当前工作目录 |
| `{{TIMESTAMP}}` | ISO 8601 时间戳 |

### 3.4 默认配置

```yaml
hooks:
  enabled: true
  events: {}
  # 空列表 = 无默认外部命令 hook。Herdr 等内置集成使用 Go 回调，不走这里
```

---

## 4. Hook 执行模型

### 4.1 模块位置

新增包：`agent/hooks/`

```
agent/hooks/
├── dispatcher.go     # 事件分发核心 + Handler 注册
├── dispatcher_test.go
├── hook.go           # HookCommand 模型、配置加载
├── template.go       # 模板变量展开
└── herdr.go          # Herdr 内置集成（Go 回调，无外部脚本）
```

### 4.2 Dispatcher 架构 — 双 Handler 模型

Dispatcher 同时支持两种 handler 类型：

| Handler 类型 | 使用者 | 说明 |
|---|---|---|
| **Callback** | 内置集成（如 Herdr） | Go 函数 `func(ctx, event, payload)`，直接调用 |
| **Command** | 用户配置 | 外部命令，通过 `os/exec` 执行 |

```
Agent Loop 事件点
     │
     ▼
hooks.Dispatcher.Dispatch(ctx, eventName, payload)
     │
     ├─▶ [Callbacks] for each registered Go callback:
     │      func(ctx, eventName, payload) → 直接调用
     │      不影响 agent loop（panic 被 recover）
     │
     └─▶ [Commands] for each configured external command:
           展开模板变量 → os/exec.Command(...) → stdin JSON → 等待/超时
           async=true  → goroutine 不等待
           async=false → 等待完成或超时
```

```go
// agent/hooks/dispatcher.go

type HandlerType int

const (
    HandlerCallback HandlerType = iota // Go 函数回调
    HandlerCommand                     // 外部命令
)

type Handler struct {
    Type     HandlerType
    Name     string // 标识（如 "herdr", "my-script"）

    // Callback 模式
    Callback func(ctx context.Context, event string, payload []byte)

    // Command 模式
    Command string
    Timeout time.Duration
    Async   bool
    Env     map[string]string
}

type Dispatcher struct {
    mu       sync.RWMutex
    handlers map[string][]Handler // event → handlers
}

// RegisterCallback 注册一个 Go 回调 handler
func (d *Dispatcher) RegisterCallback(event string, name string, fn func(ctx context.Context, event string, payload []byte)) {
    d.mu.Lock()
    defer d.mu.Unlock()
    d.handlers[event] = append(d.handlers[event], Handler{
        Type:     HandlerCallback,
        Name:     name,
        Callback: fn,
    })
}

// RegisterCommand 注册一个外部命令 handler（从 YAML 配置加载）
func (d *Dispatcher) RegisterCommand(event string, cmd HookCommand) { ... }

// Dispatch 分发事件到所有匹配的 handler
func (d *Dispatcher) Dispatch(ctx context.Context, event string, payload Payload) {
    // 1. 构建 JSON 负载
    // 2. 调用所有 Callback handler
    // 3. 执行所有 Command handler（异步/同步）
}
```

### 4.3 调用点

在 `agent/agent_loop.go` 和 `agent/tool_executor.go` 中以下位置插入 Dispatch 调用：

| 位置 | 事件 | 实现方式 |
|---|---|---|
| `RunConversationStream` session 创建时 | `session_start` | `sessionManager.New()` 返回后 |
| `RunConversationStream` userMsg 记录后 | `turn_start` | 在 `messages = append(messages, userMsg)` 后 |
| `handleStopFinish` 中 | `turn_complete` | 在发送 `AgentEventTurnComplete` 前 |
| `handleLengthFinish` 中 | `turn_truncated` | 在 `*lengthRetries++` 后 |
| `executeToolCallsSequential` 每个 tool call 前 | `tool_call` | 在 `ch <- AgentEventToolCallArgs` 后 |
| `executeToolCallsSequential` 每个 tool result 后 | `tool_result` | 在 append toolMsg 后 |
| `executeToolCallsParallel` 同理 | `tool_call` / `tool_result` | 同上 |
| TUI `setState()` 状态切换 | `state_change` | 见下方说明 |
| `handleToolCallFinish` 中确认流程 | `permission_request` / `permission_result` | 在 `a.confirmRespCh` 收发前后 |
| `RespondToAskUser` | `ask_user_response` | 在发送 response 后 |

### 4.4 state_change 的特殊处理

`state_change` 事件由 TUI 层的 `setState()` 触发，因为 TUI 是状态机的拥有者。

但 channel 模式、pipe 模式 (`tachi -p`)、sub-agent 都没有 TUI。所以 `state_change` 需要在两层都插入：

1. **Agent 层**：在 `runAgentLoop` 的关键路径上插入状态变化通知
2. **TUI 层**：在 `setState()` 中分发（覆盖 UI 交互引起的状态变化，如用户取消）

为简化实现，**初始版本只从 agent loop 的关键事件推断状态变化**，不依赖 TUI 层：

```
turn_start         → "working"
permission_request → "blocked"  
ask_user_question  → "blocked"
tool_result        → "working"  (如果当前不是 blocked 状态)
turn_complete      → "idle"
turn_truncated     → "working"
error              → "idle"
```

这些事件已经覆盖了所有状态转换场景，不需要单独的 `state_change` 事件。

### 4.5 同步 vs 异步

- **异步（默认）**：Hook 命令在 goroutine 中启动，不阻塞 agent loop
  - 适用于通知、日志、状态上报等非关键路径
  - Hook 执行时长受 timeout 限制，超时后 kill
  
- **同步**：Hook 命令执行完才继续 agent loop
  - 极少数场景，如需要在 tool 执行前做外部校验
  - 会阻塞整个 agent loop，谨慎使用

Timeout 默认 5s，可通过配置微调。

### 4.6 标准输入与环境变量

每条 hook 命令都通过 stdin 接收 JSON 格式的事件负载：

```json
// session_start 示例
{
  "event": "session_start",
  "session_id": "abc123",
  "workspace_dir": "/Users/yejia/repos/tachi",
  "provider": "anthropic",
  "timestamp": "2026-07-26T20:53:44+08:00"
}
```

```json
// tool_call 示例
{
  "event": "tool_call",
  "session_id": "abc123",
  "tool_name": "Bash",
  "tool_id": "toolu_xxx",
  "tool_args": "echo hello",
  "timestamp": "2026-07-26T20:53:44+08:00"
}
```

```json
// state_change 示例 (由 turn_complete 等推断产生)
{
  "event": "state_change",
  "session_id": "abc123",
  "from_state": "working",
  "to_state": "idle",
  "timestamp": "2026-07-26T20:53:44+08:00"
}
```

同时设置以下环境变量：

| 环境变量 | 说明 |
|---|---|
| `TACHI_HOOK_EVENT` | 事件名称（如 `session_start`） |
| `TACHI_SESSION_ID` | 当前 session ID |
| `TACHI_WORKSPACE_DIR` | 当前工作目录 |
| `TACHI_HOOKS_DIR` | Hook 脚本目录 |

---

## 5. Herdr 集成

### 5.1 设计思路

Herdr 集成**不是通过外部脚本**实现的，而是直接在 Go 代码中用 `net` 包连接 Unix socket。

```
事件触发 (agent loop)
    │
    ▼
hooks.Dispatcher.Dispatch("tool_call", payload)
    │
    ├─▶ [Callback] herdrHandler.Handle(ctx, "tool_call", payload)
    │       │
    │       ▼
    │   构建 JSON-RPC 请求
    │   net.Dial("unix", HERDR_SOCKET_PATH)
    │   json.NewEncoder(conn).Encode(request)
    │   conn.Close()
    │
    └─▶ [Command] 用户配置的外部脚本（如果有）
```

优势：
- **零依赖**：只用了 `net`、`encoding/json`、`os` 等 stdlib
- **无子进程开销**：每次事件就是一次 socket 连接 + JSON 收发，毫秒级
- **可靠**：没有 bash escaping 问题、没有 python3 依赖、没有子进程僵尸
- **内建**：随 Tachi 编译，不需要额外部署

### 5.2 HerdrHandler

```go
// agent/hooks/herdr.go

package hooks

import (
    "context"
    "encoding/json"
    "fmt"
    "math/rand"
    "net"
    "os"
    "time"
)

type HerdrHandler struct {
    sockPath string // HERDR_SOCKET_PATH
    paneID   string // HERDR_PANE_ID
    source   string // "herdr:tachi"
    agent    string // "tachi"
}

// DetectHerdr 检查是否运行在 Herdr pane 内部
func DetectHerdr() bool {
    return os.Getenv("HERDR_ENV") == "1" &&
        os.Getenv("HERDR_SOCKET_PATH") != "" &&
        os.Getenv("HERDR_PANE_ID") != ""
}

// NewHerdrHandler 从环境变量创建 Herdr handler
func NewHerdrHandler() *HerdrHandler {
    return &HerdrHandler{
        sockPath: os.Getenv("HERDR_SOCKET_PATH"),
        paneID:   os.Getenv("HERDR_PANE_ID"),
        source:   "herdr:tachi",
        agent:    "tachi",
    }
}

// Herdr 事件 → socket 方法映射
type herdrAction string

const (
    actionSession = herdrAction("session")  // pane.report_agent_session
    actionState   = herdrAction("state")    // pane.report_agent
)

// eventActions 定义哪些 Tachi 事件对应上报到 Herdr 以及参数
var eventActions = map[string]struct {
    action herdrAction
    state  string // "working" / "blocked" / "idle"（仅 actionState 时需要）
    desc   string
}{
    "session_start":     {action: actionSession, desc: "session start"},
    "turn_complete":     {action: actionState, state: "idle", desc: "turn complete"},
    "turn_truncated":    {action: actionState, state: "working", desc: "turn truncated"},
    "tool_call":         {action: actionState, state: "working", desc: "tool call"},
    "permission_request": {action: actionState, state: "blocked", desc: "waiting permission"},
    "ask_user_question":  {action: actionState, state: "blocked", desc: "asking user"},
    "error":             {action: actionState, state: "idle", desc: "error"},
}

// Handle 是 hooks.Dispatcher 的 callback
// 接收事件名和 JSON payload，通过 Unix socket 上报到 Herdr
func (h *HerdrHandler) Handle(ctx context.Context, event string, payload []byte) {
    emap, ok := eventActions[event]
    if !ok {
        return
    }

    // 解析 payload 获取 session_id
    var data struct {
        SessionID string `json:"session_id,omitempty"`
    }
    json.Unmarshal(payload, &data) // 忽略解析错误，可选字段

    // 构建请求
    req := h.buildRequest(emap.action, emap.state, data.SessionID)

    // 发送到 Herdr socket（异步执行）
    go h.send(req)
}

func (h *HerdrHandler) buildRequest(action herdrAction, state string, sessionID string) map[string]interface{} {
    id := fmt.Sprintf("tachi:%d:%06d", time.Now().UnixMilli(), rand.Intn(1_000_000))
    seq := time.Now().UnixNano()

    params := map[string]interface{}{
        "pane_id": h.paneID,
        "source":  h.source,
        "agent":   h.agent,
        "seq":     seq,
    }

    var method string
    switch action {
    case actionSession:
        method = "pane.report_agent_session"
        params["agent_session_id"] = sessionID
        params["session_start_source"] = "startup"
    case actionState:
        method = "pane.report_agent"
        params["state"] = state
        if sessionID != "" {
            params["agent_session_id"] = sessionID
        }
    }

    return map[string]interface{}{
        "id":     id,
        "method": method,
        "params": params,
    }
}

func (h *HerdrHandler) send(req map[string]interface{}) {
    conn, err := net.DialTimeout("unix", h.sockPath, 500*time.Millisecond)
    if err != nil {
        return // Herdr 不在运行，静默忽略
    }
    defer conn.Close()
    conn.SetDeadline(time.Now().Add(500 * time.Millisecond))

    _ = json.NewEncoder(conn).Encode(req)
    // 不等待响应，单向通知
}
```

### 5.3 自动注册

在 `AIAgent.Configure()` 中：

```go
// agent/agent_configure.go

// 初始化 hook dispatcher
a.hookDispatcher = hooks.NewDispatcher()

// 从 config.yaml 加载用户自定义命令 hooks
for event, cmds := range a.cfg.Hooks.Events {
    for _, cmd := range cmds {
        a.hookDispatcher.RegisterCommand(event, cmd)
    }
}

// 检测 Herdr 环境，自动注册内置回调
if hooks.DetectHerdr() {
    handler := hooks.NewHerdrHandler()
    for event := range hooks.EventActions {
        evt := event // capture
        a.hookDispatcher.RegisterCallback(evt, "herdr", func(ctx context.Context, e string, p []byte) {
            handler.Handle(ctx, e, p)
        })
    }
    a.logger.Info(ctx, "Herdr integration enabled (auto-detected)")
}
```

### 5.4 事件上报策略

| Tachi 事件 | Herdr 方法 | state | 说明 |
|---|---|---|---|
| `session_start` | `pane.report_agent_session` | — | 上报 session ID，Herdr 可做 session resume |
| `tool_call` | `pane.report_agent` | `working` | LLM 开始执行工具时标记 working |
| `permission_request` | `pane.report_agent` | `blocked` | 等待用户确认时标记 blocked |
| `ask_user_question` | `pane.report_agent` | `blocked` | LLM 向用户提问时标记 blocked |
| `turn_complete` | `pane.report_agent` | `idle` | turn 完成回到 idle |
| `turn_truncated` | `pane.report_agent` | `working` | 截断续写时标记 working |
| `error` | `pane.report_agent` | `idle` | 错误后回到 idle |

不在 `turn_start` 上报 `working`——因为 turn_start 时 LLM 还没开始真正做事（正在等待第一个 token），屏幕检测可能还在 idle。由实际 `tool_call` 触发 working 更准确。

### 5.5 Herdr config 配置

`config.yaml` 中新增 `herdr` 字段，仅控制开关：

```yaml
herdr:
  # 自动检测 HERDR_ENV，默认 true
  enabled: true
  # Socket 路径和 Pane ID 始终从环境变量 HERDR_SOCKET_PATH / HERDR_PANE_ID 读取
```

---

## 6. 实现计划

### 6.1 文件变更清单

#### 新增文件

| 文件 | 内容 |
|---|---|
| `agent/hooks/dispatcher.go` | Dispatcher + Handler 注册、Dispatch 方法 |
| `agent/hooks/hook.go` | HookCommand 模型、配置加载、模板展开 |
| `agent/hooks/herdr.go` | HerdrHandler（Go 回调，直接 socket 通信） |

#### 修改文件

| 文件 | 变更 |
|---|---|
| `config/config.go` | 新增 `HooksConfig`、`HerdrConfig` 结构体 |
| `config/config.go` Config struct | 增加 `Hooks HooksConfig`、`Herdr HerdrConfig` 字段 |
| `agent/agent.go` AIAgent struct | 增加 `hookDispatcher *hooks.Dispatcher` 字段 |
| `agent/agent_configure.go` | 初始化 HookDispatcher，检测 Herdr 环境 |
| `agent/agent_loop.go` | 在 `RunConversationStream`、`handleStopFinish`、`handleLengthFinish`、`handleToolCallFinish` 等关键点插入 Dispatch 调用 |
| `agent/tool_executor.go` | 在 `executeToolCallsSequential`、`executeToolCallsParallel` 中插入 `tool_call`/`tool_result` 事件 |
| `agent/tools/ask_user.go` | 在 `AskUserQuestion` 工具执行前后插入事件 |
| `agent/agent.go` Confirm 方法 | 在 `PermissionRequest` 前后插入事件 |

### 6.2 实现顺序

1. **Phase A: Hook 基础设施**（`agent/hooks/` 包）
   - Dispatcher 核心：Dispatch(event, payload)
   - 配置加载与模板展开
   - 命令执行（sync/async + timeout）

2. **Phase B: Agent Loop 集成**
   - 在关键位置插入 Dispatch 调用
   - 测试所有事件路径

3. **Phase C: Herdr 集成**
   - `agent/hooks/herdr.go` — Go 回调 + Unix socket 直连
   - 自动检测 HERDR_ENV + 注册回调
   - Herdr 侧写 tachi.toml 检测 manifest

4. **Phase D: Session Resume**
   - 可选：支持 Herdr 通过环境变量传递 session ID，实现 `tachi --resume` 自动恢复

---

## 7. 边界情况

### 7.1 Pipe 模式 (`tachi -p`)
- Hook 系统在 pipe 模式下同样生效
- 但 `HERDR_ENV` 在 pipe 模式下几乎不可能为 1（没有终端），所以 Herdr 集成自然禁用
- 用户仍可配置自定义 hook 在 pipe 模式下执行

### 7.2 Channel 模式
- Channel 模式下 Tachi 运行在后台，没有 TUI 状态机
- Hook 系统不受影响，事件直接从 agent loop 分发
- Herdr 集成在 channel 模式下无意义（没有 Herdr pane）

### 7.3 Sub-agent
- Sub-agent 创建时继承 parent 的 hook 配置，但 `HERDR_ENV` 不会传递到 sub-agent 进程
- Sub-agent 有独立的 session ID，如果确实需要上报可以额外配置

### 7.4 并发安全
- Hook dispatch 使用 `sync.Map` 或 `sync.RWMutex` 保护配置读取
- 异步 hook 使用独立 goroutine，不共享状态
- 同步 hook 通过 context timeout 防止死锁

### 7.5 Hook 失败不影响主流程
- Hook 命令失败（非零退出、超时、panic）不中断 agent loop
- 错误记录到 debug 日志，不传播到用户
- `async: false` 的 hook 超时时继续执行，不阻塞

---

## 8. 与 Herdr 的屏幕检测配合

Hook 系统上报状态到 Herdr socket 后，Herdr 的响应方式：

1. **侧边栏状态更新**：Herdr 根据 `pane.report_agent` 的 state 更新 sidebar
2. **状态聚合**：blocked → tab 高亮 → workspace rollup
3. **Agent wait 触发**：`herdr agent wait tachi --until blocked` 在收到 blocked 状态后返回
4. **Session restore**：Herdr 记录 `agent_session_id`，重启后可以 `tachi --resume <id>`

屏幕检测 Manifest（Herdr 侧）作为辅助：
- 当 hook 上报不可靠时（如 Herdr 启动时 Tachi 已经在运行，来不及注册 hook）
- 定义在 `src/detect/manifests/tachi.toml`，交由 Herdr 项目维护

---

## 9. 后续扩展

- **Webhook hooks**：除了执行命令，还可以内置 HTTP webhook 支持（不依赖 bash curl）
- **Go 插件 hooks**：未来可注册 Go 函数作为 hook handler，用于内部集成
- **事件日志**：记录所有 hook 执行历史用于调试
- **安装第三方 hooks**：类似 `herdr integration install` 的机制来管理 Tachi hook 包
