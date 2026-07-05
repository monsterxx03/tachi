# Deep Research 功能设计文档

> 在 Tachi 中内置深度研究（Deep Research）能力的设计方案。
>
> **设计哲学**：DeepResearch 是**配置驱动的轻量协调器**，**仅通过 `/research` slash command 启动**，不暴露为 LLM 可见的 Tool。实际的研究任务（搜索→阅读→提取）通过 SubAgent 并行执行，DeepResearch 只负责编排流程和管理上下文。所有可变策略（prompt 模板、模型选择、行为参数）通过 `config.yaml` 配置，不改代码即可调整研究行为。

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

**缺失的**是一个编排层，把 SubAgent 组织成多轮搜索的研究流程。用户不需要手动一个个调 SubAgent，而是打一句 `/research 研究一下 X` 就自动完成。

### 目标

- 用户输入 `/research <topic>` → 自动完成多层搜索、阅读、分析、报告生成
- depth（深度）和 breadth（宽度）参数控制研究范围
- 实际搜索提取工作交给 SubAgent，DeepResearch 引擎只做流程控制
- **不暴露为 Tool**——LLM 不会自主决定调用，始终由用户通过 slash command 触发
- 不依赖新外部服务

---

## 二、总体架构

```
┌──────────────────────────────────────────────────────────────┐
│                   配置层 (config.yaml)                         │
│  prompts / provider 引用 / default_depth / default_breadth ...   │
└──────────────────────────────────────────────────────────────┘
                          │ 读取
                          ▼
┌──────────────────────────────────────────────────────────────┐
│           DeepResearch 引擎 (普通 Go struct，非 Tool)          │
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
│  │ ④ 最终报告 (SubAgent 写 HTML 并保存到 ~/.tachi/research/)       │      │
│  └──────────────────────────────────────────────────┘      │
└──────────────────────────────────────────────────────────────┘
           │
           ▼
    /research handler → 用户看到报告
```

### 分工

| 层次 | 负责 | 实现 |
|------|------|------|
| **配置层** | prompt 模板、provider 引用、参数默认值 | `config.yaml`，用户可自由修改 |
| **DeepResearch 引擎** | 流程编排：生成查询、启动 SubAgent、判断递归、写报告 | 普通 Go struct，~250 行（行为由配置驱动） |
| **SubAgent** | 实际干活：搜索 → 抓取 → 提取 learnings | 已有 SubAgentTool |
| **LLM (在协调器内)** | 生成搜索查询、提取 learnings 的 prompt | 直接调用 provider（使用配置指定的 provider） |

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

### 3.1 slash command 参数

```
/research <topic> [--depth 2] [--breadth 3]
```

由 `/research` handler 解析参数后传给 DeepResearch 引擎。

### 3.2 输出

以自包含 HTML5 格式返回报告，同时自动保存到 `~/.tachi/research/` 目录。
文件名格式：`{日期}_{时间}-{slugified主题}.html`，如 `2026-07-05_2110-ai-application-financial-analysis.html`。

---

## 四、核心算法

### 4.1 主流程

DeepResearch 引擎本身不调用 WebSearch/WebFetch，而是把实际工作委托给 SubAgent。所有 prompt 模板和模型引用从 `config.yaml` 读取：

```
function deepResearch(query, depth, breadth, learnings=[], urls=[]):
    ──────────────────────────────────────────
    ① 生成搜索查询（引擎内直接 LLM 调用）
    ──────────────────────────────────────────
    // 使用 config.deep_research.query_generator 的:
    //   - system prompt
    //   - provider 引用（默认用轻量 provider "fast"）
    serpQueries = LLM.generateQueries(
        query=query,
        learnings=learnings,
        numQueries=breadth
    )
    // 返回 [{query, researchGoal}, ...]

    ──────────────────────────────────────────
    ② 并行 SubAgent：搜索 + 提取
    ──────────────────────────────────────────
    // 使用 config.deep_research.researcher 的:
    //   - system prompt
    //   - tools（默认 [WebSearch, WebFetch]）
    //   - max_iterations（默认 5）
    // 每个 SubAgent 独立运行，有各自的 context window
    subAgents = []
    for each serpQuery in serpQueries:
        agent = SubAgent(
            prompt = config.researcher.prompt.format(serpQuery),
            allowed_tools = config.researcher.tools,
            max_iterations = config.researcher.max_iterations
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
    ④ 写最终报告（SubAgent 写出 HTML 文件）
    ──────────────────────────────────────────
    // 使用一个报告 SubAgent 来写 HTML 报告。
    // SubAgent 的工具集中包含 WriteFile，可以直接将 HTML 写入 outputPath。
    // 成功路径返回 HTML 内容；失败时降级为 buildPartialReport 并保存到同一路径。
    report = writeReportViaSubagent(query, allLearnings, allUrls, outputPath)
    return report
```

