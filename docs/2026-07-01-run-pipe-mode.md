# `tachi run` 管道模式改造

> 版本: 1.0 | 日期: 2026-07-01 | 状态: 设计阶段

## 一、动机

当前 `tachi run` 的定位是"非交互式单次对话"，但输出方式存在根本性问题：

1. **进度信息污染 stdout** — `fmt.Printf("Provider: ...")` 直接写到 stdout，导致 pipe 到下游命令时带垃圾
2. **输出只有最终快照** — LLM 的回复文本必须等整个 agent loop 结束才一次性打印，不能边跑边看
3. **缺乏结构化输出** — `--json` flag 生成的 JSON 没有 schema 约束，也没有流式事件
4. **没有工具权限控制** — 每次调用无法限制 LLM 可用的工具，CI/CD 场景不安全
5. **stdin 独占读取** — `io.ReadAll` 一次性吞掉整个 stdin，不能在 agent loop 中边读边消费

改造目标：让 `tachi run` 成为一个真正的 **Unix 管道公民**。

```bash
# 管道输入 + 安静输出 → 链式组合
tail -200 app.log | tachi run -p "分析异常"
git diff main | tachi run -p "review" | jq '.summary'

# 结构化流式输出 → 程序化消费
tachi run -p "重构" --output-format json-stream \
  | jq --unbuffered 'select(.type == "tool_call") | .tool_name'

# 工具权限限制 → CI 安全
tachi run -p "部署" --disallowed-tools Bash --timeout 5m
```

## 二、设计

### 2.1 退出 `--json`，引入 `--output-format`

删除 `--json` flag，新增 `--output-format`：

| 值 | 行为 | 典型场景 |
|----|------|---------|
| `text` | 人类可读，流式输出 TextDelta 到 stdout | 终端交互、管道输入 |
| `json` | 单次 JSON 对象，agent 结束后输出 | 脚本消费、CI 单次检查 |
| `json-stream` | NDJSON 流，实时输出每个 AgentEvent | 实时监控、管道链式处理 |

`text` 是默认值。

### 2.2 安静模式

新增 `--quiet` / `-q` flag。当启用时：
- stdout **只输出 LLM 回复文本**（text 模式）或 JSON（json/json-stream 模式）
- 所有进度信息（Provider 信息、tool call 日志、exit summary）写到 stderr

安静模式**自动推断**规则：
- stdout 不是终端（pipe 到文件或另一个命令）→ 自动安静
- 终端 → 正常输出进度
- 显式 `--quiet` / `-q` → 强制安静

```go
quiet := cmd.Bool("quiet") || !term.IsTerminal(int(os.Stdout.Fd()))
```

### 2.3 工具权限控制

新增两个 flag，互斥使用（白名单优先）：

| Flag | 格式 | 示例 | 效果 |
|------|------|------|------|
| `--allowed-tools` | 逗号分隔 | `ReadFile,Grep` | 只保留这俩工具，其他全部注销 |
| `--disallowed-tools` | 逗号分隔 | `Bash,WriteFile` | 只注销这俩，其他保留 |

匹配基于 `agent/tools/tool.go` 中的工具名常量：
- `Bash`, `ReadFile`, `WriteFile`, `EditFile`, `Glob`, `Grep`
- `WebSearch`, `WebFetch`, `SubAgent`, `Skill`, `LSP`
- MCP 工具：`mcp__<server>__<tool>`（支持前缀匹配）

```bash
# 只读分析模式
tachi run -p "审查代码安全性" --allowed-tools ReadFile,Grep,Glob

# 禁止执行命令
tachi run -p "更新 package.json" --disallowed-tools Bash

# 只允许 git 操作（通过限制 tool，具体命令由 LLM 自觉遵守）
tachi run -p "提交代码" --allowed-tools Bash
```

当 `--allowed-tools` 和 `--disallowed-tools` 同时指定时：
1. 先应用白名单：只保留 `--allowed-tools` 中的工具
2. 再从白名单中排除 `--disallowed-tools` 中的工具

### 2.4 stdin 输入

输入优先级：
1. `--prompt` / `-p` 优先级最高（显式提供）
2. 没有 `--prompt` 时自动检测 stdin pipe → `io.ReadAll`
3. 既没有 `--prompt` 也没有 stdin → **报错退出**（不再有默认 prompt）

### 2.5 输出的文本模式改进

**当前**：`text` 模式将 LLM 回复攒到 `result.Response`，agent loop 结束后一次性 `fmt.Printf("Response:\n%s\n", result.Response)`。

