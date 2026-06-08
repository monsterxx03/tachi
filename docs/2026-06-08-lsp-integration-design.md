# LSP 集成设计方案

## 概述

为 Tachi 接入 Language Server Protocol (LSP) 支持，使 LLM 能够通过工具调用获得代码智能（跳转定义、查找引用、悬停提示等）。LLM 只需要提供文件路径 + 行号 + 列号，不需要理解 LSP 协议细节。

---

## 一、架构总览

```
┌───────────────────────────────────────────────────────────────────┐
│                        agent/lsp/                                 │
│                                                                   │
│  ┌──────────┐    ┌──────────┐    ┌──────────┐   ┌─────────────┐ │
│  │ jsonrpc  │───▶│  server  │───▶│ manager  │──▶│    tool     │ │
│  │(transport)    │(lifecycle)    │ (routing)    │ (interface) │ │
│  └──────────┘    └──────────┘    └──────────┘   └─────────────┘ │
│       │               │              │                              │
│  ┌──────────┐    ┌──────────┐    ┌────────────┐                    │
│  │protocol  │    │file_     │    │ formatters  │                    │
│  │ (types)  │    │tracker   │    │(formatting) │                    │
│  └──────────┘    └──────────┘    └────────────┘                    │
│       │               │              │                              │
│  ┌──────────┐    ┌──────────┐    ┌────────────┐                    │
│  │ config   │    │          │    │  TUI view  │                    │
│  └──────────┘    │          │    └────────────┘                    │
│       │          │          │         │                              │
│       ▼          │          │         ▼                              │
│  config.yaml    │          │    tui/model.go                        │
│                 └──────────┘                                         │
│                   systemreminder                                     │
│                   Collector                                          │
└───────────────────────────────────────────────────────────────────┘
```

---

## 二、文件结构

```
agent/lsp/
├── manager.go       ← LSPManager：全局单例、异步初始化、server 路由
├── server.go        ← LSPServer：进程生命周期、健康检查、请求/通知
├── jsonrpc.go       ← JSON-RPC 2.0 客户端（stdio 传输层）
├── protocol.go      ← LSP 协议类型定义（仅需要的子集）
├── config.go        ← 从 YAML 加载 LSP server 配置
├── tool.go          ← LSPTool：实现 tools.Tool 接口
├── diagnostics.go   ← LSPDiagnosticsTool：诊断信息查询
├── formatters.go    ← 格式化 LSP 结果为 LLM 可读文本
├── file_tracker.go  ← 文件打开状态跟踪（didOpen / didClose）
└── gitignore.go     ← git check-ignore 过滤

agent/systemreminder/
└── lsp_reminder.go  ← LSPStatusReminder：注入 server 状态到 system-reminder
```

**新增/修改的外部文件：**

| 文件 | 变更 |
|------|------|
| `config/config.go` | 新增 `LSPConfig` 字段 |
| `agent/agent_configure.go` | 初始化 LSPManager、注册 LSPTool、注册 Reminder |
| `tui/model.go` | 状态栏显示 LSP 连接状态 |
| `go.mod` | 新增依赖 `go.lsp.dev/jsonrpc2` |

---

## 三、详细设计

### 3.1 配置（config）

在 `~/.tachi/config.yaml` 中新增 `lsp` 节：

```yaml
lsp:
  enabled: true                        # 全局开关
  startup_timeout: 10s                 # 每个 server 初始化超时
  request_timeout: 15s                 # 单次 LSP 请求超时（防止 server 卡死）
  max_restarts: 3                      # 崩溃后最大重启次数
  max_file_size: 10485760              # 10MB，超出不发送 didOpen
  max_results: 50                      # 单次操作返回结果上限（超出截断）
  concurrency_limit: 4                 # 同 server 最大并发请求数
  servers:
    - name: typescript
      command: "typescript-language-server"
      args: ["--stdio"]
      extensions: [".ts", ".tsx", ".js", ".jsx"]
      languages: ["typescript", "javascript"]
      initialization_options:
        hostInfo: "tachi"

    - name: gopls
      command: "gopls"
      args: []
      extensions: [".go"]
      languages: ["go"]
      startup_timeout: 30s             # 可 per-server 覆盖
      settings:                        # 通过 workspace/configuration 返回给 server
        gopls:
          staticcheck: true
```

Go 类型：

```go
type LSPConfig struct {
    Enabled          bool              `yaml:"enabled" default:"true"`
    StartupTimeout   Duration          `yaml:"startup_timeout" default:"10s"`
    RequestTimeout   Duration          `yaml:"request_timeout" default:"15s"`
    MaxRestarts      int               `yaml:"max_restarts" default:"3"`
    MaxFileSize      int64             `yaml:"max_file_size" default:"10485760"`
    MaxResults       int               `yaml:"max_results" default:"50"`
    ConcurrencyLimit int               `yaml:"concurrency_limit" default:"4"`
    Servers          []LSPServerConfig `yaml:"servers"`
}

type LSPServerConfig struct {
    Name               string            `yaml:"name"`
    Command            string            `yaml:"command"`
    Args               []string          `yaml:"args"`
    Extensions         []string          `yaml:"extensions"`
    Languages          []string          `yaml:"languages"`
    InitializationOpts map[string]any    `yaml:"initialization_options"`
    Settings           map[string]any    `yaml:"settings"`           // workspace/configuration 返回值
    Env                map[string]string `yaml:"env"`
    WorkspaceFolder    string            `yaml:"workspace_folder"`
    StartupTimeout     Duration          `yaml:"startup_timeout"`
}
```

