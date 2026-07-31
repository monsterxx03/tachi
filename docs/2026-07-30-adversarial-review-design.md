# 对抗式代码审查 (Adversarial Review) 设计文档

> 日期: 2026-07-30 | 状态: 设计定稿，可直接实现

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
    ├─ N 可选：覆盖配置中的 rounds（N≥2 时同时覆盖 enabled: false）
    │
    ├─ 编排器创建报告目录 .tachi/reviews/<YYYYMMDD-HHmm>/（os.MkdirAll）
    │
    ├─ Round 1 ─ Reviewer ──────────────┐
    │  fork + RunOneOffStream            │ git diff → 初步审查报告
    │  prompt: "你是第 1 轮审查者..."     │ → <dir>/round-1-review-<model>.md
    │                                    │
    ├─ Round 2 ─ Challenger ─────────────┤
    │  fork + RunOneOffStream            │ git diff + round-1 报告 → 挑战/补充
    │  prompt: "你是第 2 轮挑战者..."     │ → <dir>/round-2-challenge-<model>.md
    │                                    │
    ├─ Round 3 ─ Judge ─────────────────┤
    │  fork + RunOneOffStream            │ git diff + round-1 + round-2 → 裁决
    │  prompt: "你是第 3 轮裁决者..."     │ → <dir>/round-3-judge-<model>.md
    │                                    │
    ├─ Round N ─ (角色周期轮转) ─────────┤
    │  ...                               │ 最终轮固定为 Judge/Summarizer
    │                                    │
    └─ 恢复对话历史，展示最终报告路径
```

**报告路径由编排器拥有**：每轮的输出路径在启动该轮**之前**就已确定并写进 prompt（见第五节），每轮结束后编排器用 `os.Stat` 校验落盘（见第六节）。LLM 不自拟文件名——这是多轮信息链能成立的前提。

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
| 4 | Reviewer | 同 R1（首版无 cycle 差异化，仍覆盖全部变更） |
| 5 | **Judge** (固定) | 最终裁决 |

示例（rounds=2）：

| 轮次 | 角色 | 说明 |
|------|------|------|
| 1 | Reviewer | 全面审查 |
| 2 | **Judge** (固定) | 最终裁决（跳过了 Challenger 阶段） |

> ⚠️ **rounds % 3 == 1 时会出现连续两轮 Judge**（rounds=4/7/10 → `[..., Judge, Judge]`）：取模轮转把第 N-1 轮定为 Judge，最终轮又固定为 Judge。首版接受此行为——中间 Judge 只标记争议区域（"中间裁决"），最终 Judge 做逐个裁决（"最终执行摘要"），职责有区分、prompt 已差异化。若后续希望避免，可让"最终轮固定"只在 `(rounds-1)%3 != 2` 时生效。

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
    enabled: false                  # false = 无参 /review 保持单轮（显式 /review N 仍可开启）
    rounds: 3                       # 对抗轮次（默认 3，有效范围 1-10，代码内 clamp）
    models:                         # 各轮 model 分配（可选）
      - claude-sonnet               # 按索引分配各轮
      - gpt-4o
      - claude-opus
    judge_model: claude-opus        # 最终轮固定 model（可选；空 = 维持取模分配）
    # model_strategy 隐式由 models 列表长度决定：
    #   未设置或空 → 所有轮次用 review.provider
    #   只有一个模型 → 所有轮次用同一个
    #   多个模型 → 按索引分配，不够则取模轮转
    # judge_model 设置后，最终轮固定使用它，其余轮次仍按上述规则分配
```

### `AdversarialReviewConfig` 结构体

