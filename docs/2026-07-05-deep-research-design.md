# Deep Research 功能设计文档

> 在 Tachi 中内置深度研究（Deep Research）能力的设计方案。
>
> **设计哲学**：DeepResearchTool 是轻量协调器，不做具体搜索提取工作。实际的研究任务（搜索→阅读→提取）通过 SubAgent 并行执行，DeepResearchTool 只负责编排流程和管理上下文。

---

## 一、背景与目标

### 现状

Tachi 已具备深度研究所需的所有基础能力：

| 组件 | 用途 | 状态 |
|------|------|------|
| `SubAgentTool` | 并行子任务执行（自带 WebSearch/WebFetch/LLM） | ✅ 现有 |
| `WebSearchTool` | 搜索引擎查询 | ✅ 现有（SubAgent 可调用） |
| `WebFetchTool` | URL 内容抓取 + HTML→Markdown | ✅ 现有（SubAgent 可调用） |
| LLM Provider | 查询生成、内容分析、报告合成 | ✅ 现有 |
| Tool Registry | 工具注册与并行执行 | ✅ 现有 |

**缺失的**是一个编排层，把 SubAgent 组织成多轮搜索的研究流程。用户不需要手动一个个调 SubAgent，而是说一句"研究一下 X"就自动完成。

### 目标

- 用户输入研究主题 → 自动完成多层搜索、阅读、分析、报告生成
- depth（深度）和 breadth（宽度）参数控制研究范围
- 实际搜索提取工作交给 SubAgent，DeepResearchTool 只做流程控制
- 不依赖新外部服务

---

## 二、总体架构

```
┌──────────────────────────────────────────────────────────────┐
│                   DeepResearchTool (轻量协调器)                │
│                                                              │
│  ┌──────────────┐    ┌──────────────────┐   ┌────────────┐  │
│  │ ① 生成搜索   │───→│ ② SubAgent × N  │──→│ ③ 判断    │  │
│  │ 查询 (LLM)   │    │ 并行搜索+提取    │   │ 是否继续   │  │
│  └──────────────┘    └──────────────────┘   └────────────┘  │
│       │                     │                     │         │
│       │                     │               ┌─────┴─────┐  │
│       │                     │               │ depth>0   │  │
│       │                     │               │  → 回到①  │  │
│       │                     │               │ depth=0   │  │
│       │                     │               │  → 写报告  │  │
│       ▼                     ▼               └─────┬─────┘  │
│  ┌──────────────────────────────────────────────────┐      │
│  │ ④ 最终报告 (SubAgent 或直接 LLM)                  │      │
│  └──────────────────────────────────────────────────┘      │
└──────────────────────────────────────────────────────────────┘
           │
           ▼
    LLM / 用户看到报告
```

### 分工

| 层次 | 负责 | 实现 |
|------|------|------|
| **DeepResearchTool** | 流程编排：生成查询、启动 SubAgent、判断递归、写报告 | Go Tool，~200 行 |
| **SubAgent** | 实际干活：搜索 → 抓取 → 提取 learnings | 已有 SubAgentTool |
| **LLM (在 Tool 内)** | 生成搜索查询、提取 learnings 的 prompt | 直接调用 provider |

### 搜索树模型

```
depth=2, breadth=3

                  研究主题
                  /    |     \
                q1    q2     q3          ← 第 1 层 (depth=2, breadth=3)
               / \   / \    / \
             q4  q5 q6 q7 q8  q9         ← 第 2 层 (depth=1, breadth=2)
                                           ← depth=0 → 写报告
```

- 每深入一层，宽度减半（逐步聚焦）
- 每层将上一层的发现作为上下文传给下一层
- 同一层的搜索通过 SubAgent 并行执行

---

## 三、数据流设计

### 3.1 输入

```go
type DeepResearchArgs struct {
    Query   string `json:"query"`             // 研究主题/问题
    Depth   int    `json:"depth"`             // 递归深度 1-4（默认 2）
    Breadth int    `json:"breadth"`           // 每层搜索宽度 2-8（默认 3）
    Format  string `json:"format,omitempty"`  // "report" | "answer"（默认 "report"）
}
```

### 3.2 输出

```markdown
# Deep Research Report: {Topic}
## Executive Summary
...
## Key Findings
...
## Detailed Analysis
...
## Sources
- [{Title}]({url})
```

以 Markdown 文本返回，LLM 可直接展示给用户。

---

## 四、核心算法

### 4.1 主流程

DeepResearchTool 本身不调用 WebSearch/WebFetch，而是把实际工作委托给 SubAgent：

