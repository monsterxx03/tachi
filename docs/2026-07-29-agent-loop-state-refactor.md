# Agent Loop 状态管理重构

日期：2026-07-29
状态：方案确认
关联：`2026-07-28-agent-loop-refactor.md`（循环体可读性重构）

## 背景

`2026-07-28-agent-loop-refactor.md` 解决了循环体内部的代码组织问题。本文聚焦另一个正交的问题：**状态管理**。

### 现状：AIAgent 42 个字段

当前 `AIAgent` 持有 42 个字段，状态散落在三个容器里：

```
AIAgent (42 fields)
  ├── 配置类 (provider, toolRegistry, permissionMode, ...)
  ├── Channel (confirmRespCh, askUserRespCh, steerRespCh)
  ├── Manager (sessionManager, mcpManager, lspManager, ...)
  ├── 可变权限 (autoApprovePolicyAsks, autoApproveEdits)
  ├── Flag (acpFileMode, planToolEnabled, sharedMCP, ...)
  ├── 回调/引用 (subagentRunner, deferredToolReminder, ...)
  └── turn (*turnState)

turnState (10 fields, sync.RWMutex)
  ├── messages, inputTokens, breakdown
  ├── start, traceID, pendingImages
  ├── compactEstimate, lastMessageDate
  └── mu

loopState (4 fields, 无锁)
  ├── messages         ← 和 turnState.messages 重复，defer 同步
  ├── apiCalls, lengthRetries, budget
```

`messages` 在两处存在。注释自述 "State lives at three levels"、"grab-bag"、"No claim that every field belongs here"。

---

## 完整字段清单

当前 `AIAgent` 的 42 个字段逐一列出：

```go
// 1-5: 核心配置
provider              llm.Provider              // LLM 提供商
maxIterations         int                       // 迭代预算上限
toolRegistry          *tools.Registry           // 工具注册表
permissionMode        PermissionMode            // 权限模式
permissionPolicy      *permission.Policy        // bash 权限策略 (allow/ask/deny)

// 6-8: 通信通道
confirmRespCh         chan ConfirmResponse      // TUI/ACP → agent: 确认响应
askUserRespCh         chan tools.AskUserResult  // TUI → agent: 用户输入响应
steerRespCh           chan string               // TUI → agent: steer 输入

// 9-11: 权限运行时
permissionHandler     PermissionHandler         // 外部权限处理器 (PermissionModeExternal)
autoApprovePolicyAsks bool                      // 用户点了"全部允许"后设 true
autoApproveEdits      bool                      // 用户点了"编辑全部允许"后设 true

// 12-18: 管理器
sessionManager        SessionManager            // 会话持久化
reminderCollector     ReminderCollector          // 系统提醒收集器
contextWindow         int64                     // 上下文窗口 token 上限
mcpManager            *mcp.Manager              // MCP 连接池 + ToolSearch
processManager        *tools.ProcessManager     // 后台进程管理 (Bash 启动的进程)
lspManager            *lsp.LSPManager           // LSP 诊断

// 19-23: 专用模型
titleModelProvider    llm.Provider              // 标题生成专用模型 (nil = 用主模型)
titleGenEnabled       bool                      // LLM 标题生成开关
commitProvider        llm.Provider              // /commit 专用模型 (nil = 用主模型)
reviewProvider        llm.Provider              // /review 专用模型 (nil = 用主模型)
runProvider           llm.Provider              // -p run 模式专用模型 (nil = 用主模型)
subagentProvider      llm.Provider              // 子代理专用模型 (nil = 用主模型)

// 24-26: 标志位
acpFileMode           bool                      // ACP 文件 I/O 模式 (EditFile 走 conn.WriteTextFile)
planToolEnabled       bool                      // 启用 SavePlan 工具 (仅 ACP)
sharedMCP             bool                      // mcpManager 由外部注入，Close 时不销毁

// 27-30: 基础设施引用
logger                *logger.Logger            // 日志器
cfg                   *config.Config            // 全局配置 (给 RegisterTools 等用)
subagentRunner        tools.SubagentRunner      // 子代理执行回调
memory                *MemoryState              // 记忆子系统 (nil = 禁用)

// 31-32: 提醒引用 (指向 reminderCollector 内的具体提醒实例)
deferredToolReminder  *systemreminder.DeferredToolReminder
skillListReminder     *systemreminder.SkillListReminder

// 33: 技能
activeSkills          map[string]bool           // 当前 session 激活的技能 (运行时可变)

// 34: MCP 异步初始化输出
mcpInitErrors         []error                   // MCP 异步连接错误 (TUI 状态栏用)

// 35-36: One-off 转录
oneoffRec       *oneoffRecorder   // 一次性运行的实时记录器（每个 RunOneOffStream 设一次）
lastOneoffPath  string            // 最近关闭的一次性转录文件路径（TUI 读取）

// 37: 钩子系统
hookDispatcher  *hooks.Dispatcher // 事件钩子分发器，由 Configure() 初始化

// 38: 会话模式
mode            string            // "auto" / "chat" / "plan"，控制工具可见性

// 39: 压缩策略
compactStrategy CompactStrategy   // 自动压缩摘要生成器（测试时可 mock）

// 40-42: ⬇ 已有
turn            *turnState        // 每轮状态 (带 mutex)
skipSessionWrites bool            // 跳过 session 持久化（一次性任务）
activeSkills    map[string]bool   // 当前 session 激活的技能
```