---

### 3.2 JSON-RPC 2.0 传输层（jsonrpc.go）

使用 `go.lsp.dev/jsonrpc2` 作为 stdio 传输层，处理双向通信（请求/通知 + server→client 的 request）。

```go
type rpcConn struct {
    conn   *jsonrpc2.Connection
    cancel context.CancelFunc
}

func newRPCConn(ctx context.Context, cmd *exec.Cmd, handler jsonrpc2.Handler) (*rpcConn, error)
func (c *rpcConn) Call(ctx context.Context, method string, params, result any) error
func (c *rpcConn) Notify(ctx context.Context, method string, params any) error
func (c *rpcConn) Close()
```

说明：`go.lsp.dev/jsonrpc2` 成熟且轻量（~500 行），天然支持 stdio reader/writer 和双向 request。如果不想引入外部依赖，也可以手写一个约 300 行的最小 JSON-RPC 2.0 实现（协议本身很简单）。

---

### 3.3 LSP 协议类型（protocol.go）

只定义 LSPTool 需要的类型子集，不引入全部 LSP 规范。约 150 行。

> ⚠️ **编码陷阱**：LSP 协议规定 `Position.Character` 为 **UTF-16 code unit** 偏移。对于 BMP 内字符（包括中文），UTF-16 code unit = 1 个 code point；但 emoji（如 🎉）占 2 个 UTF-16 code units。绝大多数 LSP server（gopls、rust-analyzer、typescript-language-server）实际行为各有不同（有的用 UTF-8 byte offset，有的用 code point），因此 `character` 参数在包含 BMP 外字符时可能存在偏移偏差。
>
> **Phase 1 处理方式**：在 tool description 中提醒 LLM 注意此限制，如果结果位置有偏差尝试 ±1 调整。Phase 3 可加入文件内容感知的 UTF-16 偏移矫正。

```go
// 位置
type Position struct {
    Line      uint `json:"line"`      // 0-based
    Character uint `json:"character"` // 0-based
}
type Range struct {
    Start Position `json:"start"`
    End   Position `json:"end"`
}
type Location struct {
    URI   string `json:"uri"`
    Range Range  `json:"range"`
}
type LocationLink struct {
    OriginSelectionRange *Range `json:"originSelectionRange,omitempty"`
    TargetURI            string `json:"targetUri"`
    TargetRange          Range  `json:"targetRange"`
    TargetSelectionRange Range  `json:"targetSelectionRange"`
}

// Hover
type Hover struct {
    Contents MarkupContent `json:"contents"`
    Range    *Range        `json:"range,omitempty"`
}
type MarkupContent struct {
    Kind  string `json:"kind"`   // "plaintext" | "markdown"
    Value string `json:"value"`
}

// DocumentSymbol（层次结构）
type DocumentSymbol struct {
    Name           string            `json:"name"`
    Detail         string            `json:"detail,omitempty"`
    Kind           SymbolKind        `json:"kind"`
    Range          Range             `json:"range"`
    SelectionRange Range             `json:"selectionRange"`
    Children       []DocumentSymbol  `json:"children,omitempty"`
}

// SymbolInformation（扁平结构，用于 workspace/symbol）
type SymbolInformation struct {
    Name          string     `json:"name"`
    Kind          SymbolKind `json:"kind"`
    Location      Location   `json:"location"`
    ContainerName string     `json:"containerName,omitempty"`
}

// Call Hierarchy
type CallHierarchyItem struct {
    Name         string     `json:"name"`
    Kind         SymbolKind `json:"kind"`
    Detail       string     `json:"detail,omitempty"`
    URI          string     `json:"uri"`
    Range        Range      `json:"range"`
    SelectRange  Range      `json:"selectionRange"`
}
type CallHierarchyIncomingCall struct {
    From       CallHierarchyItem `json:"from"`
    FromRanges []Range           `json:"fromRanges"`
}
type CallHierarchyOutgoingCall struct {
    To         CallHierarchyItem `json:"to"`
    FromRanges []Range           `json:"fromRanges"`
}

// SymbolKind 枚举
type SymbolKind int
const (
    SKFile        SymbolKind = 1
    SKModule      SymbolKind = 2
    SKNamespace   SymbolKind = 3
    SKPackage     SymbolKind = 4
    SKClass       SymbolKind = 5
    SKMethod      SymbolKind = 6
    SKProperty    SymbolKind = 7
    SKField       SymbolKind = 8
    SKConstructor SymbolKind = 9
    SKEnum        SymbolKind = 10
    SKInterface   SymbolKind = 11
    SKFunction    SymbolKind = 12
    SKVariable    SymbolKind = 13
    // ... 其余常量
)
```

