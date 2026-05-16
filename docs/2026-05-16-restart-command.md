# /restart 命令 — 热重启自己

> 版本: 1.0 | 日期: 2026-05-16 | 状态: 设计阶段
> 关联: [Config Hot-Reload](./2026-05-16-config-hot-reload.md),
>       [session 存储](./2026-05-10-session-replace-transcript.md)

## 一、问题

Tachi 在 channel 模式下（微信/Telegram）是一个常驻后台进程。
用户让我改自己代码后，需要我能**自己重启以加载新代码**。

当前做不到——改完代码必须有人去终端 `systemctl restart tachi` 或
kill 进程再拉起来。

## 二、核心思路：syscall.Exec

不用退出进程再让 systemd 拉起来。直接用 `syscall.Exec` **原地替换当前进程**：

```
                    ┌──────────────────────┐
                    │    tachi (PID 1234)    │
                    │  channel manager 跑着  │
                    │  正在处理用户消息...     │
                    └──────────┬───────────┘
                               │ 用户输入 /restart
                               ▼
                    ┌──────────────────────┐
                    │ 1. 保存当前 session    │
                    │ 2. 断开 channel 连接   │
                    │ 3. 杀掉 MCP 子进程     │
                    │ 4. syscall.Exec()     │
                    └──────────┬───────────┘
                               │
                    ┌──────────▼───────────┐
                    │    tachi (PID 1234)    │  ← PID 不变！
                    │  重新加载 config.yaml  │
                    │  重连 channel          │
                    │  开始处理用户消息       │
                    └──────────────────────┘
```

**为什么 syscall.Exec 而不是退出让 systemd 重启：**

| 方案 | PID | systemd 感知 | 需要 sudo |
|------|-----|-------------|-----------|
| 退出 → systemd 拉起来 | 变 | 看到进程退出，可能误判为崩溃 | 不需要 |
| `syscall.Exec` | **不变** | **零感知，永远不知道你重启过** | 不需要 |
| `systemctl restart` | 变 | 正常流程 | 需要 |

**syscall.Exec 的代价**：当前 goroutine 全部暴毙。所以重启前必须：
1. 保存 session 到磁盘
2. 断开 channel（让用户看到"正在重启"）
3. 杀掉 MCP 子进程（否则变孤儿）

## 三、命令接口

```
/restart          → 等当前 turn 结束 → 保存状态 → exec
/restart force    → 立即 exec（当前正在跑的消息丢了别怪我）
```

channel 模式下的行为：

```
用户: /restart
        ↓
Agent 检测到这是 slash command，不交给 LLM
        ↓
channel manager 停掉当前正在处理的 turn（force 则立即停）
        ↓
保存所有 session 到磁盘
        ↓
遍历 MCP servers，杀掉子进程
        ↓
断开 channel 连接，发送最后一条 "🔄 Tachi 正在重启..."
        ↓
syscall.Exec("/usr/local/bin/tachi", os.Args, os.Environ())
        ↓
新进程启动，重读 config.yaml
重连 channel
"🔄 重启完成，可以继续了"
```

## 四、实现细节

### 4.1 channel.Manager 新增 Restart 方法

```go
// channel/manager/manager.go

type RestartMode int
const (
    RestartGraceful RestartMode = iota  // 等当前 turn 结束
    RestartForce                         // 立即重启
)

// Restart 保存状态、断开连接、退出当前进程。
// 返回一个 error 让调用者执行 syscall.Exec。
// 永不返回（除非出错）。
func (m *Manager) Restart(ctx context.Context, mode RestartMode) error {
    // 1. 广播重启通知到所有 channel
    m.broadcast("🔄 Tachi 正在重启...")

    // 2. 停掉 cron scheduler
    if m.scheduler != nil {
        m.scheduler.Stop()
    }

    // 3. 停掉所有 channel
    m.stopAll()

    // 4. 保存 session（每个 session 的最后一条消息）
    if m.agent != nil && m.agent.SessionManager() != nil {
        // session manager 的 JSONL 已经是实时写入的
        // 只需确保关闭文件
    }

    // 5. 断开 MCP 连接（杀掉子进程）
    if m.mcpMgr != nil {
        m.mcpMgr.Close()
    }

    // 6. 返回，让调用者 exec
    return nil
}
```

### 4.2  main.go 集成

