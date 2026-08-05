# Usage Billing — 统一计费账本设计文档

> 目标：让 one-off 请求（标题生成、keyword 提取、commit、review、dream、ambient 等）计入成本，
> 并修复模型切换后按"当前模型单价混算历史全部请求"的计费错误。
> 机制：provider 层包装统一采集 usage → append-only 按日账本 → `/usage` 成本切账本单一来源，按行计价。

---

## 目录

1. [背景与问题](#一背景与问题)
2. [设计目标 / 非目标](#二设计目标--非目标)
3. [现状盘点](#三现状盘点)
4. [总体设计](#四总体设计)
5. [文件格式与落盘布局](#五文件格式与落盘布局)
6. [配置](#六配置)
7. [归属策略（模式差异）](#七归属策略模式差异)
8. [成本计算：按行计价](#八成本计算按行计价)
9. [展示：会话视图 + 全局视图](#九展示会话视图--全局视图)
10. [与既有系统的不变量](#十与既有系统的不变量)
11. [API 变更与调用点改造清单](#十一api-变更与调用点改造清单)
12. [测试计划](#十二测试计划)
13. [实施分期](#十三实施分期)
14. [开放问题](#十四开放问题)

---

## 一、背景与问题

### 问题 1：三类 one-off 请求漏计费

`/usage`（`ComputeSessionUsage`）只聚合两个来源：会话 `messages.jsonl` 与
`session/<id>/subagent/*.jsonl`。其余 LLM 请求全部漏计：

| 类别 | 范围 | 现状 |
|------|------|------|
| **1. 直连 `CreateChat`** | 标题生成、keyword 提取、compact 摘要、deepresearch 查询/报告 | `resp.Usage` 拿到后直接丢弃，**从未落盘**——不是"没聚合"而是"没采集" |
| **2. 走 loop 的 one-off** | `/commit`、`/review`、ambient、dream、github bot | usage **已写入 oneoff 侧车 JSONL**（`recordAssistantTurn` → `oneoffRec.record`），只是 `/usage` 没扫 `oneoff/` 目录——只差聚合 |
| **3. 无 session 上下文** | `tachi -c`、`tachi -p`、dream、github、deepresearch 研究员 | 连归属地都没有；无会话下的 subagent 连 `subagent/<id>.jsonl` 都不生成（`subagent/executor.go` 要求 `ParentSessionID() != ""`） |

关键事实：**`tachi -p`（`main_agent.go` 的 `runAgent`）根本没有挂 SessionManager**——
整个会话不落盘、不计费；`tachi -c`、dream、github 同样无 session。

### 问题 2：模型切换后计费错误

- `session.Usage` / `session.Message` 只有 token 数，**没有 provider/model 字段**；
- 三个前端 `/usage` 全部用**当前** provider + **当前** model 解析一个价格，对整个会话的
  **历史 token 总和**一次性计价（TUI `model.go:resolveModelPrice`、ACP
  `commands.go:225`、channel `commands_mcp_usage.go:122`）；
- 后果：`/model` 切换后，切换前的请求也按新模型价格算。flash（¥1/2M）→ pro（¥3/6M）
  混会话可高估/低估 50%；切到无内置价目表的模型（Anthropic/OpenAI）则整个会话显示
  "No pricing data available"；provider 价格 override 与模型错配时双重错位。

旧路径（`messages.jsonl`）**补不了**问题 2：数据模型里没有 model 维度，修复要改
`session.Usage` 存储协议 + 历史迁移 + 读取兼容。本设计从源头（provider 边界）解决，
一并覆盖问题 1 与问题 2。

---

## 二、设计目标 / 非目标

### 目标

| 目标 | 说明 |
|------|------|
| **全量采集** | 所有 LLM 调用（loop 会话、one-off、直连、无 session）的 usage 进账本 |
| **按行计价** | 每次请求按**当时的** provider/model 计价，模型切换不再混算 |
| **双维度展示** | 会话视图（`/usage`）+ 全局视图（`/usage --all`）；无 session 成本在全局视图显形 |
| **埋点零风险** | 采集/记录失败只告警，绝不影响 LLM 执行本身 |
| **文件优先** | 纯 JSONL，grep/jq 友好，与 oneoff/subagent recorder 同构 |
| **成本单一来源** | 账本是唯一成本来源，杜绝 messages/subagent/账本多处聚合的双重计费 |

### 非目标

- 不做实时聚合 / 看板 / 统计（eventlog 的职责）
- 不改 `session.Usage` 存储协议、不迁移历史数据（升级前会话显示"无计费数据"）
- 不扩展价格表（未知模型单列"未计价"，不猜测价格）
- 不改变会话 token/上下文统计语义（context%、compact 阈值仍走 messages.jsonl 估算）

---

## 三、现状盘点

### 已计入（现状 `/usage`）

1. 主会话 turn：usage 在 `messages.jsonl` 的 assistant 行（`recordAssistantTurn` → `AppendMessage`）。
2. 有 session 上下文时的 subagent：usage 经 `StreamEventTurnComplete` 写入
   `session/<id>/subagent/<shortID>.jsonl`（`subagent/executor.go`），
   `ComputeSessionUsage` 经 `LoadSubagentMessages` 聚合。

### 漏计费：直连 `CreateChat` 调用点（全部非流式）

| # | 调用点 | 用途 | 无 session 时 |
|---|--------|------|---------------|
| 1 | `agent/agent_provider.go` `generateTitle` | 会话标题 | `-p` 无 SM → 全局 |
| 2 | `agent/extractor.go` `LLMKeywordExtractor.ExtractKeywords` | memory 检索 keyword | —（会话内触发） |
| 3 | `agent/compact_strategy.go` `llmCompactStrategy.Compact` | /compact + 自动 compact 摘要 | — |
| 4 | `channel/manager/commands_model.go`（/model 摘要） | /model 切换时摘要 | — |
| 5 | `agent/deepresearch/engine.go` `generateQueries` / `generateReportHTML` | 查询生成 + 报告 HTML | 全局 |

### 漏计费：走 loop 的 one-off（usage 已在侧车，缺聚合）

| # | 调用点 | kind | 会话上下文 |
|---|--------|------|-----------|
| 1 | `main_agent.go` `runCommit`（`tachi -c`） | `commit` | 无 SM → 全局 oneoff 目录 |
| 2 | TUI/ACP/channel `/commit` | `commit` | 有 session |
| 3 | TUI/ACP/channel `/review`（fork） | `review` | fork 无 SM，meta 带 session id |
| 4 | `channel/manager/ambient.go` | `ambient` | fork 无 SM，锚 thread session |
| 5 | `dream/runner.go` | `dream` | 无 |
| 6 | `channel/github/{discussion,pr_agent}.go` | `github-*` | 无 |

### 已核实的关键事实

1. `StreamEvent` 的 done 事件携带 `Usage`，`Response` 携带 `Usage`（`llm/openai.go:315`、
   `llm/anthropic.go:360`）——provider 边界可统一采集，无额外请求。
2. 三前端 `/usage` 的价格都是"当前 provider + 当前 model"（见问题 2），会话内无模型维度。
3. `tachi -p` 全程无 SessionManager（`main_agent.go`），会话不落盘。
4. oneoff 侧车的 assistant 行已含完整 `Usage` 字段（`usageToSession`），格式与
   `messages.jsonl` 同构。

---

## 四、总体设计

### 核心链路

```
LLM 调用（CreateChat / CreateChatStream）
        │
        ▼
RecordingProvider（llm 层包装，agent 构造 + Fork 时套在全部 provider 外）
        │  读 ctx 的 kind + opts.SessionID
        ▼
UsageRecorder ──O_APPEND 单行写──▶ ~/.tachi/usage/YYYY-MM-DD.jsonl
        │
        ▼
/usage（会话视图：session_id 过滤 + kind 分组）   /usage --all（全局视图）
```

### 1. 采集：`RecordingProvider`

```go
// llm 层新增（与 RetryProvider 同构的装饰器；必须是装饰器链最外层，见下）
type RecordingProvider struct {
    inner llm.Provider
    rec   *UsageRecorder
    price func(model string) *llm.ModelPrice // 注入：捕获 cfg + 配置 provider 名
}
func (p *RecordingProvider) CreateChat(...) (*Response, error) {
    resp, err := p.inner.CreateChat(...)
    if err == nil && resp.Usage != nil { p.rec.Record(ctx, opts, p.inner, resp.Usage) }
    return resp, err
}
func (p *RecordingProvider) CreateChatStream(...) (<-chan StreamEvent, error) {
    ch, err := p.inner.CreateChatStream(...)
    // passthrough goroutine：原样转发事件；done 事件的 Usage 记录一次
    return wrapped, err
}
```

- **包装点**：`NewAIAgentWithConfig`/`configure` 内统一包装 main provider 与全部
  dedicated provider（title/commit/review/keyword/subagent）；`Fork` 时子代理继承
  包装（fork 手工复制 Config，需在 `agent_fork.go` 补套）。channel 每线程 agent
  同样走 `NewAIAgentWithConfig`，自动覆盖。
- **装饰器顺序**：`RecordingProvider` **必须是装饰器链最外层**（在
  `RetryProvider` 之外）。否则重试场景下每次成功尝试各记一行——一次逻辑调用产生
  多行，成本被重复计数。实现时在 `WrapProviders` 处固定顺序并注释理由，防止后续
  维护者调整。
- **price resolver**：包装时注入闭包 `func(model string) *llm.ModelPrice`（捕获 cfg +
  该 provider 的配置名）——Record 时用它解析**当时的**单价快照写入账本行
  （§8）。`inner.Name()` 只返回类型名，无法查 override，必须注入。
- **kind 来源**：上下文 `usage.WithKind(ctx, "commit")`，默认 `conversation`。
  `RunOneOffStream` / `RunConversationStream` 用 `meta.Kind` 设一次（一个调用点覆盖
  commit/review/ambient/dream/github 全部）；subagent 在 `subagent/executor.go` 的
  `run()` 内设置 `kind=subagent`（子代理不经过 meta 路径）；直连调用点各加一行
  （§11 清单）。
- **session 锚定（三级）**：`opts.SessionID` 非空用之；否则回退
  `llm.SessionIDFromCtx(ctx)`（调用方在 ctx 上显式 `llm.WithSessionID`）；仍为空 →
  全局（session_id 字段留空）。要点：
  - runLoop 已设置 `opts.SessionID`（`agent_loop.go`，来自 `SessionManager.Current()`）；
  - **oneoff 路径统一复用 `resolveOneoffSessionID`**（`agent/oneoff_recorder.go`：
    meta.SessionID → 当前会话 → 全局 三级逻辑），在 `RunOneOffStream` /
    `RunConversationStream` 的 oneoff 分支把解析结果 `llm.WithSessionID` 注入 ctx——
    ambient 与 /review fork（均"meta 带 session id"）一并覆盖，不制造特例；
  - **subagent 行规范化**（已核实 `subagent/executor.go:115-118` 用
    `parentID + ":" + shortID` 作为 `opts.SessionID`）：Record 时若 `kind=subagent`，
    `session_id` 取 `opts.SessionID` 的 `":"` 前缀（主会话 id）；无冒号（无父会话）
    则留空归全局——避免复合 ID 绕过会话过滤、裸 shortID 成为隐形孤儿；
  - 直连调用点锚定见 §11 清单（逐点明确，防静默落全局）。
  包装器不依赖 agent 状态，纯粹 `(ctx, opts, provider) → 一行记录`，可独立测试。

### 2. 存储：按日分片的全局账本

单一全局账本而非 per-session 文件：

- **无 session 场景（类别 3）自然落入**，不需要特判或第二套布局；
- 按日分片使"今日 / 近 7 天 / 本月"花费直接可算，`/usage --all` 有数据源；
- 一个目录、一个 mutex；`session_id` 字段承载会话维度过滤。
  **UsageRecorder 为进程级共享单例**（由 `--home` 定位，注入到每个 agent 的 Config，
  所有 RecordingProvider 引用同一实例）——否则 per-agent 各自持锁，进程内多 agent
  并发写只靠 O_APPEND 兜底，mutex 形同虚设（正确性依赖见 §5）。

会话删除后的孤儿行**永久保留**（见 §5），全局视图继续聚合——语义上"会话已删但
花费仍在全局视图"是合理的。

### 3. 展示

`/usage` 成本计算切到账本单一来源（见 §9），Token/上下文统计保持从 messages.jsonl。

---

## 五、文件格式与落盘布局

### 布局

```
<home>/usage/YYYY-MM-DD.jsonl
```

- 一文件一天；一行 = 一次 API 调用（成功且有 usage 的调用；失败不记录）。
- 文件名日期 = 本地时区调用日。跨天调用（极少）按完成时刻归属。

### 行格式

```json
{"ts":"2026-08-05T09:30:00+08:00","session_id":"2026-08-05-0900-abcd",
 "kind":"commit","provider":"deepseek-v4-flash","model":"deepseek-chat",
 "input_tokens":1234,"output_tokens":56,"cache_read_input_tokens":100,"cache_creation_input_tokens":50,
 "input_price":1.0,"output_price":2.0,"cache_read_price":0.02,"cache_creation_price":1.0}
```

- `kind` 取值：`conversation` | `title` | `keyword` | `compact` | `commit` | `review` |
  `ambient` | `dream` | `github-discussion` | `github-pr` | `subagent` | `research-query` |
  `research-report` | …；定义为 `type UsageKind string` + 常量集，与
  `OneOffMeta.Kind`（`agent/oneoff_recorder.go`）对齐复用，避免两套字符串漂移。
  `session_id` 为空 = 无 session 的全局调用。
- **token 口径（归一化 cache-miss）**：`input_tokens` 统一为 **cache-miss（未命中）**
  口径，写入前归一化——OpenAI/DeepSeek 系 API 的 `input_tokens` 含 cache-read，须减去
  `cache_read_input_tokens`（与 `llm.CalculateCost` 的 `cacheMissInput` 语义一致）；
  Anthropic 系 API 的 `input_tokens` 不含 cache，原样写入。归一化在 Record 时一次完成，
  行内不再做减法，杜绝 cache 命中 token 双重计费（见 §8）。
- **价格单位**：`input_price` / `output_price` / `cache_read_price` /
  `cache_creation_price` 均为**每百万 token 的 CNY**（与 `llm.ModelPrice` 一致），
  成本公式按 `/1e6` 计，避免实现者按 per-token 写入造成 10⁶ 倍错误。
- `provider` 为**配置 provider 名**（如 `deepseek-v4-flash`，agent 构造时注入包装器——
  `llm.Provider.Name()` 只返回类型名 `openai`/`anthropic`，无法查 override）；
  `model` 为 `inner.Model()`（调用当时的解析结果）。这是按行计价（§8）修复模型切换
  混算的基础。
- **价格快照字段**：Record 时用**当时的**价格表解析出**最终有效价**写入——
  fallback 语义（0 = 回退 `InputPrice`）在写入前应用完毕，行内价格为实际计价价
  （示例中 `cache_creation_price` 已回退为 `1.0`；`0` 只出现在"确实免费/未计价"行）。
  行自包含。修改 config 单价或升级内置价目表**只影响之后的新行，历史行成本不变**（§8）。
- **session_id 写入规则**：普通行 = `opts.SessionID`（或 ctx 回退）原样；
  `kind=subagent` 时取 `":"` 前缀（主会话 id），无冒号则留空归全局（§4）；
  会话 id 格式为 `YYYY-MM-DD-HHMMSS-uuid8`，不含冒号，前缀规则安全。
- 文件权限 `0600`；字段命名与 `session.Usage` 对齐（`cache_read_input_tokens` 等）。

### 原子性与并发

- **正确性基础是单次 `write(2)`**：每行必须**一次 `write()` 系统调用**完成
  （`json.Marshal` 后直接 `os.File.Write`，**不得经 bufio 拆分**）。POSIX 规定
  `O_APPEND` 下 offset 定位与写入不可分割，配合单次 write 保证并发下（含跨进程，
  如 TUI 与 `tachi -c` 同时运行）行不撕裂、不错位。
- 进程内 `sync.Mutex` 为次要兜底（recorder 单例，§4），主要正确性依赖上述追加写性质。

### Retention：永久保留（不清理）

账本是费用审计数据，**永不清理**——不设 retention、不做 sweep。用户决策。

- **数据规模**：单行约 200~300 字节；日均几十到几百行（取决于使用强度），
  年增长量级为几十 MB——对本地磁盘可忽略；
- **会话删除后的孤儿行**：永久保留，全局视图继续聚合（§4）；
- **用户取消的流式调用不计费（已知取舍）**：只有 done 事件携带的 Usage 被记录，
  Esc/Ctrl+C 中断时 done 不到达，已产生并被计费的 token 不进账本——设计上接受
  （低成本换取实现简单；Anthropic 的 `StreamEventMessageDelta` 也携带 Usage，如需
  降低丢失率可记录"最后一个携带 Usage 的事件"，列入 §14 开放问题）；
- 若未来确需清理（如磁盘受限），再单独引入显式命令（如 `tachi usage prune`），
  默认行为保持"不清理"。

---

## 六、配置

**无配置项，始终开启**。账本是内置费用审计，不需要开关、没有 retention——
用户决策：不清理、不关闭。不新增 `config.yaml` section，不引入 `UsageConfig`。

未来若出现必须关闭的场景（如隐私/合规），再评估独立开关；当前不提供。

---

## 七、归属策略（模式差异）

| 模式 | session 有无 | 锚定 | 备注 |
|------|-------------|------|------|
| TUI | 有（标题生成在 `ensureSessionAndRecordUser` 建 session 之后） | `opts.SessionID` | 首个 turn 前无会话的调用（标题）也已归属新会话 |
| ACP | 有（per-session agent） | `opts.SessionID` | /commit /review 经 meta 显式传 session id |
| channel | thread session 存在；ambient fork 无 SM | oneoff 路径复用 `resolveOneoffSessionID` → ctx 注入 | fork 的 direct call 无 SM → 靠 oneoff meta 锚定（§4） |
| subagent | 继承父会话；无父会话则无 | `parent:shortID` → Record 时取 `":"` 前缀；无冒号归全局 | 复合 ID 不会绕过会话过滤（§4/§5） |
| `tachi -p` | **无 SM** | 空 → 全局 | 整个对话计入全局账本 |
| `tachi -c` | 无 SM | 空 → 全局 | kind=commit |
| dream / github | 无 | 空 → 全局 | kind=dream / github-* |

---

## 八、成本计算：按行计价 + 价格快照

```go
// 成本 = Σ(每行 token 数 × 该行"当时"的价格快照)——不查当前 config，历史成本稳定。
// 行内 input_tokens 已是归一化后的 cache-miss 口径（§5），此处不再做减法；
// 各行 token 分类互斥，cache 命中 token 只按 cache-read 价计一次：
cost += float64(row.InputTokens)/1_000_000*row.InputPrice +          // cache-miss input
        float64(row.CacheReadInputTokens)/1_000_000*row.CacheReadPrice +      // cache 命中
        float64(row.CacheCreationInputTokens)/1_000_000*row.CacheCreationPrice + // cache 写入
        float64(row.OutputTokens)/1_000_000*row.OutputPrice
```

- **价格快照在 Record 时解析并写入**（§5）：`ResolveModelPrice(cfg, 配置provider名, model)`
  的最终有效价（fallback 已应用）落行。修改 config 单价 / 升级内置价目表
  **只影响之后的新调用，历史行成本不变**——计费稳定、可预期，账本自包含可审计。
- **会话成本** = 该会话全部账本行之和（`conversation` + 各 kind，含 subagent 行——
  子代理在会话上下文内运行，视为会话成本的一部分）；
- **模型小计**：按 `(provider, model)` 分组求和，`/usage` 在会话内存在多模型时展示，
  切换影响一目了然；
- **未计价行**：Record 时解析不到价格（无 override 且无内置价目）的行以
  `input_price=0` 等快照写入，计 0，汇总"N 次调用未计价"单列提示，不污染已知部分；
- 替换现状"总 token × 当前单价"（§一问题 2），**修复模型切换混算**。

### 历史重算（可选，二期）

快照语义下修改单价不追溯历史。若用户意图是"统一修正历史"（如发现内置价目表错误），
预留显式重算入口（如 `/usage --all --reprice`：遍历账本行、忽略快照、按当前价重算），
默认不提供、不自动触发。一期不做。

### 旧会话（无账本行）

升级前创建的会话没有账本数据 → `/usage` 显示"无计费数据（升级前会话）"，
Token/上下文统计不受影响。不做回退估算、不迁移（决策：显示空）。

---

## 九、展示：会话视图 + 全局视图

### `/usage`（会话视图，TUI / channel / ACP 现状命令）

- **Token 段**：保持现状（messages.jsonl 累计 + context%），语义不变；
- **Cost 段**：改为账本聚合——

```
**Cost**
会话成本: ¥0.0182   （conversation ¥0.0131 + subagent ¥0.0051）
旁路请求: ¥0.0049
  ├─ title   ¥0.0001 × 2
  ├─ commit  ¥0.0046 × 1
  └─ research-report ¥0.0002 × 1
模型分布: deepseek-v4-flash ¥0.0180 | deepseek-v4-pro ¥0.0051
2 次调用未计价（无价格表）
```

- 无账本行 → "无计费数据（升级前会话）"。
- **实现要点**：
  - 会话过滤 = `session_id == 当前会话 id`（subagent 行已归一化为主会话 id，等值匹配
    即覆盖，无需前缀匹配）；
  - **扫描下界**：session id 格式内嵌创建日期（`YYYY-MM-DD-…`，已核实
    `session/store.go` `GenerateID`），直接跳过早于会话创建日的日文件，避免文件数与
    读取量随账本年限线性增长（`--all` 全局视图天然全扫，不受此约束）。

### `/usage --all`（全局视图，新增）

- TUI：`/usage --all`；channel/ACP：`/usage all`（共享 `commands` 包的参数解析）；
- 聚合 `<home>/usage/*.jsonl`：今日 / 近 7 天 / 30 天总花费 + 按 kind、按模型分组；
- **无 session 的 `-c` / `-p` / dream / github 成本在此显形**（session_id 为空的行）。

---

## 十、与既有系统的不变量

本设计依赖并必须维持以下不变量（review 检查清单）：

1. **messages.jsonl 零改动**：不往 `session.Usage` / `session.Message` 加字段，
   存储协议不变；Token/上下文统计、compact 阈值、statusbar context% 全部不受影响。
2. **成本单一来源**：`ComputeSessionUsage` 的 Cost 只从账本算；**禁止**再叠加
   messages/subagent 的 usage 计 cost（subagent 调用已进账本，叠加即双重计费）。
3. **oneoff 侧车 / subagent jsonl 保持现状**：仍用于调试/排查（oneoff 侧车见
   `docs/2026-07-24-oneoff-transcript-design.md`），但不参与成本计算。
4. **埋点零风险**：采集失败（目录不可写、序列化错误）只 Warn，绝不影响 LLM 执行。
5. **one-off 隔离语义不变**：账本不写 session、不进 memory/dream、不 bump
   `UpdatedAt`——dream 门控、会话排序不受影响。
6. **消息透传无损**：`RecordingProvider` 的流式包装必须原样转发每个事件，不得丢包、
   不得改变顺序（测试重点）。
7. **subagent 行为不变**：子代理执行、记录路径（`subagent/<id>.jsonl`）不动；
   只在其 provider 上多套一层账本采集。
8. **隐私**：账本行**永不写入 prompt/响应内容片段**——只含 token 计数、价格与
   归属元数据；文件权限 `0600`。未来扩展（如记录调用摘要）不得违反此条。

---

## 十一、API 变更与调用点改造清单

### 新增

```go
// llm/usage_recorder.go
type UsageKind string // 常量集对齐 OneOffMeta.Kind，避免两套字符串漂移
const (
    UsageKindConversation UsageKind = "conversation"
    UsageKindTitle        UsageKind = "title"
    UsageKindKeyword      UsageKind = "keyword"
    UsageKindCompact      UsageKind = "compact"
    UsageKindCommit       UsageKind = "commit"
    UsageKindReview       UsageKind = "review"
    UsageKindAmbient      UsageKind = "ambient"
    UsageKindDream        UsageKind = "dream"
    UsageKindSubagent     UsageKind = "subagent"
    UsageKindResearchQuery   UsageKind = "research-query"
    UsageKindResearchReport  UsageKind = "research-report"
    // github-discussion / github-pr 等按需扩展
)

type UsageRecorder struct { ... }                    // 写 <home>/usage/ 按日文件；进程级共享单例（§4）
func (r *UsageRecorder) Record(ctx, opts ChatOptions, p Provider, u *Usage)
func WithUsageKind(ctx context.Context, kind UsageKind) context.Context
func UsageKindFromCtx(ctx context.Context) UsageKind // 默认 "conversation"

type RecordingProvider struct {
    inner llm.Provider
    rec   *UsageRecorder
    price func(model string) *llm.ModelPrice // 注入：捕获 cfg + 配置 provider 名，Record 时解析单价快照
} // 实现 llm.Provider；WrapProviders(inner, rec, price) 构造；必须是装饰器链最外层（§4）
```

### 改动清单

| 文件 | 改动 |
|------|------|
| `llm/usage_recorder.go` | 新增：RecordingProvider + UsageRecorder（永久保留，无 sweep）+ UsageKind 常量 |
| `agent/agent_config.go` / `agent_configure.go` | 构造时 WrapProviders（main + dedicated） |
| `agent/agent_fork.go` | Fork 子代理继承包装 |
| `agent/agent_loop.go` | oneoff 分支（RunOneOffStream / RunConversationStream）：`WithUsageKind(ctx, meta.Kind)` + `llm.WithSessionID(ctx, a.resolveOneoffSessionID(meta))`——复用 oneoff 三级锚定，ambient / /review fork 一并覆盖 |
| `agent/subagent/executor.go` | `run()` 内 `ctx = usage.WithUsageKind(ctx, UsageKindSubagent)`（子代理不经过 meta 路径，缺此则全部落成 conversation） |
| `agent/agent_provider.go` | `generateTitle` 加 `WithUsageKind(ctx, "title")`；**锚定已有**（自行设置 `chatOpts.SessionID`，已核实 `:184-186`）✓ |
| `agent/extractor.go` | `ExtractKeywords` 加 `WithUsageKind(ctx, "keyword")`；**锚定**：`resolveKeywordProvider`（`agent_configure.go`）创建 extractor 时注入 `a.sessionID`（或在 ctx 上 `llm.WithSessionID`） |
| `agent/compact_strategy.go` | `Compact` 加 `WithUsageKind(ctx, "compact")`；**锚定**：`doCompact` 在 compactCtx 上 `llm.WithSessionID(compactCtx, a.sessionID())` |
| `channel/manager/commands_model.go` | /model 摘要加 `WithUsageKind(ctx, "compact")` + ctx 上 `llm.WithSessionID`（thread 会话）。**注意**：其 `oldProvider` 为 channel manager 自建，不在 agent 包装链——需在 manager 侧对 provider 补包装，或改走线程 agent 的 provider（见 §14 开放问题 1） |
| `agent/deepresearch/engine.go` | 查询生成 / 报告各加 `research-query` / `research-report`；锚定：有会话时在 ctx 上 `llm.WithSessionID`（缺省落全局可接受，标注） |
| `agent/usage.go` | `ComputeSessionUsage` 改为 `ComputeSessionUsage(sm SessionManager, sessionID string, rec *UsageRecorder, contextWindow int64)`——去掉 `price` 参数（成本只从账本算）、新增 sessionID 与账本来源；Cost 按行计价 + kind 分组 + 模型小计；无账本行 → 空。三前端调用点（`tui/commands_misc.go`、`channel/manager/commands_mcp_usage.go`、`agent/acp/commands.go`）同步更新 |
| `agent/commands/format.go` | `FormatUsageReport`：旁路明细、模型分布、未计价提示、`--all` 全局渲染 |
| `agent/commands/commands.go` | `/usage --all` 参数解析（或各前端各自解析） |
| `tui/commands_misc.go`、`channel/manager/commands_mcp_usage.go`、`agent/acp/commands.go` | `/usage` 调用更新 + `all` 分支 |

---

## 十二、测试计划

1. **RecordingProvider 单测**（镜像 `llm/retry_test.go` 的 stub provider 模式）：
   `CreateChat` 记录 `resp.Usage`；`CreateChatStream` passthrough 事件不丢不错序、
   done 事件 Usage 记录一次；错误路径不记录；`Usage=nil` 不记录；
   **装饰器顺序**：RecordingProvider 包在 RetryProvider 外时，一次逻辑调用（含重试
   成功）只记一行。
2. **UsageRecorder**：行格式、按日分片、`session_id` 空值、目录不可写时
   返回错误且调用方只告警；**无清理逻辑**（文件永不删除）；
   **并发写**：多 goroutine / 多 recorder 实例同时写同一日文件，断言行不撕裂、
   总数正确（依赖单次 write(2)，§5）。
3. **按行计价 + 价格快照**：同一会话混用 flash/pro 的账本行 → 各按各价；**Record 后
   修改 config 单价，重算 `/usage` 历史行成本不变**（快照语义）；未知模型行以
   `input_price=0` 快照写入、计 0 且计入"未计价"提示；`subagent` 行计入会话成本
   且不重复计（messages 侧不再计 cost）；
   **cache 只计一次**：含 `cache_read_input_tokens` 的行，input 归一化为 cache-miss
   口径后，命中 token 只按 cache-read 价计一次（OpenAI 系与 Anthropic 系各一条）。
4. **归属**：`tachi -p` / `-c`（无 SM）→ 全局行 `session_id=""`；TUI 首 turn 标题 →
   新会话 id；ambient fork → thread session id；
   **subagent 复合 ID**：`"<parent>:<shortID>"` 行归一化后计入 parent 会话成本；
   无父会话（裸 shortID）→ 归全局 `session_id=""`。
5. **/usage 视图**：会话视图 kind 分组 + 模型小计；`--all` 全局聚合（含无 session 行）；
   升级前会话（无账本行）→ "无计费数据"；
   **跨天会话**：同一会话的账本行分散在多个日文件时，会话视图聚合正确（含扫描下界
   跳过早于创建日的文件）。
6. **失败隔离**：`<home>/usage/` 不可写时，会话/one-off 执行正常完成，仅 Warn。

---

## 十三、实施分期

**一期（本设计的完整范围）**：UsageRecorder + RecordingProvider + 包装点 +
kind 标注全部调用点 + 按行计价 + `/usage` 会话视图改造。

**二期**：
- `/usage --all` 全局视图（TUI `--all`、channel/ACP `all`）；
- 全局视图展示"今日 / 近 7 天 / 30 天"分段与按模型分组；
- （可选）eventlog 落地后，账本行可作为其 `api_call` 事件的数据源。

---

## 十四、开放问题

1. **包装点位置**：agent 构造 + Fork（本设计，config 保持纯净）vs
   `config.BuildProvider`（一处覆盖含 channel manager 自建的 provider，但 config 层
   引入副作用）。当前选前者；**已确认一个绕开点**：channel `/model` 摘要的
   `oldProvider` 是 manager 自建、不在 agent 包装链（§11）——实施时要么在 manager
   侧补包装，要么改走线程 agent 的 provider；若后续出现更多绕开点，再评估后者。
2. **subagent 行的归属展示**：账本中 kind=subagent 计入会话成本（当前决定）；
   是否在会话视图里单列"子代理"明细行，取决于用户体验反馈。
3. **`tachi -p` 是否值得建 session**：账本已兜底计费；若未来希望 `-p` 也能
   `--resume`/`/usage` 查历史，可单独立项（本设计不建 session，保持其瞬时语义）。
4. **跨天/超长调用**：按完成时刻归属当日，天然避免双计；极端跨天场景接受。
5. **价格表未覆盖的模型**：Record 时以 `input_price=0` 快照落行、显示时列"未计价"
   提示；用户可通过 `provider.spec.pricing` 配置价格（按行计价天然生效，且新行
   快照采用新价、历史行不变）。
6. **历史重算**：快照语义下改价不追溯（§8）。若用户提出"统一修正历史"需求（如内置
   价目表 bug），再评估 `/usage --all --reprice`（按当前价忽略快照重算），一期不做。
7. **流式取消的 usage 丢失**（§5 已声明取舍）：当前只记录 done 事件的 Usage。
   Anthropic 的 `StreamEventMessageDelta` 也携带 Usage（`llm/anthropic.go:356-360`）；
   若希望降低丢失率，可记录"最后一个携带 Usage 的事件"（增加少量复杂度），按需评估。

---

## 十五、实施状态（2026-08-05 一期完成）

一期（§13 完整范围）已实现并通过全量测试（`go test ./...`，35 包 ok）。

**已落地**：

| 模块 | 实现 |
|------|------|
| 账本 | `llm/usage_recorder.go`：`UsageRecorder`（按日分片、O_APPEND 单次 write、0600、永久保留）+ `UsageRow`（cache-miss 归一化 + 价格快照）+ `Rows(sessionID, from)` 下界扫描 |
| 采集 | `RecordingProvider`（装饰器链最外层）：CreateChat / CreateChatStream 双路径；OpenAI 系减 cache-read、Anthropic 原样；`kind=subagent` 复合 ID 取 `":"` 前缀、无冒号归全局 |
| 包装点 | `NewAIAgentWithConfig` 构造前包装全部 provider（含 CompactStrategy/Setup 派生）；`wrapResolvedAdversarial` 在 Setup 后补包 adversarial；`Fork` 继承（子代理复用 fork 的 provider） |
| kind/锚定 | `RunOneOffStream` / `RunConversationStream` oneoff 分支：`WithUsageKind(meta.Kind)` + `resolveOneoffSessionID` 三级锚定；`subagent/executor.go` 设 `kind=subagent`；直连调用点（title/keyword/compact/deepresearch/channel /model）逐点标注 |
| /usage | `ComputeSessionUsage(sm, rec, ctxWindow)`：成本切账本单一来源，kind 分组 + 模型分布 + 未计价提示；旧会话（无账本行）→ "无计费数据"；三前端（TUI/channel/ACP）已同步 |
| 渲染 | `FormatUsageReport`：Cost 段显示总成本 + 旁路明细 + 模型分布 + 未计价行数 |
| 测试 | `llm/usage_recorder_test.go`（8 例：双路径采集、归一化、复合 ID、日切/下界、并发写、0600）+ `agent/usage_ledger_test.go`（4 例：账本聚合、nil recorder、oneoff 端到端锚定、无 session 落全局） |

**已知缺口（对应 §14）**：

- deepresearch 的 `query_generator_provider` 独立配置时其自建 provider 不在包装链（默认走 defaultProvider 已覆盖）——若需完整覆盖再评估 config 层包装；
- 二期（`/usage --all` 全局视图、历史重算）未实现。

**Round 3 审查修复（2026-08-05）**：

- **F1（核心采集缺口）**：config 解析的专用 provider（title/commit/review/run/subagent）此前逃过包装——现于 `setupDedicatedProvider` assign 路径统一补包（新建实例的唯一必经点），并加 `WrapRecordingProvider` 幂等保护（同 recorder 不双包）；`TestNewAIAgentWithConfig_DedicatedProvidersBilled` 为回归防线；
- **F2**：账本行 `Provider` 改为配置 provider 名（`RecordingProvider.providerName`，各包装点注入），对齐 §5 契约；
- **F3**：流式透传 goroutine 感知 ctx 取消（`select` 弃发），消除弃读场景的泄漏风险；
- **F4**：`review-round-N` 动态 kind 在 Record 前归一化为 `review`（`normalizeUsageKind`）；
- **F5**：`Rows()` 改 bufio 逐行流式读，配合日期下界，长线读取量不再线性膨胀；
- **F6**：无计费数据文案改"本会话暂无计费数据"（不再误标"升级前会话"）；
- **F7**：渲染拆分"会话成本"（conversation+subagent）与"旁路请求"（其余 kind），对齐 §9 示例；
- **N1**：`wrapResolvedAdversarial` 注释修正（预解析与 config 解析统一在此包装）+ 守卫条件改为按实际列表判断；
- **F9**：channel manager 的 `usageDirFn` 可注入，与 agent 双轨统一。

**Round 4 审查修复（2026-08-05）**：

- **B1（dream/github 裸构造路径漏包）**：dream runner、channel dream fallback、github
  discussion/PR 均用 `agent.NewAIAgent` 直构造（provider 不经 `wrapUsageProviders`）——
  新增导出 `agent.WrapProviderForUsage`（全局 recorder + 幂等），在 `dream/runner.go`
  （resolveProvider 后）、`channel/github/channel.go`（resolveProvider 内）补包；
  `RunConfig` 增加 `ProviderName`/`Recorder` 字段（fallback 场景行名正确 + 测试隔离）；
  `TestRunDream_RecordsUsageLedger` 为回归防线；
- **B2（channel /model compact 用错 provider 名）**：改用 `getProviderForThread` 第三
  返回值（线程实际 provider 名，锁内读取），账本行与价格解析双双正确；
- **W1**：`wrapResolvedAdversarial` 注释改为与实现一致（不再描述不存在的 early return）；
- **W2**：gofmt 清理变更集内三个文件；
- **S1**：`format.go` 冗余 `int()` 转换移除；
- **S2**：删除 `tui/model.go` 墓碑注释；
- **S3**：`UsageRecorder()` 注释改为"实际永不返回 nil，防御性容忍"。