```
function deepResearch(query, depth, breadth, learnings=[], urls=[]):
    ──────────────────────────────────────────
    ① 生成搜索查询（Tool 内直接 LLM 调用）
    ──────────────────────────────────────────
    serpQueries = LLM.generateQueries(
        query=query,
        learnings=learnings,
        numQueries=breadth
    )
    // 返回 [{query, researchGoal}, ...]

    ──────────────────────────────────────────
    ② 并行 SubAgent：搜索 + 提取
    ──────────────────────────────────────────
    // 每个 SubAgent 独立运行，有各自的 context window
    // 工具集: [WebSearch, WebFetch]
    // prompt: "搜索 '{query}'，阅读结果，提取关键发现"
    subAgents = []
    for each serpQuery in serpQueries:
        agent = SubAgent(
            prompt = format_research_prompt(serpQuery),
            allowed_tools = ["WebSearch", "WebFetch"],
            max_iterations = 5
        )
        subAgents.append(agent)
    
    results = await parallel_execute(subAgents)
    // 每个 result: {learnings: [...], followUpQuestions: [...], urls: [...]}

    合并 learnings 和 urls

    ──────────────────────────────────────────
    ③ 判断递归
    ──────────────────────────────────────────
    if depth > 0:
        nextQuery = synthesize(query, results)
        return deepResearch(
            query=nextQuery,
            depth=depth-1,
            breadth=max(2, breadth/2),
            learnings=allLearnings,
            urls=allUrls
        )

    ──────────────────────────────────────────
    ④ 写最终报告（SubAgent 或直接 LLM）
    ──────────────────────────────────────────
    // 方案 A：SubAgent 写报告（独立 context，适合大量 learnings）
    // 方案 B：Tool 内直接 LLM 写报告（共享 tool 的 provider）
    report = LLM.writeReport(query, allLearnings, allUrls)
    return report
```

### 4.2 两种 SubAgent Prompt（英文）

给 SubAgent 的 prompt 使用英文：

#### 搜索+提取 SubAgent

```
You are a research analyst. Your task:
1. Search for: "{query}"
2. Read the search results and linked pages
3. Extract key learnings (factual, detailed, with specific metrics and entities)
4. Suggest follow-up questions for deeper research

Research goal: {researchGoal}

Return your findings as a structured summary with:
- Key learnings (up to 3, concise and information-dense)
- Follow-up questions (up to 3, for deeper research)
- Source URLs visited

Available tools: WebSearch, WebFetch
```

#### 写报告 SubAgent

```
You are a research report writer. Write a comprehensive, well-structured
report in Markdown based on the following research findings.

Research topic: {query}

Findings:
{learnings}

Source URLs:
{urls}

The report should include:
1. Executive Summary
2. Key Findings
3. Detailed Analysis organized by sub-topic
4. Conclusion
5. Sources section with all referenced URLs

Make it detailed but well-organized. Aim for a thorough analysis.
```

### 4.3 Tool 内直接 LLM 调用（查询生成用）

```
System: You are a research query generator. Given a research topic and
existing learnings, generate {breadth} specific, non-overlapping search
engine queries. Each query should target a distinct aspect of the topic.

User:
Research topic: {query}
Existing learnings: {learnings}

Generate {breadth} search queries with a "researchGoal" for each explaining
what this query aims to discover.
```

### 4.4 递归终止条件

```
继续递归的条件（全部满足）：
  ✓ depth > 0
  ✓ learnings 未超过上限（默认 200 条）
  ✓ 至少有一个 SubAgent 返回了新的 learnings

停止递归的条件（任一满足）：
  ✗ depth == 0
  ✗ learnings 已达上限
  ✗ 所有 SubAgent 都无新发现
  ✗ 总耗时超过超时（默认 5 分钟）
```

---

## 五、文件组织

### 新增文件

```
agent/tools/deepresearch.go       # 主实现：DeepResearchTool（轻量协调器）
agent/tools/deepresearch_test.go  # 单元测试
```

### 修改文件

| 文件 | 修改内容 |
|------|---------|
| `agent/tools/tool.go` | 添加 `ToolNameDeepResearch = "DeepResearch"` 常量 |
| `agent/agent_configure.go` | 注册 `DeepResearchTool`（根据配置启用） |
| `config/config.go` | 添加 `DeepResearchConfig` 结构体及默认值 |
| `agent/commands/commands.go` | `Registry` 中添加 `/research` 命令定义 |
| `tui/commands.go` | 注册 `research` handler + `handleResearchCommand()` |
| `channel/manager/commands.go` | `executeSlashCommand` 添加 `research` 分支 |
| `agent/acp/commands.go` | 注册 `handleACPResearch` handler |

### 不修改的文件

