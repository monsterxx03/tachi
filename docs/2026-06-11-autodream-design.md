# AutoDream — 会话记忆整合系统

> 版本: 2.0 | 日期: 2026-06-12 | 状态: 设计阶段
> 关联: [可插拔 Memory Backend](./2026-05-17-memory.md),
>       [自动压缩设计](./2026-05-30-auto-compact-design.md),
>       [Native Memory v1](./2026-05-16-native-memory.md),
>       [会话存储设计](./2026-05-10-session-replace-transcript.md)

---

## 一、动机

### 1.1 现有记忆机制的缺口

Tachi 目前已具备三层记忆写入（`turn` / `compact` / `session`），覆盖了对话过程中的及时存储。但存在一个结构性盲区：

| 现有机制 | 时机 | 粒度 | 问题 |
|---------|------|------|------|
| `StoreScopeTurn` | 每次 agent 回复后 | 当前轮次上下文 | 碎片化，单轮信息不足以形成抽象结论 |
| `StoreScopeCompact` | `/compact` 时 | 整个 session 的压缩摘要 | 丢失了操作细节和中间结论 |
| `StoreScopeSession` | 会话结束时 | session 摘要 | 需要用户主动 `/new` 或正常退出才会触发 |

这些机制共同的问题是：

1. **跨 session 的关联从未被建立**——你周一讨论的 API 设计决策和周三遇到的 bug 可能相关，但没有任何机制去发现这种关系
2. **概念随时间迭代**——你最初对某个模块的设计思路可能后来被推翻了，但旧记忆停留在索引里，没有被标记为"已过时"
3. **记忆没有被"消化"**——现有机制只做存储（Store），不做整合（Consolidate）。存储的是原始对话的摘录，而非经过推理后的知识结构

### 1.2 Claude Code autoDream 的启示

Anthropic 在 2026 年 3 月的 Claude Code 源码泄漏中暴露了一个名为 `autoDream` 的内部系统：

> *"The memory consolidation of the most advanced agent in the world is a forked subagent running grep on text logs."* — Fabio Akita, AkitaOnRails

关键特征：
- **后台 fork 子 agent**，不与主 agent 共享上下文
- **用 grep 搜索文本日志**，不做向量 embedding
- **输出为 topic files**（Markdown 文件），不是向量数据库
- **记忆被当作 hint 而非 truth**——系统假设存储的内容可能过期，需要验证后才信任
- **四条阶段**：Orient（定向）→ Gather（收集）→ Consolidate（整合）→ Prune（裁剪）
- **三道闸门**：距上次 dream ≥ 24h、至少 5 个新 session、互斥锁防并发

### 1.3 设计目标

| 目标 | 说明 |
|------|------|
| 跨 session 关联 | 发现不同对话之间的事实联系和矛盾 |
| 知识提炼 | 从原始对话中提取可复用的结论（设计决策、用户偏好、项目上下文） |
| 记忆保鲜 | 标记/删除过期或已被推翻的记忆 |
| 零新外部依赖 | 复用 Tachi 已有的 SubAgent + Cron + Grep |
| 被动触发 | 不占用主 agent 的上下文 window 和 token 预算 |

---

## 二、架构概览

```
┌─────────────────────────────────────────────────────────┐
│              tachi channel (长驻进程)                      │
│                                                         │
│  ┌──────────────┐            ┌───────────────────────┐  │
│  │ 用户 Cron     │            │ SystemScheduler        │  │
│  │ (crons.json)  │            │ (config.yaml only)     │  │
│  │ CronTool CRUD │            │ 用户/LLM 不可见         │  │
│  └──────┬───────┘            └──────────┬────────────┘  │
│         │                               │               │
│         ▼                               ▼               │
│  OnCronTrigger()             executeDream()              │
│  (channel 回复)               (Dream Orchestrator)       │
│                                         │               │
│                              ┌──────────┴──────────┐    │
│                              ▼                     ▼    │
│                       Dream SubAgent        Dream SubAgent
│                       (项目 A)              (全局)        │
│                              │                     │    │
│                              ▼                     ▼    │
│                       WriteFile →           WriteFile → │
│                       .tachi/memory/        ~/.tachi/memory/
└─────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────┐
│          memory.type: topic (任何模式生效)                 │
│                                                         │
│  TopicBackend (实现 memory.Backend 接口)                  │
│    ├── Recall() → rg grep topic files                   │
│    └── Store()  → no-op (DirectContent → inbox.md)      │
│                                                         │
│  MemoryRecallReminder → TopicBackend.Recall()           │
│  MemoryRecall tool    → TopicBackend.Recall()           │
└─────────────────────────────────────────────────────────┘
```

### 2.1 TopicBackend 在 Memory Backend 体系中的位置

TopicBackend 是与 mem9、agentmemory **同级互斥**的第三种 memory backend 实现：

```yaml
memory:
  type: topic          # 三选一：topic / mem9 / agentmemory
```

