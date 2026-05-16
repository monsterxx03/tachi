# /restart 命令 — 热重启自己

> 版本: 2.0 | 日期: 2026-05-16 | 状态: 设计阶段
> 关联: [Config Hot-Reload](./2026-05-16-config-hot-reload.md)
>
> 变更: v2.0 从 `syscall.Exec` 改为 `os.Exit(42)` + systemd `RestartForceExitStatus`

## 一、问题

Tachi 在 channel 模式下（微信/Telegram）是一个 systemd 管理的常驻后台进程。
用户让我改自己代码后，需要我能**自己重启以加载新代码**。

当前做不到——改完代码必须有人 ssh 上去跑 `systemctl restart tachi`。

## 二、核心思路：exit(42) -> systemd 重启

systemd 本身就是进程管理器。不需要在 Go 里模拟进程重启。

```
用户: /restart
       ↓
os.Exit(42)
       ↓
systemd 检测到退出码 42
       ↓
RestartForceExitStatus=42 → 启动新进程
       ↓
新进程读取新二进制、新 config.yaml
       ↓
"🔄 重启完成"
```

### 为什么不是别的方式

| 方案 | 问题 |
|------|------|
| `syscall.Exec` | **多此一举**。systemd 本来就是做进程管理的，我在 Go 里重写一遍？ |
| `systemctl restart` | 需要 root 权限或 polkit，Tachi 以普通用户运行的话搞不定 |
| `SIGUSR1` + 信号处理 | 可以，但不如 exit code 简单——信号处理要在进程里等当前 turn 结束，exit code 可以 `os.Exit` 之前自己决定什么时候 exit |

### 退出码约定

```ini
[Service]
ExecStart=/usr/local/bin/tachi channel
Restart=on-failure          # 非 0 退出重启，0 退出不重启
RestartForceExitStatus=42   # 强制：42 也算"要重启"
RestartSec=3
```

| 操作 | 退出码 | systemd 行为 |
|------|--------|-------------|
| `/restart` | 42 | **重启**（`RestartForceExitStatus` 生效） |
| crash / panic | 非 0 | **重启**（`Restart=on-failure` 生效） |
| 正常关机 | 0 | **不重启**（`Restart=on-failure` 不触发） |

`RestartForceExitStatus=42` 的意思是：**即使 `Restart=on-failure` 的策略认为这个退出码不需要重启，但只要它是 42，就给我重启。**

这样三个场景互不干扰：

- 崩溃 → systemd 重启它（本来就会）
- `/restart` → systemd 重启它（靠 42 触发）
- 正常停机 → systemd 不重启（靠 0 抑制）

## 三、命令接口

```
/restart          → 等当前 turn 结束 → 保存 session → os.Exit(42)
/restart force    → 立即退出（当前 turn 丢了不管）
```

### channel 模式下的行为

```
用户输入 /restart
       ↓
channel manager 拦截，不交给 LLM
       ↓
Graceful 模式：等当前在跑的 RunConversationStream 结束
Force 模式：不等，直接走
       ↓
mgr.Stop() 断开所有 channel 连接
       ↓
os.Exit(42)
       ↓
systemd 看到 42 → 3 秒后拉起新进程
新进程加载新二进制 → 重连 channel → 等用户说话
```

### TUI 模式下的行为

```
> /restart
⚠ TUI 模式下你直接 Ctrl+C 重新跑就行。
  或者用 /config reload 来重读配置。
```

TUI 前台进程不走 systemd，没必要用这个命令。

## 四、实现细节

### 4.1 改动量：极小

| 改什么 | 在哪里 | 行数 |
|--------|--------|------|
| 拦截 `/restart` slash command | `channel/manager/manager.go` | ~20 行 |
| `mgr.Restart()` 方法 | 同上 | ~15 行 |
| 广播"正在重启"消息 | 同上 | ~5 行 |
| **总计** | | **~40 行** |

### 4.2 核心代码

