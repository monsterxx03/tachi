# Native Memory — 会话记忆系统

> 版本: 1.0 | 日期: 2026-05-16 | 状态: 设计阶段
> 关联: [session 存储](./2026-05-10-session-replace-transcript.md),
>       [systemreminder 机制](../agent/systemreminder/reminder.go)

## 一、概述

### 1.1 问题

当前 Tachi 是完美的健忘症患者：

- 每个会话从零开始——我不知道你是谁、上次聊过什么、有什么偏好
- `session/` 目录保存了所有历史对话（`messages.jsonl`），但我**没有能力去读它们**
- 唯一的"记忆"是 `.tachi.md`（静态项目上下文），但它不会随对话积累而生长

这和"越用越懂你"的目标相差甚远。

### 1.2 设计原则

| 原则 | 说明 |
|------|------|
| 工具优先 | 不引入新存储引擎——用已有的 Read/Grep/Bash/Write 搭 |
| 零新依赖 | 没有 embedding、没有向量库、没有外部服务 |
| 渐进披露 | 不往 system prompt 里塞东西——我自己查，查到就知 |
| 会话自治 | 记忆不依赖跨会话状态机，每场对话自己搜自己看 |

### 1.3 与 OpenClaw MEMORY.md 的本质区别

| | OpenClaw | Tachi（本方案） |
|--|---------|----------------|
| 载体 | `workspace/MEMORY.md` 文件 | `session/` 下的历史 JSONL + `memory/log` 索引 |
| 注入 | 强塞进 system prompt（每次） | 我自己用工具查，查到注入 |
| 维护 | 用户或 Agent 手动编辑 | 会话结束时自动 append 一行 |
| 细查 | 无——只有 MEMORY.md | 直接 Grep session 目录找原文 |
| 哲学 | "你告诉我你是谁" | "我自己记住然后自己去想" |

---

## 二、数据模型

### 2.1 `~/.tachi/memory/log` — 会话索引

纯文本文件，每行一条，记录一场对话的要点：

```
2026-05-16 22:00 | designed native memory system | tags: design, memory
2026-05-16 21:30 | discussed Cardputer-Adv hardware | tags: hardware, review
2026-05-15 14:00 | fixed subagent worktree leak | tags: bugfix, subagent
```

**字段**（空格分隔的纯文本，Grep 友好）：

```
<日期> | <一句话摘要> | tags: <关键词逗号分隔>
```

**大小估计**：~5 行/天 → ~1800 行/年 → 一次 Grep `< 1ms`。

### 2.2 `session/<id>/messages.jsonl` — 对话原文（已有，不变）

不做任何改造。所有历史消息已经在那里了，每行一个 JSON 对象。

---

## 三、三个操作

### 3.1 写：会话结束时 append 索引行

**触发时机**：每次 `/new` 或 `/quit` 时，在 agent loop 结束路径上。

**现有锚点**：`AIAgent.RunConversationStream` 的 goroutine 在 `defer` 中已有关闭逻辑。
在 session 结束前插入：

```
1. 从 session 取出 title（已有，由 LLM 首轮生成）
2. 如果没有 title，用用户首条消息的前 30 字
3. 追加一行到 ~/.tachi/memory/log
```

```go
// 伪代码：在 agent.go 的 closeSession() 附近
func (a *AIAgent) recordMemoryIndex() {
    session := a.sessionManager.Current()
    if session == nil {
        return
    }
    title := session.Title
    if title == "" {
        title = summarizeFirstMessage(session.ID) // 前30字
    }
    line := fmt.Sprintf("%s | %s\n", time.Now().Format("2006-01-02 15:04"), title)
    f, _ := os.OpenFile(memoryIndexPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    defer f.Close()
    f.WriteString(line)
}
```

> 注意：append 是 write-only，不读不解析。不会因为文件损坏导致会话失败。

### 3.2 查：新会话开始时搜索引

**机制**：在 systemreminder 中新增 `MemoryRecallReminder`。

