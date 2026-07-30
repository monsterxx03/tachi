# 对抗式代码审查 (Adversarial Review) 设计文档

> 版本: 1.0 | 日期: 2026-07-30 | 状态: 设计阶段

## 一、概述

### 动机

当前的 `/review` 是单次快照式审查：一个 LLM agent 拿到 git diff 跑一遍就出结果。这种方式有几个局限：

- **没有辩论** — 模型可能过度自信或遗漏问题，没有第二视角挑战
- **单点盲区** — 不同模型擅长的维度不同（如 Claude 擅长正确性、GPT-4o 擅长安全性）
- **不能收敛** — 发现的分歧无法被裁决，所有发现处于同等地位

对抗式审查引入多轮串行 agent，每个 agent 上下文完全隔离，通过**审查 → 挑战 → 裁决**的角色链来提升代码审查的深度和质量。

### 设计原则

1. **串行编排** — Go handler 负责任务调度，不依赖 LLM 自我编排
2. **完全隔离** — 每个 agent 独立 fork，不共享上下文，只通过磁盘文件交换信息
3. **配置驱动** — 轮次、模型、角色等参数皆可配置，不改代码
4. **向后兼容** — `adversarial.enabled: false` 时退化为现有单轮行为
5. **简单优先** — 首版不做 cycle-aware prompt 差异化，只告诉 LLM 当前轮次和角色

---

## 二、总体架构

```
用户输入 /review [N]
    │
    ├─ N 可选：覆盖配置中的 rounds
    │
    ├─ Round 1 ─ Reviewer ──────────────┐
    │  fork + RunOneOffStream            │ git diff → 初步审查报告
    │  prompt: "你是第 1 轮审查者..."     │ → .tachi/reviews/round-1-*.md
    │                                    │
    ├─ Round 2 ─ Challenger ─────────────┤
    │  fork + RunOneOffStream            │ git diff + round-1 报告 → 挑战/补充
    │  prompt: "你是第 2 轮挑战者..."     │ → .tachi/reviews/round-2-*.md
    │                                    │
    ├─ Round 3 ─ Judge ─────────────────┤
    │  fork + RunOneOffStream            │ git diff + round-1 + round-2 → 裁决
    │  prompt: "你是第 3 轮裁决者..."     │ → .tachi/reviews/round-3-*.md
    │                                    │
    ├─ Round N ─ (角色周期轮转) ─────────┤
    │  ...                               │ 最终轮固定为 Judge/Summarizer
    │                                    │
    └─ 恢复对话历史，展示最终报告路径
```

### 角色周期 (Role Cycle)

定义 3 个角色，按周期循环：

| 角色 | 职责 | 出现在轮次 |
|------|------|-----------|
| **Reviewer** | 全面审查代码变更，按 5 维度分析 | 1, 4, 7, 10, ... |
| **Challenger** | 挑战 Reviewer 的结论，补充遗漏 | 2, 5, 8, 11, ... |
| **Judge** | 裁决前两轮分歧，综合输出报告 | 3, 6, 9, ... |

**轮次到角色的映射规则**：

```
角色索引 = (round - 1) % 3
其中 0=Reviewer, 1=Challenger, 2=Judge

最后一轮（round == totalRounds）固定为 Judge/Summarizer
```

示例（rounds=5）：

| 轮次 | 角色 | 说明 |
|------|------|------|
| 1 | Reviewer | 首次全面审查 |
| 2 | Challenger | 挑战 R1 |
| 3 | Judge | 第一次裁决 |
| 4 | Reviewer | 聚焦未决区域 |
| 5 | **Judge** (固定) | 最终裁决 |

示例（rounds=2）：

| 轮次 | 角色 | 说明 |
|------|------|------|
| 1 | Reviewer | 全面审查 |
| 2 | **Judge** (固定) | 最终裁决（跳过了 Challenger 阶段） |

---

## 三、配置设计