### 4.2 SubAgent Prompt（配置可覆盖）

以下为默认 prompt（英文），用户可通过 `config.yaml` 的 `deep_research.prompts` 覆盖：

#### 搜索+提取 SubAgent（`config.deep_research.prompts.researcher`）

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

Available tools: WebSearch, WebFetch, ReadFile, Grep
```

#### 写报告 SubAgent（`config.deep_research.prompts.report_writer`）

```
You are a research report writer. Write a comprehensive, well-structured,
self-contained HTML5 report based on the following research findings.

Research topic: {query}

Findings:
{learnings}

Source URLs:
{urls}

Write a self-contained HTML5 document with inline CSS styling.
Use the WriteFile tool to save it to: {outputPath}

The report should include:
1. Executive Summary
2. Key Findings
3. Detailed Analysis organized by sub-topic
4. Conclusion
5. Sources section with all referenced URLs

Make it detailed but well-organized. Aim for a thorough analysis.
```

### 4.3 引擎内直接 LLM 调用（查询生成用，`config.deep_research.prompts.query_generator`）

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
agent/deepresearch/engine.go    # 主实现：DeepResearch 引擎（普通 Go struct）
agent/deepresearch/engine_test.go  # 单元测试
```

### 修改文件

| 文件 | 修改内容 |
|------|---------|
| `config/config.go` | 添加 `DeepResearchConfig` 结构体及默认值 |
| `agent/commands/commands.go` | `Registry` 中添加 `/research` 命令定义 |
| `tui/commands.go` | 注册 `research` handler → 调用 DeepResearch 引擎 |
| `channel/manager/commands.go` | `executeSlashCommand` 添加 `research` 分支 |
| `agent/acp/commands.go` | 注册 `handleACPResearch` handler |

### 不修改的文件

| 文件 | 原因 |
|------|------|
| `agent/tools/tool.go` | DeepResearch 不是 Tool |
| `agent/agent_configure.go` | DeepResearch 不注册为 Tool |
| `agent/agent_loop.go` | 工具执行路径不变 |
| `agent/tools/websearch.go` | SubAgent 内部调用，DeepResearch 引擎不直接使用 |
| `agent/tools/webfetch.go` | SubAgent 内部调用，DeepResearch 引擎不直接使用 |
| `agent/tools/subagent.go` | DeepResearch 引擎调用 SubAgent，而非替代它 |

---

## 六、Config 设计

### 6.1 配置结构

```go
// config/config.go 新增

type DeepResearchConfig struct {
    DefaultDepth   int           `yaml:"default_depth" default:"2"`
    DefaultBreadth int           `yaml:"default_breadth" default:"3"`
    MaxDepth       int           `yaml:"max_depth" default:"4"`
    MaxBreadth     int           `yaml:"max_breadth" default:"8"`
    Timeout        time.Duration `yaml:"timeout" default:"5m"`
    MaxLearnings   int           `yaml:"max_learnings" default:"200"`

    // provider 引用：query_generator 建议用轻量 provider，避免每次查询生成消耗大量 token。
    // 值引用 config.yaml 中定义的 provider name（如 "fast"、"default"）。
    // 为空时使用 agent 的主 provider。
    QueryGeneratorProvider string `yaml:"query_generator_provider"`

    // prompts：所有 prompt 模板均可自定义，不改代码即可调整研究行为。
    // 为空时使用内置默认值。
    Prompts *DeepResearchPrompts `yaml:"prompts,omitempty"`

    // Researcher 控制搜索研究员 SubAgent 的默认配置
    Researcher *ResearcherConfig `yaml:"researcher,omitempty"`
}

type DeepResearchPrompts struct {
    // QueryGenerator 用于生成搜索查询的 system prompt
    // 模板变量: {breadth}, {query}, {learnings}
    QueryGenerator string `yaml:"query_generator,omitempty"`

    // Researcher 用于搜索+提取的 SubAgent system prompt
    // 模板变量: {query}, {researchGoal}
    Researcher string `yaml:"researcher,omitempty"`

    // ReportWriter 用于写最终报告的 SubAgent system prompt
    // 模板变量: {query}, {learnings}, {urls}, {outputPath}
    ReportWriter string `yaml:"report_writer,omitempty"`
}

type ResearcherConfig struct {
    // AllowedTools 研究员 SubAgent 可用的工具列表
    AllowedTools []string `yaml:"allowed_tools"`

    // MaxIterations 每个研究员 SubAgent 的最大迭代次数
    MaxIterations int `yaml:"max_iterations" default:"5"`
}
```