```go
type AdversarialReviewConfig struct {
    Enabled    bool     `yaml:"enabled"`            // 开关，默认 false
    Rounds     int      `yaml:"rounds" default:"3"` // 对抗轮次（代码内 clamp 到 [1, 10]）
    Models     []string `yaml:"models"`             // 各轮 model 名（可选）
    JudgeModel string   `yaml:"judge_model"`        // 最终轮固定 model 名（可选，空 = 取模）
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

> ⚠️ **creasty/defaults 行为（实现代码必须遵守）**：
>
> - `defaults.Set()` **不会**自动分配 nil 指针字段：`shouldInitializeField`（defaults.go）对 **nil 指针且无 default tag** 的字段返回 false，直接跳过。因此未配置 `adversarial:` 时 `ReviewConfig.Adversarial` **恒为 nil**——判断是否启用对抗模式**必须同时检查 nil 与 `Enabled`**（`adv != nil && adv.Enabled`），只查 `Enabled` 会 nil pointer panic（实测 creasty/defaults v1.8.0）。
> - 显式配置 `adversarial:`（哪怕只有 `enabled: false`）时指针非 nil，其内部字段会递归应用 default tag；此时 `rounds: 0` 是 zero value，会被 `default:"3"` **覆盖成 3**。config 层无法表达 "0 = 单轮"；要单轮请用 `enabled: false` 或 `/review 1`。
> - `rounds` 负数不会被 default 修正，需在代码中 clamp（见第四节），否则 `make([]llm.Provider, rounds)` 会 panic。

---

## 四、命令行参数

### `/review [rounds]`

轮次来源优先级：

1. `/review N`（N ≥ 2）→ N 轮，**覆盖 `enabled: false`**（显式参数视为用户明确要对抗模式）
2. `/review 0`、`/review 1`、非数字参数 → 单轮
3. `/review`（无参数）→ 配置值：`adversarial.enabled: true` 时用 `rounds`（default tag 保证默认 3），否则单轮
4. 所有来源统一 clamp 到 `[1, 10]`

解析逻辑（放 **`agent/commands`** 共享包并**导出**，TUI 与 ACP 两端复用——`ResolveReviewRounds`/`ResolveRoundModels` 都是纯函数，**不能放 `tui/`**，否则 `agent/acp` 导入会构成循环依赖）：

```go
const maxReviewRounds = 10

// ResolveReviewRounds 决定本次 /review 的轮数。input 为完整输入（如 "/review 6"）。
func ResolveReviewRounds(input string, cfg *config.Config) int {
    cfgRounds := 1
    if cfg != nil {
        // 注意：未配置 adversarial 时指针为 nil（defaults 不分配），
        // 显式配置后非 nil —— 必须同时判 nil 与 Enabled，只判其一都会出错。
        if adv := cfg.Review.Adversarial; adv != nil && adv.Enabled {
            cfgRounds = adv.Rounds
        }
    }
    if parts := strings.Fields(input); len(parts) >= 2 {
        n, err := strconv.Atoi(parts[1])
        if err != nil {
            return 1 // 非数字参数（如 "/review foo"）→ 单轮；不静默回落配置轮次，拼写错误不升级为 N 倍成本
        }
        if n <= 0 {
            return 1 // /review 0 或负数 → 单轮
        }
        return min(n, maxReviewRounds) // 显式参数覆盖 enabled
    }
    // cfgRounds 可能为负数（defaults 不修正负数），min+max 双 clamp 不可省
    return max(1, min(cfgRounds, maxReviewRounds))
}
```

当返回 `1` 时，走现有单轮路径（老 prompt、LLM 自拟文件名，完全不变）。

---

## 五、Prompt 设计

每轮的 prompt 由统一的 builder 构造，接受角色 + 轮次 + 总轮数 + **本轮的确切输出路径** + 前序报告状态，输出完整的 user message。

**关键约束：所有路径都是编排器生成的确定路径，prompt 中不出现任何占位符。** 前序报告是否成功落盘也如实告知 LLM。

### `BuildReviewPrompt(role ReviewRole, round, totalRounds int, outPath string, prev []roundReport) string`

```go
type ReviewRole int

const (
    RoleReviewer   ReviewRole = iota // 0
    RoleChallenger                   // 1
    RoleJudge                        // 2
)

// roundReport 记录一轮前序报告的状态（prompt builder 与编排器共用）。
type roundReport struct {
    Round int
    Path  string // 编排器分配的期望路径
    Saved bool   // 是否已成功落盘（false 时 prompt 中标注"缺失，跳过"）
}

// resolveRole 返回某轮的角色；最终轮固定为 Judge。
func resolveRole(round, totalRounds int) ReviewRole {
    if round == totalRounds {
        return RoleJudge
    }
    return ReviewRole((round - 1) % 3)
}

// roleFileSuffix 返回报告文件名中的角色后缀。
func roleFileSuffix(r ReviewRole) string {
    return []string{"review", "challenge", "judge"}[r]
}

// ReportPathFor 返回某轮报告的确切路径，格式 round-<N>-<role>-<model>.md。
// startReviewRound（写指令）与 recordReviewReport（落盘校验）必须共用它，
// 保证两处引用的是同一个文件。
func ReportPathFor(dir string, round int, role ReviewRole, model string) string {
    return fmt.Sprintf("%s/round-%d-%s-%s.md", dir, round, roleFileSuffix(role), sanitizeFileName(model))
}

