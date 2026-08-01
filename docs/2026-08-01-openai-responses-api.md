# OpenAI Responses API 支持设计文档

> 日期: 2026-08-01 | 状态: 已实现 | 范围: 仅客户端维护历史消息（stateless）模式
>
> **实现备注**：`openai-res` provider 已落地（`llm/openai_responses.go`，基于 openai-go v3.49.0），全部测试通过。与本文档的差异：provider type 定为 `openai-res`（常量 `ProviderTypeOpenAIResponses`）；`openai.Client` 为值类型（非指针）；`response.completed` 事件到达后立即终止流读取（SDK 已保证它是最后一个数据事件）。

## 一、动机

1. **go-openai 不支持 Responses API**：当前 `llm/openai.go` 基于 `sashabaranov/go-openai`（含 `monsterxx03` ExtraBody fork，v1.41.2，2025-09 为最新版）走 `/chat/completions` 端点。该库及其 fork 均无 `responses` 端点实现（无 `responses.go`，release notes 亦无此计划）。
   **但有官方替代**：`openai/openai-go`（v3，2026-07-31 v3.49.0）原生支持 Responses API，且官方将其定位为"与 OpenAI 模型交互的主 API"。

2. **新模型官方推荐路径是 Responses API**：gpt-5 系列及后续模型的官方首选接入方式是 `POST /responses`，chat completions 对新特性（reasoning item、prompt cache retention、hosted tools 等）支持是兼容层。tachi 目前无法使用这些模型的 Responses 端到端能力。

3. **消息模型不同**：Responses API 用**平铺的 input item 数组**（message / function_call / function_call_output 混排），与 chat completions 的"assistant 消息内嵌 tool_calls + 独立 tool role"结构不同，转换逻辑无法复用。

### 本次范围

- 新增 provider type **`openai-res`**，实现现有 `llm.Provider` 接口
- **只支持客户端维护历史消息模式**：每次请求携带完整 input（全部历史平铺），服务端无状态；**不做** `previous_response_id` 状态链 / `store: true` 服务端会话
- 接入点最小化：agent 层、TUI、stream consumer 均无感知，只动 `llm` 包 + config 少量映射

## 二、设计

### 2.1 总览

新增 `llm/openai_responses.go`，基于 **OpenAI 官方 Go SDK `github.com/openai/openai-go/v3`** 实现 `Provider` 接口。SDK 负责协议层（请求序列化、SSE 解析、类型定义），本文件只承担**转换 + 映射**：

```go
type OpenAIResponsesProvider struct {
    client *openai.Client   // github.com/openai/openai-go/v3
    model  string
    apiKey string
}

func NewOpenAIResponsesProvider(apiKey, baseURL, model string) *OpenAIResponsesProvider {
    client := openai.NewClient(
        option.WithAPIKey(apiKey),
        option.WithBaseURL(baseURL),                    // 含 /v1，如 https://api.openai.com/v1
        option.WithHTTPClient(&http.Client{
            Transport: &tachiTransport{base: http.DefaultTransport}, // 注入 User-Agent + x-tachi-session-id
        }),
    )
    return &OpenAIResponsesProvider{client: client, model: model, apiKey: apiKey}
}
```

为什么不用手写 net/http：
- SDK 是官方维护的 stainless 生成库，随 API 演进，SSE 解析/类型/错误处理都有人维护
- `WithBaseURL` + `WithHTTPClient` 完美适配 tachi 现有基建（自定义 base_url、`tachiTransport` 注入 session header、RetryProvider 包装）
- 与现有 `sashabaranov/go-openai` 包路径不同，**两个 OpenAI SDK 可共存**：老路径继续走 chat completions（含 DeepSeek ExtraBody fork），新 provider 走官方 SDK，互不干扰

配置示例（与现有 openai provider 平级）：

```yaml
providers:
  - name: gpt5
    type: openai-res
    model: gpt-5.6
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
    spec:
      context_window: 400000
      thinking_level: high
```

### 2.2 请求协议（stateless 模式）

`POST {baseURL}/responses`，请求体：