### 6.2 默认配置

```yaml
# config.yaml 示例
deep_research:
  default_depth: 2
  default_breadth: 3
  max_depth: 4
  max_breadth: 8
  timeout: 5m
  max_learnings: 200

  # 查询生成用轻量 provider（引用 config 中名为 "fast" 的 provider）
  query_generator_provider: fast

  # 所有 prompt 均可自定义，留空则使用 Go 代码中的内置默认值
  prompts:
    query_generator: |
      You are a research query generator...
    researcher: |
      You are a research analyst...
    report_writer: |
      You are a research report writer...

  # 研究员 SubAgent 配置
  researcher:
    allowed_tools: [WebSearch, WebFetch, ReadFile, Grep, WriteFile]
    max_iterations: 5
```

### 6.3 配置驱动的行为

| 场景 | 用户操作 | 效果 |
|------|---------|------|
| 想要更深的研究 | 调大 `default_depth` / `max_depth` | 更多递归层数 |
| 想要更广的覆盖 | 调大 `default_breadth` / `max_breadth` | 每层更多 SubAgent 并行 |
| 想用中文 prompt | 改 `prompts.*` 为中文 | 研究过程中的 LLM 调用使用中文 |
| 想换查询生成 provider | 改 `query_generator_provider` | 查询生成用不同 provider |
| 研究员想看代码 | 改 `researcher.allowed_tools` 加 `ReadFile`、`Grep` | 研究代码库 |
| 输出 HTML 报告 | `report_writer` prompt 模板控制 | 内置默认已生成 HTML5 自包含文档 |

所有配置变更**无需重新编译**，重启 tachi 即可生效。

### 6.4 默认 Prompt 的内置实现

prompt 默认值在 Go 代码中以内置常量的形式存在（而非硬编码在流程逻辑中），
只有用户显式在 YAML 中覆盖时才使用配置值：

```go
const defaultQueryGeneratorPrompt = `You are a research query generator...`
const defaultResearcherPrompt = `You are a research analyst...`
const defaultReportWriterPrompt = `You are a research report writer...`

func (cfg *DeepResearchConfig) QueryGeneratorPrompt() string {
    if cfg.Prompts != nil && cfg.Prompts.QueryGenerator != "" {
        return cfg.Prompts.QueryGenerator
    }
    return defaultQueryGeneratorPrompt
}

// report_writer 不再有 mode 切换——始终通过 SubAgent 写 HTML 报告。
// Prompt 模板变量增加 {outputPath}，SubAgent 通过 WriteFile 写入指定路径。
func (cfg *DeepResearchConfig) ReportWriterPrompt() string {
    if cfg.Prompts != nil && cfg.Prompts.ReportWriter != "" {
        return cfg.Prompts.ReportWriter
    }
    return defaultReportWriterPrompt
}
```

---

## 七、Slash Command: `/research`

> **设计决策**：DeepResearch 不暴露为 Tool，仅通过 `/research` slash command 触发。
> LLM 不会自主调用研究功能——研究行为始终由用户主动发起。

### 7.1 命令注册

```go
// agent/commands/commands.go
{Name: "research", Description: "Deep research on a topic. Usage: /research <topic> [--depth 2] [--breadth 3]",
 InputHint: "<topic>", Modes: []Mode{ModeTUI, ModeChannel, ModeACP}},
```

### 7.2 三模式 handler

| 模式 | 文件 | 行为 |
|------|------|------|
| **TUI** | `tui/commands.go` | 解析参数 → 实例化 DeepResearch 引擎 → 执行研究 → 展示报告 |
| **Channel** | `channel/manager/commands.go` | 解析参数 → 实例化 DeepResearch 引擎 → 执行研究 → 返回报告 |
| **ACP** | `agent/acp/commands.go` | 解析参数 → 实例化 DeepResearch 引擎 → 执行研究 → 流式返回报告 |

### 7.3 参数解析

handler 解析 `/research` 后的参数：

```
/research <topic> [--depth N] [--breadth N]
```

- `topic`（必需）：研究主题
- `--depth`（可选，默认 `default_depth`）：研究深度
- `--breadth`（可选，默认 `default_breadth`）：每层搜索宽度

### 7.4 调用流程