// sanitizeFileName 将模型 ID 中的路径非法字符替换为 '-'（如 "qwen3:32b" → "qwen3-32b"）。
func sanitizeFileName(s string) string { ... }
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

完成后用 WriteFile 保存报告到：<outPath>（编排器给出的确切路径，目录已创建，无需 mkdir）
```

**Round 2 — Challenger prompt 概要：**

```
你是代码审查的第 2 轮挑战者 (Round 2/3 — Challenger)。
你的队友 (Reviewer) 已完成第 1 轮审查。

前序报告：
  - Round 1 (Reviewer): <确切路径> — 用 ReadFile 阅读
（若某轮 Saved=false，则该轮标注为：注意：第 K 轮未能成功保存报告，跳过）

任务：
1. 用 ReadFile 阅读第 1 轮报告
2. 标注每条发现的立场：
   - ✅ Agree — 同意并补充理由
   - ❌ Disagree — 反驳并说明理由
   - ➕ Addition — 全新的发现
3. 特别注意 Reviewer 可能遗漏的 edge case、安全面、性能瓶颈

完成后用 WriteFile 保存报告到：<outPath>
```

**Round N — Judge prompt 概要：**

```
你是代码审查的第 N 轮最终裁决者 (Round N/N — Judge)。
你的队友已完成前 N-1 轮。

前序报告（逐条列出确切路径；Saved=false 的轮次标注"未保存，跳过"）：
  - Round 1 (Reviewer):    <path-1>
  - Round 2 (Challenger):  <path-2>
  - ...

任务：
1. 阅读所有前序报告
2. 对每一条分歧做出最终裁决（Confirmed / Disputed / Rejected）
3. 生成统一的最终报告，按严重性排序

完成后用 WriteFile 保存报告到：<outPath>
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

核心改动在 `tui/commands.go`、`tui/model_events.go` 和 `tui/model.go`。

```
Model 新增字段:

type reviewState struct {
    totalRounds  int              // 总轮数
    currentRound int              // 当前轮 (1-based)
    roundModels  []llm.Provider   // 各轮 model（长度 = totalRounds）
    reportDir    string           // .tachi/reviews/<YYYYMMDD-HHmm>（编排器创建）
    reports      []roundReport    // 各轮报告状态（有序，含未落盘标记）
    config       reviewResolved   // 共享配置（allowedTools, maxIterations, thinking）
}

Model 新增:
- reviewState *reviewState       // nil = 不在对抗审查中
- isReviewing bool               // 对抗审查进行中（用于阻塞用户输入）
```

> 💰 **成本提示**：N 轮 = N 次独立 fork + N 次完整 LLM 调用，每轮都要重新跑 git diff（现有 `ReviewUserPrompt` 的上下文收集逻辑），总成本约为单轮审查的 **N 倍**。`rounds` 上限 10 意味着 10 倍成本——usage 已逐轮累积进 `/usage`，建议在 UI 启动横幅中顺带提示总轮数，避免用户误配大轮次。首版不做跨轮 diff 缓存（每轮独立 fork、上下文隔离是设计约束）。

**贯穿全节的两条状态机纪律：**

1. **`savedHistory` 是 one-off 的唯一标记**（`isOneOff := m.savedHistory != nil`，model_events.go）。多轮期间它必须**一直保持非 nil**，直到最终轮的 `TurnComplete` 由正常分支恢复——中间任何提前恢复都会让后续事件的 `m.history = event.Messages` 用审查轮消息污染主历史。
2. **取消/失败产生的是 `AgentEventError` 而非 `TurnComplete`**（agent_loop.go 的 ctx.Done 分支）。多轮调度必须同时挂在两个事件分支上，缺一不可。

#### 1. `sendReviewCommand()` 修改