---

## 目标结构

### 1. `AgentConfig` — 构造后只读（已存在，需补全）

`agent/agent_config.go` 中已有 `AgentConfig` 结构体，用于 `NewAIAgentWithConfig`。
它已有大部分配置字段，但与 AIAgent 上的字段还有 **缺口**：

**已有字段：**

| 字段                                                                                   | 状态                                         |
| :------------------------------------------------------------------------------------- | :------------------------------------------- |
| `Provider`、`ContextWindow`、`MaxIterations`、`Logger`                                 | ✅ 已有                                      |
| `PermissionMode`                                                                       | ✅ 已有                                      |
| `TitleProvider`、`CommitProvider`、`ReviewProvider`、`RunProvider`、`SubagentProvider` | ✅ 已有（构造输入，见下方说明）             |
| `ACPFileMode`、`PlanToolEnabled`                                                       | ✅ 已有（它们应拆到 FrontendConfig，见下节） |
| `AutoApproveEdits`、`AutoApprovePolicyAsks`                                            | ✅ 已有（应拆到 PermissionState）            |
| `ProcessManager`                                                                       | ✅ 已有                                      |
| `SharedMCP`（`*mcp.Manager`）                                                          | ✅ 已有                                      |
| `TitleGenEnabled`                                                                      | ✅ 已有（构造输入）                          |
| `SystemConfig`（含 FullConfig）                                                        | ✅ 已有                                      |

**关于专用 Provider 字段**：`TitleProvider`、`CommitProvider`、`ReviewProvider`、
`RunProvider`、`SubagentProvider`、`TitleGenEnabled` 在 Config 中作为**可选构造输入**
存在——调用方可以在 `NewAIAgentWithConfig` 时传入 override。如果为 nil，
`SetupTitleProvider()` 等函数会在构造过程中从 `FullConfig` 解析出实际值，
写入 AIAgent 的运行时字段（`titleModelProvider` 等）。

因此，Config 中的这些字段是**输入**而非"运行时只读配置"。
字段映射表中对应的 AIAgent 运行时字段（`titleModelProvider`、`titleGenEnabled` 等）
**不映射到 Config**，而是保留在 AIAgent 上作为构造结果。详见字段映射表。

**缺少的字段（需从 AIAgent 搬入）：**

```go
    // 权限策略（当前在 a.permissionPolicy）
    PermissionPolicy *permission.Policy

    // 基础设施句柄（当前直存在 AIAgent 上）
    ToolRegistry     *tools.Registry
    SessionManager   SessionManager
    ReminderCollector ReminderCollector
    LSPManager       *lsp.LSPManager
    SubagentRunner   tools.SubagentRunner
    Memory           *MemoryState
    SkillStore       *skill.Store

    // 钩子系统
    HookDispatcher   *hooks.Dispatcher     // 由 Configure() 初始化

    // 压缩策略（测试时可注入 mock）
    CompactStrategy  CompactStrategy
```

**需要搬出的字段（从 AgentConfig 移至更合适的位置）：**

