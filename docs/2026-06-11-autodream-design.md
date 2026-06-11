# AutoDream — 会话记忆整合系统

> 版本: 1.1 | 日期: 2026-06-11 | 状态: 设计阶段
> 关联: [可插拔 Memory Backend](./2026-05-17-memory.md),
>       [自动压缩设计](./2026-05-30-auto-compact-design.md),
>       [Native Memory v1](./2026-05-16-native-memory.md),
>       [会话存储设计](./2026-05-10-session-replace-transcript.md)

---

## 一、动机

> **Changelog**: v1.0 → v1.1（基于 code review 修正了锁机制、权限、集成策略等）

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
| 零新外部依赖 | 复用 Tachi 已有的 SubAgent + Cron + Grep + Backend |
| 被动触发 | 不占用主 agent 的上下文 window 和 token 预算 |

---

## 二、架构概览

```
┌─────────────────────────────────────────────────────────┐
│                    Cron Scheduler                        │
│  (每天凌晨 3 点 / 或 idle 时触发)                         │
└──────────────────┬──────────────────────────────────────┘
                   │ 调度
                   ▼
┌─────────────────────────────────────────────────────────┐
│                  Dream Orchestrator                      │
│  ┌─────────────┐  ┌─────────────┐  ┌──────────────────┐ │
│  │ Gate Keeper  │  │ Lock Manager │  │ Branch Selector  │ │
│  │ (三道闸门)    │  │ (互斥锁文件)  │  │ (按项目分叉)      │ │
│  └─────────────┘  └─────────────┘  └──────────────────┘ │
└──────────────────┬──────────────────────────────────────┘
                   │ 通过闸门后，按 WorkingDir 分组
           ┌───────┴───────┐
           ▼                ▼
┌──────────────────┐ ┌──────────────────┐
│  Dream SubAgent   │ │  Dream SubAgent   │  ← 每个项目一个独立 SubAgent
│  (项目 A)         │ │  (项目 B)         │
│                   │ │                   │
│  1. ORIENT        │ │  1. ORIENT        │
│  2. GATHER        │ │  2. GATHER        │
│  3. CONSOLIDATE   │ │  3. CONSOLIDATE   │
│  4. PRUNE         │ │  4. PRUNE         │
└────────┬──────────┘ └────────┬──────────┘
         ▼                     ▼
┌──────────────────┐ ┌──────────────────┐
│  项目 A 记忆       │ │  项目 B 记忆       │
│  .tachi/memory/   │ │  .tachi/memory/   │
│     (项目根目录)    │ │     (项目根目录)    │
└──────────────────┘ └──────────────────┘

┌─────────────────────────────────────────┐
│  全局记忆                                 │
│  ~/.tachi/memory/                        │
│  (记录非项目相关的事实：工作流偏好、          │
│   通用习惯、跨项目知识)                    │
└─────────────────────────────────────────┘
```

### 2.1 项目记忆 vs 全局记忆

| | **项目记忆** | **全局记忆** |
|--|------------|------------|
| 路径 | `<git-root>/.tachi/memory/` | `~/.tachi/memory/` |
| 存储内容 | 该项目的设计决策、bug 模式、架构讨论 | 用户偏好、工作流习惯、跨项目知识 |
| 有效期 | 随项目存在 | 永久（可跨项目复用） |
| 谁看到 | 只有在该项目下启动的 session | 所有 session |
| 隔离方式 | 按 git root 天然隔离 | 全局共享 |

**判断规则**（复用已有的 `config.FindProjectRoot()`，但需改造为接受参数的形式，因为 session 的 WorkingDir 可能与当前目录不同）：

```go
// FindGitRoot returns the git repository root for the given directory.
// Returns empty string if dir is not inside a git repository.
// TODO: move to config/ package alongside FindProjectRoot().
func FindGitRoot(dir string) string {
    if dir == "" {
        return ""
    }
    out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
    if err != nil {
        return ""
    }
    return strings.TrimSpace(string(out))
}

// 确定一条新事实应该写入哪个 memory 域
func resolveMemoryDomain(sessionDir, sessionWorkingDir string) (domain string, memoryRoot string) {
    workingDir := sessionWorkingDir
    if workingDir == "" {
        workingDir = sessionDir // fallback: 用 session 存储目录
    }
    projRoot := FindGitRoot(workingDir)
    if projRoot != "" {
        // 在 git 仓库内 → 项目记忆
        return "project", filepath.Join(projRoot, ".tachi", "memory")
    }
    // 不在任何 git 仓库 → 全局记忆
    return "global", filepath.Join(config.BaseDir(), "memory")
}

// 搜索时同时搜两个域
func searchAllMemoryDomains(query string, limit int) []string {
    // 1. 搜项目记忆（如果有当前项目）
    cwd, err := os.Getwd()
    if err == nil {
        if projRoot := FindGitRoot(cwd); projRoot != "" {
            results := searchTopics(filepath.Join(projRoot, ".tachi", "memory", "topics"), query)
            // ...
        }
    }
    // 2. 搜全局记忆
    results := searchTopics(filepath.Join(config.BaseDir(), "memory", "topics"), query)
    // ...
}
```

### 2.2 核心设计原则

**原则一：记忆是 hint，不是 truth**

Dream 产出的 topic files 不直接注入 system prompt。它们通过两种方式被使用：
1. **`MemoryRecall` tool** 的搜索范围：Recall 时除了搜 backend（mem9/agentmemory），还会 grep topic files
2. **SystemReminder**（可选）：在 session 开始时注入 `index.md` 的前几行作为"线索提示"

Agent 永远需要对 topic file 中的信息保持怀疑——"我记得上次讨论过这个问题，但让我验证一下"。

**原则二：Dream 是异步的，不阻塞主 loop**

与 auto-compact（同步阻塞）不同，autoDream 完全在后台通过 cron 触发。当前会话永远不会被 dream 打断。

**原则三：用 Grep，不用 embedding**