```go
func (m *Model) sendReviewCommand() tea.Cmd {
    rounds := cmds.ResolveReviewRounds(m.subcommandInput, m.cfg)

    m.savedHistory = make([]llm.Message, len(m.history))
    copy(m.savedHistory, m.history)

    if rounds == 1 {
        // 现有单轮逻辑，完全不变
        ...
    }

    rc := m.resolveReviewConfig()

    // 名字 → provider 解析，fail fast：任一配置了但无法解析的名字
    // → 报错并中止，不开始第 1 轮（静默回退会让"多模型对抗"名存实亡）。
    // 解析出来的 []llm.Provider 交给共享的 ResolveRoundModels 做轮次分配；
    // 注意该函数是纯分配不报错，"配置了但解析失败"的检查在此完成
    // （resolvedModels 里出现 nil provider → 视为解析失败，报错中止）。
    resolvedModels, resolvedJudge, fallbackProvider, err := m.resolveAdversarialProviders(rc)
    if err != nil {
        m.savedHistory = nil
        m.chatview.AddMessage(chatMessage{Role: "error", Content: err.Error()})
        m.setState(stateIdle)
        return nil
    }
    roundModels := cmds.ResolveRoundModels(resolvedModels, resolvedJudge, fallbackProvider, rounds)

    // 编排器拥有报告目录：启动前创建，路径对 LLM 可见且确定。
    // 注意：精度必须到秒（20060102-150405）——分钟级会导致同一分钟内
    // 连续两次 /review 共用一个目录，round-N 报告互相覆盖（MkdirAll 幂等不报错）。
    reportDir := fmt.Sprintf(".tachi/reviews/%s", time.Now().Format("20060102-150405"))
    if err := os.MkdirAll(reportDir, 0o755); err != nil {
        m.savedHistory = nil
        m.chatview.AddMessage(chatMessage{Role: "error", Content: "创建报告目录失败: " + err.Error()})
        m.setState(stateIdle)
        return nil
    }

    m.reviewState = &reviewState{
        totalRounds: rounds,
        roundModels: roundModels,
        reportDir:   reportDir,
        config:      rc,
    }
    m.isReviewing = true
    return m.startReviewRound() // 启动第 1 轮（内部含 statusbar.Tick + nextEvent）
}
```

#### 2. `startReviewRound()` 新增

```go
func (m *Model) startReviewRound() tea.Cmd {
    rs := m.reviewState
    rs.currentRound++
    round := rs.currentRound
    role := resolveRole(round, rs.totalRounds)

    provider := rs.roundModels[round-1]

    // 本轮的确切输出路径（编排器分配，prompt 中无占位符；文件名含模型名）
    outPath := cmds.ReportPathFor(rs.reportDir, round, role, provider.Model())
    userPrompt := cmds.BuildReviewPrompt(role, round, rs.totalRounds, outPath, rs.reports)

    forked := m.agent.Fork(agent.ForkConfig{
        Provider:      provider,
        MaxIterations: rs.config.maxIterations,
        AllowedTools:  rs.config.allowedTools,
        Logger:        m.agent.Logger(),
    })
    m.forkedAgent = forked

    // 每轮重置流式状态（上一轮的气泡已在 TurnComplete 分支 FinishStreaming 封口；
    // 第 1 轮由 sendReviewCommand 做过一次，幂等）
    m.setState(stateWaiting)
    m.chatview.ResetStreaming()
    m.thinkingView.Reset()

    // 显示轮次标题
    roleName := []string{"审查者", "挑战者", "裁决者"}[role]
    m.chatview.AppendTextDelta(
        fmt.Sprintf("\n══════════ Round %d/%d — %s (%s) ══════════\n",
            round, rs.totalRounds, roleName, provider.Model()))

    // reviewOpts 同现有单轮逻辑（chatOpts + thinking 覆盖），在 sendReviewCommand 构造一次复用
    ctx := m.startTurn() // 每轮新 ctx + streamGen++（上一轮迟到的事件自动失效）
    m.eventCh = forked.Agent().RunOneOffStream(ctx, provider,
        m.systemPrompt, userPrompt, reviewOpts,
        agent.OneOffMeta{Kind: fmt.Sprintf("review-round-%d", round), SessionID: m.currentSessionID()})
    return tea.Batch(m.statusbar.Tick(), m.nextEvent())
}
```

#### 3. `TurnComplete` 事件处理修改

插入位置：`case agent.AgentEventTurnComplete:` 的**最顶部**（在 `m.history = event.Messages` 和 compact 处理之前）——中间轮全程不碰 `m.history`，`savedHistory` 保持非 nil。

