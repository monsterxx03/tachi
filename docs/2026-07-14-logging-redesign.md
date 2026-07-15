# Tachi 日志系统重构设计文档

> 目标：构建分级、结构化、可按入口/渠道/链路追踪的日志系统。
> 新包 `pkg/logger/`，与现有 `pkg/debuglog/` 并存，逐步迁移。

---

## 目录

1. [现状与问题](#1-现状与问题)
2. [设计目标](#2-设计目标)
3. [新包设计：`pkg/logger`](#3-新包设计pkglogger)
4. [日志文件布局](#4-日志文件布局)
5. [日志级别](#5-日志级别)
6. [Trace ID](#6-trace-id)
7. [使用示例](#7-使用示例)
8. [迁移计划](#8-迁移计划)

---

## 一、现状与问题

### 当前实现

日志系统位于 `pkg/debuglog/`，基于 Go 标准库 `log/slog`：

- 单文件 `$BASEDIR/logs/debug.log`，10MB 轮转，保留 10 份
- 唯一结构化字段是 `source`（如 `source=tui`）
- 全部使用 `slog.Info` 级别，无分级
- 无 Trace ID，无法关联同一请求的日志
- 日志格式不统一（printf 风格 vs 结构化混用）

### 已知问题

详见 [2026-07-14-channel-feedback](#)。

1. **双 source 属性** — `DefaultLogger` 预设 `source=tui`，`WithSource()` 追加而非替换，导致 `source=tui source=channel:manager`
2. **入口混淆** — channel 模式的 agent 日志标为 `source=tui`，ACP 模式也用 `source=tui`
3. **无法按级别过滤** — 全部 INFO，DEBUG 级别日志（如 tool call 入参细节）和 ERROR 级别日志混在一起

---

## 二、设计目标

| 目标 | 说明 |
|------|------|
| **入口分离** | 按 TUI / Channel / ACP / Run 分文件或加固定标签 |
| **Channel 子分类** | channel 下按 discord / weixin / chrome 细分 |
| **分级日志** | DEBUG / INFO / WARN / ERROR，可配置最低显示级别 |
| **Trace ID** | 每个 agent turn 生成 trace_id，通过 context 传递 |
| **结构化字段** | slog 原生属性，组件统一添加关键字段 |
| **逐步迁移** | 新包不替换现有调用，可共存和渐进采用 |

---

## 三、新包设计：`pkg/logger`

### 包结构

```
pkg/logger/
├── logger.go       # Logger 核心类型
├── level.go        # 日志级别定义
├── fields.go       # 常用字段常量
├── trace.go        # Trace ID 生成与 context 传递
└── config.go       # 配置：目标文件、级别、轮转
```

### Logger 类型

```go
package logger

import (
    "context"
    "log/slog"
)

// Logger 包装 slog.Logger，提供命名空间和简化接口。
// 零值不可用，必须通过 New() 创建。
type Logger struct {
    slog *slog.Logger
    name string  // 如 "channel.discord"
}

// New 创建命名 Logger，name 格式为 "入口.子分类"。
//   logger.New("tui")
//   logger.New("channel.discord")
//   logger.New("channel.weixin")
//   logger.New("acp")
//   logger.New("run")
func New(name string) *Logger { ... }

// NewSub 创建子级 Logger，继承父级属性。
//   l := logger.New("channel")
//   dl := l.NewSub("discord")  // name = "channel.discord"
func (l *Logger) NewSub(name string) *Logger { ... }

// With 返回带附加属性的 Logger 副本。
func (l *Logger) With(attrs ...any) *Logger { ... }

// WithTrace 返回带 trace_id 的 Logger 副本。
func (l *Logger) WithTrace(traceID string) *Logger { ... }

// Debug / Info / Warn / Error
func (l *Logger) Debug(msg string, attrs ...any)
func (l *Logger) Info(msg string, attrs ...any)
func (l *Logger) Warn(msg string, attrs ...any)
func (l *Logger) Error(msg string, err error, attrs ...any)
```

### 设计要点

**命名空间即 source**：
- Logger 的 `name` 字段直接作为 `source` 属性输出
- 入口层负责创建根 Logger（`New("tui")`），子模块通过 `NewSub` 继承
- `source` 最终值如 `tui`、`channel.discord`、`acp`、`run`

**不预设全局 DefaultLogger**：
- 每个入口在 main.go 中创建根 Logger
- 通过 context 或显式传参向下传递
- 保留向后兼容的 `Default()` 函数用于未迁移的代码

---

## 四、日志文件布局

### 文件组织

每个入口对应独立日志文件，**各自独立轮转**。日志目录始终位于 `$BASEDIR/logs/`，其中 `$BASEDIR` 由 `--home` 命令行参数或默认的 `~/.tachi` 决定，**不在配置文件中指定**：

```
$BASEDIR/logs/
├── debug.log{.1,.2,...}   ← 默认日志（未指定入口的日志落这里）
├── tui.log{.1,.2,...}     ← TUI 模式
├── run.log{.1,.2,...}     ← CLI Run 模式（-p / 管道）
├── acp.log{.1,.2,...}     ← ACP 模式（JSON-RPC）
└── channel/
    ├── all.log{.1,.2,...} ← channel 全局日志
    ├── discord.log{...}   ← Discord 渠道
    ├── weixin.log{...}    ← 微信渠道
    └── chrome.log{...}    ← Chrome 扩展渠道
```

每个文件 10MB 轮转，保留 10 份，互不影响。WeChat 日志写满 10MB 轮转时不影响 Discord 日志。

### 为什么分开文件

- `tui.log` — 用户交互日志，高频但量小
- `run.log` — 单次执行，每次启动都是新 session
- `acp.log` — JSON-RPC 协议日志，结构化为主
- `channel/*.log` — 渠道日志分开，Discord/WeChat/Chrome 各看各的
  - **Troubleshooting 时**：WeChat 出问题了只看 `channel/weixin.log`，无需从单文件 grep

### 轮转策略

每个文件独立轮转，互不影响：

| 参数 | 值 | 说明 |
|------|-----|------|
| 单文件上限 | 10 MB | 写满后轮转 |
| 保留份数 | 10 | debug.log → debug.log.1 → ... → debug.log.9，最旧丢弃 |
| 触发方式 | 写入时检测 | 每次 Write() 检查当前文件大小，超过上限则轮转 |
| 检测粒度 | 按文件实例 | 每个 Logger 对应一个写入器，各自追踪文件大小 |

实现方式：`RotatingFileHandler` 结构体，每个实例管理一个文件的轮转。Logger 构造时传入目标文件路径，内部创建对应的 handler。

```go
// 每个 Logger 持有独立写入器
type RotatingFileHandler struct {
    dir      string
    baseName string
    maxSize  int64   // 默认 10MB
    maxFiles int     // 默认 10
    mu       sync.Mutex
    file     *os.File
    size     int64
}
```

### 配置

日志目录**不可配置**，始终为 `config.BaseDir() + "/logs/"`（由 `--home` 决定）。其他参数可通过 `config.yaml` 调整：

```yaml
# config.yaml（可选，不配置则所有日志合并到 debug.log）
logs:
  level: info                  # 默认最低级别
  max_size: 10mb               # 单个文件最大
  max_files: 10                # 保留文件数
  per_entry: true              # 是否按入口分文件（默认 true）
```

> **为什么 `dir` 不在配置文件中？**  
> Tachi 所有持久化数据（session、skills、MCP tokens、cron、日志等）统一存放在 `$BASEDIR` 下。  
> `--home` 是唯一的基础路径入口，日志目录不应独立于它存在。如果允许 `config.yaml` 指定  
> 另一个目录，会导致 `--home /custom/path` 时日志和其余数据分离，排查问题更困难。

默认 `per_entry: true` 时，所有日志按入口分文件写入。设为 `false` 时，所有日志仍写入 `debug.log`，保持向后兼容。

---

## 五、日志级别

| 级别 | 用法 | 例子 |
|------|------|------|
| `DEBUG` | 工具调用的入参/返回值、LLM raw response、频繁的内部事件 | `Debug("Tool call: Bash", "cmd", "make", "cwd", "/repo")` |
| `INFO` | 组件声明周期、Session 创建/切换/压缩、Provider 切换 | `Info("Session created", "session_id", sess.ID)` |
| `WARN` | 重试、降级、fallback、非致命错误 | `Warn("MCP server slow", "server", srv.Name, "dur", dur)` |
| `ERROR` | API 请求失败、连接断开、配置错误 | `Error("LLM request failed", err, "model", model)` |

### 输出格式

```
# Text（默认，人类可读）
2026-07-14T23:15:00.123+08:00 [INFO]  channel.discord connect=ready guilds=12  trace_id=turn_a1b2
2026-07-14T23:15:01.456+08:00 [DEBUG] channel.discord msg=received from=user123 content="hello" trace_id=turn_a1b2
2026-07-14T23:15:02.789+08:00 [ERROR] llm.api model=deepseek-chat err="rate limit" retry_after=30s trace_id=turn_a1b2

# JSON（可选，给工具消费）
{"time":"2026-07-14T23:15:00.123+08:00","level":"INFO","source":"channel.discord","msg":"connect=ready","guilds":12,"trace_id":"turn_a1b2"}
```

Text 格式主体是 `[LEVEL] source msg`，结构化字段放在后面。

---

## 六、Trace ID

### 生成时机

每个 agent turn 开始时生成：

```go
// agent_loop.go
traceID := logger.NewTraceID()  // 如 "turn_a1b2c3d4"
ctx := logger.WithTraceID(ctx, traceID)
```

### 传递方式

通过 `context.Context` 传递，`slog` 的 `slog.ContextHandler` 自动附加到每条日志：

```go
ctx := logger.WithTraceID(ctx, "turn_a1b2c3d4")
logger.FromContext(ctx).Info("Auto compact completed", "msgs_after", 10)
// → [INFO] agent.loop Auto compact completed msgs_after=10 trace_id=turn_a1b2c3d4
```

### 跨子协程传递

```go
// 子 agent 自动继承 trace_id
childCtx := logger.WithTraceID(ctx, logger.TraceIDFromContext(ctx))
```

### Trace ID 生命周期

```
用户消息 → 生成 trace_id → agent loop → tool calls → LLM calls → 回复完成 → trace_id 生命周期结束
                                 ↓                         ↓
                           Tool: Bash                  LLM: Chat Completion
                           trace_id=turn_a1b2           trace_id=turn_a1b2
```

用 `grep trace_id=turn_a1b2 channel/discord.log` 可回溯一次完整交互的所有日志。

### 消息尾部展示

Turn 结束时，trace ID 追加到消息尾部，与当前回合耗时/迭代数在同一行：

```go
// agent/agent_loop.go — FormatTurnSummary
func FormatTurnSummary(iterations int, duration time.Duration, traceID string) string {
    if iterations <= 0 && duration <= 0 && traceID == "" {
        return ""
    }
    parts := []string{}
    if iterations > 0 {
        parts = append(parts, fmt.Sprintf("%d 次迭代", iterations))
    }
    if duration > 0 {
        parts = append(parts, formatTurnDuration(duration))
    }
    if traceID != "" {
        parts = append(parts, fmt.Sprintf("trace: %s", traceID))
    }
    return fmt.Sprintf("\n\n*(回合: %s)*", strings.Join(parts, ", "))
}
```

输出示例：

```
🤖 这是 Tachi 的回复内容……

*(回合: 5 次迭代, 12.3s, trace: turn_a1b2c3d4)*
```

这样做的好处：
- **Channel 用户**：看到 trace ID 后可以直接在日志里 grep 定位完整交互链路
- **TUI 用户**：同样能看到 trace ID，便于跨端协作排查
- **不需要额外数据**：trace ID 本来就存在 context 里，只是多一个参数传递

### TraceID 格式

```
turn_<随机8字符hex>
```

示例：`turn_a1b2c3d4`、`turn_ef561234`

---

## 七、使用示例

### 入口初始化

```go
// main.go — TUI 入口
var log *logger.Logger

func runTUI(ctx context.Context, cmd *cli.Command) error {
    log = logger.New("tui")
    log.Info("TUI started", "version", Version)
    // ...
}

// main.go — Channel 入口
func runChannels(ctx context.Context, cmd *cli.Command) error {
    log := logger.New("channel")
    log.Info("Channel daemon starting")
    // ...
}

// main.go — ACP 入口
func runACPAgent(ctx context.Context) error {
    log := logger.New("acp")
    log.Info("ACP agent starting")
    // ...
}
```

### Channel 子 Logger

```go
// channel/discord/channel.go
func NewChannel(cfg DiscordConfig) (*DiscordChannel, error) {
    logger := logger.New("channel.discord")
    return &DiscordChannel{logger: logger, ...}
}

// channel/weixin/channel.go
func NewChannel(cfg config.WeixinConfig) (*Channel, error) {
    logger := logger.New("channel.weixin")
    return &Channel{logger: logger, ...}
}
```

### Agent 接收 Logger

```go
// channel/manager/agent_cache.go
func (m *Manager) buildAgent(...) (*agent.AIAgent, error) {
    a := agent.NewAIAgent(prov, 0)
    // Channel 模式下注入渠道子 logger
    agentLogger := m.logger.NewSub("agent")
    a.SetLogger(agentLogger)
    return a, nil
}
```

### 带 Trace 的日志

```go
// agent/agent_loop.go
func (a *AIAgent) runTurn(ctx context.Context, msg string) {
    traceID := logger.NewTraceID()
    ctx = logger.WithTraceID(ctx, traceID)

    l := logger.FromContext(ctx)
    l.Info("Turn started", "msg_len", len(msg))

    result, err := a.callLLM(ctx)
    if err != nil {
        l.Error("LLM call failed", err)
        return
    }
    l.Info("Turn completed", "output_len", len(result))
}
```

### 工具执行日志

```go
// agent/tool_executor.go
func (e *executor) execute(ctx context.Context, tc tools.Call) (tools.Result, error) {
    l := logger.FromContext(ctx)
    l.Debug("Tool start", "tool", tc.Function.Name, "args", tc.Arguments)

    result, err := tc.Function.Fn(ctx, tc.Arguments)
    dur := time.Since(start)

    if err != nil {
        l.Error("Tool failed", err, "tool", tc.Function.Name, "dur", dur)
    } else {
        l.Debug("Tool completed", "tool", tc.Function.Name, "dur", dur)
    }
    return result, err
}
```

### 从 Context 获取 Logger

```go
// 在已有 context 的代码中
l := logger.FromContext(ctx)
l.Info("Processing message", "from", userID)

// 在无法访问 context 的代码中
l := logger.Default()  // 兜底，输出到 debug.log
l.Warn("Fallback log", "reason", "no context")
```

---

## 八、迁移计划

### 阶段一：新包落地 ✅

1. 创建 `pkg/logger/` 包，实现核心类型
2. 在 `main.go` 各入口创建根 Logger
3. 验证：新日志独立写入，不与现有 debuglog 冲突

### 阶段二：关键路径迁移

按优先级逐个模块迁移：

| 优先级 | 模块 | 原因 |
|--------|------|------|
| 🔴 P0 | `channel/manager/` | 当前双 source 问题最严重 |
| 🔴 P0 | `agent/agent_loop.go` | Trace ID 从这里发起 |
| 🟡 P1 | `agent/tool_executor.go` | 工具日志带 trace 最有价值 |
| 🟡 P1 | `channel/discord/`, `channel/weixin/` | source 正确区分 |
| 🟢 P2 | `acp/` | ACP 日志较少 |
| 🟢 P2 | `cron/`, `dream/`, `memory/` | 低频模块 |
| ⚪ P3 | `agent/` 其余文件 | 逐步迁移 |

### 阶段三：清理

1. 所有模块迁移完成后，`pkg/debuglog/` 标记为 deprecated
2. 保留 `pkg/debuglog/` 的旋转写入器实现（`rotatingWriter` 可复用）
3. `pkg/logger/` 可内联或封装 `rotatingWriter`

### 兼容性

整个迁移过程中：
- 新旧两套日志**同时写入**，互不干扰
- `pkg/debuglog/` 的 `DefaultLogger` 和 `WithSource` 继续可用
- 迁移以模块为单位，一个模块内全部切换后再删除旧调用
- 用户配置不变，旧日志文件不受影响

---

## 附录：现有 debuglog 问题对照

| 问题 | debuglog | pkg/logger |
|------|----------|------------|
| source 重复 | DefaultLogger 预设 `source=tui` | name 即 source，无预设 |
| 无分级 | 全部 `slog.Info` | DEBUG / INFO / WARN / ERROR |
| 无 trace | 无 | Trace ID 通过 context 传递 |
| 单文件 | 全部写入 debug.log | 按入口分文件 |
| 格式 | `slog.TextHandler` 标准格式 | 自定义 `[LEVEL]` 前缀 + 结构化字段 |
| 初始化 | `Init()` 设置全局 DefaultLogger | `New()` 创建实例，无全局状态 |
