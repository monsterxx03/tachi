# Agent Loop 可读性重构方案

日期：2026-07-28
状态：方案已确认，待实施

## 背景

`agent/agent_loop.go` 的 `runAgentLoop` 是主对话循环（TUI）与一次性循环（/commit、sub-agent、dream 等）共用的核心。功能正确，但可读性跟不上复杂度，主要问题：

1. **循环状态散落三处**：局部变量（`apiCallCount`、`lengthContinueRetries`）、指针传递（`&messages`、`&lengthRetries` 穿过 3 层 handler）、agent 字段（`a.turn`、`a.iterationBudget`）
2. **单 handler 混合过多关注点**：`handleToolCallFinish` 一个函数做 7 件事（session 记录、工具执行、LSP 同步、steer 阻塞等待、token 估算、loop reminder 注入、构造下轮消息）
3. **入口点微妙且重复**：`RunConversationStream` / `RunOneOffStream` 各有 ~100 行 setup，逻辑高度相似但 session 写入、memory skip、history 处理上有隐式差异
4. **bool 返回值语义模糊**：`handleFinishReason` 返回 `false` 可能是正常完成也可能是出错，terminal event 归属只能靠脑子记

实际流程骨架是清晰的：

```
for {
    budget? → ctx.Done()? → autoCompact?
      ↓
    CreateChatStream → consumeStream
      ↓
    finishReason?
      ├─ tool_calls → 执行工具 → steer → reminder → 继续
      ├─ length     → 续写(≤3次) → 继续 / 耗尽退出
      └─ stop       → TurnComplete → 退出
}
```

问题在于每个箭头里塞的东西太多。

## 设计原则

1. **行为完全等价**——发给 LLM 的消息序列、session 记录、hook 事件、AgentEvent 协议全部不变（TUI/channel/ACP 都依赖这些）
2. **分阶段提交**，每个 Phase 独立可测、可回滚
3. 现有 `turnState`（`agent/turn_state.go`）职责不变：它是**跨 goroutine 共享、mutex 保护**的回合级状态。循环里的 `messages`/`apiCalls`/`lengthRetries` 是**单 goroutine 的循环局部状态**，不混入 `turnState`，避免模糊其"每个字段都要加锁"的语义

## 目标结构

新增 `loopState`（放 `agent/agent_loop.go` 或新文件 `loop_state.go`）：

```go
// loopState 是 runAgentLoop 单 goroutine 拥有的循环状态。
// 与 turnState 的分工：turnState 跨 goroutine 共享、需要锁；
// loopState 只在循环 goroutine 内使用，无需同步。
type loopState struct {
    messages      []llm.Message     // 进行中的消息切片（原 &messages 指针传递）
    apiCalls      int               // API 调用次数（原 apiCallCount 局部变量）
    lengthRetries int               // length 续写重试计数（原 *int 传递）
    budget        *iterationBudget  // 从 AIAgent 字段下沉为循环所有
}

func (ls *loopState) append(msgs ...llm.Message) { ls.messages = append(ls.messages, msgs...) }
```

分工对比：

| 结构 | 拥有者 | 同步 | 内容 |
|------|--------|------|------|
| `turnState`（已有） | AIAgent，跨 goroutine | mutex | tokens/breakdown/最终快照/trace/pendingImages |
| `loopState`（新增） | runAgentLoop 单 goroutine | 无 | 进行中 messages/apiCalls/lengthRetries/budget |

## 分阶段方案

### Phase 1 — 收拢循环状态（纯机械替换）

- `runAgentLoop` 顶部构造 `ls := &loopState{messages: messages, budget: NewIterationBudget(...)}`
- handler 签名变化：

```go
// 之前
handleToolCallFinish(ctx, acc, messages *[]llm.Message, ch chan<- AgentEvent, _ int, lengthRetries *int) bool
// 之后
handleToolCallFinish(ctx, acc, ls *loopState, ch chan<- AgentEvent) loopOutcome
```

- `defer func() { a.turn.setMessages(ls.messages) }()` 替代现在的闭包捕获
- **收益**：消灭 6 处 `&messages` / `*int` 指针传递；handler 不再反向改 caller 局部变量
- **风险**：低，纯签名变化

