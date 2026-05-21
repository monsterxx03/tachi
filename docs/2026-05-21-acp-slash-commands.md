# ACP Slash Command 支持设计

> 版本: 1.0 | 日期: 2026-05-21 | 状态: 设计阶段

## 一、概述

在 ACP 模式下，编辑器（Client）将用户输入的 `/commit`、`/usage` 等命令作为普通 `session/prompt` 文本发送给 Agent。Agent 需要：

1. **广告**（Advertising）：通过 `available_commands_update` session notification 告知 Client 哪些 slash command 可用
2. **拦截**（Interception）：在 `Prompt()` 中检测到 slash command 前缀时，不走常规 LLM 对话流程，而是执行对应操作并将结果以 `SessionUpdate` 形式流式返回

[ACP 协议规范](https://agentclientprotocol.com/protocol/slash-commands) 已完整支持上述机制。SDK (`coder/acp-go-sdk v0.13.0`) 也已有 `SessionAvailableCommandsUpdate`、`AvailableCommand`、`AvailableCommandInput` 等类型。

---

## 二、当前状态分析

### 2.1 已有的 Slash Commands（TUI 层）

所有 slash commands 目前都在 `tui/commands.go` 中，与 TUI 的 `Model` 状态机强耦合：

| 命令 | 类型 | ACP 适用 | 说明 |
|------|------|----------|------|
| `/new` | TUI 状态操作 | ❌ | 编辑器通过 `session/new` 处理 |
| `/quit` | TUI 退出 | ❌ | 编辑器自己退出 |
| `/model` | TUI 交互选择 | ❌ | 编辑器通过 `session/set-mode` 处理 |
| `/commit` | LLM 驱动 | ✅ | `RunOneOffStream` + commit prompt |
| `/compact` | LLM 驱动 | ✅ | `RunConversationStream` + `FinalizeCompact` |
| `/init` | LLM 驱动 | ✅ | `RunConversationStream` + init prompt |
| `/mcp` | 管理操作 | ⚠️ 部分 | `list` 可用；`toggle/reconnect/auth` 太交互 |
| `/sessions` | TUI 选择列表 | ❌ | 编辑器通过 `session/list` 处理 |
| `/usage` | 纯计算 | ✅ | `ComputeSessionUsage`，无需 LLM |
| `/skill` | 管理操作 + LLM | ✅ | `list` 可用；`<name>` 可激活并调 LLM |
| `/transcript` | 生成报表 | ⚠️ | 可生成 HTML，但编辑器未必支持打开 |

### 2.2 ACP 当前缺失

`agent/acp/agent.go` 中：
- `NewSession()`：创建 session 后**没有发送** `available_commands_update` 通知
- `Prompt()`：`convertContentBlocks()` 直接将内容转为 `userMsg` 丢给 `RunConversationStream`，**不做 slash 检测**

`agent.go` 中引用的 `acp.AgentCapabilities` 也没有声明 slash command 相关能力（目前协议无此字段，commands 通过 session update 广告，不需要 initialize 声明）。

---

## 三、设计

### 3.1 核心思路：`ACPSlashCommand` 抽象

在 `agent/acp/` 下新增 `commands.go`，定义 ACP 模式专属的 slash command 抽象：

```go
// ACPSlashCommand represents a slash command available in ACP mode.
// Unlike TUI commands (which mutate the TUI Model), ACP commands work
// with ACPSession — they compute results and stream them back as
// SessionUpdate notifications.
type ACPSlashCommand struct {
    Name        string
    Description string
    InputHint   string  // optional: hint for argument input (e.g. "query to search for")
    Handler     func(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error)
}
```

签名设计要点：
- `sess *ACPSession`：提供对 `AIAgent`、`session.Manager`、`MCP Manager` 的访问
- `conn *acp.AgentSideConnection`：用于直接发送 `SessionUpdate` 通知（结果不是通过常规 `RunConversationStream` event channel，而是 command handler 自行推送）
- `args string`：命令后的参数（如 `/skill code-review` 中的 `code-review`）
- 返回 `(acp.StopReason, error)`：与 `Prompt()` 的返回签名对齐

### 3.2 Prompt() 改造：拦截 + 分发

```go
func (t *TachiAgent) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
    sess, ok := t.sessions.Get(string(req.SessionId))
    if !ok {
        return acp.PromptResponse{}, fmt.Errorf("session not found: %s", req.SessionId)
    }

    t.logger.Log("ACP: Prompt called for session %s", sess.ID)

    sess.mu.Lock()
    defer sess.mu.Unlock()

    promptCtx, promptCancel := context.WithCancel(sess.ctx)
    defer promptCancel()
    sess.setPromptCancel(promptCancel)
    defer sess.setPromptCancel(nil)

    promptCtx = wdctx.WithDir(promptCtx, sess.cwd)

    userMsg := convertContentBlocks(req.Prompt)

    // ---- NEW: Slash command interception ----
    if cmd, args := parseSlashCommand(userMsg); cmd != nil {
        t.logger.Log("ACP: slash command detected: %s (args=%q)", cmd.Name, args)
        stopReason, err := cmd.Handler(promptCtx, sess, t.conn, args)
        if err != nil {
            return acp.PromptResponse{}, err
        }
        return acp.PromptResponse{StopReason: stopReason}, nil
    }
    // ---- END NEW ----

    // ... existing LLM conversation logic unchanged ...
}
```

`parseSlashCommand` ：
```go
// parseSlashCommand checks if the message is a slash command.
// Returns the matching command and the argument portion (text after the
// command name). Returns (nil, "") for normal messages.
func parseSlashCommand(msg string) (*ACPSlashCommand, string) {
    msg = strings.TrimSpace(msg)
    if msg == "" || msg[0] != '/' {
        return nil, ""
    }

    // Split into command name and remainder
    parts := strings.SplitN(msg, " ", 2)
    cmdName := parts[0]
    args := ""
    if len(parts) > 1 {
        args = strings.TrimSpace(parts[1])
    }

    // Exact match first, then prefix match (e.g. "/mcp list" → "/mcp")
    cmd := findACPCommand(cmdName)
    if cmd == nil {
        // Try prefix match: "/mcp list" with ""/mcp" in registry
        cmd = findACPCommandByPrefix(cmdName)
    }
    if cmd != nil {
        return cmd, args
    }
    // Not a known command — let it flow through as normal LLM input
    // (user might be talking about slash commands, or it's a skill
    //  invocation like "/code-review" handled by the skill system)
    return nil, ""
}
```

**重要设计决策**：不匹配任何已注册命令的 `/xxx` **不做拦截**，让它作为普通消息进入 LLM 对话。这允许：
- 用户在对话中讨论 slash commands 本身
- Skill invocation（`/code-review main.go`）由 LLM 通过 `ActivateSkill` 机制处理，而不是在 ACP 层硬编码

### 3.3 命令广告（Advertising）

在 `NewSession()` 返回前，推送 `available_commands_update`：

```go
// 在 NewSession() 中，return 之前插入：
if t.conn != nil {
    acpCommands := buildACPAvailableCommands()
    _ = t.conn.SessionUpdate(ctx, acp.SessionNotification{
        SessionId: acp.SessionId(sess.ID),
        Update: acp.SessionUpdate{
            AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
                AvailableCommands: acpCommands,
            },
        },
    })
}
```

`buildACPAvailableCommands()` 从 `acpCommands` 注册表构建 `[]acp.AvailableCommand`：

```go
func buildACPAvailableCommands() []acp.AvailableCommand {
    result := make([]acp.AvailableCommand, 0, len(acpCommands))
    for _, cmd := range acpCommands {
        ac := acp.AvailableCommand{
            Name:        cmd.Name,
            Description: cmd.Description,
        }
        if cmd.InputHint != "" {
            ac.Input = &acp.AvailableCommandInput{Hint: cmd.InputHint}
        }
        result = append(result, ac)
    }
    return result
}
```

**动态更新**：ACP 协议支持在 session 生命周期中多次发送 `available_commands_update`。以下场景可能需要动态更新：

| 场景 | 变更 |
|------|------|
| 首次 Prompt 后 (`SkillStore` 就绪) | 追加 skills 为 slash commands |
| Compact 完成后（新 session 创建） | 命令列表不变（历史累积不影响命令可用性） |
| MCP server 动态连接/断开 | 理论上可添加/移除 MCP 子命令，但复杂度高，先不做 |

对于 skill 的动态注册：`SkillStore()` 在 `Configure()` 时初始化，在 `NewSession` 时就应该可用。因此可以在 `NewSession` 时一次性构建包含 skills 的完整命令列表。不需要延迟到首次 Prompt。

### 3.4 各命令详细设计

#### 3.4.1 `/commit`

```
输入: (无参数)
逻辑:
  1. 临时保存当前 tool registry，只保留 Bash 工具
  2. 用 sess.agent.RunOneOffStream(ctx, commitProvider, systemPrompt, commitPrompt, opts) 执行
  3. 用 streamToACP() 复用现有的事件流转换管道
  4. 恢复 tool registry
输出: 与正常 LLM turn 一样的 streaming
```

关键差异 vs TUI：TUI 的 `/commit` 需要保存/恢复 `m.history`（因为 `RunOneOffStream` 会覆盖），ACP 不需要——ACP 的 `Prompt()` 从 `sess.sessMgr.LoadMessages()` 构建 history，不保存内存引用。

```go
func handleACPCommit(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
    aiAgent := sess.agent

    // Save tool registry, leave only Bash
    savedTools := aiAgent.SaveToolRegistry()
    for _, name := range aiAgent.ToolNames() {
        if name != tools.ToolNameBash {
            aiAgent.UnregisterTool(name)
        }
    }
    defer func() {
        if savedTools != nil {
            aiAgent.RestoreToolRegistry(savedTools)
        }
    }()

    commitProvider := aiAgent.CommitProvider()
    model := aiAgent.Model()

    thinkingDisabled := false
    opts := llm.ChatOptions{
        MaxTokens: config.DefaultMaxTokens,
        Thinking:  &thinkingDisabled,
    }

    // commitUserPrompt is in tui/commands.go — need to extract to a shared location
    systemPrompt := buildSystemPromptForCwd(sess.cfg.Language, sess.cwd)

    eventCh := aiAgent.RunOneOffStream(ctx, commitProvider, systemPrompt,
        CommitUserPrompt(model), opts)

    stopReason := streamToACP(ctx, sess, conn, eventCh)
    return stopReason, nil
}
```

**依赖提取**：`commitUserPrompt()` 目前在 `tui/commands.go` 的 package private 函数中。需要提取到共享位置（如 `agent/commit_prompt.go`）或直接在 `agent/acp/commands.go` 中内联。

#### 3.4.2 `/init`

```
输入: (无参数)
逻辑:
  1. 使用 InitPromptTemplate 作为 user message
  2. sess.agent.RunConversationStream(ctx, history, InitPromptTemplate, systemPrompt, opts)
  3. 不修改 tool registry（init 需要 WriteFile 等工具）
输出: 与正常 LLM turn 一样的 streaming
```

最简实现——几乎与常规对话相同，只是 user message 替换为 init prompt。

```go
func handleACPInit(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
    // Build history from session
    var history []llm.Message
    if sess.sessMgr != nil {
        msgs, _ := sess.sessMgr.LoadMessages()
        history, _ = agent.ConvertSessionToLLMMessages(msgs, sess.providerType)
    }

    systemPrompt := buildSystemPromptForCwd(sess.cfg.Language, sess.cwd)

    eventCh := sess.agent.RunConversationStream(ctx, history,
        InitPromptTemplate,  // from tui/commands.go — needs extraction
        systemPrompt,
        llm.ChatOptions{MaxTokens: config.DefaultMaxTokens},
    )

    return streamToACP(ctx, sess, conn, eventCh), nil
}
```

#### 3.4.3 `/compact`

```
输入: (无参数)
逻辑:
  1. Pre-check: session manager 存在且历史非空
  2. 保存工具注册表，清空所有工具（compact 不应调用工具）
  3. 用 RunConversationStream(ctx, history, BuildCompactInstruction(), ...) 执行
  4. 用 DrainCompactEvents 收集结果
  5. 调用 FinalizeCompact(sm, systemPrompt, summary)
  6. 更新 ACPSession 的 sessMgr 引用（FinalizeCompact 创建了新 session）
  7. 流式返回结果文本（"对话已压缩: X 条消息 → 摘要"）
```

关键复杂度：`FinalizeCompact` 在 session manager 中创建了新的 Tachi session（`sm.New(...)`），旧的 session 被归档。ACP 层需要：
- 更新 `sess.sessMgr` 的内部状态（`ACPSession` 不持有对具体 session ID 的引用——它通过 `sessMgr.Current()` 获取）
- **不需要**改变 ACP session ID——编辑器通过同一个 `sessionId` 继续发送 prompt

```go
func handleACPCompact(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
    sm := sess.sessMgr
    if sm == nil || !sm.HasCurrent() {
        _ = conn.SessionUpdate(ctx, acp.SessionNotification{
            SessionId: acp.SessionId(sess.ID),
            Update:    acp.UpdateAgentMessageText("No active session to compact."),
        })
        return acp.StopReasonEndTurn, nil
    }

    // 1. Save and clear tools
    savedTools := sess.agent.SaveToolRegistry()
    sess.agent.ClearToolRegistry()
    defer func() {
        if savedTools != nil {
            sess.agent.RestoreToolRegistry(savedTools)
        }
    }()

    // 2. Run compact turn
    systemPrompt := buildSystemPromptForCwd(sess.cfg.Language, sess.cwd)
    eventCh := sess.agent.RunConversationStream(ctx, nil,
        agent.BuildCompactInstruction(), systemPrompt,
        llm.ChatOptions{MaxTokens: config.DefaultMaxTokens},
    )

    // 3. Stream events to ACP client (so user sees progress)
    //    AND collect the summary
    summary, stopReason := streamAndCollectCompact(ctx, sess, conn, eventCh)
    if summary == "" {
        return stopReason, nil
    }

    // 4. Create new session from summary
    newHistory, err := agent.FinalizeCompact(sm, systemPrompt, summary)
    if err != nil {
        _ = conn.SessionUpdate(ctx, acp.SessionNotification{
            SessionId: acp.SessionId(sess.ID),
            Update:    acp.UpdateAgentMessageText("Compact failed: " + err.Error()),
        })
        return acp.StopReasonEndTurn, nil
    }

    // 5. Notify client
    _ = conn.SessionUpdate(ctx, acp.SessionNotification{
        SessionId: acp.SessionId(sess.ID),
        Update:    acp.UpdateAgentMessageText(
            fmt.Sprintf("Conversation compacted. New session created with summary of previous context.")),
    })

    return acp.StopReasonEndTurn, nil
}
```

**`streamAndCollectCompact`** — 一个新的辅助函数：在 streaming 的同时收集文本内容。与 `streamToACP` 类似但额外返回累积的完整响应文本。

> **备选简化方案**：如果不想新增 `streamAndCollectCompact`，可以让 compact 走一个两步流程：
> 1. `RunConversationStream` → `streamToACP` → 拿到 summary 后
> 2. 调用 `FinalizeCompact`
> 
> 但 `streamToACP` 不返回文本内容。可以用 `DrainCompactEvents`（已存在于 `agent/compact.go`）代替——不在 compact 过程中流式输出，等拿到完整结果后再推送一条 summary。这样用户体验稍差（用户看不到 streaming 进度），但实现简单。

**推荐**：使用 `DrainCompactEvents` 方案，简单可靠。Compact 通常很快（没有工具调用），用户几乎感知不到 waiting。

#### 3.4.4 `/usage`

```
输入: (无参数)
逻辑: 纯计算，不调 LLM
  1. agent.ComputeSessionUsage(sm, price, contextWindow)
  2. 格式化为文本
  3. 发送 SessionUpdate
```

这是最简单的——完全不需要 LLM。Handler 内部直接组装消息然后通过 `conn.SessionUpdate()` 推送。

```go
func handleACPUsage(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
    sm := sess.sessMgr
    if sm == nil || !sm.HasCurrent() {
        _ = conn.SessionUpdate(ctx, acp.SessionNotification{
            SessionId: acp.SessionId(sess.ID),
            Update:    acp.UpdateAgentMessageText("No active session."),
        })
        return acp.StopReasonEndTurn, nil
    }

    // Resolve price from current provider
    price := llm.ResolveModelPrice(sess.agent.Provider().Name(), sess.agent.Model())

    report, err := agent.ComputeSessionUsage(sm, price, sess.agent.ContextWindow())
    if err != nil {
        _ = conn.SessionUpdate(ctx, acp.SessionNotification{
            SessionId: acp.SessionId(sess.ID),
            Update:    acp.UpdateAgentMessageText("Usage: " + err.Error()),
        })
        return acp.StopReasonEndTurn, nil
    }

    text := formatUsageReportACP(report) // 纯文本版本（与 TUI Markdown 版不同）
    _ = conn.SessionUpdate(ctx, acp.SessionNotification{
        SessionId: acp.SessionId(sess.ID),
        Update:    acp.UpdateAgentMessageText(text),
    })

    return acp.StopReasonEndTurn, nil
}
```

#### 3.4.5 `/mcp`

```
输入: 子命令（list | toggle <name> | reconnect <name> | auth <name>）
逻辑:
  - list: 遍历 sess.mcpMgr 输出服务器状态
  - toggle/reconnect/auth: 与 TUI 相同的操作，但不走 TUI message channel
```

`list` 稳定可用。`toggle`/`reconnect`/`auth` 在 ACP 模式下的价值存疑——编辑器通常自己管理 MCP 连接（`session/new` 时传入 `mcpServers`）。**第一阶段只实现 `list` 和 `reconnect`**。

```go
func handleACPMCP(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
    parts := strings.Fields(args)
    sub := ""
    if len(parts) > 0 {
        sub = parts[0]
    }
    name := ""
    if len(parts) > 1 {
        name = parts[1]
    }

    switch sub {
    case "list", "":
        return handleACPMCPList(ctx, sess, conn)
    case "reconnect":
        return handleACPMCPReconnect(ctx, sess, conn, name)
    default:
        _ = conn.SessionUpdate(ctx, acp.SessionNotification{
            SessionId: acp.SessionId(sess.ID),
            Update:    acp.UpdateAgentMessageText(
                "Usage: /mcp list | /mcp reconnect <name>"),
        })
        return acp.StopReasonEndTurn, nil
    }
}
```

#### 3.4.6 `/skill`

```
输入:
  - (无参数) 或 "list": 列出所有可用 skills
  - <name> [args]: 激活 skill 并作为 LLM prompt 发送
```

两种模式：
- `list`：纯计算，不调 LLM
- `<name>`：调用 `aiAgent.ActivateSkill(name, args)` 获取激活消息，然后走正常 `RunConversationStream`

```go
func handleACPSkill(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
    store := sess.agent.SkillStore()
    if store == nil {
        _ = conn.SessionUpdate(ctx, acp.SessionNotification{
            SessionId: acp.SessionId(sess.ID),
            Update:    acp.UpdateAgentMessageText("Skill system not available."),
        })
        return acp.StopReasonEndTurn, nil
    }

    parts := strings.Fields(args)

    if len(parts) == 0 || parts[0] == "list" {
        // List skills — no LLM call
        return handleACPSkillList(ctx, sess, conn, store)
    }

    // Activate a specific skill
    skillName := parts[0]
    extraArgs := ""
    if len(parts) > 1 {
        extraArgs = strings.Join(parts[1:], " ")
    }

    if sess.agent.IsSkillActive(skillName) {
        _ = conn.SessionUpdate(ctx, acp.SessionNotification{
            SessionId: acp.SessionId(sess.ID),
            Update:    acp.UpdateAgentMessageText(
                fmt.Sprintf("Skill **%s** is already active in this session.", skillName)),
        })
        return acp.StopReasonEndTurn, nil
    }

    msg, err := sess.agent.ActivateSkill(skillName, extraArgs)
    if err != nil {
        _ = conn.SessionUpdate(ctx, acp.SessionNotification{
            SessionId: acp.SessionId(sess.ID),
            Update:    acp.UpdateAgentMessageText(
                fmt.Sprintf("Skill **%s** not found.", skillName)),
        })
        return acp.StopReasonEndTurn, nil
    }

    // Run as normal conversation turn with skill activation message
    var history []llm.Message
    if sess.sessMgr != nil {
        msgs, _ := sess.sessMgr.LoadMessages()
        history, _ = agent.ConvertSessionToLLMMessages(msgs, sess.providerType)
    }

    systemPrompt := buildSystemPromptForCwd(sess.cfg.Language, sess.cwd)
    eventCh := sess.agent.RunConversationStream(ctx, history, msg, systemPrompt,
        llm.ChatOptions{MaxTokens: config.DefaultMaxTokens})

    return streamToACP(ctx, sess, conn, eventCh), nil
}
```

#### 3.4.7 `/transcript`

```
输入: (无参数)
逻辑:
  1. 加载当前 session 的 messages
  2. render.BuildReportDataFromMessages + GenerateHTML
  3. render.OpenInBrowser (但 ACP 没有浏览器...)
  4. 回退：保存到文件，返回文件路径
```

```go
func handleACPTranscript(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
    sm := sess.sessMgr
    if sm == nil || !sm.HasCurrent() {
        // ...
    }

    msgs, _ := sm.LoadMessages()
    curr := sm.Current()
    data := render.BuildReportDataFromMessages(curr, msgs)
    html, _ := render.GenerateHTML(data)

    // 保存到文件（用户可手动打开）
    path, _ := render.SaveToFile(html, curr.ID)

    _ = conn.SessionUpdate(ctx, acp.SessionNotification{
        SessionId: acp.SessionId(sess.ID),
        Update:    acp.UpdateAgentMessageText(
            fmt.Sprintf("Transcript saved to: %s", path)),
    })
    return acp.StopReasonEndTurn, nil
}
```

> **注意**：`render.SaveToFile` 当前可能不存在（`render.OpenInBrowser` 在 `/tmp` 中创建临时文件后调用 `open`）。需要确认或新增保存函数。

### 3.5 完整命令注册表

```go
var acpCommands = []ACPSlashCommand{
    {
        Name:        "/commit",
        Description: "Generate commit message for staged changes and commit via git",
        Handler:     handleACPCommit,
    },
    {
        Name:        "/init",
        Description: "Generate .tachi.md project context file",
        Handler:     handleACPInit,
    },
    {
        Name:        "/compact",
        Description: "Compress conversation history into a summary and start fresh",
        Handler:     handleACPCompact,
    },
    {
        Name:        "/usage",
        Description: "Show token usage, cost, and tool call statistics",
        Handler:     handleACPUsage,
    },
    {
        Name:        "/mcp",
        Description: "Manage MCP servers (list, reconnect)",
        InputHint:   "list | reconnect <name>",
        Handler:     handleACPMCP,
    },
    {
        Name:        "/skill",
        Description: "List or activate skills",
        InputHint:   "list | <name> [args]",
        Handler:     handleACPSkill,
    },
    {
        Name:        "/transcript",
        Description: "Generate session transcript report",
        Handler:     handleACPTranscript,
    },
}
```

### 3.6 与 Skill 系统交互

Tachi 的 skill 系统（`/code-review main.go` → `sendSkillMessage`）在 ACP 模式下的处理路径有**两种选择**：

**方案 A：在 ACP slash command 层拦截所有 skill**
- 需要动态注册 skill 名称为 slash command
- 每次 skill 变更需要重新发送 `available_commands_update`
- 复杂度高

**方案 B（推荐）：让 `/xxx` 未知命令透过 LLM**
- 上文 `parseSlashCommand` 中，不匹配已注册命令的 `/xxx` 不做拦截
- 用户输入 `/code-review main.go` 时，作为普通文本 `"/code-review main.go"` 进入 LLM
- LLM 看到这个文本后，如果它能理解这是一个 skill 调用，它会自行调用 `Skill` tool
- **但实际上**，Tachi 的 LLM 不会"看到" skill 名就能自动激活——skill 激活是通过 `ActivateSkill()` 注入的 message，不是 LLM 自己能做的

因此，需要第三种方案：

**方案 C（推荐）：动态 skill 注册**
在 `NewSession()` 时，扫描 `SkillStore()` 中所有已加载的 skills，为每个 skill 生成一个 `ACPSlashCommand`，加入广告列表。

```go
// In buildACPAvailableCommands():
func buildACPAvailableCommands(aiAgent *agent.AIAgent) []acp.AvailableCommand {
    result := make([]acp.AvailableCommand, 0, len(acpCommands))

    // Static commands
    for _, cmd := range acpCommands {
        result = append(result, acp.AvailableCommand{
            Name:        cmd.Name,
            Description: cmd.Description,
            Input:       cmd.InputHint,
        })
    }

    // Dynamic skill commands
    if store := aiAgent.SkillStore(); store != nil {
        for _, meta := range store.List() {
            result = append(result, acp.AvailableCommand{
                Name:        "/" + meta.Name,
                Description: meta.Description,
                Input: &acp.AvailableCommandInput{
                    Hint: "optional instruction for this skill",
                },
            })
        }
    }

    return result
}
```

对于 skill handler：

```go
// In parseSlashCommand, after checking static commands:
cmdName := "/" + skillName  // user typed "/code-review"
if aiAgent.SkillStore() != nil && aiAgent.SkillStore().Has(skillName) {
    return &ACPSlashCommand{
        Name: cmdName,
        Handler: makeACPSkillHandler(skillName),
    }, args
}
```

`makeACPSkillHandler` 返回与 `handleACPSkill` 的 `<name>` 分支相同逻辑的 handler。

---

## 四、新增代码结构

```
agent/acp/
  commands.go        ← 新增：ACPSlashCommand 定义 + 所有 handler + 注册表 + 解析
  agent.go           ← 修改：Prompt() 添加拦截逻辑；NewSession() 添加广告
  (其余文件不变)
```

### 4.1 commands.go 内容概览

```go
package acp

// ACPSlashCommand 定义
type ACPSlashCommand struct { ... }

// 命令注册表
var acpCommands = []ACPSlashCommand{ ... }

// 入口函数
func parseSlashCommand(msg string, aiAgent *agent.AIAgent) (*ACPSlashCommand, string)
func buildACPAvailableCommands(aiAgent *agent.AIAgent) []acp.AvailableCommand

// 各命令 handler
func handleACPCommit(ctx, sess, conn, args) (acp.StopReason, error)
func handleACPInit(ctx, sess, conn, args) (acp.StopReason, error)
func handleACPCompact(ctx, sess, conn, args) (acp.StopReason, error)
func handleACPUsage(ctx, sess, conn, args) (acp.StopReason, error)
func handleACPMCP(ctx, sess, conn, args) (acp.StopReason, error)
func handleACPSkill(ctx, sess, conn, args) (acp.StopReason, error)
func handleACPTranscript(ctx, sess, conn, args) (acp.StopReason, error)
```

### 4.2 agent.go 改动点

```diff
// NewSession() 末尾，return 之前新增：
+   // Advertise available slash commands
+   if t.conn != nil {
+       acpCommands := buildACPAvailableCommands(aiAgent)
+       _ = t.conn.SessionUpdate(ctx, acp.SessionNotification{
+           SessionId: acp.SessionId(sess.ID),
+           Update: acp.SessionUpdate{
+               AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
+                   AvailableCommands: acpCommands,
+               },
+           },
+       })
+   }
```

```diff
// Prompt() 内部，convertContentBlocks 之后、build history 之前新增：
+   // Slash command detection
+   if cmd, args := parseSlashCommand(userMsg, sess.agent); cmd != nil {
+       t.logger.Log("ACP: slash command detected: %s", cmd.Name)
+       stopReason, err := cmd.Handler(promptCtx, sess, t.conn, args)
+       if err != nil {
+           return acp.PromptResponse{}, err
+       }
+       return acp.PromptResponse{StopReason: stopReason}, nil
+   }
```

### 4.3 依赖提取

当前几个常量/函数在 `tui/commands.go` 的 package private 中，ACP commands.go 需要引用：

| 原始位置 | 内容 | 提取目标 |
|----------|------|----------|
| `tui/commands.go` | `InitPromptTemplate` | `agent/commit_prompt.go` 或 `agent/acp/commands.go` |
| `tui/commands.go` | `commitUserPrompt()` | `agent/commit_prompt.go` 或 `agent/acp/commands.go` |
| `tui/commands.go` | `formatTokens()` | 复制到 `agent/acp/commands.go` 或用更简单的实现 |

也可以直接在 `agent/acp/commands.go` 中内联这些内容，避免引入 `tui` 包依赖。

---

## 五、ACPSession 需要新增的字段

当前 `ACPSession` 未持有 `*config.Config`，但 command handler 需要它（如 `/commit` 需要解析 commit provider）：

```diff
type ACPSession struct {
    ID           string
    cwd          string
    providerType string
+   cfg          *config.Config  // 新增：用于 slash command 内的 provider 解析等

    agent   *agent.AIAgent
    mcpMgr  *mcp.Manager
    sessMgr *session.Manager
    ...
}
```

在 `NewSession()` 和 `ResumeSession()` 中传入 `cfg`。

---

## 六、实施计划

### Phase 1：基础设施（1 天）

- [ ] `agent/acp/commands.go` — `ACPSlashCommand` 类型 + 注册表 + `parseSlashCommand` + `buildACPAvailableCommands`
- [ ] `ACPSession` 添加 `cfg` 字段
- [ ] `agent.go`: `NewSession()` 发送 `available_commands_update`
- [ ] `agent.go`: `Prompt()` 添加拦截逻辑（桩实现 —— 所有命令返回 "not yet implemented"）
- [ ] 验证：编辑器显示命令列表；输入 `/usage` 收到 placeholder 回应

### Phase 2：纯计算命令（1 天）

- [ ] `/usage` handler
- [ ] `/mcp list` + `/mcp reconnect` handler
- [ ] `/transcript` handler
- [ ] `/skill list` handler
- [ ] 验证：各命令返回正确结果，无需 LLM 调用

### Phase 3：LLM 驱动命令（2 天）

- [ ] `/commit` handler（提取 `commitUserPrompt`）
- [ ] `/init` handler（提取 `InitPromptTemplate`）
- [ ] `/compact` handler（使用 `DrainCompactEvents` + `FinalizeCompact`）
- [ ] `/skill <name>` handler
- [ ] 验证：完整的 LLM 交互流正常，streaming 正常，compact 后 session 可用

### Phase 4：动态 skill 命令（1 天）

- [ ] `buildACPAvailableCommands` 中加入 skill 动态注册
- [ ] `parseSlashCommand` 支持 skill 名称匹配
- [ ] `makeACPSkillHandler` 工厂函数
- [ ] 验证：不同 project 下的 skill 正确加载；skill 新增/删除后的动态更新

### Phase 5：测试与打磨（1 天）

- [ ] 单元测试：`parseSlashCommand` 各种输入
- [ ] 集成测试：`Prompt()` 拦截逻辑（mock `ACPSession`）
- [ ] 验证：`/compact` 后编辑器继续发消息正常
- [ ] 验证：`/commit` 工具注册表恢复正常
- [ ] 验证：`Cancel` 在 slash command 执行中正常工作

**总计：~6 天，~600 行新 Go 代码**（不含测试）

---

## 七、风险与注意事项

### 7.1 Cancel 语义

当前 `Cancel()` 只取消 `sess.promptCancel`——如果 slash command handler 内部启动了 `RunOneOffStream` / `RunConversationStream`，这些函数已经接收了 `promptCtx`（从 `sess.ctx` 派生），`promptCancel()` 会正确中断。

但**纯计算命令**（如 `/usage`、`/mcp list`）不通过 context 传播——它们同步执行。让它们也检查 `ctx.Done()`：

```go
func handleACPUsage(ctx context.Context, ...) {
    select {
    case <-ctx.Done():
        return acp.StopReasonCancelled, nil
    default:
    }
    // ... compute ...
}
```

### 7.2 并发安全

`Prompt()` 已通过 `sess.mu.Lock()` 保证串行。Slash command handler 在同一个临界区内执行，天然安全。

### 7.3 `/commit` 与 `/init` 的 Tool Registry 恢复

`/commit` 需要临时只保留 Bash 工具。Handler 内部必须 `defer RestoreToolRegistry`。如果 handler 中途 panic，defer 保证恢复。

`/init` 不需要修改 tool registry——它使用全部工具。

### 7.4 Skill 动态列表与 available_commands_update

`NewSession` 时发送一次 `available_commands_update`，包含所有 skill。后续 skill reload（`/skill reload`）后，需要重新发送。简化方案：`/skill reload` 调用后自动推送更新的命令列表。

### 7.5 未知 `/xxx` 的处理

`parseSlashCommand` 对未知命令返回 `nil`——消息透传到 LLM。这处理了：
- 用户讨论 slash commands（"/commit is useful"）
- 未加载的 skill（LLM 按指令理解）
- 打错字（`/commmit`）

不需要 strict validation，让 LLM 自行处理未识别的命令。

---

## 八、与现有设计的交互

| 现有系统 | ACP Slash Command 影响 |
|----------|----------------------|
| `agent/acp/stream.go` | 复用 `streamToACP()`，部分 handler（如 compact）可能需要 `streamAndCollectCompact` 变体 |
| `agent/compact.go` | 复用 `BuildCompactInstruction()`、`FinalizeCompact()`、`DrainCompactEvents()` |
| `agent/usage.go` | 复用 `ComputeSessionUsage()` |
| `agent/agent_memory.go` | 不变 |
| `agent/agent_skill.go` | 复用 `SkillStore()`、`ActivateSkill()`、`IsSkillActive()` |
| `agent/agent_provider.go` | 复用 `CommitProvider()` |
| `agent/acp/permission.go` | 不变——slash command handler 内部运行的工具调用仍走 permission handler |
| `agent/acp/session.go` | 新增 `cfg` 字段 |
| `agent/acp/convert.go` | 不变 |
| `tui/commands.go` | 不变——TUI 和 ACP 的 slash command 完全独立 |

---

## 九、备选：不做 slash command 拦截

**如果不在 ACP 层做拦截**，`/commit` 等文本会被当作普通消息发送给 LLM。LLM 可能"理解"这个意图并尝试执行，但：

1. **不可靠**：LLM 不一定知道 `/commit` 的准确含义（专用 prompt + 只留 Bash 工具）
2. **行为不一致**：同样叫 `/commit`，TUI 和 ACP 表现完全不同
3. **Compact 语义丢失**：compact 的 session 切换逻辑 LLM 无法完成

**结论**：拦截是必需的。但应该保持拦截和 LLM 路径的共存——未知命令透传是好的设计。

---

## 附录 A：SDK 类型参考

```go
// AvailableCommand — 广告给 Client 的命令定义
type AvailableCommand struct {
    Name        string                 `json:"name"`
    Description string                 `json:"description"`
    Input       *AvailableCommandInput `json:"input,omitempty"`
}

type AvailableCommandInput struct {
    Hint string `json:"hint"`
}

// SessionAvailableCommandsUpdate — available_commands_update 通知体
type SessionAvailableCommandsUpdate struct {
    Meta              map[string]any     `json:"_meta,omitempty"`
    AvailableCommands []AvailableCommand `json:"availableCommands"`
    SessionUpdate     string             `json:"sessionUpdate"` // const "available_commands_update"
}

// SessionUpdate — 联合体，AvailableCommandsUpdate 是其中一个 variant
type SessionUpdate struct {
    UserMessageChunk       *SessionUpdateUserMessageChunk   `json:"-"`
    AgentMessageChunk       *SessionUpdateAgentMessageChunk `json:"-"`
    AgentThoughtChunk       *SessionUpdateAgentThoughtChunk `json:"-"`
    ToolCall                *SessionUpdateToolCall          `json:"-"`
    ToolCallUpdate          *SessionToolCallUpdate          `json:"-"`
    Plan                    *SessionUpdatePlan              `json:"-"`
    AvailableCommandsUpdate *SessionAvailableCommandsUpdate `json:"-"`
    CurrentModeUpdate       *SessionCurrentModeUpdate       `json:"-"`
    ConfigOptionUpdate      *SessionConfigOptionUpdate      `json:"-"`
    SessionInfoUpdate       *SessionSessionInfoUpdate       `json:"-"`
    UsageUpdate             *SessionUsageUpdate             `json:"-"`
}

// SessionNotification 构造（通过 conn.SessionUpdate 发送）
type SessionNotification struct {
    SessionId SessionId     `json:"sessionId"`
    Update    SessionUpdate `json:"update"`
}
```

## 附录 B：完整数据流图（`/commit` 为例）

```
Editor                          Tachi (ACP Agent)
  │                                  │
  │ session/prompt {                 │
  │   prompt: [{text: "/commit"}]    │
  │ }                                │
  │ ─────────────────────────────>  │
  │                                  │ Prompt() 收到请求
  │                                  │ parseSlashCommand → 匹配 /commit
  │                                  │ handleACPCommit():
  │                                  │   1. 保存 tool registry → 只留 Bash
  │                                  │   2. RunOneOffStream(commitProvider, ...)
  │                                  │      → chan AgentEvent
  │                                  │
  │ <─  session/update {            │   3. streamToACP():
  │      agent_message_chunk: "..."  │      发送 text delta
  │    }                             │
  │ <─  session/update {            │      发送 tool call start
  │      tool_call: Bash(commit ...) │
  │    }                             │
  │ <─  session/update {            │      发送 tool result
  │      tool_call_update: done      │
  │    }                             │
  │ <─  session/update {            │      发送 final text
  │      agent_message_chunk: "..."  │
  │    }                             │
  │                                  │   4. 恢复 tool registry
  │                                  │   5. 返回 PromptResponse
  │ <─  session/prompt response {   │
  │      stopReason: "end_turn"      │
  │    }                             │
  │                                  │
```

---

### 版本历史

| 版本 | 日期 | 变更 |
|------|------|------|
| 1.0 | 2026-05-21 | 初始设计 |