---

### 3.4 LSPServer 生命周期（server.go）

状态机：

```
stopped → starting → running
                  ↘ error → starting（最多 maxRestarts 次）
            running → stopping → stopped
```

```go
type LSPServer struct {
    name    string
    config  LSPServerConfig
    state   atomic.Int32           // ServerState enum
    cmd     *exec.Cmd
    conn    *rpcConn
    startAt time.Time
    lastErr error
    crashCount int
    mu      sync.Mutex
    sem     chan struct{}          // 并发控制（容量 = config.ConcurrencyLimit）

    capabilities *ServerCapabilities  // 从 InitializeResult 获取
    openFiles    map[string]struct{}   // URI → isOpen
}
```

**`Start()` 流程：**

1. `exec.LookPath` 检查 command 是否存在（不存在 → 标记 `unavailable`，给出安装提示）
2. Spawn 子进程（`Setpgid: true` 进程组隔离，类比 `ProcessManager`）
3. 将 stderr pipe 到 `~/.tachi/logs/lsp/<name>.log`（环形缓冲，最多 1MB，用于排查崩溃）
4. 等待 `spawn` 确认（避免 ENOENT 异步错误）
5. 创建 `rpcConn`（stdio 双向通信）
6. 发送 `initialize` → 接收 `InitializeResult`，提取 `ServerCapabilities`
7. 发送 `initialized` 通知
8. 注册 `workspace/configuration` handler（返回 `config.Settings`，而非 null——eslint/pyright 等需要 settings）
9. 状态 → `running`

**`Stop()` 流程：**

1. 标记 `isStopping`（防止 crash handler 误报）
2. 发送 `shutdown` 请求
3. 发送 `exit` 通知
4. `conn.Close()` → dispose connection
5. Kill 进程（SIGTERM → 3s → SIGKILL，同 ProcessManager 模式）
6. 清理 `openFiles`

**`Call()` — 请求超时 + ContentModified 指数退避 + 并发控制：**

```go
func (s *LSPServer) Call(ctx context.Context, method string, params, result any) error {
    // 并发控制：获取 semaphore 槽位
    select {
    case s.sem <- struct{}{}:
        defer func() { <-s.sem }()
    case <-ctx.Done():
        return ctx.Err()
    }

    // 单次请求超时
    reqCtx, cancel := context.WithTimeout(ctx, s.config.RequestTimeout)
    defer cancel()

    for attempt := 0; attempt <= 3; attempt++ {
        err := s.conn.Call(reqCtx, method, params, result)
        if err == nil {
            return nil
        }
        // 错误码 -32801 = ContentModified（rust-analyzer 索引中常见）
        if isContentModified(err) && attempt < 3 {
            delay := 500 * time.Millisecond * (1 << attempt)
            time.Sleep(delay) // 500ms → 1s → 2s
            continue
        }
        return err
    }
    return err
}
```

**Crash 恢复：**

- 子进程 exit code ≠ 0 时 → 状态设为 `error`，递增 `crashCount`
- `crashCount > maxRestarts` → 不再尝试重启（防止崩溃 server 无限 fork）
- 每次 `Call()` 前检查健康状态，不健康时自动 `Start()`（如果未超过上限）

---

### 3.5 LSPManager 全局路由（manager.go）

类比 MCP Manager 的异步初始化模式。

```go
type LSPManager struct {
    config     *LSPConfig
    servers    map[string]*LSPServer  // name → Server
    extIndex   map[string]*LSPServer  // ".ts" → Server（小写）
    fileOpened map[string]*LSPServer  // URI → Server
    unavailable map[string]time.Time  // name → 标记时间，30s 内不再重试

    state      atomic.Value   // not-started | pending | success | failed
    initDone   chan struct{}
    initErr    error
    generation atomic.Int64   // 防过期
}

var globalLSP *LSPManager

func InitLSP(ctx context.Context, cfg *LSPConfig) (*LSPManager, error)
func GetLSP() *LSPManager
func IsLSPEnabled() bool
func WaitForInit(ctx context.Context) bool
```

**`InitLSP()` 流程：**

1. 检查 `cfg.Enabled`，跳过如果禁用
2. 遍历所有 server 配置，对每个执行 `exec.LookPath(cmd)`：
   - 未找到 → 标记 `unavailable`（记录时间），跳过该 server
   - 找到 → 构建 `extIndex`（`.ts` → typescript server）
3. **不启动 server**（lazy start），只解析配置→状态设为 `success`
4. 关闭 `initDone` channel

**`GetServer(filePath)` 路由：**

```go
func (m *LSPManager) GetServer(filePath string) *LSPServer {
    ext := strings.ToLower(path.Ext(filePath))
    return m.extIndex[ext]
}
```