```yaml
# config.yaml
review:
  provider: claude-sonnet          # 兜底模型（向后兼容）
  max_iterations: 200
  allowed_tools:
    - Bash
    - ReadFile
    - WriteFile
    - Glob
    - Grep
  thinking: false

  # --- 对抗式审查配置 ---
  adversarial:
    enabled: false                  # false = 退化为现有单轮行为
    rounds: 3                       # 对抗轮次（默认 3）
    models:                         # 各轮 model 分配（可选）
      - claude-sonnet               # 按索引分配各轮
      - gpt-4o
      - claude-opus
    # model_strategy 隐式由 models 列表长度决定：
    #   未设置或空 → 所有轮次用 review.provider
    #   只有一个模型 → 所有轮次用同一个
    #   多个模型 → 按索引分配，不够则取模轮转
```

### `AdversarialReviewConfig` 结构体

```go
type AdversarialReviewConfig struct {
    Enabled bool     `yaml:"enabled"`              // 开关，默认 false
    Rounds  int      `yaml:"rounds" default:"3"`   // 对抗轮次
    Models  []string `yaml:"models"`               // 各轮 model 名（可选）
}
```

嵌入到 `ReviewConfig`：

```go
type ReviewConfig struct {
    Provider      string                    `yaml:"provider"`
    MaxIterations int                       `yaml:"max_iterations" default:"200"`
    AllowedTools  []string                  `yaml:"allowed_tools"`
    Thinking      *bool                     `yaml:"thinking" default:"false"`
    Adversarial   *AdversarialReviewConfig  `yaml:"adversarial"`  // 新增
}
```

---

## 四、命令行参数

### `/review [rounds]`

```
/review         → 使用配置的 rounds 值（默认 3）
/review 6       → 6 轮对抗审查
/review 2       → 2 轮快速审查
/review 0       → 退化为单轮（等价于 adversarial.enabled: false）
```

解析逻辑：

```go
func parseReviewRounds(input string, cfgRounds int) int {
    // input: m.subcommandInput = "/review 6"
    parts := strings.Fields(input)
    if len(parts) >= 2 {
        if n, err := strconv.Atoi(parts[1]); err == nil && n >= 0 {
            if n == 0 {
                return 1 // /review 0 = 单轮
            }
            return n
        }
    }
    return cfgRounds
}
```

当 `rounds == 1` 时，退化为现有单轮行为（不启动对抗模式）。

---

## 五、Prompt 设计

每轮的 prompt 由统一的 builder 构造，接受角色 + 轮次 + 总轮数 + 前序报告路径，输出完整的 user message。

### `BuildReviewPrompt(role ReviewRole, round, totalRounds int, prevReportPaths []string) string`

```go
type ReviewRole int

const (
    RoleReviewer   ReviewRole = iota // 0
    RoleChallenger                   // 1
    RoleJudge                        // 2
)
```

**Round 1 — Reviewer prompt 概要：**

```
你是代码审查的第 1 轮审查者 (Round 1/3 — Reviewer)。

请全面审查以下代码变更的每个文件，从 5 个维度分析：
1. Correctness
2. Code Quality
3. Efficiency
4. Security
5. Maintainability

输出格式要求：
- 每个发现标注 File/行号、Severity (🐛/⚠️/💡)、Category
- 具体的理由和修复建议

你的报告将被下一轮的挑战者审查，请确保足够详细。
注意：本轮审查范围是**全部变更**。

[上下文: git diff + git status + git log]

完成后保存报告到：.tachi/reviews/round-1-<timestamp>-<summary>-review.md
```

**Round 2 — Challenger prompt 概要：**

```
你是代码审查的第 2 轮挑战者 (Round 2/3 — Challenger)。
你的队友 (Reviewer) 已完成第 1 轮审查。

报告路径：.tachi/reviews/round-1-<timestamp>-<summary>-review.md

任务：
1. 用 ReadFile 阅读第 1 轮报告
2. 标注每条发现的立场：
   - ✅ Agree — 同意并补充理由
   - ❌ Disagree — 反驳并说明理由
   - ➕ Addition — 全新的发现
3. 特别注意 Reviewer 可能遗漏的 edge case、安全面、性能瓶颈

完成后保存报告到：.tachi/reviews/round-2-<timestamp>-<summary>-challenge.md
```

**Round N — Judge prompt 概要：**

