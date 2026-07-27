# 可维护性重构方案

> 版本: 1.0 | 日期: 2026-07-27 | 状态: 设计阶段（待评审）

## 目录

- [一、背景与问题](#一背景与问题)
- [二、设计目标 / 非目标](#二设计目标--非目标)
- [三、现状盘点（已核实）](#三现状盘点已核实)
- [四、P0：turnState 抽取与 data race 修复](#四p0turnstate-抽取与-data-race-修复)
- [五、P0：三个行为不一致 bug](#五p0三个行为不一致-bug)
- [六、P1：Frontend 接口与 command core](#六p1frontend-接口与-command-core)
- [七、P1：工具集视图化](#七p1工具集视图化)
- [八、P2：FrontendProfile 替代布尔开关](#八p2frontendprofile-替代布尔开关)
- [九、P2：AIAgent deps 与 Fork 语义反转](#九p2aiagent-deps-与-fork-语义反转)
- [十、P3：文件级拆分与测试补洞](#十p3文件级拆分与测试补洞)
- [十一、实施分期](#十一实施分期)
- [十二、开放问题](#十二开放问题)

---

## 一、背景与问题

代码库现状：**87281 行 Go / 328 文件 / 37 包**。

这份文档要说的债务不是"写得糙"。恰恰相反：18 篇带日期的设计文档、全库仅 4 处 TODO/FIXME、`agent/commands/` 已经抽出了共享的格式化层与 prompt 模板、`wdctx` 用 context 而非全局变量做工作目录隔离——这些都在平均线以上。

问题是**成功的代价**：

1. **三个前端（TUI / channel / ACP）都做得足够完整**，于是 slash command 的编排逻辑复制了三份，并已开始产生真实的行为分叉。
2. **`AIAgent` 承担了太多"正确"的职责**（60+ 字段 / 83 个导出方法），于是变成上帝对象；其中 per-turn 可变状态挂在被跨 goroutine 缓存复用的长生命周期对象上，产生了一个**已用 `-race` 复现的数据竞争**。

这类债务的特征是"每一步都合理，累积起来不合理"。它不会突然爆炸，但会持续提高每个新功能的边际成本——例如现在新增第四个前端，需要读遍 6 个 setter 的注释才能猜出正确的开关组合。

### 与既有设计文档的关系

- `2026-07-02-agent-fork-design.md` — 本文第九章讨论 `Fork()` 的字段继承语义反转，是对该文档的修订建议。
- `2026-07-24-oneoff-transcript-design.md` — `skipSessionWrites` / `oneoffRec` 是第八章 `FrontendProfile` 的输入之一。
- `2026-07-20-event-stream-design.md` — 第四章的 `turnState` 若落地，`turnStart` / `turnTraceID` 将成为事件流埋点的天然载体，两者方向一致。

---

## 二、设计目标 / 非目标

### 目标

1. **消除已证实的 data race**，让 `AIAgent` 的 per-turn 状态在并发读写下安全。
2. **消除三前端间的行为不一致**——同一个 slash command 在不同前端应有相同语义。
3. **让共享层从"纯函数"上升到"编排"**，使新增前端 / 新增命令的成本从 O(前端数) 降到 O(1)。
4. **把非法状态变为不可表示**：临时工具集、模态状态、前端能力组合。

### 非目标

- **不追求消除所有重复**。TUI 的异步（bubbletea `Update` 不可阻塞）与 channel 的多 agent 缓存是本质差异，强行统一只会造出更坏的抽象。
- **不重写 `/compact`**。三前端三种执行模型，本阶段只统一语义（见 5.2），不统一代码。
- **不改变任何对外契约**：`-o json` 字段、配置文件 schema、session JSONL 格式保持不变。
- **不引入新的第三方依赖**。

---

## 三、现状盘点（已核实）

### 3.1 规模分布

| 目录 | 源码行 | 测试文件 | 备注 |
|---|---|---|---|
| `agent/tools` | 6291 | 17 | 覆盖健康 |
| `tui` | 7535 | 5 | 测试密度最低的大包 |
| `agent` | 5631 | 12 | |
| `channel/manager` | 3834 | 2 | **并发最复杂 + 测试最少** |
| `agent/mcp` | 3007 | 7 | |
| `channel/discord` | 2990 | 2 | |
| `agent/acp` | 2943 | 11 | |
| `channel/weixin` | 2632 | 5 | |
| `channel/github` | 2461 | **0** | 功能未完成（有 TODO） |
| `.` (main.go) | 1288 | **0** | 含全部 CLI 输出契约 |
| `agent/hooks` | 411 | **0** | 最新功能 |

单文件 Top 5：`tui/commands.go` 1425 / `main.go` 1277 / `config/config.go` 1150 / `channel/manager/commands.go` 1114 / `agent/agent_loop.go` 929。

### 3.2 已抽出的共享层（不要重复劳动）

| 共享设施 | 位置 | 三前端是否都用 |
|---|---|---|
| `Registry` / `Find` / `MatchPrefixForMode` | `agent/commands/commands.go:36-131` | 是 |
| `ParseResearchArgs` | `commands.go:141-163` | 是 |
| `FormatUsageReport` / `FormatSkillList` / `FormatMCPList` | `format.go:63,177,239` | 是 |
| `BuildCompactInstruction` | `compact.go:16` | 是 |
| `CommitUserPrompt` / `ReviewUserPrompt` / `InitPromptTemplate` | `commit.go:39`, `prompts.go:8-14` | 是 |
| `agent.ComputeSessionUsage` | `agent/usage.go:41-120` | 是 |
| `agent.FinalizeCompact` / `DrainCompactEvents` | `agent/compact.go:27,95` | 是 |
| `agent.ActivateSkill` / `IsSkillActive` | `agent/agent_skill.go:21-45` | TUI + ACP；**channel 绕过** |

**结论：metadata + 格式化 + 纯计算已抽干净。剩余重复全在编排层。**

### 3.3 三前端 slash command 实现矩阵

三个 commands 文件合计 **3339 行**，其中约 **520–600 行**为可消除的业务逻辑复制。

| 命令 | TUI | channel | ACP | 重复率 |
|---|---|---|---|---|
| `/usage` | `tui/commands.go:582-632` (51) | `:629-698` (70) | `acp:473-530` (58) | **~75%** |
| `/compact` | `:1224-1330` + `model_events.go:157-238` (~215) | `:513-547` + `agent_turn.go` (~90) | `acp:389-471` (83) | ~40% |
| `/research` | `:934-1058` (125) | `:1005-1060` (56) | `acp:741-800` (60) | ~55% |
| `/skill` | `:1336-1425` (90) | `:764-894` (131) | `acp:610-691` (82) | ~50% |
| `/transcript` | `:637-701` (65) | `:904-995` (92) | `acp:693-739` (47) | ~55% |
| `/review` 配置解析 | `:1145-1198` (54) | — | `acp:301-330` (30) | **逐行同构 28 行** |
| `/commit` registry 裁剪 | `:1076-1080` | — | `acp:256-261` | **逐字相同 5 行** |
| `/mcp` | `:221-579` (359, 含 OAuth) | `:562-626` (65) | `acp:532-607` (76) | 低（TUI 独有交互） |
| `/model` | `:52-77` + selector | `:189-324` (136) | `acp:206-227` (22) | 低 |
| `/new` `/quit` `/sessions` `/dream` | 仅 TUI | — | — | — |
| `/cron` `/cd` `/stop` `/restart` | — | 仅 channel | — | — |

最典型的 `/usage`：`UsageReportInfo` 的 **21 个字段填充写了三遍**，真正的前端差异只有 3 处约 8 行（session 定位、token 估算来源、输出通道）。

### 3.4 AIAgent 字段职责聚类

`AIAgent` 60+ 字段 / 83 个导出方法（25 个是 setter/getter）/ **零 mutex**。

| 组 | 字段 | 生命周期 |
|---|---|---|
| LLM providers (6) | `provider`, `titleModelProvider`, `commitProvider`, `reviewProvider`, `runProvider`, `subagentProvider` | 长 |
| 工具/权限 (8) | `toolRegistry`, `permissionMode`, `permissionHandler`, `permissionPolicy`, `processManager`, `savedTools`, `maxIterations`, `iterationBudget` | 长 |
| 持久化 (4) | `sessionManager`, `oneoffRec`, `lastOneoffPath`, `skipSessionWrites` | 长 |
| 记忆/技能 (4) | `memory`, `skillStore`, `activeSkills`, `skillListReminder` | 长 |
| MCP/LSP (4) | `mcpManager`, `sharedMCP`, `deferredToolReminder`, `lspManager` | 长 |
| 前端开关 (6) | `acpFileMode`, `planToolEnabled`, `autoApproveEdits`, `autoApprovePolicyAsks`, `mode`, `titleGenEnabled` | 长 |
| 通信通道 (3) | `confirmRespCh`, `askUserRespCh`, `steerRespCh` | 长 |
| **per-turn 可变 (8)** | **`lastInputTokens`, `lastTokenBreakdown`, `lastMessages`, `turnStart`, `turnTraceID`, `pendingImages`, `lastCompactTokenEstimate`, `lastMessageDate`** | **短（每轮变）** |

### 3.5 data race —— 已用 `-race` 复现

`go test -race ./channel/... ./agent/` 现状全绿，但这只说明现有测试未覆盖该路径（`channel/manager` 仅 2 个测试文件）。构造最小复现后**竞争确实存在**：

```
WARNING: DATA RACE
Write at 0x00c0003bb940 by goroutine 9:
  agent.(*AIAgent).EstimateAndUpdateTokens()  agent/token_estimate.go:213
Previous read at 0x00c0003bb940 by goroutine 10:
  agent.(*AIAgent).LastInputEstimate()        agent/agent.go:422

WARNING: DATA RACE
Write at 0x00c0003bb948 by goroutine 9:
  agent.(*AIAgent).EstimateAndUpdateTokens()  agent/token_estimate.go:214
  (lastTokenBreakdown — struct，撕裂后可读到混合状态)
```

**真实触发路径**：channel 模式下 agent 被 `agentCache` 缓存。正常 turn 走 `acquireForTurn`（`agent_turn.go:502`）持 `cachedAgent` 锁，安全。但 **slash command 路径不 acquire**：

```go
// channel/manager/agent_cache.go:273
func (m *Manager) getAgentEstimate(threadID string) int64 {
    m.agentCacheMu.Lock()          // ← 只保护 map 查找
    defer m.agentCacheMu.Unlock()
    ca, ok := m.agentCache[threadID]
    if !ok || ca.agent == nil { return 0 }
    return ca.agent.LastInputEstimate()   // ← 无锁读 agent 内部字段
}
```

`handleUsageCommand`（`commands.go:629-698`）全程不 acquire agent。用户在 agent 正在跑时发 `/usage`（channel 的 pendingQueue 机制明确允许），即触发。`getAgentBreakdown`（`:285`）同理，且读的是 struct。

### 3.6 SaveToolRegistry / RestoreToolRegistry 的 13 个调用点

| 场景 | 位置 |
|---|---|
| `/commit` 只留 Bash | `tui:1077`, `acp:256` |
| `/compact` 清空 | `tui:1259-1260`, `acp:428-429`, `channel/agent_turn.go:510` |
| cron 临时注册 CronTool | `channel/cron.go:60-61` |
| 每 turn 临时注册 SendFileTool | `channel/agent_turn.go:523,536` |
| provider 切换 | `tui/provider_selector.go:197-198` |
| Fork 清理 | `agent_fork.go:55` |
| 恢复点（TUI 分散） | `model_events.go:200`, `:267`, `:380`, `commands.go:1297`, `:1317` |

TUI 的 `m.savedTools` 存在 `Model` 上，恢复点分散在 **5 处**。任一错误路径漏恢复，agent 永久失去工具——**不报错，只是变笨**，极难定位。

另注：`RestoreToolRegistry`（`agent.go:679-688`）遍历 map 恢复，Go map 迭代顺序随机。`.tachi.md` 载明 registry 有顺序语义（"Built-ins alpha, MCPs appended (monotonic cache key)"），恢复后 prompt cache key 可能不稳定 → cache 失效。**待验证**（见第十二章）。

### 3.7 其它已核实的重复

- `ConvertSessionToLLMMessages` 有 **12 个非测试调用点**，多数是同一个"load → 空检查 → convert"模式。`agent.LoadSessionHistory()`（`agent_fork.go:127-145`）已封装该模式，**但无人调用**。
- ACP 包内部自我重复：该 10 行块在 `acp/commands.go:361-372`、`400-413`、`670-681` 出现 **3 次**。
- `/research` 的 depth/breadth 默认值回填：`tui:953-958` ≡ `channel:1015-1020` ≡ `acp:759-765`，**逐字符相同的 6 行 × 3**。

---

## 四、P0：turnState 抽取与 data race 修复

### 4.1 设计

把 3.4 表中最后一组（8 个 per-turn 字段）抽入独立结构，自带 `RWMutex`：

```go
// agent/turn_state.go
type turnState struct {
    mu sync.RWMutex

    inputTokens     int64                    // was lastInputTokens
    breakdown       tokenbreakdown.Breakdown // was lastTokenBreakdown
    messages        []llm.Message            // was lastMessages
    start           time.Time                // was turnStart
    traceID         string                   // was turnTraceID
    pendingImages   []llm.ContentPart        // was pendingImages
    compactEstimate int64                    // was lastCompactTokenEstimate
    lastMessageDate string                   // was lastMessageDate
}

func (s *turnState) setEstimate(total int64, tb tokenbreakdown.Breakdown) {
    s.mu.Lock(); defer s.mu.Unlock()
    s.inputTokens, s.breakdown = total, tb
}

func (s *turnState) estimate() int64 {
    s.mu.RLock(); defer s.mu.RUnlock()
    return s.inputTokens
}

func (s *turnState) snapshotBreakdown() tokenbreakdown.Breakdown {
    s.mu.RLock(); defer s.mu.RUnlock()
    return s.breakdown  // 值拷贝，调用方拿到一致快照
}

// beginTurn 在 runAgentLoop 顶部调用，让"每轮重置"语义显式化
func (s *turnState) beginTurn(traceID string) { ... }
```

`AIAgent` 中 8 个字段替换为一个 `turn *turnState`（在 `NewAIAgent` 中初始化）。

### 4.2 `messages` 字段的特殊处理

`lastMessages` 由 `GetLastMessages()` 返回给 channel 用作历史缓存（`.tachi.md` 载明）。当前返回 slice header，调用方可能持有底层数组引用。锁只保护 header 读写，不保护元素。

**决策**：`GetLastMessages()` 返回**浅拷贝的 slice**（`append([]llm.Message(nil), s.messages...)`）。`llm.Message` 内部若含 slice 字段（`ContentPart`），元素仍共享——但历史消息在写入后是只读的，可接受。此约束需在方法注释中写明。

### 4.3 改动清单

| 文件 | 改动 |
|---|---|
| `agent/turn_state.go` | **新增** — `turnState` 定义 + 访问器 |
| `agent/agent.go` | 删 8 字段，加 `turn *turnState`；`NewAIAgent` 初始化；`LastInputEstimate` (`:421`) 改走访问器 |
| `agent/token_estimate.go` | `:213-214` → `a.turn.setEstimate(...)`；`:220` → `snapshotBreakdown()` |
| `agent/agent_loop.go` | `:220` 的 `defer` 恢复逻辑；`turnStart`/`turnTraceID` 赋值改走 `beginTurn` |
| `agent/agent_configure.go` | `:462`,`:464` resume 时的估算注入 |
| `agent/compact.go`, `agent/agent_compact.go` | `lastCompactTokenEstimate` 读写 |
| 其余读写点 | 全库 grep `lastInputTokens\|lastMessages\|turnStart\|turnTraceID\|pendingImages\|lastCompactTokenEstimate\|lastMessageDate` |

### 4.4 测试计划

1. **回归探针**（保留为正式测试，`agent/turn_state_test.go`）：一个 goroutine 循环 `EstimateAndUpdateTokens`，另一个循环 `LastInputEstimate` + `LastTokenBreakdown`，`-race` 下必须干净。这是 3.5 中复现 race 的那个用例，修好后转为防回归。
2. **channel 层集成测试**：`channel/manager` 补一个"turn 进行中并发调 `handleUsageCommand`"的测试——顺带缓解 3.1 表中该包测试过少的问题。
3. `make test` + `go test -race ./...` 全绿。

### 4.5 收益

- 关掉唯一已证实的正确性问题。
- `AIAgent` 减 8 字段。
- "每轮重置"从散落赋值（`lastMessages` 仅 1 处写、`turnStart` 1 处，靠约定维持）变为 `beginTurn()` 一处显式。
- 为 `2026-07-20-event-stream-design` 的 turn 级埋点提供天然载体。

---

## 五、P0：三个行为不一致 bug

复制粘贴已经产生真实分叉。这三个都是**几十行内的独立修复**，不依赖任何架构重构，应优先落地。

### 5.1 ACP `/usage` 成本计算错误

```go
// agent/acp/commands.go:198-200
func resolveModelPrice(sess *ACPSession) *llm.ModelPrice {
    return llm.ResolveModelPrice(sess.agent.Model(), nil, nil, nil, nil)
}
```

四个 `nil` 丢掉了 provider 配置中的 `InputPrice` / `OutputPrice` / `CacheReadInputPrice` / `CacheCreationInputPrice`。TUI（`tui/model.go:282-297`）与 channel（`commands.go:648-660`）都正确读取。

**影响**：配置了自定义价格的 provider，在 Zed 中看到的成本是错的（静默）。

**修复**：抽 `cmds.ResolveModelPrice(cfg *config.Config, providerName, model string) *llm.ModelPrice`，三前端共用。注意保留 channel 的 fallback 逻辑（`pCfg` 命中但价格为 nil 时退回内置价格表，`commands.go:657-659`）。

### 5.2 channel / ACP 的 `/compact` 不写 memory

`StoreCompactMemory()` 仅在 3 处被调用：`tui/commands.go:1255`、`tui/provider_selector.go:195`、`agent/agent_compact.go:62`（自动 compact）。

**影响**：channel 与 ACP 的**手动** `/compact` 静默跳过记忆固化，而同前端的**自动** compact 会写。同一前端内行为不一致。

**修复**：在 `channel/manager/commands.go:513` 的 `finalizeCompactResult` 与 `agent/acp/commands.go:453` 的 finalize 段前补调。需确认 channel 的 throwaway agent（`agent_turn.go:505-512`）是否持有 memory backend——若无，应改由 cached agent 调用，或把该调用上移进 `agent.FinalizeCompact()`（更彻底，但要先确认 3 处调用方的 memory 状态一致）。

**倾向**：上移进 `FinalizeCompact()`，让"compact 必写 memory"成为 agent 层不变量，前端无从遗漏。

### 5.3 channel skill 激活丢失 activeSkills 标记

```go
// channel/manager/commands.go:838-850
func (m *Manager) prepareSkillActivation(skillName, extraArgs string) (string, string, error) {
    sk, err := m.skillStore.Load(skillName)          // ← 绕过 agent
    ...
    msg := skill.BuildActivationMessage(sk, extraArgs)  // ← 手写 ActivateSkill 的两步
    return msg, "", nil
}
```

对比 `agent.ActivateSkill`（`agent_skill.go:20-36`）：多了 `a.activeSkills[sk.Meta.Name] = true`。

**根因**：channel 在 slash-command 阶段尚未 acquire agent，所以走了旁路——`Manager` 自己持有一个独立的 `m.skillStore`（`manager.go` New 中初始化）。

**影响**：
- channel 中同一 skill 反复激活 → 每次重新注入完整 skill body，浪费 token。TUI 有 `IsSkillActive` 检查后走轻量 `BuildDirectiveMessage`（`tui/commands.go:1388-1390`）。
- ACP 第三种行为：已激活时**直接拒绝**（`acp:654-656` 返回 "already active"）。

三前端对"重复激活"有三种语义。

**修复**：
1. 先统一语义。建议采用 TUI 的做法（已激活 → 注入轻量 directive），因为它对用户最友好且省 token。ACP 的"拒绝"应改为同样行为。
2. channel 侧把激活消息构建延后到 acquire agent 之后，改调 `agent.ActivateSkill`。若时序上确实困难，退路是在 `acquireForTurn` 后补一次 `agent.MarkSkillActive(name)`。
3. `m.skillStore` 与 agent 的 `skillStore` 双持有值得单独审视——`reloadAgentSkills`（`commands.go:823-834`）已经在做广播同步，说明双持有的代价已经显现。

### 5.4 顺带的小修

- `agent/acp/commands.go:748` 的 `/research` usage 文案提到不存在的 `--format` 参数。
- `/research` 的 timeout：TUI 用 `cfg.Timeout + time.Minute`（`:986`），另两个用 `cfg.Timeout`。疑似有意（留渲染余量）但无注释——补注释或统一。

---

## 六、P1：Frontend 接口与 command core

### 6.1 抽象边界

关键判断：**共享的是编排，不是渲染**。差异收敛到一个窄接口上。

```go
// agent/commands/exec.go
type Frontend interface {
    // Session 返回当前会话管理器。三前端定位方式不同：
    //   TUI     — agent.SessionManager()
    //   channel — newSessionManager() + FindByThreadID(threadID)
    //   ACP     — sess.sessMgr
    Session() *session.Manager

    Agent()  *agent.AIAgent
    Config() *config.Config

    // Emit 输出一段最终结果：chatview.AddMessage / reply text / SessionUpdate
    Emit(ctx context.Context, text string)

    // Progress 输出中间进度。TUI 实现为写 chan（因 bubbletea Update 不可阻塞），
    // channel/ACP 直接发送。实现方须保证并发安全（deepresearch 会并发回调）。
    Progress(ctx context.Context, text string)
}
```

命令核心变成纯编排函数：

```go
func RunUsage(ctx context.Context, f Frontend) error
func RunTranscript(ctx context.Context, f Frontend) (htmlPath string, err error)
func ResolveResearchArgs(cfg *config.Config, args string) (ResearchArgs, error)  // 含默认值回填
func ResolveReviewConfig(cfg *config.Config, a *agent.AIAgent) ReviewConfig
func ResolveModelPrice(cfg *config.Config, providerName, model string) *llm.ModelPrice
```

### 6.2 迁移分档

**A 档 · 直接迁**（纯计算 + 单次输出，无异步）

| 目标 | 消除 | 顺带修复 |
|---|---|---|
| `RunUsage` | 21 字段填充 × 3 ≈ 120 行 | 5.1 的 ACP 价格 bug |
| `RunTranscript` | 5 步流程 × 3 ≈ 60 行 | — |
| `ResolveReviewConfig` | 28 行同构 | — |
| `ResolveResearchArgs` | 6 行 × 3 + usage 文案 | 5.4 的文案 bug |
| `ResolveModelPrice` | 3 处 | 5.1 |
| `/commit` registry 裁剪 | 5 行 × 2 | — |
| `LoadSessionHistory()` 收口 | 12 处样板，ACP 内部 3 处 | — |

预计净减 **250–300 行**，且**修 bug 与减重同时发生**——这是本方案中最划算的部分。

`LoadSessionHistory()` 收口尤其便宜：函数已存在，只是没人用。ACP 那 3 处重复是十分钟的活儿，可作为整个重构的第一个 commit（低风险、立即可验证）。

**B 档 · 需 Progress 抽象**

`/research`（三前端 ~240 行 → 预计 ~90 行 + 3 个薄适配）、`/mcp list`。TUI 的异步是本质约束，但用 `Frontend.Progress` 可把差异收敛到一个方法实现里。

**C 档 · 本阶段不动**

`/compact`——三前端三种执行模型：

| | TUI | channel | ACP |
|---|---|---|---|
| 执行 | `RunConversationStream` + 异步事件循环 | 普通 agent turn（`agent_turn.go:78` 识别 `isCompactCmd`） | `RunConversationStream` + `DrainCompactEvents` 同步 |
| 工具处理 | save/clear + `model_events.go:199` 恢复 | throwaway agent（`agent_turn.go:505-512`） | `savedTools` + `defer` |
| rollback | `rollbackCompact` + `abortCompactForSwitch`（20+13 行，**9 行重复**） | 无，返回错误文本 | 无，靠 defer |
| 状态串联 | `m.isCompacting` 横跨两文件 | — | — |

这些差异**大部分是本质的**（TUI 不能阻塞 / channel 需 ThreadID 迁移）。本阶段只做 5.2 的语义统一。若日后要动，第一步是把 `abortCompactForSwitch` 与 `rollbackCompact` 的 9 行重复合并，而非跨前端统一。

### 6.3 `/model` `/new` `/mcp` 的处理

这三个在三前端的实现**语义本就不同**，不属重复：
- `/new`：TUI 清 view，channel 清 session + evict cache。
- `/model`：channel 独有 pre-switch compact（`commands.go:332-381`）。
- `/mcp`：TUI 独有 overlay + 双向 OAuth 交互流（359 行）；ACP 的 reconnect 是 stub。

**不迁移**。但 ACP 的 reconnect stub（`acp:572-607`）应记为待补功能。

---

## 七、P1：工具集视图化

### 7.1 问题

3.6 的 13 个 save/restore 调用点，本质都是"临时改工具集 → 用完恢复"。用**可变状态 + 手工恢复**实现一个本该是**不可变视图**的概念。失败模式恶劣（静默变笨）。

### 7.2 方案

**方案 A（推荐）· per-call 工具视图**

```go
type RunOption func(*runOptions)

func WithToolSet(names ...string) RunOption  // 只暴露指定工具（/commit → "Bash"）
func WithNoTools() RunOption                 // /compact
func WithExtraTools(t ...tools.Tool) RunOption // cron 的 CronTool、channel 的 SendFileTool

func (a *AIAgent) RunOneOffStream(..., opts ...RunOption) chan AgentEvent
func (a *AIAgent) RunConversationStream(..., opts ...RunOption) chan AgentEvent
```

`buildLLMTools`（`agent_loop.go` 内，每轮重建）读 `runOptions` 做过滤，**registry 本身不变**。13 处 save/restore 全部消失，漏恢复的可能性从设计上消除。

**方案 B · registry 派生视图**

```go
view := a.toolRegistry.View(filter)  // 只读视图，不动父 registry
```

更通用，但要改 `Registry` 内部与 schema 缓存键（`.tachi.md` 提到 monotonic cache key），风险高于 A。

**倾向 A**：改动集中在 `buildLLMTools` 一处，调用方逐个迁移，可增量进行（先迁 `/commit`，验证后推广）。

### 7.3 附带待验证项

`RestoreToolRegistry` 的 map 随机序是否影响 schema 顺序 → prompt cache key 稳定性。若确认有影响，则方案 A 顺带修掉一个**隐性的成本问题**（cache 失效意味着每次 restore 后的首轮请求全价计费）。验证方法见第十二章。

---

## 八、P2：FrontendProfile 替代布尔开关

### 8.1 问题

6 个独立开关（读取点数量为 grep 非测试文件计数）：

| 字段 | 读取点 | 语义 |
|---|---|---|
| `acpFileMode` | 4 | EditFile 走 ACP conn 而非本地 |
| `planToolEnabled` | 5 | 注册 SavePlan |
| `skipSessionWrites` | 6 | recordSession no-op |
| `autoApproveEdits` | 5 | 跳过 EditFile 确认 |
| `autoApprovePolicyAsks` | 4 | policy "ask" → 批准 |
| `sharedMCP` | 20 | MCP 不由本 agent 拆除 |
| `mode` | — | auto / chat / plan |

理论组合 2^5 × 3 = **96 种**，实际有效约 **4 种**（TUI / channel / ACP / one-off）。这些字段的注释都写得很详尽——恰恰说明作者自己也觉得需要解释。但注释救不了组合爆炸：新增前端时需读遍所有 setter 才能猜出正确组合，且编译器不会检查你漏设了哪个。

### 8.2 方案

```go
// agent/profile.go
type FrontendProfile struct {
    Name           string           // "tui" | "channel" | "acp" | "oneoff"
    FileIO         FileIOStrategy   // LocalFileIO | ACPConnFileIO
    PlanTool       bool
    PersistSession bool
    EditApproval   ApprovalPolicy   // AlwaysAsk | AutoApprove | Skip
    PolicyAsks     ApprovalPolicy
}

var (
    ProfileTUI     = FrontendProfile{Name: "tui", FileIO: LocalFileIO, PersistSession: true, EditApproval: AlwaysAsk, ...}
    ProfileChannel = FrontendProfile{...}
    ProfileACP     = FrontendProfile{Name: "acp", FileIO: ACPConnFileIO, PlanTool: true, ...}
    ProfileOneOff  = FrontendProfile{Name: "oneoff", PersistSession: false, EditApproval: Skip, ...}
)
```

`Configure` 接收 profile。`sharedMCP` 不属此列（是资源所有权而非前端能力），保持原样。`mode` 保持独立（用户运行时可切）。

**收益**：新增前端 = 定义一个 profile 值；有效组合显式枚举、可测试、可 diff。

**风险**：`autoApproveEdits` 有运行时变更路径（用户在确认框选 "always"，session 内生效）。profile 须区分"静态默认"与"运行时覆盖"——建议 profile 不可变，运行时覆盖存 `turnState` 之外的一个 `sessionOverrides` 结构。此细节需在实施前定稿。

---

## 九、P2：AIAgent deps 与 Fork 语义反转

### 9.1 Fork 的字段遗漏风险

`Fork()`（`agent_fork.go:66-115`）手工拷贝 7 项：logger、tools（含 `forkTool` 对 `ReadTool` 的特殊处理）、`PermissionModeSkip`、`ReminderCollector(nil)`、processManager、mcpManager（受 `NoMCP` 控制）、permissionPolicy。

**未处理的长生命周期字段**：`cfg`、`hookDispatcher`、`lspManager`、`skillStore`、`contextWindow`、`memory`、`iterationBudget`。

其中部分是**有意**不继承（session / memory / LSP，`agent_fork.go:60-64` 注释说明）。但 **`child.cfg == nil`** 是隐患：任何将来在 child 路径上读 `a.cfg` 的代码会 panic 或静默降级。`Configure` 中大量逻辑依赖 `a.cfg`，而 child 从不走 `Configure`——这个不变量目前只靠"没人这么写"维持。

### 9.2 方案：默认继承 + 显式排除

```go
type agentDeps struct {   // 所有共享依赖，长生命周期
    cfg            *config.Config
    logger         *logger.Logger
    processManager *tools.ProcessManager
    mcpManager     *mcp.Manager
    lspManager     *lsp.LSPManager
    skillStore     *skill.Store
    permissionPolicy *permission.Policy
    hookDispatcher *hooks.Dispatcher
}

func (a *AIAgent) Fork(cfg ForkConfig) *ForkedAgent {
    child := &AIAgent{deps: a.deps}   // 浅拷贝：默认全继承
    // 然后显式排除
    child.deps.mcpManager = nil  // if cfg.NoMCP
    child.sessionManager = nil   // 永不继承
    child.memory = nil
    ...
}
```

方向与现状相反：**遗漏时的失败模式从"静默 nil"变为"多继承一个无害引用"**。对 `AIAgent` 也是一次结构性瘦身（8 字段 → 1 个 `deps`）。

### 9.3 风险

`Close()` 的所有权语义必须重新审计。当前 `ForkedAgent` 用 `sharedPM` / `sharedMCP` 字段标记"不要拆"（`agent_fork.go:37-40`）。改为 deps 浅拷贝后，需要一个显式的 `ownedResources` 集合，或让 deps 中每项资源自带引用计数。**这是本方案中设计不确定性最高的一项**，建议排在最后，且单独出设计文档。

---

## 十、P3：文件级拆分与测试补洞

### 10.1 纯文件拆分（零行为风险）

| 文件 | 现状 | 建议 |
|---|---|---|
| `config/config.go` | 1150 行 / 37 struct | 按域拆：`provider.go` `channel.go` `memory.go` `lsp.go` `research.go` `hooks.go` |
| `main.go` | 1277 行 / 25 函数 | 拆 `cmd/` 子包；4 个 output 函数 → `internal/output` |
| `agent/mcp/oauth_flow.go` | 916 行 | OAuth2 / PKCE / DCR 三流程分文件（有 7 个测试文件保护，优先级低） |

代价是 `git blame` 断层，换来可读性。建议用 `git mv` + 纯移动 commit（不含任何逻辑修改），使 `--follow` 可用。

### 10.2 tui.Model 的模态状态

`tui.Model` 44 字段。模态状态 `AwaitingConfirmation` / `AskUserQuestion` / `SelectingModel` / `SelectingSession` / `isCompacting` / `isResearching` 是**并行布尔**，但语义互斥——当前形式允许"同时在 compact 和 research"这种非法状态。

建议 `modalState` 枚举 + payload：

```go
type modalState int
const (
    modalNone modalState = iota
    modalConfirmEdit; modalAskUser; modalSelectModel; modalSelectSession
    modalCompacting; modalResearching
)
```

与第六章的 Frontend 抽象独立，可并行进行。

### 10.3 测试补洞（按性价比排序）

1. **`main.go` 的 output 契约**。`runOutputJSON` / `runOutputJSONStream` / `usageToJSON` 是**对外接口**（`-o json` 被用户脚本依赖），字段改名即破坏性变更，当前零测试。抽到 `internal/output` + golden file 测试。**最高性价比。**
2. **`channel/manager`**。并发最复杂（`threadActivation` 含 mutex + atomic + 3 层 goroutine + ambient timer），仅 2 个测试文件。与第四章的 race 修复配套补测。
3. **`agent/hooks`**（411 行 / 0 测试）。最新功能，趁记忆新鲜补。
4. `channel/github`（2461 行 / 0 测试）功能未完成（`channel.go:630` TODO 说明 comment 检测未接 agent turn），**待功能定稿后再补测**，现在写会白写。

---

## 十一、实施分期

### 第一期 · 正确性（低风险，独立可验证）

| # | 任务 | 依赖 | 验证 |
|---|---|---|---|
| 1 | `LoadSessionHistory()` 收口 12 处样板 | 无 | `make test` |
| 2 | `turnState` 抽取 + race 修复（第四章） | 无 | `-race` 探针转正式测试 |
| 3 | 三个行为不一致 bug（第五章） | 无 | 各补一个前端级测试 |
| 4 | `main.go` output golden tests | 无 | 新增测试 |

任务 1 建议作为首个 commit——十分钟、零风险、立即可验证，用于跑通流程。

### 第二期 · 结构（中风险，需评审接口）

| # | 任务 | 依赖 |
|---|---|---|
| 5 | `Frontend` 接口 + A 档迁移（6.2） | 第一期 3 |
| 6 | 工具集视图化（第七章），先迁 `/commit` 验证 | 无 |
| 7 | B 档：`/research` 迁移 | 5 |
| 8 | `config.go` / `main.go` 拆文件 | 4 |
| 9 | `tui.Model` 模态枚举 | 无 |

### 第三期 · 需单独设计文档

| # | 任务 | 说明 |
|---|---|---|
| 10 | `FrontendProfile`（第八章） | 需先定稿"静态默认 vs 运行时覆盖" |
| 11 | `agentDeps` + Fork 反转（第九章） | 所有权语义需重新审计，**单独出文档** |
| 12 | `/compact` 跨前端统一 | 需先积累第二期经验 |

---

## 十二、开放问题

1. **`RestoreToolRegistry` 的 map 随机序是否影响 prompt cache 命中？**
   验证：连续两次 save/restore 后 dump `ToolSchemas()` 顺序，比对是否稳定；再查 Anthropic/OpenAI 的 cache key 是否含 tools 数组顺序。若有影响，第七章顺带修掉一个隐性计费问题。

2. **`StoreCompactMemory` 上移进 `FinalizeCompact` 是否安全？**（5.2）
   需确认 3 个现有调用方的 memory backend 状态一致，以及 channel 的 throwaway agent 是否持有 memory。

3. **channel 的 `m.skillStore` 与 agent `skillStore` 双持有是否应合并？**（5.3）
   `reloadAgentSkills` 的广播逻辑（`commands.go:812-834`）说明代价已显现，但合并要解决"slash command 阶段还没 agent"的时序问题。

4. **`GetLastMessages()` 浅拷贝的代价**（4.2）
   长会话每次 `/usage` 都拷贝一次 slice header 数组。需实测长会话（500+ 消息）下的开销，若显著则改为返回只读视图类型。

5. **`Frontend` 接口是否需要 `Confirm` / `Ask` 方法？**
   当前 `AskUserQuestion` 与 EditFile 确认走 agent 的 channel 机制而非前端接口。若第二期发现有命令需要交互，接口要扩——建议届时再加，避免过早抽象。

6. **`/mcp` 的 ACP reconnect stub**（`acp:572-607`）是待补功能还是有意留空？

---

## 附：核实方法

本文档所有数字与行号均来自实际测量，非估算：

- 规模统计：`find . -name "*.go" | xargs wc -l`
- 重复度：逐文件对读三前端 commands.go，人工比对字段级
- data race：构造最小复现用例 + `go test -race`（输出见 3.5），验证后删除探针文件
- 读取点计数：`grep -rn <field> --include=*.go . | grep -v _test | wc -l`
- 测试覆盖：按包统计 `*_test.go` 数量与非测试源码行数
