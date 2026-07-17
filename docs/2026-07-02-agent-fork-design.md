# Agent Fork 基建设计文档

> 版本: 1.0 | 日期: 2026-07-02 | 状态: 设计阶段

## 一、概述

`AIAgent.Fork()` 是从父 agent 派生受限子 agent 的通用基建。它将当前 SubAgent 内部手动创建 agent + 注册工具的逻辑抽取为 `AIAgent` 的一等公民方法，供所有需要隔离 agent turn 的场景复用。

**消费者**：
- `subagent/executor`（重构，用 Fork 替代手动创建）
- `channel/manager/ambient`（ambient turn 始终走 fork）
- 未来：dream、cron jobs 等

## 二、总体架构

```
AIAgent
  ├── Fork(ForkConfig) → ForkedAgent
  │     ├── 继承父 toolRegistry（按白名单过滤）
  │     ├── 共享父 MCP Manager（不重复连接）
  │     ├── 共享父 ProcessManager（不 kill）
  │     ├── 无 SessionManager（调用方自行设置或使用 RunOneOffStream）
  │     ├── 无 Memory（不读不写）
  │     └── 无 ReminderCollector
  │
  └── LoadSessionHistory() → []llm.Message
        └── 从 sessionManager 加载全部消息并转为 LLM 格式

ForkedAgent
  ├── Agent() *AIAgent        // 获取内部 agent
  └── Close()                  // 只清理自身，不 kill/close 共享资源
```

```
                         ┌──────────────────────────────┐
                         │        父 AIAgent             │
                         │                              │
                         │  toolRegistry (full)          │
                         │  mcpManager                   │
                         │  processManager               │
                         │  sessionManager (current)     │
                         └──────────────┬───────────────┘
                                        │ Fork(ForkConfig{
                                        │   AllowedTools: ["MemoryRecall"],
                                        │   MaxIterations: 5,
                                        │ })
                                        ▼
                         ┌──────────────────────────────┐
                         │        ForkedAgent            │
                         │                              │
                         │  agent *AIAgent               │
                         │    ├── toolRegistry (filtered)│
                         │    ├── mcpManager (shared)    │
                         │    ├── processManager (shared)│
                         │    ├── sessionManager: nil    │
                         │    └── memory: nil            │
                         │                              │
                         │  sharedPM  → parent.PM        │
                         │  sharedMCP → parent.MCP       │
                         └──────────────────────────────┘
```

## 三、核心设计决策

| 维度 | 决策 | 理由 |
|------|------|------|
| 触发方式 | `AIAgent.Fork(cfg)` 显式调用 | 通用基建，任何调用方可用 |
| 工具权限 | 白名单（`AllowedTools`），空 = 全部继承 | 灵活性与安全性兼顾 |
| MCP 工具 | 通过共享 MCP Manager 继承，白名单可过滤 | 不重复建立连接 |
| ProcessManager | 共享，Close 不 kill | 父 agent 统一管理进程生命周期 |
| LSP Manager | 不共享 | Fork agent 不做代码分析 |
| Memroy | 不启用 | Fork turn 是短暂任务，不应污染记忆 |
| Reminder | 不设置 | Fork agent 不需要提醒干扰 |
| Session | 不自动设置 | 调用方决定：ambient 用 RunOneOffStream，subagent 用 recorder |
| 迭代预算 | 通过 ForkConfig 显式设置 | 不同场景预算不同 |
| Close 行为 | 只清理自身，不杀共享进程/不断 MCP | 父 agent 统一控制生命周期 |
| 返回类型 | `*ForkedAgent`（包装类型） | Close 语义清晰，编译期保证 |

## 四、模块设计

### 4.1 ForkConfig — `agent/agent_fork.go`

```go
// ForkConfig 控制从父 agent 派生子 agent 的参数。
type ForkConfig struct {
    Provider      llm.Provider     // 必填 — LLM provider
    Model         string           // 必填 — 模型名
    MaxIterations int              // 0 = unlimited
    MaxTokens     int              // 0 = default (4096)
    AllowedTools  []string         // 空白名单 = 全部继承
    Logger        *debuglog.Logger // nil = 使用父 logger
    SessionID     string           // 日志标识
}
```

### 4.2 ForkedAgent — `agent/agent_fork.go`