| | **TopicBackend** | **Mem9** | **AgentMemory** |
|--|-----------------|----------|-----------------|
| 依赖 | 无（纯本地） | 云端 API | 本地 server |
| 写入方式 | Dream SubAgent 离线提炼 | 每轮实时写入 | 每轮实时写入 |
| 搜索方式 | ripgrep 关键词 | 向量语义搜索 | BM25 + 向量 + KG |
| 记忆延迟 | ≤24h（等待 dream） | 实时 | 实时 |
| 适合场景 | 不想依赖外部服务、接受滞后 | 需要实时语义搜索 | 本地全功能 |

**核心 tradeoff**：TopicBackend 的记忆有 ≤24h 滞后——今天下午的对话需要等到下一次 dream 跑完后才能被 recall。需要实时记忆的用户应选择 mem9 或 agentmemory。

### 2.2 项目记忆 vs 全局记忆

| | **项目记忆** | **全局记忆** |
|--|------------|------------|
| 路径 | `<git-root>/.tachi/memory/` | `~/.tachi/memory/` |
| 存储内容 | 该项目的设计决策、bug 模式、架构讨论 | 用户偏好、工作流习惯、跨项目知识 |
| 有效期 | 随项目存在 | 永久（可跨项目复用） |
| 谁看到 | 只有在该项目下启动的 session | 所有 session |
| 隔离方式 | 按 git root 天然隔离 | 全局共享 |

TopicBackend 在 Recall 时**同时搜索两个域**（如果有当前项目的话），返回合并结果。

### 2.3 核心设计原则

**原则一：记忆是 hint，不是 truth**

Dream 产出的 topic files 通过 `TopicBackend.Recall()` 被搜索到后，agent 应对其中的信息保持怀疑——"我记得上次讨论过这个问题，但让我验证一下"。

**原则二：Dream 是异步的，不阻塞主 loop**

AutoDream 完全在后台通过 SystemScheduler 触发（仅 channel 模式）。当前会话永远不会被 dream 打断。

**原则三：用 Grep，不用 embedding**

与 Claude Code 一致，dream 的 Gather 阶段只用关键词/正则搜索 session 文本：
- 确定性——搜到就是搜到，搜不到就是搜不到
- 零维护——不需要 embedding 模型、向量数据库、重索引管线
- 跨 session 的文本搜索已经足够发现大多数事实关联

---

## 三、数据模型

### 3.1 双域目录结构

```
# 全局记忆（所有 session 可访问）
~/.tachi/memory/
├── topics/
│   ├── preferences.md        ← 用户偏好、工作流习惯
│   ├── general-knowledge.md  ← 跨项目知识
│   └── workflows.md          ← 常用工作流
├── inbox.md                   ← MemoryRecord 直接写入（不等 Dream）
├── index.md                   ← 全局索引（≤ 200 行）
├── last_dream.json            ← 全局 dream 状态
└── dream.lock                 ← 全局互斥锁

# 项目记忆（仅该项目下 session 可访问）
<git-root>/.tachi/memory/
├── topics/
│   ├── design-decisions.md    ← 架构决策、技术选型
│   ├── bug-patterns.md        ← 反复出现的 bug 和解决方案
│   ├── project-ctx.md         ← 项目背景、模块说明
│   └── conventions.md         ← 代码约定/风格
├── inbox.md                   ← MemoryRecord 直接写入
├── index.md                   ← 项目索引（≤ 200 行）
├── last_dream.json            ← 项目 dream 状态
└── dream.lock                 ← 项目互斥锁
```

> **为什么 `.tachi/memory/` 放在项目根目录里？** 这是 Tachi 的惯例——`project_root/.tachi/` 已有 `.tachi.md`、`skills/` 等，memory 是自然扩展。且 `.tachi/` 通常已在 `.gitignore` 中。

### 3.2 `topics/*.md` — 主题文件

每个文件聚焦一个主题领域：

```markdown
# Design Decisions

## 2026-06-10: 选择了 Go 1.26 的 `iter` 包做流式处理

来源: session 2026-06-10-223045-a1b2c3d4
状态: active
关键词: iter, stream, GC, channel

理由：相比 channel-based 方案，`iter` 包在 GC 压力上减少了约 30%，
且与标准库风格一致。

关联文件：`agent/stream.go`

---

## 2026-06-08: 数据库选型从 PostgreSQL 改为 SQLite

来源: session 2026-06-08-140022-e5f6g7h8
状态: superseded  ← 已被后续决策推翻
关键词: database, sqlite, postgresql

理由：当时认为 PG 的扩展性更好...
```

**字段约定**：

| 字段 | 说明 |
|------|------|
| `来源: session <id>` | 事实来源的原始会话 ID，可追溯 |
| `状态: active / superseded / declined` | 事实当前是否仍然有效 |
| `关键词: ...` | Dream 生成的搜索关键词，优化 grep 命中率 |
| `---` | 分隔不同事实块 |