| 文件 | 原因 |
|------|------|
| `agent/agent_loop.go` | 工具执行路径不变 |
| `agent/tools/websearch.go` | SubAgent 内部调用，DeepResearchTool 不直接使用 |
| `agent/tools/webfetch.go` | SubAgent 内部调用，DeepResearchTool 不直接使用 |
| `agent/tools/subagent.go` | DeepResearchTool 调用 SubAgent，而非替代它 |

---

## 六、Config 设计

```go
// config/config.go 新增

type DeepResearchConfig struct {
    Enabled        bool          `yaml:"enabled" default:"true"`
    DefaultDepth   int           `yaml:"default_depth" default:"2"`
    DefaultBreadth int           `yaml:"default_breadth" default:"3"`
    MaxDepth       int           `yaml:"max_depth" default:"4"`
    MaxBreadth     int           `yaml:"max_breadth" default:"8"`
    Timeout        time.Duration `yaml:"timeout" default:"5m"`
    MaxLearnings   int           `yaml:"max_learnings" default:"200"`
}
```

```yaml
# config.yaml 示例
deep_research:
  enabled: true
  default_depth: 2
  default_breadth: 3
  max_depth: 4
  max_breadth: 8
  timeout: 5m
```

不再需要 `summarization_model` 和 `report_model`，因为实际提取和报告工作由 SubAgent 完成（SubAgent 使用其自身配置的 provider）。DeepResearchTool 只需要一个轻量模型做查询生成。

---

## 七、工具 Description（LLM 看到的界面）

```
## DeepResearch
Performs in-depth research on a given topic through multi-layer search
and analysis. Returns a structured research report.

How it works:
1. Analyzes the question and generates multiple search directions
2. Spawns parallel sub-agents to search, read, and extract findings
3. Based on findings, decides whether to go deeper
4. Synthesizes a final report with sources

Parameters:
- query (required): Research topic or question
- depth (optional, default 2): Research depth. 1=single pass, 2=follow-up, 3=even deeper
- breadth (optional, default 3): Search breadth per layer. Higher = wider coverage
- format (optional, default "report"): "report"=detailed markdown, "answer"=concise answer

Good for:
- Comprehensive understanding of a complex topic
- Comparing information from multiple sources

Not suitable for:
- Simple fact lookups (use WebSearch instead)
```

---

## 八、Slash Command: `/research`

### 8.1 命令注册

```go
// agent/commands/commands.go
{Name: "research", Description: "Deep research on a topic. Usage: /research <topic>",
 InputHint: "<topic>", Modes: []Mode{ModeTUI, ModeChannel, ModeACP}},
```

### 8.2 三模式 handler

| 模式 | 文件 | 行为 |
|------|------|------|
| **TUI** | `tui/commands.go` | 解析参数 → 无 topic 报错 → 有 topic 发给 LLM |
| **Channel** | `channel/manager/commands.go` | 无 topic 返回错误 → 有 topic 发给 agent turn |
| **ACP** | `agent/acp/commands.go` | 无 topic 报错 → 有 topic 发给 LLM |

### 8.3 与 DeepResearchTool 的关系

```
/research <topic>
  → Handler: "Do deep research on: {topic}"
  → LLM 调用 DeepResearchTool
    → DeepResearchTool 生成查询
    → 启动 SubAgent × N 并行搜索
    → 判断是否递归
    → 返回报告
```

---

## 九、实现路线图

### Phase 1：MVP（~200 行）

单层搜索，depth=1，固定配置。

```
[√] 查询生成（Tool 内 LLM 调用）
[√] 并行 SubAgent 搜索+提取（复用 SubAgentTool）
[√] 报告合成（直接 LLM 调用或 SubAgent）
[√] 基本错误处理
```

### Phase 2：递归（~+100 行）

```
[√] depth/breadth 参数
[√] 递归深入
[√] 终止条件
```

### Phase 3：优化（~+50 行）

```
[√] Config 集成
[√] 超时控制
```

---

## 十、与现有设计的关系

### 与 SubAgent 的关系

这是本方案的核心设计决策：

| 维度 | 原方案（Goroutine Pool） | 现方案（SubAgent） |
|------|------------------------|-------------------|
| 实际搜索提取 | DeepResearchTool 内直接调 WebSearch | **委托给 SubAgent** |
| 并行方式 | Goroutine + semaphore | **SubAgent 天然并行** |
| 上下文管理 | 手动管理 learnings 数组 | **SubAgent 各自独立上下文** |
| Token 开销 | 共享上下文，累加 learnings 不浪费 | **每个 SubAgent 独立上下文，有重复 cost** |
| 代码复杂度 | ~300 行（自己实现搜索+提取） | **~200 行（只做编排）** |
| 测试难度 | 需要 mock WebSearch/WebFetch | **SubAgent 已有测试，Tool 只需测编排** |
| 错误隔离 | 一个 goroutine 挂影响整体 | **SubAgent 各自运行，互不影响** |

