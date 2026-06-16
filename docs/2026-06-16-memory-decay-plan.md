# 记忆衰减系统（Memory Decay）— 轻量实现方案

状态: draft
日期: 2026-06-16

## 动机

当前记忆系统（MemoryRecall + Dream）存在盲点：

| 环节 | 现有逻辑 | 问题 |
|------|---------|------|
| MemoryRecall 召回 | 纯关键词匹配（rg keyword grep + 标题/关键词/时效加分） | 不区分"刚刚用过的重要决策"和"三个月前的碎碎念"，只要关键词命中就一起捞 |
| Dream Prune | LLM 看日期字符串，">30 天 superseded 就删" | 一刀切，不可靠。被反复引用的 fact 即使 superseded 也不该急着删 |
| 记忆留存 | 写进 topic 文件就是永久的，直到被 superseded 或手动删 | 没有"这段记忆多久没被碰过了"的生命周期信号 |

艾宾浩斯遗忘曲线提供了一种思路：记忆被频繁访问的存活更久，冷门的自然衰减。本方案在不改 topic 文件格式的前提下，引入轻量的 **衰减追踪 + 召回加权 + 强化机制**。

## 核心设计

### Fact 标识

每个 topic 文件中的 fact 块通过稳定 ID 追踪：

```
fact_id = "topic:<文件名>:<sha256前8位(fact内容)>"
```

与 TopicBackend 已有的 `Entry.ID` 格式一致（`topic:<文件>:<hash>`），仅 hash 算法改用 SHA-256。

### 衰减公式

```
decay = exp(-ln(2) × elapsed / halfLife)
```

- **半衰期** = 7 天（fact 7 天不被引用，衰减到 0.5；14 天到 0.25）
- 衰减范围 0~1，1 为最新未衰减

### 强化机制

每次 MemoryRecall 命中一个 fact，自动强化：

```
reinforcements++
last_reinforced = now
decay = 1.0
```

### 存储位置

衰减数据全部存在现有 `last_dream.json` 中，新增 `fact_states` 字段：

```json
{
  "last_dream_at": "2026-06-16T03:00:00Z",
  "sessions_dreamed": 5,
  "fact_states": {
    "topic:deploy.md:abc123": {
      "id": "topic:deploy.md:abc123",
      "topic_file": "deploy.md",
      "decay": 0.72,
      "reinforcements": 3,
      "last_reinforced": "2026-06-14T10:00:00Z",
      "created_at": "2026-06-01T00:00:00Z",
      "superseded": false
    }
  }
}
```

JSON `omitempty` 保证旧格式直接兼容（`fact_states` 不存在 → 空 map，不影响现有逻辑）。

## 改动计划

### Step 1：基础设施 — 衰减算法 + Fact 扫描（新文件）

新增 `dream/decay.go`：

- `CalculateDecay(lastReinforced time.Time) float64` — 纯函数，基于半衰期计算当前衰减
- `ScanTopicFacts(memoryRoot string, existingStates map[string]*FactState) map[string]*FactState` — 扫描 `topics/*.md`，识别 fact 块，与已有状态 merge：
  - 新 fact → 初始化 `decay=1.0, reinforcements=0`
  - 已有 fact → 保留强化计数，更新 superseded 标记和衰减
  - 已不在文件中的 fact → 标记待清理（返回时排除）

`dream/dream.go` State struct 扩展：

```go
type FactState struct {
    ID              string    `json:"id"`
    TopicFile       string    `json:"topic_file"`
    Decay           float64   `json:"decay"`
    Reinforcements  int       `json:"reinforcements"`
    LastReinforced  time.Time `json:"last_reinforced"`
    CreatedAt       time.Time `json:"created_at"`
    Superseded      bool      `json:"superseded"`
}

// State 新增字段
FactStates map[string]*FactState `json:"fact_states,omitempty"`
```

新增 `dream/decay_test.go`：`TestCalculateDecay`、`TestScanTopicFacts`、`TestScanTopicFacts_Merge`

### Step 2：Dream 后处理接入

修改 `dream/runner.go` — `RunDream()` 在 LLM 执行完毕、返回 State 之前：