| 字段                                        | 目标                                               |
| :------------------------------------------ | :------------------------------------------------- |
| `ACPFileMode`、`PlanToolEnabled`            | `FrontendConfig`                                   |
| `AutoApproveEdits`、`AutoApprovePolicyAsks` | `PermissionState`                                  |
| `SharedMCP`                                 | 嵌入 `AgentConfig.MCPManager` 中，作为一个内部字段 |

调整后的 `AgentConfig`：

```go
type AgentConfig struct {
    Provider         llm.Provider
    MaxIterations    int
    ContextWindow    int64

    // 专用模型 override（构造输入；nil = 从 FullConfig 解析）
    TitleProvider    llm.Provider
    CommitProvider   llm.Provider
    ReviewProvider   llm.Provider
    RunProvider      llm.Provider
    SubagentProvider llm.Provider
    TitleGenEnabled  *bool

    // 权限策略
    PermissionMode   PermissionMode
    PermissionPolicy *permission.Policy  // 新增

    // 工具与基础设施
    ToolRegistry     *tools.Registry       // 新增
    SessionManager   SessionManager        // 新增
    ReminderCollector ReminderCollector     // 新增
    MCPManager       *mcp.Manager          // 替换 SharedMCP
    ProcessManager   *tools.ProcessManager
    LSPManager       *lsp.LSPManager       // 新增
    SubagentRunner   tools.SubagentRunner  // 新增
    SkillStore       *skill.Store          // 新增
    Memory           *MemoryState          // 新增
    Logger           *logger.Logger

    // 系统配置
    SystemConfig AgentSystemConfig
    FullConfig   *config.Config

    // 钩子系统（由 Configure 初始化）
    HookDispatcher *hooks.Dispatcher

    // 压缩策略（测试可 mock）
    CompactStrategy CompactStrategy

    // 不再包含（已搬出）：
    //   ACPFileMode, PlanToolEnabled       → FrontendConfig
    //   AutoApproveEdits, AutoApprovePolicyAsks → PermissionState
}
```

**注意**：`DeferredToolReminder` 和 `SkillListReminder` 不在 Config 中出现。
它们是 `ReminderCollector` 内部提醒实例的指针，当前直接暴露给 AIAgent 是为了
绕过 ReminderCollector 的 Getter 方法。这属于封装破损，修复方式是给
ReminderCollector 加访问方法，而不是把内部指针列为顶级配置。

### 3. `FrontendConfig` — 前端模式配置（纯只读）

与 `AgentConfig` 同级，按前端类型（TUI / ACP / channel）在构造时设一次。
全部字段构造后不变。

```go
type FrontendConfig struct {
    ACPFileMode     bool    // ← a.acpFileMode
    PlanToolEnabled bool    // ← a.planToolEnabled
    SharedMCP       bool    // ← a.sharedMCP
}
```

**不包含** `Mode`——它在运行时通过 `SetMode()` 改变，放在 `FrontendConfig`
会破坏（大部分）只读的契约。直接留在 AIAgent 上作为一级字段：

```go
type AIAgent struct {
    // ...
    mode string  // "auto" / "chat" / "plan"，运行时可变
}
```

### 4. `RuntimeChannels` — 通信通道

loop 与外部（TUI / ACP）通信的 channel。未使用的 channel 传 nil
（steer 禁用时 steerResp = nil）。

```go
type RuntimeChannels struct {
    ConfirmResp chan ConfirmResponse      // ← a.confirmRespCh
    AskUserResp chan tools.AskUserResult  // ← a.askUserRespCh
    SteerResp   chan string               // ← a.steerRespCh; nil = steer 禁用
}
```

`AIAgent` 和 `runLoop` 持有同一份引用（`*RuntimeChannels`）：

- TUI 通过 `agent.Channels.ConfirmResp <- AllowOnce` 写入
- loop 通过参数传入的同一份 channel 读取

`ConfirmTool()` 等方法保留为 AIAgent 的方法（封装写入逻辑），内部操作 `a.Channels.ConfirmResp`。

### 5. `PermissionState` — 运行时可变权限

这三个字段在 session 中途可能被用户操作改变，不能放 `AgentConfig`。

```go
type PermissionState struct {
    PermissionHandler     PermissionHandler  // ← a.permissionHandler
    AutoApprovePolicyAsks bool               // ← a.autoApprovePolicyAsks
    AutoApproveEdits      bool               // ← a.autoApproveEdits
}
```