```
/research <topic>
  → handler 解析参数
  → 创建 DeepResearch 引擎（传入 config、provider）
  → 引擎运行：
       ① LLM 生成搜索查询
       ② 并行启动 SubAgent × N 搜索+提取
       ③ 判断是否递归（depth > 0）
       ④ 写最终报告（SubAgent 或直接 LLM）
  → handler 展示报告给用户
```

---

## 八、实现路线图

### Phase 1：MVP（~250 行）

```
[√] Config 结构体定义（DeepResearchConfig + 子结构体）
[√] 默认 prompt 常量（Go 内置 + YAML 覆盖）
[√] 配置读取逻辑（从 config 读取参数/prompts/provider 引用）
[√] 查询生成（引擎内 LLM 调用，使用配置的 prompt 和 provider）
[√] 并行 SubAgent 搜索+提取（复用 SubAgentTool）
[√] 报告合成（SubAgent 写 HTML 并保存到 ~/.tachi/research/）
[√] 基本错误处理
```

### Phase 2：递归（~+100 行）

```
[√] depth/breadth 参数（从 config 读取默认值）
[√] 递归深入（每层从 config 读取 researcher prompt）
[√] 终止条件
[√] 超时控制（从 config 读取 timeout）
```

### Phase 3：优化（~+50 行）

```
[√] /research 三模式 handler
[√] 使用体验打磨
[√] 测试覆盖
```

---

## 九、与现有设计的关系

### 与 SubAgent 的关系

这是本方案的核心设计决策：

| 维度 | 原方案（Goroutine Pool） | 现方案（SubAgent） |
|------|------------------------|-------------------|
| 实际搜索提取 | DeepResearch 引擎内直接调 WebSearch | **委托给 SubAgent** |
| 并行方式 | Goroutine + semaphore | **SubAgent 天然并行** |
| 上下文管理 | 手动管理 learnings 数组 | **SubAgent 各自独立上下文** |
| Token 开销 | 共享上下文，累加 learnings 不浪费 | **每个 SubAgent 独立上下文，有重复 cost** |
| 代码复杂度 | ~300 行（自己实现搜索+提取） | **~250 行（只做编排，配置驱动）** |
| 测试难度 | 需要 mock WebSearch/WebFetch | **SubAgent 已有测试，引擎只需测编排** |
| 错误隔离 | 一个 goroutine 挂影响整体 | **SubAgent 各自运行，互不影响** |

**为什么选择 SubAgent 路线：**

1. **职责清晰** — DeepResearch 引擎只回答"往哪搜、搜多深"；SubAgent 回答"怎么搜、找到什么"。
2. **复用成熟能力** — SubAgent 已经有迭代预算、tool 权限控制、worktree 隔离、结果截断等能力，DeepResearch 引擎不需要重新实现。
3. **实现极简** — 核心代码只需要：读配置 → 生成 query → 启动 SubAgent → 收集结果 → 判断递归。约 250 行 Go 代码。
4. **错误隔离** — 某个 SubAgent 超时或失败不影响其他 SubAgent，深搜路径彼此独立。

**代价：** 每个 SubAgent 有独立的 context window，如果 breadth=5，每层相当于 5 个独立的 LLM 会话，token 消耗比共享上下文方案高。但对于研究任务来说，这个代价可以接受——准确性比 token 成本更重要。

### 配置驱动 vs 代码硬编码

与设计文档原始方案相比，配置驱动的变化：

| 维度 | 纯代码硬编码 | 配置驱动 |
|------|------------|---------|
| prompt 模板 | 写在 Go 代码的字符串里 | YAML 可覆盖，内置 Go 常量作为 fallback |
| provider 选择 | 使用主 provider | 可指定不同环节用不同 provider（如查询生成用轻量 provider） |
| 行为参数 | 代码内常量 | YAML 可配置 |
| 研究策略 | 固定 | YAML 可配置（prompt、depth、breadth、tools 等） |
| 修改成本 | 改代码 → 重新编译 | 改 YAML → 重启即可 |
| 代码量 | ~200 行 | ~250 行（多了配置读取和 fallback 逻辑） |

### Slash Command Only —— 不暴露为 Tool

这是本设计的关键决策：

