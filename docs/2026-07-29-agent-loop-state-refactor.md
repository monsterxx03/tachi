# Agent Loop 状态管理重构

日期：2026-07-29（2026-07-30 定稿；同日 review 修订：补 one-off RunState 语义、
recorder RunOption 化、Step 5.5 runParams 改造、Step 5/6 调用点清单补全；
2026-07-30 review-2 修订：sharedMCP 派生值 + fork 迁移、oneoffRec 过渡策略、
mode 锁迁移、manager turn-active 标记、race 测试、budget 两入口）
状态：方案确认
关联：`2026-07-28-agent-loop-refactor.md`（循环体可读性重构）

本文档是实施规格：按「迁移步骤」顺序执行，每步保持 `go test ./agent/...` 绿色，
验收标准见文末。

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

turnState (9 fields, sync.RWMutex)
  ├── messages, inputTokens, breakdown
  ├── start, traceID, pendingImages
  ├── compactEstimate, lastMessageDate
  └── mu

loopState (4 fields, 无锁)
  ├── messages         ← 和 turnState.messages 重复，defer 同步
  ├── apiCalls, lengthRetries, budget
```

`messages` 在两处存在（`agent_loop.go` 中 `defer func() { a.turn.setMessages(ls.messages) }()`）。

---

## 完整字段清单

当前 `AIAgent` 的 42 个字段（按 struct 声明顺序分组）：

```go
// 1-5: 核心配置
provider              llm.Provider              // LLM 提供商
maxIterations         int                       // 迭代预算上限
toolRegistry          *tools.Registry           // 工具注册表
permissionMode        PermissionMode            // 权限模式
permissionPolicy      *permission.Policy        // bash 权限策略 (allow/ask/deny)

// 6-8: 通信通道
confirmRespCh         chan ConfirmResponse      // TUI/ACP → agent: 确认响应（构造一次）
askUserRespCh         chan tools.AskUserResult  // TUI → agent: 用户输入响应（构造一次）
steerRespCh           chan string               // TUI → agent: steer 输入（每轮重建，loop 退出时置 nil）

// 9-11: 权限运行时
permissionHandler     PermissionHandler         // 外部权限处理器 (PermissionModeExternal)
autoApprovePolicyAsks bool                      // 用户点了"全部允许"后设 true
autoApproveEdits      bool                      // 用户点了"编辑全部允许"后设 true

// 12-17: 管理器
sessionManager        SessionManager            // 会话持久化
reminderCollector     ReminderCollector          // 系统提醒收集器
contextWindow         int64                     // 上下文窗口 token 上限
mcpManager            *mcp.Manager              // MCP 连接池 + ToolSearch
processManager        *tools.ProcessManager     // 后台进程管理 (Bash 启动的进程)
lspManager            *lsp.LSPManager           // LSP 诊断

// 18-23: 专用模型
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

// 33-34: 技能
skillStore            *skill.Store              // 技能存储
activeSkills          map[string]bool           // 当前 session 激活的技能 (运行时可变)

// 35: MCP 异步初始化输出
mcpInitErrors         []error                   // MCP 异步连接错误 (TUI 状态栏用)

// 36-37: One-off 转录
oneoffRec       *oneoffRecorder   // 一次性运行的实时记录器（运行中被 recordSession 读取）
lastOneoffPath  string            // 最近关闭的一次性转录文件路径（TUI 读取）

// 38: 钩子系统
hookDispatcher  *hooks.Dispatcher // 事件钩子分发器，由 Configure() 初始化

// 39: 会话模式
mode            string            // "auto" / "chat" / "plan"，控制工具可见性

// 40: 压缩策略
compactStrategy CompactStrategy   // 自动压缩摘要生成器（测试时可 mock）

// 41-42: 运行状态
turn            *turnState        // 每轮状态 (带 mutex)
skipSessionWrites bool            // 跳过 session 持久化（一次性任务，RunOneOffStream 设置/恢复）
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
| `ACPFileMode`、`PlanToolEnabled`                                                       | ✅ 已有（构造输入，派生 FrontendConfig）     |
| `AutoApproveEdits`、`AutoApprovePolicyAsks`                                            | ✅ 已有（构造输入，初始化 PermState）        |
| `ProcessManager`                                                                       | ✅ 已有                                      |
| `SharedMCP`（`*mcp.Manager`）                                                          | ✅ 已有（更名为 `MCPManager`，见下）         |
| `TitleGenEnabled`（`*bool`）                                                           | ✅ 已有（构造输入）                          |
| `SkipMemoryRecall`                                                                     | ✅ 已有（保留）                              |
| `SystemConfig`（含 FullConfig）                                                        | ✅ 已有                                      |

**"构造输入"字段的语义**：`TitleProvider`、`CommitProvider`、`ReviewProvider`、
`RunProvider`、`SubagentProvider`、`TitleGenEnabled`、`AutoApproveEdits`、
`AutoApprovePolicyAsks`、`ACPFileMode`、`PlanToolEnabled` 在 Config 中作为
**可选构造输入**存在——调用方在 `NewAIAgentWithConfig` 时传入，构造过程中被消费：
专用 Provider 为 nil 时由 `SetupTitleProvider()` 等从 `FullConfig` 解析出实际值，
写入 AIAgent 的运行时字段（`titleModelProvider` 等）；auto-approve 用于初始化
`PermState`；ACP/PlanTool 用于派生 `FrontendConfig`。
这些字段是**输入**而非运行时只读配置，构造完成后不再读取。

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
    HookDispatcher   *hooks.Dispatcher     // 由 Configure() 初始化（构造期回填）

    // 压缩策略（测试时可注入 mock；nil = 默认 llmCompactStrategy）
    CompactStrategy  CompactStrategy