```json
{
  "model": "gpt-5.6",
  "instructions": "<system prompt，来自历史中的 system 消息拼接>",
  "input": [
    {"role": "user", "content": [{"type": "input_text", "text": "你好"}]},
    {"role": "assistant", "content": "你好！"},
    {"type": "function_call", "call_id": "call_1", "name": "Bash", "arguments": "{\"command\":\"ls\"}"},
    {"type": "function_call_output", "call_id": "call_1", "output": "main.go\n"}
  ],
  "tools": [
    {"type": "function", "name": "Bash", "description": "...",
     "parameters": {"type": "object", "properties": {...}, "required": []},
     "strict": true}
  ],
  "reasoning": {"effort": "high"},
  "max_output_tokens": 4096,
  "temperature": 1,
  "parallel_tool_calls": true,
  "stream": true,
  "store": false
}
```

关键点：

- **input 是平铺数组**，不区分"哪条 assistant 消息带了 tool_calls"——`function_call` 和 `function_call_output` 是独立的顶层 item，按历史顺序插入
- **content 简写**：纯文本 content 可以传字符串（`"content": "你好"`），多模态必须用数组 `input_text` / `input_image`。实现上统一用字符串简写（文本）或数组（有图片时），与现有 `convertMessages` 的多模态分支对齐
- **system 消息**：收集全部 system role 消息，按序拼接为 `instructions` 字段（官方推荐），不再出现在 input 中；`RoleSteer` 内部角色归并为 user
- **`store: false`**：显式关闭服务端存储，强化 stateless 语义
- 请求头复用现有 `tachiTransport`（User-Agent + `x-tachi-session-id`），鉴权 `Authorization: Bearer`

### 2.3 消息转换映射

`convertMessages(messages []Message) []map[string]any`（与 chat completions 版并存，不共用）：

| llm.Message | Responses input item | 说明 |
|---|---|---|
| `Role == "user"` | `{"role":"user","content":...}` | 文本用字符串，多模态用 `input_text`/`input_image` 数组 |
| `Role == "assistant"` | `{"role":"assistant","content":...}` | 同上 |
| `Role == "system"` | 并入 `instructions` | 不生成 input item |
| `Role == "steer"` | 归并为 user | 与现有 OpenAI 路径一致 |
| `Role == "assistant"` 且带 `ToolCalls` | message item **后追加**多个 `{"type":"function_call","call_id","name","arguments"}` | `call_id` 缺失时生成 `call_<n>` 占位 |
| `Role == "tool"` + `ToolCallID` | `{"type":"function_call_output","call_id","output"}` | output 为字符串；`IsError` 时加 `\n[error] ` 前缀标记（与 Anthropic 路径的语义对齐） |
| `Role == "tool"` 但 `ToolCallID` 为空 | 跳过并记 warn 日志 | 协议要求 call_id 必填，脏数据不能进请求 |

**ThinkingBlocks 策略 —— 丢弃**：Responses API 官方明确多轮对话时**不应把上一轮的 reasoning 回传**（input 里没有 reasoning 字段；assistant message 只带 output text）。这与 chat completions 路径（`reasoning_content` 拼回 Content 保上下文）不同，属于协议层面的正确行为，不做拼接。`agent/session_convert.go` 的 `providerType == llm.ProviderTypeOpenAI` 分支不会命中 `openai-res`，无需改动。

### 2.4 工具转换

```go
{"type": "function", "name": ..., "description": ...,
 "parameters": {...}, "strict": true|false}
```

- **strict 按 schema 兼容性逐工具开启**：OpenAI strict 模式要求 object 节点的每个属性都必须出现在 `required` 数组里（含嵌套对象）。tachi 工具普遍有可选参数（如 askuser 的 `options`），无条件 `strict: true` 会被 API 以 400 拒绝。实现用 `strictCompatibleSchema` 递归检查：满足约束才开 strict，否则原样发送（`strict` 缺省）——与现有 `NewTool` 的 `required` 空数组归一化配合，行为稳定。

### 2.5 参数映射（ChatOptions → 请求体）