```go
// channel/manager/manager.go

const RestartExitCode = 42

// handleSlashCommand 处理用户输入中的 slash 命令。
// 返回 true 表示已消费（不再发给 LLM）。
func (m *Manager) handleSlashCommand(msg *Message) bool {
    text := strings.TrimSpace(msg.Content)

    switch {
    case text == "/restart":
        go m.restart(RestartGraceful)
        return true
    case text == "/restart force":
        go m.restart(RestartForce)
        return true
    }

    return false // 不是 slash 命令，正常走 LLM
}

type RestartMode int
const (
    RestartGraceful RestartMode = iota
    RestartForce
)

func (m *Manager) restart(mode RestartMode) {
    // 1. 发通知
    m.broadcast("🔄 Tachi 正在重启，请稍候...")

    // 2. 如果是 graceful 模式，等当前 turn 结束
    if mode == RestartGraceful {
        m.waitForCurrentTurn()
    }

    // 3. 停掉所有 channel
    m.stopAll()

    // 4. session 已经是实时写入 JSONL 的，不用额外保存
    //    MCP 子进程由 systemd 自动收孤儿
    //    数据都在磁盘上了

    // 5. 退出，让 systemd 处理重启
    os.Exit(RestartExitCode)
}
```

### 4.3 `waitForCurrentTurn` 的简单实现

channel manager 在 `processMessage` 里调用 `RunConversationStream` 时，
记录一个 `inFlight` 计数器。`/restart` 时轮询这个计数器归零。

```go
func (m *Manager) waitForCurrentTurn() {
    for i := 0; i < 300; i++ { // 最多等 5 分钟
        if m.inFlight.Load() == 0 {
            return
        }
        time.Sleep(time.Second)
    }
    // 超时了？不等了，直接退走人
    m.log("restart: grace period exceeded, forcing restart")
}
```

### 4.4 不需要的改动

| 不需要做 | 原因 |
|---------|------|
| ❌ 保存 session | `AppendMessage` 已经是实时写入磁盘的 |
| ❌ 杀 MCP 子进程 | systemd 接管了进程组，会自动清理 |
| ❌ `syscall.Exec` | 整个设计 v1 的核心，现在全部删掉 |
| ❌ 复杂的生命周期管理 | 直接 `os.Exit` 完事，systemd 管一切 |

## 五、安全边界

| 场景 | 行为 |
|------|------|
| `/restart` 时 LLM 正在回复 | Graceful 模式等 `inFlight == 0` |
| `/restart force` | 不等，直接 os.Exit |
| escape 分析发现不需要等待 channel 关闭 | 不关闭其实也行——核都爆了，channel 自己会断 |
| 新二进制有问题（启动就崩） | systemd 会尝试重启，`RestartSec=3` 防止烧 CPU |
| | `StartLimitInterval=30` + `StartLimitBurst=3` 防止无限循环 |
| 用户重复输入 `/restart` | 第二个请求看到已在重启中，忽略 |
| 正在跑 subagent | subagent 记录已写入 `subagent/<id>.jsonl`，重启后可以恢复 |

## 六、systemd unit 完整配置

```ini
[Unit]
Description=Tachi AI Agent (Channel Mode)
After=network.target

[Service]
Type=simple
User=will
ExecStart=/usr/local/bin/tachi channel

# /restart 退出码 → 触发重启
Restart=on-failure
RestartForceExitStatus=42
RestartSec=3

# 防止快速崩溃死循环
StartLimitInterval=30
StartLimitBurst=3
StartLimitAction=start

[Install]
WantedBy=default.target
```

## 七、和已有命令的对比

```
/config reload    → 不改二进制，只改 YAML
                    适合：改语言、改模型、改 API key
                    hot 配置秒生效，不用重启

/restart          → 加载新二进制
                    适合：改了代码、重新编译、部署了新版本
                    os.Exit(42) → systemd 拉起

手动 systemctl    → 管理员远程操作
                    适合：改 unit 文件、升级 tachi 包
```

三者没有重叠，各自解决各自的问题。

## 八、不做的事

| 不做 | 原因 |
|------|------|
| ❌ 自动检测二进制变化重启 | 留着以后想做再做 |
| ❌ Docker 兼容 | 你有 systemd，不需要 |
| ❌ Mac launchd 支持 | 你跑在 Linux 上 |
| ❌ Windows 服务支持 | 同上 |
