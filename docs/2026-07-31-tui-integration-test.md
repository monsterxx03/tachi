# TUI 集成测试设计文档

> 日期: 2026-07-31 | 状态: 设计定稿，可直接实现
>
> **2026-07-31 评审修订**：采纳评审 8 条发现（§八 隔离口径统一、示例场景补种子文件、`●` 空闲信号歧义处理、双协议重试机理差异、新增契约测试、场景编号、`Hold()` 清理、`Require` 与截断边界），并补充 §6.2 配置文件处理。
>
> **已确认的技术决策**：① mock LLM 用 **HTTP 兼容服务器**，同时支持 **OpenAI 与 Anthropic 两种线上协议**（零生产代码改动，测真实 HTTP/SSE/重试栈）；② 测试风格用 **Ginkgo + Gomega**（BDD 词汇，可读性优先）；③ 整个套件独立于单元测试，用 `//go:build integration` 隔离。

## 一、概述

### 动机

现有测试分两层：

- **单元测试**（`go test ./...`）：覆盖 `Model.Update` 状态机、`startTurn`、stream 消费者、工具执行等纯逻辑。`testModel()` 里根本没有真实 agent，`sendMessage` 都跑不通。
- **进程级 ACP 测试**（`agent/acp/`）：在进程内构造 `AIAgent` + mock Provider，测协议层，但不碰真实二进制。

两层都**没有**验证过：真实 `tachi` 二进制 + 真实 agent loop + 真实终端渲染这一整条链。`tui/model.go` 里 `stateWaiting → stateStreaming → stateIdle`、`pendingQueue`、steer 检查、`AskUserQuestion` 表单这些跨层交互，一旦在真实环境下出问题（goroutine 时序、TTY 渲染、配置解析），单测根本测不到——历史上有过 steer 死锁、stale event 这类 bug，全是跨层问题。

本设计新增**三层进程级集成测试**（与单元测试完全独立），按依赖复杂度递进：

| 层 | driver | 验证面 | 前置依赖 |
|----|--------|--------|----------|
| **`-p` 链路**（M0，**最先实现**） | `exec.Command` + 管道 | agent loop → mock LLM → 工具执行 → 输出管线 | 无（不需要 tmux） |
| **TUI**（M1/M2） | tmux 键盘驱动真实 TUI | 链路 + 终端渲染与交互（pending/steer/表单/取消） | tmux |
| **ACP**（M3） | stdio JSON-RPC（acp-go-sdk） | 链路 + 事件流协议 | 无（复用 mockllm） |

`-p` 层最先实现的理由：不依赖 tmux/TTY；`-o json-stream` 把 AgentEvent 序列直接暴露成 NDJSON，是比屏幕文本更精确的断言面；且 `-p` 是 `--allowed-tools` **唯一真实生效**的模式（见 §6.1），隔离有双保险。先确保"agent loop → mock LLM API"这条链路 okay，再叠 tmux 渲染层。

### 设计原则

1. **零生产代码改动** — mock 走 `base_url` 配置层，TUI/agent/llm 一行不改
2. **黑盒优先** — 断言基于屏幕文本、mock 收到的请求、落盘的 session 文件，不读进程内部状态
3. **确定性优先** — mock 按脚本逐请求应答，脚本耗尽即 fail（防 agent loop 失控）；流式节奏由 mock 控制（可插入停顿），保证交互类场景可确定地插入键盘输入
4. **可读性优先** — Ginkgo + Gomega 的场景即文档（BDD 词汇），失败时自动 dump 屏幕 + 请求日志
5. **隔离必须彻底** — `--home` + 临时工作目录 + 工具白名单，不碰本地系统
6. **与单测不混跑** — `//go:build integration` + `make itest` 显式运行，`go test ./...` 保持现状
7. **分层递进** — `-p` 链路（M0）先行：无 TTY、断言面最简、`--allowed-tools` 真实生效；通了再叠 tmux 渲染层与 ACP 事件流层

---

## 二、总体架构

```
itest/ 场景（Ginkgo 描述层）
  │
  ├── run driver ──────────► tachi -p（exec + 管道，M0 先行）
  │   NDJSON / 文本 / 退出码断言
  ├── tmux driver ──────► 真实 tachi TUI（tmux 键盘驱动）
  │   send-keys / capture-pane
  ├── acp driver ───────► tachi acp（stdio JSON-RPC）
  │   acp-go-sdk ClientSideConnection
  │                                        │
  └──────► mockllm ◄────────────────────────┘
        httptest.Server（双协议：OpenAI /v1/chat/completions、
        Anthropic /v1/messages）
        流式 SSE / 非流式 JSON / 故障注入
        请求记录 → 断言 agent loop 行为
```