```
你是代码审查的第 N 轮最终裁决者 (Round N/N — Judge)。
你的队友已完成前 N-1 轮。

报告路径：
  - .tachi/reviews/round-1-...
  - .tachi/reviews/round-2-...
  - ...

任务：
1. 阅读所有前序报告
2. 对每一条分歧做出最终裁决（Confirmed / Disputed / Rejected）
3. 生成统一的最终报告，按严重性排序

完成后保存报告到：.tachi/reviews/round-N-<timestamp>-<summary>-judge.md
```

**中间 Judge（非最后一轮）与最终 Judge 的区别：**

- 中间 Judge：综合前序信息，标记已确认和仍有争议的区域，给出中间裁决
- 最终 Judge：综合所有信息，逐个裁决，输出最终执行摘要

Prompt 中通过 `isFinalRound` 参数区分：

```go
if isFinalRound {
    prompt += "\n这是**最终轮**，你需要做出最终裁决并生成完整的执行摘要。\n"
} else {
    prompt += "\n这是中间裁决，请标记仍有争议的区域供后续轮次聚焦。\n"
}
```

### 工具白名单

所有轮次使用相同的工具白名单（与当前一致）：`[Bash, ReadFile, WriteFile, Glob, Grep]`。

---

## 六、编排逻辑

### TUI 端调度

核心改动在 `tui/commands.go` 和 `tui/model_events.go`。

```
Model 新增字段:

type reviewState struct {
    totalRounds   int              // 总轮数
    currentRound  int              // 当前轮 (1-based)
    roundModels   []llm.Provider   // 各轮 model（长度 = totalRounds）
    reportPaths   []string         // 各轮报告路径
    config        reviewResolved   // 共享配置（allowedTools, maxIterations, thinking）
}

Model 新增:
- reviewState *reviewState       // nil = 不在对抗审查中
- isReviewing bool               // 对抗审查进行中（用于阻塞用户输入）
```

#### 1. `sendReviewCommand()` 修改

```go
func (m *Model) sendReviewCommand() tea.Cmd {
    // 解析轮次
    rounds := parseReviewRounds(m.subcommandInput, defaultRounds)
    
    m.savedHistory = make([]llm.Message, len(m.history))
    copy(m.savedHistory, m.history)
    
    if rounds == 1 {
        // 退化为现有单轮逻辑
        forked := m.agent.Fork(...)
        m.forkedAgent = forked
        m.eventCh = forked.Agent().RunOneOffStream(...)
    } else {
        // 初始化对抗审查状态
        rc := m.resolveReviewConfig()
        m.reviewState = &reviewState{
            totalRounds:  rounds,
            currentRound: 0,
            roundModels:  resolveRoundModels(rc, rounds),
            config:       rc,
        }
        m.isReviewing = true
        m.startReviewRound()  // 启动第 1 轮
    }
    return tea.Batch(m.statusbar.Tick(), m.nextEvent())
}
```

#### 2. `startReviewRound()` 新增

```go
func (m *Model) startReviewRound() tea.Cmd {
    rs := m.reviewState
    rs.currentRound++
    round := rs.currentRound
    
    role := resolveRole(round, rs.totalRounds)
    
    // 构建本轮的 prompt（包含前序报告路径）
    userPrompt := BuildReviewPrompt(role, round, rs.totalRounds, rs.reportPaths)
    
    // Fork agent
    provider := rs.roundModels[round-1]
    forked := m.agent.Fork(agent.ForkConfig{
        Provider:      provider,
        MaxIterations: rs.config.maxIterations,
        AllowedTools:  rs.config.allowedTools,
        Logger:        m.agent.Logger(),
    })
    m.forkedAgent = forked
    
    // 显示轮次标题
    roleName := []string{"审查者", "挑战者", "裁决者"}[role]
    m.chatview.AppendTextDelta(
        fmt.Sprintf("\n══════════ Round %d/%d — %s (%s) ══════════\n",
            round, rs.totalRounds, roleName, provider.Model()))
    
    ctx := m.startTurn()
    m.eventCh = forked.Agent().RunOneOffStream(ctx, provider,
        m.systemPrompt, userPrompt, reviewOpts,
        agent.OneOffMeta{Kind: fmt.Sprintf("review-round-%d", round), SessionID: m.currentSessionID()})
}
```

#### 3. `TurnComplete` 事件处理修改