**为什么用 Markdown 而非结构化格式**：因为 dream subagent 本身就是 LLM，读写 Markdown 天然的高质量。结构化格式需要额外的 parse/serialize 步骤，且对 LLM 不如 Markdown 友好。

### 3.3 `inbox.md` — 即时记忆

用户通过 `MemoryRecord` 工具显式存入的内容，不等 Dream，直接追加到 inbox：

```markdown
## 2026-06-11T14:30:00+08:00

用户偏好：代码注释用英文，commit message 用中文。

---
```

Dream 在 Consolidate 阶段会读取 inbox，将其中的内容分类整合到对应的 topic file 中，然后清空 inbox。

### 3.4 `index.md` — 索引文件

纯指针文件，每行约 150 字符，保持在 200 行以内：

```markdown
# Memory Index

- [Preferences](./topics/preferences.md): CLI keybindings, Go style, logging (8 facts)
- [Design Decisions](./topics/design-decisions.md): stream iter, SQLite, config (19 facts, 2 superseded)
- [Bug Patterns](./topics/bug-patterns.md): subagent worktree leak, MCP tool race (6 patterns)
```

当达到 200 行时，下次 dream 的 Prune 阶段会压缩合并。

### 3.5 `last_dream.json` — 状态文件

```json
{
  "last_dream_at": "2026-06-11T03:00:00+08:00",
  "last_session_id": "2026-06-10-223045-a1b2c3d4",
  "sessions_dreamed": 7,
  "topics_created": 1,
  "facts_added": 12,
  "facts_superseded": 3,
  "facts_pruned": 2,
  "errors": []
}
```

### 3.6 `dream.lock` — 互斥锁

Dream 启动时创建，结束时删除。格式：`<PID>:<timestamp>`。

---

## 四、TopicBackend 实现

### 4.1 接口实现

```go
// agent/memory/topic_backend.go
package memory

type TopicBackend struct {
    globalDir  string // ~/.tachi/memory/
    projectDir string // <git-root>/.tachi/memory/（可能为空）
}

func NewTopicBackend(globalDir, projectDir string) *TopicBackend {
    return &TopicBackend{globalDir: globalDir, projectDir: projectDir}
}

func (t *TopicBackend) Recall(ctx context.Context, query string, limit int) ([]Entry, error) {
    var allResults []Entry

    // 1. 始终搜全局记忆
    results, err := t.searchDir(filepath.Join(t.globalDir, "topics"), query)
    if err != nil {
        debuglog.Log("TopicBackend: global search failed: %v", err)
    }
    allResults = append(allResults, results...)

    // 2. 搜 inbox（全局）
    inboxResults, _ := t.searchFile(filepath.Join(t.globalDir, "inbox.md"), query)
    allResults = append(allResults, inboxResults...)

    // 3. 如果有项目目录，也搜项目记忆
    if t.projectDir != "" {
        results, err = t.searchDir(filepath.Join(t.projectDir, "topics"), query)
        if err != nil {
            debuglog.Log("TopicBackend: project search failed: %v", err)
        }
        allResults = append(allResults, results...)

        inboxResults, _ = t.searchFile(filepath.Join(t.projectDir, "inbox.md"), query)
        allResults = append(allResults, inboxResults...)
    }

    // 4. 按 score 排序，截断到 limit
    sort.Slice(allResults, func(i, j int) bool {
        return allResults[i].Score > allResults[j].Score
    })
    if len(allResults) > limit {
        allResults = allResults[:limit]
    }

    return allResults, nil
}

func (t *TopicBackend) Store(ctx context.Context, opts StoreOptions) error {
    if opts.DirectContent == "" {
        return nil // no-op：非显式写入一律等 Dream 处理
    }

    // MemoryRecord 显式写入 → 追加到 inbox.md（立即可 recall）
    domain := t.resolveWriteDomain(opts)
    inboxPath := filepath.Join(domain, "inbox.md")
    entry := fmt.Sprintf("\n## %s\n\n%s\n\n---\n",
        time.Now().Format(time.RFC3339), opts.DirectContent)
    return appendToFile(inboxPath, entry)
}

func (t *TopicBackend) Forget(ctx context.Context, id string) error {
    // TODO: 按 session ID 搜索并删除对应事实块
    return nil
}

func (t *TopicBackend) Observe(ctx context.Context, opts ObserveOptions) error {
    return nil // topic backend 不需要 observe
}
```

### 4.2 搜索实现

