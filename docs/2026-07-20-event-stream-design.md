# Tachi 会话级事件流设计文档

> 目标：构建独立于 messages.jsonl 的结构化执行事件流，回答"为什么慢 / 为什么贵 / 哪里失败"三类运维问题。
> 新包 `pkg/eventlog/`，与 `pkg/logger/` 互补：logger 记录离散诊断文本，eventlog 记录结构化执行事件。

---

## 目录

1. [背景与问题](#一背景与问题)
2. [设计目标 / 非目标](#二设计目标--非目标)
3. [总体设计](#三总体设计)
4. [事件 Schema](#四事件-schema)
5. [事件类型清单](#五事件类型清单)
6. [新包设计：`pkg/eventlog`](#六新包设计pkgeventlog)
7. [埋点位置](#七埋点位置)
8. [配置](#八配置)
9. [性能与可靠性](#九性能与可靠性)
10. [与 messages.jsonl 的边界](#十与-messagesjsonl-的边界)
11. [消费方与应用规划](#十一消费方与应用规划)
12. [实施分期](#十二实施分期)
13. [开放问题](#十三开放问题)

---

## 一、背景与问题

messages.jsonl 是**对话日志**，为上下文重放与 transcript 渲染优化。经核实，它已包含高精度时间戳、
`tool_call`/`tool_result` 配对（`tool_call_id`）、`assistant` 消息的 `Usage`——相当多信息可以推导。
但以下四类数据**推导不出或推导代价过高**：

1. **从不变成消息的执行事件**：API 重试/退避、length continuation（max 3）、MCP 重连、LSP 重启
  （max 3 restarts）、用户取消、steer 注入、auto-compact 触发、iteration budget 耗尽。
2. **被人类等待污染的延迟**：`EditFile` 等确认型工具的 tool_call → tool_result 间隔包含用户审阅
   diff 的时间，从消息时间戳推导的"工具耗时"混合了人类等待与真实执行。
3. **会话外执行**：MCP/LSP 生命周期、app 启动/退出不属于任何会话；dream 走 `RunOneOffStream`。
4. **挖掘成本**：messages.jsonl 单行可达 MB 级（scanner buffer 开到 10MB），算一个"本周工具错误率"
   需全量解析大文件并配对 call_id。事件流是追加式小行，`jq` 直接出结果。

### 与 2026-07-14-logging-redesign 的关系

`pkg/logger` 落地后已具备分级、分文件、trace_id 注入能力。本设计**复用其 trace_id 约定**，
不重复建设。两者分工：

| | pkg/logger | pkg/eventlog |
|---|---|---|
| 内容 | 自由文本诊断（人读） | 结构化事件（机器算） |
| 查询方式 | grep / tail | jq / stats 命令 / 瀑布图 |
| 典型问题 | "这行为什么报错" | "这个 turn 哪一步花了 30s" |

---

## 二、设计目标 / 非目标

### 目标

| 目标 | 说明 |
|------|------|
| **三类问题可答** | 慢（每步耗时分解）、贵（token/成本归因）、失败（错误事件全覆盖） |
| **会话内外全覆盖** | 会话内 turn 事件 + 会话外系统事件（MCP/LSP/cron/dream/channel） |
| **零外部依赖** | 纯 JSONL 文件，符合项目"文件优先"哲学 |
| **埋点零风险** | 事件写入失败不得影响 agent 主流程 |
| **低成本查询** | 小行 JSONL，`jq` 一行出结果 |
| **反哺运行时** | 事件可作为运行时信号数据源（健康信号 reminder、阈值校准），不只是事后分析 |

### 非目标

- 不替代 messages.jsonl（对话内容仍归它管）
- 不做实时 metrics 聚合 / dashboard（事件流是数据源，聚合留给消费方）
- 不引入 OpenTelemetry 依赖（预留 Sink 接口，未来可加 OTel 导出器）
- 不实现完整分布式 span 树（见 [四、事件 Schema](#四事件-schema) 的简化说明）

---

## 三、总体设计

### 双流布局

事件按**归属**写入两个流：

```
~/.tachi/
├── session/<id>/
│   ├── messages.jsonl      # 对话日志（现有）
│   └── events.jsonl        # 会话事件流（新增）
└── logs/
    ├── *.log               # 诊断日志（现有 pkg/logger）
    └── events.jsonl        # 全局事件流（新增）
```

**归属原则**：

- 有会话上下文的事件（turn / llm_call / tool_call / steer / compact / subagent）→ **会话流**
- 无会话的系统事件（app 生命周期、MCP、LSP）→ **全局流**
- "触发器"事件（cron_job、dream、channel_msg_in）→ **全局流**，携带其产生的
  `session_id` + `trace_id` 做关联（cron 经由 `channel/manager/cron.go` 的
  `RunConversationStream` 执行，有真实会话；dream 走 `RunOneOffStream`）
- `turn_end`（含成本汇总）**双写**全局流，便于跨会话成本/用量统计而无需遍历全部会话目录

### 关联模型

不引入 span_id 树，复用现有标识符（刻意的简化）：

| 关联 | 机制 |
|------|------|
| turn 内全部事件 | `trace_id`（现有 `turn_<8hex>`，`agent_loop.go` 生成） |
| LLM 调用对 | `trace_id` + `seq`（turn 内第几次 API 调用，对应 `apiCallCount`） |
| 工具调用对 | `tool_call_id`（协议层已有，与 messages.jsonl 一致） |
| 子代理 | `subagent_id` + 父 `trace_id` |
| 触发器 → 会话 | `session_id` + `trace_id` |
| 与 messages.jsonl join | `tool_call_id` / 时间戳 |

理由：turn 内是线性循环（LLM → 并行工具 → LLM …），`seq` 与 `tool_call_id` 已足够重建
完整时序；span 树只在跨进程嵌套时必要，而 subagent 用 `subagent_id` 单层关联即可。

---

## 四、事件 Schema

### 信封（Envelope）

所有事件统一结构：

```json
{
  "v": 1,
  "ts": "2026-07-20T15:18:13.921126+08:00",
  "type": "tool_call_end",
  "trace_id": "turn_a1b2c3d4",
  "session_id": "2026-07-20-151813-4b28611f",
  "attrs": { "name": "EditFile", "duration_ms": 45230, "exec_ms": 380, "confirm_wait_ms": 44850, "status": "ok" }
}
```

| 字段 | 说明 |
|------|------|
| `v` | schema 版本，恒为 1；破坏性变更才递增 |
| `ts` | RFC3339Nano 带时区（与 messages.jsonl 一致） |
| `type` | 事件类型，snake_case |
| `trace_id` | turn 级追踪 ID；全局流中系统事件可缺省 |
| `session_id` | 始终携带（会话流中也带），便于双流合并后聚合 |
| `attrs` | 类型专属负载，扁平键值，**只存元数据不存内容** |

### 内容红线

- **不记录** tool args / result 内容、用户消息内容、LLM 生成内容——这些在 messages.jsonl 中，
  事件只存尺寸（`args_bytes` / `result_bytes` / `input_len`）。避免双份存储与敏感信息扩散。
- channel 事件不存原始 chat_id / 用户标识，必要时存哈希。

### 字段稳定性约定

以下字段是下游消费方（健康信号 reminder、成本报表、CI 门禁）的**稳定契约**。
Schema 演进（`v` 不变）只允许新增枚举值和 attrs 属性；这些字段不得改名、不得改语义：

- `tool_call_end.attrs.status` ∈ {ok, error, denied, timeout}
- `turn_end.attrs.exit_reason` ∈ {stop, length, cancel, budget, error}
- `turn_end.attrs.cost_cny`
- `llm_call_end.attrs.usage` 的 {input, output, cache_read, cache_create}
- `llm_call_start.attrs.est_input_tokens`

理由：运行时反哺（如"某 MCP 近 10 次调用 8 次失败"的 reminder）直接依赖这些枚举做聚合，
一旦改名会产生静默错误的统计。新增枚举值时消费方必须把未知值归入 "other" 兜底。

---

## 五、事件类型清单

### 会话流（`session/<id>/events.jsonl`）

| type | attrs | 说明 |
|------|-------|------|
| `turn_start` | `source`(tui/channel/acp/pipe), `input_len`, `resumed`(bool) | trace_id 生成处 |
| `turn_end` | `iterations`, `duration_ms`, `exit_reason`(stop/length/cancel/budget/error), `usage{input,output,cache_read,cache_create}`, `cost_cny` | 数据来自现有 `RunResult`；双写全局流 |
| `llm_call_start` | `seq`, `provider`, `model`, `history_msgs`, `est_input_tokens` | |
| `llm_call_end` | `seq`, `duration_ms`, `ttft_ms`, `finish_reason`, `usage{...}`, `cost_cny` | TTFT 见埋点说明 |
| `llm_retry` | `seq`, `attempt`, `reason`(length_continuation/api_error), `backoff_ms` | 每次 continuation/重试一条 |
| `tool_call_start` | `name`, `tool_call_id`, `args_bytes`, `is_mcp`(bool), `parallel_idx` | |
| `tool_call_end` | `name`, `tool_call_id`, `duration_ms`, `exec_ms`, `confirm_wait_ms`, `status`(ok/error/denied/timeout), `result_bytes` | **exec_ms 与 confirm_wait_ms 分离**，解决人类等待污染 |
| `steer_injected` | `len` | |
| `compact` | `trigger`(manual/auto), `before_tokens`, `after_tokens`, `duration_ms` | |
| `subagent_start` | `subagent_id`, `worktree`(bool), `prompt_len` | |
| `subagent_end` | `subagent_id`, `duration_ms`, `status`, `usage{...}` | |

### 全局流（`logs/events.jsonl`）

| type | attrs | 说明 |
|------|-------|------|
| `app_start` / `app_stop` | `version`, `mode`(tui/channel/acp/pipe) | |
| `mcp_connect` / `mcp_reconnect` / `mcp_disconnect` | `server`, `duration_ms`, `error`, `deferred`(bool) | 含异步 InitMCPAsync 路径 |
| `lsp_start` / `lsp_restart` / `lsp_exit` | `server`, `ext`, `restart_count` | restart_count 已有 |
| `cron_job_start` / `cron_job_end` | `job_id`, `job_type`(oneshot/recurring), `session_id`, `trace_id`, `duration_ms`, `status`, `notify` | |
| `dream_start` / `dream_end` | `domain`, `session_count`, `duration_ms`, `status`, `skip_reason` | gate 拦截也记录（skip_reason） |
| `channel_msg_in` | `channel`, `msg_len`, `session_id`, `trace_id` | 入站消息 → turn 关联 |
| `session_create` / `session_delete` | `session_id`, `source` | |

---

## 六、新包设计：`pkg/eventlog`

### 包结构

```
pkg/eventlog/
├── event.go     # Event 信封类型 + 构造
├── sink.go      # Sink 接口 + JSONL 文件实现 + Nop
├── context.go   # context 传递（与 pkg/logger 同模式）
└── emit.go      # Emit() 统一埋点入口
```

### API 草图

```go
package eventlog

type Event struct {
    V         int            `json:"v"`
    Timestamp time.Time      `json:"ts"`
    Type      string         `json:"type"`
    TraceID   string         `json:"trace_id,omitempty"`
    SessionID string         `json:"session_id,omitempty"`
    Attrs     map[string]any `json:"attrs,omitempty"`
}

type Sink interface {
    Write(Event)
    Close() error
}

// 构造：会话流 / 全局流
func NewSessionSink(sessionID string) (Sink, error) // → session/<id>/events.jsonl
func GlobalSink() Sink                              // → logs/events.jsonl（单例）
func Tee(sinks ...Sink) Sink                        // turn_end 双写用

// context 传递
func WithSink(ctx context.Context, s Sink) context.Context

// 唯一埋点入口：自动填充 v/ts/trace_id（logger.TraceIDFromContext）/session_id。
// ctx 无 sink 时 no-op。任何写入错误仅 debug log，绝不返回。
func Emit(ctx context.Context, typ string, attrs map[string]any)
```

### 关键决策

| 决策 | 选择 | 理由 |
|------|------|------|
| 调用面 | 单一 `Emit(ctx, type, attrs)` | 信封字段集中填充，埋点处零样板 |
| trace_id 来源 | `logger.TraceIDFromContext(ctx)` | 复用现有约定，两处永不脱节 |
| 写入方式 | 同步 append（同 `AppendMessage`） | 事件量小（见九），异步化是过度设计 |
| 轮转 | 全局流 10MB×5（复用 logger 轮转策略）；会话流不轮转 | 会话流随 max 100 sessions 清理自然有界 |
| SubAgent 继承 | ctx 传递 sink → subagent 事件落入父会话流 | 与 `subagent/<id>.jsonl` 消息记录互补 |

---

## 七、埋点位置

| 文件 | 位置 | 事件 |
|------|------|------|
| `agent/agent_loop.go` | `runAgentLoop` 入口（trace_id 生成处）/ `TurnComplete` 出口 | `turn_start` / `turn_end` |
| `agent/agent_loop.go` | `CreateChatStream` 调用点前后 | `llm_call_start` / `llm_call_end` |
| `agent/agent_loop.go` | `length` continuation 分支 | `llm_retry` |
| `agent/stream_consumer.go` | `consumeStream` 首个 `TextDelta`/`ThinkingDelta` | 记录 TTFT，回填 `llm_call_end.ttft_ms` |
| `agent/tool_executor.go` | `executeToolCalls{Parallel,Sequential}` | `tool_call_start` / `tool_call_end` |
| `agent/tool_executor.go` | `ToolResultPendingConfirm` 分支：挂起时记时刻，恢复执行时计算 | `tool_call_end.confirm_wait_ms` |
| `agent/compact.go` | compact 执行处 | `compact` |
| `agent/subagent/executor.go` | `Execute` 前后（含 worktree fallback） | `subagent_start` / `subagent_end` |
| `agent/mcp/` | connect / reconnect / disconnect（含 `InitMCPAsync`） | `mcp_*` |
| `agent/lsp/manager.go` | server start / restart / exit | `lsp_*` |
| `channel/manager/cron.go` | job 触发与完成（携带 `RunConversationStream` 的 session） | `cron_job_*` |
| `channel/manager/dream.go` + `dream/` | dream 触发、gate 拦截、完成 | `dream_*` |
| `channel/manager/agent_turn.go` | 入站消息分发处 | `channel_msg_in` |
| `main.go` | 各模式入口 / 退出 | `app_start` / `app_stop` |
| `session/` | `CreateSession` / `DeleteSession` | `session_*` |

**取消路径**：用户 Ctrl+C（cancel steer）时 `runAgentLoop` 退出分支补 `turn_end{exit_reason: cancel}`，
已开始的工具调用补 `tool_call_end{status: error}`——保证事件流中无"悬挂"的 start。

---

## 八、配置

`config.yaml` 新增：

```yaml
observability:
  events:
    enabled: true        # 总开关
    session_stream: true # 会话流
    global_stream: true  # 全局流
    max_size: 10mb       # 全局流轮转
    max_files: 5
```

默认全开。`enabled: false` 时 `Emit` 直接 no-op（ctx 中无 sink），无运行时开销。

---

## 九、性能与可靠性

### 事件量估算

重度 turn（50 迭代上限）：~50 组 `llm_call_*` + ~100 组 `tool_call_*` ≈ **200 行 × ~250B ≈ 50KB/turn**。
普通 turn（3-5 迭代）：< 5KB。channel 模式一天数百 turn，全局流（含双写的 `turn_end`）
约 10-20MB/天 → 10MB×5 轮转保留约 3-5 天，可配置。

### 可靠性原则

1. **写入失败零影响**：`Emit` 内部 recover + error 仅写 debug log，调用方无感
2. **无悬挂事件**：cancel/timeout/panic 路径必须补对应的 `*_end`（见七、取消路径）
3. **不 fsync**：与 messages.jsonl 同策略（OS page cache 足够，进程崩溃丢尾部事件可接受）
4. **并发安全**：Sink 内部持 mutex（parallel 工具调用并发 Emit）

### 测试策略

- 每个事件类型的 envelope 序列化单测
- 集成测试：fake provider 跑一个完整 turn，断言事件序列（类型顺序、trace_id 一致、start/end 配对）
- `Emit` 在无 sink / sink 写入失败时的 no-op 行为

---

## 十、与 messages.jsonl 的边界

| | messages.jsonl | events.jsonl |
|---|---|---|
| 定位 | 对话内容日志 | 执行遥测 |
| 消费者 | LLM 上下文重放、transcript 渲染 | 人 / jq / stats 命令 / 瀑布图 |
| 内容 | 消息全文（重） | 元数据：id、尺寸、耗时、状态（轻） |
| 关联 | — | 通过 `tool_call_id` + 时间戳 join 过来 |

原则：**事件永不复制消息内容**。需要看某次工具调用的具体入参时，用 `tool_call_id`
回查 messages.jsonl。

---

## 十一、消费方与应用规划

事件流是通用数据源，消费方独立演进。按用途分六类：

### 1. 直接出口（事后分析）

1. **`tachi stats <session-id>`**（CLI）：读会话 events.jsonl，输出 turn 耗时瀑布文本版、
   工具调用排行、token/成本汇总
2. **transcript HTML 瀑布图**：`agent/transcript/render/` 增加 trace waterfall 视图
   （LLM 调用 → 并行工具的时间条，标注 TTFT / confirm_wait / token）
3. **TUI `/stats`**：当前会话实时聚合
4. **（可选）OTel Sink**：实现 `Sink` 接口将事件转为 OTLP span（GenAI semantic conventions），
   接入 Jaeger/Langfuse。接口已预留，不引依赖

### 2. 运行时反哺（agent 自我感知）

事件流不只是事后分析，可成为 agent 的**实时输入**，与 system reminder 架构天然契合：

- **健康信号 reminder**：`systemreminder.Collector` 新增数据源——聚合近期事件注入提醒，
  如"MCP server X 近 10 次调用 8 次失败"→ LLM 主动换工具或建议 `/mcp reconnect`，
  而非反复撞墙。依赖 [字段稳定性约定](#四事件-schema)
- **`/doctor` 事后视角**：doctor 不只是即时 ping，而是读事件流回答慢性问题——
  "MCP 过去 7 天重连几次、LSP 重启是否触顶（max 3）"，即时检查发现不了慢性劣化
- **dream 新数据源**：dream sub-agent 可读事件流发现**使用模式**（周期性任务、
  高成本项目），写入 memory topics 反哺 recall
- **魔数校准**：iteration budget（50）、auto-compact 阈值（80%）、hashline
  `fuzzy_threshold`（0.95）等经验值，依据事件统计数据驱动地校准

### 3. 成本治理

- **成本报表**：`tachi cost --since 7d`，按 model / source（tui/channel/cron）/ 项目分解；
  cron 任务成本归因（"哪个定时任务最烧钱，值不值"）
- **预算告警**：日累计成本超阈值 → channel 主动推送（复用现有通知通路）
- **异常 turn 检测**：某 turn 成本超历史 P95 的 N 倍 → 当场提示"会话很贵，要不要 `/compact`"，
  比 80% 上下文阈值更早介入

### 4. 工程应用（开发 tachi 自身）

- **harbor_adapter / Terminal-Bench**：事件流直接产出每个 task 的结构化结果
  （成本、耗时、工具序列、失败点），基准分析不再从消息日志里抠
- **prompt 膨胀门禁**：CI 跑标准任务集，对比 main 分支的 `est_input_tokens` / 总 token，
  涨幅超 20% 报警——system prompt 或工具 schema 改大了立刻可见
- **A/B 验证**：改 system prompt、换模型、调参数后，用历史同类任务的 turn 事件
  对比耗时与 token，改动有效性不再凭感觉

### 5. 工具生态与配置优化

- **工具使用率审计**：从未被调用的内置/MCP 工具 → 精简 schema（直接省 token）或
  优化 description
- **慢工具画像**：各工具 P95 耗时排名，定位如 `LSP workspaceSymbol` 平均 8s 的问题
- **MCP 去留三指标**：重连频率 + 调用失败率 + 使用率，数据化决定 MCP server 取舍

### 6. 会话管理与审计

- **会话列表指标列**：`tachi sessions` 显示 turns / cost / errors，支持 `--sort cost`
  （"上周那个花了 5 块钱的会话是哪个"）
- **轻量审计**：配合 `2026-07-20-bash-permission-design`，该功能落地时按 schema 演进规则
  **新增** `bash_exec` / `bash_denied` 事件（additive），统计危险命令频率与拒绝分布；
  channel 模式下"谁在什么时候触发了什么"有迹可查

### 应用落地顺序

| 应用 | 依赖分期 | 预估成本 | 备注 |
|------|---------|---------|------|
| 健康信号 reminder | P1 | 低 | 离线聚合器读 JSONL，无新子系统 |
| `tachi cost` 报表 | P1（P2 双写后更全） | 低 | |
| prompt 膨胀门禁 | P1 | 中 | 需先建标准任务集 |
| `tachi stats` / 瀑布图 | P1 / P2 | 中 | 原计划消费方 |
| doctor 事后视角 | P2 | 低 | |
| dream 数据源 | P2 | 中 | dream prompt 增加读事件流能力 |
| bash 审计 | bash-permission 落地时 | 低 | 新增事件类型，一行 Emit |
| OTel Sink | P4 | 高 | 可选 |

### jq 查询示例

```bash
# 某 turn 的完整时序
jq -c 'select(.trace_id=="turn_a1b2c3d4")' session/<id>/events.jsonl

# 工具错误率排行（跨会话）
cat session/*/events.jsonl | jq -r 'select(.type=="tool_call_end") | "\(.attrs.name)\t\(.attrs.status)"' | sort | uniq -c | sort -rn

# 今日成本（全局流双写的 turn_end）
jq -r 'select(.type=="turn_end") | .attrs.cost_cny' logs/events.jsonl | awk '{s+=$1} END {print s}'

# MCP 重连频率
jq -c 'select(.type=="mcp_reconnect")' logs/events.jsonl | wc -l
```

---

## 十二、实施分期

| 期 | 内容 | 价值 |
|----|------|------|
| **P1** | `pkg/eventlog` + 会话流核心事件（turn/llm_call/tool_call/steer） | 回答"慢和贵"，占整体价值 80% |
| **P2** | 全局流（mcp/lsp/cron/dream/channel/app）+ compact/subagent + 双写 | 回答"失败"，channel 模式运维可见 |
| **P3** | 消费方：`tachi stats` → transcript 瀑布图 → TUI `/stats` | 数据变现 |
| **P4** | （可选）OTel Sink | 外部系统集成 |

P1 预计改动：`pkg/eventlog` 新包 + `agent_loop.go` / `tool_executor.go` / `stream_consumer.go`
埋点 + 配置项，不涉及其他子系统。

---

## 十三、开放问题

1. **`turn_end` 双写的边界**：目前仅 `turn_end` 双写全局流。若未来需要跨会话工具统计，
   是否把 `tool_call_end` 也双写？倾向否（遍历 session 目录可接受），待 P3 验证。
2. **pipe 模式（`tachi -p`）**：one-off 运行若有 session 则事件入会话流；若无（SkipWrites 路径），
   退化为全局流 + `mode=pipe`。需实现时核实。
3. **channel_msg_in 的标识**：chat_id / 用户标识是否哈希后记录？取决于排查渠道问题时
   是否需要定位到具体会话。
4. **事件保留策略**：会话流随 session 清理（max 100）删除；全局流按轮转。是否需要
   独立的更长保留（如成本数据）？可后续加 `cost.jsonl` 专用追加流，不在本期。
5. **健康信号 reminder 的聚合窗口**：运行时反哺需要读"近期事件"——读当前会话流
   （快，但跨会话问题如 MCP 劣化不可见）还是全局流（全，但每次 reminder 收集都要
   tail 大文件）？倾向：reminder 聚合器只 tail 全局流末尾 N KB，窗口为最近 M 小时，
   具体参数在实现时测定。