| ChatOptions | 请求体字段 | 说明 |
|---|---|---|
| `MaxTokens` | `max_output_tokens` | Responses 没有 max_tokens / max_completion_tokens 之分，统一用这个 |
| `ThinkingEffort` | `reasoning: {"effort": ...}` | 仅 gpt-5 / o 系列模型有效；值透传（low/medium/high/xhigh/max）。同时请求 `summary: "auto"`，让非流式响应能拿到推理摘要（POST /responses 的 reasoning content 默认加密，不请求摘要会为空） |
| `Thinking == false` | `reasoning: {"effort": "none"}`（推理模型） | Responses API 官方支持 `effort: "none"`，可以真正关闭推理——比 chat completions 只能"省略字段"更进一步；对非推理模型（gpt-4o 等）省略 reasoning，避免服务端拒绝参数 |
| `Thinking == true && Effort == ""` | `reasoning: {"summary": "auto"}` | 显式启用但用模型默认强度：只请求摘要，不覆盖 effort |
| `Thinking == nil && Effort == ""` | 不设置 reasoning | 走服务端默认 |
| `SessionID` | `x-tachi-session-id` header | 复用 `WithSessionID` |

### 2.6 非流式响应映射

`POST /responses`（`stream: false`）返回的 `output` 数组，元素类型按 `type` 分发：

| output item | Response 字段 |
|---|---|
| `{"type":"message","content":[{"type":"output_text","text":...}]}` | 拼接进 `Response.Content`（多个 message item 按序拼接） |
| `{"type":"function_call","call_id","name","arguments"}` | `Response.ToolCalls` 追加 |
| `{"type":"reasoning",...}` | `Response.Reasoning`（若服务端返回了 summary/text，透传；多数情况为空） |
| `{"type":"web_search_call"}` 等 hosted tool item | 忽略（本次不支持 hosted tools，出现即记 debug 日志） |

**finish_reason 推导**（response 对象没有 chat completions 式的 `finish_reason` 字段）：

```go
switch {
case resp.Status == "incomplete" && resp.IncompleteDetails != nil:
    // reason: "max_output_tokens" | "content_filter" | ...
    finish = "length"   // max_output_tokens → length，其余映射到 "stop"
case lastOutputIsFunctionCall:
    finish = "tool_calls"
default:
    finish = "stop"
}
```

**usage 映射**：

| Responses usage | llm.Usage |
|---|---|
| `input_tokens` | `InputTokens` / `LastInputTokens` |
| `output_tokens` | `OutputTokens`（含 reasoning_tokens，OpenAI 计价如此） |
| `input_tokens_details.cached_tokens` | `CacheReadInputTokens` |
| `input_tokens_details.cache_write_tokens` | `CacheCreationInputTokens` |

### 2.7 流式事件映射

`stream: true` 时 SDK 的 `NewStreaming` 返回事件流，`stream.Current()` 返回已解析的事件对象（`responses.EventUnion`，按 `Type` 分发），SSE 解析由 SDK 内部完成。tachi 侧只需做事件 → `StreamEvent` 的映射，事件类型名与 data JSON 的 `type` 字段一致（`response.output_text.delta` 等）：

| SSE 事件 | StreamEvent | 说明 |
|---|---|---|
| `response.output_item.added`（item.type=message） | — | 仅跟踪 item 边界，无需输出 |
| `response.output_item.added`（item.type=function_call） | `StreamEventToolUseStart` | `output_index` 作为 `ToolIndex`（并行调用时按序递增） |
| `response.output_text.delta` | `StreamEventTextDelta` | `delta` 字段 |
| `response.reasoning_text.delta` | `StreamEventThinkingDelta` | 原始推理文本（gpt-5/o 系列流式暴露） |
| `response.reasoning_summary_text.delta` | `StreamEventThinkingDelta` | 推理摘要文本 |
| `response.function_call_arguments.delta` | `StreamEventInputJSONDelta` | `delta` 为参数 JSON 增量，用 `output_index` 关联 ToolIndex |
| `response.completed` | `StreamEventDone` | 事件内嵌完整 response 对象：取 `usage` 和最终 output 推导 finish_reason |
| `response.incomplete` | `StreamEventDone` | 终止事件（如 max_output_tokens 截断）：同样推导 finish_reason → `length`，不丢失截断标记 |
| `response.failed` | `StreamEventError` | 取出 response.error |
| SSE `error:` 行 / 非 2xx | `StreamEventError` | 请求级错误 |

