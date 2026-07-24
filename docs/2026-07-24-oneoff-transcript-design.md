# One-off Transcript — 旁路执行记录设计文档

> 目标：让 /commit、/review、ambient、dream 等旁路 LLM 执行「隔离且留痕」——
> 不污染主会话上下文（现状），但完整落盘可排查（新增）。
> 机制：泛化 subagent recorder 的先例，把 `skipSessionWrites` 从「丢弃」降级为「改道到 sidecar 文件」。

---

## 目录

1. [背景与问题](#一背景与问题)
2. [设计目标 / 非目标](#二设计目标--非目标)
3. [现状盘点](#三现状盘点)
4. [总体设计](#四总体设计)
5. [文件格式与落盘布局](#五文件格式与落盘布局)
6. [配置](#六配置)
7. [Retention](#七retention)
8. [与既有系统的不变量](#八与既有系统的不变量)
9. [可见性](#九可见性)
10. [API 变更与调用点改造清单](#十api-变更与调用点改造清单)
11. [测试计划](#十一测试计划)
12. [实施分期](#十二实施分期)
13. [开放问题](#十三开放问题)

---

## 一、背景与问题

为避免污染主会话上下文，多个旁路执行路径**完全不留记录**：

- `RunOneOffStream` 设置 `skipSessionWrites = true`，`recordSession` 全部 no-op；
- channel ambient 的 fork agent 根本没挂 SessionManager，`recordSession` 因 nil check 短路。

后果：/review 结论奇怪、ambient 在群里突然插话、/commit 生成离谱 message、
dream 写出了错误的 topic 内容——这些「为什么」全部无从排查，只能靠 debug.log
里的碎片日志推测，而 debug.log 不含完整 prompt 与 LLM 响应。

明确不要的方案：把大 payload 打进 debug.log。日志会被人间不可读的巨型
JSON 冲垮，且丢失 tool_call/tool_result 配对、usage 等结构化信息。

### 已有的先例

SubAgent 早已解决同一问题：`agent/subagent/recorder.go` 把每次子代理执行写到
`session/<id>/subagent/<shortID>.jsonl`——独立文件、不碰 messages.jsonl、
结构化 session.Message 行。本设计就是把这一思路泛化到所有旁路执行。

### 与 2026-07-20-event-stream-design 的关系

eventlog 设计（**尚未实现**，无 `pkg/eventlog` 代码）已把「会话外执行」识别为
空白。两者分工：

| | eventlog（未落地） | oneoff transcript（本设计） |
|---|---|---|
| 回答的问题 | 聚合统计：慢 / 贵 / 失败率 | 个案排查：这次旁路执行到底发生了什么 |
| 内容 | 结构化小行事件 | 完整 prompt、消息流、tool 结果 |
| 查询方式 | jq / stats | grep / transcript 渲染 / 人读 |

两者互补、可独立落地。本设计更简单，可先行。

---

## 二、设计目标 / 非目标

### 目标

| 目标 | 说明 |
|------|------|
| **隔离语义不变** | 一字不改：不写 messages.jsonl、不进 memory/dream、不影响 token 计量 |
| **全量记录** | system prompt + 完整消息流（user/reminder/thinking/assistant/tool_call/tool_result），tool result 不截断 |
| **埋点零风险** | 记录失败只告警，绝不影响旁路执行本身 |
| **文件优先** | 纯 JSONL，grep/jq 友好，与 subagent recorder 同构 |
| **可发现** | debug.log 一行索引（kind + 路径 + trace_id）；用户主动触发的命令在 TUI 提示路径 |

### 非目标

- 不改 subagent recorder 的现有行为（未来可清理共用底层实现，见开放问题）
- 不做实时聚合 / 统计 / dashboard（那是 eventlog 的职责）
- 不把 sidecar 内容纳入 memory、dream、compact 等任何记忆系统
- 不做 transcript TUI/CLI 的深度集成（仅保留为二期可选项）

---

## 三、现状盘点

### RunOneOffStream 调用点（9 个）

| # | 调用点 | 用途 | 会话上下文 | 建议 kind |
|---|--------|------|-----------|-----------|
| 1 | `main.go:437` | `tachi -c` commit | 无 SessionManager | `commit` |
| 2 | `tui/commands.go:1082` | TUI `/commit` | 主 agent 有 Current session | `commit` |
| 3 | `tui/commands.go:1131` | TUI `/review`（fork agent） | fork 无 SM，TUI 持有 session id | `review` |
| 4 | `agent/acp/commands.go:268` | ACP `/commit` | 有 session | `commit` |
| 5 | `agent/acp/commands.go:329` | ACP `/review`（fork） | fork 无 SM，ACP session 持有 id | `review` |
| 6 | `dream/runner.go:91` | dream 管道 | 无（沙箱子代理） | `dream` |
| 7 | `channel/github/discussion.go:67` | GH discussion bot | 无 | `github-discussion` |
| 8 | `channel/github/pr_agent.go:192` | GH PR 实现 agent | 无 | `github-pr` |
| 9 | `agent/agent_subagent.go:88` | SubAgent | **不动**（已有独立 recorder） | — |

另：/compact 摘要在初稿中曾列为 kind `compact`——实现时核实它实际走
`RunConversationStream`（摘要需进入新 compact 会话的历史），不是旁路执行，
已从范围中移除。

### RunConversationStream 旁路（1 个）

| # | 调用点 | 用途 | 会话上下文 | 建议 kind |
|---|--------|------|-----------|-----------|
| 10 | `channel/manager/ambient.go:319` | ambient 群聊旁听（fork） | fork 无 SM；**thread 关联 session**（`acquireAgent(threadID)` 的 cached agent 上有 `SessionManager.Current().ID`） | `ambient` |

channel cron（`manager/cron.go`）走的是 thread 的 cached agent（有 SessionManager），
本就会记入 thread 会话，不在本设计范围。

### 关键事实（已核实）

1. `recordSession` 的埋点在主循环里已覆盖 thinking / assistant / tool_call /
   tool_result（`agent_loop.go`、`tool_executor.go`），one-off 路径全部经过，
   只是被 `skipSessionWrites` 吞掉——**无需新埋点，只需改道**。
2. `RunOneOffStream` 目前**不记录 user message 与 reminder**（相关
   `recordSession` 调用只存在于 `RunConversationStream`），需补两个调用。
   由于 `skipSessionWrites` 在 one-off 内恒为 true，补调用对现有行为零影响。
3. dream 通过 `loadMessages(id)` 按会话 ID 读 `messages.jsonl`，**天然看不到
   `oneoff/` 子目录**；sidecar 不经过 SessionManager，不 bump `UpdatedAt`，
   dream 的 `ActiveSessionsSince` 门控不受影响。
4. `FileStore.DeleteSession` 是 `os.RemoveAll(整个会话目录)`——per-session 的
   `oneoff/` 随会话淘汰（max 100）自动清理。
5. ambient fork 创建时已携带 `SessionID: "ambient-" + threadID`（用于 LLM
   请求头），但那不是真实 session id；真实 id 需在 fork 前从 cached agent
   的 `SessionManager.Current().ID` 取出。

---

## 四、总体设计

### 核心改动：recordSession 改道

```go
func (a *AIAgent) recordSession(msg *session.Message) {
    if a.sessionManager == nil || a.skipSessionWrites {
        // 旁路执行：有 sidecar 则留痕，否则维持现状（丢弃）
        if a.oneoffRec != nil {
            a.oneoffRec.record(msg) // 失败仅 Warn，不影响主流程
        }
        return
    }
    if err := a.sessionManager.AppendMessage(msg); err != nil { ... }
}
```

一个分支覆盖两类旁路：

- `RunOneOffStream`（skipSessionWrites=true，SM 或有或无）→ sidecar；
- ambient fork（SM=nil，skipSessionWrites=false）→ sidecar。

正常会话 `oneoffRec == nil`，走原路径，零行为变化。

### Recorder 生命周期

新增 `agent/oneoff_recorder.go`（与 subagent recorder 同构，独立文件）：

- `RunOneOffStream` 新增参数接收记录元信息；`Kind == ""` 时不创建 recorder
  （subagent 调用点传空，行为不变）。
- recorder 在 goroutine 内创建（沿用现有 skipSessionWrites 的 set/restore
  模式），`defer close()`。
- 创建时写 meta 头行；随后 `recordSession` 改道写入消息行。
- ambient 不走 RunOneOffStream，提供显式挂载 API：
  `forkAgent.AttachOneOffRecorder(meta OneOffMeta) error`，
  ambient.go 在 fork 后、Run 前调用，`defer` 关闭。

### RunOneOffStream 补记 user/reminder

```go
// 现有：构建 wrappedUser 之后、runAgentLoop 之前，补：
if reminderBlock != "" {
    a.recordSession(&session.Message{Type: session.MessageTypeReminder, Content: reminderBlock})
}
a.recordSession(&session.Message{Type: session.MessageTypeUser, Content: userMessage})
```

对现有行为零影响（one-off 内这两个调用原本就是 no-op），sidecar 挂载后自动生效。

### 错误处理哲学

recorder 创建 / 写入 / 清理任何失败：`logger.Warn` 后继续。旁路记录是排障
辅助，永远不得让 /commit 或 ambient 因为写不了记录文件而失败。

---

## 五、文件格式与落盘布局

### 布局（两级）

有会话上下文（TUI/ACP 的 commit、review、compact、ambient）：

```
<sessionDir>/<sessionID>/oneoff/<kind>-<YYYYMMDD-HHMMSS>-<rand4>.jsonl
```

无会话上下文（tachi -c、github bot、dream）：

```
<home>/oneoff/<kind>/<YYYYMMDD-HHMMSS>-<rand4>.jsonl
```

- 一 run 一文件：文件边界 = 一次完整执行的故事边界，可直接转发、渲染。
- rand4 后缀防同秒冲突（ambient 突发、多 thread 并发）。
- per-session 与 `subagent/` 平级，跟随会话淘汰自动清理。

### 文件内容

首行 meta（新增 `"type":"meta"`，渲染端对未知 type 有默认分支，jq 友好）：

```json
{"type":"meta","kind":"ambient","session_id":"2026-07-22-101530-a1b2","cwd":"/repo","provider":"deepseek-v4","started_at":"2026-07-24T11:19:44+08:00","system_prompt":"...","extra":{"domain":"project:/repo"}}
```

随后是标准 `session.Message` JSON 行（与 messages.jsonl、subagent jsonl
同构）：`user` → `reminder` → (`thinking` / `assistant` / `tool_call` /
`tool_result`)\*。tool_result **全量不截断**——截断是查看端（render/grep）
的事，落盘丢信息不可逆。

`system_prompt` 记在 meta 行而非新 MessageType：它是排障 ambient 插话、
whisper 沉默、/review 漏报时第一要看的东西，且不属于消息流。

### trace_id 关联

turn 级 trace_id 在 `runAgentLoop` 顶部生成，晚于 recorder 创建，时序上
无法放进 meta 头行。关联策略：recorder 关闭时在 debug.log 写一行 Info
（kind + 路径 + 当前 turnTraceID + 耗时 + 文件大小），作为日志到文件的
跳转锚点。

---

## 六、配置

`config.yaml` 新增 section：

```yaml
oneoff:
  enabled: true        # 默认 true；false 时完全不创建 recorder（恢复现状）
  retention_days: 30   # 默认 30；仅作用于全局 <home>/oneoff/ 目录
```

`config.OneoffConfig`，带 default tag，与现有 `ToolResultConfig` 等同风格。

---

## 七、Retention

两条规则，语义明确：

| 位置 | 策略 |
|------|------|
| `session/<id>/oneoff/` | **跟随会话生命周期**：会话被淘汰（max 100）时 `DeleteSession` 的 RemoveAll 一并清掉，无需额外机制 |
| `<home>/oneoff/<kind>/` | **按龄清理**：保留最近 `retention_days`（默认 30）天；惰性 sweep——每次创建全局 recorder 时 readdir kind 目录、删除 modtime 超龄文件。无后台定时器，开销 O(文件数) |

选择惰性 sweep 而非启动时全量扫：channel 模式下 github bot / dream 触发时
自然会扫到对应 kind 目录；长期不跑的 kind 目录里旧文件留着也无害。

---

## 八、与既有系统的不变量

本设计依赖并必须维持以下不变量（review 检查清单）：

1. **messages.jsonl 零写入**：sidecar 只写独立文件，`AppendMessage` 路径不触及。
2. **session meta 零变更**：不 bump `UpdatedAt`、不改 title——dream 门控、
   会话排序、resume 全部不受影响。
3. **dream 不可见**：dream 经 `loadMessages(id)` 只读 messages.jsonl；
   oneoff 文件不是记忆素材（用户已确认：跳过）。
4. **memory 不可见**：RunOneOffStream 已有 `SkipWrites=true`；ambient fork
   无 memory backend；sidecar 文件不进任何记忆后端。
5. **subagent 不动**：`agent_subagent.go` 调用点传空 Kind，recorder 维持现状。
6. **token 计量不受影响**：RunOneOffStream 现有的 lastInputTokens 保存/恢复
   逻辑不变。
7. **transcript list 不受影响**：会话浏览器只读 meta.json。
8. **失败隔离**：recorder 任何错误不传播到旁路执行本身。

---

## 九、可见性

### debug.log 索引（自动触发的旁路）

recorder 创建与关闭各一行 Info：

```
INFO oneoff transcript opened  kind=ambient path=~/.tachi/session/.../oneoff/ambient-20260724-111944-x3f9.jsonl
INFO oneoff transcript written kind=ambient path=... trace_id=... duration=12.3s size=84.2KB
```

不进大 payload，但从日志可一跳定位完整记录。

### TUI 提示（用户主动触发的命令）

/commit、/review 完成后，在 chatview 追加一条系统样式消息：

```
📄 旁路记录: ~/.tachi/session/<id>/oneoff/review-20260724-112015-a7b2.jsonl
```

规则：**用户显式发起的命令提示，后台自动执行（ambient、dream、github bot）
保持沉默**。ACP 模式下由 SessionUpdate 流自然透出（可选，二期）。

---

## 十、API 变更与调用点改造清单

### 新增类型

```go
// agent/oneoff_recorder.go
type OneOffMeta struct {
    Kind      string            // "commit" | "review" | "ambient" | "dream" | "compact" | "github-discussion" | "github-pr"；空 = 不记录
    SessionID string            // 显式指定；空 → 尝试 sessionManager.Current().ID，仍无 → 全局目录
    Extra     map[string]string // 如 dream 的 domain、github 的 repo
}

func (a *AIAgent) AttachOneOffRecorder(meta OneOffMeta) error // ambient 等 RunConversationStream 旁路用
```

### 签名变更

```go
RunOneOffStream(ctx, provider, systemPrompt, userMessage string, opts llm.ChatOptions, meta OneOffMeta) <-chan AgentEvent
```

9 个调用点全部更新（subagent 传 `OneOffMeta{}`）。显式参数优于
`SetOneOffKind()` 式状态 API：调用点即文档，grep 可得全部记录方。

### 改造清单

| 文件 | 改动 |
|------|------|
| `agent/oneoff_recorder.go` | 新增：recorder + meta 行 + 全局 sweep |
| `agent/agent.go` | 新增 `oneoffRec` 字段；`recordSession` 改道分支 |
| `agent/agent_loop.go` | RunOneOffStream 签名 + recorder 生命周期 + 补记 user/reminder |
| `config/config.go` | `OneoffConfig` |
| `tui/commands.go` | /commit、/review 传 Kind；完成后提示路径 |
| `agent/acp/commands.go` | /commit、/review 传 Kind（review 显式传 SessionID） |
| `main.go` | `tachi -c` 传 Kind |
| `dream/runner.go` | 传 Kind + Extra.domain |
| `channel/github/{discussion,pr_agent}.go` | 传 Kind + Extra.repo |
| `channel/manager/ambient.go` | fork 前取 thread session id → `AttachOneOffRecorder` |

---

## 十一、测试计划

1. **recorder 单测**（镜像 `subagent/recorder_test.go`）：建文件、写各类
   message、meta 行字段完整性、无 SessionID 时落全局目录。
2. **recordSession 改道**：skipSessionWrites=true + oneoffRec → 写 sidecar
   不写 session；SM=nil + oneoffRec → 写 sidecar；oneoffRec=nil → 原行为。
3. **RunOneOffStream 端到端**（stub provider）：断言文件含 meta/user/
   tool_call/tool_result/assistant 全链路，且 messages.jsonl 为空。
4. **retention sweep**：构造超龄文件，创建 recorder 后断言删除。
5. **失败隔离**：recorder 目录不可写时，one-off 执行正常完成。
6. **ambient**：AttachOneOffRecorder 后跑 RunConversationStream，断言
   per-session oneoff 文件生成。

---

## 十二、实施分期

**一期（本设计的完整范围）**：recorder + recordSession 改道 + 全部调用点
接入 + 配置 + retention + debug.log 索引 + TUI 提示。查看靠 grep/less/jq。

**二期（可选，独立设计）**：

- `tachi transcript show --file <path>`：复用 `render/html.go`（已消费
  session.Message）渲染任意 oneoff JSONL 为 HTML 报告；
- `tachi transcript list` 对含 oneoff 的会话加标记；
- eventlog 落地后，oneoff meta 行可作为其 `execution_started` 事件的数据源。

---

## 十三、开放问题

1. **subagent recorder 统一**：两套 recorder 写入逻辑高度同构。一期不动
   subagent；稳定后可把底层 JSONL 写入抽成共用实现（subagent 的路径规则、
   事件翻译保持不变）。收益小、风险低，但不紧急。
2. **tool_result 体积**：全量记录下，dream / deepresearch 类长任务单文件
   可能达 MB 级。可接受（session messages.jsonl 本就如此，scanner buffer
   已有 10MB 先例）；若成为问题，在查看端做截断渲染，不改落盘策略。
3. **channel cron**：当前记入 thread 会话主历史。若日后希望 cron 也隔离，
   可直接复用本机制加 kind `cron`，无需新设计。