```go
func (t *TopicBackend) searchDir(dir, query string) ([]Entry, error) {
    if _, err := os.Stat(dir); os.IsNotExist(err) {
        return nil, nil
    }

    // 先找匹配文件
    cmd := exec.CommandContext(ctx, "rg", "-l", "-i", query, dir)
    out, err := cmd.Output()
    if err != nil {
        if isRgNoMatch(err) {
            return nil, nil
        }
        return nil, fmt.Errorf("rg search failed: %w", err)
    }

    files := strings.Split(strings.TrimSpace(string(out)), "\n")
    var results []Entry

    for _, f := range files {
        if f == "" {
            continue
        }
        // 提取匹配的事实块（用 --- 分隔）
        entries := t.extractMatchingBlocks(f, query)
        results = append(results, entries...)
    }

    return results, nil
}

// extractMatchingBlocks 读取 topic file，按 --- 分割为事实块，
// 返回包含 query 关键词的块作为 Entry。
func (t *TopicBackend) extractMatchingBlocks(filepath, query string) []Entry {
    content, err := os.ReadFile(filepath)
    if err != nil {
        return nil
    }

    blocks := splitByHR(string(content)) // 按 "\n---\n" 分割
    var entries []Entry

    for _, block := range blocks {
        if !containsIgnoreCase(block, query) {
            continue
        }
        entry := Entry{
            Summary:   extractTitle(block),    // 取 ## 行
            Content:   truncate(block, 1000),  // 限制单条长度
            Timestamp: extractTimestamp(block), // 从"来源"行解析
            Score:     computeScore(block, query),
        }
        entries = append(entries, entry)
    }

    return entries
}

// computeScore 基于匹配质量计算分数
func computeScore(block, query string) float64 {
    base := 0.6
    // 关键词行精确匹配加分
    if matchesKeywordLine(block, query) {
        base += 0.2
    }
    // 标题匹配加分
    if containsIgnoreCase(extractTitle(block), query) {
        base += 0.15
    }
    // superseded 降分
    if strings.Contains(block, "状态: superseded") {
        base -= 0.3
    }
    return base
}
```

### 4.3 域解析

```go
// FindGitRoot 返回给定目录所在的 git 仓库根目录。
// 不在 git 仓库中时返回空字符串。
// 纯 Go 实现，不 fork 进程。
func FindGitRoot(dir string) string {
    if dir == "" {
        return ""
    }
    dir, err := filepath.Abs(dir)
    if err != nil {
        return ""
    }
    for {
        if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
            return dir
        }
        parent := filepath.Dir(dir)
        if parent == dir {
            break
        }
        dir = parent
    }
    return ""
}

// resolveWriteDomain 确定写入目标域
func (t *TopicBackend) resolveWriteDomain(opts StoreOptions) string {
    if t.projectDir != "" {
        return t.projectDir // 有项目上下文时写项目域
    }
    return t.globalDir
}
```

---

## 五、触发机制

### 5.1 SystemScheduler

AutoDream 通过 **SystemScheduler** 触发——一个与用户 Cron 完全隔离的系统级调度器，仅在 `tachi channel` 模式中运行：

```go
// cron/system_scheduler.go
package cron

type SystemScheduler struct {
    engine *cron.Cron
    logger *debuglog.Logger
}

type SystemSchedulerConfig struct {
    Logger *debuglog.Logger
}

func NewSystemScheduler(cfg SystemSchedulerConfig) *SystemScheduler {
    return &SystemScheduler{
        engine: cron.New(cron.WithParser(
            cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
        )),
        logger: cfg.Logger,
    }
}

func (s *SystemScheduler) Register(name, schedule string, fn func(ctx context.Context) error) error {
    _, err := s.engine.AddFunc(schedule, func() {
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
        defer cancel()
        if err := fn(ctx); err != nil {
            s.logger.Log("system cron [%s] failed: %v", name, err)
        } else {
            s.logger.Log("system cron [%s] completed", name)
        }
    })
    return err
}

func (s *SystemScheduler) Start() { s.engine.Start() }
func (s *SystemScheduler) Stop()  { s.engine.Stop() }
```

**与用户 Cron 的差异**：

| | 用户 Cron | SystemScheduler |
|--|----------|-----------------|
| 存储 | `crons.json` | 无持久化（从 config.yaml 加载） |
| 可见性 | `/cron list` 可见，CronTool 可操作 | 对 LLM/用户完全不可见 |
| CRUD | CronTool 可增删改查 | 仅 config.yaml 开关 + 调时间 |
| 绑定 | channel + thread | 无绑定，独立执行 |
| 触发 handler | channel Manager.OnCronTrigger | 独立 handler（直接调 SubAgent） |

### 5.2 Channel Manager 集成

```go
// channel/manager/manager.go
func (m *Manager) Start(ctx context.Context) error {
    // ... 现有 channel 启动逻辑 ...

    // 系统级调度（与用户 cron 完全隔离）
    if m.cfg.Dream.Enabled {
        m.systemScheduler = cron.NewSystemScheduler(cron.SystemSchedulerConfig{
            Logger: m.logger,
        })
        m.systemScheduler.Register("auto-dream", m.cfg.Dream.Schedule, m.executeDream)
        m.systemScheduler.Start()
    }

    return nil
}

func (m *Manager) Stop() {
    if m.systemScheduler != nil {
        m.systemScheduler.Stop()
    }
    // ... 现有停止逻辑 ...
}
```

