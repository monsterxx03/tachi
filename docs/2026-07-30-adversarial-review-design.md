# 对抗式代码审查 (Adversarial Review) 设计文档

> 日期: 2026-07-30 | 状态: 设计定稿，可直接实现
>
> **2026-07-31 修订**：去掉 `adversarial.enabled` / `adversarial.rounds` 配置项。多轮对抗不再由配置开关触发——**无参 `/review` 恒为单轮**（普通审查），**`/review N`（N ≥ 2）才进入 N 轮对抗**。`adversarial.models` / `adversarial.judge_model` 保留（多轮模式下的逐轮模型分配）。老配置里的 `enabled:` / `rounds:` 键会被非严格 yaml 解析静默忽略，不影响加载。

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
4. **向后兼容** — 无参 `/review` 保持现有单轮行为，多轮需显式 `/review N` 才进入
5. **简单优先** — 首版不做 cycle-aware prompt 差异化，只告诉 LLM 当前轮次和角色

---

## 二、总体架构

```
用户输入 /review [N]
    │
    ├─ N ≥ 2：进入 N 轮对抗模式（无参或 N ≤ 1 → 单轮，走现有路径）
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
  thinking_level: high

  # thinking / thinking_level 默认"跟随当前会话"（nil/空 = 继承当前会话的
  # 思考开关与 effort，会话无覆盖时再回退到 provider/模型默认）。配置任一
  # 字段即钉住该维度：thinking 只影响开关，thinking_level 只影响 effort
  # （"none" 强制关开关，"default" 回到 provider 默认）。

  # --- 对抗式审查配置（可选；只控制多轮模式下的逐轮模型分配）---
  # 多轮模式由命令参数触发：/review N（N ≥ 2），无参 /review 恒为单轮。
  adversarial:
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
    Models     []string `yaml:"models"`      // 各轮 model 名（可选）
    JudgeModel string   `yaml:"judge_model"` // 最终轮固定 model 名（可选，空 = 取模）
}
```

> 轮次**不在配置里**——多轮只能通过 `/review N` 显式进入（见第四节）。config 层不再表达"默认轮数"或"开关"，少一个状态源，语义更直接。

嵌入到 `ReviewConfig`：

```go
type ReviewConfig struct {
    Provider      string                    `yaml:"provider"`
    MaxIterations int                       `yaml:"max_iterations" default:"200"`
    AllowedTools  []string                  `yaml:"allowed_tools"`
    Thinking      *bool                     `yaml:"thinking"`        // nil = 跟随当前会话
    ThinkingLevel string                    `yaml:"thinking_level"`  // "" = 跟随当前会话
    Adversarial   *AdversarialReviewConfig  `yaml:"adversarial"`     // 新增
}
```

> ⚠️ **creasty/defaults 行为（实现代码必须遵守）**：
>
> - `defaults.Set()` **不会**自动分配 nil 指针字段：`shouldInitializeField`（defaults.go）对 **nil 指针且无 default tag** 的字段返回 false，直接跳过。因此未配置 `adversarial:` 时 `ReviewConfig.Adversarial` **恒为 nil**——访问 `Models`/`JudgeModel` 前**必须判空**（`SetupAdversarialProviders`、`CheckAdversarialProviders` 都以 `adv != nil` 为门）。
> - 显式配置 `adversarial:` 时指针非 nil，但 **不再有任何 default tag**——内部字段全部走 zero value（空切片 / 空串），语义就是"未指定"。
> - 老配置若残留已删除的 `enabled:` / `rounds:` 键：`yaml.Unmarshal` 非严格，**静默忽略**，配置正常加载。

---

## 四、命令行参数

### `/review [rounds]`

轮次来源只有命令参数（config 不参与）：

1. `/review N`（N ≥ 2）→ N 轮（clamp 到 `[2, 10]`）
2. `/review 0`、`/review 1`、负数或非数字参数 → 单轮（拼写错误不升级为 N 倍成本）
3. `/review`（无参数）→ **单轮**（普通审查；多轮必须显式给轮数）

解析逻辑（放 **`agent/commands`** 共享包并**导出**，TUI 与 ACP 两端复用——`ResolveReviewRounds`/`ResolveRoundModels` 都是纯函数，**不能放 `tui/`**，否则 `agent/acp` 导入会构成循环依赖）：