```go
case agent.AgentEventTurnComplete:
    if m.reviewState != nil {
        rs := m.reviewState

        if rs.currentRound < rs.totalRounds {
            // ===== 中间轮：完整收尾后链式启动下一轮 =====
            m.finishReviewRound(event)         // 收尾动作抽成 helper（见下），与错误分支共用
            return m.startReviewRound()        // 不恢复历史，不进正常分支
        }

        // ===== 最终轮：清状态，fall through 走正常 one-off 收尾 =====
        // 保留 m.forkedAgent —— 正常分支会在 Close 前读取旁路记录路径
        // （若在这里提前 Close，正常分支会退化为读 m.agent 的陈旧路径，
        //   末尾的"📄 旁路记录"将指向错误文件）。
        // usage 累积、FinishStreaming、savedHistory 恢复也都由正常分支完成。
        m.recordReviewReport(rs)
        m.isReviewing = false
        m.reviewState = nil
        m.chatview.AppendTextDelta(fmt.Sprintf(
            "\n✅ 对抗式审查完成 (%d/%d rounds)\n", rs.totalRounds, rs.totalRounds))
        // 多轮耗时长，完成时主动通知（正常分支对 one-off 不通知）
        if m.notifyOnComplete && !herdrNotifications(m.cfg) {
            notifyTerminal("tachi", "对抗式审查完成")
        }
        // fall through
    }

    // 原有 TurnComplete 逻辑...
```

落盘校验（编排器自行 `os.Stat`，不依赖 LLM 自觉）：

```go
// finishReviewRound 执行一轮结束时的收尾动作，中间轮与 AgentEventError
// 分支共用，避免"正常分支的收尾动作一个都不能省"在多处复制后漂移。
func (m *Model) finishReviewRound(event agent.AgentEvent) {
    rs := m.reviewState
    m.chatview.FinishStreaming()       // 封口上一轮流式气泡
    if event.Usage != nil {
        m.accumulateUsage(event.Usage) // 每轮成本都计入 /usage（N 倍成本更要可见）
    }
    m.recordReviewReport(rs)           // 校验报告落盘 + UI 提示（见下）
    if m.forkedAgent != nil {
        m.forkedAgent.Close()
        m.forkedAgent = nil
    }
}

// recordReviewReport 校验本轮报告是否落盘，更新 rs.reports 并在 UI 提示。
func (m *Model) recordReviewReport(rs *reviewState) {
    role := resolveRole(rs.currentRound, rs.totalRounds)
    model := rs.roundModels[rs.currentRound-1].Model()
    path := cmds.ReportPathFor(rs.reportDir, rs.currentRound, role, model)
    _, err := os.Stat(path)
    saved := err == nil
    rs.reports = append(rs.reports, roundReport{Round: rs.currentRound, Path: path, Saved: saved})
    if saved {
        m.chatview.AddMessage(chatMessage{Role: "oneoff_note", Content: "💾 报告已保存: " + path})
    } else {
        // 策略：继续链，下一轮 prompt 会标注该轮缺失（BuildReviewPrompt 的 prev 参数）
        m.chatview.AddMessage(chatMessage{Role: "error",
            Content: fmt.Sprintf("⚠️ 第 %d 轮未成功保存报告，后续轮次将跳过它", rs.currentRound)})
    }
}
```

#### 4. `AgentEventError` 分支修改

任何一轮中途 API 失败/超时/取消时，agent loop 发的是 `AgentEventError` 而非 `TurnComplete`。现有 error 分支（model_events.go:345）会恢复 `savedHistory`、关 fork、清队列、`setState(stateIdle)`，但不知道 `reviewState`——不清理的话 `isReviewing` 永远为 true，用户输入被永久阻塞，`reviewState` 还会泄漏到下一次 `/review`。

```go
case agent.AgentEventError:
    // 对抗式审查中断清理（放在现有逻辑之前）
    wasReviewing := m.reviewState != nil
    if wasReviewing {
        m.reviewState = nil
        m.isReviewing = false
    }

    // ... 现有逻辑（compactForSwitch 检查、savedHistory 恢复、fork 关闭、清 pendingQueue）...

    if event.Result != nil && event.Result.ExitReason == agent.ExitReasonInterrupted {
        m.chatview.FinishStreaming()
        if wasReviewing {
            m.chatview.AddMessage(chatMessage{Role: "assistant", Content: "⏹️ 对抗式审查已取消"})
        }
    } else {
        // 现有错误提示——审查轮失败时链自然终止（reviewState 已在上面清理），无需额外处理
        ...
    }
```

#### 5. 用户输入阻塞

```go
// model.go Update() 输入处理中，stateStreaming 分支**之前**
// （与 isResearching 阻塞同位置，参照 model.go:532）：
if m.isReviewing {
    m.chatview.AddMessage(chatMessage{
        Role:    "assistant",
        Content: "⚔️ 对抗式审查进行中，请等待完成",
    })
    return m, nil
}
```

