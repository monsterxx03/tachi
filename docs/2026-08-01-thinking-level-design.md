# 模型思考级别 (Thinking Level) 配置 + ModelSpec 重构设计文档

> 日期: 2026-08-01 | 状态: 已实现

## 一、动机

1. **thinking level 配置缺失**：`ChatOptions.Thinking / ThinkingEffort` 早已存在，但没有配置入口——用户无法在 `config.yaml` 中控制模型的思考强度。DeepSeek 的思考模式（thinking mode）通过顶层 `thinking: {type}` + `reasoning_effort` 控制，且**不同模型支持不同级别**（`deepseek-v4-flash` 不支持 `max`，`deepseek-v4-pro` 支持 `max`），需要内置 per-model 支持范围表。

2. **model 配置分散**：`ProviderConfig` 把 `context_window` 和四个价格字段平铺在 struct 上，模型级属性没有聚合，添加新属性（如 thinking）会让结构继续膨胀。

## 二、设计

### 2.1 ModelSpec —— 模型级属性聚合（config 层）

新增 `config.ModelSpec` / `config.ModelPricing`，通过 `ProviderConfig.Spec` 正式嵌套（**不向前兼容**旧版平铺字段）：

```go
type ProviderConfig struct {
    Name    string `yaml:"name"`
    Type    string `yaml:"type"`
    Model   string `yaml:"model"`
    BaseURL string `yaml:"base_url"`
    APIKey  string `yaml:"api_key"`
    Spec    ModelSpec `yaml:"spec"`
}

type ModelSpec struct {
    ContextWindow *int64       `yaml:"context_window,omitempty"`
    ThinkingLevel string       `yaml:"thinking_level,omitempty"` // none | low | medium | high | xhigh | max
    Pricing       *ModelPricing `yaml:"pricing,omitempty"`
}

type ModelPricing struct {
    InputPrice              *float64 `yaml:"input_price,omitempty"`
    OutputPrice             *float64 `yaml:"output_price,omitempty"`
    CacheReadInputPrice     *float64 `yaml:"cache_read_input_price,omitempty"`
    CacheCreationInputPrice *float64 `yaml:"cache_creation_input_price,omitempty"`
}
```

- **不向前兼容**：旧版平铺字段（`context_window:` / `input_price:` 直接挂 provider 下）不再解析，旧配置需迁移为 `spec:` 嵌套。
- `Pricing` 用指针 + `omitempty`，未配置时模板输出干净的 `spec: {}`，不产生 `null` 噪音。
- 访问点（`config/resolve.go`、`agent/commands/pricing.go`）已相应更新，`Spec.Pricing` 判空后取覆盖值。

### 2.2 ThinkingLevel 解析（config/resolve.go）

`ResolveProviderConfig` 把 `ThinkingLevel` 解析为 `ResolvedProvider.Thinking / ThinkingEffort`：

| ThinkingLevel | Thinking | ThinkingEffort |
| ------------- | -------- | -------------- |
| 空            | nil（模型默认） | ""（模型默认） |
| `none`        | `false`（显式关闭） | "" |
| 其他          | nil | 原样透传（`low`/`medium`/`high`/`xhigh`/`max`） |