**自动 `ensureStarted()`：**

所有对外方法（`Call`、`OpenFile` 等）内部先检查 server 状态，未启动或崩溃时自动启动：

```go
func (m *LSPManager) ensureStarted(ctx context.Context, filePath string) (*LSPServer, error) {
    server := m.GetServer(filePath)
    if server == nil {
        return nil, nil
    }
    if !server.IsHealthy() {
        return server, server.Start(ctx)
    }
    return server, nil
}
```

---

### 3.6 File Tracker（file_tracker.go）

跟踪哪些文件已在 LSP server 上打开，避免重复 didOpen。

```go
type FileTracker struct {
    opened map[string]string  // URI → serverName
    mu     sync.RWMutex
}

func (t *FileTracker) IsOpen(uri string) bool
func (t *FileTracker) MarkOpen(uri, serverName string)
func (t *FileTracker) MarkClosed(uri string)
func (t *FileTracker) Clear()
```

**文件同步时机**：在 Agent 的工具执行后通过 hook 触发。

在 `agent_loop.go` 的 `handleToolCallFinish()` 中的 `executeToolCalls()` 之后：

```go
if a.lspManager != nil {
    for _, result := range results {
        if fp := extractFilePath(result); fp != "" {
            a.lspManager.SyncFile(ctx, fp)
        }
    }
}
```

`SyncFile()` 逻辑：

1. 检查文件是否已在该 server 上打开（`fileTracker.IsOpen(uri)`）
2. 未打开 → 读取文件内容 → 发送 `textDocument/didOpen`
3. 已打开 → 读取最新内容 → 发送 `textDocument/didChange`

`extractFilePath()` 从工具执行结果中提取文件路径（覆盖 `ReadFile`、`WriteFile`、`EditFile`、`Glob` 等工具）。

---

### 3.7 LSPTool（tool.go）

**命名**：`LSP`

**Schema**：

```go
func (t *LSPTool) Properties() map[string]PropertySchema {
    return map[string]PropertySchema{
        "operation": {
            Type: "string",
            Description: "要执行的 LSP 操作",
            Enum: []string{
                "goToDefinition", "findReferences", "hover",
                "documentSymbol", "workspaceSymbol", "goToImplementation",
                "prepareCallHierarchy", "incomingCalls", "outgoingCalls",
            },
        },
        "filePath": {
            Type: "string",
            Description: "文件的绝对或相对路径",
        },
        "line": {
            Type: "integer",
            Description: "行号（1-based，和编辑器显示一致）",
        },
        "character": {
            Type: "integer",
            Description: "列号（1-based，和编辑器显示一致）",
        },
        "query": {
            Type: "string",
            Description: "workspaceSymbol 时的搜索查询（仅该操作需要）",
        },
    }
}
```

**Tool Description 示例**（降低 LLM 使用门槛）：

```
Use the LSP tool for code intelligence operations. Each operation requires
filePath + line + character (except workspaceSymbol which needs query instead).

Examples:
- "goToDefinition": {"operation": "goToDefinition", "filePath": "src/main.go", "line": 42, "character": 10}
- "findReferences": {"operation": "findReferences", "filePath": "src/main.go", "line": 42, "character": 10}
- "hover":        {"operation": "hover", "filePath": "src/main.go", "line": 42, "character": 10}
- "workspaceSymbol": {"operation": "workspaceSymbol", "query": "MyClass"}
- "documentSymbol": {"operation": "documentSymbol", "filePath": "src/main.go"}

Note: line/character are 1-based (same as editor display). Character offset may
drift with non-BMP characters (emoji etc) — if results seem off, try ±1 adjustment.
```

**Parallel**：`true`（LSP 操作是只读的，可以并发）

**`ExecuteContext()` 核心逻辑**：

```
1. 解析入参（operation, filePath, line, character）
2. 等待 LSPManager 初始化完成
3. 通过 ext 查找对应 server
4. 如果未找到 → 返回 "No LSP server for file type: .xxx"
5. 检查 server capabilities 是否支持该操作：
   - goToDefinition     → server.capabilities.DefinitionProvider != nil
   - findReferences     → server.capabilities.ReferencesProvider != nil
   - hover              → server.capabilities.HoverProvider != nil
   - documentSymbol     → server.capabilities.DocumentSymbolProvider != nil
   - workspaceSymbol    → server.capabilities.WorkspaceSymbolProvider != nil
   - goToImplementation → server.capabilities.ImplementationProvider != nil
   - callHierarchy 系列  → server.capabilities.CallHierarchyProvider != nil
   不支持 → 返回 "Operation 'xxx' not supported by <serverName>"
6. SyncFile（首次访问时发送 didOpen，10MB 限制检查）
7. 坐标转换：line-1, character-1（1-based → 0-based）
8. 根据 operation 选择请求方法并发送：
   - goToDefinition     → textDocument/definition
   - findReferences     → textDocument/references（includeDeclaration: true）
   - hover              → textDocument/hover
   - documentSymbol     → textDocument/documentSymbol
   - workspaceSymbol    → workspace/symbol（query 参数）
   - goToImplementation → textDocument/implementation
   - prepareCallHierarchy → textDocument/prepareCallHierarchy
   - incomingCalls      → prepareCallHierarchy + callHierarchy/incomingCalls（两步）
   - outgoingCalls      → prepareCallHierarchy + callHierarchy/outgoingCalls（两步）
9. 过滤 git-ignored 结果（findReferences 等位置列表操作）
10. 截断结果：超过 max_results（默认 50）条时截断，追加 "… and N more results"
    - documentSymbol 层级 > 3 时折叠深层 children，显示 "… N more symbols"
11. 格式化结果 + 统计（resultCount, fileCount, truncated?）
12. 返回 JSON：{operation, result, filePath, resultCount?, fileCount?, truncated?}
```

