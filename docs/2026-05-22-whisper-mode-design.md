# Whisper Mode — 耳语模式

> 版本: 2.0 | 日期: 2026-05-22 | 状态: 设计阶段
> 关联: [Memory 系统](./2026-05-17-memory.md),
>       [Subagent 设计](./2026-05-09-subagent-design.md)

## 一、概述

### 1.1 一句话定义

`tachi whisper` 是一个独立的 CLI 命令。它在你工作时静默监听工作目录的文件变化，把事件投入一个 LLM 驱动的信号门——绝大部分改动被判定为噪音，沉默忽略；只有那些你真正想知道的事，ta 才会在终端里轻轻说一句。

和 TUI 无关。和 cron 无关。它不参与你的对话，只是一个在后台帮你**留意环境**的耳朵。

### 1.2 它不是什么

| 不是 | 是 |
|------|-----|
| 不是通知轰炸（来什么都弹） | 绝大多数事件被判定为噪音，静默丢弃 |
| 不是 cron（按时间表执行任务） | 事件驱动，只在变化发生时评估 |
| 不是 TUI 功能 | 独立 CLI 命令，无 UI 依赖 |
| 不是代码审查工具 | 通用文件感知——代码、文档、数据、配置都可以 |
| 不是 git hook | 不关心版本控制，只关心**文件级变化 + 语义判断** |

### 1.3 MVP 范围

| 范围 | MVP | 之后 |
|------|-----|------|
| 事件来源 | 文件系统 (fsnotify) | 进程监控、系统日志、API 数据源、网络流量 |
| 运行方式 | `tachi whisper` 前台进程 | daemon 模式 (`--daemon`) + systemd |
| 上下文引用 | 文件内容 + memory recall | 外部 API、实时数据 |
| 通知方式 | stdout 打印 + macOS 系统通知 | 邮件 digest、IM 推送 |

---

## 二、命令行接口

### 2.1 基本用法

```bash
# 持续监听当前目录
tachi whisper

# 监听指定目录
tachi whisper --dir ~/projects/my-paper

# 调低阈值（更多通知）
tachi whisper --threshold 5

# 指定 Gate 用的模型
tachi whisper --model claude-haiku-4-20250514

# 使用专属 provider
tachi whisper --provider cheap-provider --model gpt-4o-mini
```

### 2.2 完整参数

```
tachi whisper [flags]

Flags:
  --dir <path>           Working directory to watch (default: current directory)
  --threshold <1-10>     Minimum Signal Gate score to notify (default: 7)
  --cooldown <sec>       Minimum seconds between notifications (default: 300)
  --provider <name>      LLM provider for Signal Gate (default: main provider)
  --model <name>         Model for Signal Gate (default: cheapest available, 
                          e.g. haiku / gpt-4o-mini)
  --batch-window <ms>    Inactivity window before flushing event batch (default: 30000)
  --exclude <pattern>    Additional exclude glob (repeatable)
  --max-events <n>       Ring buffer capacity (default: 100)
  --debug                Print all gate evaluations to stdout (including silenced)
```

### 2.3 输出格式

Whisper 的输出极简，不做 TUI，不弹窗。通知以 ANSI 高亮块输出到 stderr：

```
┌─ tachi whisper ─────────────────────────────────────────── 14:32 ─┐
│ 👀 exp-042 结果出来了：val_acc=0.79（比上次的 0.78 好），但        │
│ loss 曲线还在下降——100 epoch 还没收敛。建议跑 200 epoch。          │
└──────────────────────────────────────────────────────────────────┘
```

同时触发 macOS 系统通知（`terminal-notifier`，失败时静默退化）。

### 2.4 Config 持久化

大部分参数可以从 config.yaml 读取默认值，命令行 flag 覆盖：

```yaml
# ~/.tachi/config.yaml
whisper:
  enabled: true                # 是否允许 whisper 命令运行
  provider: ""                 # 空 = 用主 provider
  model: ""                    # 空 = 自动选该 provider 下最便宜的模型
  batch_window_ms: 30000       # 事件批处理窗口
  cooldown_sec: 300            # 通知冷却时间
  gate_threshold: 7            # 1-10
  max_events_batch: 100        # ring buffer 容量
  watch_paths: ["."]           # 默认监听路径
  exclude_patterns:            # 默认排除
    - ".git/**"
    - "node_modules/**"
    - "dist/**"
    - "build/**"
    - "*.pyc"
    - "__pycache__/**"
  system_notify: true          # 是否发系统通知
```

