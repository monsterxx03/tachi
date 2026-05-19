# MCP ToolSearch：渐进式工具发现机制

> 版本: 1.0 | 日期: 2026-05-19 | 状态: 设计阶段

## 一、问题

### 现状

当前所有 MCP server 在启动时通过 `ListTools()` 一次性发现全部工具，注册到 `tool.Registry`，然后**全部**通过 `buildLLMTools()` 转换成 `llm.Tool` 并在每一轮 LLM API 调用中发送。

```
AIAgent.Configure()
  └─ ConnectAll() → ListTools() × N server → 注册 N×M 个 tool

runAgentLoop()
  └─ buildLLMTools(GetSchemas())  ← 一次性转换所有工具
  └─ for {
       CreateChatStream(..., llmTools)  ← 每次都传全部工具
     }
```

### 问题

| 场景 | MCP 工具数 | 每轮消耗的 schema tokens |
|------|-----------|------------------------|
| 1 个 server（如 filesystem） | ~10 | ~2K-3K |
| 3-5 个 server | ~30-50 | ~7K-15K |
| 10+ 个 server | 100+ | ~25K+ |

这些 tokens 大部分浪费了——LLM 在一轮对话中通常只需要 1-3 个 MCP 工具。

### 约束

1. **Prompt Cache 不可忽视**：Anthropic/OpenAI 都对工具定义区域做 prompt caching。每轮动态筛选会导致 cache 频繁失效，成本反而更高。
2. **工具必须能被发现**：LLM 不知道它不知道的工具。不能让工具对 LLM 完全不可见。
3. **不依赖任何 Provider 专有 API**：项目中的 Anthropic API client 可能指向第三方兼容实现（如 proxy/self-hosted）。不能使用 `defer_loading`、`tool_reference` 等 Anthropic 专有 API 特性。**整个机制必须纯应用层实现**。

## 二、设计思路

借鉴 Claude Code 的 `ToolSearchTool` 模式——**不是筛选已有工具，而是按需发现工具**。

### 核心原则

1. **启动最小化**：初始只发送核心内置工具 + 一个 `mcp_tool_search` 工具
2. **工具目录可见**：通过 system reminder 告知 LLM 所有可用工具的**名称+一句话描述**（无 schema）
3. **按需加载**：LLM 通过 `mcp_tool_search` 发现并加载具体工具的完整 schema。搜索工具以**纯文本 JSON** 格式返回参数定义，没有任何 Provider 特定的编码（如 `tool_reference`）。
4. **单调增长**：一旦被发现的工具就永久加入活跃集合，不缩小、不 churn
5. **Cache 友好**：活跃工具集稳定增长，每个会话只失效 cache 1-3 次

## 三、总体架构

```
┌───────────────────────────────────────────────────────────┐
│                   Tool Registry                            │
│                                                           │
│  ┌─────────────────────────────────────┐                  │
│  │  始终发送 (always-load set)          │                  │
│  │  ├─ Bash, Read, Write, Edit        │  ← 内置核心       │
│  │  ├─ Glob, Grep, WebSearch, WebFetch│  ← 内置核心       │
│  │  ├─ AskUser, SubAgent, RecordMemory│  ← 内置核心       │
│  │  └─ mcp_tool_search                │  ← 新增搜索工具   │
│  └─────────────────────────────────────┘                  │
│                                                           │
│  ┌─────────────────────────────────────┐                  │
│  │  延迟池 (deferred pool)             │                  │
│  │  ├─ mcp__postgres__query + 完整schema│  ← 不发送给 LLM │
│  │  ├─ mcp__filesystem__read + schema  │  ← 不发送给 LLM │
│  │  └─ mcp__github__* + schema         │  ← 仅存于内存   │
│  │  来源: ConnectAll() 时 ListTools()   │                  │
│  └─────────────────────────────────────┘                  │
│                                                           │
│  ┌─────────────────────────────────────┐                  │
│  │  已发现集 (discovered set)           │                  │
│  │  ├─ (初始为空)                       │                  │
│  │  ├─ mcp_tool_search 返回后加入       │                  │
│  │  └─ 单调增长，从不缩小                │                  │
│  └─────────────────────────────────────┘                  │
└───────────────────────────────────────────────────────────┘
```