**改造后**：每收到 `AgentEventTextDelta` 就实时 `fmt.Fprint(os.Stdout, event.TextDelta)`，stdout 逐字符输出。

```go
// runOutputText — 流式人类可读输出
func runOutputText(ctx context.Context, ch <-chan agent.AgentEvent, quiet bool) *agent.RunResult {
    var result *agent.RunResult
    for event := range ch {
        switch event.Type {
        case agent.AgentEventTextDelta:
            fmt.Fprint(os.Stdout, event.TextDelta) // ← 实时输出
            if f, ok := os.Stdout.(*os.File); ok {
                f.Sync() // 确保立即 flush
            }

        case agent.AgentEventThinkingDelta:
            if !quiet {
                fmt.Fprint(os.Stderr, event.ThinkingDelta)
            }

        case agent.AgentEventToolCallStart:
            if !quiet {
                fmt.Fprintf(os.Stderr, "\n🔧 %s(", event.ToolName)
            }

        case agent.AgentEventToolCallArgs:
            if !quiet {
                fmt.Fprintf(os.Stderr, "%s...)\n", truncate(event.ToolArgs, 60))
            }

        case agent.AgentEventToolResult:
            if !quiet {
                icon := "✅"
                if event.ToolIsError {
                    icon = "❌"
                }
                fmt.Fprintf(os.Stderr, " %s (%s)\n", icon, event.ToolDuration.Round(time.Millisecond))
            }

        case agent.AgentEventTurnComplete:
            result = event.Result

        case agent.AgentEventError:
            result = event.Result

        case agent.AgentEventToolConfirmation:
            aiAgent.ConfirmTool(true)
        }
    }
    return result
}
```

### 2.6 JSON 输出格式

#### `--output-format json`（单次 JSON，替换现有 `--json`）

```json
{
  "exit_reason": "stop",
  "iterations_used": 3,
  "response": "分析完成...",
  "usage": {
    "input_tokens": 1500,
    "output_tokens": 300,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0
  },
  "error": ""
}
```

与现有 `--json` 的区别：
- 进度信息全部走 stderr
- 没有 `schema_version` 或 `tool_calls` 等新字段（保持简洁）
- 纯数据，无额外文本包装

#### `--output-format json-stream`（NDJSON 流）

每行一个 JSON 事件：

```
{"type":"text_delta","content":"我来"}
{"type":"text_delta","content":"分析"}
{"type":"thinking_delta","content":"用户想要..."}
{"type":"tool_call","tool_name":"Bash","tool_args":"{\"command\":\"ls\"}","tool_call_id":"call_abc"}
{"type":"tool_result","tool_name":"Bash","tool_result":"main.go\nREADME.md\n","duration_ms":342,"is_error":false}
{"type":"text_delta","content":"目录下有以下文件"}
{"type":"turn_complete","exit_reason":"stop","iterations_used":2,"usage":{"input_tokens":1500,"output_tokens":300}}
{"type":"error","error":"rate limit exceeded"}
```

事件类型完整列表：

| type | 字段 | 说明 |
|------|------|------|
| `text_delta` | `content` | LLM 回复文本片段 |
| `thinking_delta` | `content` | LLM 思考过程片段 |
| `tool_call` | `tool_name`, `tool_args`, `tool_call_id` | 工具调用开始 |
| `tool_result` | `tool_name`, `tool_result`, `duration_ms`, `is_error` | 工具执行结果 |
| `turn_complete` | `exit_reason`, `iterations_used`, `usage` | agent 回合完成 |
| `error` | `error` | agent 出错 |

```bash
# 消费示例
tachi run -p "分析代码" --output-format json-stream \
  | jq --unbuffered -c 'select(.type == "tool_result") | {tool: .tool_name, dur: .duration_ms}'
```

### 2.7 退出码

保留现有 `exitCodeForReason` 映射：

| 退出码 | 含义 |
|--------|------|
| `0` | `stop` — 正常完成 |
| `1` | `error` / `cancelled` — 出错 |
| `2` | `budget_exhausted` / `length_exhausted` — 达到限制 |
| `130` | `interrupted` — SIGINT（Ctrl+C） |

## 三、代码变更

### 3.1 文件清单

| 文件 | 变更 | 说明 |
|------|------|------|
| `main.go` | 修改 | 重构 `runAgent()`，替换 flag，实现三种输出模式 |
| `agent/tools/tool.go` | 无变更 | 工具名常量已存在，直接复用 |
| `go.mod` | 修改 | `golang.org/x/term` 从 indirect 提升为 direct |