---

## 三、核心架构

### 3.1 数据流

```
                              ┌──────────────────────────────┐
                              │        EVENT SOURCES          │
                              │    (plugin, 并行运行)          │
                              │                               │
                              │  ┌────────────┐ (MVP)         │
                              │  │ FileSource  │              │
                              │  │ fsnotify    │              │
                              │  │ CREATE/WRITE│              │
                              │  │ RENAME/DEL  │              │
                              │  └─────┬──────┘              │
                              │        │                      │
                              │  ┌─────┴──────┐ (future)      │
                              │  │ ProcSource  │              │
                              │  │ process     │              │
                              │  │ start/exit  │              │
                              │  │ cpu/mem/io  │              │
                              │  └─────┬──────┘              │
                              │        │                      │
                              │  ┌─────┴──────┐ (future)      │
                              │  │ LogSource   │              │
                              │  │ error spike │              │
                              │  │ pattern     │              │
                              │  │ anomaly     │              │
                              │  └─────┬──────┘              │
                              │        │                      │
                              │   所有 source 写入同一个 chan   │
                              └────────┬─────────────────────┘
                                       │ RawEvent{ Source: "file"|"proc"|"log", ... }
                                       ▼
                              ┌──────────────────────────────┐
                              │        RING BUFFER            │
                              │  容量: 100 (所有 source 共享)  │
                              │  Flush: 30s 无新事件后打包     │
                              │  去重: (source, key) 2s 合并   │
                              └────────────┬─────────────────┘
                                       │ Batch (mixed sources)
                                       ▼
                              ┌──────────────────────────────┐
                              │     CONTEXT ENRICHER          │
                              │  按 source 类型分发:            │
                              │  file → ReadFile + ls          │
                              │  proc → (future) process tree  │
                              │  log  → (future) log snippet   │
                              │  跨 source 公共: memory recall  │
                              └────────────┬─────────────────┘
                                       │ EnrichedBatch
                                       ▼
                              ┌──────────────────────────────┐
                              │       SIGNAL GATE (LLM)       │
                              │  source-aware prompt          │
                              │  { score, reasoning, whisper } │
                              │  score ≥ threshold → notify   │
                              │  异步，不阻塞 event loop       │
                              └────────────┬─────────────────┘
                                       │
                                       ▼
                              ┌──────────────────────────────┐
                              │     NOTIFICATION              │
                              │  → stderr ANSI 块             │
                              │  → macOS 系统通知              │
                              │  → ~/.tachi/logs/whisper.log   │
                              │  cooldown 防轰炸               │
                              └──────────────────────────────┘
```

**核心设计**：所有 EventSource 共享同一个 channel → 同一个 Ring Buffer → 同一个 Signal Gate。Gate 的 prompt 是 source-aware 的——知道文件变更、进程事件、日志异常各自意味着什么，并且能在多 source 事件间做**跨源关联推理**（例如："日志报 connection refused + 同时发现 config.yaml 里端口从 8080 改成了 8081 → 配置改了但服务没重启"）。

### 3.2 包结构

```
agent/whisper/
  watcher.go          — Watcher: 生命周期、goroutine 编排、所有 source 的 Start/Stop
  source.go           — EventSource 接口定义
  source_file.go      — FileSource: fsnotify wrapper（MVP 唯一实现）
  source_proc.go      — (future) ProcSource: 进程生命周期 + 资源监控
  source_log.go       — (future) LogSource: 日志 tail + 模式匹配
  ring.go             — RingBuffer: 跨 source 共享，去重、批处理、flush
  enricher.go         — ContextEnricher: 按 source 类型分发的并行上下文收集
  gate.go             — SignalGate: source-aware LLM 调用 + prompt 模板 + JSON 解析
  whisper.go          — 类型定义（RawEvent, EnrichedBatch, GateDecision, Notification）
  watcher_test.go     — 单测（mock EventSource、mock LLM provider）

main.go               — whisperCmd 命令注册 + 参数解析 + Watcher 启动
```

### 3.3 与已有代码的集成