`AIAgent` 保持现有的 Setter 方法（`SetPermissionHandler`、`SetAutoApproveEdits` 等），
内部改为操作 `a.PermState.XXX`。这样既能从组织结构上明确"这些是可变权限"，
又保留 Setter 的扩展点（日后加日志、校验、事件通知不用改调用方）。

### 6. `RunState` — 实时运行状态（取代 loopState + turnState，带并发读）

`turnState` 的 `sync.RWMutex` **不是设计缺陷**——它是必需的，因为 TUI 在 loop 运行中
会并发读取 token 估算、trace ID、耗时等信息（状态栏、日志）。

`RunState` 继承这个职责：loop 写、TUI 通过 Getter 并发读、结束时通过 channel 回传同一份引用。

```go
type RunState struct {
    mu               sync.RWMutex    // TUI 并发读取保护

    Messages        []llm.Message    // 完整消息历史（loop 追加，结束时可读）
    APICalls        int              // ← loopState.apiCalls
    LengthRetries   int              // ← loopState.lengthRetries
    Budget          *IterationBudget // ← loopState.budget

    InputTokens     int64                    // ← turnState.inputTokens
    TokenBreakdown  tokenbreakdown.Breakdown // ← turnState.breakdown
    StartTime       time.Time                // ← turnState.start
    TraceID         string                   // ← turnState.traceID
    CompactEstimate int64                    // ← turnState.compactEstimate
    LastMessageDate string                   // ← turnState.lastMessageDate
    SkipSessionWrites bool                   // ← a.skipSessionWrites
}
```

**不包含** `PendingImages`：它是 `runLoop` 的输入，不是运行状态。
通过已有的 `RunOption` 模式传入，不破坏 `RunConversationStream` 签名：

```go
// agent/agent.go
func WithPendingImages(imgs []llm.ContentPart) RunOption {
    return func(p *runParams) { p.pendingImages = imgs }
}
```

TUI 调用时：

```go
agent.RunConversationStream(ctx, history, text, sysPrompt, opts,
    agent.WithPendingImages(images))
```

`runLoop` 内部通过 `runParams` 读取。

Getter 方法保留（`LastInputEstimate()`, `GetLastMessages()`, `trace()`, `elapsed()`），
内部操作 `RunState` 的 mutex。外部调用方不变。

**Channel 契约**：

- `<-chan *RunState` **恰好发送一次后 close**
- 正常结束：发送最终的 `*RunState`
- context 提前取消：发送当前的 `*RunState`（含已累积消息），然后 close
- TUI 用 `<-resultCh` 接收一次即可

**currentRun 生命周期**：

```
状态         AIAgent.currentRun    含义
空闲         nil                   没有运行中的 loop
运行中       *RunState             loop 写，TUI 并发读
结束         *RunState             指针保留，直到下一次 RunConversationStream 覆盖
```

`AIAgent` 中管理 `currentRun`：

```go
func (a *AIAgent) RunConversationStream(...) <-chan AgentEvent {
    rs := &RunState{
        StartTime: time.Now(),
        TraceID:   logger.NewTraceID(),
        Budget:    NewIterationBudget(a.Config.MaxIterations),
    }
    a.mu.Lock()
    a.currentRun = rs
    a.mu.Unlock()

    eventCh, resultCh := runLoop(ctx, a.Config, a, &a.Channels, rs,
        messages, opts, ropts...)

    go func() {
        <-resultCh
        a.mu.Lock()
        a.currentRun = nil
        a.mu.Unlock()
    }()
    return eventCh
}
```

### 7. 已归类但留在 AIAgent 的字段

下列字段当前留在 AIAgent 上，原因是迁移成本高或涉及封装修复：