### 数据流

```
Turn 1:
  LLM API ← [Bash, Read, Edit, ..., mcp_tool_search]
             ↑ 仅 ~10 个工具，cache 写入小
  System: <available-deferred-tools>
          mcp__postgres__query — query PostgreSQL database
          mcp__postgres__list_tables — list database tables
          mcp__filesystem__read — read files from remote filesystem
          mcp__filesystem__write — write files to remote filesystem
          mcp__github__create_pr — create pull requests on GitHub
          ...

  LLM: "我要查一下 users 表"
     → 看到 mcp__postgres__query 在列表中
     → 调用 mcp_tool_search(query="postgres query")
  
  mcp_tool_search 返回:
    {matches: ["mcp__postgres__query", "mcp__postgres__list_tables"],
     schemas: { "mcp__postgres__query": {name, description, parameters...},
                "mcp__postgres__list_tables": {...} }}

  发现集 ← ["mcp__postgres__query", "mcp__postgres__list_tables"]

Turn 2:
  LLM API ← [Bash, ..., mcp_tool_search,
             mcp__postgres__query,           ← 新加入
             mcp__postgres__list_tables]     ← 新加入
             ↑ cache 失效一次，写入新 cache（~12 个工具）
  
  LLM: "查一下 users 表"
     → 调用 mcp__postgres__query(sql="SELECT * FROM users")

Turn 3+:
  LLM API ← [同上 12 个工具]
             ↑ cache 全部命中
```

## 四、组件设计

### 4.1 延迟池（Deferred Pool）

**位置**: `agent/mcp/deferred_pool.go`

一个内存中的 map，存储所有 MCP 工具的完整信息（包括 schema）但不注册到 LLM 工具列表。

```go
type DeferredTool struct {
    Name        string            // "mcp__postgres__query"
    ServerName  string            // "postgres"
    Description string            // 原始 description
    Schema      tools.Schema      // 完整参数 schema
    SearchHint  string            // 搜索关键词提示（从 server 配置或自动生成）
}

type DeferredPool struct {
    mu    sync.RWMutex
    tools map[string]*DeferredTool  // key: tool name
}
```

**初始化**: 在 `ConnectAll()` 中，不直接注册为 tool，而是存入 `DeferredPool`：

```go
// 当前代码（简化）：
for _, t := range mcpTools {
    a.RegisterTool(t)  // ← 直接注册到 LLM 可见的 registry
}

// 新代码：
pool := NewDeferredPool()
for _, t := range mcpTools {
    pool.Add(t)  // ← 存入延迟池，LLM 不可见
}
```

### 4.2 MCP ToolSearch 工具

**位置**: `agent/tools/mcp_tool_search.go`

注册为一个内置工具，**不进入**延迟池。

```go
{
    Name: "mcp_tool_search",
    Description: "搜索并加载 MCP 工具。使用关键词搜索或精确名称选择。" +
        "返回匹配工具的完整 JSON Schema，之后即可像内置工具一样调用。",
    Parameters: {
        "query": {
            Type: "string",
            Description: "搜索关键词，或 \"select:ToolName1,ToolName2\" 精确选择。" +
                "关键词会匹配工具名称和 server 名称，支持 \"+server term\" 语法" +
                "（+ 开头表示必需匹配该 server）",
        },
        "max_results": {
            Type: "number",
            Description: "最大返回结果数（默认 5，最大 20）",
        },
    },
    Execute: func(ctx, args) -> {
        // 1. 解析 query
        // 2. 搜索延迟池
        // 3. 将匹配工具从 deferred → discovered 迁移
        // 4. 返回匹配工具的完整 schema
    },
}
```

#### 搜索算法

借鉴 Claude Code 的三层搜索：