| 集成点 | 方式 |
|------|------|
| `main.go` | 新增 `whisperCmd`，注册到 CLI app |
| `config.Config` | 新增 `Whisper WhisperConfig` 字段 |
| `llm.Provider` | 复用现有 provider 接口；gate 调用 `Chat()`（非流式，需要 JSON 输出） |
| `agent/memory` | 复用 memory backend 做 recall |
| `pkg/debuglog` | 复用 logger 写 `whisper.log` |

**不与 TUI 集成**。Whisper 是完全独立的进程，和 TUI 之间没有 channel、没有共享状态。唯一的连接点是 memory（读写同一个 mem9 backend 或共享 `~/.tachi/` 下的文件索引）。

---

## 四、数据模型

### 4.1 EventSource 接口

```go
// agent/whisper/source.go

// EventSource produces RawEvents. Multiple sources run concurrently,
// all writing to the same channel. Each source owns its lifecycle —
// Start() blocks until ctx is cancelled or an unrecoverable error.
type EventSource interface {
    Name() string                                 // "file", "proc", "log", ...
    Start(ctx context.Context, ch chan<- RawEvent) error
}
```

### 4.2 RawEvent

```go
// agent/whisper/whisper.go

type RawEvent struct {
    Source    string    `json:"source"`    // "file", "proc", "log", ...
    Timestamp time.Time `json:"timestamp"`
    Key       string    `json:"key"`       // 用于去重: file→path, proc→pid, log→path:offset
    Operation string    `json:"operation"` // source-specific: file→"CREATE", proc→"START", log→"ERROR"
    Payload   string    `json:"payload"`   // 人类可读摘要: file→"config.yaml", proc→"nginx (pid=42) exited with code 137", log→"connection refused on :8080 (3x in 5s)"
}
```

`Payload` 是 source 产出的唯一文本表示——下游（Ring Buffer、Enricher、Signal Gate）都通过它理解事件含义，不感知 source 内部细节。这保证了多 source 的可扩展性：新增 source 只需满足接口，不必修改 pipeline。

### 4.3 EnrichedBatch

```go
type BatchContext struct {
    Events       []RawEvent           `json:"events"`        // mixed sources
    FilePreviews map[string]string    `json:"file_previews"` // file source: path → 前 100 行
    DirLayouts   map[string]string    `json:"dir_layouts"`   // file source: 受影响目录 → ls
    ProcSnapshots []ProcSnapshot      `json:"proc_snapshots"` // (future) proc source: 进程快照
    LogSnippets  []LogSnippet         `json:"log_snippets"`   // (future) log source: 匹配行前后文
    MemoryHits   []MemoryHit          `json:"memory_hits"`    // 跨 source 公共
}

type MemoryHit struct {
    Score   float64 `json:"score"`
    Content string  `json:"content"`
}
```

### 4.4 GateDecision

```go
type GateDecision struct {
    Score     int    `json:"score"`     // 1-10, >= threshold 才通知
    Reasoning string `json:"reasoning"` // 为何打这个分（debug log 用）
    Whisper   string `json:"whisper"`   // ≤2 句话，用户语言，以 👀 开头
}
```

### 4.5 Notification

```go
type Notification struct {
    Timestamp time.Time  `json:"timestamp"`
    Whisper   string     `json:"whisper"`
    Events    []RawEvent `json:"events"`     // 触发该通知的原始事件
    Score     int        `json:"score"`
    Reasoning string     `json:"reasoning"`
}
```

---

## 五、Signal Gate Prompt 设计

这是整个系统的灵魂——一个 source-aware 的 prompt，让 LLM 成为可靠的信号过滤器。