```go
const maxReviewRounds = 10

// ResolveReviewRounds 决定本次 /review 的轮数。input 为完整输入（如 "/review 6"）。
func ResolveReviewRounds(input string) int {
    parts := strings.Fields(input)
    if len(parts) < 2 {
        return 1 // 无参数 → 单轮（普通审查）
    }
    n, err := strconv.Atoi(parts[1])
    if err != nil {
        return 1 // 非数字参数（如 "/review foo"）→ 单轮；拼写错误不升级为 N 倍成本
    }
    if n < 2 {
        return 1 // /review 0、/review 1、负数 → 单轮
    }
    return min(n, maxReviewRounds)
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
// 编排器（Next 写进 prompt、Complete 落盘校验）两端共用它，保证引用同一个文件。
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

## 六、编排逻辑（共享编排器）

> **2026-07-31 修订**：多轮编排状态从 TUI/ACP 两端收敛到共享的
> `cmds.ReviewOrchestrator`（`agent/commands/review.go`）。两端不再各自维护
> round/providers/reports 状态机，只做三件事：**构造编排器 → 驱动轮次 →
> 渲染结果**。channel 端接入时同理，零复制。

### `ReviewOrchestrator` 职责

编排器持有全部编排状态（轮次、provider 列表、报告目录、轮次计数、报告落盘
记录），并提供纯驱动接口——**单轮与多轮走同一条路径**，前端不分支：

| 方法 | 职责 |
|------|------|
| `NewReviewOrchestratorFromCommand(input, opts, resolve)` | 轮次解析（`ResolveReviewRounds`）+ provider 分配（前端注入的 `resolve` 闭包，多轮含 fail-fast）+ 报告目录创建（仅多轮）；失败在第 1 轮前中止 |
| `NewReviewOrchestrator(rounds, providers, reportDir, opts)` | 低层构造（测试/复用用），校验 `len(providers) == rounds` |
| `Next() (RoundSpec, bool)` | 推进到下一轮：单轮返回标准 review prompt（`Kind:"review"`）；多轮返回 role/prompt/确定性 outPath（`Kind:"review-round-N"`） |
| `Complete() (done bool, report RoundReport)` | 本轮落盘校验（`os.Stat`，编排器不信任 LLM 自觉）+ 记录 reports；最后一轮返回 `done=true` |
| `Run(runRound func(RoundSpec) error) error` | 同步驱动整个链（ACP/channel 用）；`runRound` 返回错误（含 `ErrStopReview` 哨兵）立即终止 |
| `IsMultiRound()` / `TotalRounds()` / `CurrentRound()` / `Options()` / `Reports()` | 前端渲染与 fork 参数查询 |

```go
type RoundSpec struct {
    Round    int
    Role     ReviewRole
    Provider llm.Provider
    OutPath  string // 多轮：编排器拥有的报告路径（写进 prompt）；单轮：空（LLM 自拟）
    Prompt   string
    Kind     string // OneOffMeta.Kind: "review" / "review-round-N"
}
```

### 前端契约

**事件驱动前端（TUI）**：`Next()` 启动一轮 → 轮次流结束（`TurnComplete`）时
`Complete()` → `done=false` 链式启动下一轮，`done=true` 走正常 one-off 收尾。

```go
func (m *Model) sendReviewCommand() tea.Cmd {
    // ... 显示 /review、savedHistory ...
    orch, err := cmds.NewReviewOrchestratorFromCommand(m.subcommandInput,
        cmds.ResolveReviewOptions(m.cfg), m.resolveReviewProviders) // 单轮 [p] / 多轮对抗分配
    if err != nil { /* 报错回 idle，不发第 1 轮 */ }
    m.reviewOrch = orch
    m.isReviewing = true
    return m.startReviewRound()
}