| 维度 | 暴露为 Tool | 仅 Slash Command（本方案） |
|------|------------|--------------------------|
| 触发方式 | LLM 自主决定调用 | **用户手动触发** |
| 使用场景 | LLM 认为需要研究时自动使用 | **用户主动发起研究任务** |
| 可控性 | LLM 可能在不恰当的时候启动耗时研究 | **用户完全控制何时开始研究** |
| Token 消耗 | LLM 可能反复调用，成本不可控 | **用户每次触发都知情** |
| 实现复杂度 | 需注册 Tool 接口 + 权限管理 | **普通 Go struct，handler 直接调用** |
| 与现有架构耦合 | 需要修改 `agent_configure.go`、`tool.go` | **零耦合，不修改 agent 核心路径** |
| LLM 上下文污染 | Tool description 占用 LLM 上下文窗口 | **LLM 完全不知晓 DeepResearch 存在** |

**选择 Slash Command Only 的理由：**

1. **研究是用户行为，不是 LLM 行为** — 深度研究耗时耗 token，应该由用户主动决定何时进行，而非 LLM 在工具调用中自主触发。
2. **避免 LLM 误用** — 如果暴露为 Tool，LLM 可能在简单问答场景下也调用 DeepResearch（如"今天天气怎么样"），造成不必要的开销。
3. **架构简洁** — 不需要注册 Tool 接口、不需要在 agent_configure.go 中处理、不需要在 tool.go 中添加常量。就是一个纯 Go 的引擎 struct，slash command handler 按需调用。
4. **不污染 LLM 上下文** — Tool description 会占用 LLM 的上下文窗口（即使不被调用也会被读取）。不注册为 Tool 就完全避免了这个问题。

### 引擎内直接 LLM 调用

DeepResearch 引擎在查询生成时需要直接调用 LLM。
这不同于"引擎只做编排"的纯粹理念，但权衡是合理的：

- 查询生成是一个**轻量 LLM 调用**（输入小、输出结构化），不值得为此启动一个 SubAgent
- 报告生成**始终通过 SubAgent** 完成，确保 context window 足够容纳大量 learnings

---

## 十、风险与权衡

### 风险

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| SubAgent 独立上下文 → token 消耗高 | 成本 | breadth 默认 3，depth 默认 2，每层最多 3 个 SubAgent |
| SubAgent 超时 | 搜索结果缺失 | 超时的 SubAgent 跳过，不影响整体 |
| 递归深度失控 | 总耗时长 | 硬限制 depth ≤ 4，全局超时 5min |
| 搜索质量不稳定 | 报告质量 | 多层收敛，宽泛→聚焦 |
| 配置错误（如引用了不存在的 provider） | 引擎初始化失败 | 配置校验 + fallback 到主 provider |
| prompt 模板变量不匹配 | 渲染后 prompt 格式错误 | 变量缺失时保底仍能工作（硬编码占位符替代） |

### 设计取舍

| 决策 | 选择 | 理由 |
|------|------|------|
| 暴露方式 | **仅 Slash Command** | 研究是用户行为，不应由 LLM 自主触发 |
| 实际搜索提取用 SubAgent 还是直接调 | **SubAgent** | 职责清晰、复用成熟能力、错误隔离 |
| 报告用 SubAgent 写还是直接 LLM 写 | **SubAgent（唯一路径）** | 简化实现，确保 context 足够容纳完整报告 |
| 查询生成用引擎内 LLM 调用还是 SubAgent | **引擎内直接 LLM** | 轻量节点，不需要 SubAgent 的开销 |
| prompt 用代码常量还是 YAML 配置 | **代码常量 + YAML 覆盖** | 开箱即用，同时支持自定义 |
| provider 用主 provider 还是可配置 | **可配置**（如 `query_generator_provider: fast`） | 不同环节适合不同 provider |
| 配置校验严格还是宽松 | **宽松（fallback 优先）** | 配置出错时降级而非崩溃 |
| 报告输出格式 | **HTML5（自包含，内嵌 CSS）** | 可读性更好，可直接在浏览器打开 |
| 报告持久化 | **自动保存 ~/.tachi/research/** | 用户可随时回溯历史研究报告 |

---

## 附录：参考实现

- **[dzhng/deep-research](https://github.com/dzhng/deep-research)** — 极简递归搜索树，~500 行 TypeScript（19.3k ⭐）
  - 核心思想：breadth × depth 搜索树，逐层递进聚焦
  - 本设计借鉴其递归结构

- **[langchain-ai/open_deep_research](https://github.com/langchain-ai/open_deep_research)** — Multi-Agent 架构，基于 LangGraph（11.9k ⭐）
  - 核心思想：Supervisor 分配任务给并行 Researcher 子图
  - 本设计借鉴其"反思后再深入"的模式
  - 本设计中 SubAgent 的角色类似于其 Researcher Subgraph