```go
type MemoryRecallReminder struct {
    memoryDir string // ~/.tachi/memory/
}

func (r MemoryRecallReminder) Generate(ctx Context) []string {
    if !ctx.IsFirstMessage || ctx.IsToolResult {
        return nil
    }

    logFile := filepath.Join(r.memoryDir, "log")
    data, err := os.ReadFile(logFile)
    if err != nil {
        return nil // 没有记忆文件，不报错
    }

    lines := strings.Split(strings.TrimSpace(string(data)), "\n")
    if len(lines) == 0 {
        return nil
    }

    // 取最近 20 条
    var recent []string
    for i := max(0, len(lines)-20); i < len(lines); i++ {
        recent = append(recent, lines[i])
    }

    return append(
        []string{"## Memory Index (recent sessions)"},
        recent...,
    )
}
```

**为什么不直接注入完整记忆？** 因为要保持上下文精简。20 行索引 ≈ 500 tokens，可接受。
详细的回忆留给我自己搜索。

### 3.3 搜：对话中我主动 Grep 历史

不需要新 Tool——我已经有 `Grep` 工具了。

```
你: "我们上次讨论的接口性能问题后来怎么解决的？"
       ↓
我: Grep("接口性能", "~/.tachi/session/")
       ↓
    匹配 session 中的消息，找到原文
       ↓
我: "找到了，在 5 月 10 日的会话里你说..."
```

**这也不是新代码**——GrepTool 已经注册了。只是我现在没有"查历史"的意识，
需要对 system prompt 增加一句话来启用这个行为。

### 3.4 忘：`/forget` 命令

```go
// /forget <关键词>
// 从 memory/log 中删除包含关键词的行
```

```go
func handleForget(keyword string) {
    data, _ := os.ReadFile(memoryIndexPath())
    lines := strings.Split(string(data), "\n")
    filtered := slices.DeleteFunc(lines, func(line string) bool {
        return strings.Contains(line, keyword)
    })
    os.WriteFile(memoryIndexPath(), []byte(strings.Join(filtered, "\n")), 0644)
}
```

---

## 四、唯一的新代码

| 新增 | 位置 | 行数 |
|------|------|------|
| `~/.tachi/memory/` 目录 | 运行时自动创建 | ~10 行 |
| `recordMemoryIndex()` | `agent/agent.go` | ~25 行 |
| `MemoryRecallReminder` | `agent/systemreminder/reminder.go` | ~30 行 |
| `/forget` 命令 | `tui/commands.go` | ~20 行 |
| system prompt 加一句话 | `.tachi.md` | 1 行 |
| **总计** | | **~86 行** |

零新依赖、零新存储、零新 Tool。

---

## 五、与现有架构的集成点

| 现有组件 | 怎么用上 |
|---------|---------|
| `session.Manager` | `Session.Title` 已有，直接取用 |
| `agent/agent.go` | 在 `RunConversationStream` 的退出路径加一个调用 |
| `systemreminder` | 新增一个 Reminder，现有 `Collector` 直接支持 |
| `Grep` 工具 | 已有，我需要被告知"你可以搜历史" |
| `tui/commands.go` | 新增 `/forget` 处理分支 |
| `~/.tachi/` 目录 | 建个子目录，不污染现有结构 |
| `.gitignore` | `memory/` 默认忽略 |

---

## 六、不做的事（故意为之）

| 不做 | 原因 |
|------|------|
| ❌ 自动摘要 | 太复杂，不如让我自己搜 |
| ❌ 向量 embedding | 依赖太重，Tachi 是 Go 单体 |
| ❌ 记忆过期 | 文件在磁盘上，用户自己用 `/forget` 或 `rm` |
| ❌ 跨设备同步 | 以后再说 |
| ❌ 注入历史全文 | 撑爆上下文 |
| ❌ OpenClaw 式 MEMORY.md | 那是平台思维，我是工具思维 |

---

## 七、可能的演进方向

1. **人肉写标签** — 你可以在 memory/log 里手动加行，语法不变
2. **会话标题即索引** — session title 本身就是 LLM 生成的摘要，直接复用
3. **Grep 历史作为 fallback** — 如果我查 memory/log 没命中，自动 fallback 到 Grep session 目录
4. **TUI 里按 `Ctrl+M` 看记忆** — 和 `/mcp` 类似的浮层，滚动显示 memory/log