func (m *Model) startReviewRound() tea.Cmd {
    spec, _ := m.reviewOrch.Next()
    forked := m.agent.Fork(agent.ForkConfig{Provider: spec.Provider, ...})
    if m.reviewOrch.IsMultiRound() { /* Round N/M banner */ }
    m.eventCh = forked.Agent().RunOneOffStream(ctx, spec.Provider,
        m.systemPrompt, spec.Prompt, reviewOpts,
        agent.OneOffMeta{Kind: spec.Kind, ...})
    return tea.Batch(m.statusbar.Tick(), m.nextEvent())
}
```

`TurnComplete` 分支（最顶部，中间轮不碰 `m.history`）：

```go
if m.reviewOrch != nil {
    done, report := m.reviewOrch.Complete()
    if !done { // 中间轮：封口气泡、累积 usage、关 fork、链下一轮
        return m.startReviewRound()
    }
    // 最终轮：清 reviewOrch，fall through 走正常 one-off 收尾
    //（savedHistory 恢复、usage、FinishStreaming、fork Close 全在正常分支）
}
```

`AgentEventError` 分支同理在最顶部清理 `m.reviewOrch` / `isReviewing`（取消与
失败都产生 `AgentEventError` 而非 `TurnComplete`，两个分支缺一不可）。

**同步前端（ACP/channel）**：`orch.Run(...)` 一行驱动：

```go
orch, err := cmds.NewReviewOrchestratorFromCommand("/review "+args, ropts,
    func(rounds int) ([]llm.Provider, error) { /* 单轮 [reviewProvider] / 多轮对抗分配 */ })
if err != nil { sendTextUpdate(...); return EndTurn, err }

stopReason := acp.StopReasonEndTurn
err = orch.Run(func(spec cmds.RoundSpec) error {
    forked := aiAgent.Fork(agent.ForkConfig{Provider: spec.Provider, ...})
    defer forked.Close()
    stopReason, _, err = streamToACP(ctx, sess, conn,
        forked.Agent().RunOneOffStream(ctx, spec.Provider, systemPrompt,
            spec.Prompt, opts, agent.OneOffMeta{Kind: spec.Kind, ...}))
    if err != nil { return err }
    if stopReason != acp.StopReasonEndTurn { return cmds.ErrStopReview } // 客户端断开等
    return nil
})
sess.history = nil // one-off 不入会话缓存
if errors.Is(err, cmds.ErrStopReview) { return stopReason, nil }
if err != nil { return acp.StopReasonEndTurn, err }
return acp.StopReasonEndTurn, nil
```

> 💰 **成本提示**：N 轮 = N 次独立 fork + N 次完整 LLM 调用，每轮都要重新跑
> git diff，总成本约为单轮审查的 **N 倍**。`/review N` 上限 10；usage 已逐轮
> 累积进 `/usage`。首版不做跨轮 diff 缓存（每轮独立 fork、上下文隔离是设计约束）。

**贯穿全节的两条状态机纪律（前端实现必须遵守）：**

1. **`savedHistory` 是 one-off 的唯一标记**（TUI：`isOneOff := m.savedHistory != nil`）。
   多轮期间它必须**一直保持非 nil**，直到最终轮 `TurnComplete` 由正常分支恢复——
   中间任何提前恢复都会让后续事件的 `m.history = event.Messages` 用审查轮消息
   污染主历史。
2. **取消/失败产生的是 `AgentEventError` 而非 `TurnComplete`**（agent_loop.go 的
   ctx.Done 分支）。多轮调度必须同时挂在两个事件分支上，缺一不可。

**TUI 特有 UI 职责**（不属编排逻辑，留在前端）：
- 轮次 banner（`Round N/M — 角色 (model)`）与完成横幅、系统通知；
- 报告落盘提示（💾 已保存 / ⚠️ 未保存，后续轮跳过）——校验本身在 `Complete()`；
- 审查期间阻塞用户输入（`isReviewing`，在 `stateStreaming` 分支**之前**——流式期间
  的输入会进 `pendingQueue`，steer 检查时被注入运行中的审查 fork，破坏轮次隔离）；
- Ctrl+C：取消 + 排队读尾随的 `AgentEventError`，由 error 分支统一清理
  （不提前恢复 `savedHistory`）。

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

> 以下示例中轮数来自命令参数 `/review N`（N 即所示 rounds），配置只控制模型分配。

**例 1：单一模型，6 轮（`/review 6`）**

```yaml
review:
  provider: claude-sonnet
  # models 未设置
```

→ 所有 6 轮使用 `claude-sonnet`

**例 2：3 个模型，6 轮（`/review 6`）**

```yaml
review:
  adversarial:
    models: [claude-sonnet, gpt-4o, claude-opus]