```go
// ForkedAgent 是 Fork() 的返回值，包装受限子 agent。
type ForkedAgent struct {
    agent     *AIAgent
    sharedPM  *tools.ProcessManager // 指向父 agent 的 PM，Close 不 kill
    sharedMCP *mcp.Manager          // 指向父 agent 的 MCP，Close 不 close
}

// Agent 返回内部 AIAgent，供调用方设置 sessionManager / steer channel 等。
func (f *ForkedAgent) Agent() *AIAgent

// Close 清理自身资源。不 kill sharedPM，不 close sharedMCP。
// 安全调用多次。
func (f *ForkedAgent) Close()
```

实现细节：

```go
func (f *ForkedAgent) Close() {
    if f.agent == nil {
        return
    }
    // 只清理 toolRegistry —— 里面可能有 hashline SnapshotStore
    f.agent.ClearToolRegistry()
    f.agent = nil
}
```

### 4.3 Fork() — `agent/agent_fork.go`

```go
// Fork 从父 agent 派生一个受限子 agent。
func (a *AIAgent) Fork(cfg ForkConfig) *ForkedAgent
```

实现步骤：

```
1. child := NewAIAgent(cfg.Provider, cfg.Model, cfg.MaxIterations)
2. if cfg.Logger != nil → child.SetLogger(cfg.Logger)
3. 遍历 a.toolRegistry.GetToolNames()：
     if cfg.AllowedTools 为空 或 name ∈ cfg.AllowedTools：
       child.toolRegistry.Register(a.toolRegistry.GetTool(name))
4. child.SetSkipEditConfirm(true)
5. child.SetReminderCollector(nil)     // 无提醒
6. child.SetProcessManager(a.processManager)          // 共享 PM
7. child.SetSharedMCP(a.mcpManager)                  // 共享 MCP
8. return &ForkedAgent{
     agent:     child,
     sharedPM:  a.processManager,
     sharedMCP: a.mcpManager,
   }
```

注意：
- 如果 `a.mcpManager == nil`，`SetSharedMCP(nil)` 是安全的（内部处理）
- 不拷贝 `sessionManager`、`memory`、`lspManager`

### 4.4 LoadSessionHistory() — `agent/agent_fork.go`

```go
// LoadSessionHistory 从当前 session 加载全部消息并转为 LLM 格式。
// 无 session 或 session 为空时返回 (nil, nil)。
func (a *AIAgent) LoadSessionHistory() ([]llm.Message, error)
```

实现：

```
1. sm := a.SessionManager(); if sm == nil → return nil, nil
2. sess := sm.Current(); if sess == nil → return nil, nil
3. msgs, err := sm.LoadMessages(); if err → return err
4. return ConvertSessionToLLMMessages(msgs, sess.Provider)
```

`ConvertSessionToLLMMessages` 已存在于 `agent/` 包中（`session_convert.go`）。

## 五、消费者改造

### 5.1 重构 SubAgent — `agent/agent_subagent.go`

当前 `childAdapter.Run()` 手动创建 agent + 注册工具。改为：

```go
func (c *childAdapter) Run(ctx context.Context, provider llm.Provider,
    systemPrompt, userPrompt string, opts llm.ChatOptions,
) <-chan subagent.StreamEvent {
    out := make(chan subagent.StreamEvent, 64)
    go func() {
        defer close(out)

        forked := c.parent.Fork(ForkConfig{
            Provider:      c.childProvider,
            Model:         c.childModel,
            MaxIterations: c.maxIterations,
            AllowedTools:  c.allowedTools,
            Logger:        c.logger,
            SessionID:     c.sessionID,
        })
        defer forked.Close()

        child := forked.Agent()
        if opts.MaxTokens <= 0 {
            opts.MaxTokens = defaultMaxTokens
        }

        ch := child.RunOneOffStream(ctx, provider, systemPrompt, userPrompt, opts)
        for event := range ch {
            out <- translateEvent(event)
        }
    }()
    return out
}
```

### 5.2 Channel Ambient Turn — `channel/manager/ambient.go`

Ambient turn 始终走 fork 路径，不再有共享 session 的旧路径。