三层结构：

| 层 | 职责 | 位置 |
|----|------|------|
| **场景层** | BDD 描述：用户行为 + 断言 | `itest/run/`、`itest/tui/`、`itest/acp/` |
| **驱动层** | `-p` 管道 / tmux 会话 / ACP stdio 连接 | `itest/run/`、`itest/tmux/`、`itest/acp/` |
| **桩层** | mock LLM 服务器 + 脚本 + 断言 API | `itest/mockllm/` |

一次场景的完整数据流：

```
Scenario: 工具调用流程
  mockllm.Script(步骤1: tool_call(Bash), 步骤2: 文本回答)
        │
  tmux.Launch(bin --home <tmp>；工具集由 mock 脚本决定，见 §6.1)
        │
  send-keys "看一下当前目录" Enter
        │                      │
        ▼                      ▼
  TUI 渲染"~ Bash(...)"   ──►  mock 收到第 1 个请求
        │                      │
        ▼                      ▼
  TUI 渲染"v Bash(...)"   ──►  mock 收到第 2 个请求（含 tool result）
        │
        ▼
  TUI 渲染最终回答；断言 mock 请求体、session 文件
```

---

## 三、Mock LLM 服务器（`itest/mockllm`）

### 3.1 协议对齐（OpenAI）

mock 必须精确复刻 `llm/openai.go`（go-openai 客户端）期望的线格式。从客户端代码反推出的契约如下：

**请求**（`POST {base_url}/v1/chat/completions`）：

```json
{
  "model": "mock-model",
  "messages": [...],
  "tools": [...],
  "stream": true,
  "stream_options": {"include_usage": true}
}
```

**流式响应**（`Content-Type: text/event-stream`，每条 `data: <json>\n\n`，结尾 `data: [DONE]`）：

| 语义 | chunk 内容 | 客户端映射（`openai.go`） |
|------|-----------|--------------------------|
| 思考 | `delta.reasoning_content` | `StreamEventThinkingDelta` → TUI thinking view |
| 文本 | `delta.content` | `StreamEventTextDelta` → chatview 流式渲染 |
| 工具调用开始 | `delta.tool_calls[0]` 带 `id`/`type`/`function.name` | `StreamEventToolUseStart` |
| 工具参数增量 | `delta.tool_calls[0].function.arguments`（可多片） | `StreamEventInputJSONDelta` |
| 结束原因 | `delta:{}` + `finish_reason:"tool_calls"`/`"stop"` | `StreamEventMessageDelta` |
| usage | `choices: []` + `usage:{prompt_tokens,completion_tokens}` | 客户端先捕获 usage 再跳过空 choices |
| 流结束 | `data: [DONE]` | EOF → `StreamEventDone` |

> ⚠️ usage chunk 必须 `choices: []`（客户端 `len(resp.Choices) == 0 { continue }`，但 usage 在其前已捕获）；`[DONE]` 前必须有 usage chunk，否则状态栏 ctx 占比不会更新。

**非流式响应**：`CreateChat` 会走到（首条消息的标题生成 `generateTitle`、`/compact`、deepresearch）。返回标准 `ChatCompletionResponse` JSON，`message.reasoning_content` 承载思考。测试配置默认 `title_generation: false` 屏蔽标题调用，让脚本只面对主 loop 的流式调用。

### 3.2 协议对齐（Anthropic）

`type: anthropic` 的 provider 走 `/v1/messages`。从 `llm/anthropic.go`（anthropic-sdk-go v1.37 的 `Messages.NewStreaming`）反推的 SSE 事件契约：

**请求**（`POST {base_url}/v1/messages`，携带 `anthropic-version`/`x-api-key` header）：

```json
{
  "model": "mock-model",
  "max_tokens": 4096,
  "system": "...",
  "messages": [...],
  "tools": [...],
  "stream": true
}
```

**流式响应**（每条 `event: <type>` + `data: <json>`，json 内 `type` 字段与 event 名一致）：