```go
// 在 drain events 之后：

// Post-dream: scan topic files and update decay states.
factStates := ScanTopicFacts(plan.Group.MemoryRoot, plan.LastState.FactStates, logger)

state := State{
    LastDreamAt:     time.Now(),
    SessionsDreamed: len(plan.ActiveSessions),
    FactStates:      factStates,
    // 统计新增/取代/清理的 fact 数量
}
```

无需改 `Orchestrator` 和 `executePlans`，State 透传即可。

### Step 3：MemoryRecall 召回加权

修改 `agent/memory/topic_backend.go`：

**数据加载**：`Recall()` 开始时加载对应域的 `last_dream.json`（已知道 global 和 project 路径）。

**评分改造**，当前公式：

```
score = 0.5 + titleMatch(0.2) + keywordMatch(0.2) - superseded(0.3) + recency(0.1)
```

改为加入衰减因子：

```
score = (0.5 + titleMatch(0.2) + keywordMatch(0.2) - superseded(0.3) + recency(0.1))
      × decayMultiplier
```

其中 `decayMultiplier = 0.3 + 0.7 × factDecay`（最低 0.3 保底）。

**强化回写**：新增 `ReinforceFact(ctx, entryID) error`：

```go
// agent/memory/memory.go — Backend 接口扩展
ReinforceFact(ctx context.Context, entryID string) error
```

TopicBackend 实现：加载 `last_dream.json` → 找到对应 `FactState` → `Reinforcements++` / `LastReinforced=now` / `Decay=1.0` → 写回。

### Step 4：召回后强化触发

**注入点**：`MemoryRecallReminder`（自动）和 `MemoryRecallTool`（LLM 主动调用）返回结果后。

在 `agent/systemreminder/memory_reminder.go` 里，`Generate()` 返回结果后调用 `r.Backend.ReinforceFact()` 对每条命中 fact 做强化。

### Step 5：Dream 上下文增强

修改 `dream/prompt.go` — `BuildPrompt()` 注入衰减快照：

```
## Fact Decay Snapshot

The following facts have decay below 0.3 and may need review:
- topic:deploy.md:abc123 — decay: 0.15, reinforcements: 0 (last touched 2026-05-01)
- topic:config.md:def456  — decay: 0.22, superseded, reinforcements: 1

These facts are "fresh" (decay ≥ 0.8) and should be preserved:
- ...
```

帮助 LLM 子代理在 consolidate/prune 阶段做出更好的决策。

## 涉及文件一览

| 文件 | 改动类型 | 预估行数 |
|------|---------|---------|
| `dream/decay.go` | **新增** | ~120 |
| `dream/decay_test.go` | **新增** | ~80 |
| `dream/dream.go` | 扩展 State struct | ~15 |
| `dream/runner.go` | RunDream 后处理接入 | ~20 |
| `agent/memory/topic_backend.go` | 衰减加载 + 加权 + ReinforceFact | ~60 |
| `agent/memory/memory.go` | Backend 接口扩展 | ~5 |
| `agent/systemreminder/memory_reminder.go` | 召回后强化 | ~10 |
| `agent/tools/memory_recall.go` | 召回后强化 | ~10 |
| `dream/prompt.go` | BuildPrompt 衰减上下文 | ~20 |
| `agent/memory/topic_backend_test.go` | 衰减加权测试 | ~40 |

**总计：约 380 行改动，10 个文件（2 新增 + 8 修改）**

## 不改变的部分

- Topic 文件格式不变（仍然是 markdown `---` 分隔的 fact 块）
- `last_dream.json` 向后兼容（新字段有 omitempty）
- TUI 无改动
- 用户配置无改动（半衰期先硬编码 7 天，后续可加 config）
- Dream 的 LLM prompt 结构不变（仅追加衰减快照上下文）

## 风险与取舍

| 风险 | 缓解 |
|------|------|
| `last_dream.json` 被多个进程读写（dream goroutine + MemoryRecall） | 文件级锁已存在（`dream.lock`），只需确保读取时处理并发 |
| 衰减半衰期 7 天可能不适用于所有场景 | 先硬编码观察效果，后续可配 config |
| SHA-256 取前 8 位碰撞 | fact 数量远小于碰撞阈值，且跨文件隔离（topic 前缀） |
| 强化回写增加一次文件 I/O | 只在召回后写，频率低；可批量去抖 |
