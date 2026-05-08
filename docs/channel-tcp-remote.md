# Channel 远程集成方案：TCP / Unix Socket 协议

> 设计文档，尚未实施。目标：支持企业私有 IM 以独立进程方式集成 tachi，纯 Go 代码无需对 NPM 生态引入依赖。

---

## 目录

1. [动机](#1-动机)
2. [总体架构](#2-总体架构)
3. [协议层](#3-协议层)
4. [传输层](#4-传输层)
5. [tachi 侧实现](#5-tachi-侧实现)
6. [Channel 侧实现指南](#6-channel-侧实现指南)
7. [Config 改造](#7-config-改造)
8. [生命周期与错误处理](#8-生命周期与错误处理)
9. [安全考量](#9-安全考量)
10. [与 MCP 集成的对比](#10-与-mcp-集成的对比)
11. [迁移路径](#11-迁移路径)
12. [附录：Go 实现骨架](#12-附录go-实现骨架)

---

## 1. 动机

当前 `channel` 集成存在两个硬编码点：

| 位置 | 问题 |
|------|------|
| `main.go` — 直接 `import weixin` 并调用 `weixin.NewChannel()` | 新增 IM 通道需改动 tachi 仓库源码 |
| `config/config.go` — `ChannelConfig` 内硬编码 `WeixinConfig` 结构体 | 配置模式与具体实现耦合 |

这种设计对公开 IM（微信、Slack 等）尚可接受，但企业私有 IM 的集成代码不适合放入 tachi 仓库。需要一个机制让外部进程通过标准协议与 tachi 通信，做到：

- **零代码侵入** — 新增 channel 不改 tachi 一行代码
- **语言无关** — channel 端可用任何语言实现（Go / Python / Rust / Java …）
- **协议极简** — 模仿 MCP 的 stdio JSON-line 协议，但走 Unix Socket（默认）/ TCP，更稳定可靠
- **部署隔离** — channel 可独立构建、升级、重启，不中断 tachi 运行

---

## 2. 总体架构

```
┌─────────────────────────────────────────────────────────┐
│                        tachi                             │
│                                                          │
│  ┌──────────┐   ┌───────────────┐   ┌───────────────┐   │
│  │  channel │   │ TCPChannel  #1│   │ TCPChannel  #2│   │
│  │  Manager │──►│ (unix socket) │   │ (tcp 9741)    │   │
│  └──────────┘   │ weixin        │   │ feishu        │   │
│                 └──────┬────────┘   └──────┬────────┘   │
│                        │                   │             │
└────────────────────────┼───────────────────┼────────────┘
                         │   unix socket     │  loopback
                         │ /tmp/tachi-weixin │  tcp :9741
                         ▼                   ▼
                 ┌──────────────┐  ┌──────────────────┐
                 │  channel 实现 │  │  channel 实现     │
                 │  (独立进程)   │  │  (独立进程/容器)  │
                 │              │  │                  │
                 │ connect to   │  │ listen on :9741  │
                 │ /tmp/tachi-  │  │ (tachi connects) │
                 │ weixin.sock  │  │                  │
                 └──────────────┘  └──────────────────┘
```

### 两种连接模式

| 模式 | 连接方向 | 适用场景 |
|------|----------|----------|
| **Unix Socket（默认）** | Channel 端 listen，tachi 端 connect | 同机部署、最简单可靠 |
| **TCP** | Channel 端 listen，tachi 端 connect | 跨机部署、容器化 |

两种模式共享完全相同的上层协议，仅传输层不同。

> **设计决策：谁 listen？** — Channel 端 listen，tachi 端 connect。理由：
> - Channel 端往往有自己的启动/预热流程（登录、扫码），先启动后 accept 更自然
> - tachi 重启时只需重连，channel 不受影响
> - 简化 tachi 的实现，避免端口冲突管理

---

## 3. 协议层

### 3.1 设计原则

- **单连接双向** — 仅需一个 TCP/Unix 连接，同时承载上下行
- **JSON-line** — 每条消息以 `\n` 分隔，与 MCP stdio 协议风格一致
- **异步** — 入站消息和出站回复分独立帧，互不阻塞
- **最少字段** — 仅传递消息路由必需的字段，不做序列化框架

### 3.2 帧格式

每帧一行 JSON，以 `\n`（0x0A）结尾。接收方按行读取。

```
{...json payload...}\n
```

### 3.3 Frame 类型

#### Frame: `message`（Channel → tachi）

Channel 收到 IM 消息后，封装发往 tachi。

```json
{
  "type": "message",
  "id": "req-001",
  "thread_id": "wx_user_abc@im.wechat",
  "message_id": "100001",
  "content": "帮我查下今天的会议室预定",
  "channel_id": ""
}
```

**字段说明**：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `type` | string | ✓ | 固定 `"message"` |
| `id` | string | ✓ | Channel 生成的唯一请求 ID，用于关联回复 |
| `thread_id` | string | ✓ | 会话标识，对应 session.ThreadID，用于多轮对话记忆 |
| `message_id` | string | ✓ | IM 侧消息 ID，用于 reply_to 引用 |
| `content` | string | ✓ | 纯文本消息内容 |
| `channel_id` | string | | 可选的群组 / 频道标识 |

#### Frame: `response`（tachi → Channel）

tachi 的 Agent 处理完成后返回。

```json
{
  "type": "response",
  "id": "req-001",
  "thread_id": "wx_user_abc@im.wechat",
  "content": "今天下午 3 点 A301 会议室空闲，已帮你预定。",
  "reply_to": "100001"
}
```

**字段说明**：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `type` | string | ✓ | 固定 `"response"` |
| `id` | string | ✓ | 对应请求的 `id`，用于关联 |
| `thread_id` | string | ✓ | 回传原 thread_id |
| `content` | string | ✓ | Agent 回复文本 |
| `reply_to` | string | ✓ | 原消息 message_id，IM 端正确 threading |

#### Frame: `error`（tachi → Channel）

Agent 处理出现不可恢复错误时返回。

```json
{
  "type": "error",
  "id": "req-001",
  "thread_id": "wx_user_abc@im.wechat",
  "error": "provider not configured"
}
```

#### Frame: `ack`（tachi → Channel）

tachi 收到 message 帧后发的确认帧（可选，见 3.5）。

```json
{
  "type": "ack",
  "id": "req-001"
}
```

#### Frame: `slash`（Channel → tachi，可选扩展）

用于未来扩展斜杠命令（如 `/mode thinking`），当前版本不实现。

```json
{
  "type": "slash",
  "id": "cmd-001",
  "thread_id": "wx_user_abc@im.wechat",
  "command": "/new"
}
```

> `slash` 帧的 reply 走 `response` 帧。tachi 的 Manager 内已有 slash command 处理逻辑（handleSlashCommand），通过此帧可让远程 channel 也复用。


### 3.4 消息流示例

时间线视角的完整一次对话：

```
Channel                           tachi                            LLM
  |                                |                                |
  |-- {"type":"message","id":"1",  |                                |
  |    "thread_id":"u1",           |                                |
  |    "content":"hello"}\n ------►|                                |
  |                                |-- RunConversationStream() ────►|
  |                                |◄── streaming text deltas ─────|
  |                                |                                |
  |◄── {"type":"response","id":"1",|                                |
  |     "thread_id":"u1",          |                                |
  |     "content":"Hi! 有什么可以帮",|                               |
  |     "reply_to":"1001"}\n ------|                                |
  |                                |                                |
  |  IM 后端发送回复                |                                |
```

### 3.5 并发与请求关联

- **并发处理**：一个 channel 连接上，多个 `message` 帧可流水线发送（不等前一个回复就发下一个）。tachi 侧 `Manager.buildHandler()` 天然并发安全——每次处理创建新的 agent 实例。
- **关联方式**：通过 `id` 字段。Channel 端为每个 outgoing message 生成唯一 `id`，tachi 在 response/error 中原样回传。
- **无需 ack**：默认不启用 ack。`message` 帧即隐含"请求已发送"。如果 channel 需要对请求丢失进行检测，可通过配置启用 ack 模式。

### 3.6 流式文本（未来扩展）

当前版本不支持流式输出——response 帧一次性携带完整回复。未来可在 response 之前增加 `delta` 帧实现流式（类似 SSE）。

```json
{"type":"delta","id":"req-001","content":"具体来说"}
{"type":"delta","id":"req-001","content":"，你这样..."}
{"type":"response","id":"req-001","thread_id":"u1","content":"完整文本...","reply_to":"1001"}
```

---

## 4. 传输层

### 4.1 Unix Socket（默认模式）

```
Channel 端:
  - listen on /tmp/tachi-{name}.sock (可配置)
  - accept 单个连接
  - 连接建立后按 JSON-line 协议收发

tachi 端:
  - connect to /tmp/tachi-{name}.sock
  - 连接建立后按 JSON-line 协议收发
```

**优势**：
- 零网络开销，仅在文件系统创建 socket inode
- 自动受 Linux/macOS 文件权限保护（`0700`）
- 无需端口分配，无冲突

### 4.2 TCP 模式

```
Channel 端:
  - listen on 127.0.0.1:{port} (或 0.0.0.0 跨机)
  - accept 单个连接

tachi 端:
  - connect to {host}:{port}
```

**适用场景**：
- Channel 运行在独立容器中
- 跨机部署（channel 在边缘节点，tachi 在中心）
- Unix socket 不可用的平台

### 4.3 Config 中的传输配置

```yaml
channel:
  backends:
    - name: weixin
      transport: unix          # "unix" | "tcp"
      address: /tmp/tachi-weixin.sock  # for unix: socket path; for tcp: host:port
      enabled: true
    
    - name: lark
      transport: tcp
      address: 127.0.0.1:9742
      enabled: true
```

`transport` 默认为 `"unix"`。`address` 对于 unix 默认为 `/tmp/tachi-{name}.sock`，对于 tcp 必填。

### 4.4 连接重试策略

tachi 作为 connect 方，需要自己处理重连：

```
连接策略:
  1. 启动时按配置顺序尝试连接
  2. 连接失败 → 退避重试 (1s → 2s → 4s → ... 上限 30s)
  3. 连接成功后进入正常通信状态
  4. 连接断开 → 进入步骤 2 (退避重试)
  5. ctx cancelled → 停止重试
```

---

## 5. tachi 侧实现

### 5.1 新增文件

| 文件 | 说明 |
|------|------|
| `channel/remote.go` | `RemoteChannel` 实现，封装 TCP/Unix socket 连接、协议收发、断线重连 |

### 5.2 RemoteChannel 结构

`RemoteChannel` 实现 `channel.Channel` 接口：

```go
type RemoteChannel struct {
    name      string
    transport string   // "unix" | "tcp"
    address   string   // socket path or host:port
    handler   MessageHandler
    logger    *debuglog.Logger
}
```

### 5.3 RemoteChannel.Run() 核心流程

```
Run(ctx, handler):
  1. 保存 handler 引用
  2. 循环：
     a. 等待 ctx 取消或超时 → 退出
     b. 尝试 connect() → 成功后进入 receiveLoop()
     c. 连接断开 → sleep(退避) → 继续循环
```

```
connect():
  根据 transport 类型:
    - unix: net.Dial("unix", address)
    - tcp:  net.Dial("tcp", address)
  返回 net.Conn
```

```
receiveLoop(ctx, conn):
  1. 启动 goroutine A: readLoop — 从 conn 按行读 JSON
  2. 在 goroutine A 中:
     a. 读到 {"type":"message",...} → 启动 goroutine B 调用 handler
     b. goroutine B 完成后 → 写 {"type":"response",...} 到 conn
  3. conn 读端关闭或出错 → 关闭 conn，返回
```

### 5.4 关键设计决策

**单连接 vs 多连接**：单连接。因为每个 channel 实例只对应一个 IM 后端，没有多客户端问题。

**goroutine 模型**：

```
main goroutine: Run() → 连接管理 + 退避重试
goroutine A:    readLoop — 读取 JSON frame
goroutine B(i): 并发 handler 调用 — 每个 incoming message 一条
```

**写入并发安全**：`net.Conn` 的 `Write` 是 goroutine-safe 的（Go 保证），但多个 goroutine 并发写可能导致帧交错。解决方案：
- 写操作通过 channel 排队到一个 writer goroutine，或
- 简单加 `sync.Mutex` 保护写端

### 5.5 与现有 Manager 的关系

**不需要改动 Manager**。`RemoteChannel` 是一个标准的 `Channel` 接口实现，通过 `Manager.Add()` 注册即可。现有的 `Manager.Start()` → `buildHandler()` → `process()` 完全不变。

唯一的变化入口在 `main.go`：

```go
// 之前 (hardcode)
if cfg.Channel.Weixin.Enabled {
    wxCh, _ := weixin.NewChannel(cfg.Channel.Weixin)
    manager.Add(wxCh)
}

// 之后 (配置驱动)
for _, bcfg := range cfg.Channel.Backends {
    if !bcfg.Enabled {
        continue
    }
    ch := channel.NewRemoteChannel(bcfg.Name, bcfg.Transport, bcfg.Address)
    manager.Add(ch)
}
```

> **注意**：`weixin` channel 迁移到远程模式后，`channel/weixin/` 整个目录从 tachi 仓库移除，发布为独立二进制 `tachi-channel-weixin`。


---

## 6. Channel 侧实现指南

### 6.1 职责

Channel 侧进程负责：

1. 连接到 IM 后端（登录、认证、维持长连接/长轮询）
2. 接收 IM 消息并转换为协议帧
3. 监听 Unix socket / TCP 端口，接受 tachi 的连接
4. 接收 tachi 的 response 帧，转换为 IM 格式发送给用户

### 6.2 协议状态机（Channel 视角）

```
                 ┌──────────────────┐
                 │  IM Backend 登录  │
                 └────────┬─────────┘
                          │ 登录完成
                          ▼
                 ┌──────────────────┐
                 │  监听 socket/端口  │
                 └────────┬─────────┘
                          │ tachi 连接
                          ▼
    ┌─────────────────────────────────────────────┐
    │              正常通信状态                      │
    │                                              │
    │  IM 消息到达 ──► 构造 message frame ──► write │
    │  read ◄── response frame ◄── tachi 处理完成   │
    │                                              │
    └────────┬──────────────────┬─────────────────┘
             │ tachi 断开       │ 致命错误
             ▼                  ▼
    ┌──────────────┐   ┌──────────────┐
    │ accept 新连接  │   │     退出      │
    └──────────────┘   └──────────────┘
```

### 6.3 最小实现要求

一个合规的 channel 实现需要：

1. **监听**：`bind()` / `listen()` 到配置的地址
2. **accept**：接受单个连接（额外连接可拒绝或排队）
3. **读帧**：按 `\n` 分割读 JSON，反序列化 `response` / `error` 帧
4. **写帧**：序列化 `message` 帧为 JSON + `\n`，写入连接
5. **回复发送**：解析 `response` 帧中的 `content` 和 `reply_to`，通过 IM API 发送

### 6.4 连接是长连接

Channel 端应维持与 tachi 的长连接。如果 tachi 断开（正常退出或崩溃），channel 应回到 accept 状态等待 tachi 重连，而不是退出。

### 6.5 消息去重（Channel 侧责任）

Channel 侧负责保证 `message` 帧不重复投递。实现方式：
- 维护已发送且待回复的 request `id` 集合
- 连接断开后，未收到 reply 的 request 可选择重发（重新连接后）
- 收到 response 后从集合中移除

---

## 7. Config 改造

### 7.1 目标结构

```yaml
channel:
  backends:
    - name: weixin
      transport: unix                              # "unix" (default) | "tcp"
      address: /tmp/tachi-weixin.sock              # socket path or host:port
      enabled: true

    - name: lark
      transport: tcp
      address: 127.0.0.1:9742
      enabled: false
```

### 7.2 Config 结构体变化

```go
// 旧 (删除)
type WeixinConfig struct { ... }
type ChannelConfig struct {
    Weixin WeixinConfig `yaml:"weixin"`
}

// 新
type RemoteChannelConfig struct {
    Name      string `yaml:"name"`
    Transport string `yaml:"transport"` // "unix" (default) | "tcp"
    Address   string `yaml:"address"`
    Enabled   bool   `yaml:"enabled"`
}

type ChannelConfig struct {
    Backends []RemoteChannelConfig `yaml:"backends"`
}
```

`RemoteChannelConfig` 的默认值：
- `transport` 为空 → `"unix"`
- `address` 为空 + `transport=unix` → `"/tmp/tachi-{name}.sock"`
- `address` 为空 + `transport=tcp` → 报错

### 7.3 向后兼容

为平滑迁移，保留短暂的兼容期：如果 `channel.weixin.enabled: true` 存在且 `channel.backends` 为空，自动将原有 weixin 配置转为 `backends` 列表中的一项，并打印 deprecation warning。

```go
// config.Load() 内部
if cfg.Channel.Weixin.Enabled && len(cfg.Channel.Backends) == 0 {
    cfg.Channel.Backends = append(cfg.Channel.Backends, RemoteChannelConfig{
        Name:      "weixin",
        Transport: "unix",
        Address:   "/tmp/tachi-weixin.sock",
        Enabled:   true,
    })
    log.Printf("[config] channel.weixin is deprecated, migrating to channel.backends")
}
```

---

## 8. 生命周期与错误处理

### 8.1 RemoteChannel 生命周期

```
tachi 启动
  │
  ├── Manager.Start() ──► 为每个 enabled backend 启动 goroutine
  │                          │
  │                     RemoteChannel.Run(ctx, handler)
  │                          │
  │                     ┌────▼────────────────────────────────┐
  │                     │  连接循环 (connect → recv → 断开)    │
  │                     │                                     │
  │                     │  connect() 失败 → 退避 → 重试       │
  │                     │  recvLoop 中连接断开 → 退避 → 重试   │
  │                     │  ctx.Done() → 退出循环              │
  │                     └────────────────────────────────────┘
  │
  ▼
tachi 退出 (ctx cancelled)
  ├── 所有 RemoteChannel goroutine 退出
  └── Manager 清理
```

### 8.2 错误场景

| 场景 | Channel 侧行为 | tachi 侧行为 |
|------|---------------|-------------|
| Channel 进程未启动 | — | RemoteChannel 退避重试，日志告警 |
| Channel 启动在 tachi 之后 | tachi connect → socket 不存在 → 退避重试 | — |
| tachi 重启 | 连接断开，回到 accept | 新进程连接 |
| Channel 进程崩溃 | 进程退出，socket 消失 | connect 失败，退避重试 |
| 消息处理 LLM 超时 | — | response 携带 error（或空内容） |
| 消息处理 panic | — | recover → error frame |
| 网络分区 (TCP 模式) | 连接断开 | TCP keepalive 检测，重连 |

### 8.3 优雅关闭

tachi 退出时：
1. `ctx` 被 cancel
2. `RemoteChannel.Run()` 检测到 → 关闭当前 conn
3. 给 channel 进程留一定时间处理剩余 response（由 channel 进程自行负责）

Channel 进程退出时：
1. Socket 关闭
2. tachi 的 `recvLoop` 读端返回 `io.EOF`
3. 进入退避重试

---

## 9. 安全考量

### 9.1 Unix Socket 权限

- Socket 文件创建时设置 `0700`，仅 owner 可读写
- tachi 和 channel 进程应以同一用户运行

### 9.2 TCP 安全

- 默认只允许 `127.0.0.1` 绑定（loopback）
- 跨机部署场景：
  - 建议配合 WireGuard / mTLS
  - 或在反向代理层面加 TLS termination
  - 本文档不内置传输加密——保持协议层纯粹

### 9.3 认证

当前版本不需要认证——socket 文件权限 / loopback 绑定已提供足够的隔离。未来如需认证，可在握手阶段加入 token 校验：

```
Channel 端 accept 后的第一帧:
  {"type":"hello","version":1,"channel":"weixin"}

tachi 回复:
  {"type":"welcome","version":1}
```

当前版本：连接建立即视为可用。

---

## 10. 与 MCP 集成的对比

| 维度 | MCP | Channel Remote |
|------|-----|---------------|
| **角色** | tachi 是 client，MCP server 提供工具 | Channel 端 serve，tachi 连接 |
| **传输** | stdio 子进程 / HTTP | Unix socket / TCP |
| **协议风格** | JSON-RPC 2.0 | 极简 JSON-line |
| **消息方向** | 请求-响应 | 双向异步推送 |
| **并发** | 单请求流 | 多消息流水线 |
| **OAuth** | 支持（HTTP 模式） | 不需要（socket 权限替代） |
| **连接数** | 每 server 一个进程/连接 | 每 backend 一个连接 |

两者的设计理念一致：
- 都用 JSON-line 风格的帧协议
- 都通过 config 驱动，tachi 本身不 hardcode 集成
- 都支持独立进程、独立生命周期

---

## 11. 迁移路径

### 11.1 阶段 1：基础设施（不影响现有功能）

1. 新增 `channel/remote.go` — `RemoteChannel` 实现
2. 新增 `config.RemoteChannelConfig` 结构体
3. 在 `ChannelConfig` 中同时保留 `Weixin` 和 `Backends` 字段（兼容期）

### 11.2 阶段 2：weixin 迁移

1. 将 `channel/weixin/` 抽取到独立仓库 `tachi-channel-weixin`
2. 实现 JSON-line 协议和 Unix socket listen
3. 在 tachi 侧默认配置中通过 `channel.backends` 连接
4. 验证功能无误后：
   - 删除 tachi 仓库中的 `channel/weixin/`
   - 删除 `config.WeixinConfig`
   - 删除 `main.go` 中的 weixin import

### 11.3 阶段 3：推广

新增其他 IM channel 时，只需：
1. 在外部仓库实现 JSON-line 协议
2. 在 `config.yaml` 中增加一个 `backends` 条目
3. 部署 channel 二进制并启动

ENDDOFFILE
echo "--- part4 done ---"

---

## 12. 附录：Go 实现骨架

### 12.1 tachi 侧：`channel/remote.go`

```go
package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// RemoteChannel implements the Channel interface by connecting to an external
// channel process over Unix socket or TCP.
//
// Connection model: the external channel process listens; RemoteChannel
// connects as a client. This keeps the channel process independently
// manageable — it can start/restart without affecting tachi.
type RemoteChannel struct {
	name      string
	transport string // "unix" | "tcp"
	address   string // socket path or host:port

	mu      sync.Mutex
	conn    net.Conn
	handler MessageHandler
	logger  *debuglog.Logger
}

// NewRemoteChannel creates a RemoteChannel.
//
//   - transport: "unix" (default) or "tcp"
//   - address:  socket path (unix) or "host:port" (tcp)
func NewRemoteChannel(name, transport, address string) *RemoteChannel {
	return &RemoteChannel{
		name:      name,
		transport: transport,
		address:   address,
		logger:    debuglog.DefaultLogger.WithSource("channel:remote:" + name),
	}
}

// Name returns the channel identifier.
func (rc *RemoteChannel) Name() string { return rc.name }

// Run connects to the external channel process and enters the message loop.
// It retries connections with exponential backoff. Returns on ctx cancellation
// or unrecoverable error.
func (rc *RemoteChannel) Run(ctx context.Context, handler MessageHandler) error {
	rc.handler = handler

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		conn, err := rc.dial()
		if err != nil {
			rc.logger.Log("remote %s: connect: %v (retry in %v)",
				rc.name, err, backoff)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		rc.logger.Log("remote %s: connected", rc.name)
		backoff = time.Second // reset on successful connection

		rc.mu.Lock()
		rc.conn = conn
		rc.mu.Unlock()

		rc.receiveLoop(ctx, conn)

		rc.mu.Lock()
		rc.conn = nil
		rc.mu.Unlock()

		rc.logger.Log("remote %s: disconnected", rc.name)
	}
}

// dial creates a connection based on transport type.
func (rc *RemoteChannel) dial() (net.Conn, error) {
	timeout := 10 * time.Second
	switch rc.transport {
	case "tcp":
		return net.DialTimeout("tcp", rc.address, timeout)
	default: // "unix" or empty
		return net.DialTimeout("unix", rc.address, timeout)
	}
}

// receiveLoop reads JSON-line frames from conn and dispatches to handler.
func (rc *RemoteChannel) receiveLoop(ctx context.Context, conn net.Conn) {
	reader := bufio.NewReader(conn)
	// Serialize writes to avoid frame interleaving.
	var writeMu sync.Mutex

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				rc.logger.Log("remote %s: read: %v", rc.name, err)
			}
			conn.Close()
			return
		}

		var frame frameMsg
		if err := json.Unmarshal(line, &frame); err != nil {
			rc.logger.Log("remote %s: bad frame: %v", rc.name, err)
			continue
		}

		switch frame.Type {
		case "message":
			// Process each message in its own goroutine.
			go rc.processMessage(ctx, frame, conn, &writeMu)

		default:
			rc.logger.Log("remote %s: unknown frame type: %s", rc.name, frame.Type)
		}
	}
}

// processMessage calls the handler and writes the response back.
func (rc *RemoteChannel) processMessage(
	ctx context.Context, frame frameMsg, conn net.Conn, writeMu *sync.Mutex,
) {
	msg := IncomingMessage{
		ThreadID:  frame.ThreadID,
		MessageID: frame.MessageID,
		Content:   frame.Content,
		ChannelID: frame.ChannelID,
	}

	rc.logger.Log("remote %s: msg id=%s thread=%s len=%d",
		rc.name, frame.ID, msg.ThreadID, len(msg.Content))

	out, err := rc.handler(ctx, msg)

	reply := frameReply{
		Type:     "response",
		ID:       frame.ID,
		ThreadID: msg.ThreadID,
		Content:  out.Content,
		ReplyTo:  msg.MessageID,
	}

	if err != nil {
		reply.Type = "error"
		reply.Error = err.Error()
	}

	data, _ := json.Marshal(reply)
	data = append(data, '\n')

	writeMu.Lock()
	conn.Write(data)
	writeMu.Unlock()
}

// --- Frame types (shared between tachi and channel implementations) ---

// frameMsg is sent by the channel implementation when an IM message arrives.
type frameMsg struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ThreadID  string `json:"thread_id"`
	MessageID string `json:"message_id"`
	Content   string `json:"content"`
	ChannelID string `json:"channel_id"`
}

// frameReply is sent by tachi in response to a frameMsg.
type frameReply struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	ThreadID string `json:"thread_id"`
	Content  string `json:"content"`
	ReplyTo  string `json:"reply_to"`
	Error    string `json:"error,omitempty"`
}

// --- Helpers ---

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
```

### 12.2 main.go 变化（runChannels 函数）

```go
func runChannels(ctx context.Context, cmd *cli.Command) error {
	// ... init debuglog, load config ...

	manager := channel.NewManager(channel.ManagerConfig{
		Config:       cfg,
		SystemPrompt: buildSystemPrompt(cfg.Language),
	})

	hadAny := false

	for _, bcfg := range cfg.Channel.Backends {
		if !bcfg.Enabled {
			continue
		}
		transport := bcfg.Transport
		if transport == "" {
			transport = "unix"
		}
		addr := bcfg.Address
		if addr == "" && transport == "unix" {
			addr = "/tmp/tachi-" + bcfg.Name + ".sock"
		}
		if addr == "" {
			return fmt.Errorf("channel %s: address required for tcp transport", bcfg.Name)
		}
		ch := channel.NewRemoteChannel(bcfg.Name, transport, addr)
		manager.Add(ch)
		hadAny = true
		fmt.Printf("[channel] %s backend registered (%s://%s)\n", bcfg.Name, transport, addr)
	}

	// Temporary: support legacy channel.weixin config
	if !hadAny && cfg.Channel.Weixin.Enabled {
		fmt.Println("[channel] WARNING: channel.weixin is deprecated, use channel.backends instead")
		// ... legacy fallback ...
	}

	if !hadAny {
		return fmt.Errorf("no channels enabled in config; add entries to channel.backends")
	}

	// ... Start, block ...
}
```

### 12.3 Channel 侧实现骨架（独立仓库）

```go
// Package main — example channel implementation for a hypothetical IM.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
)

// --- Protocol frames (duplicated from channel package for independence) ---

type Message struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ThreadID  string `json:"thread_id"`
	MessageID string `json:"message_id"`
	Content   string `json:"content"`
	ChannelID string `json:"channel_id"`
}

type Reply struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	ThreadID string `json:"thread_id"`
	Content  string `json:"content"`
	ReplyTo  string `json:"reply_to"`
	Error    string `json:"error,omitempty"`
}

func main() {
	addr := "/tmp/tachi-myim.sock"
	os.Remove(addr)

	ln, err := net.Listen("unix", addr)
	if err != nil {
		panic(err)
	}
	os.Chmod(addr, 0700)
	fmt.Printf("channel-myim: listening on %s\n", addr)

	// --- Connect to IM backend ---
	go connectToIMBackend()

	// --- Accept tachi connection loop ---
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-sigCh:
				fmt.Println("\nchannel-myim: shutting down")
				os.Remove(addr)
				return
			default:
				fmt.Printf("channel-myim: accept: %v\n", err)
				continue
			}
		}
		fmt.Println("channel-myim: tachi connected")
		handleConn(conn)
		fmt.Println("channel-myim: tachi disconnected")
	}
}

// IM message queue — messages from IM backend waiting to be forwarded.
var msgQueue = make(chan Message, 64)

func connectToIMBackend() {
	// Connect to your IM backend (polling, websocket, etc.).
	// When a message arrives, push to msgQueue:
	//
	//   msgQueue <- Message{
	//     Type:      "message",
	//     ID:        generateID(),
	//     ThreadID:  msg.FromUser,
	//     MessageID: msg.ID,
	//     Content:   msg.Text,
	//   }
}

func handleConn(conn net.Conn) {
	defer conn.Close()

	// Reader goroutine — process replies from tachi, send to IM.
	go func() {
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var reply Reply
			if err := json.Unmarshal(line, &reply); err != nil {
				continue
			}
			if reply.Type == "response" || reply.Type == "error" {
				// Send reply.Content to IM backend using reply.ReplyTo
				sendToIMBackend(reply.ThreadID, reply.Content, reply.ReplyTo)
			}
		}
	}()

	// Writer loop — forward IM messages to tachi.
	for {
		select {
		case msg := <-msgQueue:
			data, _ := json.Marshal(msg)
			data = append(data, '\n')
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}
}

func sendToIMBackend(threadID, content, replyTo string) {
	// Call your IM API to send the message.
	fmt.Printf("SEND to %s (reply %s): %s\n", threadID, replyTo, content)
}
```

---

## 附录 B：配置完整示例

### 开发环境（同机，Unix Socket）

```yaml
# ~/.tachi/config.yaml
channel:
  backends:
    - name: weixin
      transport: unix
      address: /tmp/tachi-weixin.sock
      enabled: true
    
    - name: enterprise-im
      transport: unix
      address: /tmp/tachi-ent-im.sock
      enabled: true
```

### 容器环境（TCP）

```yaml
channel:
  backends:
    - name: weixin
      transport: tcp
      address: 10.0.1.5:9741
      enabled: true
```

### 启动顺序

```bash
# 1. 先启动所有 channel 进程
./tachi-channel-weixin &
./tachi-channel-lark &

# 2. 再启动 tachi（它会 connect 到 channel）
./tachi channel
```

---

## 附录 C：待定事项（Open Questions）

1. **流式响应**：当前 response 帧一次性返回完整 Agent 回复。如果后续需要支持"边生成边发送"（IM 端显示 typing + 逐段输出），需引入 `delta` 帧。

2. **心跳**：当前依赖 TCP keepalive 检测连接状态。是否需要应用层心跳（`{"type":"ping"}` / `{"type":"pong"}`）？

3. **并发上限控制**：如果 channel 流水线发送大量 message 帧，后端 LLM 调用压力可能过大。是否需要在 tachi 侧限制并发 handler 调用数？（当前无限制，与 channel 数量 × 消息频率相关）

4. **二进制附件**：图片、文件等媒体，是走连接内 base64，还是走文件系统约定路径？（建议文件系统路径，避免协议膨胀）

5. **metrics/logs**：RemoteChannel 是否需要暴露 Prometheus 指标（连接状态、消息量）？