| 语义 | 事件序列 | 客户端映射（`anthropic.go`） |
|------|---------|------------------------------|
| 开始 | `message_start`（`message.usage.input_tokens`） | SDK 累积 usage |
| 思考 | `content_block_start`(thinking) → `content_block_delta`(`thinking_delta`) → `signature_delta` → `content_block_stop` | `StreamEventThinkingDelta` / `SignatureDelta` |
| 文本 | `content_block_start`(text) → `content_block_delta`(`text_delta`) → `content_block_stop` | `StreamEventTextDelta` |
| 工具调用 | `content_block_start`(tool_use，含 id/name/input) → `content_block_delta`(`input_json_delta`) → `content_block_stop` | `StreamEventToolUseStart` / `InputJSONDelta` |
| 结束原因 | `message_delta`（`delta.stop_reason` + `usage.output_tokens`） | `StreamEventMessageDelta` + usage |
| 流结束 | `message_stop` | SDK 结束 → `StreamEventDone` |
| 心跳 | `ping`（可任意位置插入） | SDK 静默处理 |

要点：

- `stop_reason` 取值：`end_turn` / `tool_use` / `max_tokens` / `stop_sequence` 等
- **思考块必须带 signature**：agent loop 会把 assistant 的 ThinkingBlocks（含 signature）原样回传历史（`buildRequest` 用 `NewThinkingBlock(tb.Signature, tb.Thinking)`），mock 缺 signature 会让多轮思考场景的历史回传失真
- `content_block_start`(tool_use) 的 `input` 先给空对象，参数靠 `input_json_delta` 增量传
- 非流式同理：`stream: false` 返回标准 Message JSON（content 块数组，`CreateChat` 对 thinking/tool_use 块有对应解析）

**双协议共用一套脚本**：协议只是渲染层差异。mock 服务器按模式构造（`mockllm.NewServer(mockllm.ProtocolOpenAI)` / `mockllm.NewServer(mockllm.ProtocolAnthropic)`），同一份 `Stream(Thinking(...), Text(...), ToolCallStart(...), Finish(...), Usage(...), Done())` 在内部渲染成对应线格式；`Require` 断言面也抽象成协议无关的访问器（`HasToolResult`/`HasUserMessage` 等把两种请求体归一成内部 Message 视图）。场景层不感知协议，切换 provider `type` 即可复用整套脚本——"同一场景在两种 provider 下行为一致"因此成为可测断言。

### 3.3 脚本模型

```go
mock := mockllm.NewServer()
mock.Script(
    // 第 1 个请求：要求携带某系统提示，回复工具调用
    mockllm.Step{
        Require: mockllm.HasSystemPrompt(ContainSubstring("tachi")),
        Reply: mockllm.Stream(
            mockllm.Thinking("让我查一下目录"),
            mockllm.Text("我来看看"),
            mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
            mockllm.Pause(300*time.Millisecond), // 控制节奏：让 TUI 停留在流式状态
            mockllm.Finish("tool_calls"),
            mockllm.Usage(120, 30),
            mockllm.Done(),
        ),
    },
    // 第 2 个请求：要求上一轮工具结果已注入 messages
    mockllm.Step{
        Require: mockllm.HasToolResult("call_1", "README.md"),
        Reply: mockllm.Stream(
            mockllm.Text("目录里有 README.md。"),
            mockllm.Finish("stop"),
            mockllm.Usage(200, 20),
            mockllm.Done(),
        ),
    },
)
```

> 上例 `Require`/`Expect` 里的 `README.md` 依赖工作目录已播种该文件（见 §七 `harness.WithSeedFiles`），否则 `ls` 输出为空、场景必现失败。

**Step 语义**：

- `Require` 是可选前置条件，按请求顺序逐个消费脚本；不满足 → 测试失败并 dump 该请求（防 agent loop 回归：脚本耗尽时同样 fail）
- `Reply` 支持：`Stream(...)`（SSE）、`JSON(...)`（非流式）、`StatusError(code, msg)`（重试测试用 429，须以**流建立前**的非 SSE HTTP 错误返回）、`MalformedSSE()`、`Hold()`（挂起流，测超时/取消；实现须绑定 `r.Context().Done()`，请求取消即结束，避免 handler goroutine 泄漏）
- 断言 API：`mock.Requests()` 返回全部请求体，供 `It` 内用 Gomega 事后断言（如"第二次请求的 messages 里 tool role 消息包含 ls 输出"）

### 3.4 可复现的请求结构

请求记录保留：完整 `messages`、`tools`、HTTP header（`x-tachi-session-id` 可验证 session 传递）。为防内存膨胀，超过阈值的 tool result 截断记录并标注——**截断只作用于持久化的请求记录**；Step 的 `Require` 始终在完整原始请求体上求值，超大工具结果不会让本应通过的断言失败。
### 3.5 契约测试（锁死线格式）

