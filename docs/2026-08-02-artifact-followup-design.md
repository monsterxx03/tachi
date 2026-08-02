# 隔离工作流产物回主会话 —— 通用追问支持机制

日期: 2026-08-02
状态: 已实现（含对抗审查 J1–J14 修复，2026-08-02 晚）
相关: `docs/2026-07-05-deep-research-design.md`、`docs/2026-07-30-adversarial-review-design.md`

> 本文档描述的是一个**通用机制**：任何与主会话隔离运行的工作流（深度研究、多轮代码审查，以及未来新增的其他工作流），其产物都通过同一机制回到主会话，使用户能够继续追问。当前落地的两个工作流是 `/research` 与多轮 `/review`。
>
> 实现经三轮对抗式代码审查（20260802-210636），阻断性缺陷（J1：产物提醒无法进入 LLM 上下文）与数据安全缺陷（J2：伪造 marker 行丢数据）均已修复，见"实现要点与审查修复"章节。

## 背景与问题

`/research` 与多轮 `/review` 的产出（报告、结论）目前与主会话**完全隔离**：

- research 通过引擎直调 LLM + 独立 sub-agent fork 运行，报告落盘到 `~/.tachi/research/`，主会话 `messages.jsonl` 不记录任何研究内容。
- review 通过独立 fork 逐轮运行，报告写入 `<workDir>/.tachi/reviews/<ts>/`，同样不进主会话历史。
- 命令的最终回复（research 摘要、review 结论文本）是**一次性回复消息**，只存在于 Discord 的 ephemeral followup / TUI 渲染层，不是 assistant 历史消息。

结果：用户无法就研究/审查结论**继续追问**——下一个 turn 主 agent 加载会话历史时，上下文里没有刚才的研究内容，追问"刚才报告里 X 展开讲讲"时 LLM 一无所知。

## 目标

让用户能对 research / review 的产出继续追问，同时：

- 不把完整报告（10KB+ HTML / 多轮 md）灌进对话上下文（token 可控）
- 不污染普通对话的静态结构，无每轮重复开销
- 生命周期自然跟随会话（`/new` 即隔离，compact 后随旧历史消失）

## 方案对比（讨论记录）

### 方案 A：研究结果直接注入主会话历史

research 完成后把「摘要 + 发现要点（截断）+ 路径」作为一条 assistant 消息 append 进 session。

- 优点: 实现简单（append 一条消息）、体验自然
- 缺点: 发现要点仍需截断策略；注入量偏大（数十条 learnings）；本质是"把内容搬进历史"，而非"按需取用"

### 方案 B：结果存 sidecar，追问时按需注入

结果存 session 目录旁路文件，系统记住"最近一次研究"，检测到追问意图（或 `/research followup`）时注入。

- 优点: 上下文零污染
- 缺点: 需要额外状态管理 + 意图检测；体验生硬

### 方案 C：research 变成主 agent 的 tool

主 agent 对话中自行调用，结果以 tool_result 进上下文。

- 优点: 最"agentic"
- 缺点: 改动最大；研究耗时数分钟会阻塞主对话；无"研究一次、多轮讨论"的缓存语义

### 方案 D（选定）：完成时向会话历史注入产物提醒

隔离工作流**成功完成时**，直接向当前 session 的 `messages.jsonl` 写入产物提醒（一条 `MessageTypeReminder` 消息），内容为「产物类型 + 主题 + 路径 + 按需读取提示」。LLM 后续所有轮次从历史中自然读到这条提醒，追问时按需 `ReadFile` 产物文件。

- 优点: 只写一次、零每轮开销；内容极小（一两行）；完整产物按需读取（token 可控）；生命周期天然随 session；三端（TUI/Channel/ACP）统一；**机制通用**（任何隔离工作流完成时调用同一 API）
- 缺点: 无（相对其他方案）

## 关键机制（代码已验证）

### 1. reminder 消息会进入会话历史

`agent/agent_loop.go:444` `recordUserTurn`：

```go
func (a *AIAgent) recordUserTurn(rs *RunState, userMessage, reminderBlock string) {
	if reminderBlock != "" {
		a.recordSession(rs, &session.Message{
			Type:    session.MessageTypeReminder,
			Content: reminderBlock,
		})
	}
	...
}
```

每轮注入的 reminder 块以独立 `MessageTypeReminder` 消息写入历史。

### 2. 历史加载时 reminder 还原为前缀

`agent/session_convert.go` `ConvertSessionToLLMMessages`：每条 `MessageTypeReminder` 被 prepend 到其**后续的第一条 user 消息**上，重新变回 `<system-reminder>` 前缀。

**注意（J1 修复后）**：末尾**悬垂**的 reminder（其后没有 user 消息——如刚 append 的产物提醒）在转换收尾时被显式追加为最后一条 user 消息，不再丢失。这是磁盘重载路径（agent 被 evict / bot 重启后）产物提醒能到达 LLM 的关键。

### 3. 先例：project context 只在首轮注入