### 5.3 三道闸门（Gate Keeper）

闸门按记忆域独立检查。Orchestrator 先按 WorkingDir 分组 session，然后对每个有足够新 session 的域触发独立 Dream SubAgent：

```go
func (m *Manager) executeDream(ctx context.Context) error {
    sessions, err := m.sessionManager.ListSessions()
    if err != nil {
        return fmt.Errorf("list sessions: %w", err)
    }

    // 过滤标记为 skip_dream 的 session
    sessions = filterSkippedSessions(sessions)

    // 按 WorkingDir 分组为记忆域
    groups := groupSessionsByDomain(sessions)

    // 对每个域独立检查闸门
    var plans []DreamPlan
    for _, g := range groups {
        lastState := loadLastDreamState(g.MemoryRoot)

        // 闸门 1: 距上次 dream ≥ MinInterval
        if time.Since(lastState.LastDreamAt) < m.cfg.Dream.MinInterval {
            continue
        }

        // 闸门 2: 该域至少 N 个新 session
        newSessions := countNewSessions(lastState.LastSessionID, g.Sessions)
        if newSessions < m.cfg.Dream.MinNewSessions {
            continue
        }

        // 闸门 3: 该域没有并发 dream 在运行
        if !acquireDreamLock(g.MemoryRoot) {
            continue
        }

        plans = append(plans, DreamPlan{Group: g, LastState: lastState})
    }

    // 并行执行（限制最多 3 个并发 dream）
    return m.executeDreamPlans(ctx, plans)
}
```

**闸门条件**：
- 1 天 1 次确保 topic files 不会频繁变动
- 5 个新 session 确保有足够的信息密度值得整合
- 每个域内互斥，跨域可并行（最多 3 个并发）

### 5.4 会话分组

```go
type SessionGroup struct {
    Domain     string   // "global" 或 "project"
    Root       string   // git root 或 ""
    MemoryRoot string   // memory 目录路径
    Sessions   []*session.Session
}

func groupSessionsByDomain(sessions []*session.Session) []SessionGroup {
    groups := make(map[string]*SessionGroup)

    for _, s := range sessions {
        workingDir := s.WorkingDir
        projRoot := FindGitRoot(workingDir)

        var key string
        var group *SessionGroup

        if projRoot != "" {
            key = "project:" + projRoot
            group = &SessionGroup{
                Domain:     "project",
                Root:       projRoot,
                MemoryRoot: filepath.Join(projRoot, ".tachi", "memory"),
            }
        } else {
            key = "global"
            group = &SessionGroup{
                Domain:     "global",
                Root:       "",
                MemoryRoot: filepath.Join(config.BaseDir(), "memory"),
            }
        }

        if _, ok := groups[key]; !ok {
            groups[key] = group
        }
        groups[key].Sessions = append(groups[key].Sessions, s)
    }

    var result []SessionGroup
    for _, g := range groups {
        result = append(result, *g)
    }
    return result
}
```

### 5.5 锁机制

使用 `O_EXCL` 原子创建锁文件，同时检查 PID 存活性：

```go
func acquireDreamLock(memoryDir string) bool {
    lockPath := filepath.Join(memoryDir, "dream.lock")

    // 尝试原子创建锁文件
    f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0644)
    if err == nil {
        defer f.Close()
        fmt.Fprintf(f, "%d:%s", os.Getpid(), time.Now().Format(time.RFC3339))
        return true
    }

    if !errors.Is(err, os.ErrExist) {
        return false
    }

    // 锁文件已存在 — 检查是否过期
    data, _ := os.ReadFile(lockPath)
    parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
    if len(parts) != 2 {
        os.Remove(lockPath)
        return acquireDreamLock(memoryDir) // 格式损坏，接管
    }

    pid, pidErr := strconv.Atoi(parts[0])
    timestamp, timeErr := time.Parse(time.RFC3339, parts[1])

    // PID 存活性检查
    if pidErr == nil {
        if proc, err := os.FindProcess(pid); err == nil {
            if proc.Signal(syscall.Signal(0)) == nil {
                return false // 进程还在运行，锁有效
            }
        }
    }

    // 时间戳超时检查（双重保障）
    if timeErr == nil && time.Since(timestamp) < 5*time.Minute {
        return false // 时间未过期，保守等待
    }

    // 锁过期，清理后重试
    os.Remove(lockPath)
    return acquireDreamLock(memoryDir)
}

func releaseDreamLock(memoryDir string) {
    os.Remove(filepath.Join(memoryDir, "dream.lock"))
}
```

---

## 六、Dream 执行流程

Dream 的核心执行体是一个 **SubAgent**（复用已有机制），限定工具为：

```
allowed_tools: [ReadFile, Grep, Glob, WriteFile]
```