线格式最大的维护风险是"mock 的输出与客户端期望**同时漂移**"——只用 mock 自己的编码器验证自己发现不了（自己写格式自己读，双漂移照样绿）。契约测试把**真实 SDK 客户端**指向 mock，让第三方解析器当场裁决：

```go
// mockllm 包内测试（无 build tag，随 go test ./... 常跑）
mock := NewServer(ProtocolOpenAI) // 或 ProtocolAnthropic
provider := llm.NewOpenAIProvider("test-key", mock.URL(), "mock-model") // 或 NewAnthropicProvider
ch, err := provider.CreateChatStream(ctx, messages, tools, opts)

// 断言 StreamEvent 序列与脚本语义一致：
// ThinkingDelta → TextDelta → ToolUseStart → InputJSONDelta → MessageDelta(usage) → Done
```

两种协议各一组：mock 服务器喂入脚本化 SSE，用真实 Provider 解析，逐事件断言。线格式一旦与客户端期望漂移，桩层自测阶段即暴露，无需跑到 tmux 场景层。

---

## 四、`-p` 管道模式驱动（`itest/run`）

**M0 最先实现**：`-p` 模式不需要 tmux/TTY，`exec.Command` 起真实二进制 + 管道即可跑通——它是"agent loop → mock LLM API"的**最短链路**。同时 `-p` 是 `--allowed-tools` **唯一真实生效**的模式（见 §6.1），隔离有双保险。本层通了再上 tmux 渲染层。

### 4.1 驱动原语

```go
out, err := exec.Command(bin, "--home", home,
    "-p", "看一下当前目录",
    "-o", "json-stream",        // NDJSON AgentEvent 序列 = 最精确的断言面
    "--allowed-tools", "Bash",  // 仅 -p 模式真实生效（TUI 不接线，见 §6.1）
).CombinedOutput()
```

- **stdout 断言面**：`-o json-stream` 每行一个 AgentEvent JSON（`text_delta`/`tool_call`/`tool_result`/`turn_complete`/`usage`）；`-o text` 是流式文本累积；`-o json` 是单对象（`exit_reason`/`iterations`/`response`/`usage`）
- **exit code 断言**：`exitCodeForReason`（`main.go`）——`stop`=0、budget/length=2、interrupted=130、error=1，错误路径可直接断言退出码
- **stderr 进度默认安静**：`resolveQuiet` 在 stdout 非终端时自动 `-q`，进度类断言不依赖 stderr，以 json-stream 事件序列为准
- 种子文件/隔离环境由 harness 提供（见 §6.2），`-p` 场景同样可配 `permissions.bash`

### 4.2 场景示例

```go
It("工具调用往返", func() {
    mock := mockllm.NewServer()                 // 脚本同 §三，协议无关
    mock.Script(
        mockllm.Step{Reply: mockllm.Stream(
            mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
            mockllm.Finish("tool_calls"), mockllm.Usage(120, 30), mockllm.Done(),
        )},
        mockllm.Step{Require: mockllm.HasToolResult("call_1", "README.md"),
            Reply: mockllm.Stream(
                mockllm.Text("目录里有 README.md。"),
                mockllm.Finish("stop"), mockllm.Usage(200, 20), mockllm.Done(),
            )},
    )
    home := harness.NewHome(t, mock, harness.WithSeedFiles(
        map[string]string{"README.md": "# Fixture\n"}))

    out := run.Binary(bin, home, "-p", "看一下当前目录", "-o", "json-stream")

    events := run.ParseNDJSON(out)
    Expect(events).To(ContainElements(
        HaveToolCall("Bash"), HaveToolResult("call_1"), HaveTurnComplete("stop"),
    ))
    Expect(mock.Requests()).To(HaveLen(2)) // agent loop 第二轮回传了 tool result
})
```

### 4.3 与 TUI/ACP 的关系

同一 mockllm 脚本 + harness 隔离环境，差异只在 driver 与断言面。`-p` 层没有渲染层，不覆盖 TUI 状态机与交互——它的价值是**最早、最快、最稳定**地验证 agent loop 行为（迭代、工具结果回传、重试、输出管线），为 tmux 场景铺路。

---

## 五、tmux 驱动层（`itest/tmux`）

### 5.1 会话管理

