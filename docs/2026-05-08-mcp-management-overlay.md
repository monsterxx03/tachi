# MCP Management Overlay — Development Plan

## Problem

当前 MCP server 管理完全依赖 `/mcp` slash command，结果以普通 chat message 形式呈现。主要痛点：

1. **看不到 tools** — 用户无法在 TUI 中查看某个 MCP server 暴露了哪些 tool，也没法看到 tool 的描述和参数 schema
2. **操作体验差** — 每次 toggle/reconnect/auth 后结果混在聊天流中，没有独立的管理视图
3. **状态不可见** — server 的 enabled/connected/tool 数量等信息无法一目了然
4. **无实时反馈** — reconnect/auth 等异步操作的进度通过聊天消息逐条插入，体验割裂

## Goal

提供一个专用的浮层（overlay）来管理 MCP server，类似 `/model` 的 provider 选择浮层，但更丰富：

- 列表展示所有 MCP server：名称、类型（stdio/http）、状态（enabled + connected）、tool 数量
- 选中某个 server 后展开其 tool 列表（名称 + 描述）
- 支持 toggle、reconnect、auth 操作，操作结果在 overlay 内反馈
- 纯键盘操作，最终可以绑定到一个全局快捷键（如 `Ctrl+M`）

## Design

### 数据结构

#### MCPView — 浮层组件 (`tui/mcpview.go`)

一个自包含的组件，不持有 Model 引用，而是通过数据注入和回调与 Model 交互：

```go
type MCPServerItem struct {
    Name        string
    Type        string   // "stdio" or "http"
    Enabled     bool
    Connected   bool
    ToolCount   int
    Tools       []MCPToolItem
    HasOAuth    bool
    Profile     string   // 来自哪个 profile，空 = 默认
}

type MCPToolItem struct {
    Name        string
    Description string   // 首行简述（截断到 80 字）
    Required    []string
}
```

```go
type MCPView struct {
    servers      []MCPServerItem
    selIdx       int      // 当前选中的 server
    toolScrollOff int     // tool 列表的滚动偏移
    width        int
    height       int
    expandServer string   // 当前展开 tool 列表的 server name（空 = 不展开）
    message      string   // 操作结果提示（e.g. "Connected ✓"）
}
```

核心方法：

| 方法                                       | 说明                             |
| ------------------------------------------ | -------------------------------- |
| `SetServers(items []MCPServerItem)`        | 注入当前 server 列表             |
| `HandleKey(key string) (action MCPAction)` | 键盘处理，返回需要外部执行的操作 |
| `SetMessage(msg string)`                   | 显示操作反馈（3 秒后自动清除）   |
| `View() string`                            | 渲染浮层                         |

#### MCPAction — 操作意图

```go
type MCPAction int
const (
    MCPActionNone    MCPAction = iota
    MCPActionToggle
    MCPActionReconnect
    MCPActionAuth
    MCPActionExpand   // 展开/收起 tool 列表
    MCPActionDismiss  // 关闭浮层
)
```

### 状态管理

在 `Model` 中新增：

- `stateManagingMCP` — 新的 TUI 状态
- `mcpView *MCPView` — 浮层组件实例
- `mcpActionCh chan MCPActionResult` — 异步操作结果通道

打开浮层时，从 `m.mcpManager` 和 `m.mcpServers` 构建 `[]MCPServerItem` 注入 `MCPView`。Tool 列表从 `m.agent.ToolSchemas()` 中以 `mcp__<server>__` 前缀筛选得到。

### UI 布局

```
┌─ MCP Servers (↑↓ navigate, Enter expand, t toggle, r reconnect, a auth, Esc close) ──┐
│                                                                                       │
│  🟢 test1                     stdio  • 6 tools                                     │
│  🟢 test2                         http   • 4 tools  [OAuth]                            │
│  🔴 disabled-server             stdio  • —                                           │
│                                                                                       │
│  ── test1 tools ─────────────────────────────────────────────────────────────────  │
│  tool1         工具 1...                                   │
│                                                                                       │
│  ✓ Connected with 6 tool(s)                                                          │
└───────────────────────────────────────────────────────────────────────────────────────┘
```

- 上半部分（约 1/3 高度）：server 列表，单行展示 `[状态图标] name  type  • N tools  [OAuth badge]`
- 下半部分（约 2/3 高度）：选中 server 的 tool 列表，可滚动
- 底部一行：操作结果 message
- 窗口占屏幕的 80% 宽度、70% 高度，居中显示，带 border

### 键盘绑定

| 键                | 操作                         |
| ----------------- | ---------------------------- |
| `↑` `↓` `k` `j`   | 在 server 列表中移动         |
| `Enter` / `Space` | 展开/收起 tool 列表          |
| `t`               | toggle enabled/disabled      |
| `r`               | reconnect                    |
| `a`               | auth（仅 HTTP+OAuth server） |
| `Esc` / `q`       | 关闭浮层                     |

### 交互流程

#### 打开浮层

1. 触发 `/mcp` 命令（不带 subcommand）或 `Ctrl+M`
2. `setState(stateManagingMCP)`
3. 从 `m.mcpServers` + `m.mcpManager` + `m.agent.ToolSchemas()` 构建 `[]MCPServerItem`
4. `mcpView.SetServers(items)`
5. `layout()` 重算尺寸