**不做客户端归一化**：DeepSeek 官方 [thinking_mode 文档](https://api-docs.deepseek.com/zh-cn/guides/thinking_mode) 的 effort 映射表显示，两个 v4 模型都**接受**全部五个级别作为请求输入，由服务端映射到实际推理 effort（如 flash 的 `medium`→`high`、`xhigh`→`high`、`max`→`max`；pro 的 `xhigh`→`max`）。客户端硬编码映射表既与服务器语义重复，又会随服务端调整（文档注 3：pro 的映射 2026-08 月初更新）而失真 —— 比如把用户请求的 `max` 降级为 `high` 反而浪费了模型能力。因此 effort 一律透传，映射责任完全交给 API。

（曾短暂实现过 `llm.NormalizeThinkingEffort` 客户端降级，基于错误的"flash 不支持 max"理解，已删除。）

### 2.3 Provider 层传输

**OpenAI provider（`deepseek-v4-*` 走 OpenAI 端点时）**：
- 依赖 fork 版 go-openai（`monsterxx03/go-openai` tag `v1.41.2-extrabody`，go.mod `replace`）：`ChatCompletionRequest.ExtraBody map[string]any` 在客户端内部 merge 到请求体根节点（等价 Python SDK 的 `extra_body`）。非流式 `CreateChatCompletion` 与流式 `CreateChatCompletionStream` 都支持。
- **默认（未配置 thinking_level）不注入任何 thinking 字段**——请求交给服务端默认策略（DeepSeek: thinking 开启、effort high），客户端不覆盖。
- 显式配置时才发送：
  - `thinking_level: none` → `{"thinking": {"type": "disabled"}}`
  - `thinking_level: <effort>` → `{"thinking": {"type": "enabled"}}` + `reasoning_effort: <effort>`
- 此前 go-openai 官方版无 `ExtraBody`，曾用手工 HTTP + SSE 解析 hack 注入顶层字段；`PR #1069` 合并上游后可删 replace 回归官方版。

**Anthropic provider（`deepseek-v4-*` 走 `/anthropic` 端点时）**：已有 `thinking: adaptive/disabled` + `output_config.effort` 支持，无需改动。默认（未配置）发送 `thinking: adaptive`，effort 由 `effortFromString` 兜底为 `high`。

### 2.5 Agent 级默认值（agent/agent_loop.go）

`AgentConfig` 新增 `Thinking *bool` / `ThinkingEffort string`（构造输入），`runLoop` 在 `ChatOptions` 未显式指定时填充：

```go
if opts.Thinking == nil { opts.Thinking = a.Config.Thinking }
if opts.ThinkingEffort == "" { opts.ThinkingEffort = a.Config.ThinkingEffort }
```

- 所有前端（TUI / channel / ACP / one-off）自动继承模型级思考配置，无需每个调用点改动
- 显式覆盖优先（`/commit` 禁用、`/review` 自定义、subagent thinking 配置均不受影响）
- `/model` 切换模型时同步 `SetThinking`（`tui/provider_selector.go` / `session_selector.go` / `agent/acp/model_config.go` / `main.go` run 分支）
- 已知限制：`thinking_level: none` 对 o1/o3/o4/gpt-5 等强制推理模型静默无效（resolve 时打 warning 提示）

### 2.6 顺带修复

非流式 `CreateChat` 对 o1/o3/o4/gpt-5 模型用 `MaxTokens` 会触发 go-openai `ReasoningValidator` 报错（这些模型必须用 `max_completion_tokens`）。新增 `isReasoningModelPrefix` 按模型族选择字段。此前因无配置入口（无人设置 effort）未暴露，属潜在 bug。

### 2.7 启动期 /thinking（无活跃 session）

TUI 刚启动时没有 session，此前 `/thinking` 会直接拒绝。现在引入 **pending per-session override**：

- `AgentConfig.PendingSessionThinking`（`agent/agent_config.go`）+ `SetPendingSessionThinking` / `PendingSessionThinking`（`agent/agent.go`）记录启动期待定 override。
- `handleThinkingCommand`（`tui/commands.go`）无 session 时不再拒绝：校验级别后用当前 provider 解析，`SetPendingSessionThinking` + `SetThinking`（立即生效），并提示"将在创建首个会话时生效"。
- `ensureSessionAndRecordUser`（`agent/agent_loop.go`）首次创建 session 时，把 pending override 写入新 session 的 `ThinkingLevel` 并持久化到 meta.json，然后清空 pending。
- `currentThinkingLevel`（`tui/model.go`）无 session 时优先反映 pending，让状态栏 badge 正确显示。

语义：pending 只作用于**即将创建的首个 session**（与 `/thinking` 的 per-session 定位一致）；`/new` 或重启后的新会话不继承，`/thinking default` 可清除 pending。

## 三、配置示例

```yaml
providers:
  - name: deepseek-v4-flash
    type: openai
    model: deepseek-v4-flash
    base_url: https://api.deepseek.com/v1
    api_key: sk-xxx
    spec:
      context_window: 1000000
      thinking_level: high   # none=关闭; low/medium/high/xhigh/max 原样透传，服务端映射
      pricing:
        input_price: 1.0
        output_price: 2.0
        cache_read_input_price: 0.02

  - name: deepseek-v4-pro
    type: anthropic
    model: deepseek-v4-pro
    base_url: https://api.deepseek.com/anthropic
    api_key: sk-xxx
    spec:
      thinking_level: max    # pro 支持 max
```

## 四、测试覆盖

- `config/config_test.go`：`TestModelSpec_YAMLNested`（嵌套 YAML 解析 + 旧平铺字段不再解析 + round-trip）、`TestResolveProviderConfig_ThinkingLevel`（none/high/max 原样透传）
- `agent/commands/thinking_test.go`：`ThinkingOverrideFromLevel` / `EffectiveThinking`（none 关闭、其余 effort 透传）
- `llm/openai_test.go`：httptest 验证非流式/流式请求体的顶层 `thinking` 字段 + `reasoning_effort`；非 deepseek 模型不注入顶层字段