**Git-ignored 过滤策略**（借鉴 Claude Code 的设计）：

```
对 findReferences / goToDefinition / goToImplementation / workspaceSymbol：
1. 收集所有结果中的 URI → 转换为文件路径
2. 去重文件路径
3. 每 50 个一组调用 `git check-ignore`（5s 超时）
4. 过滤掉被 gitignore 的结果
```

这样可以避免 LLM 看到 node_modules 等无关结果。

---

### 3.8 Formatters（formatters.go）

将 LSP 协议类型格式化为 LLM 易读的文本。按操作分组：

**goToDefinition：**
```
Defined in src/foo.go:42:15
```
或（多定义，如 TypeScript 重载）：
```
Found 3 definitions:
  src/foo.go:42:15
  src/bar.go:10:5
```

**findReferences：**
```
Found 12 references across 3 files:

src/foo.go:
  Line 15:10
  Line 23:4

src/bar.go:
  Line 5:1
```

**hover：**
```
Hover info at 42:15:

function foo(bar: string): void

JSDoc: Does something useful.
```

**documentSymbol：**
```
Document symbols:
  MyClass (Class) - Line 1
    constructor (Method) - Line 3
    myMethod (Method) - Line 10
```

**workspaceSymbol：**
```
Found 5 symbols in workspace:

src/foo.go:
  Foo (Function) - Line 42
  Bar (Variable) - Line 10

src/baz.go:
  Baz (Struct) - Line 1
```

**prepareCallHierarchy：**
```
Call hierarchy item: handleRequest (Function) - src/main.go:10
```

**incomingCalls / outgoingCalls：**
```
Found 3 incoming calls:

src/main.go:
  handleRequest (Function) - Line 10 [calls at: 12:5, 15:3]
```

---

### 3.9 系统提醒（agent/systemreminder/lsp_reminder.go）

实现 `systemreminder.Reminder` 接口，通过 `LSPStatusProvider` 接口获取 server 状态（避免 `systemreminder` 包直接依赖 `lsp` 包）：

```go
// LSPStatusProvider is implemented by lsp.LSPManager.
type LSPStatusProvider interface {
    IsConfigured() bool
    ServerInfos() []LSPServerInfo
}

type LSPServerInfo struct {
    Name       string
    Ready      bool
    Extensions []string
}

type LSPStatusReminder struct {
    Provider LSPStatusProvider
}

func (r *LSPStatusReminder) Generate(ctx systemreminder.Context) []string {
    if r.Provider == nil || !r.Provider.IsConfigured() {
        return nil
    }
    if !ctx.IsFirstMessage {
        return nil
    }

    servers := r.Provider.ServerInfos()
    // ... build output as shown below
}
```

注入到 `<system-reminder>` 中的效果：

```
<system-reminder>
...
Available LSP servers:
  typescript: ✓ ready (.ts, .tsx, .js, .jsx)
  gopls: ✓ ready (.go)

Use the LSP tool for code intelligence (goToDefinition, findReferences, etc.)
</system-reminder>
```

---

### 3.10 TUI 集成

**状态栏**：在右侧（token 占比区域旁）显示 LSP 连接状态。

```
Status: streaming | 1200 ctx | LSP: ✓
                         或
Status: idle     | 400 ctx  | LSP: ⏳
                         或
Status: idle     | 400 ctx  |   ← 没有配置 LSP server 时不显示
```

在 `tui/model.go` 中添加：

```go
type model struct {
    // ... 现有字段
    lspStatus string  // "✓" | "⏳" | "✗" | ""
}
```

**斜杠命令**（可选）：`/lsp list`

```
/lsp list
→ Connected LSP servers:
  typescript (typescript-language-server) ✓ running since 21:30:00
  gopls (gopls)                           ✓ running since 21:30:02
```

---

## 四、整合到 Agent 生命周期

### 初始化（agent_configure.go）