```
1. 精确名称匹配（fast path）
   query == "mcp__postgres__query" → 直接返回

2. 精确选择（select: 前缀）
   query == "select:mcp__postgres__query,mcp__filesystem__read"
   → 按名称精确查找，不执行关键词搜索

3. 关键词搜索
   query == "postgres query"
   → 分词，按权重评分:
     - 工具名中包含完整词: +10
     - 工具名中包含子串: +5
     - SearchHint 匹配: +4
     - Description 匹配: +2
   + 语法: 前置 + 表示 must-have（+postgres query → 必须匹配 postgres）
```

#### SearchHint 的来源

- **MCP 标准**: 读取 MCP tool 的 `_meta.anthropic/searchHint` 字段（Claude Code 的 MCP 扩展标准）
- **自动生成**: 从 `serverName + toolName + description` 提取关键词
- **配置覆盖**: 用户可在 config.yaml 中指定

### 4.3 活跃工具过滤器

**位置**: `agent/agent.go`，在 `buildLLMTools` 之前

```go
func (a *AIAgent) filterActiveSchemas(schemas []tools.Schema) []tools.Schema {
    var active []tools.Schema
    for _, s := range schemas {
        name := s.Name
        switch {
        case !isMCPSchema(name):
            // 内置工具永远发送
            active = append(active, s)
        case a.discoveredSet.Contains(name):
            // 已发现的 MCP 工具发送
            active = append(active, s)
        case name == MCPToolSearchName:
            // 搜索工具本身永远发送
            active = append(active, s)
        default:
            // 未发现的 MCP 工具跳过
        }
    }
    return active
}
```

**关键变更点**: 在 `runAgentLoop()` 中把 `buildLLMTools` 的调用时机从**循环外**移到**循环内**，但 `llmTools` 在每轮之间是**单调增长**的（只增不减）：

```go
func (a *AIAgent) runAgentLoop(...) {
    // 不再在这里固定计算 llmTools
    
    for {
        // 每轮重新计算，但从 deferred pool + discovered set 构建
        schemas := a.filterActiveSchemas(a.toolRegistry.GetSchemas())
        llmTools := buildLLMTools(schemas)
        
        streamCh, err := provider.CreateChatStream(ctx, messages, llmTools, opts)
        // ...
    }
}
```

由于 `filterActiveSchemas` 只返回 always-load + discovered set，且 discovered set 只增不减，所以 `llmTools` 也是单调增长的——cache 失效次数 = 发现新工具的次数。

### 4.4 已发现集（Discovered Set）

**位置**: `agent/mcp/discovered_set.go`

```go
type DiscoveredSet struct {
    mu     sync.RWMutex
    names  map[string]bool  // 已发现的工具名
}

// Add 添加工具到已发现集。幂等操作。
func (s *DiscoveredSet) Add(name string)

// Contains 检查工具是否已发现。
func (s *DiscoveredSet) Contains(name string) bool

// List 返回所有已发现工具的副本。
func (s *DiscoveredSet) List() []string
```

**持久化**: 已发现集记录在 session 的 `meta.json` 中，用于 Resume 时恢复：

```json
{
    "id": "session-xxx",
    "discovered_tools": [
        "mcp__postgres__query",
        "mcp__postgres__list_tables"
    ]
}
```

这样 Resume 后不需要重新搜索，工具集直接从断点恢复。

#### 4.4.1 Compact 时的降级策略

当会话执行 `/compact`（上下文压缩）时，已发现集记录在 session meta 中，不受 compact 影响。

两种策略可选：

**策略 A（推荐）**：Compact 时保留已发现集的 snapshot，写入 compact boundary marker。恢复时从 marker 读取。

```json
// compact boundary marker 中携带
{
    "type": "compact_boundary",
    "discovered_tools": ["mcp__postgres__query", "mcp__postgres__list_tables"]
}
```

**策略 B（兜底）**：Compact 后清空已发现集。LLM 如需要可重新调用 `mcp_tool_search`。虽然多一次 round-trip，但不受 compact 影响。