**streaming 状态下 function_call 的组装**（与 chat completions 的 delta 拼装不同）：

1. `output_item.added`（function_call）→ 先发 `ToolUseStart`（此时 arguments 为空，ToolCall 带 call_id + name）
2. 若干 `function_call_arguments.delta` → 逐个 `InputJSONDelta`（消费端按 ToolIndex 累加，复用现有逻辑）
3. `function_call_arguments.done` / `output_item.done` → 无需额外事件，参数已在 delta 中累计完整

> 注意：OpenAI 的 `output_index` 是**全局输出项索引**，而现有 `StreamEvent.ToolIndex` 语义是"第几个并行工具"。由于输出中 message 和 function_call 交错，直接用 `output_index` 会留下空洞。**实现上维护一个自增计数器**：每遇一个 function_call item 的 `output_item.added` 就分配下一个序号（0,1,2...），后续该 item 的 arguments delta 通过 `item_id` 或 `output_index` 关联回该序号。

### 2.8 重试

**不包 RetryProvider**：官方 openai-go SDK 内部自带重试（默认 `MaxRetries=2`），覆盖 408/409/429/5xx 与连接错误，且尊重 `Retry-After` 头；其错误类型 `apierror.Error` 位于 internal 包，`RetryProvider.isRetryable` 无法识别——外层包装对 429/5xx 实际不生效，只会对连接错误造成 SDK + 外层双重退避（最多 5 次尝试）。故直接返回裸 provider，零冗余。

## 三、代码变更

| 文件 | 变更 | 说明 |
|---|---|---|
| `llm/provider.go` | 修改 | 新增 `ProviderTypeOpenAIResponses = "openai-res"` 常量；`NewProvider` 加 case |
| `llm/openai_responses.go` | **新增** | 约 250 行：Provider 实现 + 消息/工具转换 + 事件映射（协议层由 SDK 承担） |
| `llm/openai_responses_test.go` | **新增** | httptest mock server 单测（见第五节） |
| `go.mod` / `go.sum` | 修改 | 新增 `github.com/openai/openai-go/v3`（v3.49.0+） |
| `config/resolve.go` | 修改 | `EnvForProviderType` 加 case → `OPENAI_API_KEY` |
| `agent/session_convert.go` | 无变更 | thinking 拼接分支只匹配 `openai`，新 type 自然走"不回传 reasoning"路径 |

`NewProvider` 变更示意：

```go
case ProviderTypeOpenAIResponses:
    return NewRetryProvider(
        NewOpenAIResponsesProvider(apiKey, baseURL, model),
        RetryConfig{MaxRetries: 2},
    ), nil
```

## 四、实现步骤

### Step 1：Provider 骨架 + 非流式 CreateChat

- 定义 `OpenAIResponsesProvider` 与 `NewOpenAIResponsesProvider`（`option.WithBaseURL` + `option.WithHTTPClient` 注入 `tachiTransport`）
- `convertMessages` / `convertTools`：llm.Message/Tool → `responses.ResponseNewParamsInputUnion` / `responses.ToolParam`
- `CreateChat`：`client.Responses.New` 非流式 → 遍历 output → 映射 `*Response`（含 finish_reason 推导、usage）

### Step 2：流式 CreateChatStream

- `client.Responses.NewStreaming(ctx, params)` 发起流式请求
- goroutine 内 `for stream.Next()` 循环，`stream.Current()` 按 `Type` 分发到 emit 逻辑
- 工具索引计数器 + arguments delta 关联（见 2.7 注意点）

### Step 3：接线

- `provider.go` 常量 + case
- `config/resolve.go` env 映射
- `go build ./...` + 现有测试全绿

### Step 4：测试

- 单元测试（转换函数、事件映射、finish_reason 推导）
- httptest 集成（非流式 / 流式 / 错误路径）

### Step 5：手工验证

- 配一个 `openai-res` provider 跑一轮真实对话 + 一轮工具调用
- 验证：thinking delta 显示、tool call 组装、usage 计入成本、会话恢复（客户端历史重放）

## 五、测试用例

### 单元测试