- **无 Bash** → 不能通过绝对路径读取系统敏感文件
- **无 SubAgent** → 不能递归 fork
- **无 Cron** → 不能自注册 cron job
- **无 EditFile** → 不能修改项目代码

### 6.1 WriteFile 路径沙箱

Dream SubAgent 的 WriteFile 被限制为只能写入当前域的 memory 目录：

```go
// agent/tools/path_policy.go
type PathPolicy struct {
    AllowedWriteDirs []string // 允许写入的目录白名单
}

type pathPolicyKey struct{}

func WithPathPolicy(ctx context.Context, policy PathPolicy) context.Context {
    return context.WithValue(ctx, pathPolicyKey{}, policy)
}

func GetPathPolicy(ctx context.Context) *PathPolicy {
    if v, ok := ctx.Value(pathPolicyKey{}).(*PathPolicy); ok {
        return v
    }
    return nil // nil = 无限制（普通模式）
}

// 在 WriteFile.ExecuteContext 中校验
func (w *WriteTool) ExecuteContext(ctx context.Context, args json.RawMessage) (string, error) {
    // ... 解析参数 ...
    absPath, _ := filepath.Abs(resolvedPath)

    if policy := GetPathPolicy(ctx); policy != nil {
        allowed := false
        for _, dir := range policy.AllowedWriteDirs {
            if strings.HasPrefix(absPath, dir) {
                allowed = true
                break
            }
        }
        if !allowed {
            return "", fmt.Errorf("write denied: %s is outside allowed directories", absPath)
        }
    }

    // ... 正常写入 ...
}
```

Orchestrator 启动 SubAgent 时注入 PathPolicy：

```go
ctx = WithPathPolicy(ctx, PathPolicy{
    AllowedWriteDirs: []string{plan.Group.MemoryRoot},
})
```

### 6.2 消息预过滤

Dream SubAgent 不应读取完整的 `messages.jsonl`（含大量 tool_call/tool_result），否则 token 爆炸。Orchestrator 在准备 SubAgent 输入时预过滤：

```go
// 只保留 user + assistant 消息，跳过 tool_call / tool_result / thinking
func filterConversationMessages(msgs []session.Message) []session.Message {
    var filtered []session.Message
    for _, m := range msgs {
        if m.Role == "user" || m.Role == "assistant" {
            filtered = append(filtered, m)
        }
    }
    return filtered
}
```

预过滤后的消息写入临时文件供 SubAgent 读取，而非让 SubAgent 直接读 `messages.jsonl`。

### 6.3 Phase 1: Orient（定向）

**目标**：了解当前记忆域的状态，确定需要处理哪些新 session。

```
步骤：
1. 读 <memory-root>/last_dream.json → 知道上次处理到哪个 session
2. 读 <memory-root>/index.md → 知道现有 topic 结构
3. 读 <memory-root>/inbox.md → 知道有哪些待整合的即时记忆
4. 读每个新 session 的 meta.json → 获取标题和时间
```

**输出**：新 session 列表 + 现有 topic 结构 + inbox 内容。

### 6.4 Phase 2: Gather（收集）

**目标**：从新 session 中提取事实。

SubAgent 读取预过滤后的会话内容，使用 Grep 搜索关键模式：
- 决策相关：决定、选择、选了、改用、弃用
- 偏好相关：喜欢、不喜欢、偏好、习惯
- Bug 相关：bug、问题、修复、报错、workaround
- 显式记忆：记住、注意、重要、别忘了

Grep 模式不是硬编码——LLM 根据上下文自行决定搜什么。上面的模式只是 prompt 中的指导。

**输出**：候选事实列表，每个带来源 session ID、分类、摘要。

### 6.5 Phase 3: Consolidate（整合）

**目标**：将新事实 + inbox 内容写入 topic files，与已有事实对比合并。

```
规则：
- 若 topic file 中已有相同事实 → 跳过（去重）
- 若新事实与旧事实矛盾 → 保留新事实，旧事实标记为 superseded
- 若新事实是旧事实的补充 → 合并内容
- inbox.md 中的内容按主题分类写入对应 topic file
- 每个事实块生成 `关键词:` 行，优化后续 grep 命中率
```

写入完成后清空 inbox.md。

### 6.6 Phase 4: Prune（裁剪）

**目标**：保持 topic files 和 index.md 的精简。

```
步骤：
1. 删除标记为 superseded 超过 30 天的事实
2. 合并少于 3 个事实的"稀疏 topic"到 misc.md
3. 如果单个 topic file 超过 50 个事实，生成摘要替换旧内容
4. 更新 index.md（反映新增 topic，更新事实计数，保持 ≤200 行）
5. 写入 last_dream.json（更新状态）
6. 删除 dream.lock
```

### 6.7 Compact 交互

如果 session 有 `CompactedParentID`（压缩产物），Dream 同时读取原始 session 的消息来补充信息密度：

