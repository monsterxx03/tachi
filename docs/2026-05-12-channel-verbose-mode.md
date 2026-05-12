# Channel Verbose Mode (`/v` Command)

> Date: 2026-05-12
> Status: Draft

## Problem

Currently channel mode (e.g., WeChat bot via `channel/manager`) only outputs the LLM's **final text response**. All intermediate tool calls — which tools were invoked, with what arguments, and what results they returned — are silently dropped from the user-facing output. They are only visible in the `debug.log`.

This opacity makes it hard for users to:
- Know whether the agent actually searched for information or just guessed
- See what files were read/written before trusting the agent's summary
- Debug cases where the agent went down the wrong path but produced a plausible answer

## Solution: `/v` Toggle Command

A new `/v` slash command toggles **per-thread verbose mode**. When on, every reply is prefixed with a concise summary of the tool calls made during that turn.

Design priorities:
- **Per-thread state** (`map[threadID]bool`), no persistence — restart resets to off
- **Scoped to channel mode only** — TUI already renders tool calls inline
- **Minimal code impact** — changes are confined to `channel/manager/manager.go`

---

## Architecture

```
channel.Manager
├── verboseState map[string]bool          ← NEW
├── handleSlashCommand("/v")              ← NEW case
│   └── handleVerboseCommand(threadID)    ← NEW
├── handleSlashCommand("/new")
│   └── delete(verboseState, threadID)    ← MODIFIED
├── process()
│   └── reads verboseState[threadID]      ← MODIFIED
│       └── drainEvents(ch, agent, verbose) ← MODIFIED signature
│           ├── collects tool call summaries ← NEW
│           └── prepends them to final text  ← NEW
└── default slash-command help text       ← MODIFIED (add /v line)
```

## Details

### 1. State: `verboseState`

```go
type Manager struct {
    // ... existing fields ...

    verboseState map[string]bool   // threadID -> verbose toggle
    verboseMu    sync.RWMutex
}
```

- RWMutex: reads (`process()`) are cheap, writes (`/v`) are infrequent.
- Lazy-init on first `/v` call.
- No persistence — ephemeral runtime state. If we later want persistence, can store as a `session.Metadata` key.

### 2. `/v` Slash Command

```go
func (m *Manager) handleVerboseCommand(threadID string) (string, error) {
    m.verboseMu.Lock()
    if m.verboseState == nil {
        m.verboseState = make(map[string]bool)
    }
    current := m.verboseState[threadID]
    m.verboseState[threadID] = !current
    m.verboseMu.Unlock()

    if !current {
        return "🔍 Verbose mode: ON\nSubsequent replies will include tool call details.", nil
    }
    return "🔍 Verbose mode: OFF\nSubsequent replies will only show the final result.", nil
}
```

### 3. `/new` Reset

```go
func (m *Manager) handleNewCommand(threadID string) (string, error) {
    // ... existing session logic ...

    // Reset verbose state for the new session.
    m.verboseMu.Lock()
    if m.verboseState != nil {
        delete(m.verboseState, threadID)
    }
    m.verboseMu.Unlock()

    return "✅ Started a new conversation. Previous session has been ended.", nil
}
```

### 4. `process()` — Pass Verbose Flag

```go
func (m *Manager) process(ctx context.Context, msg channel.IncomingMessage) (string, error) {
    // ... existing agent setup ...

    m.verboseMu.RLock()
    verbose := m.verboseState[msg.ThreadID]
    m.verboseMu.RUnlock()

    return m.drainEvents(eventCh, aiAgent, verbose)
}
```

Same change applied in `OnCronTrigger` — read `verboseState[job.TargetThreadID]`.

### 5. `drainEvents()` — Collect + Prepend Tool Summaries

**Signature change:**

```go
// Old:
func (m *Manager) drainEvents(ch <-chan agent.AgentEvent, aiAgent *agent.AIAgent) (string, error)

// New:
func (m *Manager) drainEvents(ch <-chan agent.AgentEvent, aiAgent *agent.AIAgent, verbose bool) (string, error)
```

**New behavior** (only when `verbose == true`):

- Intercept `AgentEventToolCallArgs` and `AgentEventToolResult` events.
- Build a `[]string` of formatted one-liners.
- After event loop ends, if the list is non-empty, prepend:
  ```
  🔍 工具调用过程:
  🔧 ReadFile(path.go)
    ✅ 读取 42 行
  🔧 Bash(go build ./...)
    ✅ 输出 5 行 (126B)

  <original LLM response>
  ```
- If the agent made zero tool calls, no prefix is added — the output is identical to non-verbose mode.

**Events NOT exposed:**

| Event | Why hidden |
|-------|------------|
| `AgentEventThinkingDelta` | LLM internal chain-of-thought; extremely verbose (100s–1000s of chars), not suitable for IM |
| `AgentEventSubagentStart/Done` | Sub-agent lifecycle is an implementation detail; output is consumed by parent agent |
| `AgentEventToolConfirmation` | Already auto-approved (`skipEditConfirm=true`) |
| `AgentEventAskUser` | Already auto-rejected in channel mode |