### 4.5 `<available-deferred-tools>` 系统提醒

**位置**: 扩展 `agent/systemreminder/`，新增 `DeferredToolReminder`

每条用户消息前注入一个 `<available-deferred-tools>` 块：

```
<available-deferred-tools>
mcp__postgres__query — query PostgreSQL database
mcp__postgres__list_tables — list tables in database
mcp__filesystem__read — read files from remote filesystem
mcp__github__create_pr — create pull requests on GitHub
(共 23 个 MCP 工具可用。使用 mcp_tool_search 搜索并加载。)
</available-deferred-tools>
```

**设计要点**:
- 只显示**未发现**的工具（已发现的已有完整 schema，不需要再列在目录中）
- 每行格式: `tool_name — one_line_description`
- 末尾提示如何搜索
- 使用 `Collector.Collect()` 机制注入（和 DateReminder、GitReminder 相同）
- 成本: 假设 30 个 MCP 工具，每行 ~60 chars，共 ~1.8K chars ≈ ~500 tokens，比发送全部 schema (~15K tokens) 节省 97%
- Cache 行为：当工具集稳定时（无新增/移除 server），这块内容不变 → **cache 稳定**

**为什么放在 user 消息而非 system prompt？** 和现有 reminder 机制一致，system prompt 用于全局不变的上下文（角色、行为约束），而可用工具列表可能随 MCP server 连接状态变化。

### 4.6 MCP 连接动画处理

当某些 MCP server 还在连接中（慢启动 / OAuth 认证中），延迟池尚未完全填充。

**处理方式**:

1. `mcp_tool_search.Execute()` 返回 `pending_servers: ["slack", "github"]` 字段
2. `<available-deferred-tools>` 末尾添加 `⚠ 以下 server 正在连接: slack, github`
3. LLM 看到后可以选择等待或先使用已就绪的工具

## 五、Provider 兼容性：纯应用层实现

### 核心原则：同一套代码，所有 Provider 行为一致

**本方案不依赖任何 Provider 的专有 API 特性。** 所有 Provider（OpenAI、Anthropic 及第三方兼容实现）收到的请求格式完全相同，差异仅在于标准的 tool encoding 方式。

```
同一套 filterActiveSchemas() 逻辑
       ↓
同意套 buildLLMTools() 转换
       ↓
OpenAI:  → convertTools() → openai.Tool  (不带 API 特殊标记)
Anthropic: → buildRequest() → anthropic.ToolUnionParam  (不带 defer_loading)
第三方:  → 各 provider 的标准编码
```

### 关键设计点

**1. 不在 API 调用中标记 "deferred"**

某些 provider 支持在 tool 定义中标记 `defer_loading: true`，让工具名可见但 schema 不可见。本项目**不使用**这个特性。代替方案是：直接在 API 调用中**不包含**被 deferred 的 tool。LLM 根本不知道这些工具的存在——直到 `<available-deferred-tools>` 系统提醒告诉它。

**2. 搜索工具返回纯文本 JSON**

当 LLM 调用 `mcp_tool_search` 时，结果格式是普通文本：

```
配合工具的完整 JSON Schema：
mcp__postgres__query:
  {
    "description": "Execute SQL query against PostgreSQL database",
    "input_schema": {
      "type": "object",
      "properties": {
        "connection_string": { "type": "string" },
        "sql": { "type": "string" },
        "params": { "type": "array", "items": { "type": "string" } }
      },
      "required": ["connection_string", "sql"]
    }
  }
```

LLM 阅读这段文本后，就知道该工具的完整参数结构。**没有任何 `tool_reference` 块的参与。**

**3. 后续调用走标准 tool encoding**

工具一旦被发现，就作为标准 tool 加入后续 API 调用。各 provider 使用其标准的 tool schema 编码方式（OpenAI 用 `function` 对象，Anthropic 用 `tool` 对象），和项目中现有的编码逻辑完全一致。不需要任何 provider-specific 的处理。