| 字段                   | 原因                       | 目标                                                    |
| :--------------------- | :------------------------- | :------------------------------------------------------ |
| `activeSkills`         | session 级技能状态         | ⏳ 移入 SessionManager                                  |
| `skillStore`           | 技能存储                   | ⏳ 移入 SessionManager                                  |
| `deferredToolReminder` | ReminderCollector 内部指针 | ⏳ ReminderCollector 加 Getter                          |
| `skillListReminder`    | ReminderCollector 内部指针 | ⏳ ReminderCollector 加 Getter                          |
| `oneoffRec`            | 每 run 生命周期            | ✅ 在 `RunOneOffStream` 中作为局部变量，不从 AIAgent 读 |
| `lastOneoffPath`       | TUI 查询用                 | ⏳ 暂留，未来可走事件或 Store                           |
| `mcpInitErrors`        | 时序原因（MCP 异步初始化） | ⏳ 暂留                                                 |
| `mode`                 | AIAgent 直接留用           | 可变                                                    |
| `hookDispatcher`       | 由 Configure() 初始化      | ⏳ 移入 AgentConfig                                     |
| `compactStrategy`      | 测试可 mock                | ✅ 移入 AgentConfig                                     |

---

## AIAgent 最终结构

```go
type AIAgent struct {
    Config       AgentConfig         // 只读配置（含基础设施句柄）
    Frontend     FrontendConfig      // 前端模式（纯只读）
    Channels     RuntimeChannels     // 通信通道（只读，可 nil）
    PermState    *PermissionState    // 可变权限（session 级）

    // 运行时可变字段（直接在 AIAgent 上）
    mode string                     // "auto" / "chat" / "plan"

    // ⏳ 待迁移 / 暂留（见 §7）
    activeSkills          map[string]bool
    skillStore            *skill.Store
    deferredToolReminder  *systemreminder.DeferredToolReminder
    skillListReminder     *systemreminder.SkillListReminder
    lastOneoffPath        string
    mcpInitErrors         []error        // 暂留（时序原因）
    titleModelProvider    llm.Provider   // 构造结果
    titleGenEnabled       bool           // 构造结果

    currentRun  *RunState           // 当前运行的实时状态（loop 写，TUI 读）
    mu          sync.RWMutex        // 保护 currentRun + mode
}
```

字段从 42 个平铺 → **4 个子结构 + 1 个可变字段 + 8 个暂留/结果 + 1 个运行状态**：

| 分组                     | 字段数 | 生命周期              |
| :----------------------- | :----: | :-------------------- |
| `AgentConfig`            |  ~22   | 只读（部分构造输入）  |
| `FrontendConfig`         |   3    | 纯只读                |
| `RuntimeChannels`        |   3    | 只读（可 nil）        |
| `PermissionState`        |   3    | 可变                  |
| `mode`（AIAgent 直接）   |   1    | 可变                  |
| `RunState`               |  ~10   | 实时运行（mutex 保护）|
| 暂留 / 构造结果          |   8    | 逐步迁移              |

**注**：`mcpInitErrors` 保留在 AIAgent 上，不移入 MCPManager。
原因是时序问题——MCP 异步初始化，`mcpInitErrors` 可能在 MCPManager 尚未就绪时
就需要被查询。作为一个简单的 `[]error` 字段，留在 AIAgent 上是最安全的做法。
未来如果 MCP 初始化改为同步，可以重新评估。
应由 `Config.MCPManager` 以 Getter 方法（如 `InitErrors() []error`）提供。
TUI 通过 `agent.Config.MCPManager.InitErrors()` 读取，不再经过 AIAgent。

### 字段映射总表