```
You are a whisper gate. Your only job: observe events from multiple 
sources in a working directory and decide what's worth whispering to 
the user about.

The user is working in: {{.Directory}}
Current time: {{.Time}}

In the last {{.BatchWindowSeconds}} seconds, these events occurred:
{{range .Events}}
  [{{.Source}}] {{.Payload}}
{{end}}

---
Context gathered from each source:

{{if .FilePreviews}}
AFFECTED FILES:
{{range $path, $preview := .FilePreviews}}
  --- {{$path}} ---
  {{$preview}}
{{end}}
{{end}}

{{if .DirLayouts}}
AFFECTED DIRECTORIES:
{{range $path, $layout := .DirLayouts}}
  --- {{$path}} ---
  {{$layout}}
{{end}}
{{end}}

{{if .MemoryHits}}
RELATED MEMORIES (from past sessions):
{{range .MemoryHits}}
  - {{.Content}}
{{end}}
{{end}}

---
Evaluate:

1. WHAT HAPPENED? One-sentence summary. If events span multiple sources,
   look for correlations between them (e.g. "log error appears right 
   after a config file was changed").
2. DOES IT MATTER? What problem, insight, or opportunity does this reveal?
3. WOULD THEY CARE? Be brutally honest: grateful or annoyed?
4. SCORE: Rate 1-10 on relevance.

SCORING GUIDE:
  1-2: Routine noise across any source (autosave, log heartbeat, 
       normal process cycle)
  3-4: Mildly notable but not actionable (dep bump, formatting change, 
       a process that rarely exits did exit but nothing else looks wrong)
  5-6: Interesting but can wait (new file, config tweak, moderate 
       resource usage spike)
  7-8: Useful insight (experiment results, data contradiction, repeated 
       failure pattern, opportunity they'd miss)
  9-10: Critical (security issue, data corruption, crash loop, resource 
       exhaustion)

SOURCE-SPECIFIC SIGNALS TO WATCH FOR:
  File events:
    - Repeated rewrites in a short window → user may be stuck
    - New files that contradict or duplicate existing work
    - Content patterns: hardcoded secrets, SQL injection, 
      data with obvious quality issues
  Proc events: (future)
    - Unexpected start/exit → crash or manual intervention
    - Resource spikes (cpu/mem/io) correlated with file changes
  Log events: (future)
    - Error rate spikes → regression or misconfiguration
    - Pattern matches (panic, OOM, connection refused) → urgent

CRITICAL RULES:
- Default to silence. You are a WHISPER — not a notification center.
- Fail open: if unsure, score low. Better to miss one than to cry wolf.
- Score ≥7 ONLY when you have a concrete, actionable reason.
- Cross-source correlation is a strong signal (e.g. config changed +
  + log errors appearing = "they forgot to restart").
- The whisper must be ≤2 sentences, warm, useful. Start with 👀.

Return ONLY valid JSON:
{
  "score": <1-10>,
  "reasoning": "<one sentence>",
  "whisper": "<if score >= threshold: ≤2 sentences. Otherwise: empty string>"
}
```

### 5.1 通过/不通过示例

| 事件 | 上下文 | 评分 | Whisper |
|------|--------|------|---------|
| `results/exp-042.csv` CREATE | memory 中有 exp-038~041 的实验记录；文件内容是训练日志，val_acc=0.79，loss 还在降 | 8 | 👀 exp-042 跑完了：val_acc=0.79，比上次的 0.78 好，但 loss 曲线还在降——100 epoch 没收敛。试试 200 epoch？ |
| `thesis/chapter2.md` WRITE × 6 + `bibliography.bib` WRITE | memory 中记过"清朝盐政论文"主题；bibliography 引用了《清盐法志》光绪版 | 7 | 👀 chapter2 反复改了 6 次，你引用的《清盐法志》是光绪刻本。国图最近公开了宣统续修本，有两淮盐政专章，可能和你的第三节有关。 |
| `budget-2026.md` WRITE | 项目中没有其他 .csv/.numbers 文件；但 `~/Downloads/` 下出现了 5 个招行账单 PDF | 7 | 👀 你在改预算表，同时下载了 5 个月的招行账单。要我帮你把 PDF 里的收支分类汇总，和预算对比一下吗？ |
| `server.go` WRITE | 文件内容显示 `query := "SELECT * FROM users WHERE id = " + userInput` 字符串拼接 | 10 | 👀 server.go 里直接拼接了用户输入到 SQL 查询——这是 SQL 注入漏洞。需要参数化查询。 |
| `app.css` WRITE | 正常的前端样式修改 | 1 | （静默） |
| `package-lock.json` WRITE | npm install 正常更新 | 2 | （静默） |
| `test_auth.py` WRITE × 5 (2分钟内) | 文件内容显示同一个测试函数反复修改 assert 语句；memory 中有 3 天前"auth 模块调试困难"的记录 | 7 | 👀 你 2 分钟内改了 test_auth.py 的断言 5 次了。上次在 auth 模块也卡了很久——要不要我看看测试逻辑本身有没有问题？ |
| `data/air-quality.csv` CREATE (2.4GB) | 文件前 100 行显示 PM2.5 列 12% 缺失值、坐标用了 BD-09 而非 WGS-84 | 8 | 👀 刚创建的空气质量数据有 2300 万行，PM2.5 列 12% 缺失值，坐标是 BD-09 系（不是标准 WGS-84）。需要先清洗才能用。 |
| `tmp/debug.log` WRITE | `tmp/` 在 exclude 列表中 | — | （未进入 pipeline） |