与 Claude Code 一致，dream 的 Gather 阶段只用关键词/正则搜索 session 文本。原因同 [前述 Grep vs RAG 讨论](https://akitaonrails.com/en/2026/04/06/rag-is-dead-long-context/)：
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
│   ├── preferences.md     ← 用户偏好、工作流习惯
│   ├── general-knowledge.md  ← 跨项目知识
│   └── workflows.md       ← 常用工作流
├── index.md               ← 全局索引（≤ 200 行）
├── last_dream.json        ← 全局 dream 状态
└── dream.lock             ← 全局互斥锁

# 项目记忆（仅该项目下 session 可访问）
<git-root>/.tachi/
├── memory/
│   ├── topics/
│   │   ├── design-decisions.md  ← 架构决策、技术选型
│   │   ├── bug-patterns.md      ← 反复出现的 bug 和解决方案
│   │   ├── project-ctx.md       ← 项目背景、模块说明
│   │   └── conventions.md       ← 代码约定/风格
│   ├── index.md                 ← 项目索引（≤ 200 行）
│   ├── last_dream.json          ← 项目 dream 状态
│   └── dream.lock               ← 项目互斥锁
```

> **为什么 `.tachi/` 放在项目根目录里？** 这是 Tachi 的惯例——`project_root/.tachi/` 已有 `.tachi.md`、`skills/` 等，memory 是自然扩展。且 `.tachi/` 通常已在 `.gitignore` 中。

> **为什么全局记忆放在 `~/.tachi/`？** 跨项目的记忆（用户偏好、通用知识）不应该绑定到任何特定仓库，放在 home 目录下天然共享。

### 3.2 会话到记忆域的映射

Session 的 `WorkingDir` 字段决定它属于哪个域：

```go
// session/session.go — 已有字段
type Session struct {
    WorkingDir string `json:"working_dir,omitempty"`
    // ...
}
```

Dream 在 Orient 阶段通过读取每个 session 的 `meta.json`，按 `WorkingDir` 分组。注意处理 WorkingDir 为空的情况（旧 session 或未设置）：

```go
type SessionGroup struct {
    Domain   string            // "global" 或 "project"
    Root     string            // git root 或 ""
    RootPath string            // memory 目录根路径
    Sessions []Session         // 该组下的 session 列表
}

func groupSessionsByDomain(sessions []Session, sessionDir string) []SessionGroup {
    groups := make(map[string]*SessionGroup)
    
    for _, s := range sessions {
        workingDir := s.WorkingDir
        if workingDir == "" {
            workingDir = sessionDir // fallback: 用 session 存储目录
        }
        projRoot := FindGitRoot(workingDir)
        
        var key string
        var group *SessionGroup
        
        if projRoot != "" {
            key = "project:" + projRoot
            group = &SessionGroup{
                Domain:   "project",
                Root:     projRoot,
                RootPath: filepath.Join(projRoot, ".tachi", "memory"),
            }
        } else {
            key = "global:"
            group = &SessionGroup{
                Domain:   "global",
                Root:     "",
                RootPath: filepath.Join(config.BaseDir(), "memory"),
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

### 3.3 `topics/*.md` — 主题文件

每个文件聚焦一个主题领域。项目记忆和全局记忆的 topic 分类建议不同：

```markdown
# Design Decisions

## 2026-06-10: 选择了 Go 1.26 的 `iter` 包做流式处理

来源: session 2026-06-10-223045-a1b2c3d4
状态: active

理由：相比 channel-based 方案，`iter` 包在 GC 压力上减少了约 30%，
且与标准库风格一致。

反对方：@yejia 最初倾向于 channel 方案
决策者：@yejia
关联文件：`agent/stream.go`

---

## 2026-06-08: 数据库选型从 PostgreSQL 改为 SQLite

来源: session 2026-06-08-140022-e5f6g7h8
状态: superseded  ← 已被后续决策推翻

理由：当时认为 PG 的扩展性更好...
```

**字段约定**：

| 字段 | 说明 |
|------|------|
| `来源: session <id>` | 事实来源的原始会话 ID，可追溯 |
| `状态: active / superseded / declined` | 事实当前是否仍然有效 |
| `---` | 分隔不同事实块 |

**为什么用 Markdown 而非结构化格式**：因为 dream subagent 本身就是 LLM，读写 Markdown 天然的高质量。结构化格式（JSON/YAML）需要额外的 parse/serialize 步骤，且对 LLM 不如 Markdown 友好。

### 3.4 `index.md` — 索引文件（全局 + 项目各一份）

纯指针文件，每行约 150 字符，保持在 200 行以内：

```markdown
# Memory Index

- [Preferences](./topics/preferences.md): CLI tool keybindings, Go style, logging verbosity
- [Design Decisions](./topics/design-decisions.md): stream iter, SQLite, config format (19 facts, 2 superseded)
- [Bug Patterns](./topics/bug-patterns.md): subagent worktree leak, MCP tool race (6 patterns)
- [Project Context](./topics/project-ctx.md): tachi architecture, build targets, deployment
```

**为什么不超过 200 行**：Claude Code 的经验——超过 200 行后 LLM 在压缩/回顾时倾向于忽略后半部分。200 行 × 150 字符 ≈ 30KB，足以容纳多数项目的记忆索引。

当达到 200 行时，下次 dream 的 Prune 阶段会压缩合并，而不是无限增长。

### 3.5 `last_dream.json` — 状态文件（全局 + 项目各一份）

记录上次 dream 的执行状态：

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

### 3.6 `dream.lock` — 互斥锁（全局 + 项目各一份）

Dream 启动时创建，结束时删除。如果锁文件存在（超过 5 分钟），视为上次 dream 异常退出，本次强制接管。

```
格式：<PID>:<timestamp>
示例：12345:2026-06-11T03:00:00+08:00
```

---

## 四、触发机制（Gate Keeper）

### 4.1 触发方式

两种触发路径，任一满足即可：

**路径 A：Cron 定时触发（推荐）**

使用 Tachi 现有的 Cron Scheduler，注册一个系统级 cron job：

```yaml
# 默认配置：每天凌晨 3 点
cron:
  auto_dream:
    schedule: "0 3 * * *"
    enabled: true
```

注册方式：

```go
// cron/auto_dream.go (伪代码)
func RegisterAutoDream(scheduler *cron.Scheduler) {
    if !cfg.Cron.AutoDream.Enabled {
        return
    }
    scheduler.AddJob(cron.Job{
        Name:     "auto-dream",
        Schedule: cfg.Cron.AutoDream.Schedule,
        Action:   executeDream, // → 启动 dream subagent
        Type:     cron.TypeOneshot, // 每次触发执行一次
        Notify:   "when_relevant", // 无有意义产出时不通知
    })
}
```

**路径 B：Idle 触发（备用）**

当 Tachi 检测到用户连续 N 分钟无操作（通过 TUI 的 idle 检测），且距离上次 dream 已超过阈值时，在后台触发：

```go
// 在 agent loop 的 select 中检测 idle
if idleDuration > cfg.Dream.IdleTriggerThreshold &&
   time.Since(lastDreamAt) > cfg.Dream.MinInterval {
    go triggerDreamAsync() // 异步，不阻塞主 loop
}
```

### 4.2 三道闸门（按记忆域独立检查）

由于每个记忆域（全局 + 每个项目）有独立的 `last_dream.json` 和 `dream.lock`，闸门检查也按域独立进行。Orchestrator 会先分组 session，然后对每个有足够新 session 的域触发独立的 dream SubAgent：

```go
func planDreams(sessions []session.Session, cfg *config.Config) []DreamPlan {
    groups := groupSessionsByDomain(sessions)
    var plans []DreamPlan
    
    for _, g := range groups {
        lastState := loadLastDreamState(g.RootPath)
        
        // 闸门 1: 距上次 dream ≥ MinInterval
        if time.Since(lastState.LastDreamAt) < cfg.Dream.MinInterval {
            continue
        }
        
        // 闸门 2: 该域至少 N 个新 session
        newSessions := countNewSessions(lastState.LastSessionID, g.Sessions)
        if newSessions < cfg.Dream.MinNewSessions {
            continue
        }
        
        // 闸门 3: 该域没有并发 dream 在运行
        if !acquireDreamLock(g.RootPath) {
            continue
        }
        
        plans = append(plans, DreamPlan{
            Group:    g,
            LastState: lastState,
        })
    }
    
    return plans
}
```

> **为什么按域独立？** 如果你同时改了 Tachi 项目和另一个 Go 项目，两个项目都有新 session。如果混在一起处理，一个 dream subagent 要同时处理两个完全无关的项目上下文，topic 分类会混乱。各自独立 dream 更清晰。

闸门条件：太频繁会导致 topic files 频繁变动，反而降低记忆稳定性。1 天 1 次对大多数用户足够。5 个 session 确保有足够的新信息值得整合。

### 4.3 锁机制

使用 `O_EXCL` 原子创建锁文件，避免 TOCTOU 竞态。同时检查 PID 存活性，加速过期锁回收：

```go
func acquireDreamLock(memoryDir string) (bool, error) {
    lockPath := filepath.Join(memoryDir, "dream.lock")
    
    // 尝试原子创建锁文件
    f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0644)
    if err == nil {
        // 成功获取锁
        defer f.Close()
        fmt.Fprintf(f, "%d:%s", os.Getpid(), time.Now().Format(time.RFC3339))
        return true, nil
    }
    
    if !errors.Is(err, os.ErrExist) {
        return false, fmt.Errorf("acquire dream lock: %w", err)
    }
    
    // 锁文件已存在 — 检查是否过期
    data, err := os.ReadFile(lockPath)
    if err != nil {
        // 文件刚被删了？重试一次
        return acquireDreamLock(memoryDir)
    }
    
    parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
    if len(parts) != 2 {
        // 格式损坏，接管
        os.Remove(lockPath)
        return acquireDreamLock(memoryDir)
    }
    
    pid, pidErr := strconv.Atoi(parts[0])
    timestamp, timeErr := time.Parse(time.RFC3339, parts[1])
    
    if pidErr == nil {
        // PID 存活性检查 — 如果进程还活着，锁有效
        proc, err := os.FindProcess(pid)
        if err == nil && proc.Signal(os.Signal(syscall.Signal(0))) == nil {
            return false, nil // 进程还在运行
        }
        // PID 已死，锁过期
    }
    
    if timeErr == nil && time.Since(timestamp) < 5*time.Minute {
        // 时间戳仍然在有效期内，但 PID 已死 → 视为过期
        // 双重保障：超时 + PID 检查
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

## 五、Dream 执行流程

Dream 的核心执行体是一个 **SubAgent**（复用已有的 SubAgent 机制）。给它限制只允许访问以下工具：

```
allowed_tools: [ReadFile, Grep, Glob, WriteFile]
```

> **为什么没有 Bash？** 安全性考虑——即使 working directory 被限制，Bash 仍可通过绝对路径读取系统敏感文件（`/etc/passwd`、`~/.ssh/id_rsa` 等）。Grep + Glob + ReadFile 的组合已经能覆盖 Gather 阶段的所有需求。如果确实需要 `wc -l`、`date` 等元操作，可考虑后续创建受限的 `DreamBash` 工具（白名单命令模式）。

这样 dream subagent 只能读 session 目录和写 memory 目录，不能执行任意命令或修改项目文件。

此外，Dream 在 Consolidate 阶段还应将新发现的事实**同步写入现有的 memory backend**（通过 `Backend.Store` + `DirectContent`），而不仅仅是 topic files。这样：
- 自动召回（MemoryRecallReminder）能立即看到新知识
- 显式工具调用（MemoryRecall）能搜到更丰富的 topic file 上下文
- 两个读取路径都覆盖到了

### 5.1 Phase 1: Orient（定向）

**目标**：了解当前记忆域的边界，确定需要处理哪些新 session。

```
步骤：
1. 读 <memory-root>/last_dream.json → 知道上次处理到哪个 session
2. 读 <memory-root>/index.md → 知道现有 topic 结构  
3. 该域下的 session 列表已在 Orchestrator 层面按 WorkingDir 分组传入
4. 对每个新 session，读 meta.json 获取标题和时间
```

> **记忆域的切换**：SubAgent 启动时已经知道自己属于哪个域（全局/项目）。Orient 阶段读取的是该域的 `last_dream.json` 和 `index.md`，不会跨域混淆。

**输出**：一个新 session 列表，按时间排序，每个带标题、ID 和 WorkingDir。

**SubAgent prompt 示例**：

```
你是一个记忆整合 agent。你的任务是从新的对话 session 中提取有价值的信息。
你当前处理的记忆域是：{{.Domain}}（{{.MemoryRoot}}）。

## 第一步：定向

请完成以下操作：
1. 读取 {{.MemoryRoot}}/last_dream.json（如果存在）
2. 读取 {{.MemoryRoot}}/index.md（如果存在）
3. 以下 session 列表属于当前域，逐一读取 meta.json 获取标题

请用以下格式输出结果：

## Sessions to process
| # | Session ID | Title | WorkingDir | Created At |
|---|-----------|-------|-----------|-----------|
| 1 | ... | ... | ... | ... |

## Existing topics
- topic/xxx.md: 现有条目数，状态
```

### 5.2 Phase 2: Gather（收集）

**目标**：从新 session 中提取事实。

```
步骤：
1. 对每个新 session，读 messages.jsonl
2. 使用 Grep 搜索关键模式：
   - "我决定" / "选择" / "选了" → 可能的决策点
   - "不喜欢" / "偏好" / "习惯" → 用户偏好
   - "bug" / "问题" / "修复" / "workaround" → bug 模式
   - "记住" / "注意" / "重要" → 显式需要记住的信息
3. 对每个匹配的上下文，ReadFile 读取完整段落
4. 用 Bash 统计 session 中的一些元数据（工具调用次数、模型、项目等）
```

**Grep 模式不是硬编码的**——LLM 自己决定搜什么。上面的模式只是作为指导，subagent 可以根据具体情况调整搜索词。

**输出**：一组候选事实，每个事实带来源 session ID、摘要、原始上下文。

**SubAgent prompt 示例**：

```
## 第二步：收集

对每个新 session，读取 messages.jsonl 并使用 Grep 搜索以下模式：

1. 决策相关的词：决定、选择、选了、改用、弃用、替换
2. 偏好相关的词：喜欢、不喜欢、偏好、习惯、希望
3. Bug 相关的词：bug、问题、修复、报错、崩溃、workaround
4. 显式记忆：记住、注意、重要、别忘了

对每个 Grep 匹配：
- 用 ReadFile 读取匹配行前后各 5 行的上下文
- 判断是否构成一个"事实"（有信息量、可复用的结论）
- 如果是，记录为候选事实

请输出候选事实列表：

## Candidate Facts
| # | Session ID | Category | Summary | Key Evidence |
|---|-----------|---------|---------|-------------|
| 1 | ... | decision | "将数据库从 PG 改 SQLite" | "因为部署环境只有 256MB" |

注意：只提取有价值的信息。问候、闲聊、无关的对话不要记录。
```

### 5.3 Phase 3: Consolidate（整合）

**目标**：将新事实写入 topic files，与已有事实进行对比和合并。

```
步骤：
1. 对每个候选事实，判断它属于哪个 topic（或创建新 topic）
2. 如果 topic file 已存在：
   a. 搜索 topic file 中是否已有相似事实
   b. 如果新事实与旧事实一致 → 跳过（去重）
   c. 如果新事实与旧事实矛盾 → 将旧事实标记为 superseded，写入新事实
   d. 如果新事实补充了旧事实 → 合并更新
3. 如果 topic file 不存在 → 创建新 topic file
4. 每个事实块包含：来源 session ID、状态（active/superseded）、内容
```

**矛盾检测**：当 LLM 判断新旧事实存在逻辑矛盾时（"从 PG 改 SQLite" vs "决定继续用 PG"），旧事实会被标记为 `superseded` 而非删除。这样保留了决策演变的历史，且不会丢失回滚所需的信息。

**输出**：更新后的 topic files。

**SubAgent prompt 示例**：

```
## 第三步：整合

对每个候选事实，将其写入对应的 topic file：

规划主题：
- preferences: 用户偏好、习惯、工作流
- design-decisions: 架构决策、技术选型
- bug-patterns: 反复出现的 bug 和解决方案
- project-ctx: 项目背景、模块说明、构建信息

写入格式：

## YYYY-MM-DD: 事实标题

来源: session <session-id>
状态: active

事实描述...

---

合并规则：
- 若 topic file 中已有相同事实 → 跳过
- 若新旧事实矛盾 → 保留新事实，旧事实标记为 superseded
- 若新事实是旧事实的补充 → 合并内容

请在所有写入操作前先 ReadFile 确认现有内容，再决定如何写入。
```

### 5.4 Phase 4: Prune（裁剪）

**目标**：保持 topic files 和 index.md 的精简。

```
步骤：
1. 检查 index.md 行数是否超过 200 行
2. 如果超过：
   a. 删除标记为 superseded 超过 30 天的事实
   b. 合并少于 3 个事实的"稀疏 topic"到 misc.md
   c. 合并语义相似的条目（LLM 自行判断）
   d. 如果单个 topic file 超过 50 个事实，生成一个 10-15 行的摘要
      放在文件顶部（旧内容归档为 <topic>.backup.md）
   e. 对长期未被 MemoryRecall 访问过的 topic 降低排序优先级
3. 更新 index.md：
   a. 反映新增的 topic
   b. 更新每个 topic 的事实计数（含 active/superseded 统计）
   c. 保持在 200 行以内
4. 将新提取的事实同步写入 memory backend（Backend.Store + DirectContent）
5. 写入 last_dream.json（更新状态）
6. 删除 dream.lock
```

**裁剪策略不是硬删除**：
- `superseded` 的事实保留 30 天后再删除，给用户回看的时间窗口
- 删除前会将关键决策路径保留在 topic file 的概述段落中
- 如果一个 topic file 过长，LLM 会生成摘要替换前半部分

---

## 六、集成点

### 6.1 与现有 Memory Backend 的关系

```
┌──────────────┐      turn/compact/session 写入
│  Agent Loop   │ ──────────────────────────────►  mem9 / agentmemory backend
│  (每轮对话)    │                                      (向量语义搜索)
└──────────────┘
                                                     ┌─────────────────┐
                                                     │  Fast Recall     │
                                                     │  (每次用户消息)    │
                                                     └────────┬────────┘
                                                              │
                                                              ▼
┌──────────────┐      cron/dream 写入                    ┌─────────────────┐
│  Dream        │ ──────────────────────►  memory/topics/*.md                │
│  (每日后台)    │      │                                      (结构化 Markdown)  │
│               │      │                                     └─────────────────┘
│               │      │                                              ▲
│               │      ▼                                              │
│               │ ──────────────────────►  memory backend              │
│               │      (Backend.Store + DirectContent)                 │
└──────────────┘                                                     │
                                                     ┌─────────────────┐
                                                     │  Deep Recall     │
                                                     │  (MemoryRecall   │
                                                     │   tool 触发)      │
                                                     └────────┬────────┘
                                                              │
                                                              ▼
                                                     ┌─────────────────┐
                                                     │  topic files +  │
                                                     │  backend (合并)  │
                                                     └─────────────────┘
```

两个存储路径互补，不重叠：

| | Backend（mem9/agentmemory） | Dream Topics（本地 Markdown） |
|--|----------------------------|-----------------------------|
| 写入时机 | 每轮对话 / 压缩 / 会话结束 | 每日后台整合 |
| 写入粒度 | 原始对话摘录 | 经过推理的知识 |
| 搜索方式 | 向量语义搜索 + 关键词 | Grep 关键词搜索 |
| 信息类型 | "原始记忆"——他说了什么 | "知识"——这意味着什么 |
| 一致性 | 写时即一致 | 可能有滞后（每日更新） |
| 使用方式 | 自动 Recall，每次用户消息 | 工具触发 Recall（MemoryRecall tool） |

### 6.2 MemoryRecall 的增强——自动召回也搜 topic files

现有的 `MemoryRecallReminder`（`agent/systemreminder/memory_reminder.go`）每轮对话自动召回 backend 记忆。增强后，它也应同时 grep topic files：

```go
func (r *MemoryRecallReminder) Generate(ctx Context) []string {
    if r.Backend == nil || ctx.IsToolResult || ctx.SkipRecall {
        return nil
    }
    if ctx.CurrentPrompt == "" {
        return nil
    }
    
    limit := r.Limit
    if limit <= 0 { limit = 5 }
    recallTimeout := r.Timeout
    if recallTimeout <= 0 { recallTimeout = 3 * time.Second }
    
    // 1. 从 backend 搜索（语义搜索，有超时）
    recallCtx, cancel := context.WithTimeout(context.Background(), recallTimeout)
    defer cancel()
    backendEntries, _ := r.Backend.Recall(recallCtx, ctx.CurrentPrompt, limit)
    
    // 2. 从 topic files 搜索（grep，毫秒级，无额外成本）
    topicResults := searchAllMemoryDomains(ctx.CurrentPrompt, limit)
    
    // 3. 合并结果：topic 优先（提炼过的知识），backend 补充
    entries := mergeResults(backendEntries, topicResults, limit)
    if len(entries) == 0 {
        return nil
    }
    
    // ... 格式化为 <relevant-memories> 块
}
```

`searchAllMemoryDomains` 的实现——始终搜全局，有条件地搜项目。增加了错误日志便于调试：

```go
func searchAllMemoryDomains(query string, limit int) []string {
    var results []string
    
    // 1. 始终搜全局记忆
    globalTopics := filepath.Join(config.BaseDir(), "memory", "topics")
    r, err := searchTopics(globalTopics, query)
    if err != nil {
        debuglog.DefaultLogger.Log("DreamTopics: global search failed: %v", err)
    } else {
        results = append(results, r...)
    }
    
    // 2. 如果有当前项目，也搜项目记忆
    if cwd, err := os.Getwd(); err == nil {
        if projRoot := FindGitRoot(cwd); projRoot != "" {
            projTopics := filepath.Join(projRoot, ".tachi", "memory", "topics")
            r, err := searchTopics(projTopics, query)
            if err != nil {
                debuglog.DefaultLogger.Log("DreamTopics: project search failed: %v", err)
            } else {
                results = append(results, r...)
            }
        }
    }
    
    return results
}

func searchTopics(topicsDir, query string) ([]string, error) {
    if _, err := os.Stat(topicsDir); os.IsNotExist(err) {
        return nil, nil // 目录不存在不是错误
    }
    
    cmd := exec.Command("rg", "-l", "-i", query, topicsDir)
    out, err := cmd.Output()
    if err != nil {
        if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
            return nil, nil // rg 返回码 1 = 没有匹配，不是错误
        }
        return nil, fmt.Errorf("rg search failed: %w", err)
    }
    
    files := strings.Split(strings.TrimSpace(string(out)), "\n")
    if len(files) == 0 || (len(files) == 1 && files[0] == "") {
        return nil, nil
    }
    
    var results []string
    for _, f := range files {
        cmd = exec.Command("rg", "-C", "3", "-i", query, f)
        out, err = cmd.Output()
        if err == nil && len(out) > 0 {
            results = append(results, string(out))
        }
    }
    return results, nil
}
```

**merge 策略**（同时用于 MemoryRecall tool 和 MemoryRecallReminder）：

```
1. backend 搜索得 N 条，topic 搜索得 M 条
2. 去重：相同 session ID + 相同摘要内容只保留一次（保留 topic 版本，因为更精炼）
3. 排序：topic 结果优先于 backend 结果
4. 截断：总共不超过 limit 条
   - 如果 N+M ≤ limit，全部展示
   - 如果 N+M > limit，topic 结果全保留，backend 结果按 score 截断
```

> **为什么自动召回也要搜 topic files？** 虽然 `searchTopics` 额外调用了一次 `rg`，但本地 `rg` 是毫秒级的（即使搜数千行 Markdown）。相比 `Backend.Recall()` 的 HTTP 网络延迟（通常 50-500ms），这个额外成本几乎可以忽略不计。但它带来的收益显著——Dream 提炼的知识在主对话中自动可见，不需要 LLM 主动调用 MemoryRecall tool。

### 6.3 SystemReminder——二级补充（默认关闭）

在自动召回（MemoryRecallReminder）增强为也搜 topic files 后，DreamReminder 的作用降为**二级补充**。它可以在 session 开始时，注入**当前域**的 index.md 前几行作为轻量级"线索"预览：

```go
// agent/systemreminder/dream_reminder.go
type DreamReminder struct {
    globalDir string // ~/.tachi/memory/
    projDir   string // <git-root>/.tachi/memory/（可能为空）
}

func (r *DreamReminder) Generate(ctx Context) []string {
    if !ctx.IsFirstMessage {
        return nil
    }
    
    var hints []string
    hints = append(hints, "## Memory Topics (hints from past sessions)")
    
    // 1. 搜项目记忆（如果有）
    if r.projDir != "" {
        indexData, err := os.ReadFile(filepath.Join(r.projDir, "index.md"))
        if err == nil {
            lines := strings.Split(string(indexData), "\n")
            if len(lines) > 10 {
                lines = lines[:10]
            }
            hints = append(hints, "--- Project Memory ---")
            hints = append(hints, lines...)
        }
    }
    
    // 2. 搜全局记忆
    indexData, err := os.ReadFile(filepath.Join(r.globalDir, "index.md"))
    if err == nil {
        lines := strings.Split(string(indexData), "\n")
        if len(lines) > 10 {
            lines = lines[:10]
        }
        hints = append(hints, "--- Global Memory ---")
        hints = append(hints, lines...)
    }
    
    if len(hints) == 1 {
        return nil // 没有可用的记忆线索
    }
    
    hints = append(hints, "Use MemoryRecall to search topic files for details.")
    return hints
}
```

> **注意**：这个 reminder 是**可选的**，**默认关闭**。因为：
> - 自动召回已经能搜到 topic files，DreamReminder 提供的额外价值有限
> - 注入任何额外内容都会占用上下文 token
> - 如果项目上下文很复杂，前 10 行 index 可能反而是噪音
> - Agent 可以在需要时主动调用 MemoryRecall tool，不依赖自动注入

### 6.4 配置

在 `config/config.go` 中新增 `DreamConfig`：

```go
type DreamConfig struct {
    Enabled          bool          `yaml:"enabled" default:"true"`           // 是否启用 autoDream
    Schedule         string        `yaml:"schedule" default:"0 3 * * *"`    // cron 表达式
    MinInterval      time.Duration `yaml:"min_interval" default:"24h"`       // 最小触发间隔
    MinNewSessions   int           `yaml:"min_new_sessions" default:"5"`     // 最少新 session 数
    IdleTrigger      bool          `yaml:"idle_trigger" default:"false"`     // 是否启用 idle 触发
    IdleDuration     time.Duration `yaml:"idle_duration" default:"10m"`      // idle 多久后触发
    MaxTopicsLines   int           `yaml:"max_topics_lines" default:"200"`   // index.md 最大行数
    TopicDir         string        `yaml:"-"`                               // 运行时解析：~/.tachi/memory/topics/
    
    // Dream subagent 配置
    SubagentTimeout  time.Duration `yaml:"subagent_timeout" default:"10m"`  // dream 超时
    SubagentMaxIters int           `yaml:"subagent_max_iters" default:"30"` // subagent 最大迭代次数
}
```

### 6.5 启动注册

在 `main.go` 或 agent 初始化时注册：

```go
func setupAutoDream(scheduler *cron.Scheduler, cfg *config.Config) {
    if !cfg.Dream.Enabled {
        return
    }
    
    scheduler.AddJob(cron.Job{
        Name:     "auto-dream",
        Schedule: cfg.Dream.Schedule,
        Action: func(ctx context.Context) error {
            return executeDream(ctx, cfg)
        },
        Type:   cron.TypeOneshot,
        Notify: "when_relevant",
    })
}
```

---

## 七、安全性

### 7.1 SubAgent 沙箱

Dream subagent 只被授予有限的工具：

```
allowed_tools: ["ReadFile", "Grep", "Glob", "WriteFile"]
```

- **无 SubAgent 工具** → 不能递归 fork
- **无 Cron 工具** → 不能自注册 cron job
- **无 EditFile 工具** → 不能修改项目文件
- **无 Bash 工具** → 不能通过绝对路径读取系统敏感文件

这样 dream subagent 只能读 session 目录和写 memory 目录，不能执行任意命令或修改项目文件。

Grep + Glob + ReadFile 的组合已足以覆盖 Gather 阶段的所有需求（关键词搜索、文件名匹配、文件内容读取）。如需 `wc -l`、`date` 等元操作，后续可创建白名单式的受限 Bash。

### 7.2 写入范围限制（双域）

WriteFile 的目标路径被限制为全局记忆 `~/.tachi/memory/` 或当前项目的 `.tachi/memory/` 目录内。同一个 Dream SubAgent 只写一个域：

```go
// 在 WriteFile tool 中增加路径检查
func validateDreamWritePath(path string, allowedDomains []string) error {
    abs, _ := filepath.Abs(path)
    
    for _, domain := range allowedDomains {
        if strings.HasPrefix(abs, domain) {
            return nil // 允许写入该域
        }
    }
    
    return fmt.Errorf("write denied: %s is outside allowed memory domains", path)
}
```

> Dream Orchestrator 启动 SubAgent 时会传入 `--allowed-write-dirs` 参数。全局 dream 只能写 `~/.tachi/memory/`，项目 dream 只能写 `<project>/.tachi/memory/`。Cross-contamination 被路径校验阻止。

### 7.3 锁超时

如果 dream 异常退出（SIGKILL、OOM、断电），锁文件不会被清理。重启后的下次 dream 会检测到过期锁并接管。

检测策略（详见第 4.3 节）：
1. **PID 存活性检查**：`os.FindProcess(pid)` + `proc.Signal(syscall.Signal(0))`，判断持有锁的进程是否还在运行
2. **时间戳超时**：>5 分钟视为过期（双重保障，避免 PID 被回收后分配给新进程的极端情况）
3. 两者任一满足即视为可接管

---

## 八、实现计划

### Phase 1：基础设施（P0）

| 任务 | 文件 | 预估行数 |
|------|------|---------|
| DreamConfig 定义 | `config/config.go` | ~15 |
| 目录结构初始化（`mkdir -p ~/.tachi/memory/topics/`） | 启动路径 | ~10 |
| Lock 管理（acquire/release/stale check） | `cron/auto_dream.go` | ~40 |
| last_dream.json 读写 | `cron/auto_dream.go` | ~30 |
| 三道闸门实现（shouldDream） | `cron/auto_dream.go` | ~35 |
| Cron job 注册 | `main.go` | ~15 |
| **小计** | | **~145** |

### Phase 2：Dream SubAgent + Backend 写入（P1）

| 任务 | 文件 | 预估行数 |
|------|------|---------|
| Dream prompt 构建（Orient + Gather 指令） | `cron/auto_dream.go` | ~50 |
| Pipeline 编排（Orient→Gather→Consolidate→Prune） | `cron/auto_dream.go` | ~60 |
| WriteFile 路径安全限制 | `agent/tools/write.go` | ~15 |
| Dream 结果写入 backend（Backend.Store + DirectContent） | `cron/auto_dream.go` | ~20 |
| 超时处理 + 锁清理 | `cron/auto_dream.go` | ~20 |
| **小计** | | **~165** |

### Phase 3：集成（P1）

| 任务 | 文件 | 预估行数 |
|------|------|---------|
| MemoryRecallReminder 增强（自动召回也搜 topic files + merge 策略） | `agent/systemreminder/memory_reminder.go` | ~30 |
| MemoryRecall tool 增强（同样搜 topic files） | `agent/tools/memory_recall.go` | ~15 |
| `searchDreamTopics()` 实现（含错误处理） | `agent/memory/topic_search.go` | ~45 |
| `skip_dream` 字段在 meta.json 中的支持 | `session/session.go`, `cron/auto_dream.go` | ~15 |
| DreamReminder（可选，二级补充） | `agent/systemreminder/dream_reminder.go` | ~35 |
| **小计** | | **~140** |

### Phase 4：Idle 触发 + 完善（P2）

| 任务 | 文件 | 预估行数 |
|------|------|---------|
| TUI idle 检测 | `tui/model.go` | ~20 |
| Idle 触发 dream | `agent/agent_loop.go` | ~15 |
| 测试：闸门条件测试 | `cron/auto_dream_test.go` | ~50 |
| 测试：topic 搜索测试 | `agent/memory/topic_search_test.go` | ~40 |
| 测试：lock 竞争测试 | `cron/auto_dream_test.go` | ~30 |
| **小计** | | **~155** |

### 总计：约 605 行

---

## 九、与 Claude Code autoDream 的差异

| 维度 | Claude Code autoDream | Tachi AutoDream（本方案） |
|------|----------------------|-------------------------|
| 执行体 | Fork 子 agent（read-only bash） | **SubAgent**（Grep + Glob + ReadFile，安全性更高） |
| 触发 | idle 检测 + session 数 | **Cron Scheduler** + 可选的 idle 触发 + `tachi dream` CLI |
| 存储 | `MEMORY.md` + topic files | **Markdown topic files** + **Backend**（同时写入双存储） |
| 索引 | `MEMORY.md`（纯指针，≤200 行） | `index.md`（纯指针，≤200 行） |
| 搜索 | Grep on JSONL transcripts | Grep on `session/<id>/messages.jsonl` |
| 记忆使用 | 写入 `MEMORY.md` 后被主 agent 自动读取 | **自动召回（MemoryRecallReminder）**搜 backend + topic files + 显式 MemoryRecall 工具 |
| 记忆定位 | 永久在上下文中（hint） | 自动召回时注入 `<relevant-memories>`，不在上下文中常驻 |
| 回滚机制 | 无显式回滚 | Superseded 状态保留 30 天 |
| 锁机制 | 未公开 | `O_EXCL` 原子锁 + PID 存活性检查 + 时间戳超时 |
| 守护进程 | 内置在 Claude Code 中 | **零守护进程**——纯 cron job + SubAgent |

**最大的设计差异**：Claude Code 把 `MEMORY.md` 作为常驻上下文的索引，每次用户请求都能看到。Tachi 的设计更偏向"按需加载"——Dream 产出的知识通过增强后的 MemoryRecallReminder 在**每轮对话中自动召回**（backend + topic files 双搜索），但只召回到当前 query 相关的部分，而非全部注入。这样：
- 比常驻上下文更省 token（只召回相关的）
- 比纯显式调用更主动（不需要 LLM 主动想起去调 MemoryRecall）
- 是"常驻内存"和"纯按需"之间的折中方案

---

## 十、未解决的问题

### 10.1 Topic 分类的稳定性

第一次 dream 时，LLM 可能把事实分到不太合理的 topic 里。后续 dream 可能会修正分类，但也可能因为 LLM 的不一致性导致事实在 topic 之间反复移动。

**应对**：首次分类后，Orient 阶段先读取现有 topic 结构并作为 context 告诉 LLM"已有这些分类"，尽可能保持稳定。分类调整只发生在 Consolidate 阶段发现明显归类错误时。

### 10.2 Dream 的成本

每次 dream 调 SubAgent 会产生 token 消耗。粗略估算：
- 读 5 个 session（平均每个 200 条消息）→ 大量的 ReadFile/Grep 调用
- 写/更新 topic files → WriteFile
- LLM 调用本身（subagent loop）+ 工具结果 token

一个典型的 dream 可能消耗 50K-200K 输入 token 和 5K-10K 输出 token。

**应对**：
- 每天凌晨 3 点触发，此时 token 价格较低（非高峰时段）
- `MinNewSessions` 默认 5，确保每次 dream 处理的 session 有足够的信息密度
- `Notify: "when_relevant"`——如果 dream 没有发现任何新事实（全部去重），不通知用户

### 10.3 用户隐私

Dream 会读取所有 session 的历史对话。如果用户有敏感会话，可能不希望被整合进 memory。

**应对**：
- 用户可以通过 `~/.tachi/config.yaml` 完全禁用 Dream：`dream.enabled: false`
- **每个 session 独立控制**（P0 实现）：在 `session/meta.json` 中增加 `skip_dream: true` 标记

```go
// session/session.go
type Session struct {
    // ...
    SkipDream bool `json:"skip_dream,omitempty"` // 该 session 不参与 Dream 整合
}
```

Orchestrator 在分组时过滤掉标记为 `skip_dream` 的 session：

```go
func filterSkippedSessions(sessions []Session) []Session {
    var filtered []Session
    for _, s := range sessions {
        if s.SkipDream {
            continue
        }
        filtered = append(filtered, s)
    }
    return filtered
}
```

用户可以在对话中说"这个 session 不要记入记忆"，由 agent 调用配置工具修改 `meta.json`，或者通过 `/dream skip` 命令设置。

### 10.4 Dream 与 Compact 的交互

如果 session 在 auto-compact 后被分成"原始 session + 压缩后 session"的父子关系（`CompactedParentID` / `CompactedChildID`），dream 应该处理哪个？

**规则**：Dream 优先处理**叶子 session**（没有 `CompactedChildID` 的 session）。但如果叶子 session 有 `CompactedParentID`（说明它是压缩产物），**同时也读取原始 session 的消息内容**来补充信息密度，避免压缩摘要中的信息损失：

```go
func collectSessionsForDream(sessionID string, sessionManager *session.Manager) []SessionWithMessages {
    sess, _ := sessionManager.Get(sessionID)
    if sess == nil {
        return nil
    }
    
    // 如果有压缩父 session，同时收集父 session 的消息
    if sess.CompactedParentID != "" {
        parentMsgs, _ := sessionManager.LoadMessagesByID(sess.CompactedParentID)
        if len(parentMsgs) > 0 {
            return []SessionWithMessages{
                {Session: *sess, Messages: currentMsgs},
                {Session: parentSess, Messages: parentMsgs},
            }
        }
    }
    
    return []SessionWithMessages{{Session: *sess, Messages: currentMsgs}}
}
```

> 规则只读不写——原始 session 的消息只用于 Gather 阶段的信息提取，不会因为被读取而被标记为"已处理"。`last_dream.json` 中记录的仍是叶子 session 的 ID。

### 10.5 Dream 的并发上限

如果有多个项目都有新 session 需要 dream，Orchestrator 可以并行启动多个 SubAgent 吗？

**规则**：可以并行，但限制全局 + 所有项目最多 3 个并发 dream。原因是：
- 每个 dream SubAgent 占用一个 LLM API 调用，并行太多可能触发 rate limit
- 3 个并发对大多数场景足够（通常只有 1-2 个项目活跃）
- 本域内的 dream 已经是互斥的（通过 `dream.lock`），不会有同一个域同时跑两个 dream

```go
const maxConcurrentDreams = 3
var activeDreams atomic.Int64

func executeDream(ctx context.Context, plan DreamPlan) error {
    if activeDreams.Load() >= maxConcurrentDreams {
        return fmt.Errorf("too many concurrent dreams: %d", activeDreams.Load())
    }
    activeDreams.Add(1)
    defer activeDreams.Add(-1)
    // ... run subagent
}
```

### 10.6 记忆域初始化

首次使用时，项目 `.tachi/memory/` 目录可能还不存在。谁来创建它？

**方案**：Dream Orchestrator 在启动 SubAgent 前自动创建：

```go
func ensureMemoryDomain(memoryRoot string) error {
    dirs := []string{
        filepath.Join(memoryRoot, "topics"),
    }
    for _, d := range dirs {
        if err := os.MkdirAll(d, 0700); err != nil {
            return fmt.Errorf("create memory dir %s: %w", d, err)
        }
    }
    return nil
}
```

> 全局记忆在 Tachi 启动时初始化（已在 Phase 1 中）。项目记忆在 dream 触发时按需初始化。不需要在进入每个新项目时提前创建。

### 10.7 非 git 目录的特殊情况

如果 session 的 WorkingDir 在一个目录树中，但不是 git 仓库（没有 `.git` 目录，或 `git rev-parse` 失败），该 session 应归入哪一域？

**规则**：统一归入**全局记忆**域。因为：
- 没有项目边界，所有非 git 目录的 session 共享同一个记忆空间
- 例如你在 `/tmp/` 下测试、或在家目录下跑一些零散命令，这些都属于"全局活动"
- 如果你后来把某个目录变成了 git 仓库，未来在该目录下的 session 会自动分配到该项目域。之前的 session（非 git 时）仍然留在全局记忆中

---

### 10.8 `tachi dream` CLI 子命令

在开发测试和生产运维中，用户可能需要手动触发 Dream，而不是等 cron 在凌晨 3 点自动触发。

**建议**：在 Phase 1 或 Phase 2 中加入 `tachi dream` CLI 子命令：

```
tachi dream                   # 为当前项目运行 dream
tachi dream --all             # 为所有项目 + 全局运行 dream
tachi dream --domain global   # 只跑全局记忆
tachi dream --dry-run         # 只展示会处理哪些 session，不实际执行
```

这也有助于 Phase 4 的集成测试——不需要修改系统时间就能验证 Dream 管线是否正常工作。

---

### 10.9 Notify 心跳

当前设计使用 `Notify: "when_relevant"`——如果 Dream 没有发现新事实（全部去重），用户不会收到任何通知。这在减少噪音的同时，也让用户无法确认系统是否在正常工作。

**建议**：改为混合策略：
- **有实质产出**（新事实 ≥ 1）：`Notify: "always"`，报告新增/更新/淘汰的事实数量
- **无实质产出**（新事实 = 0）：定期发送简短心跳，例如每 7 天一次：

```
"AutoDream heartbeat (global): last run 2026-06-11, processed 5 sessions, 0 new facts found. All memories up to date. [project:tachi] last run 2026-06-10, processed 3 sessions, 2 new facts, 1 superseded."
```

这样用户既能感知系统存活状态，又不会被每日"无事可报"的通知刷屏。