#### Toggle

1. 用户按 `t` → `HandleKey` 返回 `MCPActionToggle`
2. `handleMCPOverlayKey` 调用 `m.mcpToggle(serverName)`（复用现有逻辑）
3. 异步完成后更新 `mcpView` 中的 server 状态，刷新 tool 列表
4. 如果 disable：同步调用 `m.unregisterMCPTools(serverName)`
5. 如果 enable 成功：同步调用 `m.connectAndRegisterMCP(...)`

#### Reconnect

1. 用户按 `r` → `HandleKey` 返回 `MCPActionReconnect`
2. 类似 toggle enable 流程：先 unregister old tools → reconnect → register new tools
3. 异步执行，结果通过 `mcpView.SetMessage()` 显示

#### Auth

1. 用户按 `a` → `HandleKey` 返回 `MCPActionAuth`
2. 复用现有 `m.mcpAuth(name)` 逻辑
3. 异步执行，中间状态通过 `mcpView.SetMessage()` 实时反馈

#### 关闭浮层

1. 用户按 `Esc` → `HandleKey` 返回 `MCPActionDismiss`
2. `setState(stateIdle)`，layout 恢复

### 数据流

```
Config (m.mcpServers)
    │
    ▼
Manager (m.mcpManager.IsConnected / ConnectedServers)
    │
    ▼
Agent (m.agent.ToolSchemas → filter mcp__<server>__)
    │
    ▼
MCPView.SetServers(items)  →  渲染
    │
    ▼
HandleKey → MCPAction → mcpToggle/mcpReconnect/mcpAuth
    │
    ▼
异步操作 → 更新 mcpView message / 刷新 servers
```

## Implementation Phases

### Phase 1: 基础组件 `tui/mcpview.go`

**约 250 行**

- 定义 `MCPServerItem`、`MCPToolItem`、`MCPAction` 类型
- 实现 `MCPView` 结构体及 `SetServers`、`View`、`HandleKey` 方法
- 仅支持 server 列表展示和上下导航
- 展开/收起 tool 列表
- 静态数据渲染（状态图标、颜色）

### Phase 2: 接入 Model 状态机

**约 150 行**

- 在 `Model` 中新增 `stateManagingMCP` 状态
- 添加 `mcpView` 字段
- 实现 `handleKeyManagingMCP` — 将 `MCPAction` 分发到对应的操作函数
- 实现 `enterMCPOverlay()` — 构建 `[]MCPServerItem` 并注入 `mcpView`
- 实现 `exitMCPOverlay()` — 恢复 `stateIdle`
- 在 `View()` 中路由到 `mcpView.View()`
- 在 `layout()` 中分配浮层区域（80% 宽 × 70% 高，居中）
- `/mcp` 不带 subcommand 时进入浮层（替代当前的 `mcpList()` 行为）

### Phase 3: 操作实现

**约 100 行**

- toggle/reconnect/auth 的异步执行 + 消息反馈
- tool 列表在 enable/disable 后的增量刷新（不重建整个列表）
- OAuth 流程的中间状态展示（"Opening browser..." → "Authorization complete ✓"）

### Phase 4: `Ctrl+M` 快捷键 & 优化

**约 50 行**

- 在 idle 状态下绑定 `Ctrl+M` 进入 MCP 浮层
- tool 描述截断与排版优化
- server 较多时支持搜索（可选，后续迭代）

### Phase 5: 测试 & 文档

- 更新 `y` 中 `/mcp` 命令文档
- 手动测试各种操作组合（toggle → reconnect → auth → toggle）

## Files to Create / Modify

| 文件                  | 操作     | 说明                                                             |
| --------------------- | -------- | ---------------------------------------------------------------- |
| `tui/mcpview.go`      | **新建** | MCP 浮层组件（~250 行）                                          |
| `tui/model.go`        | **修改** | 新增 `stateManagingMCP`、`mcpView`、key handler、View 路由       |
| `tui/messages.go`     | **修改** | 可能需要新的消息类型（可复用现有 `mcpStatusMsg` 或直接 channel） |
| `tui/commands.go`     | **修改** | `/mcp` 无 subcommand 时进入浮层                                  |
| `tui/styles.go`       | **修改** | 新增 MCP 浮层相关 style（border、状态色）                        |
| `docs/mcp-command.md` | **修改** | 更新文档，补充浮层交互说明                                       |

## Open Questions

1. **Tool 列表刷新时机**：enable/disable 后 tool 列表会变化，是否需要在每次渲染时实时从 `agent.ToolSchemas()` 读取？还是缓存直到显式刷新？建议实时读取（`ToolSchemas()` 只是遍历 map，开销极小）。

2. **OAuth 中间状态**：auth 流程可能长达数分钟，中间需要反馈 "Opening browser, please complete authorization..."。建议通过 channel 发送多阶段消息到 `mcpView.SetMessage()`，并在 View 中展示 spinner。

3. **Profile 信息**：config 支持 `mcp_profiles`，浮层中是否展示 server 来自哪个 profile？建议展示，用 dim style 在名称后加 `[profile:xxx]`。

4. **Tool 参数详情**：在 tool 列表中是否展示 required 参数？建议展示 tool 的描述首行（截断到一行），完整参数 schema 可以后续通过 `Enter` 在 tool 上展开查看（Phase 5+）。