```go
s := tmux.Launch(tmux.Config{
    Binary:        bin,               // BeforeSuite 里 go build 一次
    Home:          homeDir,           // --home，隔离一切状态
    WorkDir:       workDir,           // 临时工作目录
    Env:           []string{"TERM=xterm-256color"},
    Size:          "120x40",
})
```

底层：每个 spec 用**独立 socket**（`tmux -L <唯一名>`）起独立 server，`new-session -d -s it -x 120 -y 40 -c <workdir>`，再 `send-keys 'exec <bin> --home <home> ...' Enter`。`AfterEach` 里 `tmux -L <sock> kill-server` 清场，绝不与用户 tmux 会话冲突。

### 5.2 驱动原语

| 方法 | 底层 | 用途 |
|------|------|------|
| `Type(text)` | `send-keys -l` | 字面输入（不解析快捷键） |
| `Key("C-c")` / `Key("Ctrl+o")` | `send-keys` | 快捷键 |
| `Capture()` | `capture-pane -p`（纯文本） | 读全屏文本 |
| `Expect(text, timeout)` | 轮询 `Capture()` | 等待子串出现 |
| `Eventually()` | Gomega 的 `Eventually` 包一层 | 场景层断言 |
| `WaitIdle()` | 轮询状态栏整段左半区（`● tachi | openai (mock)`）且不含 `⏳`；主锚点：mock 请求序列 | 启动就绪 / 回合结束等待 |

### 5.3 断言依据（屏幕可见信号）

从 `tui/statusbar.go`、`tui/chatview.go` 反推的可稳定断言的渲染物：

| 信号 | 屏幕形态 | 断言含义 |
|------|---------|---------|
| 空闲态 | 状态栏左半区 `● tachi | openai (mock)` | 启动完成 / 回合结束（弱信号，见下） |
| 流式/等待态 | 状态栏 spinner（`⠋⠙...`） | 正在请求 LLM |
| 工具运行中 | `~ Bash(cmd)` | 工具已开始执行 |
| 工具成功 | `v Bash(cmd)` + 结果摘要 | 工具已返回 |
| 工具失败 | `x Bash(cmd)` + 错误摘要 | 工具出错 |
| 待发送队列 | `⏳ N pending` | pendingQueue 非空 |
| 思考视图 | `Ctrl+o` 切换 | thinking 渲染 |

> 只用子串断言，不用整屏快照（CJK 宽度、终渲细节在不同环境下有差异）。`●` 会被 idle/选择器/确认框/表单五种状态共用（`statusbar.go:83-91`），单独的 `●` 不能证明"回合已结束"——状态推断以 **mock 收到的请求序列为主锚点**（脚本消费完 / 收到预期请求数），屏幕信号只作辅助。

---

## 六、隔离策略

| 维度 | 手段 |
|------|------|
| 状态文件 | `--home <t.TempDir()>`：config、session、日志、memory、skills、输入历史全隔离 |
| 工作目录 | `t.TempDir()`：无 `.tachi.md`/`.tachi/`，git 状态可选 `git init` 控制；场景可经 `harness.WithSeedFiles(...)` 播种已知文件（如 `README.md`），让工具输出可断言 |
| 工具副作用 | 工具调用**完全由 mock 脚本决定**（LLM 是假的，未脚本化的调用不会发生），限制只是纵深防御。TUI 模式下 `--allowed-tools` **不生效**（仅 `-p` 接线，见 §6.1）；Bash 走全局 config `permissions.bash`（TUI 模式生效）：默认场景不调 Bash，需要时用只读命令并配 `permissions: {bash: {allow: ["*"]}}`（不配则默认可能 ask，弹确认框干扰流程） |
| 网络 | mock 绑 127.0.0.1 随机端口；config fixture 不配 MCP server、不配 web_search key、不配 channel → 零外联 |
| 额外 LLM 调用 | `title_generation: false`（`generateTitle` 会多发一次非流式请求，见 3.1） |
| 环境 | `TERM=xterm-256color`；不依赖用户 `$HOME`/`$PATH` 状态（二进制路径显式传入） |
| 并行 | 每个 spec 独立 tmux socket + 独立 mock 端口 + 独立 home → Ginkgo `-p` 可并行 |


#### 6.1 `--allowed-tools` 的作用域（现状）

`--allowed-tools` / `--disallowed-tools` 目前**只在 `-p`/管道模式接线**（`main.go` 的 `runAgent` 调用 `applyToolRestrictions`）；`runTUI` 不解析这两个 flag。集成测试因此不能靠它限制 TUI 的工具集，替代方案：