**关键洞察**：同一个事件（"npm 更新了 50 个包"）在规则引擎中永远无法区分重要性，但 LLM Gate 可以基于上下文（文件内容、memory、关联的文件变化模式）做出精准判断。

---

## 六、实现路径

### Phase 1: 骨架（多 source 管道）

- [ ] `agent/whisper/` 包创建
- [ ] `EventSource` 接口 + `RawEvent` / `BatchContext` / `GateDecision` / `Notification` 类型定义
- [ ] `FileSource`（fsnotify wrapper）：recursive watch + exclude patterns + 去重 + Payload 生成
- [ ] `RingBuffer`：跨 source 共享，容量限制 + 定时 flush + `(source, key)` 去重 + 溢出丢弃
- [ ] `Watcher`：source 注册表 + 并行 Start/Stop + event channel 消费
- [ ] `config.WhisperConfig` + 默认值
- [ ] `main.go` 中注册 `whisperCmd`（参数解析 + Watcher 启动）
- [ ] 单测（mock EventSource、mock ring buffer flush、验证 source 注册/Stop）

### Phase 2: 信号判断（source-aware LLM Gate）

- [ ] `ContextEnricher` 实现：
  - 按 source 分发：file events → ReadFile 前 100 行 + ls 目录结构
  - （stub）proc/log enrichers（空实现，占位）
  - 跨 source 公共：memory recall
- [ ] `SignalGate`：Go text/template 渲染 source-aware prompt + LLM Chat() + JSON 解析
- [ ] Gate provider 解析（`whisper.provider` → `whisper.model`，fallback 到主 provider 的最便宜模型）
- [ ] Cooldown 机制
- [ ] 单测（mock LLM provider，验证 prompt 模板、JSON 解析、cooldown）

### Phase 3: 输出与调优

- [ ] stderr ANSI 格式化输出
- [ ] macOS 系统通知集成（`terminal-notifier`，可选依赖）
- [ ] `--debug` 模式（打印所有 gate 评估）
- [ ] 日志写入 `~/.tachi/logs/whisper.log`
- [ ] 端到端手动验证 + prompt 迭代优化

---

## 七、端到端走查

### 场景：论文写作

```
$ cd ~/projects/qing-salt-monopoly
$ tachi whisper --model claude-haiku-4-20250514

[14:00:01] whisper: listening on ~/projects/qing-salt-monopoly/
            (threshold=7, cooldown=300s, batch=30s)

用户在编辑 thesis/chapter2.md，连续保存了 6 次...

[14:15:32] batch: 6 events over 30s
            events:
              thesis/chapter2.md WRITE × 6
              bibliography.bib WRITE

            enricher:
              file_preview: chapter2.md (1-100 lines, mentions "两淮盐政" × 4, 
                            引用 "《清盐法志》光绪三十四年刻本")
              file_preview: bibliography.bib (contains @book{qingsalt,
                            year={1908}, title={清盐法志}...)
              memory_hits: "用户在做清朝盐政相关研究，主要关注两淮地区"

            gate call → {score: 7, reasoning: "用户反复修改盐政章节，引用的
                        文献是1908年光绪刻本。memory 显示ta关注两淮地区。
                        宣统续修本新增了两淮专章，ta可能不知道。"}

┌─ tachi whisper ────────────────────────────────────── 14:15:33 ─┐
│ 👀 chapter2 反复改了 6 次，你引用的《清盐法志》是光绪刻本。      │
│ 国图最近公开了宣统续修本，有两淮盐政专章——可能和第三节有关。      │
└─────────────────────────────────────────────────────────────────┘

            同时触发 macOS 系统通知。
            cooldown 开始：下次通知不早于 14:20:33。

用户在终端看到这条 whisper，去搜索了宣统续修本，发现确实有相关材料。
但ta没有通过 TUI 跟 agent 对话——ta只是在写论文，而 whisper 帮ta注意到
了一个ta自己可能永远发现不了的文献更新。
```