```

**SharedMCP 的处理**：现有 `SharedMCP *mcp.Manager` 字段更名为 `MCPManager`，
语义不变（构造输入；nil = 由 Configure 创建并回填）。"Close 时是否销毁 manager"
不需要独立的 bool 字段——由**构造时 `MCPManager` 是否为 nil** 推导：
`NewAIAgentWithConfig` 在 Configure 前记录 `mcpOwned = (cfg.MCPManager == nil)`
（unexported 派生值，供 `Close()` 判断；见「AIAgent 最终结构」）。
AIAgent 上的 `sharedMCP bool` 字段删除。

**Fork 路径对齐**：`ForkConfig`（`agent_fork.go`）当前持有 `sharedMCP *mcp.Manager`
字段，fork 时透传父 agent 的 `mcpManager` 给子（`agent_fork.go:115`）。删除
`AIAgent.sharedMCP` 后，fork 创建的子 agent 同样不拥有 manager——`ForkConfig`
改设 `mcpOwned = false`（manager 由父持有，子 agent `Close()` 不销毁）。
`ForkConfig` 的 `sharedMCP` 字段更名/语义调整在 Step 1 一并处理。

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

    // 功能开关（构造输入 / 只读配置）
    ACPFileMode           bool   // 构造输入 → 派生 FrontendConfig
    PlanToolEnabled       bool   // 构造输入 → 派生 FrontendConfig
    AutoApproveEdits      bool   // 构造输入 → 初始化 PermState
    AutoApprovePolicyAsks bool   // 构造输入 → 初始化 PermState
    SkipMemoryRecall      bool   // 只读配置

    // 权限策略
    PermissionMode   PermissionMode
    PermissionPolicy *permission.Policy  // 新增

    // 工具与基础设施
    ToolRegistry     *tools.Registry       // 新增
    SessionManager   SessionManager        // 新增
    ReminderCollector ReminderCollector     // 新增
    MCPManager       *mcp.Manager          // 由 SharedMCP 更名；Configure 回填
    ProcessManager   *tools.ProcessManager
    LSPManager       *lsp.LSPManager       // 新增
    SubagentRunner   tools.SubagentRunner  // 新增
    SkillStore       *skill.Store          // 新增
    Memory           *MemoryState          // 新增
    Logger           *logger.Logger

    // 系统配置
    SystemConfig AgentSystemConfig
    FullConfig   *config.Config

    // 钩子系统（由 Configure 初始化，构造期回填）
    HookDispatcher *hooks.Dispatcher

    // 压缩策略（测试可 mock）
    CompactStrategy CompactStrategy
}
```

**注意**：`DeferredToolReminder` 和 `SkillListReminder` 不进 Config。
它们是 `ReminderCollector` 内部提醒实例的指针，暂留 AIAgent（见 §6）；
后续由 ReminderCollector 提供访问方法后再移除。

### 2. `FrontendConfig` — 前端模式配置（纯只读）

与 `AgentConfig` 同级，按前端类型（TUI / ACP / channel）在构造时设一次。
全部字段构造后不变。

```go
type FrontendConfig struct {
    ACPFileMode     bool    // ← a.acpFileMode
    PlanToolEnabled bool    // ← a.planToolEnabled
}
```

- 不含 `Mode`：运行时经 `SetMode()` 改变，留在 AIAgent 一级字段（见 §6）。
- 不含 `SharedMCP`：它是生命周期所有权标志，由构造时 `MCPManager` 是否注入推导（见 §1）。

### 3. `RuntimeChannels` — 通信通道（agent 生命周期，只读）

```go
type RuntimeChannels struct {
    ConfirmResp chan ConfirmResponse      // ← a.confirmRespCh（NewAIAgent 创建，缓冲 1）
    AskUserResp chan tools.AskUserResult  // ← a.askUserRespCh（NewAIAgent 创建，缓冲 1）
}
```

只收**随 agent 生命周期**的 channel：这两个在 `NewAIAgent` 一次性创建、永不重建。
`ConfirmTool()`、`RespondToAskUser()` 保留为 AIAgent 方法（封装写入逻辑），
内部操作 `a.Channels.XXX`。

steer channel 不在其中：它是 per-run 状态（TUI/channel 每轮重建，
loop 退出时置 nil），通过 `RunOption` 传入（见 §5）。

### 4. `PermissionState` — 运行时可变权限

这三个字段在 session 中途被用户操作改变，且工具执行路径会**写**
（`tool_executor.go` 在 ConfirmAllowAlways 时置 `autoApproveEdits = true`），
因此独立于只读的 `AgentConfig`：

```go
type PermissionState struct {
    PermissionHandler     PermissionHandler  // ← a.permissionHandler
    AutoApprovePolicyAsks bool               // ← a.autoApprovePolicyAsks
    AutoApproveEdits      bool               // ← a.autoApproveEdits
}
```

`AIAgent` 保持现有 Setter 方法（`SetPermissionHandler`、`SetAutoApproveEdits` 等），
内部改为操作 `a.PermState.XXX`，保留 Setter 扩展点（日志、校验、事件通知）。
初始值由 `AgentConfig.AutoApprove*` 构造输入在 `NewAIAgentWithConfig` 时注入。

