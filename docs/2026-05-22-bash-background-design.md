# Bash 后台进程支持 — 设计文档

> 版本: 1.0 | 日期: 2026-05-22 | 状态: 设计阶段

## 一、概述

当前 Bash tool 是纯同步执行的：`ExecuteContext` 启动命令后阻塞直到命令退出，返回 stdout/stderr/exit code。对于需要跨 tool-call 生命周期的长驻进程（如 HTTP server、dev server、watch 模式），当前设计不满足需求。

本方案参考 [Claude Code](https://github.com/anthropics/claude-code) 的 LocalShellTask 设计，为 tachi 的 Bash tool 增加后台进程管理能力，分两个阶段实施。

## 二、现状分析

### 2.1 当前 Bash tool 执行流程

```
agent/tools/bash.go: ExecuteContext()
    │
    ├─ exec.CommandContext(ctx, "bash", "-c", cmd)
    │     └─ ctx 带 timeout（默认 120s，最大 600s）
    │
    ├─ cmd.Run()  ← 阻塞等待命令退出
    │
    └─ 返回 BashResult{stdout, stderr, exitCode, durationMs}
```

关键约束：

- `Parallel()=false`：Bash 独占 agent loop，一次只能跑一个
- `cmd.Run()` 是同步阻塞的——命令不退出，tool call 不返回
- 没有跨 tool-call 的进程状态保持能力

### 2.2 问题场景

```
用户: 帮我启动 dev server 然后验证首页返回 200

LLM 期望:
  1. Bash: python -m http.server 8080   ← 后台启动
  2. Bash: curl localhost:8080          ← 验证
  3. Bash: kill <pid>                    ← 关闭

实际:
  1. Bash: python -m http.server 8080   ← 永远阻塞，超时 120s 后被 kill
  2. (无法到达)
```

当前唯一的 workaround 是 `&` + `nohup`，但存在严重缺陷：

| 缺陷 | 说明 |
|------|------|
| PID 靠 LLM 记忆 | 长对话中容易遗忘，无法停止 |
| 无生命周期管理 | Agent 退出后进程变孤儿（reparent 到 init） |
| 无输出捕获 | stderr 丢失，无法判断进程是否健康 |
| 非跨平台 | Windows 不支持 `&` |

## 三、Claude Code 参考设计

Claude Code 的 Bash 后台任务是一套完整的生命周期管理系统，核心概念如下：

### 3.1 ShellCommand 状态机

```
running ──background(taskId)──→ backgrounded ──[进程退出]──→ completed / failed
   │                                  │
   └──── kill() ────→ killed          └── kill() ──→ killed
```

状态由 `ShellCommand` 对象自身管理，不依赖 BashTool 的 `ExecuteContext` 返回。

### 3.2 三种后台化入口

| 入口 | 触发方式 | 说明 |
|------|---------|------|
| 主动后台 | `run_in_background: true` | LLM 明确指定后台运行 |
| 自动后台 | 命令运行超过 `ASSISTANT_BLOCKING_BUDGET_MS`（15s） | assistant mode 下自动转后台，避免阻塞对话 |
| 用户手动 | `Ctrl+B` | 用户快捷键（tachi 场景暂不需要） |

### 3.3 关键机制

**输出持久化**：后台进程的 stdout/stderr 直接写到磁盘文件（fd 重定向），不经过 JS 内存。按需读取（`ReadFile` tool 读输出文件）。

**完成通知**：进程退出后，通过 `<task_notification>` XML 块注入到 LLM 消息流：

```xml
<task_notification>
  <task_id>xxx</task_id>
  <output_file>/tmp/output.log</output_file>
  <status>completed</status>
  <summary>Background command "npm test" completed (exit code 0)</summary>
</task_notification>
```

**卡死检测（Stall Watchdog）**：每 5s 检查输出文件大小。若 45s 无增长且最新一行输出匹配交互式提示模式（`(y/n)`、`Continue?` 等），注入通知提醒模型 kill 后重跑。

**进程树清理**：使用 `treeKill(pid, SIGKILL)` 杀整个进程树。Agent 退出时遍历 `killShellTasksForAgent(agentId)` 清理所有残留。

**大输出保护**：后台任务输出写到磁盘而非内存，另有文件大小看门狗——超限自动 SIGKILL。

### 3.4 与 tachi 的映射

| Claude Code 概念 | tachi 对应 |
|------------------|-----------|
| `ShellCommand` 对象 | `agent/tools/process_manager.go` — 新文件 |
| `LocalShellTask` / `Task` | 映射到 `ManagedProcess` |
| `<task_notification>` | 新的 `BackgroundTaskReminder`（system-reminder 机制） |
| `treeKill` | Go: `syscall.Kill(-pid, SIGKILL)` + `Setpgid: true` |
| `TaskOutput` 文件持久化 | `os.CreateTemp` + fd 重定向 |
| `killShellTasksForAgent` | `ProcessManager.KillAll()`（agent 退出时调用） |

## 四、分阶段实施

### Phase 1（最小可行）：后台启动 + 生命周期管理

**范围**：

- Bash tool 新增 `background: bool` 参数
- 全局 `ProcessManager`：管理后台进程的启动、停止、列表查询
- 进程组隔离（`Setpgid: true`），确保 kill 时子进程也一并终止
- Agent 退出时自动清理所有后台进程

**不包含**：

- 输出持久化（输出仍走内存 buffer，同前台命令）
- 完成通知注入
- 卡死检测
- 大输出保护

**够用场景**：启动 server → 跑验证 → kill server。进程生命周期 ≤ agent 生命周期。

### Phase 2（体验提升）：输出文件化 + 异步通知

**范围**：

- 后台进程输出写到临时文件，不限大小
- `BackgroundTaskReminder`：后台任务完成后作为 `<system-reminder>` 注入下一条 LLM 消息
- 大输出文件大小看门狗（可选的 safe guard）
- 卡死检测（可选，后续迭代）

**够用场景**：长后台任务（如 `npm install`、`make build`），完成后自动通知 LLM。

## 五、Phase 1 详细设计

### 5.1 新增文件：`agent/tools/process_manager.go`

```
agent/tools/
├── bash.go                  ← 修改：增加 background/stop_id/list_bg 参数
├── process_manager.go       ← 新增：后台进程管理器
└── process_manager_test.go  ← 新增：测试
```

#### 5.1.1 ProcessManager 结构

```go
package tools

import (
    "context"
    "os/exec"
    "sync"
    "syscall"
    "time"
)

// ProcessManager manages background processes started by Bash tool.
// It is a singleton (package-level) shared across all BashTool instances.
type ProcessManager struct {
    mu        sync.Mutex
    processes map[string]*ManagedProcess // key = process name
}

// ManagedProcess represents a single background process.
type ManagedProcess struct {
    Name       string        // user-defined name for referencing
    Command    string        // original command string
    Cmd        *exec.Cmd    // the running command
    PID        int          // OS process ID
    StartedAt  time.Time
    Status     ProcessStatus
    ExitCode   int
    ExitErr    error
    lastStdout []byte       // ring buffer for recent output (max 64KB)
    lastStderr []byte
    mu         sync.Mutex
}

type ProcessStatus string

const (
    ProcessRunning  ProcessStatus = "running"
    ProcessExited   ProcessStatus = "exited"
    ProcessKilled   ProcessStatus = "killed"
    ProcessError    ProcessStatus = "error"  // failed to start
)

// ManagedProcessInfo is the JSON-serializable summary returned to the LLM.
type ManagedProcessInfo struct {
    Name       string        `json:"name"`
    PID        int           `json:"pid"`
    Command    string        `json:"command"`
    Status     ProcessStatus `json:"status"`
    ExitCode   int           `json:"exitCode,omitempty"`
    StartedAt  string        `json:"startedAt"`
    Uptime     string        `json:"uptime,omitempty"`
    Error      string        `json:"error,omitempty"`
    RecentStdout string      `json:"recentStdout,omitempty"`
    RecentStderr string      `json:"recentStderr,omitempty"`
}
```

#### 5.1.2 核心方法

```go
// Start starts a command in background. Returns ManagedProcessInfo.
// If a process with the same name already exists, returns an error
// (caller must Stop first or use a different name).
func (pm *ProcessManager) Start(ctx context.Context, name, command string) (*ManagedProcessInfo, error)

// Stop stops a background process by name. Sends SIGTERM first, then
// SIGKILL after a 5s grace period. Cleans up the managed process entry.
func (pm *ProcessManager) Stop(name string) (*ManagedProcessInfo, error)

// List returns info for all tracked processes.
func (pm *ProcessManager) List() []ManagedProcessInfo

// KillAll stops all tracked processes. Called on agent shutdown.
func (pm *ProcessManager) KillAll()

// Get returns info for a specific process, or nil if not found.
func (pm *ProcessManager) Get(name string) *ManagedProcessInfo
```

#### 5.1.3 进程组隔离

启动命令时使用进程组，确保 kill 时子进程（如 `python -m http.server` 可能 fork 的子进程）也一并终止：

```go
func (pm *ProcessManager) Start(ctx context.Context, name, command string) (*ManagedProcessInfo, error) {
    pm.mu.Lock()
    if _, exists := pm.processes[name]; exists {
        pm.mu.Unlock()
        return nil, fmt.Errorf("background process '%s' already exists; stop it first or use a different name", name)
    }
    pm.mu.Unlock()

    cmd := exec.CommandContext(context.Background(), "bash", "-c", command) // 不用 ctx，后台独立存活
    cmd.Dir = wdctx.Dir(ctx)

    // 进程组隔离：kill 时发信号给 -pid，确保整个进程树终止
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

    // 输出捕获：环形缓冲区（Phase 1 用内存，Phase 2 切文件）
    var stdoutBuf, stderrBuf bytes.Buffer
    cmd.Stdout = &stdoutBuf
    cmd.Stderr = &stderrBuf

    if err := cmd.Start(); err != nil {
        return nil, fmt.Errorf("failed to start background process '%s': %w", name, err)
    }

    mp := &ManagedProcess{
        Name:      name,
        Command:   command,
        Cmd:       cmd,
        PID:       cmd.Process.Pid,
        StartedAt: time.Now(),
        Status:    ProcessRunning,
    }

    // 后台 goroutine 等待进程退出
    go func() {
        err := cmd.Wait()
        mp.mu.Lock()
        defer mp.mu.Unlock()

        if mp.Status == ProcessKilled {
            return // already marked as killed by Stop()
        }

        if err != nil {
            if exitErr, ok := err.(*exec.ExitError); ok {
                mp.ExitCode = exitErr.ExitCode()
                mp.Status = ProcessExited
            } else {
                mp.Status = ProcessError
                mp.ExitErr = err
            }
        } else {
            mp.Status = ProcessExited
        }
        mp.lastStdout = stdoutBuf.Bytes()
        mp.lastStderr = stderrBuf.Bytes()
    }()

    pm.mu.Lock()
    pm.processes[name] = mp
    pm.mu.Unlock()

    return mp.toInfo(), nil
}
```

#### 5.1.4 Stop 实现

```go
func (pm *ProcessManager) Stop(name string) (*ManagedProcessInfo, error) {
    pm.mu.Lock()
    mp, ok := pm.processes[name]
    if !ok {
        pm.mu.Unlock()
        return nil, fmt.Errorf("background process '%s' not found", name)
    }
    pm.mu.Unlock()

    mp.mu.Lock()
    if mp.Status != ProcessRunning {
        info := mp.toInfo()
        mp.mu.Unlock()
        // 清理 entry
        pm.mu.Lock()
        delete(pm.processes, name)
        pm.mu.Unlock()
        return info, nil // already stopped
    }
    mp.mu.Unlock()

    // 先尝试优雅终止（SIGTERM 给整个进程组）
    pgid := mp.Cmd.Process.Pid
    syscall.Kill(-pgid, syscall.SIGTERM)

    // 5s 优雅期后强制 kill
    done := make(chan struct{})
    go func() {
        mp.Cmd.Wait()
        close(done)
    }()

    select {
    case <-done:
    case <-time.After(5 * time.Second):
        syscall.Kill(-pgid, syscall.SIGKILL)
        <-done
    }

    mp.mu.Lock()
    mp.Status = ProcessKilled
    info := mp.toInfo()
    mp.mu.Unlock()

    // 清理 entry
    pm.mu.Lock()
    delete(pm.processes, name)
    pm.mu.Unlock()

    return info, nil
}
```

#### 5.1.5 KillAll（agent 退出时调用）

```go
func (pm *ProcessManager) KillAll() {
    pm.mu.Lock()
    names := make([]string, 0, len(pm.processes))
    for name := range pm.processes {
        names = append(names, name)
    }
    pm.mu.Unlock()

    for _, name := range names {
        pm.Stop(name) // 忽略错误，尽力而为
    }
}
```

### 5.2 修改 `agent/tools/bash.go`

#### 5.2.1 扩展 bashArgs

```go
type bashArgs struct {
    Command    string `json:"command"`
    Timeout    *int   `json:"timeout,omitempty"`
    Background bool   `json:"background,omitempty"` // 新增：后台运行
    StopName   string `json:"stop_name,omitempty"`  // 新增：按名停止
    ListBg     bool   `json:"list_bg,omitempty"`    // 新增：列出后台进程
}
```

参数互斥约束（`ExecuteContext` 开头校验）：

| 场景 | 所需参数 | 说明 |
|------|---------|------|
| 前台执行 | `command` | 现有行为，无变化 |
| 后台启动 | `command` + `background: true` | 返回进程信息，不阻塞 |
| 停止后台 | `stop_name` | 按名称停止进程 |
| 列出后台 | `list_bg: true` | 返回所有后台进程列表 |

#### 5.2.2 全局 ProcessManager 实例

```go
var bashProcessManager = &ProcessManager{
    processes: make(map[string]*ManagedProcess),
}
```

#### 5.2.3 ExecuteContext 分支

```
ExecuteContext(ctx, args)
    │
    ├─ list_bg == true ───→ bashProcessManager.List() → JSON
    │
    ├─ stop_name != "" ───→ bashProcessManager.Stop(name) → JSON
    │
    ├─ background == true ─→ bashProcessManager.Start(ctx, name, cmd) → JSON
    │       └─ name 生成规则：LLM 通过 description 参数指定，或自动从
    │          command 提取（取第一个非路径单词，如 "http.server" → "http-server"）
    │
    └─ 默认（前台）──→ 现有逻辑不变
```

#### 5.2.4 Schema 更新

```go
func (t BashTool) Description() string {
    return "Executes a shell command and returns its output. " +
        "The working directory persists between commands. " +
        "Use for running build commands, tests, git operations, and other shell tasks. " +
        "For long-running processes (servers, watchers), use background mode."
}

func (t BashTool) Properties() map[string]PropertySchema {
    return map[string]PropertySchema{
        "command": {Type: "string", Description: "The bash command to execute"},
        "timeout": {Type: "integer", Description: "Optional timeout in milliseconds (max 600000, default 120000)"},
        "background": {Type: "boolean", Description: "Set to true to run this command in the background. The command will keep running after the tool returns. The command field must include a name in a comment with the format '#name: your-name' (e.g. 'python -m http.server 8080 #name: dev-server'). Use list_bg to check status and stop_name to stop it."},
        "stop_name": {Type: "string", Description: "Stop a background process by its name (the name specified with #name: in the command)"},
        "list_bg":  {Type: "boolean", Description: "List all running background processes with their status"},
    }
}
```

**注意**：`name` 参数方案选择 —— 考虑三种方式：

| 方案 | 示例 | 评价 |
|------|------|------|
| A. 独立 `name` 参数 | `{"command": "...", "background": true, "name": "dev-server"}` | Schema 最干净，LLM 容易理解 |
| B. 命令注释提取 | `{"command": "python -m http.server 8080 #name: dev-server"}` | Claude Code 采用 |
| C. 命令中自动提取 | `"python -m http.server"` → name = `"http-server"` | 魔术规则，LLM 依赖隐含规则 |

**选择方案 A**：独立 `name` 参数。理由：Go struct 的 JSON schema 生成更自然，LLM 不会把 name 和 command 混淆。Claude Code 用注释方案是因为 TS schema 支持更灵活的内联注释解析。

```go
type bashArgs struct {
    Command    string `json:"command"`
    Timeout    *int   `json:"timeout,omitempty"`
    Background bool   `json:"background,omitempty"`
    Name       string `json:"bg_name,omitempty"`   // 后台进程名称（background=true 时必填）
    StopName   string `json:"stop_name,omitempty"` // 停止指定名称的进程
    ListBg     bool   `json:"list_bg,omitempty"`   // 列出所有后台进程
}
```

### 5.3 典型使用流程

```
LLM: Bash({"command": "python -m http.server 8080", "background": true, "bg_name": "dev-server"})
  → {"name": "dev-server", "pid": 12345, "status": "running", "startedAt": "..."}

LLM: Bash({"command": "curl -s localhost:8080"})
  → {"stdout": "<!DOCTYPE html>...", "exitCode": 0}

LLM: Bash({"command": "echo 'test' > /tmp/test.txt; curl -s -X POST localhost:8080/upload -F 'file=@/tmp/test.txt'"})
  → {"stdout": "uploaded", "exitCode": 0}

LLM: Bash({"stop_name": "dev-server"})
  → {"name": "dev-server", "pid": 12345, "status": "killed", "exitCode": -1}

LLM: Bash({"list_bg": true})
  → []  // 全部已清理
```

### 5.4 Agent 生命周期集成

在 `AIAgent` 的清理路径中调用 `KillAll()`：

```go
// agent/agent.go 或相关的 cleanup 路径
func (a *AIAgent) cleanup() {
    tools.BashKillAllBackground() // 清理所有后台进程
    // ... 其他清理
}
```

需要考虑的清理点：

1. **正常结束** — agent loop 完成，TurnComplete
2. **用户 `/quit`** — TUI 退出
3. **Channel 断开** — channel agent 退出
4. **SubAgent 退出** — sub-agent 清理（sub-agent 通常没有 Bash 权限，但防御性处理）

### 5.5 输出捕获策略

Phase 1 使用内存环形缓冲区（64KB），原因：

- Phase 1 的典型场景是短生命周期后台进程（启动 → 验证 → 停止），不需要持久化
- 避免引入文件 I/O 的复杂性
- `list_bg` 时可以返回最近输出，帮助 LLM 诊断（如 server 启动报错时）

Phase 2 切换到文件持久化后，输出上限消失，环形缓冲区保留作为 `list_bg` 时的"最近输出快照"。

```go
type ringBuffer struct {
    buf []byte
    pos int
    cap int
}

func (rb *ringBuffer) Write(p []byte) (n int, err error) {
    for _, b := range p {
        rb.buf[rb.pos] = b
        rb.pos = (rb.pos + 1) % rb.cap
    }
    return len(p), nil
}

func (rb *ringBuffer) String() string {
    // 从 pos 开始读取 cap 个字节，组成有序字符串
}
```

### 5.6 并发安全

`ProcessManager` 所有公开方法持有 `pm.mu`（互斥锁）。`ManagedProcess` 的 `Status`、`ExitCode` 等字段通过 `mp.mu` 保护。

后台 goroutine（`go func() { cmd.Wait() }`）在更新 `mp` 状态时先持有 `mp.mu`，确保与 `Stop()`、`toInfo()` 等操作不竞争。

## 六、Phase 2 设计概要

Phase 2 在 Phase 1 基础上增加异步通知和输出持久化，仅在此给出概要设计，详细设计留待后续独立文档。

### 6.1 输出文件化

```go
type ManagedProcess struct {
    // ... Phase 1 字段
    OutputPath string        // 临时文件路径，stdout+stderr 合并写入
    outputFile *os.File      // 文件句柄
    outputSize int64         // 原子操作，大小看门狗检查
}
```

启动时：

```go
f, _ := os.CreateTemp("", "tachi-bg-*.log")
cmd.Stdout = f
cmd.Stderr = f
mp.OutputPath = f.Name()
mp.outputFile = f
```

关闭时清理临时文件（或保留到 agent 退出后统一清理）。`ReadFile` tool 可以读取 `OutputPath` 获取完整输出。

### 6.2 BackgroundTaskReminder

实现 `systemreminder.Reminder` 接口，在每次 `Collector.Collect()` 时检查已完成的后台任务：

```go
// agent/systemreminder/background_task.go
type BackgroundTaskReminder struct {
    manager *tools.ProcessManager
}

func (r *BackgroundTaskReminder) Collect(ctx Context) string {
    completed := r.manager.DrainCompleted() // 取出并清除已完成的任务列表
    if len(completed) == 0 {
        return ""
    }

    var lines []string
    for _, p := range completed {
        status := "completed"
        if p.ExitCode != 0 {
            status = fmt.Sprintf("failed (exit code %d)", p.ExitCode)
        }
        lines = append(lines, fmt.Sprintf(
            "Background command \"%s\" (%s) %s. Output: %s",
            p.Name, p.Command, status, p.OutputPath,
        ))
    }
    return strings.Join(lines, "\n")
}
```

`DrainCompleted()` 是关键——它会检查所有进程，收集已完成的，从 `processes` map 中移除，返回列表。这样每条完成通知只发送一次。

### 6.3 大小看门狗

```go
const maxOutputBytes = 100 * 1024 * 1024 // 100MB

func (mp *ManagedProcess) startSizeWatchdog() {
    go func() {
        ticker := time.NewTicker(5 * time.Second)
        defer ticker.Stop()
        for range ticker.C {
            if mp.outputSize > maxOutputBytes {
                syscall.Kill(-mp.Cmd.Process.Pid, syscall.SIGKILL)
                return
            }
        }
    }()
}
```

### 6.4 卡死检测

```go
func (mp *ManagedProcess) startStallWatchdog() {
    // 若 45s 内 outputSize 无变化，检查最后一行输出
    // 如果匹配交互式提示模式（PromptPatterns），注入通知
}
```

## 七、边界情况与决策

### 7.1 同名进程冲突

如果 LLM 尝试用相同 `bg_name` 启动新进程：

- **策略**：返回错误，要求先 `stop_name` 或使用不同名称
- **理由**：显式优于隐式。自动 kill 旧进程可能导致意外的数据丢失

未来可以考虑 `replace: true` 参数自动先 stop 再 start。

### 7.2 工作目录

后台进程继承 `wdctx.Dir(ctx)` 作为工作目录，与前台 Bash 一致。

### 7.3 环境变量

后台进程继承 `os.Environ()`，与前台 Bash 一致。不额外设置环境变量。

### 7.4 SubAgent 与后台进程

Sub-agent 有 Bash tool 权限（`allowed_tools` 包含 Bash）。如果 sub-agent 启动后台进程：

- 进程由全局 `ProcessManager` 管理
- Sub-agent 退出时不会触发 `KillAll()`（只有主 agent 退出时触发）
- 这意味着 sub-agent 启动的后台进程会存活到主 agent 结束

**决策**：接受这个行为。Sub-agent 通常执行隔离任务，不应该拥有"清理全局状态"的权限。如果 sub-agent 启动了一个 dev server，主 agent 可以继续使用它。

### 7.5 MCP 工具的交互

Bash tool 的 `Parallel()=false` 保持不变（Phase 1）。后台启动的 Bash 调用立刻返回（不阻塞），因此不影响与其他工具的并行执行。

### 7.6 进程组在 macOS/Linux 的行为

`syscall.Kill(-pgid, syscall.SIGTERM)` 的行为：

- macOS/Linux：负 PID 表示进程组，信号发送给组内所有进程 ✅
- Windows：不支持 `Setpgid`，需要条件编译使用 `taskkill /T /PID`

tachi 当前目标平台为 macOS/Linux，Windows 支持不在范围内。

### 7.7 ctx 参数的处理

后台进程不能使用 `ExecuteContext` 传入的 `ctx`（因为 `ctx` 可能在 tool call 返回后被 cancel）。因此 `Start()` 方法内部使用 `context.Background()` 创建 command：

```go
cmd := exec.CommandContext(context.Background(), "bash", "-c", command)
```

这确保了后台进程的生命周期完全由 `ProcessManager` 控制，不受 agent loop context 影响。

## 八、文件清单

| 文件 | 操作 | 说明 |
|------|------|------|
| `agent/tools/process_manager.go` | 新增 | ProcessManager 实现 |
| `agent/tools/process_manager_test.go` | 新增 | 单元测试 |
| `agent/tools/bash.go` | 修改 | 增加 background/stop_name/list_bg 参数分支 |
| `agent/tools/bash_test.go` | 修改 | 增加后台模式测试 |
| `agent/agent.go` | 修改 | cleanup 路径增加 KillAll() 调用 |

Phase 2 额外文件：

| 文件 | 操作 | 说明 |
|------|------|------|
| `agent/systemreminder/background_task.go` | 新增 | BackgroundTaskReminder |
| `agent/systemreminder/background_task_test.go` | 新增 | 测试 |

## 九、参考

- Claude Code `LocalShellTask/LocalShellTask.tsx` — 后台任务注册与状态管理
- Claude Code `utils/ShellCommand.ts` — 进程状态机 + treeKill
- Claude Code `tasks/LocalShellTask/killShellTasks.ts` — agent 退出时的进程清理