```go
// main.go

func runChannels(ctx context.Context, cmd *cli.Command) error {
    // ... 现有初始化逻辑 ...

    if err := mgr.Start(ctx); err != nil {
        return err
    }

    // 新增：等待 restart 信号
    select {
    case <-ctx.Done():
        // 正常退出（SIGINT/SIGTERM）
        return nil
    case <-mgr.RestartCh():
        // 收到 /restart 命令，准备 exec
    }

    // 执行 syscall.Exec
    binary, _ := os.Executable()
    // 保留原始参数和环境变量
    syscall.Exec(binary, os.Args, os.Environ())

    return nil
}
```

### 4.3  `/restart` 在 channel 层处理

不能在 LLM 层处理——LLM 收到 `/restart` 时已经在 agent loop 里，
等它返回再处理会增加复杂度和竞态。

```go
// channel/manager/manager.go

func (m *Manager) handleSlashCommand(cmd string) bool {
    switch {
    case cmd == "/restart":
        // 直接处理，不经过 LLM
        m.restartCh <- struct{}{}
        return true
    case cmd == "/restart force":
        m.restartCh <- struct{}{}
        return true
    }
    return false  // 不是 slash command，交给 LLM
}
```

### 4.4  TUI 模式下的 /restart

TUI 模式下 `syscall.Exec` 不太好——TUI 进程通常跑在前台，
用户重启的目的是加载新代码。但 TUI 有 `/config reload` 就够了。
所以 TUI 下 `/restart` = 礼貌地告诉你"请在终端里重启"：

```
> /restart
⚠ TUI 模式下不支持进程内重启。
  请退出并重新运行 tachi。
```

或者也可以 exec——用户可能改了代码之后想快速验证。
实际上 exec 在 TUI 下也完全可行。保持一致更好。

## 五、安全边界

| 场景 | 行为 |
|------|------|
| `/restart` 时 LLM 正在回复 | Graceful 模式等 TurnComplete → 再 Exec |
| `/restart force` 时 LLM 正在回复 | 直接终止 → 丢掉当前 turn |
| syscall.Exec 失败（二进制被删了） | 报错退出，systemd 会拉起来 |
| 有 MCP 子进程在跑 | Graceful 模式先停掉 |
| 有 subagent 在跑 | Subagent 记录已写入 `subagent/<id>.jsonl`，不丢 |
| YAML 语法错误导致新进程启动失败 | 新进程退出 → systemd 再次拉起 → 死循环？ |
| | → 需要退避机制：如果 30 秒内重启超过 3 次，等 60 秒再试 |

## 六、与 /config reload 的关系

```
/config reload          → 只重读 YAML，不重启进程（hot 配置秒生效）
                          不改代码，不改 provider，不改 channel

/restart                → 整个进程 exec，加载新编译的二进制
                          用于"我改了代码，重新编译了，让我试试"

/config reload --warm   → 介于两者之间：重连 MCP、换 provider
                          不 exec，不丢 PID
```

## 七、systemd unit 推荐配置

```ini
[Service]
ExecStart=/usr/local/bin/tachi channel
Restart=always
RestartSec=5
# 炸了等 5 秒再起，防止快速崩->重启死循环
```

不需要特殊的退出码处理——syscall.Exec 进程从没退出过，
systemd 永远看不到进程终止。

## 八、进阶：持续集成后的自动化重启

除了手工 `/restart`，还可以结合文件监听：

```go
// 当检测到 /usr/local/bin/tachi 文件被替换时（比如 CI 部署了新版本），
// 自动触发优雅重启。
//
// Phase 2 可以考虑加一个 --watch 模式：
//   tachi channel --watch
// 用 fsnotify 监控二进制文件的 mtime 变化。
```

但这是 Phase 2——先把手动 `/restart` 跑通。

## 九、不做的事

| 不做 | 原因 |
|------|------|
| ❌ systemctl restart | 需要 sudo 权限，`syscall.Exec` 不需要 |
| ❌ Docker restart | 容器环境用 `SIGTERM` + 健康检查即可，exec 也行 |
| ❌ 热加载 Go 插件 (plugin) | Go plugin 限制太多（版本必须完全一致），不如 exec 干净 |
| ❌ 二进制文件自更新 | 安全风险太高，交给 CI/CD 做 |