| 用例 | 验证点 |
|---|---|
| `TestResponsesConvertMessages` | user/assistant 文本与多模态、system 并入 instructions、steer→user |
| `TestResponsesConvertToolMessages` | tool role → function_call_output，call_id 缺失跳过 |
| `TestResponsesConvertToolCalls` | assistant + ToolCalls → 追加 function_call items |
| `TestResponsesConvertTools` | strict: true、required 空数组非 null |
| `TestResponsesDeriveFinishReason` | completed + function_call → tool_calls；incomplete(max_output_tokens) → length；其余 → stop |
| `TestResponsesUsageMapping` | cached_tokens / cache_write_tokens / reasoning 计入 |
| `TestResponsesReasoningParam` | effort 透传、Thinking=false 不设置 |

### httptest 集成测试

| 用例 | 验证点 |
|---|---|
| 非流式文本响应 | mock server 校验请求体（model/input/tools/store:false），返回 message item，断言 `Response.Content` |
| 非流式工具调用 | mock 返回 function_call item，断言 `ToolCalls` + finish_reason=tool_calls |
| 流式文本 | mock 按序发 `output_item.added` / `output_text.delta`×n / `output_item.done` / `completed`，断言 TextDelta 顺序、Done 的 usage |
| 流式工具调用 | mock 发 function_call added + arguments.delta×n，断言 ToolUseStart 在前的 ToolIndex 序号与 InputJSONDelta 对应 |
| 500 错误 / 非 2xx | 断言 Error 事件（RetryProvider 会先重试，2 次后失败） |
| 上下文取消 | ctx cancel 后流关闭无泄漏 |

### config 测试

- `EnvForProviderType("openai-res") == "OPENAI_API_KEY"`
- provider type 解析/加载正常

## 六、范围外（明确不做）

| 能力 | 原因 |
|---|---|
| `previous_response_id` 服务端状态链 | 用户明确只要客户端维护历史模式 |
| `store: true` / prompt cache retention | 服务端持久化，与 stateless 冲突 |
| hosted tools（web_search / file_search / code_interpreter） | 需要 item 级输出处理与工具执行器集成，另行设计 |
| computer use / MCP items | 协议复杂，无当前需求 |
| 图片输出 / function_call_output 的文件附件 | 无当前需求 |

## 七、风险与权衡

1. **新增依赖**：引入 `openai/openai-go/v3` 与现有 `sashabaranov/go-openai` 两个 OpenAI SDK 并存。包路径隔离、不冲突，但依赖树变大；官方 SDK 是 stainless 生成库，大版本升级（v3→v4）可能有 breaking change，锁版本并留意 release note。若未来 chat completions 路径也想迁移，可另开任务统一。
2. **SDK 行为依赖**：SSE 解析、错误结构由 SDK 封装，边缘行为（如事件乱序、`response.failed` 的 error 形状）以 SDK 类型为准。对策：httptest mock 覆盖正常 + 异常路径，锁定实际行为。
3. **reasoning 不回传**：多轮对话中模型会丢失自己上一轮的思考上下文（chat completions 路径通过 `reasoning_content` 保留）。这是 Responses 协议的正确行为（官方禁止回传），若后续发现质量损失，可加配置开关把 reasoning 以 `input_text` 前缀形式拼进 assistant content（与 chat completions 路径同构）。
4. **finish_reason 为推导值**：`tool_calls` / `length` 语义与 chat completions 对齐，极端情况下（如输出为空但 status=completed）会得到 `stop` + 空 Content，与现有 `openai.go` 的空 choices 兜底行为一致。
5. **第三方兼容端点**：DeepSeek 等厂商若提供 `openai-res` 兼容端点，字段可能缺省（如无 reasoning events、usage details）。设计上所有解析都走"字段存在才映射"的容错路径，缺失即按默认值处理。

## 八、参考

- OpenAI 官方 Go SDK: https://github.com/openai/openai-go（v3.49.0，2026-07-31）
- Responses API 官方参考: https://developers.openai.com/api/reference/resources/responses
- Responses streaming events: https://developers.openai.com/api/reference/resources/responses/streaming-events
- Function calling guide（input item 格式）: https://developers.openai.com/api/docs/guides/function-calling
- go-openai 现状: v1.41.2（含 monsterxx03 fork）无 responses 支持