**为什么必须在 `stateStreaming` 分支之前**：流式期间的用户输入会进入 `pendingQueue`（model.go:565），而 agent 在工具边界发起 steer 检查时，队列内容会被 join 后经 `steerCh` **注入正在运行的审查 fork**（model_events.go 的 steer 分支）——破坏轮次隔离、污染审查结论，还会被记进该轮的 transcript。拦截在入队之前，steer drain 发出的就是空串，审查轮不受干扰。

#### 6. Ctrl+C 中断

```go
// handleCtrlC() 中，通用 streaming 取消分支之前插入：
if m.isReviewing && m.cancelFunc != nil {
    m.cancelFunc()
    // 关键：不立即恢复 savedHistory、不 setState(idle)、不清 reviewState。
    // 取消的流会尾随一个 AgentEventError(ExitReasonInterrupted)，由 error
    // 分支统一完成：savedHistory 恢复、fork 关闭、reviewState 清理、
    // setState(idle)（见第 4 点）。
    // 若在这里提前恢复 savedHistory，尾随事件的 m.history = event.Messages
    // 会把被取消轮次的 fork 消息（git diff、工具调用）写进主历史——污染。
    m.pendingQueue = nil
    m.chatview.RemovePendingItems()
    m.statusbar.SetPendingCount(0)
    m.chatview.MarkPendingToolsInterrupted()
    // 与通用 streaming 取消分支保持一致（tui/model.go 已实现）：
    // 0) 清理模态 —— review fork 的工具集含 Bash，可能触发权限确认模态；
    //    不清的话 pendingConfirm/askUserView 悬挂到错误事件落地之后（error
    //    分支不负责清模态），下次 ToolConfirmation 会覆盖但 UI 状态已脏。
    m.pendingConfirm = nil
    m.askUserView = nil
    // 1) 杀掉后台进程 —— review fork 共享父的 ProcessManager（agent_fork.go），
    //    fork 内 background=true 启动的进程同样要随 Ctrl+C 终止；
    // 2) 继续读 eventCh —— 尾随的 AgentEventError 需要被 nextEvent 接住才能
    //    触发 error 分支的清理（review 轮无确认模态、旧 cmd 链通常存活，
    //    但显式排队更稳健，与通用分支同源）。
    if m.agent != nil {
        m.agent.KillBackgroundProcesses()
    }
    if m.eventCh != nil {
        return m, m.nextEvent()
    }
    return m, nil
}
```

### ACP 端

ACP 端无 TUI 状态机，直接串行循环，但同样的纪律适用（编排器拥有路径、落盘校验、终止条件完整）。多模型分配同样复用共享的 `cmds.ResolveRoundModels()`（见第四节——纯函数在 `agent/commands`，两端行为一致）：

```go
func handleACPReview(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
    rounds := cmds.ResolveReviewRounds("/review "+args, sess.cfg)
    if rounds == 1 {
        // 现有单轮逻辑，完全不变
        ...
    }

    // 名字 → provider 解析（fail fast：无法解析则返回错误，不开始第 1 轮）
    // 与 TUI 端相同的三来源：adversarial.models（取模）→ judge_model（最终轮固定）
    // → review.provider / 主 provider（fallback）。解析失败 → 直接返回错误。
    // 注意 ResolveRoundModels 本身是纯分配不报错；"配置了但解析失败"的检查
    // 必须在调用前完成（models 列表里出现 nil provider → 视为解析失败）。
    roundModels := cmds.ResolveRoundModels(resolvedModels, resolvedJudge, fallbackProvider, rounds)

    // reportDir 用秒级精度（20060102-150405），避免同分钟多次 /review 撞目录
    // os.MkdirAll(reportDir)
    ...

    var reports []roundReport
    for round := 1; round <= rounds; round++ {
        role := resolveRole(round, rounds)
        provider := roundModels[round-1]
        outPath := cmds.ReportPathFor(reportDir, round, role, provider.Model())
        prompt := cmds.BuildReviewPrompt(role, round, rounds, outPath, reports)

        forked := aiAgent.Fork(...)
        stopReason, err := streamToACP(ctx, sess, conn,
            forked.Agent().RunOneOffStream(ctx, provider, systemPrompt, prompt, opts,
                agent.OneOffMeta{Kind: fmt.Sprintf("review-round-%d", round), SessionID: acpOneoffSessionID(sess)}))
        forked.Close()

        // 落盘校验，更新 reports（缺失轮次在后续 prompt 中标注）
        _, statErr := os.Stat(outPath)
        reports = append(reports, roundReport{Round: round, Path: outPath, Saved: statErr == nil})

        // 任何非自然完成都终止链：MaxTurns / Cancelled / Refusal / 传输错误
        if err != nil || stopReason != acp.StopReasonEndTurn {
            sess.history = nil
            return stopReason, err
        }
    }

    sess.history = nil
    return acp.StopReasonEndTurn, nil
}
```