```go
func collectSessionMessages(sess *session.Session, mgr *session.Manager) []session.Message {
    msgs, _ := mgr.LoadMessages(sess.ID)
    filtered := filterConversationMessages(msgs)

    // 如果有压缩父 session，也读取父 session 内容
    if sess.CompactedParentID != "" {
        parentMsgs, _ := mgr.LoadMessages(sess.CompactedParentID)
        parentFiltered := filterConversationMessages(parentMsgs)
        filtered = append(parentFiltered, filtered...)
    }

    return filtered
}
```

`last_dream.json` 记录的仍是叶子 session ID（不标记父 session 为"已处理"）。

---

## 七、配置

### 7.1 Config 定义

```go
// config/config.go
type DreamConfig struct {
    Enabled         bool          `yaml:"enabled" default:"true"`
    Schedule        string        `yaml:"schedule" default:"0 3 * * *"`
    MinInterval     time.Duration `yaml:"min_interval" default:"24h"`
    MinNewSessions  int           `yaml:"min_new_sessions" default:"5"`
    SubagentTimeout time.Duration `yaml:"subagent_timeout" default:"10m"`
    SubagentMaxIter int           `yaml:"subagent_max_iters" default:"30"`
}
```

在顶层 Config 中新增：

```go
type Config struct {
    // ... 现有字段 ...
    Dream DreamConfig `yaml:"dream"`
}
```

### 7.2 用户配置示例

```yaml
# ~/.tachi/config.yaml

memory:
  type: topic   # 使用 TopicBackend

dream:
  enabled: true
  schedule: "0 3 * * *"     # 每天凌晨 3 点
  min_interval: 24h          # 最少间隔
  min_new_sessions: 5        # 至少 5 个新 session 才触发
```

> 注意：`dream.enabled: true` 只在 `tachi channel` 模式下生效。TUI 模式忽略此配置。

### 7.3 Session 扩展

```go
// session/session.go
type Session struct {
    // ... 现有字段 ...
    SkipDream bool `json:"skip_dream,omitempty"` // 该 session 不参与 Dream 整合
}
```

用户可在对话中说"这个 session 不要记入记忆"，由 agent 设置此字段。

---

## 八、安全性

### 8.1 SubAgent 沙箱

| 限制 | 手段 |
|------|------|
| 工具白名单 | `allowed_tools: [ReadFile, Grep, Glob, WriteFile]` |
| 写入路径 | PathPolicy 限制为当前域的 memory 目录 |
| 无递归 | 不给 SubAgent 工具 |
| 无执行 | 不给 Bash 工具 |
| 无代码修改 | 不给 EditFile 工具 |

### 8.2 锁超时

如果 dream 异常退出（SIGKILL、OOM、断电），锁文件不会被清理。下次 dream 检测到后接管：
1. PID 存活性检查：`proc.Signal(syscall.Signal(0))`
2. 时间戳超时：>5 分钟视为过期
3. 两者任一满足即可接管

### 8.3 并发上限

全局 + 所有项目最多 3 个并发 dream。每个域内通过 `dream.lock` 互斥。

```go
const maxConcurrentDreams = 3

func (m *Manager) executeDreamPlans(ctx context.Context, plans []DreamPlan) error {
    sem := make(chan struct{}, maxConcurrentDreams)
    var wg sync.WaitGroup

    for _, plan := range plans {
        sem <- struct{}{}
        wg.Add(1)
        go func(p DreamPlan) {
            defer wg.Done()
            defer func() { <-sem }()
            defer releaseDreamLock(p.Group.MemoryRoot)

            if err := m.runDreamSubAgent(ctx, p); err != nil {
                m.logger.Log("dream [%s:%s] failed: %v", p.Group.Domain, p.Group.Root, err)
            }
        }(plan)
    }

    wg.Wait()
    return nil
}
```

---

## 九、实现计划

### Phase 1：TopicBackend（P0）

| 任务 | 文件 | 预估行数 |
|------|------|---------|
| TopicBackend 结构 + Recall 实现 | `agent/memory/topic_backend.go` | ~120 |
| Store（inbox 追加）+ Forget | `agent/memory/topic_backend.go` | ~30 |
| FindGitRoot（纯 Go） | `config/config.go` | ~20 |
| memory.type 新增 topic 选项 + 工厂注册 | `agent/memory/memory.go` | ~15 |
| 目录初始化（`mkdir -p`） | `agent/memory/topic_backend.go` | ~10 |
| **小计** | | **~195** |

### Phase 2：SystemScheduler + 触发机制（P1）

| 任务 | 文件 | 预估行数 |
|------|------|---------|
| SystemScheduler 实现 | `cron/system_scheduler.go` | ~50 |
| DreamConfig 定义 | `config/config.go` | ~15 |
| Channel Manager 集成 | `channel/manager/manager.go` | ~20 |
| Gate Keeper（三道闸门） | `channel/manager/dream.go` | ~50 |
| Lock 管理（acquire/release/stale） | `channel/manager/dream.go` | ~40 |
| last_dream.json 读写 | `channel/manager/dream.go` | ~25 |
| Session 分组 + 预过滤 | `channel/manager/dream.go` | ~40 |
| **小计** | | **~240** |