### 3.2 `main.go` 重构

**当前**：`runAgent()` 是一个约 160 行的大函数（L369-527）。

**改造后**：拆分为 5 个函数：

```go
// runAgent 是 tachi run 子命令的入口
func runAgent(ctx context.Context, cmd *cli.Command) error {
    cfg := loadConfig()
    aiAgent := setupAgent(ctx, cfg)
    defer aiAgent.Close()

    outputFmt := parseOutputFormat(cmd)    // "text" | "json" | "json-stream"
    quiet := resolveQuiet(cmd)
    prompt := resolvePrompt(cmd)
    applyToolRestrictions(aiAgent, cmd)

    ch := aiAgent.RunConversationStream(ctx, history, prompt, systemPrompt, opts)

    result := runOutputLoop(ctx, aiAgent, ch, outputFmt, quiet)
    os.Exit(exitCodeForReason(result.ExitReason))
    return nil
}

// runOutputLoop dispatches to the correct output handler
func runOutputLoop(ctx context.Context, aiAgent *agent.AIAgent,
    ch <-chan agent.AgentEvent, outputFmt string, quiet bool) *agent.RunResult {
    switch outputFmt {
    case "json":
        return runOutputJSON(ch)
    case "json-stream":
        return runOutputJSONStream(ctx, aiAgent, ch)
    default:
        return runOutputText(ctx, aiAgent, ch, quiet)
    }
}

// runOutputText — 流式人类可读输出到 stdout，进度到 stderr
func runOutputText(ctx context.Context, aiAgent *agent.AIAgent,
    ch <-chan agent.AgentEvent, quiet bool) *agent.RunResult { ... }

// runOutputJSON — 单次 JSON 对象到 stdout，进度到 stderr
func runOutputJSON(ch <-chan agent.AgentEvent) *agent.RunResult { ... }

// runOutputJSONStream — NDJSON 流到 stdout，进度到 stderr
func runOutputJSONStream(ctx context.Context, aiAgent *agent.AIAgent,
    ch <-chan agent.AgentEvent) *agent.RunResult { ... }
```

### 3.3 CLI flag 注册

```go
{
    Name:  "run",
    Usage: "Run the AI agent (single-turn)",
    Flags: append(commonFlags,
        &cli.StringFlag{
            Name:    "prompt",
            Aliases: []string{"p"},
            Usage:   "User prompt to send (if empty, reads stdin)",
        },
        &cli.StringFlag{
            Name:  "output-format",
            Usage: "Output format: text (default) | json | json-stream",
        },
        &cli.BoolFlag{
            Name:    "quiet",
            Aliases: []string{"q"},
            Usage:   "Suppress progress output to stderr (auto-enabled when stdout is piped)",
        },
        &cli.StringFlag{
            Name:  "allowed-tools",
            Usage: "Comma-separated whitelist of tool names the agent may use",
        },
        &cli.StringFlag{
            Name:  "disallowed-tools",
            Usage: "Comma-separated blacklist of tool names the agent may NOT use",
        },
        &cli.DurationFlag{
            Name:  "timeout",
            Usage: "Maximum execution time (e.g. 5m, 30s, 1h)",
        },
    ),
    Action: runAgent,
},
```

### 3.4 `--json` 迁移

删除 `--json` bool flag。用户需要改用 `--output-format json`。

```bash
# 旧（将失效）
tachi run --json --prompt "hello"

# 新
tachi run --output-format json --prompt "hello"
```

## 四、实现步骤

### Step 1：Flag 替换和配置解析

- 删除 `--json` flag
- 新增 `--output-format`, `--quiet`, `--allowed-tools`, `--disallowed-tools`
- 实现 `parseOutputFormat()`, `resolveQuiet()`, `resolvePrompt()`, `applyToolRestrictions()`
- 引入 `golang.org/x/term`（已存为 indirect 依赖）

### Step 2：`runOutputText()` — 流式文本输出

- 删除 `result.Response` 的拼接后打印
- 改为在 `AgentEventTextDelta` 时实时 `fmt.Fprint(os.Stdout, ...)`
- `AgentEventThinkingDelta` 在有 `--verbose` 或非 `--quiet` 时输出到 stderr
- Tool call 事件输出到 stderr
- 确认 (`AgentEventToolConfirmation`) 自动确认（当前行为）

### Step 3：`runOutputJSON()` — 单次 JSON

- 保持基本行为不变
- 所有 `fmt.Printf(...)` 进度输出改为 `fmt.Fprintf(os.Stderr, ...)`
- 删除 `response` 前的 "Response:\n" 文本包装