```
Turn 1 API call:
  tools: [Bash, Read, Edit, ..., mcp_tool_search]
         ↑ 所有工具都是标准 encoding，无特殊标记

Turn 2+ API call (after discovery):
  tools: [Bash, Read, Edit, ..., mcp_tool_search,
          mcp__postgres__query,           ← 标准 encoding
          mcp__postgres__list_tables]     ← 标准 encoding
```

### 总结

| 能力 | 依赖 Provider API？ | 实现方式 |
|------|-------------------|---------|
| 工具不暴露给 LLM | 否 | 不在 API 调用中包含该 tool 即可 |
| 工具目录可见 | 否 | `user` 消息中的纯文本列表 |
| ToolSearch 返回 schema | 否 | 纯文本 JSON |
| 工具加入活跃集 | 否 | 后续 API 调用包含标准 tool encoding |

所有 Provider 通过这些纯应用层手段获得完全一致的行为。**不存在 Anthropic 能用但 OpenAI 不能用的功能差异。**

## 六、配置扩展

### 6.1 新增配置字段

```yaml
mcp_servers:
  - name: postgres
    url: http://localhost:3000/mcp
    # 可选：始终加载某些关键工具（绕过 ToolSearch）
    always_load_tools:
      - query
      - list_tables
    # 可选：自定义 search hint（覆盖自动生成）
    search_hints:
      query: "execute SQL queries against PostgreSQL"
      list_tables: "list database tables and views"
```

**`always_load_tools`**: 某些核心工具可能需要直接从 Turn 1 就可用（比如文件系统的 `read`/`write`），不需要经过 ToolSearch。这些工具直接注册到 always-load set。

### 6.2 Tool Search 的启用开关

```yaml
# 在 config 顶层
mcp_tool_search:
  enabled: true                    # 默认启用
  max_deferred_schema_chars: 50000 # 单个 deferred tool schema 的最大长度（超长截断）
  min_tools_for_search: 5          # 当 MCP 工具数 < 5 时，直接用全量发送（省 round-trip）
```

**`min_tools_for_search`**: 当 MCP 工具很少时（比如只有 3 个），不需要 ToolSearch，直接全量发送更简单。这个阈值让系统自动选择最优策略。

## 七、自动降级策略

根据 MCP 工具数量自动选择策略，无需用户配置：

| MCP 工具数 | 策略 | 理由 |
|-----------|------|------|
| 0-5 | 全量发送 | 工具少，多一轮 ToolSearch 划不来 |
| 5-30 | ToolSearch | schema 节省显著，round-trip 合理 |
| 30+ | ToolSearch + 截断 | schema 太大需要限制每次搜索返回量 |

```go
func selectMCPSchemaStrategy(pool *DeferredPool) Strategy {
    n := pool.Len()
    switch {
    case n <= 5:
        return StrategyAlwaysLoad      // 全量发送，不需要 ToolSearch
    case n <= 30:
        return StrategyToolSearch      // 标准 ToolSearch 模式
    default:
        return StrategyToolSearchWithLimit  // ToolSearch + max_results=10
    }
}
```

## 八、与现有机制的集成

### 8.1 与 SubAgent 的交互

SubAgent 目前不继承主 agent 的 MCP 工具。保持这一行为——SubAgent 不自动看到 MCP 工具。如果 SubAgent 任务需要 MCP 工具，由主 agent 通过 `SubAgent` tool 的 prompt 描述来间接访问。

未来可考虑让 SubAgent 也能使用已发现的 MCP 工具，但第一版不做。

### 8.2 与 Channel 模式的交互

Channel 模式（微信、Slack 等）同样适用 ToolSearch。Channel 中 LLM 也能看到 `<available-deferred-tools>` 并调用 `mcp_tool_search`。与 TUI 模式无区别。

### 8.3 与 Resume 的交互

Resume 时从 session meta 恢复已发现集，直接进入活跃工具列表。LLM 不需要重新搜索。

### 8.4 与 `/compact` 的交互

参见 4.4.1 节的 compact 降级策略。第一版优先实现策略 B（兜底），策略 A 作为优化后续添加。