`agent/systemreminder/project_reminder.go` 用 `rctx.IsFirstMessage` 只在首轮注入 `.tachi.md`，内容进历史后，后续轮次 LLM 从历史中自然读到。`historyHasReminder()`（agent_loop.go）防止 reload 后首轮类 reminder 重复注入。**本方案沿用同一模式**：注入一次，历史承载，无需每轮 `Generate`。

### 4. 直接 append 的 API 已存在

`session.Manager.AppendMessage(msg)`（`session/manager.go:178`）可直接向当前 session 追加消息，无需经过 `recordSession` 的 `SkipSessionWrites` 旁路（one-off 运行会跳过主历史，注入必须绕过它直用 `AppendMessage`）。

## 通用设计：ArtifactRef + 统一注入 API

机制的核心是一个**产物引用模型**和**一个统一入口**，任何隔离工作流完成时只需一行调用。

### 1. 产物模型 `ArtifactRef`（放 `session` 包，无依赖问题）

```go
// ArtifactRef 描述一个隔离工作流产出的可追问结果。
// Kind 使用注册表常量（见下），Title 是一句话主题，Path 是产物文件或目录。
type ArtifactRef struct {
	Kind  string // "research" | "review" | 未来扩展
	Title string
	Path  string
}
```

三个字段目前够用（现有工作流都是"文件 + 一句话主题"）。未来若出现需要特殊读取提示的产物（如"用户要求应用时才读取"的补丁），加可选字段（如 `Hint string`），向后兼容。

### 2. Kind 注册表 + 统一渲染

```go
const (
	ArtifactKindResearch = "research"
	ArtifactKindReview   = "review"
)
```

渲染函数（格式只此一处，全工作流统一）：

```go
func formatArtifactReminder(refs []ArtifactRef) string {
	// <system-reminder>
	// 近期产物（仅当用户主动就该产物追问时，才读取对应文件）：
	// - [研究] 主题：xxx · 产物：/path/a.html
	// - [审查] 主题：xxx · 产物：/path/round-3-judge.md
	// 若用户问题与上述产物无关，忽略本条。
	// </system-reminder>
}
```

### 3. 统一注入 API（`session.Manager.AppendArtifact`）

```go
// AppendArtifact 向当前 session 追加（或合并）一条产物提醒。
// 若历史最后一条消息已是 MessageTypeReminder，则把新产物并入该块，
// 否则新增一条 —— 避免连续产物互相覆盖（见"合并语义"）。
func (m *Manager) AppendArtifact(ref ArtifactRef) error
```

**合并语义（必须）**：`session_convert.go` 的 `pendingReminder` 是单值缓冲，连续两条 reminder 之间若没有 user 消息，前一条会被后一条覆盖。因此注入必须**检查历史最后一条消息**：

- 最后一条是 `MessageTypeReminder` → 解析出已有 refs，append 新 ref，重写该消息内容（保留原 reminder 前缀）
- 否则 → 新增一条 `MessageTypeReminder`

这也顺便提供了**累积上限**的控制点（如最多保留 5 个产物，超出丢弃最旧——待定，先全部保留观察）。

### 4. 工作流接入约定

新隔离工作流完成时，成功路径加一行：

```go
// 伪代码 — 工作流完成处
if err := sm.AppendArtifact(session.ArtifactRef{
	Kind:  session.ArtifactKindReview,
	Title: topic,
	Path:  lastRoundReportPath,
}); err != nil {
	logger.Error(ctx, "append artifact reminder", err)
}
```

接入成本一行，不再需要碰系统提示、历史格式、生命周期逻辑。

## 注入内容设计（渲染示例）

一条 reminder 消息，格式（Discord Markdown 与纯文本通用）：

```
<system-reminder>
近期产物（仅当用户主动就该产物追问时，才读取对应文件）：
- [研究] 主题：AI Agent 产品对比 · 产物：/home/will/.tachi/research/2026-08-02_2016-xxx.html
- [审查] 主题：通道 /commit 支持 · 产物：/path/.tachi/reviews/20260802-195151/round-3-judge-xxx.md
若用户问题与上述产物无关，忽略本条。
</system-reminder>
```

要点：

- **一个 session 的产物提醒合并为一条**（同一块内一行一个产物），天然避免覆盖问题
- **review 只指向最后一轮的报告**（round-N-<role>.md），追问时 agent 读它；更早轮次在同目录可按需读
- **不加生成时间字段**——文件名自带时间戳（`2026-08-02_2016` / `20260802-195151`）
- 措辞约束「仅当用户主动就该产物追问时读取」，避免无关问题触发无谓 ReadFile

## 实现位置

| 位置 | 注入点 | 说明 |
| ---- | ------ | ---- |
| `agent/research.go` `RunDeepResearch` 成功路径 | research 完成（writeReport 成功后） | Channel/ACP 共享入口 |
| `tui/commands_research.go` | TUI research 完成 | 与 Channel 共用同一注入函数 |
| `channel/manager/commands_commit_review.go` `handleReviewCommand` | review 完成后 | 取 `orch.ReportDir()` + 最后一轮 `OutPath` |
| `tui/commands_agent.go` | TUI review 完成 | 同上 |