### Step 4：`runOutputJSONStream()` — NDJSON 流

- 定义 `streamEvent` 结构体及 JSON tag
- Agent 事件循环中，对每种事件类型编码一行 JSON
- 使用 `json.NewEncoder(os.Stdout).Encode()` 输出

### Step 5：安静模式自动检测

- 在 `main()` 或 `runAgent()` 中检测 stdout 是否为终端
- `quiet = cmd.Bool("quiet") || !term.IsTerminal(int(os.Stdout.Fd()))`
- `--quiet` 手动设置时覆盖自动检测

### Step 6：工具限制

- 在 `runAgent()` 中，agent 初始化完成后、RunConversationStream 之前调用
- `allowed-tools` 模式：遍历所有工具，不在白名单中的 `UnregisterTool`
- `disallowed-tools` 模式：遍历所有工具，在黑名单中的 `UnregisterTool`
- 同时指定时：先白后黑

### Step 7：清理

- 删除 `runJSONResult` 结构体（由 `--output-format json` 的输出替代）
- 删除 `usageToJSON()` 辅助函数（内联到 JSON 输出路径）
- 删除 `--json` flag 相关逻辑

## 五、测试用例

### 功能测试

```bash
# 1. 默认 text 模式（终端）
tachi run -p "echo hello"

# 2. 默认 text 模式（管道 stdout → 自动安静）
echo "echo hello" | tachi run          # stdout 只输出 "hello\n"

# 3. 强制安静
echo "echo hello" | tachi run -q       # 同上

# 4. JSON 输出
tachi run -p "echo hello" --output-format json
# → {"exit_reason":"stop","response":"hello\n","iterations_used":1,...}

# 5. NDJSON 流
tachi run -p "echo hello" --output-format json-stream
# → {"type":"tool_call",...}
# → {"type":"tool_result",...}
# → {"type":"text_delta","content":"hello\n"}
# → {"type":"turn_complete",...}

# 6. 工具限制 — 白名单
tachi run -p "列出文件" --allowed-tools ReadFile,Grep,Glob
# Bash 不可用，LLM 只能用 ReadFile/Grep/Glob

# 7. 工具限制 — 黑名单
tachi run -p "修复 bug" --disallowed-tools SubAgent
# SubAgent 不可用

# 8. 管道链式
echo "hello world" | tachi run -q | wc -c
# 输出字符数

# 9. JSON-Stram + jq 过滤
tachi run -p "echo hello" --output-format json-stream \
  | jq -s '.[] | select(.type == "turn_complete") | .exit_reason'
# → "stop"
```

### 边界条件

```bash
# 10. 空 stdin（没有 --prompt 也没有 pipe）
tachi run    # 报错退出，提示需要 prompt

# 11. --prompt 优先级高于 stdin
echo "来自 stdin" | tachi run -p "来自 --prompt"
# LLM 收到 "来自 --prompt"

# 12. 超时
tachi run -p "sleep 60" --timeout 3s
# 退出码 130？或 1？
```

### 单元测试

| 测试 | 位置 | 说明 |
|------|------|------|
| `TestParseOutputFormat` | `main_test.go` | 解析 `--output-format` 各值 |
| `TestResolveQuiet` | `main_test.go` | 自动安静检测逻辑 |
| `TestApplyToolRestrictions` | `main_test.go` | 白名单/黑名单过滤 |
| `TestExitCodeForReason` | `main_test.go` | 退出码映射（已有，扩展） |

### 集成测试

| 测试 | 说明 |
|------|------|
| pipe input → text output | 验证 stdout 只有 LLM 回复 |
| pipe input → json output | 验证 stdout 是合法 JSON |
| pipe input → json-stream output | 验证每行是合法 JSON |
| terminal → text output | 验证进度信息可见 |
| terminal → quiet text output | 验证进度信息被抑制 |

## 六、不兼容变更清单

| 变更 | 影响 |
|------|------|
| 删除 `--json` flag | 使用 `--output-format json` 替代 |
| text 模式改为流式输出 | 脚本中 `tachi run` 的输出不再有 `Response:` 前缀 |
| 进度信息改为 stderr | 管道场景下 stdout 更干净 |
| 删除 `runJSONResult` 结构体 | 重构内部细节，不影响外部 API |

## 七、参考

- Claude Code CLI reference: `-p`, `--output-format`, `--allowedTools`, `--disallowedTools`
- NDJSON spec: https://github.com/ndjson/ndjson-spec
- `golang.org/x/term`: https://pkg.go.dev/golang.org/x/term