### 5. `RunState` — 实时运行状态（取代 loopState + turnState）

`turnState` 的 `sync.RWMutex` 是必需的：channel 模式下同一 agent 被 turn
goroutine 写入、slash-command handler（如 `/usage`）在 **turn 之间**并发读取
token 估算与 breakdown（该路径曾发生真实 data race，见 `turn_state.go` 注释）；
TUI 状态栏也在 loop 运行中并发读取。`RunState` 继承这个职责：
loop 写、外部通过 Getter 并发读。

```go
type RunState struct {
    mu               sync.RWMutex    // 并发读保护（见下文字段分类）

    // ── 并发读写（loop 写，slash-command/TUI 读）──
    Messages        []llm.Message    // 完整消息历史（loop 追加，结束时可读）
    InputTokens     int64                    // ← turnState.inputTokens
    TokenBreakdown  tokenbreakdown.Breakdown // ← turnState.breakdown
    StartTime       time.Time                // ← turnState.start
    TraceID         string                   // ← turnState.traceID
    CompactEstimate int64                    // ← turnState.compactEstimate
    LastMessageDate string                   // ← turnState.lastMessageDate

    // ── 仅 loop goroutine 访问（无需锁，归属此处仅为聚合）──
    APICalls        int              // ← loopState.apiCalls
    LengthRetries   int              // ← loopState.lengthRetries
    Budget          *IterationBudget // ← loopState.budget

    // ── per-run 标志与资源 ──
    SkipSessionWrites bool             // ← a.skipSessionWrites
    OneoffRec         *oneoffRecorder // ← a.oneoffRec
}
```

**字段处理说明**：

- **`pendingImages` 字段删除**。它有两个写入路径：run 开始前（TUI/ACP/channel
  三处，紧邻 `RunConversationStream` 调用）和 run 进行中（TUI steer 带图，
  `tui/model_events.go`）。后者目前无人消费——`takePendingImages` 唯一调用点
  在 run 开始时，`applySteer` 构造纯文本 `RoleSteer` 消息，steer 带的图会被
  静默挂到下一轮首条消息或丢弃。本次重构修复此问题：
  - run 开始时的图片 → `WithPendingImages` RunOption：

    ```go
    func WithPendingImages(imgs []llm.ContentPart) RunOption {
        return func(p *runParams) { p.pendingImages = imgs }
    }
    ```

  - steer 带图 → steer 通道类型升级为结构体：

    ```go
    type SteerInput struct {
        Text   string
        Images []llm.ContentPart
    }

    func WithSteerChannel(ch chan SteerInput) RunOption {
        return func(p *runParams) { p.steerCh = ch }
    }
    ```

    `applySteer` 改为从 `runParams.steerCh` 读取，把 `Images` 附到 steer 消息的
    `ContentParts` 上。steer channel 生命周期完全交还前端（每轮新建、
    TurnComplete 后弃用），loop 不再置 nil，`a.steerRespCh` 字段删除。
  - `SetPendingImages` / `SetSteerChannel` 公开 API 删除，TUI/ACP/channel
    四个调用点迁移（见迁移步骤 Step 6）。

- **`OneoffRec` 归属 RunState**：`recordSession` 在 run 进行中读取 recorder
  （`agent.go`），channel ambient 目前通过 `AttachOneOffRecorder` 在
  `RunOneOffStream` 之外从外部挂载（`channel/manager/ambient.go`）——
  它是 per-run 资源。重构后 recorder 生命周期完全并入 run：外部挂载改为
  `WithOneOffRecorder` RunOption，recorder 在 rs 初始化时创建、loop 结束时
  关闭，`AttachOneOffRecorder` / `DetachOneOffRecorder` 公开 API 删除
  （机制详见下方「one-off 运行的 RunState 语义」，迁移见 Step 6）。

- **`SkipSessionWrites` 归属 RunState**：当前 `RunOneOffStream` 直接改 agent
  字段再 defer 恢复（共享可变状态）；per-run 化后该问题消除。one-off 的
  rs 不发布到 `currentRun`，规则见下方「one-off 运行的 RunState 语义」。

**`recordSession` 等 helper 的 rs 获取**：`recordSession`、`ensureSessionAndRecordUser`、
`applySteer`、`injectLoopReminders` 等 loop 内部 helper 全部改为**显式接收
`*RunState` 参数**（调用点均在 run 内，rs 可达），不经 `currentRun` 中转。
注意 `recordSession` 要读 `rs.OneoffRec` / `rs.SkipSessionWrites`，其 12 个
调用点中 4 个在 `tool_executor.go`（`executeToolCalls` / `Parallel` /
`Sequential`，当前签名不含 loopState）——rs 需穿透这三层（见 Step 5）。

Getter 方法保留（`LastInputEstimate()`、`LastInputEstimateWithBreakdown()`、
`GetLastMessages()`、`trace()`、`elapsed()`），内部改为：在 `a.mu` 下读
`currentRun`（nil 时返回零值），再在 `rs.mu` 下读字段。外部调用方不变。

**完成信号契约**：

- `runLoop` 只返回 `<-chan AgentEvent`
- loop 结束时 close eventCh；**rs 的所有写入 happen-before eventCh 关闭**，
  消费方 drain eventCh 后即可安全读 rs（与现有 `GetLastMessages` 契约一致）