```

→ R1=sonnet, R2=4o, R3=opus, R4=sonnet, R5=4o, **R6=opus**（最后一轮是 opus 作为最终 Judge）

**例 3：2 个模型，5 轮（`/review 5`）**

```yaml
review:
  adversarial:
    models: [claude-sonnet, gpt-4o]
```

→ R1=sonnet, R2=4o, R3=sonnet, R4=4o, **R5=sonnet**（最后一轮取模为 sonnet）

**例 4：judge_model 固定最终轮（`/review 5`）**

```yaml
review:
  adversarial:
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
| `config/config.go` | ~15 行 | `ReviewConfig` 新增 `Adversarial *AdversarialReviewConfig`（仅 `Models`/`JudgeModel`） |
| `agent/commands/commands.go` | ~10 行 | `/review` Def 描述补充 `[rounds]` 参数提示 |
| `agent/commands/review.go` | ~230 行 | 共享编排层：`ReviewOrchestrator`（构造/Next/Complete/Run）+ `ResolveReviewRounds()` + `ResolveRoundModels()` + `CheckAdversarialProviders()` + `ReviewOptions` + `RoundSpec`/`ErrStopReview`（**不能放 `tui/`**，见第四节） |
| `agent/commands/prompts.go` | ~150 行 | 新增 `BuildReviewPrompt()`、`ReviewRole`/`roundReport` 类型、`resolveRole()`、`roleFileSuffix()`、`ReportPathFor()`、`sanitizeFileName()` |
| `agent/agent_provider.go` | ~40 行 | `adversarial.models`/`judge_model` 的名字→provider 解析（Configure 阶段，配合 fail fast） |
| `tui/commands.go` | ~60 行 | `sendReviewCommand()` 改为构造编排器 + `startReviewRound()` 从 `Next()` 取 spec（无轮次分支） |
| `tui/model_events.go` | ~45 行 | `TurnComplete` 顶部 `Complete()` 调度分支 + `AgentEventError` 的 reviewOrch 清理 |
| `tui/model.go` | ~15 行 | `reviewOrch *cmds.ReviewOrchestrator` + `isReviewing`（删除 `reviewState`）+ 输入阻塞 + `handleCtrlC` 分支 |
| `agent/acp/commands.go` | ~60 行 | `handleACPReview` 构造编排器 + `orch.Run()` 同步驱动（含 `ErrStopReview` 终止语义）；删除 `runAdversarialReviewRounds` |

**测试计划：**

| 文件 | 说明 |
|------|------|
| `agent/commands/review_test.go` | 编排器单测（单轮 spec/多轮角色周期/FromCommand 解析/fail-fast 传播/Run 终止契约/构造校验）+ 纯函数单测（`ResolveReviewRounds`、`ResolveRoundModels`、`resolveRole`） |
| `tui/review_test.go` | 多轮链式调度（事件注入）：中间轮不恢复 history、usage 逐轮累积、报告缺失标记、Ctrl+C 后由 error 分支完成清理、API 失败不残留 `isReviewing` |
| `agent/acp/commands_test.go` | `handleACPReview` 多轮：轮次链完整、任一轮非自然完成即终止（`ErrStopReview` → 原 stop reason）、`sess.history` 清空、fork 每轮 Close、报告缺失时后续 prompt 携带标记 |

**不需要改动：**
- `agent/agent_fork.go` — 完全复用
- `agent/agent_loop.go` — 完全复用
- `agent/oneoff_recorder.go` — 完全复用

---

## 十、与现有行为的关系

| 场景 | 行为 |
|------|------|
| 无参 `/review` | **单轮**，现有行为完全不变（多轮只由显式参数触发；`Adversarial` 指针未配置时恒为 nil，访问 `Models`/`JudgeModel` 前必须判空——见第三节） |
| `/review N`（N ≥ 2） | **N 轮**对抗审查 |
| `/review 0` / `/review 1` / 负数 / 非数字参数 | 单轮 |
| `/review N` 且 N > 10 | clamp 到 10 |
| 配置残留旧 `enabled:` / `rounds:` 键 | 非严格 yaml 静默忽略，加载正常 |
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