```go
func (a *AIAgent) Configure(cfg *config.Config) error {
    // ... 现有初始化 ...

    // 1. 初始化 LSP Manager（同步解析配置，不启动 server）
    if cfg.LSP.Enabled {
        a.lspManager = lsp.NewManager(&cfg.LSP)
        a.lspManager.Init()
    }

    // 2. 注册 LSPTool + LSPDiagnosticsTool（仅当至少配置了一个 server）
    if a.lspManager != nil && a.lspManager.ServerCount() > 0 {
        a.toolRegistry.Register(lsp.NewLSPTool(a.lspManager))
        a.toolRegistry.Register(lsp.NewLSPDiagnosticsTool(a.lspManager))
    }

    // 3. 注册 LSP Status Reminder（通过 LSPStatusProvider 接口，避免循环依赖）
    if a.lspManager != nil {
        a.reminderCollector.AddReminder(&systemreminder.LSPStatusReminder{Provider: a.lspManager})
    }
}
```

### 清理（agent.go Close()）

```go
func (a *AIAgent) Close() {
    // ... 现有清理 ...
    if a.lspManager != nil {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        a.lspManager.Shutdown(ctx)  // 停止所有 LSP server 进程
    }
}
```

### 文件同步钩子（agent_loop.go）

在 `handleToolCallFinish()` 中的 `executeToolCalls()` 之后，遍历执行结果，提取文件路径，同步到 LSP：

```go
// 工具执行完毕后同步文件到 LSP
if a.lspManager != nil {
    for _, result := range toolResults {
        if fp := lsp.ExtractFilePath(result.ToolName, result.Input); fp != "" {
            a.lspManager.SyncFile(ctx, fp)
        }
    }
}
```

---

### Channel 模式 & SubAgent 集成

**Channel 模式**（微信等 IM 通道）：
- LSPManager 是全局单例（`globalLSP`），所有 channel 共享同一组 LSP server
- Channel 的 AIAgent 实例通过 `SetSharedLSP(globalLSP)` 获取引用
- LSPTool 正常注册到 channel 的 tool registry 中
- 无需额外处理——文件路径解析基于 channel 的 working directory

**SubAgent（子代理）**：
- SubAgent 运行在独立的 context 中，可能位于 `git worktree add --detach` 创建的隔离目录
- **Phase 1**：SubAgent **不继承 LSP**。因为 worktree 路径下没有 `.git` 索引，LSP server（尤其 gopls）需要项目根检测，强行启动可能失败或产生误导结果
- **Phase 3**：如需支持，可在 worktree 创建后将 LSP workspace folder 指向原始项目目录，子代理通过绝对路径访问文件

**`tachi run` 模式**：
- 与 TUI 模式行为一致，正常使用 LSP
- 如果 `--skip-memory` 等 flag 被设置，不影响 LSP（LSP 独立于 memory 系统）

---

## 五、Crush 项目的借鉴

在完成初步设计方案后，对 [Crush](https://github.com/charmbracelet/crush)（Charmbracelet 的 AI 代理项目）进行了实地考察。Crush 使用 `github.com/charmbracelet/x/powernap@v0.1.4` 作为 LSP 库，在其上封装了一层薄薄的包装。以下是值得 Tachi 借鉴的设计点：

### 5.1 Powernap 库拆解

Powernap 提供了完整的 LSP 基础设施：

| 模块 | 功能 | 能否直接引用？ |
|------|------|---------------|
| `pkg/lsp/client.go` | LSP 客户端：stdio 通信、initialize/shutdown/exit 生命周期、hover/references/completion 请求 | 不建议——powernap 与 Crush 紧耦合 |
| `pkg/lsp/protocol/` | 完整 LSP 3.17 协议类型（Positon、Location、Diagnostic 等） | ❌ 太大，Tachi 只需子集 |
| `pkg/transport/` | JSON-RPC 2.0 传输层（stream reader/writer） | 可参考设计，但 `go.lsp.dev/jsonrpc2` 更轻量 |
| `pkg/config/` | LSP server 配置管理（默认内置 server list、`LoadDefaults()`) | 设计思路可借鉴 |

结论：**不直接复用 powernap**，但参考其 Client 接口设计和协议类型用法。

### 5.2 多 Tool vs 单 Tool 的设计决策

Crush 采用了**多 tool** 策略：

| Tool 名 | 功能 | 参数 |
|---------|------|------|
| `lsp_diagnostics` | 获取文件/project 的诊断信息 | `file_path`（可选） |
| `lsp_references` | 按符号名查找引用（grep + LSP 混合） | `symbol`, `path` |
| `lsp_restart` | 重启 LSP 客户端 | `name`（可选） |

而 Tachi 设计方案中采用**单 tool + 9 种 operation**。两者各有优劣：

| | 多 tool | 单 tool |
|---|--------|---------|
| LLM 选择成本 | 低——直接选名即可 | 高——需先选 tool 再填 operation |
| schema 复杂度 | 各 tool 各自独立 | 单个 schema 含多 operation，部分参数有依赖 |
| prompt 占用 | 每个 tool 占一个 slot | 只占一个 slot |
| 发现性 | 高——LLM 更容易发现 | 低——被其他工具淹没 |