### 8.5 与 System Reminder 的交互

`DeferredToolReminder` 作为 `Collector` 的一部分，在每轮注入 `user` 消息。和现有的 `DateReminder`、`GitReminder`、`IterationWarningReminder` 等使用相同的注入机制。

### 8.6 与 Memory 存储的交互

`<available-deferred-tools>` 块会被注入到 `user` 消息中。如果这一轮被 mem9 自动存储（`StoreScopeTurn`），这个块会被 `stripNoiseTags()` 过滤掉，不会写入记忆。

需要在 `agent/memory/mem9.go` 的 `noiseTags` 列表中添加 `"<available-deferred-tools>"` 条目，确保该标签在存储前被剥离：

```go
var noiseTags = []string{
    "<local-command-caveat>",
    // ... 已有标签 ...
    "<available-deferred-tools>",  // 新增
}
```

这样 `<available-deferred-tools>...</available-deferred-tools>` 块的内容不会作为记忆内容持久化，避免污染语义检索结果。

## 九、实现步骤

### Phase 1: 基础设施（2-3 天）

1. 创建 `agent/mcp/deferred_pool.go` — 延迟池数据结构
2. 创建 `agent/mcp/discovered_set.go` — 已发现集
3. 修改 `agent/mcp/manager.go` `ConnectAll()` — 将 MCP 工具存入延迟池而非直接注册
4. 添加 `filterActiveSchemas()` — 在 `runAgentLoop` 中过滤工具
5. 将 `buildLLMTools` 从循环外移到循环内

### Phase 2: 搜索工具（2-3 天）

1. 创建 `agent/tools/mcp_tool_search.go` — 搜索工具实现
2. 实现搜索算法（精确匹配 + 关键词搜索 + select: 语法）
3. 实现 `Execute` 中将匹配工具从 deferred 迁移到 discovered
4. 添加单元测试

### Phase 3: 系统提醒（1 天）

1. 创建 `agent/systemreminder/deferred_tool.go` — `DeferredToolReminder`
2. 实现 `<available-deferred-tools>` 格式化输出
3. 注册到 `systemreminder.Collector`

### Phase 4: 持久化与恢复（1 天）

1. 扩展 `session/meta.json` 结构，增加 `discovered_tools` 字段
2. Resume 时恢复已发现集
3. 处理 `/compact` 后的降级

### Phase 5: 配置与优化（1-2 天）

1. 配置项：`always_load_tools`、`search_hints`、`min_tools_for_search`
2. 自动降级策略
3. 性能测试和调优
4. 文档更新

## 十、风险和注意事项

### 风险

1. **LLM 不理解 ToolSearch**：老模型或弱模型可能不会主动调用 `mcp_tool_search`。解决：`<available-deferred-tools>` 底部明确提示"使用 mcp_tool_search 加载工具"。

2. **额外的 round-trip**：启动时 LLM 至少需要一次 ToolSearch 调用来获取第一个工具。这在大多数场景下是可接受的（一次额外调用的延迟 < 1s）。

3. **MCP server 动态增删**：若用户在运行时通过 `/mcp toggle` 增减 server，延迟池会变化，`<available-deferred-tools>` 内容变更导致 cache 失效。这是合理的——server 变更本身就应触发 cache 刷新。

4. **超长 schema**：某些 MCP 工具的 input schema 可能非常大（如包含大量枚举值）。考虑限制单个 deferred schema 的存储大小，超长时截断或摘要。

### 已知限制

- SubAgent 在第一版中不支持 MCP 工具
- `/compact` 后的首次调用可能需要重新搜索（取决于降级策略选择）

## 十一、参考

- [Claude Code ToolSearchTool 实现](https://github.com/anthropics/claude-code) - `src/tools/ToolSearchTool/ToolSearchTool.ts`
- Tachi 现有 MCP 集成: `agent/mcp/`
- Tachi 现有 System Reminder: `agent/systemreminder/`
- Tachi Tool Registry: `agent/tools/tool.go`