---

## 七、Model 分配

### 解析逻辑

名字 → provider 的解析复用 `SetupReviewProvider` 的模式（`agent/agent_provider.go`）：Configure 阶段把 `adversarial.models` / `judge_model` 逐个解析为 `llm.Provider`（无法解析的记为 nil 并打 warn 日志）。`sendReviewCommand` / `handleACPReview` 在第 1 轮启动**之前**检查：配置了但解析失败的条目 → 报错中止（fail fast）。静默回退主模型会让"多模型对抗"名存实亡而用户不知情。

```go
// ResolveRoundModels 把解析后的 provider 列表分配到各轮。
// 放 agent/commands 共享包（与 ResolveReviewRounds 同处），TUI 与 ACP 复用。
// models 为空 → 全部用 fallback（review.provider 或主 provider）；
// 非空 → 按索引分配，不够取模；judge 非 nil → 最终轮固定。
func ResolveRoundModels(models []llm.Provider, judge, fallback llm.Provider, rounds int) []llm.Provider {
    providers := make([]llm.Provider, rounds)
    for i := range providers {
        if len(models) == 0 {
            providers[i] = fallback
        } else {
            providers[i] = models[i%len(models)]
        }
    }
    if judge != nil {
        providers[rounds-1] = judge // 最终轮固定
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

**例 4：judge_model 固定最终轮**

```yaml
review:
  adversarial:
    enabled: true
    rounds: 5
    models: [claude-sonnet, gpt-4o]
    judge_model: claude-opus
```

→ R1=sonnet, R2=4o, R3=sonnet, R4=4o, **R5=opus**（最终轮固定，不受取模影响）

取模分配下最终轮的模型取决于 `rounds % len(models)`（例 3 落在了较弱的 sonnet 上）；想让最终裁决固定用更强的模型，配置 `judge_model`。

---

## 八、文件命名与路径

### 中间产物（编排器拥有的确定路径）

```
.tachi/reviews/
  └── <YYYYMMDD-HHmmss>/          # 一次对抗审查一个目录（编排器 os.MkdirAll；秒级精度，
        │                         #   避免同一分钟内连续两次 /review 共用目录导致报告互相覆盖）
        ├── round-1-review-claude-sonnet-4-20250514.md
        ├── round-2-challenge-gpt-4o.md
        ├── round-3-judge-claude-opus-4-20250514.md
        └── ...