```go
func (m *Manager) runAmbientTurn(threadID string, msgs []ambientMsg) {
    prov, resolved, _ := m.getProviderForThread(threadID)
    if prov == nil || resolved == nil { return }

    whisperCfg := m.cfg.Channel.Whisper
    ctx := context.Background()

    // 1. 获取 cached agent（只读 — 拿配置 + 共享资源 + 加载 history）
    ca, _ := m.acquireAgent(ctx, threadID)
    parentAgent := ca.agent

    // 2. 加载主 session 历史
    history, _ := parentAgent.LoadSessionHistory()

    // 3. Fork
    forked := parentAgent.Fork(agent.ForkConfig{
        Provider:      prov,
        Model:         resolved.Provider.Model,
        MaxIterations: whisperCfg.AmbientMaxIterations,
        AllowedTools:  whisperCfg.AmbientTools,
        Logger:        m.logger.WithPrefix("ambient-fork"),
        SessionID:     "ambient-" + threadID,
    })

    // 4. Release cached agent — fork 持有独立的共享资源引用
    m.releaseAgent(ca)
    defer forked.Close()

    forkAgent := forked.Agent()
    forkAgent.SetContextWindow(resolved.Provider.ContextWindow)

    // 5. Steer channel（handleAmbientMessage 的 Case A 需注入）
    steerCh := make(chan string)
    forkAgent.SetSteerChannel(steerCh)
    ta, _ := m.threadActivations.Load(threadID)
    ta.mu.Lock()
    ta.steerRespCh = steerCh
    ta.resultCh = make(chan handlerResult, 1)
    ta.mu.Unlock()

    // 6. RunConversationStream — 有 history + steer，但不写 session（无 SessionManager）
    systemPrompt := agent.BuildSystemPrompt(m.cfg.Language, "") + "\n" + whisperPromptSuffix
    eventCh := forkAgent.RunConversationStream(ctx, history,
        buildAmbientPrompt(msgs), systemPrompt,
        llm.ChatOptions{MaxTokens: whisperCfg.AmbientMaxTokens})

    text, err := m.drainEvents(eventCh, forkAgent, m.isVerboseFor(threadID), nil, ta)

    // 7. 清理 thread activation
    ta.mu.Lock()
    ta.steerRespCh = nil
    ta.resultCh = nil
    ta.lastAmbient = time.Now()
    ta.mu.Unlock()

    // 8. [SILENT] 检查 → sendToThread
    if m.isSilence(text) { ... return }
    m.sendToThread(ctx, threadID, text, "")
}
```

| 维度 | 旧（共享路径，已删除） | 新（Fork 路径） |
|------|----------------------|----------------|
| Agent | `acquireForTurn` → cachedAgent | `Fork()` → new agent |
| Session | 写入主 session JSONL | 无 SessionManager，不写 |
| History 缓存 | 更新 `ca.history` | 不更新 |
| 工具集 | 全部 | 白名单 |
| 迭代预算 | 0（unlimited）| `AmbientMaxIterations` |
| Memory | 可能写入 | 不启用 |
| 结束 | `releaseAgent` | `forked.Close()` |

## 六、配置扩展

**文件**: `config/config.go` — `ChannelWhisperConfig`

```go
type ChannelWhisperConfig struct {
    Enabled              bool          `yaml:"enabled" default:"true"`
    AmbientBatchWindow   time.Duration `yaml:"ambient_batch_window" default:"30s"`
    AmbientMaxIterations int           `yaml:"ambient_max_iterations" default:"5"`
    AmbientMaxBuffer     int           `yaml:"ambient_max_buffer" default:"50"`
    AmbientCooldown      time.Duration `yaml:"ambient_cooldown" default:"0"`
    SilenceMarker        string        `yaml:"silence_marker" default:"[SILENT]"`

    // --- 新增 ---
    AmbientTools     []string `yaml:"ambient_tools"`
    AmbientMaxTokens int      `yaml:"ambient_max_tokens" default:"4096"`
}
```

`AmbientMaxIterations` 之前已定义但未接入，本次通过 `ForkConfig.MaxIterations` 接上。

## 七、实施顺序

| # | 内容 | 文件 | 风险 |
|---|------|------|------|
| 1 | `ForkedAgent` + `ForkConfig` + `Fork()` | `agent/agent_fork.go` 新文件 | 低 |
| 2 | `LoadSessionHistory()` | `agent/agent_fork.go` | 低 |
| 3 | 重构 `childAdapter.Run()` 用 Fork | `agent/agent_subagent.go` | 中 |
| 4 | `runAmbientTurn` 改为 fork 路径 | `channel/manager/ambient.go` | 低 |
| 5 | 配置字段 | `config/config.go` | 低 |
| 6 | 测试 | 各层 | 低 |

## 八、未解决事项

- `LoadSessionHistory()` 如果 session 历史很长（比如 100k tokens），全量加载到 `RunOneOffStream` 的 `history` 参数可能导致 prompt cache miss。未来可考虑截断或摘要策略。
- Fork session 的对话记录不持久化 — 如果需要回溯 ambient turn 的推理过程，后续可由 `drainEvents` 接入 recorder。