```go
case agent.AgentEventTurnComplete:
    if m.reviewState != nil {
        rs := m.reviewState
        
        // 记录本轮报告路径
        if m.forkedAgent != nil {
            path := m.forkedAgent.Agent().LastOneoffTranscriptPath()
            rs.reportPaths = append(rs.reportPaths, path)
            m.forkedAgent.Close()
            m.forkedAgent = nil
        }
        
        // 显示"报告已保存"提示
        if len(rs.reportPaths) > 0 {
            lastPath := rs.reportPaths[len(rs.reportPaths)-1]
            m.chatview.AppendTextDelta(fmt.Sprintf(
                "\n💾 报告已保存: %s\n", lastPath))
        }
        
        // 还有下一轮？
        if rs.currentRound < rs.totalRounds {
            return m.startReviewRound()  // 不恢复历史
        }
        
        // 所有轮次完成！
        m.chatview.AppendTextDelta(fmt.Sprintf(
            "\n✅ 对抗式审查完成 (%d/%d rounds)\n最终报告: %s\n",
            rs.totalRounds, rs.totalRounds, rs.reportPaths[len(rs.reportPaths)-1]))
        
        m.isReviewing = false
        m.reviewState = nil
        // fall through 到正常 one-off 收尾（恢复历史等）
    }
    
    // 原有 TurnComplete 逻辑...
```

#### 4. 用户输入阻塞

```go
// model.go Update() 中
if m.isReviewing {
    // /review 进行中，阻塞用户输入
    m.chatview.AddMessage(chatMessage{
        Role:    "assistant",
        Content: "对抗式审查进行中，请等待完成",
    })
    return m, nil
}
```

#### 5. Ctrl+C 中断

```go
// model.go 中处理 Ctrl+C
if m.isReviewing {
    // 中断审查
    if m.cancelFunc != nil {
        m.cancelFunc()
        m.cancelFunc = nil
    }
    m.isReviewing = false
    m.reviewState = nil
    if m.forkedAgent != nil {
        m.forkedAgent.Close()
        m.forkedAgent = nil
    }
    if m.savedHistory != nil {
        m.history = m.savedHistory
        m.savedHistory = nil
    }
    m.setState(stateIdle)
    m.chatview.AddMessage(chatMessage{
        Role:    "assistant",
        Content: "⏹️ 对抗式审查已取消",
    })
}
```

### ACP 端

ACP 端逻辑相似，但不需要 TUI 的状态管理。ACP handler 可以直接串行执行多轮：

```go
func handleACPReview(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
    rounds := parseReviewRounds(args, defaultRounds)
    // ...
    
    for round := 1; round <= rounds; round++ {
        role := resolveRole(round, rounds)
        userPrompt := BuildReviewPrompt(role, round, rounds, reportPaths)
        
        forked := aiAgent.Fork(...)
        eventCh := forked.Agent().RunOneOffStream(...)
        stopReason, _ := streamToACP(ctx, sess, conn, eventCh)
        forked.Close()
        
        if stopReason == acp.StopReasonMaxTurns {
            break  // 预算耗尽，提前结束
        }
    }
    
    sess.history = nil
    return acp.StopReasonEndTurn, nil
}
```

---

## 七、Model 分配

### 解析逻辑

```go
func resolveRoundModels(rc reviewResolved, rounds int) []llm.Provider {
    // 情况 1: adversarial.models 未配置 → 全部用 review.provider
    if len(rc.adversarialModels) == 0 {
        providers := make([]llm.Provider, rounds)
        for i := range providers {
            providers[i] = rc.provider
        }
        return providers
    }
    
    // 情况 2: 只有一个模型 → 全部用同一个
    if len(rc.adversarialModels) == 1 {
        providers := make([]llm.Provider, rounds)
        for i := range providers {
            providers[i] = rc.adversarialModels[0]
        }
        return providers
    }
    
    // 情况 3: 多个模型 → 按索引分配，不够取模
    providers := make([]llm.Provider, rounds)
    for i := range providers {
        providers[i] = rc.adversarialModels[i % len(rc.adversarialModels)]
    }
    return providers
}
```

### 配置示例与对应分配

**例 1：单一模型，6 轮**