注意：注入必须**绕过** `recordSession` 的 `SkipSessionWrites`（one-off 运行不会写主历史），直接调用 `SessionManager.AppendArtifact`。

## 生命周期与边界情况

| 场景 | 行为 |
| ---- | ---- |
| `/new` 新会话 | 新 session，天然没有旧产物提醒 |
| `/compact` | 新 session，提醒随旧历史总结消失（可接受；结论已在摘要中） |
| 多条产物 | 合并为同一条 reminder（块内一行一个），互不覆盖；上限 5 条，超出丢最旧 |
| 产物文件被删 | agent ReadFile 失败，应如实说明"文件不存在"；注册时 os.Stat 校验，文件缺失不注册（J7） |
| 用户问无关问题 | 措辞约束「无关则忽略」；ReadFile 是显式工具调用，误读风险低 |
| bot 重启 / agent 被 evict | 磁盘重载路径携带悬垂 reminder（J1 修复），重启后仍可追问 |
| 线程内存缓存存活（channel） | 产物提醒拼接进 `ca.history`（J1 修复），下轮追问立即可见 |
| TUI 当前会话 | 产物提醒拼接进 `m.history`（J1 修复），下轮追问立即可见 |

## 已知决策与限制（审查裁决后）

- **单轮 review 也注册产物**：自 2026-08-02 晚起，单轮 review 同样由 orchestrator 分配报告目录与精确输出路径（prompt 指示 LLM 写入该路径，不再自命名文件），完成后注册产物。多轮 review 只注册**最后一轮**（judge）报告。
- **review 失败路径不注册（J9，待产品裁决）**：当前仅在成功后注册；失败时已完成轮次的报告存在于 `ReportDir()`，但不会被登记。裁决前保持现状。
- **`ReplaceLastMessage` 原子写（J5）**：已改用 `fileutil.AtomicWriteFilePrivate`；多 Manager 实例并发写属潜伏风险（当前调度语义下无法实际触发），接口 doc 已注明调用方必须保证无并发写。
- **合并块上限 5（J12）**：同一提醒块最多保留最近 5 个产物，超出丢最旧（每个 ref 带日期化路径，丢最旧损失最小）。

## 实现要点（三端拼接路径）

产物提醒需要到达 LLM 的**实际输入**，而不只是磁盘。三端各自的拼接路径：

| 前端 | 内存历史 | 拼接方式 |
| ---- | -------- | -------- |
| Channel | `ca.history`（cachedAgent） | `spliceArtifactIntoCache`：research/review 完成后 append 一条 user 消息（内容为 reminder 块），持 `ca.mu` 期间执行 |
| TUI | `m.history` | research 经 `researchDoneMsg` 主循环拼接；review 在 final round 的 `appendReviewArtifact` 拼接 |
| ACP | `sess.history`（每轮重载磁盘） | 不拼内存；依赖磁盘重载路径（悬垂携带） |

`RunDeepResearch` 返回 `(report, artifactRef, err)`——channel 拿 ref 构造 reminder 拼 `ca.history`；ACP 忽略 ref（磁盘路径覆盖）。

## 明确不做的事

- 不新增 `systemreminder.Reminder` 实现（无需每轮 Generate，避免重复注入与历史累积）
- 不把完整产物灌进对话上下文（保持"提示常驻、读取按需"）
- 不给 `Session` struct 加 meta 字段（reminder 消息即承载，无需额外状态）
- 不做追问意图检测（LLM 依据提示自行判断）
- 不做重型插件/注册中心/事件总线（Go 常量 + 一个渲染函数足够，新工作流接入成本一行）
- 不引入产物内容索引/全文检索（产物按路径落盘，追问时按需读取）

## 扩展性说明（未来工作流如何接入）

| 新工作流 | 接入步骤 |
| -------- | -------- |
| 任意隔离运行、产出文件/目录的工作流 | ① 若 Kind 是新的，在 `session` 包加一个常量 ② 完成处一行 `AppendArtifact` |
| 产物需要特殊读取提示（如"用户要求应用时才读"） | `ArtifactRef` 加可选 `Hint` 字段，渲染函数按 Kind/Hint 输出对应提示 |
| 产物不是单一文件（如目录、多文件） | `Path` 指向目录即可，提示文案说明"目录内按需读取" |

## 验证方式

1. `/research <topic>` 完成后，检查 session `messages.jsonl` 出现一条 `MessageTypeReminder`
2. 下一轮追问「刚才研究里 X 展开讲讲」，确认 agent 调用了 ReadFile 读取报告
3. 追问无关问题，确认不读文件
4. 重启 bot 后继续追问，确认仍有效
5. `/review 3` 完成后同样验证（报告指向最后一轮）