1. **mock 脚本即门禁** — 工具调用只可能来自 mock 返回的 `tool_calls`，agent loop 没有自主调工具的路径，未脚本化的工具永远不会被执行
2. **`permissions.bash` 配置** — TUI 模式（`PermissionModeTUI`）下 Bash 的 deny/ask/allow 规则生效，作为 Bash 相关的纵深防御

可选增强（**首期不做**，保持零生产改动）：给 `runTUI` 也接 `applyToolRestrictions`，让交互模式支持工具白名单——这是真实功能（"只读会话"），若未来需要可单独提；集成测试不依赖它。

**测试配置 fixture**（harness 生成）：

```yaml
provider: mock
providers:
  - name: mock
    type: openai
    # Anthropic 协议场景改这里：type: anthropic，base_url 指向同一 mock 的 /v1（见 §3.2）
    model: mock-model
    base_url: http://127.0.0.1:<mock端口>/v1
    api_key: test-key
    context_window: 128000
title_generation: false
language: zh
# mcp / web_search / channel / dream 均不配置
```

#### 6.2 配置文件处理

- **必须由 harness 写入**：`config.LoadFrom` 在配置文件缺失时返回 `DefaultConfig()`（`config/config.go:887-894`）——tachi 没有配置文件也能启动，但 provider 会落到默认占位配置（deepseek + 占位 key），必然连不上 mock。harness 在 `--home` 下生成最小 `config.yaml`（即上面的 fixture）：
  - `provider: mock` 引用 `providers[].name`（`config.Resolve` 按名查找，`resolve.go`），二者必须匹配
  - `api_key` 必填：`ResolveProviderConfig` 对空 key 直接报错（`resolve.go`），fixture 里 `test-key` 不可省；mock 不校验鉴权，`OPENAI_API_KEY` 等环境变量是否残留均无影响
  - `base_url` 含 mock 的**随机端口**（httptest 启动后才可知）→ fixture 在 mock 启动后动态渲染，不用固定端口
  - 场景级变体由 harness 选项控制：`WithProtocol(anthropic)`（`type: anthropic`）、`WithBashAllow("*")`（`permissions.bash.allow`）；`title_generation: false` 恒设
- **不读不写用户真实配置**：`--home` 把 `~/.tachi/config.yaml` 完全隔离在测试之外；配置解析/默认值/别名逻辑属单测覆盖范围（`config/` 已有），集成测试只把它当作场景输入，不重复测试配置层

---

## 七、场景编排（Ginkgo + Gomega）

整个 `itest/` 带 `//go:build integration`。场景即文档，示例：

```go
//go:build integration

package tui_test

var _ = Describe("工具调用流程", func() {
    It("工具调用显示、结果回传、最终回答全链路", func() {
        mock := mockllm.NewServer()
        mock.Script(
            mockllm.Step{Reply: mockllm.Stream(
                mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
                mockllm.Pause(300*time.Millisecond),
                mockllm.Finish("tool_calls"), mockllm.Usage(120, 30), mockllm.Done(),
            )},
            mockllm.Step{Require: mockllm.HasToolResult("call_1", "README.md"),
                Reply: mockllm.Stream(
                    mockllm.Text("目录里有 README.md。"),
                    mockllm.Finish("stop"), mockllm.Usage(200, 20), mockllm.Done(),
                )},
        )
        s := harness.Launch(home, mock,
            harness.WithBashAllow("*"),   // Bash 场景：TUI 默认可能 ask（见 §6.1）
            harness.WithSeedFiles(map[string]string{ // 播种已知文件，让 ls 输出可断言
                "README.md": "# Fixture\n",
            }),
        )

        s.Type("看一下当前目录").Enter()

        s.Expect("~ Bash")                       // 工具运行中
        s.Expect("v Bash")                       // 工具完成
        s.Expect("README.md")                    // 最终回答

        // agent loop 确实把工具结果传回了第二次请求
        Expect(mock.Requests()).To(HaveLen(2))
        Expect(mock.Requests()[1].Messages).To(ContainElement(HaveToolResult("call_1", "README.md")))
    })
})
```

配套设施：