### Phase 2 — bool 换成 outcome 枚举

```go
type loopOutcome int

const (
    outcomeContinue loopOutcome = iota // 继续下一轮迭代
    outcomeStop                        // 正常结束（terminal event 已由 handler 发出）
)
```

- `handleFinishReason` 及三个分支返回 `loopOutcome`，loop 体写 `if ... == outcomeStop { return }`
- 不引入第三个 error 值：error 路径也是 handler 发完事件后停循环，与 stop 对 loop 而言无区别
- **收益**：`return true` 这类需要查注释的代码消失
- **风险**：低

### Phase 3 — 拆分 `handleToolCallFinish`

当前 ~100 行拆成 5 个命名步骤，主函数变成编排：

```go
func (a *AIAgent) handleToolCallFinish(ctx, acc, ls, ch) loopOutcome {
    a.recordToolCallTurn(acc, ls)              // session 记录 + usage 事件 + append assistant 消息
    if outcome := a.executeAndAppendTools(ctx, acc, ls, ch); outcome == outcomeStop {
        return outcomeStop                     // 工具执行出错/取消，事件已发
    }
    a.syncLSPAfterTools(ctx, acc)              // LSP 文件同步 + CloseMissingFiles + 等诊断
    if outcome := a.applySteer(ctx, ls, ch); outcome == outcomeStop {
        return outcomeStop                     // ctx.Done
    }
    a.injectLoopReminders(ctx, acc, ls)        // token 估算 + reminder 收集 + append
    return outcomeContinue
}
```

- **收益**：每个子函数 10–25 行，单一关注点；主流程 5 行可读
- **风险**：中——**步骤顺序必须保持现状**（steer 在 LSP 之后、reminder 在最后），拆分时逐行对照。特别 `applySteer` 里的 channel rendezvous 阻塞语义不能变

### Phase 4 — 抽取入口共享 setup

`RunConversationStream` 和 `RunOneOffStream` 各有 ~100 行 setup，抽两个公共函数：

```go
// 构造 system 消息 + reminder 注入 + 图片附件，返回完整消息切片和 reminderBlock
func (a *AIAgent) prepareTurnMessages(ctx, history, userMessage, systemPrompt string, isFirst bool) (msgs []llm.Message, reminderBlock string)

// 确保 session 存在 + 记录 user 消息 + 生成 title
func (a *AIAgent) ensureSessionAndRecordUser(ctx, userMessage, reminderBlock string, ch chan<- AgentEvent)
```

- 两个入口保留各自的**显式差异**：OneOff 的 `skipSessionWrites` / memory `SkipWrites` / sidecar recorder 不抽，留在入口里一眼可见
- **收益**：入口从 ~100 行降到 ~40 行，差异点凸显
- **风险**：中——resume 场景的 `historyHasReminder`、system 消息插入的三个分支是微妙逻辑，抽取时保持逐行等价

## 顺手清理（可选，伴随 Phase 1）

2026-07-28 删除 `IterationWarningReminder` 后，`systemreminder.Context` 的 `IterationsLeft` / `MaxIterations` 字段**已无任何 reminder 消费**。可以：

- 删除 `Context` 的这两个字段
- `a.iterationBudget` 随之下沉进 `loopState`（目前是 agent 字段，但只在循环 goroutine 读写，属历史遗留）

这是上一轮删除的红利，不做也不影响本方案。

## 测试策略

- 现有测试全部保持绿（行为等价是最好的回归网）
- Phase 1/2 不需要新测试（机械重构）
- Phase 3 拆完后，给 `handleLengthFinish` 的三种续写提示分支补表驱动测试（当前测试盲区，且是最易错的逻辑）
- 每个 Phase 单独 commit，commit message 标注 `refactor: no behavior change`

## 执行顺序

```
Phase 1 (loopState) → Phase 2 (outcome) → Phase 3 (拆 handler) → Phase 4 (入口 setup)
   ~1 小时               ~15 分钟            ~1 小时               ~1 小时
```

Phase 1+2 是地基，收益最大风险最低；3 和 4 可按需做，甚至只做其中一个。