```

文件名格式 `round-<N>-<role>-<model>.md`：模型名取自 `provider.Model()`，经 `sanitizeFileName()` 处理（`:`、`/` 等路径非法字符替换为 `-`，如 `qwen3:32b` → `qwen3-32b`）。多模型对抗时一眼可辨每轮报告的出处。

路径在启动每轮**之前**就已确定并写进 prompt；每轮结束后编排器 `os.Stat` 校验落盘。这是多轮信息链成立的前提——编排器无法预知 LLM 运行时自拟的文件名。

**取舍说明**：对抗模式的文件名不含 `<summary>`（单轮模式沿用 LLM 生成 summary 的命名，见下），牺牲一点可浏览性换取路径确定性。summary/标题体现在报告正文；目录按时间排序。未来可通过"最终轮完成后重命名目录"找回（见第十一节）。

**单轮模式**（`rounds=1`）保持现有命名 `.tachi/reviews/<timestamp>-<summary>-review.md` 不变——单轮没有下游消费者，不需要确定性路径。两种命名约定并存是有意的（向后兼容优先）。

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
| `config/config.go` | ~15 行 | `ReviewConfig` 新增 `Adversarial *AdversarialReviewConfig`（含 `JudgeModel`） |
| `agent/commands/commands.go` | ~32 行 | 新增共享纯函数 `ResolveReviewRounds()` + `ResolveRoundModels()`（TUI 与 ACP 复用；**不能放 `tui/`**，见第四节）+ `/review` Def 描述补充 `[rounds]` 参数提示 |
| `agent/commands/prompts.go` | ~150 行 | 新增 `BuildReviewPrompt()`、`ReviewRole`/`roundReport` 类型、`resolveRole()`、`roleFileSuffix()`、`ReportPathFor()`、`sanitizeFileName()` |
| `agent/agent_provider.go` | ~40 行 | `adversarial.models`/`judge_model` 的名字→provider 解析（Configure 阶段，配合 fail fast） |
| `tui/commands.go` | ~110 行 | `sendReviewCommand()` 改造 + `startReviewRound()` + `finishReviewRound()` + `recordReviewReport()` + `resolveAdversarialProviders()`（三来源解析 + fail fast 检查，调共享 `ResolveRoundModels`） |
| `tui/model_events.go` | ~60 行 | `TurnComplete` 顶部多轮调度分支 + `AgentEventError` 的 reviewState 清理 |
| `tui/model.go` | ~35 行 | `reviewState`/`isReviewing` 字段 + 输入阻塞（stateStreaming 分支前）+ `handleCtrlC` 分支（杀后台进程 + 排 nextEvent） |
| `agent/acp/commands.go` | ~75 行 | `handleACPReview` 串行多轮 + 完整终止条件（复用共享 `ResolveReviewRounds`/`ResolveRoundModels`） |

**测试计划：**

| 文件 | 说明 |
|------|------|
| `agent/commands/review_test.go` | 纯函数单测：`ResolveReviewRounds`（enabled 开关/nil 与 Enabled 双判/CLI 覆盖/0/负数/非数字参数/超上限 clamp）、`ResolveRoundModels`（空/单/多/取模/judge 固定）、`resolveRole`（周期 + 最终轮固定 Judge） |
| `tui/model_test.go` | 多轮链式调度（仿 `start_turn_test.go` 的事件注入）：中间轮不恢复 history、usage 逐轮累积、报告缺失标记、Ctrl+C 后由 error 分支完成清理、API 失败不残留 `isReviewing` |
| `agent/acp/commands_test.go` | `handleACPReview` 多轮循环：轮次链完整、任一轮非自然完成即终止、`sess.history` 清空、fork 每轮 Close、报告缺失时后续 prompt 携带标记 |

**不需要改动：**
- `agent/agent_fork.go` — 完全复用
- `agent/agent_loop.go` — 完全复用
- `agent/oneoff_recorder.go` — 完全复用

---

## 十、与现有行为的关系

| 场景 | 行为 |
|------|------|
| `adversarial` 未配置或 `enabled: false`（默认），无参 `/review` | 现有单轮，完全不变（注意：defaults 库对 nil 指针且无 default tag 的字段不分配，`Adversarial` 指针**恒为 nil**，必须同时判 `adv != nil && adv.Enabled`，只查 `Enabled` 会 nil pointer panic——见第三节） |
| `enabled: false` + `/review 5` | **5 轮**——显式参数覆盖 `enabled` |
| `enabled: true`，未设 `rounds` | 3 轮（default tag） |
| `enabled: true`，`rounds: 0` | 3 轮（zero value 被 default 覆盖；config 层表达不了"0=单轮"，要单轮用 `enabled: false` 或 `/review 1`） |
| `enabled: true`，`rounds: 1` | 单轮 |
| `/review 0` / `/review 1` / 非数字参数 | 单轮（无论配置如何） |
| 任意来源 rounds > 10 | clamp 到 10 |
| `judge_model` 已配置 | 最终轮固定使用，其余轮按 models 取模 |
| 任一 models 条目无法解析为 provider | 第 1 轮启动前报错中止（fail fast） |
| 某轮未成功保存报告 | 链继续，后续轮次 prompt 标注该轮缺失 |
| 中途 API 失败 / Ctrl+C | 链终止，history 恢复，`isReviewing` 清理（`AgentEventError` 分支统一处理） |

---

## 十一、未来可能的扩展

- **Cycle-aware prompt 差异化** — 同一角色在不同 cycle 中收到不同的指令（如 "聚焦未决争议" vs "全面覆盖"）
- **并行化** — 同 cycle 内的多个 Challenger 并行跑
- **自定义角色周期** — 用户配置角色序列而非硬编码 `[Reviewer, Challenger, Judge]`
- **中途打断继续** — 每轮结束后暂停，用户可决定跳过或继续
- **报告对比视图** — TUI 中展示多轮报告的分歧对比
- **完成后目录重命名** — 最终轮生成 summary 后将 `<ts>/` 重命名为 `<ts>-<summary>/`，兼顾路径确定性与可浏览性
- **累计成本展示** — 完成横幅附带 N 轮累计 token/费用（usage 已逐轮累积，展示成本低）
- **排队而非阻塞** — 审查期间允许用户输入排队（需同时禁止 steer 注入审查 fork）