- **`BeforeSuite`**：`exec.LookPath("tmux")` 失败则 `Skip`；`go build -o <tmp>/bin/tachi .` 一次
- **`JustAfterEach`**：失败时自动 dump 最后一次 `Capture()` 全屏 + mock 请求摘要（调试体验 = 单测级别）
- **`DescribeTable`**：参数化场景（如"多种 finish_reason"、"错误码矩阵"）
- 全局安全：每个 spec 设超时（Ginkgo `SpecTimeout`，如 60s），防挂死；`DeferCleanup` 里无论成败都 `mock.Close()`（`Hold()` 挂起的流随请求上下文取消而结束，见 §3.3）

---

## 八、ACP 复用

同一 mock 服务器，换 driver。`tachi acp` 走 stdio JSON-RPC，acp-go-sdk 自带 **`ClientSideConnection`**（SDK 内有 `example_client_test.go` 现成用法），无需手写帧协议：

```go
cmd := exec.Command(bin, "acp", "--home", home)
stdin, _ := cmd.StdinPipe()
stdout, _ := cmd.StdoutPipe()
conn := acp.NewClientSideConnection(testClient, stdin, stdout)

// initialize → new_session → prompt
// 断言 SessionUpdate 事件流里的 text/tool_call/turn_complete
```

| 层 | TUI | ACP |
|----|-----|-----|
| 桩层 | `mockllm` 同一份 | `mockllm` 同一份（脚本完全可复用） |
| 驱动层 | tmux send-keys/capture | `ClientSideConnection` 收发 JSON-RPC |
| 场景层 | 屏幕文本断言 | 事件流断言（`SessionUpdate` 类型、累积文本） |
| 隔离 | `--home` + mock 脚本门禁（Bash 场景配 `permissions.bash`） | 同左。注：`--allowed-tools` 目前仅 `-p`/管道模式生效，TUI 与 ACP 都未接线（见 §6.1）；ACP 工具白名单属后续功能点 |

共享的部分是 **mockllm 脚本**（同一场景的 agent-loop 行为），差异只在断言面：TUI 断言渲染结果，ACP 断言事件流。首期 ACP 只做基础三件套（文本流、工具调用往返、slash command `SessionUpdate`），不复刻全部 TUI 交互场景。

---

## 九、目录结构与 CI

```
itest/
├── mockllm/                 # mock 服务器 + chunk 构造器 + 脚本模型（含自身单测）
│   ├── server.go            #   httptest.Server, /v1/chat/completions 路由
│   ├── chunk.go             #   SSE chunk 构造器（Thinking/Text/ToolCallStart/...）
│   ├── scenario.go          #   Step/Require/Reply 模型 + 请求记录
│   ├── contract_test.go    # 契约测试：真实 SDK 客户端解析 mock 输出（§3.5）
│   └── chunk_test.go
├── run/                     # `-p` 管道模式场景（M0 最先实现，无 tmux 依赖）
│   ├── run_suite_test.go
│   └── message_flow_test.go / tool_call_test.go / ...
├── tmux/                    # tmux 驱动（session 管理 + Type/Expect/Capture）
│   └── session.go
├── harness/                 # 隔离环境构造：config fixture + home/workdir + 二进制
│   └── harness.go
├── tui/                     # TUI 场景（Ginkgo suite, build tag integration）
│   ├── tui_suite_test.go
│   └── message_flow_test.go / tool_call_test.go / ...
└── acp/                     # ACP 场景
    ├── acp_suite_test.go
    └── ...
```

Makefile + CI：

```make
itest:     # go test -tags=integration ./itest/...
itest-run: # go test -tags=integration ./itest/run   （M0，最先实现）
itest-tui: # go test -tags=integration ./itest/tui
itest-acp: # go test -tags=integration ./itest/acp
```

CI 新增独立 job（常规 `test` job 不动）。run 套件**无 tmux 依赖**：本地缺 tmux 时 tui/acp 套件 `BeforeSuite` 自动 Skip，`make itest-run` 仍可全量跑：

```yaml
integration:
  runs-on: ubuntu-latest
  steps:
    - checkout / setup-go 1.26
    - run: sudo apt-get update && sudo apt-get install -y ripgrep tmux
    - run: go test -tags=integration ./itest/...
```

新增依赖：`github.com/onsi/ginkgo/v2`、`github.com/onsi/gomega`（仅 itest 使用；现有 testify 单测不受影响）。

---

## 十、场景清单

**M0 — `-p` 链路（最先实现，不依赖 tmux）**