| 原字段                  | 新位置                            |         生命周期         |
| :---------------------- | :-------------------------------- | :----------------------: |
| `provider`              | `Config.Provider`                 |           只读           |
| `maxIterations`         | `Config.MaxIterations`            |           只读           |
| `toolRegistry`          | `Config.ToolRegistry`             |           只读           |
| `permissionMode`        | `Config.PermissionMode`           |           只读           |
| `permissionPolicy`      | `Config.PermissionPolicy`         |           只读           |
| `sessionManager`        | `Config.SessionManager`           |           只读           |
| `reminderCollector`     | `Config.ReminderCollector`        |           只读           |
| `contextWindow`         | `Config.ContextWindow`            |           只读           |
| `titleModelProvider`   | AIAgent 暂留（构造结果） | 构造后只读     |
| `titleGenEnabled`      | AIAgent 暂留（构造结果） | 构造后只读     |
| `commitProvider`        | `Config.CommitProvider`           |           只读           |
| `reviewProvider`        | `Config.ReviewProvider`           |           只读           |
| `runProvider`           | `Config.RunProvider`              |           只读           |
| `subagentProvider`      | `Config.SubagentProvider`         |           只读           |
| `logger`                | `Config.Logger`                   |           只读           |
| `cfg`                   | `Config.FullConfig`               |           只读           |
| `skillStore`            | `Config.SkillStore`               |           只读           |
| `memory`                | `Config.Memory`                   |           只读           |
| `acpFileMode`           | `Frontend.ACPFileMode`            |           只读           |
| `planToolEnabled`       | `Frontend.PlanToolEnabled`        |           只读           |
| `sharedMCP`             | `Frontend.SharedMCP`              |           只读           |
| `mcpManager`            | `Config.MCPManager`               |           只读           |
| `processManager`        | `Config.ProcessManager`           |           只读           |
| `lspManager`            | `Config.LSPManager`               |           只读           |
| `subagentRunner`        | `Config.SubagentRunner`           |           只读           |
| `deferredToolReminder`  | AIAgent 暂留 ⏳                   | 待移入 ReminderCollector |
| `skillListReminder`     | AIAgent 暂留 ⏳                   | 待移入 ReminderCollector |
| `confirmRespCh`         | `Channels.ConfirmResp`            |           只读           |
| `askUserRespCh`         | `Channels.AskUserResp`            |           只读           |
| `steerRespCh`           | `Channels.SteerResp`              |           只读           |
| `permissionHandler`     | `PermState.PermissionHandler`     |           可变           |
| `autoApprovePolicyAsks` | `PermState.AutoApprovePolicyAsks` |           可变           |
| `autoApproveEdits`      | `PermState.AutoApproveEdits`      |           可变           |
| `mcpInitErrors`         | AIAgent 暂留（时序原因）          |         异步填充         |
| `turn`                  | **删除** → 被 `RunState` 替代     |            —             |
| `skipSessionWrites`     | `RunState.SkipSessionWrites`      |          每 run          |
| `activeSkills`          | AIAgent 暂留 ⏳                   |          待迁移          |
| `skillStore`            | `Config.SkillStore`               |           只读           |
| `oneoffRec`             | `RunOneOffStream` 局部变量        |          每 run          |
| `lastOneoffPath`        | AIAgent 暂留 ⏳                   |          待迁移          |
| `mode`                  | `Frontend.Mode`（可运行时变更）   |           可变           |
| `hookDispatcher`        | ⏳ 移入 `Config.HookDispatcher`   |           只读           |
| `compactStrategy`       | ✅ 移入 `Config.CompactStrategy`  |           只读           |

### runLoop 函数签名

`runLoop` 不再创建 `RunState`，由调用方传入已初始化的 `*RunState`。
引入 `ToolFilter` 接口来封装 schema 过滤逻辑，避免 `runLoop` 直接依赖
`AgentConfig` 和 `FrontendConfig` 的跨结构耦合：

```go
type ToolFilter interface {
    FilterActiveSchemas(schemas []tools.Schema) []tools.Schema
}
```

`AIAgent` 实现它（搬移现有 `filterActiveSchemas` 方法体），
`runLoop` 只依赖接口：

```go
func runLoop(
    ctx context.Context,
    cfg AgentConfig,              // 模型、预算、session 管理器等
    filter ToolFilter,            // schema 过滤（由 AIAgent 实现）
    chans *RuntimeChannels,
    rs *RunState,
    messages []llm.Message,
    opts llm.ChatOptions,
    ropts ...RunOption,           // 含 WithPendingImages 等
) (<-chan AgentEvent, <-chan *RunState)
```

**好处**：
- `runLoop` 不关心 Mode、ToolRegistry、MCPManager 在哪个子结构里
- 测试可传 mock ToolFilter，无需构造 AgentConfig
- filter 逻辑本身可单独测试

`RunConversationStream` 调用时传入 `a`（自身就是 ToolFilter）：

```go
eventCh, resultCh := runLoop(ctx, a.Config, a, &a.Channels, rs,
    messages, opts, ropts...)
```

---

## 可测试性

### 重构后：测 steer

