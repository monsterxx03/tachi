# /mcp command

## Problem

Users have MCP (Model Context Protocol) servers configured but no way to inspect or manage them from within an active Tachi session. They must restart the session to change server state.

## Behavior

A `/mcp` slash command (or `Ctrl+M`) opens a dedicated **MCP Management Overlay** — a bordered, centered popup that takes over the screen:

### Overlay UI

```
┌─ MCP Servers (↑↓ nav  Enter expand  t toggle  r reconnect  a auth  Esc close) ──┐
│                                                                                    │
│  🟢 test-1                     stdio  6 tools                                    │
│  🟢 test-2                         http   4 tools  OAuth                             │
│  🔴 disabled-server             stdio  —                                          │
│                                                                                    │
│  ── test-1 tools ──────────────────────────────────────────────────────────────  │
│  getArticleTree         获取 KM 知识库的文章树结构                                    │
│  getArticleDetail       获取文章详情内容（标题、正文、元信息等）                         │
│  searchResources        搜索 KM 知识库资源                                           │
│  …                                                                                 │
│                                                                                    │
│  ✓ test-1 connected, 6 tool(s)                                                   │
└────────────────────────────────────────────────────────────────────────────────────┘
```

### Keyboard bindings

| 键 | 操作 |
|------|------|
| `↑` `↓` `k` `j` | 在 server 列表中移动 |
| `Enter` / `Space` | 展开/收起当前 server 的 tool 列表 |
| `pgup` / `pgdown` | tool 列表翻页 |
| `t` | toggle enabled/disabled |
| `r` | reconnect |
| `a` | auth（仅 HTTP+OAuth server） |
| `Esc` / `q` | 关闭浮层 |
| `Ctrl+C` | 关闭浮层 |

### Subcommand compatibility

Subcommand syntax is still supported for programmatic use:

- **`/mcp toggle <name>`** — enables or disables a specific MCP server at runtime, no restart required.
- **`/mcp reconnect <name>`** — reconnects to a server that has dropped its connection.
- **`/mcp auth <name> [redirect-url]`** — initiates or completes OAuth flow for an HTTP MCP server.

Bare `/mcp` or `/mcp list` opens the overlay.

## Implementation notes

### Components

| 文件 | 说明 |
|------|------|
| `tui/mcpview.go` | 自包含的浮层组件：数据结构 `MCPServerItem` / `MCPToolItem`，渲染 `View()`，键盘处理 `HandleKey()` 返回 `MCPAction` |
| `tui/model.go` | 新增 `stateManagingMCP` 状态、`mcpView` 字段、`handleKeyManagingMCP` / `enterMCPOverlay` / `exitMCPOverlay` |
| `tui/commands.go` | `/mcp` 无 subcommand 时路由到 `enterMCPOverlay()` |
| `tui/styles.go` | 新增 MCP 浮层相关 style（border, server/tool 颜色, OAuth badge 等） |
| `tui/messages.go` | 新增 `mcpOverlayMsg` 类型，承载异步操作结果到 overlay |

### Data flow

1. `enterMCPOverlay()` 从 `m.mcpServers` + `m.mcpManager` + `m.agent.ToolSchemas()` 构建 `[]MCPServerItem`
2. `mcpView.View()` 按 35%/65% 分割渲染 server list + tool list
3. 异步操作（toggle/reconnect/auth）通过 goroutine + channel 执行，结果以 `mcpOverlayMsg` 注入 overlay 底部消息栏

### Key design decisions

- **无外部依赖** — `MCPView` 不持有 `Model` 引用，通过 `MCPAction` 枚举解耦
- **实时 tool 列表** — 每次渲染时从 `agent.ToolSchemas()` 按 `mcp__<server>__` 前缀筛选，无额外缓存
- **选择状态保持** — `refreshMCPServerItems()` 在异步操作完成后重建列表并恢复选中位置
- **全屏 overlay** — `stateManagingMCP` 时 `View()` 直接返回 overlay，不渲染 input/statusbar