```yaml
review:
  provider: claude-sonnet
  adversarial:
    enabled: true
    rounds: 6
    # models 未设置
```

→ 所有 6 轮使用 `claude-sonnet`

**例 2：3 个模型，6 轮**

```yaml
review:
  adversarial:
    enabled: true
    rounds: 6
    models: [claude-sonnet, gpt-4o, claude-opus]
```

→ R1=sonnet, R2=4o, R3=opus, R4=sonnet, R5=4o, **R6=opus**（最后一轮是 opus 作为最终 Judge）

**例 3：2 个模型，5 轮**

```yaml
review:
  adversarial:
    enabled: true
    rounds: 5
    models: [claude-sonnet, gpt-4o]
```

→ R1=sonnet, R2=4o, R3=sonnet, R4=4o, **R5=sonnet**（最后一轮取模为 sonnet）

如果想让最后一轮固定为更强的模型，用户应该配够轮次。

---

## 八、文件命名与路径

### 中间产物

```
.tachi/reviews/
  ├── round-1-<YYYYMMDD-HHmm>-<summary>-review.md
  ├── round-2-<YYYYMMDD-HHmm>-<summary>-challenge.md
  ├── round-3-<YYYYMMDD-HHmm>-<summary>-judge.md
  └── ...
```

`<summary>` 由第 1 轮的 LLM 生成（同现有逻辑），后续轮次沿用同一个 summary。

### 旁路记录

每轮各自有一个 oneoff transcript：

```
<session>/<id>/oneoff/
  ├── review-round-1-<ts>-<uuid>.jsonl
  ├── review-round-2-<ts>-<uuid>.jsonl
  └── review-round-3-<ts>-<uuid>.jsonl
```

`OneOffMeta.Kind` 从 `"review"` 变为 `"review-round-1"`、`"review-round-2"` 等。

---

## 九、改动文件清单

| 文件 | 改动量 | 说明 |
|------|--------|------|
| `config/config.go` | ~15 行 | `ReviewConfig` 新增 `Adversarial *AdversarialReviewConfig` |
| `agent/commands/prompts.go` | ~100 行 | 新增 `BuildReviewPrompt()`、角色枚举、角色名映射 |
| `tui/commands.go` | ~80 行 | `sendReviewCommand()` 改造 + `startReviewRound()` + `parseReviewRounds()` |
| `tui/model_events.go` | ~40 行 | `TurnComplete` 中插入多轮调度分支 |
| `tui/model.go` | ~20 行 | 新增 `reviewState`、`isReviewing` 字段 + 用户输入阻塞 + Ctrl+C 处理 |
| `agent/acp/commands.go` | ~50 行 | `handleACPReview` 改为串行多轮 |
| `agent/commands/commands.go` | ~5 行 | `ReviewUserPrompt` 改为调用 `BuildReviewPrompt`（可选） |

**不需要改动：**
- `agent/agent_fork.go` — 完全复用
- `agent/agent_loop.go` — 完全复用
- `agent/oneoff_recorder.go` — 完全复用

---

## 十、与现有行为的关系

| 场景 | 行为 |
|------|------|
| `adversarial.enabled: false`（默认） | 现有单轮 `/review`，完全不变 |
| `adversarial.enabled: true`, `rounds: 1` | 等同于单轮 |
| `adversarial.enabled: true`, `rounds: 0` | 等同于单轮 |
| `/review`（无参数） | 用配置的 `rounds` |
| `/review 5` | 5 轮，忽略配置的 `rounds` |
| `/review 1` | 单轮（无论配置如何） |
| 配置中 `adversarial` 不存在（旧配置） | `Adversarial` 为 `nil`，退化为单轮 |

---

## 十一、未来可能的扩展

- **Cycle-aware prompt 差异化** — 同一角色在不同 cycle 中收到不同的指令（如 "聚焦未决争议" vs "全面覆盖"）
- **并行化** — 同 cycle 内的多个 Challenger 并行跑
- **自定义角色周期** — 用户配置角色序列而非硬编码 `[Reviewer, Challenger, Judge]`
- **中途打断继续** — 每轮结束后暂停，用户可决定跳过或继续
- **报告对比视图** — TUI 中展示多轮报告的分歧对比