**为什么选择 SubAgent 路线：**

1. **职责清晰** — DeepResearchTool 只回答"往哪搜、搜多深"；SubAgent 回答"怎么搜、找到什么"。
2. **复用成熟能力** — SubAgent 已经有迭代预算、tool 权限控制、worktree 隔离、结果截断等能力，DeepResearchTool 不需要重新实现。
3. **实现极简** — 核心代码只需要：生成 query → 启动 SubAgent → 收集结果 → 判断递归。约 200 行 Go 代码。
4. **错误隔离** — 某个 SubAgent 超时或失败不影响其他 SubAgent，深搜路径彼此独立。

**代价：** 每个 SubAgent 有独立的 context window，如果 breadth=5，每层相当于 5 个独立的 LLM 会话，token 消耗比共享上下文方案高。但对于研究任务来说，这个代价可以接受——准确性比 token 成本更重要。

### DeepResearchTool 内部流程图

```
DeepResearchTool.ExecuteContext()
  │
  ├── ① LLM.generateQueries(query, learnings, breadth)
  │     返回 [{query, researchGoal}, ...]
  │
  ├── ② SubAgent.execute(serpQuery)  × breadth 并行
  │     prompt: "Search for '{query}', extract learnings..."
  │     tools: [WebSearch, WebFetch]
  │     ─────────────────────────────
  │     SubAgent 内部:
  │       WebSearch(query) → URLs
  │       WebFetch(url) → content
  │       LLM.extract(content) → {learnings, followUpQuestions}
  │     ─────────────────────────────
  │     返回 learnings + urls
  │
  ├── ③ depth > 0? → 递归：用 followUpQuestions 生成下一层查询
  │   depth = 0? → 继续
  │
  └── ④ 写报告
      方案 A: SubAgent(system="写报告", tools=[]) 独立写
      方案 B: Tool 内 LLM 直接写
```

### 与工具注册机制的关系

DeepResearchTool 遵循标准 `Tool` 接口，在 `agent_configure.go` 中注册：

```go
func (t *DeepResearchTool) Name() string        { return ToolNameDeepResearch }
func (t *DeepResearchTool) Description() string  { return "..." }
func (t *DeepResearchTool) Properties() map[string]PropertySchema { ... }
func (t *DeepResearchTool) Required() []string   { return []string{"query"} }
func (t *DeepResearchTool) Parallel() bool       { return true }
func (t *DeepResearchTool) ExecuteContext(ctx context.Context, args string) (string, error) { ... }
```

---

## 十一、风险与权衡

### 风险

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| SubAgent 独立上下文 → token 消耗高 | 成本 | breadth 默认 3，depth 默认 2，每层最多 3 个 SubAgent |
| SubAgent 超时 | 搜索结果缺失 | 超时的 SubAgent 跳过，不影响整体 |
| 递归深度失控 | 总耗时长 | 硬限制 depth ≤ 4，全局超时 5min |
| 搜索质量不稳定 | 报告质量 | 多层收敛，宽泛→聚焦 |

### 设计取舍

| 决策 | 选择 | 理由 |
|------|------|------|
| 实际搜索提取用 SubAgent 还是直接调 | **SubAgent** | 职责清晰、复用成熟能力、错误隔离 |
| 报告用 SubAgent 写还是直接 LLM 写 | **SubAgent**（大量 learnings 时） | 独立上下文窗口，避免主 agent 上下文膨胀 |
| 查询生成用 Tool 内 LLM 调用还是 SubAgent | **Tool 内直接 LLM** | 轻量节点，不需要 SubAgent 的开销 |
| 轻量模型还是主力模型 | SubAgent 自带 provider 配置 | DeepResearchTool 不需要关心 |

---

## 附录：参考实现

- **[dzhng/deep-research](https://github.com/dzhng/deep-research)** — 极简递归搜索树，~500 行 TypeScript（19.3k ⭐）
  - 核心思想：breadth × depth 搜索树，逐层递进聚焦
  - 本设计借鉴其递归结构

- **[langchain-ai/open_deep_research](https://github.com/langchain-ai/open_deep_research)** — Multi-Agent 架构，基于 LangGraph（11.9k ⭐）
  - 核心思想：Supervisor 分配任务给并行 Researcher 子图
  - 本设计借鉴其"反思后再深入"的模式
  - 本设计中 SubAgent 的角色类似于其 Researcher Subgraph
