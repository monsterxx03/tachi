# agentmemory 集成方案

> 版本: 1.0 | 日期: 2026-05-29 | 状态: 设计阶段
> 关联: [Memory 设计](./2026-05-17-memory.md)
> 关联: [agentmemory GitHub](https://github.com/rohitg00/agentmemory)

---

## 一、概述

### 1.1 为什么是 agentmemory

Tachi 当前已有两个记忆后端：

| 后端 | 类型 | 搜索 | 存储 | 依赖 |
|:-----|:-----|:-----|:-----|:-----|
| `native` | 本地纯文本 | Grep 关键词 | `~/.tachi/memory/log` | 无 |
| `mem9` | 云端向量 | 向量语义搜索 | mem9 API | 网络 + API Key |

**agentmemory 的定位：** 纯 HTTP 方式连接，利用 agentmemory 的 BM25 + 向量 + 知识图谱三重检索能力。与 mem9 不同的核心差异：

| 对比 | mem9 | agentmemory |
|:-----|:-----|:------------|
| 搜索方式 | 向量语义 | BM25 + 向量 + 知识图谱 RRF 融合 |
| 记忆生命周期 | 无 | 4级合并 + Ebbinghaus 遗忘曲线 |
| 检索准确率 | — | **95.2% R@5** (LongMemEval) |
| Token 成本 | API 调用费 | 本地嵌入 = **$0** |
| 记忆管理工具 | 无 | 53 个 MCP 工具 |
| 实时查看器 | ❌ | ✅ (`http://localhost:3113`) |
| 运行方式 | 远程 HTTP API | **本地 HTTP API** (localhost:3111) |

### 1.2 架构

```
┌──────────────────────────────────┐
│           Tachi (Go)              │
│                                   │
│  memory.Backend 接口              │
│    ├── Store()  ────┐             │
│    ├── Recall() ────┤             │
│    └── Forget() ────┘             │
│                      │            │
│  AgentMemoryBackend   │            │
│    (agentmemory.go)   │            │
│                      │            │
└──────────────────────┼────────────┘
                       │ HTTP REST (localhost:3111)
                       ▼
┌──────────────────────────────────┐
│      agentmemory (Node.js)        │
│                                   │
│  REST API (port 3111)             │
│  ├── /agentmemory/health          │
│  ├── /agentmemory/session/start   │
│  ├── /agentmemory/session/end     │
│  ├── /agentmemory/observe         │
│  ├── /agentmemory/remember        │
│  ├── /agentmemory/smart-search    │
│  └── /agentmemory/forget/:id      │
│                                   │
│  iii-engine + SQLite + 本地向量    │
└──────────────────────────────────┘
```

### 1.3 关键设计原则

1. **纯 HTTP，不管理进程生命周期** — Tachi 只通过 HTTP 调用 agentmemory，不负责启动/停止。用户自己 `npx @agentmemory/agentmemory` 或者 Docker 部署
2. **利用现有 `memory.Backend` 接口** — 新增一个 `agentmemory` 后端实现，agent loop 代码一行不改
3. **现有 turn 级 hook 已够用** — 工具执行点 (`tool_executor.go`) 当前没有 memory 集成，但现有的 `storeTurnMemory`（每次回复后）+ `storeCompactMemory`（压缩前）+ `storeSessionMemory`（会话结束）三级写入已经覆盖了主要记忆场景

---

## 二、HTTP 客户端

新建 `agent/memory/agentmemory/client.go`：

```go
package agentmemory

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "time"
)

// 默认地址
const DefaultBaseURL = "http://localhost:3111"

// Client 封装 agentmemory REST API。
type Client struct {
    baseURL    string
    httpClient *http.Client
}

func NewClient(baseURL string, timeout time.Duration) *Client {
    if baseURL == "" {
        baseURL = DefaultBaseURL
    }
    if timeout <= 0 {
        timeout = 10 * time.Second
    }
    return &Client{
        baseURL:    baseURL,
        httpClient: &http.Client{Timeout: timeout},
    }
}

// Health 检查服务是否运行。
func (c *Client) Health(ctx context.Context) bool {
    req, _ := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/agentmemory/health", nil)
    resp, err := c.httpClient.Do(req)
    if err != nil {
        return false
    }
    resp.Body.Close()
    return resp.StatusCode == 200
}

// --- 会话 ---

func (c *Client) StartSession(ctx context.Context, sessionID, projectPath string) error {
    body, _ := json.Marshal(map[string]string{
        "session_id": sessionID, "project_path": projectPath,
    })
    return c.doPost(ctx, "/agentmemory/session/start", body, nil)
}

func (c *Client) EndSession(ctx context.Context, sessionID string) error {
    body, _ := json.Marshal(map[string]string{"session_id": sessionID})
    return c.doPost(ctx, "/agentmemory/session/end", body, nil)
}

// --- 写入 ---

type RememberPayload struct {
    Content   string   `json:"content"`
    Tags      []string `json:"tags,omitempty"`
    SessionID string   `json:"session_id"`
}

func (c *Client) Remember(ctx context.Context, p RememberPayload) error {
    body, _ := json.Marshal(p)
    return c.doPost(ctx, "/agentmemory/remember", body, nil)
}

// --- 检索 ---

type MemoryEntry struct {
    ID        string  `json:"id"`
    Content   string  `json:"content"`
    Score     float64 `json:"score"`
    Timestamp int64   `json:"timestamp"`
    SessionID string  `json:"session_id"`
}

func (c *Client) SmartSearch(ctx context.Context, query string, limit int) ([]MemoryEntry, error) {
    if limit <= 0 {
        limit = 5
    }
    body, _ := json.Marshal(map[string]any{"query": query, "top_k": limit})

    var result struct {
        Results []MemoryEntry `json:"results"`
    }
    if err := c.doPost(ctx, "/agentmemory/smart-search", body, &result); err != nil {
        return nil, err
    }
    return result.Results, nil
}

// --- 删除 ---

func (c *Client) Forget(ctx context.Context, id string) error {
    req, _ := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+"/agentmemory/forget/"+id, nil)
    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    resp.Body.Close()
    return nil
}

// --- 内部 ---

func (c *Client) doPost(ctx context.Context, path string, body []byte, result any) error {
    req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bytes.NewReader(body))
    if err != nil {
        return err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode >= 400 {
        respBody, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("agentmemory: HTTP %d: %s", resp.StatusCode, string(respBody))
    }

    if result != nil {
        return json.NewDecoder(resp.Body).Decode(result)
    }
    return nil
}
```

---

## 三、Backend 实现

新建 `agent/memory/agentmemory_backend.go`：

```go
package memory

import (
    "context"
    "fmt"
    "strings"
    "time"

    "github.com/monsterxx03/tachi/agent/memory/agentmemory"
)

type AgentMemoryBackend struct {
    client *agentmemory.Client
}

func NewAgentMemoryBackend(cfg Config) (*AgentMemoryBackend, error) {
    return &AgentMemoryBackend{
        client: agentmemory.NewClient(cfg.AgentMemory.APIURL, cfg.Timeout),
    }, nil
}

type AgentMemoryConfig struct {
    APIURL string // 默认 http://localhost:3111
}

// Store 实现 Backend 接口。利用 Tachi 已有的三级写入时机。
func (b *AgentMemoryBackend) Store(ctx context.Context, opts StoreOptions) error {
    ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()

    switch opts.Scope {
    case StoreScopeTurn:
        content := formatContent(opts.TurnMessages)
        if content == "" {
            return nil
        }
        return b.client.Remember(ctx, agentmemory.RememberPayload{
            Content:   content,
            Tags:      append(opts.Tags, "turn"),
            SessionID: opts.SessionID,
        })

    case StoreScopeCompact:
        content := formatContent(opts.SessionMessages)
        if content == "" {
            return nil
        }
        return b.client.Remember(ctx, agentmemory.RememberPayload{
            Content:   content,
            Tags:      append(opts.Tags, "compact"),
            SessionID: opts.SessionID,
        })

    case StoreScopeSession:
        return b.client.EndSession(ctx, opts.SessionID)
    }
    return nil
}

// Recall 实现 Backend 接口。BM25 + 向量 + 知识图谱混合检索。
func (b *AgentMemoryBackend) Recall(ctx context.Context, query string, limit int) ([]Entry, error) {
    ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()

    if query == "" {
        return nil, nil
    }

    results, err := b.client.SmartSearch(ctx, query, limit)
    if err != nil {
        return nil, fmt.Errorf("agentmemory recall: %w", err)
    }

    entries := make([]Entry, 0, len(results))
    for _, m := range results {
        entries = append(entries, Entry{
            ID:        m.ID,
            SessionID: m.SessionID,
            Summary:   truncateStr(m.Content, 80),
            Content:   m.Content,
            Score:     m.Score,
            Timestamp: m.Timestamp,
        })
    }
    return entries, nil
}

func (b *AgentMemoryBackend) Forget(ctx context.Context, id string) error {
    ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()
    return b.client.Forget(ctx, id)
}

// formatContent 将 Message 列表格式化为文本。
func formatContent(messages []Message) string {
    if len(messages) == 0 {
        return ""
    }
    var sb strings.Builder
    for _, m := range messages {
        prefix := "User: "
        if m.Role == "assistant" {
            prefix = "Assistant: "
        }
        content := m.Content
        if len(content) > 500 {
            content = content[:500] + "..."
        }
        sb.WriteString(prefix + content + "\n")
    }
    return strings.TrimSpace(sb.String())
}
```

---

## 四、Agent Loop 集成（无需修改现有代码）

### 4.1 当前 tool_executor.go 的 memory 现状

经过代码审查，确认 **`executeToolCallsSequential` 和 `executeToolCallsParallel` 中没有任何 memory 调用**。每个工具执行完后只记录 session（`recordSession`），不调用 `Store`。

但这不是问题——因为现有的 **turn 级 hook** 已经覆盖了记忆持久化需求：

```
执行流程                    memory 写入点
─────────                  ───────────────
用户发消息
  → LLM 回复（含工具调用）   ← 此时不写 memory
    → 执行工具 A             ← 不写 memory (tool_executor.go 没有)
    → 执行工具 B             ← 不写 memory (tool_executor.go 没有)
    → LLM 继续推理
    → 输出最终回复           ← storeTurnMemory() 写 memory (handleFinishReason)
```

只要 agent 完成了整个回复轮次（`finish_reason = "stop"`），`handleFinishReason` 就会调用 `storeTurnMemory()`，然后通过 `Backend.Store(StoreScopeTurn)` 发送到 agentmemory。

如果将来需要**工具调用级**的细粒度记忆（每次工具执行完就记录），可以在 `tool_executor.go` 的工具执行成功后加一行：

```go
// tool_executor.go 中的 executeToolCallsSequential（可选增强）
// tr 拿到后，异步写入 agentmemory
if a.memoryBackend != nil {
    go func() {
        a.memoryBackend.Store(ctx, memory.StoreOptions{
            Scope:     memory.StoreScopeTurn,
            TurnMessages: []memory.Message{{
                Role:    "assistant",
                Content: fmt.Sprintf("Tool %s: %s", tc.Function.Name, tr.Output),
            }},
        })
    }()
}
```

**但这个优化不是必需的。v1 只靠现有的 turn 级 hook 就能正常工作。**

### 4.2 集成总览

Tachi 现有代码中，下面这些 hook 点**不需要修改**，接入 `agentmemory` 后端后自动生效：

| 触发时机 | 调用代码位置 | 调用方法 | 映射到 agentmemory API | 改动量 |
|:---------|:-------------|:---------|:----------------------|:------:|
| 每次 agent 回复后 | `agent_loop.go` → `handleFinishReason("stop")` | `storeTurnMemory()` | `POST /remember` | **0** |
| 上下文压缩前 | TUI 主动调用 | `StoreCompactMemory()` | `POST /remember` | **0** |
| 会话结束时 | TUI 退出前主动调用 | `StoreSessionMemory()` | `POST /session/end` | **0** |
| LLM 显式调用 | `RecordMemory` 工具 | `Backend.Store(DirectContent)` | `POST /remember` | **0** |
| 每次用户消息 | `systemreminder` 框架 | `Backend.Recall()` | `POST /smart-search` | **0** |
| `/forget` 命令 | TUI 命令处理 | `Backend.Forget()` | `DELETE /forget/:id` | **0** |

**真正需要改的只有 3 处，全部在配置/工厂层：**

| # | 文件 | 改动 | 行数 |
|:-:|:-----|:-----|:----:|
| 1 | `agent/memory/agentmemory/client.go` **新建** | HTTP 客户端 | ~120 |
| 2 | `agent/memory/agentmemory_backend.go` **新建** | Backend 实现 | ~100 |
| 3 | `agent/memory/memory.go` | 工厂注册 `case "agentmemory"` | +3 |
| 4 | `config/config.go` | 加 `AgentMemory` 配置结构 | +8 |
| 5 | `agent/agent_configure.go` | 透传 AgentMemory 配置 | +2 |
| **总计** | | | **~233 行** |

---

## 五、配置

### config.yaml

```yaml
memory:
  type: agentmemory                     # 改这一行即可切换
  timeout: "10s"
  agentmemory:
    api_url: "http://localhost:3111"    # agentmemory 服务地址
```

用户只需确保 agentmemory 已在运行：

```bash
# 自己启动，Tachi 不插手
npx @agentmemory/agentmemory &
```

### Go 配置结构

```go
// config/config.go
type MemoryConfig struct {
    Type         string               `yaml:"type"`
    Timeout      string               `yaml:"timeout"`
    ExcludeRepos []string             `yaml:"exclude_repos"`
    Mem9         Mem9SubConfig        `yaml:"mem9"`
    AgentMemory  AgentMemorySubConfig `yaml:"agentmemory"`  // 新增
}

type AgentMemorySubConfig struct {
    APIURL string `yaml:"api_url"`  // 默认 http://localhost:3111
}
```

---

## 六、与现有后端的对比

| 维度 | native | mem9 | agentmemory |
|:-----|:-------|:-----|:------------|
| 搜索方式 | Grep 关键词 | 向量语义 | **BM25 + 向量 + 知识图谱** |
| 外部依赖 | 无 | 网络 + API Key | **无 (本地嵌入)** |
| 记忆生命周期 | 无 | 无 | **4级合并 + 遗忘曲线** |
| 检索准确率 | Grep 级别 | — | **95.2% R@5** |
| Token 成本 | 零 | API 费 | **零** |
| 运行方式 | 进程内 | HTTP API | HTTP API (用户自己起) |
| 管理工具 | 无 | 无 | **53 MCP tools + 实时查看器** |

---

## 七、使用示例

```bash
# 终端 1：启动 agentmemory
npx @agentmemory/agentmemory &

# 终端 2：启动 Tachi（只需改配置）
tachi
```

之后每次对话自动记录、自动召回，不需要任何额外操作。