**currentRun 生命周期**（与现有 turnState 语义对齐）：

```
状态         AIAgent.currentRun    含义
空闲(首次)   nil                   尚无 run；Getter 返回零值
运行中       *RunState             loop 写，TUI/channel 并发读
结束后       *RunState             指针保留，直到下一次 run 覆盖
```

结束后不要置 nil——channel 模式 turn 间的 `/usage` 读取依赖保留语义。

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

    in := &runInput{Messages: messages, Opts: opts, Params: applyRunOptions(ropts)}
    return a.runLoop(ctx, rs, in)
}
```

注：`Budget` 构造移到调用方。`IterationBudget.Parent` 目前全库无人赋值
（dormant 机制），当下无碍；若未来启用子代理预算共享，需改为从
ctx/RunOption 取 parent 再构造。

#### one-off 运行的 RunState 语义（不发布 currentRun）

**规则：`currentRun` 只代表主会话运行；one-off 的 rs 是局部变量，跑完即弃。**

- `RunConversationStream`：创建 rs → 发布 `a.currentRun = rs` → `runLoop`
- `RunOneOffStream`：创建 rs（`SkipSessionWrites: true`）→ **不发布** → `runLoop`

配套变化：

- **删除 `savedTokens` 保存/恢复**（`agent_loop.go` 中 RunOneOffStream 的
  `savedTokens := a.turn.tokens()` + defer 恢复）——one-off 不再碰主会话
  状态，这道防线失去意义。
- **行为变化（修复而非回归）**：现状下 one-off 会覆盖 `turn.messages` 与
  token 估算（`defer setMessages` 在共享的 `runAgentLoop` 里，savedTokens
  只救了 tokens 没救 messages）；新设计下 `GetLastMessages` /
  `LastInputEstimate` 对 one-off 完全免疫。
- `turn.begin/trace/elapsed` 全部变为 rs 字段，helper 经显式 rs 参数访问。

**one-off 转录（recorder）生命周期并入 run**：

- **创建**：run goroutine 内、rs 初始化时（首次 `recordSession` 之前），
  若 meta 非空则走现有 `startOneoffRecorder` 逻辑创建 recorder 填
  `rs.OneoffRec`（`resolveOneoffSessionID` 改读 `a.Config.SessionManager`）。
  `RunOneOffStream` 已显式收 `meta OneOffMeta` 参数，直接填入；
  `RunConversationStream` 路径（channel ambient）经 `WithOneOffRecorder`
  RunOption 传入（runParams 定义见「runLoop 方法签名」，迁移见 Step 6）。
- **关闭**：loop goroutine 内 defer，close recorder → 写 `a.lastOneoffPath`
  → 日志改用 `rs.TraceID`（替代 `a.turn.trace()`）。关闭 happen-before
  eventCh close，TUI 在 TurnComplete 后读 `LastOneoffTranscriptPath()`
  的时序契约不变。
- 删除 `AttachOneOffRecorder` / `DetachOneOffRecorder` 公开 API。
- 顺带收益：ambient 目前在 attach 时即创建转录文件，若随后在 ctx 检查处
  bail 需靠 defer detach 清理空 recorder；recorder 随 run 创建后，
  不 run 就不存在，该清理路径消失。

### 6. 留在 AIAgent 的字段

| 字段                   | 原因                                          | 目标                                    |
| :--------------------- | :-------------------------------------------- | :-------------------------------------- |
| `mode`                 | 运行时经 `SetMode()` 可变                     | AIAgent 一级字段（`a.mu` 保护）         |
| `titleModelProvider`   | 构造结果（Setup 解析后只读）                  | AIAgent 一级字段                        |
| `titleGenEnabled`      | 构造结果（同上）                              | AIAgent 一级字段                        |
| `activeSkills`         | session 级运行时状态                          | 暂留；**不**移入 SessionManager（持久化接口不应承载运行时状态） |
| `deferredToolReminder` | ReminderCollector 内部指针                    | ⏳ ReminderCollector 加 Getter 后移除   |
| `skillListReminder`    | 同上                                          | ⏳ 同上                                 |
| `lastOneoffPath`       | TUI 查询用（run 结束后读取最近转录路径）      | 暂留，未来可走事件                      |
| `mcpInitErrors`        | 时序原因：MCP 异步初始化，manager 未就绪时已需可读 | 暂留；未来若 MCP 初始化同步化，可评估 `MCPManager.InitErrors()` Getter |

---

## AIAgent 最终结构

```go
type AIAgent struct {
    Config       AgentConfig         // 只读配置（含基础设施句柄；Configure 期回填）
    Frontend     FrontendConfig      // 前端模式（纯只读）
    Channels     RuntimeChannels     // 通信通道（agent 生命周期，只读）
    PermState    *PermissionState    // 可变权限（session 级）

    // 运行时可变字段（直接在 AIAgent 上）
    mode string                     // "auto" / "chat" / "plan"

    // 构造结果（Setup 解析后只读）
    titleModelProvider    llm.Provider
    titleGenEnabled       bool

    // ⏳ 暂留（见 §6）
    activeSkills          map[string]bool
    deferredToolReminder  *systemreminder.DeferredToolReminder
    skillListReminder     *systemreminder.SkillListReminder
    lastOneoffPath        string
    mcpInitErrors         []error
    mcpOwned              bool      // Configure 前由 cfg.MCPManager==nil 设定；Close 据此决定是否销毁

    currentRun  *RunState           // 当前运行的实时状态（loop 写，外部并发读）
    mu          sync.RWMutex        // 保护 currentRun + mode
}
```

字段从 42 个平铺 → **4 个子结构 + 运行时可变字段 + 暂留/构造结果 + 1 个运行状态**：

| 分组                     | 字段数 | 生命周期              |
| :----------------------- | :----: | :-------------------- |
| `AgentConfig`            |  ~30   | 只读（部分构造输入/回填）|
| `FrontendConfig`         |   2    | 纯只读                |
| `RuntimeChannels`        |   2    | 只读                  |
| `PermissionState`        |   3    | 可变                  |
| `mode` + 构造结果        |   3    | 可变 / 构造后只读     |
| `RunState`               |  ~13   | 实时运行（mutex 保护）|
| 暂留                     |   5    | 逐步迁移              |

### 字段映射总表

| 原字段                  | 新位置                                       |    生命周期    |
| :---------------------- | :------------------------------------------- | :------------: |
| `provider`              | `Config.Provider`                            |      只读      |
| `maxIterations`         | `Config.MaxIterations`                       |      只读      |
| `toolRegistry`          | `Config.ToolRegistry`                        |      只读      |
| `permissionMode`        | `Config.PermissionMode`                      |      只读      |
| `permissionPolicy`      | `Config.PermissionPolicy`                    |      只读      |
| `confirmRespCh`         | `Channels.ConfirmResp`                       |      只读      |
| `askUserRespCh`         | `Channels.AskUserResp`                       |      只读      |
| `steerRespCh`           | **删除** → `WithSteerChannel` RunOption      |    每 run      |
| `permissionHandler`     | `PermState.PermissionHandler`                |      可变      |
| `autoApprovePolicyAsks` | `PermState.AutoApprovePolicyAsks`（Config 同名构造输入） | 可变 |
| `autoApproveEdits`      | `PermState.AutoApproveEdits`（Config 同名构造输入）      | 可变 |
| `sessionManager`        | `Config.SessionManager`                      |      只读      |
| `reminderCollector`     | `Config.ReminderCollector`                   |      只读      |
| `contextWindow`         | `Config.ContextWindow`                       |      只读      |
| `titleModelProvider`    | AIAgent 一级字段（构造结果）                 |   构造后只读   |
| `titleGenEnabled`       | AIAgent 一级字段（构造结果）                 |   构造后只读   |
| `commitProvider`        | `Config.CommitProvider`                      |      只读      |
| `reviewProvider`        | `Config.ReviewProvider`                      |      只读      |
| `runProvider`           | `Config.RunProvider`                         |      只读      |
| `subagentProvider`      | `Config.SubagentProvider`                    |      只读      |
| `logger`                | `Config.Logger`                              |      只读      |
| `acpFileMode`           | `Frontend.ACPFileMode`（Config 同名构造输入）|      只读      |
| `planToolEnabled`       | `Frontend.PlanToolEnabled`（Config 同名构造输入） |   只读      |
| `sharedMCP`             | **删除** → `mcpOwned bool`（由构造时 `Config.MCPManager==nil` 推导） | 只读（构造期） |
| `skillStore`            | `Config.SkillStore`                          |      只读      |
| `activeSkills`          | AIAgent 暂留                                 |  session 级可变 |
| `subagentRunner`        | `Config.SubagentRunner`                      |      只读      |
| `memory`                | `Config.Memory`                              |      只读      |
| `cfg`                   | `Config.FullConfig`                          |      只读      |
| `deferredToolReminder`  | AIAgent 暂留 ⏳                              | 待移入 ReminderCollector |
| `skillListReminder`     | AIAgent 暂留 ⏳                              | 待移入 ReminderCollector |
| `mcpManager`            | `Config.MCPManager`                          |      只读      |
| `mcpInitErrors`         | AIAgent 暂留（时序原因）                     |    异步填充    |
| `processManager`        | `Config.ProcessManager`                      |      只读      |
| `lspManager`            | `Config.LSPManager`                          |      只读      |
| `turn`                  | **删除** → 被 `RunState` 替代                |       —        |
| `skipSessionWrites`     | `RunState.SkipSessionWrites`                 |     每 run     |
| `hookDispatcher`        | `Config.HookDispatcher`（Configure 回填）    |      只读      |
| `oneoffRec`             | `RunState.OneoffRec`                         |     每 run     |
| `lastOneoffPath`        | AIAgent 暂留                                 |     待迁移     |
| `mode`                  | AIAgent 一级字段                             |      可变      |
| `compactStrategy`       | `Config.CompactStrategy`                     |      只读      |

（42 个原字段；`SkipMemoryRecall` 本就在 AgentConfig 上，不涉及迁移。）

### runLoop 方法签名

`runLoop` 保留为 `AIAgent` 的方法，不抽自由函数——循环体的静态依赖
（`Config`/`PermState`/`Channels`/schema 过滤）若全部列为参数，签名会退化成
"AIAgent 减去可变字段"的复制。静态依赖经 receiver 访问，
签名只携带 per-run 的状态与输入：

**`runParams` 与 `RunOption` 类型改造**：现有 `RunOption` 定义为
`func(*toolView)`（`toolview.go`），需改为以 `runParams` 为目标
（`toolView` 内嵌），承载新增的 per-run 输入：

```go
// runParams 是一次运行已解析的 RunOption 集合。
type runParams struct {
    toolView                          // 工具可见性（现有 WithToolSet/WithNoTools/WithExtraTools）
    pendingImages []llm.ContentPart   // run 开始时附到首条用户消息的图片
    steerCh       chan SteerInput     // steer 输入（nil = 前端不支持 steer）
    oneoffMeta    *OneOffMeta         // one-off 转录（nil = 不录制）
}