### 6. Tool Summary Formatting

Each registered tool gets a lightweight formatter that extracts the most informative fields from its JSON arguments and results.

#### Tool Call Line

```
🔧 <tool_name>(<args_summary>)
```

| Tool | Summary extracted from |
|------|----------------------|
| `ReadFile` | `path`, optional `offset`/`limit` → `main.go` or `main.go L10+20` |
| `WriteFile` | `path` |
| `EditFile` | `path` |
| `Bash` | `command` (truncated to 60 chars) |
| `Grep` | `pattern` (truncated to 40 chars) |
| `Glob` | `pattern` |
| `WebSearch` | `query` (truncated to 40 chars) |
| `WebFetch` | `url` (truncated to 50 chars) |
| `SubAgent` | First 60 chars of `prompt` |
| All others (MCP tools, etc.) | Raw args JSON truncated to 60 chars |

#### Tool Result Line

```
  ✅ <result_summary>
  ❌ Error: <error truncated to 150 chars>
```

| Tool | Result summary |
|------|---------------|
| `ReadFile` | `读取 <N> 行` |
| `WriteFile` | `写入完成` |
| `EditFile` | `编辑完成` |
| `Bash` | `输出 <N> 行 (<size>)` — if output ≤ 200 chars, show raw |
| `Grep` | `匹配 <N> 行` |
| `Glob` | `匹配 <N> 个文件` |
| `WebSearch` | `搜索完成` |
| `WebFetch` | `抓取完成 (<size>)` |
| All others | `<N> 行 (<size>)` — if ≤ 200 chars, show raw |

Handling for **parallel tool calls** (`agent/tool_executor.go`): `executeToolCallsParallel` emits all `ToolCallArgs` first, then all `ToolResult` in order. The verbose collector naturally follows the event channel ordering, producing:

```
🔧 ReadFile(a.go)
🔧 ReadFile(b.go)
  ✅ 读取 42 行
  ✅ 读取 17 行
```

This correctly conveys the parallel execution pattern without additional grouping logic.

### 7. Length Budget

WeChat's `sendTextReply` has no explicit truncation, but practical limits apply (~2048 chars per message). Verbose overhead per tool call:

```
🔧 ReadFile(path.go)    → ~30 chars
  ✅ 读取 42 行          → ~15 chars
```

≈45 chars per tool call pair. Typical agent turns use 2–8 tool calls → **90–360 chars** overhead. This is negligible for the common case.

If a future channel has stricter limits, we can add:
- A max tool summary line count (e.g., cap at 15 lines, then `  ... 还有 3 次调用`)
- Collapse parallel reads of the same file

### 8. WeChat Markdown Filter Compatibility

`sendTextReply` → `filterMarkdown()` strips `**bold**`, `### headings`, code fences, etc.

The verbose prefix uses only:
- Emoji (`🔍`, `🔧`, `✅`, `❌`) — passed through unchanged
- Plain ASCII text — passed through unchanged
- The original LLM response already goes through `filterMarkdown`

No special handling needed.

---

## Example Output

### Non-verbose (current)

```
找到了 3 处需要修改的地方。主要是在 handler.go 中添加了输入校验，并更新了对应的测试。
```

### Verbose

```
🔍 工具调用过程:
🔧 Grep("ValidateInput")
  ✅ 匹配 3 行
🔧 ReadFile(internal/handler.go)
  ✅ 读取 156 行
🔧 ReadFile(internal/handler_test.go)
  ✅ 读取 89 行
🔧 EditFile(internal/handler.go)
  ✅ 编辑完成
🔧 Bash(go test ./internal/...)
  ✅ 输出 12 行 (384B)

找到了 3 处需要修改的地方。主要是在 handler.go 中添加了输入校验，并更新了对应的测试。所有测试通过。
```

### Verbose (zero tool calls — degenerate case)

```
好的，这个不需要改动，目前的实现已经正确处理了边界情况。
```

No prefix — identical to non-verbose.

---

## Implementation Plan

Single PR, single file changed:

1. Add `verboseState map[string]bool` and `verboseMu sync.RWMutex` to `Manager` struct
2. Add `handleVerboseCommand()` method
3. Add `/v` case to `handleSlashCommand()` switch
4. Add `/v` to the default help text
5. Reset `verboseState[threadID]` in `handleNewCommand()`
6. Read `verboseState` in `process()` + `OnCronTrigger()`, pass to `drainEvents()`
7. Change `drainEvents()` signature, add event collection + prefix logic
8. Add `summarizeToolArgs()` / `summarizeToolResult()` / helpers

---

## Future Directions

- **`/v full` vs `/v brief`**: brief shows only tool names (no results), full shows results
- **Config-driven default**: `channel.verbose_default: true` in `config.yaml`
- **Persist across restarts**: store `verbose` in session metadata
- **Selective filtering**: `/v show Bash,Edit` to only show specific tool types
- **Streaming intermediate replies**: send tool call lines as they happen instead of all at the end (requires breaking the "one message in, one message out" model — significantly more complex)