```go
func TestSteerInjection(t *testing.T) {
    steerCh := make(chan string, 1)

    eventCh, resultCh := runLoop(ctx,
        AgentConfig{
            Provider:     mockProvider,
            ToolRegistry: reg,
            Logger:       logger.NewNop(),
        },
        &RuntimeChannels{SteerResp: steerCh},
        initialMessages, opts, nil,
    )

    steerCh <- "继续写代码"

    result := <-resultCh
    assert.Equal(t, llm.RoleSteer, result.Messages[len(result.Messages)-1].Role)
}
```

不涉及：sessionManager、PermissionState、FrontendConfig、Memory、技能系统。

### 重构后：测 auto-approve

```go
func TestAutoApproveEdits(t *testing.T) {
    a := NewAIAgent(AgentConfig{...})
    a.SetAutoApproveEdits(true)  // Setter 保留，内部操作 a.PermState

    ch := a.RunConversationStream(ctx, history, msg, sysPrompt, opts)
    // ...
}
```

Setter 保留不意味着测试必须 mock 整个 AIAgent——测试仍然只需要配置关心的字段。

---

## 迁移步骤

所有步骤保持 `go test ./agent/...` 绿色。

### Step 1: 提取 `AgentConfig`

- 在 `agent/config.go` 定义 `AgentConfig`
- `AIAgent` 持有 `Config AgentConfig`
- 构造函数写入
- 现有引用 `a.provider` → `a.Config.Provider`
- **纯搬字段，机械替换**

### Step 2: 提取 `FrontendConfig`（与 Config 同级）

- 在 `agent/frontend.go` 定义 `FrontendConfig`
- `AIAgent` 持有 `Frontend FrontendConfig`（与 `Config` 同级）
- `a.acpFileMode` → `a.Frontend.ACPFileMode`
- 由前端（ACP/TUI/channel）在构造后按需设置

### Step 3: 提取 `RuntimeChannels`

- 在 `agent/channels.go` 定义 `RuntimeChannels`
- `AIAgent` 持有 `Channels RuntimeChannels`

### Step 4: 提取 `PermissionState`

- 在 `agent/permission.go` 定义 `PermissionState`
- `AIAgent` 持有 `PermState *PermissionState`
- 现有 Setter 方法保留，内部改为 `a.PermState.XXX = v`

### Step 5: 合并 `RunState`（取代 loopState + turnState）

- 在 `agent/run_state.go` 定义 `RunState`
- 包含 `loopState` + `turnState` 全部字段（保留 `mu sync.RWMutex`）
- 加上 `skipSessionWrites`
- `turnState` 的 Getter 方法（`tokens()`, `snapshotMessages()`, `traceID()`, `elapsed()` 等）
  移至 `RunState`，内部操作 `mu`
- 删除 `turnState` 和 `loopState`
- `turnState.pendingImages` → `runLoop` 参数 `pendingImages`

**不再需要 defer 同步 messages**——`loopState.messages` 和 `turnState.messages` 是
同一份 `RunState.Messages` 的引用，loop 直接追加，TUI 通过 Getter 加锁读取。

### Step 6: 提取 `runLoop` 纯函数

- `agent/loop.go` 写新函数
- 从 `runAgentLoop` 拷贝循环体
- 把 `a.xxx` 逐一改为 `cfg.xxx` / `chans.xxx` / `rs.xxx`
- 返回 `RunState` channel
- `AIAgent.runAgentLoop` 委托给 `runLoop`

### Step 7: 清理旧代码

- 删除 `turnState`、`loopState` 旧代码
- 删除 `AIAgent` 上已搬走的字段
- 重写旧测试用例

---

## 实施预算

| Step     | 内容                                        | 文件改动 |    预估     |
| :------- | :------------------------------------------ | :------: | :---------: |
| 1        | AgentConfig 提取（合并 Dependencies）       |    ~3    |   0.5 天    |
| 2        | FrontendConfig 提取                         |    ~2    |   0.2 天    |
| 3        | RuntimeChannels 提取                        |    ~3    |   0.3 天    |
| 4        | PermissionState 提取                        |    ~3    |   0.3 天    |
| 5        | RunState 合并（替换 loopState + turnState） |    ~6    |    1 天     |
| 6        | runLoop 纯函数化                            |    ~3    |   1.5 天    |
| 7        | 清理旧代码 + 测试                           |    ~5    |   0.5 天    |
| **合计** |                                             |          | **~4.3 天** |

每步 `go test ./agent/...` 保持绿色。