### Phase 3：Dream SubAgent Pipeline（P1）

| 任务 | 文件 | 预估行数 |
|------|------|---------|
| PathPolicy（ctx 注入 + WriteFile 校验） | `agent/tools/path_policy.go`, `agent/tools/write.go` | ~45 |
| Dream prompt 构建（Orient + Gather + Consolidate + Prune） | `channel/manager/dream_prompt.go` | ~80 |
| SubAgent 启动编排 + 超时 + 锁清理 | `channel/manager/dream.go` | ~50 |
| SkipDream 字段 + 过滤 | `session/session.go`, `channel/manager/dream.go` | ~10 |
| **小计** | | **~185** |

### Phase 4：测试 + 完善（P2）

| 任务 | 文件 | 预估行数 |
|------|------|---------|
| TopicBackend Recall 测试 | `agent/memory/topic_backend_test.go` | ~60 |
| 闸门条件测试 | `channel/manager/dream_test.go` | ~50 |
| Lock 竞争测试 | `channel/manager/dream_test.go` | ~30 |
| PathPolicy 测试 | `agent/tools/path_policy_test.go` | ~30 |
| **小计** | | **~170** |

### 总计：约 790 行

---

## 十、与 Claude Code autoDream 的差异

| 维度 | Claude Code autoDream | Tachi AutoDream |
|------|----------------------|-----------------|
| 执行体 | Fork 子 agent（read-only bash） | SubAgent（Grep + Glob + ReadFile + WriteFile 沙箱） |
| 触发 | idle 检测 + session 数 | SystemScheduler（channel 模式 cron） |
| 存储 | `MEMORY.md` + topic files | TopicBackend（`memory.Backend` 接口实现） |
| 在 Memory 体系中的角色 | 独立系统，不走统一接口 | **标准 Backend 实现**，与 mem9/agentmemory 同级 |
| 搜索 | Grep on JSONL | `TopicBackend.Recall()` → rg grep topic files |
| 记忆使用 | 常驻上下文（MEMORY.md） | 按需 Recall（MemoryRecallReminder + MemoryRecall tool） |
| 即时记忆 | 无 | inbox.md（MemoryRecord 直接写入，不等 Dream） |
| 回滚 | 无 | superseded 状态保留 30 天 |
| 锁机制 | 未公开 | `O_EXCL` + PID 检查 + 超时 |
| 用户可见性 | 无 | **完全不可见**（SystemScheduler 隔离） |

**最大设计差异**：Tachi 将 topic files 作为正式的 `memory.Backend` 实现，通过标准接口参与 Recall。上层（MemoryRecallReminder、MemoryRecall tool）无需任何修改即可享受 Dream 产出的知识。

---

## 十一、未解决的问题

### 11.1 Topic 分类的稳定性

第一次 dream 时，LLM 可能把事实分到不太合理的 topic 里。后续 dream 可能因 LLM 不一致性导致事实在 topic 之间反复移动。

**应对**：Orient 阶段先读取现有 topic 结构作为 context，尽可能保持稳定。分类调整只发生在 Consolidate 阶段发现明显归类错误时。

### 11.2 Dream 的成本

每次 dream 消耗估算：
- 读 5 个 session（预过滤后，平均每个 50 条 user+assistant 消息）→ 约 100K input tokens
- LLM 推理 + 工具调用 → 约 5K-10K output tokens
- 每天凌晨触发，非高峰时段

`MinNewSessions` 默认 5 确保每次 dream 有足够的信息密度。

### 11.3 用户隐私

- 全局禁用：`dream.enabled: false`
- 单 session 禁用：`meta.json` 中设置 `skip_dream: true`
- Dream 只在 channel 模式运行，TUI 用户不受影响

### 11.4 Recall 质量——grep 的局限

用 `rg -i <query>` 搜索 topic files，命中率依赖措辞匹配。例如用户说"数据库配置"，可能搜不到 topic 中的"SQLite 选型"。

**应对**：
- 每个事实块包含 `关键词:` 行（Dream 在 Consolidate 阶段自动生成同义词/相关词）
- 未来可考虑在 Recall 中对 query 做简单的关键词展开（提取名词、去停用词）

### 11.5 非 git 目录的 session

WorkingDir 不在任何 git 仓库中的 session 统一归入**全局记忆**域。

### 11.6 记忆域初始化

首次使用时目录可能不存在。Dream Orchestrator 在启动 SubAgent 前自动创建：

```go
func ensureMemoryDir(memoryRoot string) error {
    return os.MkdirAll(filepath.Join(memoryRoot, "topics"), 0700)
}
```

全局记忆在 TopicBackend 初始化时创建。项目记忆在 dream 触发时按需创建。