**最终建议**：Tachi 采用**混合策略**：

```
LSP               ← 核心操作（goToDefinition, findReferences, hover 等 9 种）
LSPDiagnostics    ← 诊断信息获取（独立是因为参数不同，且 TUI 需要）
LSPRestart        ← 重启 LSP 客户端（运维操作，LLM 极少主动调用）
```

- `LSP`：单 tool + operation 枚举，负责代码智能查询
- `LSPDiagnostics`：独立的诊断 tool，因为不需要 line/character，只需要 `filePath` 或空（project 级）
- `LSPRestart`：运维工具，按 Crush 实践这是用户调用多于 LLM 调用的工具

### 5.3 诊断集成（Crush 最有价值的模式）

Crush 的诊断系统是**贯穿全流程**的：

```
LSP server publishDiagnostics
        │
        ▼
Client.handleDiagnostics()
  → diagnostics.VersionedMap.Set(uri, diagnostics)
  → onDiagnosticsChanged callback
        │
        ▼
App.updateLSPDiagnostics()
  → 更新 LSPClientInfo.DiagnosticCount
  → 发布 pubsub event
        │
        ▼
TUI.lspInfo()
  → 在侧边栏显示每个 server 的 E/W/H 计数
```

**Crush 的 WaitForDiagnostics settle 机制**（值得直接复制）：

```go
func (c *Client) WaitForDiagnostics(ctx context.Context, timeout time.Duration) {
    // 1. 先等最多 firstChangeDuration(1s) 看是否有诊断更新
    // 2. 如果有变化，继续等 settleDuration(300ms) 稳定
    // 3. 如果一直没变化→超时返回
}
```

这个模式用于 Crush 的 `notifyLSPs()` 中——在 `edit`/`write` 工具之后等待诊断稳定再返回。

**Tachi 的借鉴方案**：

```
Phase 2 加入：
1. LSPManager 内部：缓存每个文件的诊断信息（VersionedMap）
2. LSPTool 增加 "diagnostics" operation（或独立 LSPDiagnostics tool）
3. TUI 状态栏：显示各 server 的诊断计数（⚠️3 ⚡2）
4. 写操作后：自动触发 WaitForDiagnostics，结果注入到 system-reminder
```

### 5.4 文件同步策略对比

| 方案 | 优点 | 缺点 |
|------|------|------|
| **Crush 方案**：view/edit/write tool 各自调用 `openInLSPs()`/`notifyLSPs()` | 精确控制，只同步相关文件 | 每个 tool 都要加 LSP 依赖 |
| **Tachi 方案（原）**：agent loop 后置 hook | 不侵入任何 tool | 可能过度同步（文件没变也发 didChange） |
| **Tachi 方案（修正）**：后置 hook + 去重 | 不侵入 tool + 避免过度同步 | 需正确判断文件是否变化 |

**修正方案**：后置 hook 中只对 `ReadFile`（首次打开）和 `EditFile`/`WriteFile`（内容已修改）触发同步。`Glob`/`Grep` 等只读且不修改文件的工具不触发。通过检查 `fileTracker.IsOpen()` 避免重复 didOpen。

并发安全：`SyncFile()` 内部对同一 URI 加互斥锁，防止多个并发 LSPTool 调用同时发送 didOpen/didChange 导致竞态。

### 5.5 Crush 几个值得注意的实现细节

1. **`openKeyConfigFiles()`** — LSP server 就绪后自动打开项目根文件（go.mod、package.json 等），帮助 server 更快理解项目结构（尤其 gopls）

2. **Grep + LSP 混合查找引用** — Crush 的 `lsp_references` 先用 grep 按符号名搜文件，再对每个匹配位置调用 LSP `FindReferences`。一旦有非空结果就返回（LSP 会跨文件返回所有引用）。这样即使 LSP 挂了，grep 结果也能兜底。

3. **`hasRootMarkers()`** — 检查项目根目录是否存在特定标记文件（go.mod、Cargo.toml），避免在不合适的目录启动 LSP server

4. **SkipAutoStartCommands** — 对过于通用的命令（python、node、npx、java 等）不自动启动，防止误触

5. **Unavailable backoff** — Server 命令不存在时标记 `unavailable`，30 秒内不再尝试，避免频繁启动开销

---

## 六、与 Claude Code 和 Crush 的关键差异