1. **基础消息流**：`-o json-stream` 断言 `text_delta` 累积 + `turn_complete(stop)` + exit 0
2. **工具调用循环**：断言 `tool_call` → `tool_result` → 下一轮 `text_delta` 事件序列；mock 第二轮请求含 tool result
3. **错误路径 / 重试**：mock 先 429 两次再成功（OpenAI `RetryProvider`；Anthropic 为 SDK 内部重试，机理差异见 M1 场景 3 注）
4. **`--allowed-tools` 生效**：mock 返回被白名单过滤的工具调用 → agent 不执行该工具，链路按预期结束（`-p` 模式该 flag 真实生效）

**M1 — TUI 骨架（4 个场景）**

1. **基础消息流**：thinking → text → stop；断言屏幕出现思考与回答、状态栏回到空闲左半区（`● tachi | openai (mock)`）且无 `⏳`、session 文件生成、mock 收到带 thinking 的 assistant 消息（回合结束以 mock 脚本消费完为主锚点，见 §5.3）
2. **工具调用循环**：tool_call → tool result → 下一轮 text；断言 `~ Bash` → `v Bash` → 回答，第二轮请求含 tool result（如上示例）
3. **错误路径**：mock 先 429 两次再成功。注意重试机理因协议而异——OpenAI 侧由 `RetryProvider{MaxRetries:2}` 包裹（`llm/provider.go:209-214`），Anthropic 侧是 anthropic-sdk-go 内部重试（默认 MaxRetries=2），同一脚本双协议复跑时验证的是各自的重试路径；429 须以**流建立前**的非 SSE HTTP 错误返回。另一例：流中断 → TUI 错误展示不挂死
4. **Anthropic 协议消息流**：同一份脚本用 `mockllm.NewServer(mockllm.ProtocolAnthropic)` + `type: anthropic` provider 重跑场景 1，验证双协议渲染一致性（思考块含 signature）

**M2 — TUI 交互覆盖**

1. 流式期间输入 → `⏳ 1 pending` → 回合结束自动发送（mock 收到第二条用户消息）
2. `Ctrl+C` 取消 → interrupted 语义
3. `AskUserQuestion` → TUI 表单 → 提交 → agent 继续
4. `/model` 切换（`stateSelectingModel`）
5. `/sessions`（`stateSelectingSession`）
6. 会话持久化 + `--resume`
7. 多工具并行调用（一个响应两个 tool_call）

**M3 — ACP 复用**

1. initialize → new_session → prompt 文本流
2. prompt 工具调用往返
3. slash command 的 `SessionUpdate`

---

## 十一、风险与对策

| 风险 | 对策 |
|------|------|
| 终端渲染时序不确定 | 只用 `Eventually` + 稳定子串断言，不用整屏快照；mock 用 `Pause` 控制节奏锚点 |
| CJK 宽度 / 终端差异导致布局漂移 | 断言选 ASCII 或稳定子串；`Size: 120x40` 固定 |
| 每次跑要 build + 起 tmux，慢 | `BeforeSuite` 一次 build；Ginkgo `-p` 并行（隔离已保证）；场景共享 harness |
| 本地无 tmux | `BeforeSuite` 检测后 `Skip`，`make itest` 文档注明依赖 |
| 测试工具真实副作用 | 白名单 + 临时目录 + 零外联配置；Bash 场景只允许只读命令 |
| mock 脚本与实际调用数不符（agent loop 失控） | 脚本耗尽即 fail + dump 请求体 |
| 失败时无从调试 | `JustAfterEach` 自动 dump 屏幕 + 请求摘要 + 日志尾部 |
| 双协议线格式维护成本 | 协议只是渲染层：场景脚本协议无关；mockllm 的契约测试用**真实 SDK 客户端**解析 mock 输出（§3.5），线格式漂移在桩层自测阶段被第三方解析器当场抓住 |

---

## 十二、里程碑

0. **M0**：`mockllm`（含 §3.5 契约测试）+ `harness` + `itest/run` 4 个 `-p` 场景 + CI job——先打通"agent loop → mock LLM"链路，TUI/ACP 之前必须通过
1. **M1**：`tmux` + TUI 骨架 4 个场景（复用 M0 的 mockllm/harness）——证明黑盒链路 + 渲染层可行性
2. **M2**：补齐 TUI 交互场景（pending/cancel/form/model/sessions/resume）——覆盖核心交互面
3. **M3**：ACP suite 复用 mockllm——验证跨前端复用价值
4. **打磨**：并行稳定化、失败 dump 体验、场景文档化（Ginkgo 输出即文档）