type RunOption func(*runParams)

func applyRunOptions(ropts []RunOption) *runParams { ... }
```

得益于 embedding，`WithToolSet`/`WithNoTools`/`WithExtraTools` 的闭包
签名从 `func(v *toolView)` 改为 `func(p *runParams)` 后函数体不变；
两个入口的对外签名 `...RunOption` 不变，调用方零改动；内部唯一应用点
`buildToolView(ropts)` 改为 `applyRunOptions`，toolView 部分经
`params.toolView` 传给 `withToolView`。

```go
// runInput 是一次运行的输入：消息、调用选项、已解析的 RunOption。
type runInput struct {
    Messages []llm.Message
    Opts     llm.ChatOptions
    Params   *runParams  // 已解析的 RunOption（toolView、pendingImages、steerCh、oneoffMeta）
}

func (a *AIAgent) runLoop(
    ctx context.Context,
    rs *RunState,   // 运行状态（含 SkipSessionWrites、OneoffRec、Budget）
    in *runInput,
) <-chan AgentEvent
```

`runLoop` 不再创建 `RunState`，由调用方传入已初始化的 `*RunState`。
完成信号：loop 结束即 close 返回的 eventCh；rs 的所有写入 happen-before close。

**分层约定**：runLoop 及其 helper 只经 `a.Config` / `a.PermState` /
`a.Channels` / `a.Frontend` 访问静态依赖，不直接读写 `activeSkills`、
`mcpInitErrors` 等暂留字段。schema 过滤仍由 `a.filterActiveSchemas` 方法承担。

---

## 测试模式

重构后的测试经 `newTestAgent` 构造最小 agent（与现有 `agent_loop_test.go`
的做法一致），通过公开 API 驱动：

### 测 steer

```go
func TestSteerInjection(t *testing.T) {
    a := newTestAgent(t, &mockStreamProvider{...}) // 第一次返回 tool_call，第二次返回 stop
    steerCh := make(chan SteerInput, 1)

    ch := a.RunConversationStream(ctx, history, "hi", "sys",
        llm.ChatOptions{}, WithSteerChannel(steerCh))

    steerCh <- SteerInput{Text: "继续写代码"}
    for range ch { // drain 至 close
    }

    // steer 注入发生在 tool-call 边界、之后 loop 继续，
    // 断言"历史中存在 RoleSteer 消息"而非"最后一条是 steer"
    msgs := a.GetLastMessages()
    assert.True(t, slices.ContainsFunc(msgs,
        func(m llm.Message) bool { return m.Role == llm.RoleSteer }))
}
```

### 测 auto-approve

```go
func TestAutoApproveEdits(t *testing.T) {
    a, _, err := NewAIAgentWithConfig(ctx, AgentConfig{...})
    require.NoError(t, err)
    a.SetAutoApproveEdits(true)  // Setter 保留，内部操作 a.PermState

    ch := a.RunConversationStream(ctx, history, msg, sysPrompt, opts)
    // ...
}
```

### 测并发读（race）

`RunState.mu` 存在的全部理由是 channel 模式下 turn goroutine 写、
slash-command 读曾发生真实 data race（见 `turn_state.go` 顶部注释）。
重构后需在 `go test -race` 下验证：

```go
func TestRunStateConcurrentRead(t *testing.T) {
    a := newTestAgent(t, &mockStreamProvider{...}) // tool_call → stop

    ch := a.RunConversationStream(ctx, history, "hi", "sys", llm.ChatOptions{})

    // loop 运行中从另一 goroutine 并发读（模拟 channel 模式 /usage）
    var wg sync.WaitGroup
    wg.Add(1)
    go func() {
        defer wg.Done()
        _ = a.LastInputEstimate()
        _ = a.GetLastMessages()
    }()
    for range ch {}
    wg.Wait()
}
```

---

## 迁移步骤

所有步骤保持 `go test ./agent/...` 绿色。

### Step 1: 补全 `AgentConfig`

- 在现有 `agent/agent_config.go` 的 `AgentConfig` 上补字段：
  `PermissionPolicy`、`ToolRegistry`、`SessionManager`、`ReminderCollector`、
  `LSPManager`、`SubagentRunner`、`SkillStore`、`Memory`、`HookDispatcher`、
  `CompactStrategy`；`SharedMCP` 更名为 `MCPManager`
- `SkipMemoryRecall` 保留不动
- `AutoApprove*`、`ACPFileMode`、`PlanToolEnabled` 标注为构造输入
- shared 推导：`NewAIAgentWithConfig` 在 Configure 前记录 `MCPManager != nil`
- `AIAgent` 持有 `Config AgentConfig`，现有引用 `a.provider` → `a.Config.Provider`
- **纯搬字段，机械替换**

### Step 2: 提取 `FrontendConfig`（与 Config 同级）

- 在 `agent/frontend.go` 定义 `FrontendConfig`（`ACPFileMode`、`PlanToolEnabled`）
- `AIAgent` 持有 `Frontend FrontendConfig`，由构造输入派生
- `a.acpFileMode` → `a.Frontend.ACPFileMode`

### Step 3: 提取 `RuntimeChannels`

- 在 `agent/channels.go` 定义 `RuntimeChannels`（仅 `ConfirmResp`、`AskUserResp`）
- `AIAgent` 持有 `Channels RuntimeChannels`，`NewAIAgent` 中创建（缓冲 1）
- `ConfirmTool()`、`RespondToAskUser()` 内部改为操作 `a.Channels.XXX`
- steerRespCh 暂留原处，Step 6 处理

### Step 4: 提取 `PermissionState`

- 在 `agent/permission.go` 定义 `PermissionState`
- `AIAgent` 持有 `PermState *PermissionState`，初始值来自 Config 构造输入
- 现有 Setter 方法保留，内部改为 `a.PermState.XXX = v`
- `tool_executor.go` 等读取点改为 `a.PermState.XXX`

### Step 5: 合并 `RunState`（取代 loopState + turnState）

- 在 `agent/run_state.go` 定义 `RunState`
- 包含 `loopState` + `turnState` 字段（保留 `mu sync.RWMutex`），
  **`SkipSessionWrites`、`OneoffRec` 暂不纳入**（推迟到 Step 6，见下方过渡策略）；
  不含 `pendingImages`（Step 6 删除）
- `turnState` 的 Getter 方法移至 `RunState`
- `recordSession`、`ensureSessionAndRecordUser`、`applySteer`、
  `injectLoopReminders` 等 helper 改为显式接收 `*RunState`
- **`recordSession` 的 rs 穿透**：12 个调用点中 4 个在 `tool_executor.go`
  （`executeToolCalls` / `Parallel` / `Sequential`，当前签名不含
  loopState）——rs 需穿透这三层，调用方（runLoop）一并改。
  本步 `recordSession` 内部继续读 `a.oneoffRec` / `a.skipSessionWrites`
  （Step 6 切换到 `rs.OneoffRec` / `rs.SkipSessionWrites`）
- **one-off 的 `currentRun` 语义**（规则见 §5「one-off 运行的 RunState
  语义」）：`RunConversationStream` 创建并发布 rs；`RunOneOffStream`
  的 rs 仅作局部变量。`SkipSessionWrites`、recorder 的迁移推迟到 Step 6
  （本步 `RunOneOffStream` 继续用 `a.skipSessionWrites` /
  `a.oneoffRec`，`savedTokens` 保存/恢复暂保留）
- AIAgent Getter（`LastInputEstimate` 等）改经 `currentRun`，nil 返回零值
- 删除 `turnState` 和 `loopState`

不再需要 defer 同步 messages——`loopState.messages` 和 `turnState.messages`
统一为 `RunState.Messages`，loop 直接追加，外部通过 Getter 加锁读取。

> **过渡策略（review-2）**：`SkipSessionWrites` 与 `OneoffRec` 的 RunState
> 迁移整体推迟到 Step 6（与 recorder RunOption 化一起搬），避免本步出现
> 「`recordSession` 改读 `rs.OneoffRec` 但 ambient 仍经 `AttachOneOffRecorder`
> 写 `a.oneoffRec`」的无人消费窗口。Step 5 期间 `recordSession` 继续读
> `a.oneoffRec` / `a.skipSessionWrites`，Step 6 一次性切换到 `rs.*` 并删除旧字段。

### Step 5.5: `runParams` 类型改造（RunOption 换目标类型）

- 定义 `runParams`（内嵌 `toolView`，字段见「runLoop 方法签名」），
  `RunOption` 改为 `func(*runParams)`，新增 `applyRunOptions`
- `WithToolSet`/`WithNoTools`/`WithExtraTools` 闭包签名机械替换
  （`func(v *toolView)` → `func(p *runParams)`，函数体不变）
- `runAgentLoop` 内 `buildToolView(ropts)` 调用点改为 `applyRunOptions`，
  toolView 部分经 `params.toolView` 传给 `withToolView`
- 对外 `...RunOption` 签名不变，调用方零改动；本步是纯机械重构，
  为 Step 6 的 `WithSteerChannel`/`WithPendingImages`/`WithOneOffRecorder`
  打地基

### Step 6: steer 通道、图片输入与 recorder 的 RunOption 化

- 定义 `SteerInput{Text, Images}` 与 `WithSteerChannel` / `WithPendingImages` /
  `WithOneOffRecorder` RunOption
- `applySteer` 改从 `runParams.steerCh` 读取，图片附到 steer 消息 `ContentParts`
  （同时修复 steer 带图无人消费的问题）
- run 开始图片改从 `runParams.pendingImages` 消费
- ambient 迁移：`AttachOneOffRecorder` + `SetSteerChannel` 两个 setup 调用
  改为 `RunConversationStream(..., WithOneOffRecorder(meta), WithSteerChannel(steerCh))`，
  删除 `defer DetachOneOffRecorder`（recorder 由 loop 关闭）；
  删除 `AttachOneOffRecorder` / `DetachOneOffRecorder` 公开 API
- 删除 `SetSteerChannel` / `SetPendingImages` / `a.steerRespCh` /
  `turnState.pendingImages` 及 loop 退出时的置 nil defer
- 调用点迁移（`chan string` → `chan SteerInput` 类型联动）：
  `tui/commands.go`、`tui/model_events.go`（steer 带图改发 `SteerInput`）、
  `tui/model.go`（steerRespCh 字段类型）、`agent/acp/agent.go`、
  `channel/manager/agent_turn.go`、`channel/manager/ambient.go`、
  `channel/manager/manager.go`（turnAgent.steerRespCh 字段类型）、
  `channel/manager/events.go`（drainEvents 读 + 发送 steer）
- 注意：channel 模式把 `ta.steerRespCh != nil` 当 turn-active 标记
  （`agent_turn.go` 注释明确要求调用方检查）——标记保留在 manager 侧，
  不随 `a.steerRespCh` 删除
- **manager turn-active 标记落地**（review-2）：manager 侧新增显式标记字段
  （如 `turnActive bool`，或复用 `ambientCancel` 非 nil），替换所有
  `ta.steerRespCh != nil` 比较点（`ambient.go:83/182/207/222`、
  `agent_turn.go:170/222/449`、`events.go:122`）
- **SkipSessionWrites / OneoffRec 迁入 RunState**（从 Step 5 推迟）：
  `RunOneOffStream` 的 rs 构造时设 `SkipSessionWrites: true`，不再改
  `a.skipSessionWrites`；删除 `savedTokens` 保存/恢复（one-off 不再碰
  主会话状态）；recorder 创建/关闭并入 run（`RunOneOffStream` 用 meta
  参数在 rs 初始化时创建、loop defer 关闭，写 `a.lastOneoffPath`，日志用
  `rs.TraceID`）；ambient 路径经 `WithOneOffRecorder` RunOption 传入；
  `recordSession` 等 helper 改读 `rs.OneoffRec` / `rs.SkipSessionWrites`；
  删除 `a.oneoffRec` / `a.skipSessionWrites` 字段
- **`mode` 加锁**（review-2）：`modes.go` 的 `Mode()` / `SetMode()` 改经
  `a.mu` 读写（与 `currentRun` 共用），`filterActiveSchemas`
  （`agent_loop.go:967`）加 RLock。当前 `mode` 无任何锁，ACP `SetMode`
  与 loop `filterActiveSchemas` 可能并发
- **Budget 两入口**（review-2）：`IterationBudget` 构造移到调用方后，
  `RunConversationStream` 与 `RunOneOffStream` 两处均需
  `Budget: NewIterationBudget(a.Config.MaxIterations)`；
  `Budget.Parent` 当前全库无人赋值（dormant），未来启用子代理预算共享时
  需改从 ctx/RunOption 取 parent

### Step 7: `runLoop` 方法化改造

- `agent/loop.go`：`runAgentLoop` 改造为 `runLoop(ctx, rs, in)`（仍是 AIAgent 方法）
- `messages` / `opts` / ropts 解析收敛为 `runInput`
- 循环体内 `ls.xxx` → `rs.xxx`（Step 5 已完成 turnState 侧），
  `a.skipSessionWrites` / `a.oneoffRec` → `rs.xxx`
- 只返回 eventCh，close 即完成信号
- `runAgentLoop` 删除，`RunConversationStream` / `RunOneOffStream` 直接调 `runLoop`

### Step 8: 清理旧代码

- 删除 `turnState`、`loopState`、`steerRespCh`、`sharedMCP` 旧代码
- 删除 `AIAgent` 上已搬走的字段
- 重写旧测试用例

---

## 实施范围

各 Step 的大致改动面（文件数仅供相对规模参考）：

| Step | 内容                                             | 文件改动 |
| :--- | :----------------------------------------------- | :------: |
| 1    | AgentConfig 补全                                 |    ~3    |
| 2    | FrontendConfig 提取                              |    ~2    |
| 3    | RuntimeChannels 提取                             |    ~3    |
| 4    | PermissionState 提取                             |    ~3    |
| 5    | RunState 合并（含 one-off 语义、executor 穿透）  |    ~9    |
| 5.5  | runParams 类型改造（机械）                       |    ~2    |
| 6    | steer/图片/recorder RunOption 化（3 个前端联动） |    ~9    |
| 7    | runLoop 方法化改造                               |    ~3    |
| 8    | 清理旧代码 + 测试                                |    ~5    |

每步 `go test ./agent/...` 保持绿色。

---

## 验收标准

- `make build && make test && make lint` 全绿
- `go test -race ./agent/...` 通过（验证 `RunState.mu` 并发读保护）
- `AIAgent` 字段与「AIAgent 最终结构」一致；`turnState`、`loopState`、
  `steerRespCh`、`sharedMCP`、`pendingImages` 不复存在；
  `SetSteerChannel` / `SetPendingImages` / `AttachOneOffRecorder` /
  `DetachOneOffRecorder` 公开 API 删除
- 三端行为验证：
  - TUI：steer 文本注入正常；steer 带图正确附到 steer 消息（Anthropic +
    OpenAI 各实测一次，确认图片到达 LLM）；状态栏 token 估算正常；
    一次 run 结束后、下一次开始前状态栏读数保持（currentRun 保留语义）
  - channel：turn 间 `/usage` 可读；ambient one-off 运行不写主 session、
    sidecar 转录正常生成；ambient 在 ctx 取消处提前 bail 时不残留空转录文件
  - ACP：确认/AskUser 交互正常；带图消息正常
- one-off 运行（`/commit`、`/review`、dream）不产生主 session 写入
- **one-off 不发布 `currentRun` 的回归测试**：`/commit` 跑完后主会话的
  `GetLastMessages()` 与 `LastInputEstimate()` 保持运行前的值