| 方面 | Claude Code | Crush | Tachi（设计方案） |
|------|-------------|-------|-------------------|
| LSP 配置来源 | 插件系统 | `crush.json`（JSON Schema） | `config.yaml` |
| Server 启动时机 | 启动时异步初始化 | 文件访问时懒启动 | 文件访问时懒启动 |
| JSON-RPC 库 | `vscode-jsonrpc` (npm) | 自研 `powernap/pkg/transport` | `go.lsp.dev/jsonrpc2` |
| 协议类型 | `vscode-languageserver-types` | 自研 `powernap/pkg/lsp/protocol` | 手写协议子集（~150 行） |
| Tool 策略 | 单 tool + 9 种 operation | 多 tool（diagnostics/references/restart） | 混合策略（LSP + LSPDiagnostics + LSPRestart）|
| 文件同步时机 | 工具调用时同步 | view/edit/write 工具显式调用 | Agent loop 后置 hook + 去重 |
| 诊断功能 | 无（工具只做查询） | 全流程：缓存→TUI 显示→工具查询 | Phase 2 加入，参考 Crush 方案 |
| 诊断 settle 机制 | 无 | `WaitForDiagnostics()` 300ms 稳定期 | 直接借鉴 Crush |
| git-ignore 过滤 | 有（`git check-ignore` 批量） | 无 | 有 |
| TUI 集成 | 无专属 LSP UI | 侧边栏显示各 server 状态+诊断计数 | 状态栏显示 + 可选面板 |
| 项目根检测 | 无（任何位置启动） | `hasRootMarkers()` + `skipAutoStartCommands` | 参考 Crush，避免不合适的目录启动 |
| 混合查找 | 纯 LSP | grep + LSP 双保险 | 参考 Crush，先 grep 再 LSP |

**相比 Claude Code 和 Crush，Tachi 的差异化价值**：
1. `git check-ignore` 过滤（Claude Code 有，Crush 没有）
2. 混合策略的 Tool 设计（两者都偏极端，Tachi 取中间）
3. 手写协议子集（两者都引入完整类型库）

---

## 七、依赖分析

| 候选库 | 内容 | 推荐度 |
|--------|------|--------|
| `go.lsp.dev/jsonrpc2` | JSON-RPC 2.0 传输层，~500 行 | ⭐⭐⭐ 推荐 |
| 手写 JSON-RPC 2.0 | 最小实现，~300 行 | ⭐⭐⭐ 可行，零依赖 |
| `go.lsp.dev/protocol` | 完整 LSP 3.16 类型，较庞大 | ⭐⭐ 仅参考，不建议直接引用 |

**推荐方案**：使用 `go.lsp.dev/jsonrpc2` 做传输层 + 手写约 150 行 LSP 协议类型。既不重新发明轮子（JSON-RPC 的并发、取消、错误处理等都已有成熟实现），又避免引入过于庞大的类型库。

`go.lsp.dev/jsonrpc2` 的安装：

```
go get go.lsp.dev/jsonrpc2
```

---

## 八、实现优先级

### Phase 1 — 核心可用（~2-3 天）

```
jsonrpc.go      ← stdio 传输层（或直接使用 go.lsp.dev/jsonrpc2）
protocol.go     ← LSP 协议类型子集
server.go       ← 进程生命周期、健康检查、请求/通知
                  ├─ ContentModified 指数退避重试
                  ├─ 请求超时（request_timeout，默认 15s）
                  ├─ 并发控制（concurrency_limit semaphore）
                  └─ stderr → ~/.tachi/logs/lsp/<name>.log
manager.go      ← 路由、lazy start、全局单例
                  ├─ LookPath 预检 + unavailable 标记
                  └─ ensureStarted 自动启动
config.go       ← YAML 配置加载（含 request_timeout、max_results、concurrency_limit、settings）
tool.go         ← LSPTool 基本操作（需全部 9 种 operation）
                  ├─ Capabilities 前置检查
                  ├─ 结果截断（max_results，默认 50）
                  └─ Tool description 含调用示例 + UTF-16 偏移提醒
formatters.go   ← 结果格式化
file_tracker.go ← 文件打开跟踪
```

**可用操作**：goToDefinition、findReferences、hover、documentSymbol、workspaceSymbol、goToImplementation、prepareCallHierarchy、incomingCalls、outgoingCalls

### Phase 2 — 体验完善（~2 天）

```
lsp_reminder.go ← LSP 状态系统提醒（systemreminder 包里，Provider 接口解耦）
agent_configure ← 整合到 Agent 生命周期
agent_loop      ← 文件同步 hook（去重，仅 ReadFile/EditFile/WriteFile 触发）
tui 集成        ← 状态栏显示
git-ignore 过滤  ← 过滤 gitignored 的结果
/lsp list 命令   ← 斜杠命令查看 LSP 状态
诊断缓存        ← 捕获 publishDiagnostics，VersionedMap 存储（每文件仅保留最新版本，全局上限 5000 条）
LSPDiagnostics  ← 独立的诊断查询 tool
WaitForDiagnostics ← 写操作后等待诊断稳定的 settle 机制
```

### Phase 3 — 进阶（按需）

```
Grep + LSP 混合查找 ← 先 grep 再 LSP，提高鲁棒性
项目根检测          ← hasRootMarkers() + skipAutoStartCommands
TUI 诊断面板        ← 在侧边栏/状态栏显示诊断计数
autoLSP            ← 根据项目文件自动检测并启动 LSP server
unavailable 退避    ← 命令不存在时 30s 内不重试
```
