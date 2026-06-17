# Channel Whisper — IM 群聊选择性回复

> 版本: 1.1 | 日期: 2026-06-17 | 状态: 设计阶段
> 关联: [Channel 架构](../channel/manager/manager.go),
>       [Steer 机制](./2026-05-10-steer-mechanism.md),
>       [Memory 系统](./2026-05-17-memory.md)

---

## 一、动机

### 1.1 当前 Channel 模型

tachi 的 channel 模式目前处理消息的方式是"每条消息都进入 agent turn"：

```
IncomingMessage → buildHandler()
  ├── /command → handleSlashCommand (同步，无需 LLM)
  └── 普通消息 → agent turn (阻塞 LLM + tool execution)
```

每条发给 tachi 的消息都会启动一次完整的 LLM 推理 + 工具调用循环。在单聊场景下这没问题——每条消息都是对 tachi 说的。

### 1.2 群聊场景的问题

如果 channel 能接收群内**所有**消息（不只是 @tachi 的），现有的"每条消息都启动 turn"模型会立即崩溃：

| 问题 | 影响 |
|------|------|
| token 成本爆炸 | 群聊每天几百条消息，每条都走 LLM → 天价账单 |
| 延迟崩塌 | 多条消息并发到达，串行处理 → 回复严重滞后于对话 |
| 噪音回复 | agent 对每句话都回复 → 烦死群友，被踢群 |
| session 污染 | 大量无意义对话写入 session → memory/dream 全部污染 |

### 1.3 Whisper 哲学在 Channel 中的应用

Whisper 的核心思想——"listen to everything, speak only when it matters"——在 channel 场景下同样适用，但不需要独立的 signal gate。Channel whisper 的关键洞察是：

> **Agent 本身就是最好的 gate。它拥有 session 上下文、memory 记忆、skill 知识——比任何独立的打分模型都更懂得什么时候该开口。**

---

## 二、核心设计

### 2.1 原则

| 原则 | 说明 |
|------|------|
| **不引入独立 gate** | 复用 agent 自身的判断力，不额外调用 LLM 做"要不要回复"的打分 |
| **定向消息保持现有行为** | @tachi、/command 等明确发给 agent 的消息，立即启动 agent turn，不做任何延迟 |
| **非定向消息走 whisper 管道** | 群聊中的普通消息不作为独立的 user message，而是注入为 ambient context |
| **成本可控** | 非定向消息批量注入，单次 turn 处理多条；turn 结束可标记"无回复" |
| **沉默是默认值** | 绝大多数时候 agent 不回复，只有真正有价值的洞察才开口 |

### 2.2 消息分类

Whisper 模式不是所有 channel 都支持的。支持 whisper 的 channel 也有单聊和群聊两种模式。因此需要三层判断：

```
Layer 1: Channel 是否支持 whisper？
         → channel 实现通过 IncomingMessage 的两个字段表达
         → 不支持 whisper 的 channel: Directed 和 GroupChat 均保持默认值 (false)

Layer 2: 当前 thread 是单聊还是群聊？
         → 取决于 channel 实现的平台字段（如 WeChat 的 GroupID）
         → 首次消息时判定，存入 threadActivation，session 内不变

Layer 3: 当前消息是定向还是非定向？
         → @mention、/command → 定向
         → 群聊普通消息 → 非定向（ambient）
         → 单聊中所有消息都是定向的（即便标记为非定向也不走 ambient 管道）
```

**`IncomingMessage` 新增两个字段**，均由 channel 实现负责设置：

```go
type IncomingMessage struct {
    // ... existing fields ...

    // Directed 表示本条消息是否明确指向 agent。
    //
    // 由 channel 实现根据平台语义设置：
    //   - 单聊 → true（所有消息都是发给你的）
    //   - 群聊中被 @mention 或以 /command 开头 → true
    //   - 群聊中普通对话 → false（ambient）
    //
    // 默认值 false（ambient）是安全保守的选择——
    // 不支持定向检测的 channel 走默认 false，manager 层只
    // 在 GroupChat=true 时才将 !Directed 视为 ambient。
    Directed bool

    // GroupChat 表示当前 thread 是否处于群聊模式。
    //
    // 由 channel 实现根据平台字段判断（如 WeChat 的 GroupID）。
    // 一旦设为 true，该 thread 的整个生命周期都保持群聊模式。
    //
    // 不支持群聊概念的 channel 可以忽略此字段（保持默认 false），
    // manager 层不会对非群聊 thread 启用 whisper 管道。
    GroupChat bool
}
```

**两个字段是正交的：**

| 场景 | `GroupChat` | `Directed` | 行为 |
|------|-------------|------------|------|
| 单聊（所有消息） | `false` | `true` | 现有 agent turn |
| 群聊 @tachi | `true` | `true` | 现有 agent turn |
| 群聊普通消息 | `true` | `false` | whisper 管道 |
| 不支持 whisper 的 channel | `false` | `false` (默认) | 现有 agent turn（`!Directed` 被 guard 过滤）|

### 2.3 两种注入路径

非定向消息根据 agent turn 的活跃状态走不同路径：

```
非定向消息到达
  │
  ├── agent turn 活跃中（当前 thread 正在处理定向消息）
  │     → 追加到 ta.ambientPending
  │     → 下一个 steer 点注入为 RoleSteer，带 [群聊] 前缀
  │     → agent 可在回复中顺带提及，或完全忽略
  │
  └── agent turn 空闲
        → 放入 ambient 批处理桶
        → 启动批处理窗口计时器（如 30s）
        → 窗口到期后，将累积的消息打包为一条 ambient turn
        → agent 决定是否回复
```

---

## 三、数据流

```
                              ┌─────────────────────────────────────┐
                              │   Channel 实现 (如 weixin/polling.go) │
                              │                                      │
                              │   单聊: Directed=true, GroupChat=false│
                              │   群聊@: Directed=true, GroupChat=true│
                              │   群聊普通: Directed=false, GroupChat=true│
                              └──────────────────┬──────────────────┘
                                                 │
                              ┌──────────────────▼──────────────────┐
                              │         buildHandler()               │
                              │                                      │
                              │  1. /command → 同步 / agent turn     │
                              │                                      │
                              │  2. Guard 判断:                      │
                              │     !Directed && ta.groupChat         │
                              │     && cfg.Channel.Whisper.Enabled    │
                              │     → ambient 管道                    │
                              │                                      │
                              │  3. 其余（定向消息）→ 现有 agent turn  │
                              │     (包括单聊、群聊 @mention)          │
                              └──────────────────┬──────────────────┘
                                                 │
                          ┌──────────────────────┘
                          ▼
                ┌────────────────────┐
                │  agent turn 活跃?   │  (仅非定向消息进入此判断)
                └──────┬─────────────┘
                       │
              ┌────────┴────────┐
              ▼                 ▼
         ┌─────────┐     ┌──────────────────┐
         │ 活跃     │     │ 空闲              │
         │         │     │                  │
         │ 追加到   │     │ 放入              │
         │ ambient │     │ ambientPending    │
         │ Pending │     │ (按 thread)       │
         │ (steer) │     │                  │
         │         │     │ 启动/重置计时器    │
         │ 立即返回 │     │ (默认 30s)        │
         │ Steered │     │                  │
         └────┬────┘     └────────┬─────────┘
              │                   │
              ▼                   │ 计时器到期
         ┌──────────────┐         ▼
         │ steer 注入    │   ┌──────────────────────┐
         │              │   │ 启动 ambient turn      │
         │ RoleSteer:   │   │                      │
         │ "[群聊] 张三: │   │ userContent =        │
         │  那个 CI 又   │   │   ambientPrompt +    │
         │  挂了..."     │   │   批处理消息列表       │
         │              │   │                      │
         │ agent 自行   │   │ maxIter = 3 (少量)    │
         │ 决定是否提及  │   │                      │
         └──────────────┘   │ agent 决定是否回复    │
                            └──────────┬───────────┘
                                       │
                          ┌────────────┴────────────┐
                          ▼                         ▼
                   ┌───────────┐            ┌─────────────┐
                   │ agent 回复了│            │ agent 未回复 │
                   │ → 发送回复  │            │ → 不发送      │
                   │ → 正常结束  │            │ → 正常结束    │
                   └───────────┘            └─────────────┘
```

### 3.0 WeChat 实现示例

```go
// channel/weixin/polling.go — processMessage() 中构建 IncomingMessage
isGroupChat := msg.GroupID != ""
isMentioned := containsAtTag(text, botName) || strings.HasPrefix(text, "/")

inMsg := channel.IncomingMessage{
    ThreadID:    threadID,
    MessageID:   messageID,
    Content:     text,
    ChannelID:   msg.GroupID,
    Attachments: attachments,
    Directed:    !isGroupChat || isMentioned,  // 单聊永远定向，群聊看 @
    GroupChat:   isGroupChat,
}
```

对于不支持 whisper 的 channel，两个字段保持零值（`false`），guard 条件自然不满足：

```go
// 不支持 whisper 的 channel: Directed=false, GroupChat=false
// guard: !false && ta.groupChat(false) → false → 不走 ambient 管道
// 消息直接走现有 agent turn 路径
```

### 3.1 活跃 turn 中的 steer 注入格式

当 agent turn 正在活跃时，非定向消息在 steer 点注入。`RoleSteer` 消息格式：

```
[群聊] 张三: 那个 CI 又挂了，谁知道怎么回事？
[群聊] 李四: 好像是 node_modules 没 cache，我重跑一下
[群聊] 张三: 已经第三次了，要不要换个 CI runner？
```

Agent 在 system prompt 的指导下自行判断这些消息中是否有值得回应的内容。

### 3.2 空闲时的 ambient turn

当没有活跃 turn 时，非定向消息被缓冲。计时器到期后，启动一个轻量 ambient turn：

```
user: "以下是最近 30 秒内群聊「{{.ChannelName}}」中的对话：

{{range .Messages}}
[{{.Time}}] {{.Sender}}: {{.Content}}
{{end}}

你可能不需要回复绝大多数内容。请浏览并判断是否有值得你插话的重要
洞察、警告或建议。如果没什么值得说的，回复「SILENCE」即可，我不会
发送任何回复。"
```

Agent 回复 `SILENCE` → 不发消息，turn 正常结束。
Agent 回复其他内容 → 发送到群聊。

---

## 四、System Prompt 扩展

### 4.1 注入时机：session 创建时一次注入

System prompt 是 **session 级别的常量**，不是消息级别的开关。一旦 thread 进入群聊模式（`GroupChat=true`），整个 session 期间都是群聊模式：

```
Session 生命周期:
  ┌────────────────────────────────────────────────────┐
  │  首次消息 → GroupChat=true                          │
  │           → 标准 prompt + 群聊礼仪指令（注入一次）     │
  │           → 后续所有 turn 共用同份 prompt            │
  ├────────────────────────────────────────────────────┤
  │  Turn 1: @tachi 帮我看看 → 定向消息 + 同份 prompt   │
  │  Turn 2: [群聊] 闲聊 (steer) → 同份 prompt         │
  │  Turn 3: @tachi 又挂了 → 定向消息 + 同份 prompt     │
  │  ...                                              │
  └────────────────────────────────────────────────────┘
  → System prompt 不变，LLM prompt 缓存始终命中 ✅
```

**为什么不能动态修改？** 以 Claude 为例，system prompt 是 prompt 缓存的核心 key：

| 策略 | prompt 缓存 | 每次调用成本 |
|------|------------|------------|
| 一次性注入（同一个 system prompt） | ✅ 始终命中 | 低 |
| 每条消息动态注入不同 system prompt | ❌ 每次 miss | 高（延迟 + 费用）|

群聊 session 可能持续几十上百轮，动态修改的代价完全没必要。

### 4.2 LLM 通过消息格式区分定向/非定向

LLM 不需要通过 system prompt 动态变化来区分消息类型。消息本身的格式就够了：

```
定向消息: "张三: @tachi 帮我看看这个 CI 报错"
非定向消息: "[群聊] 张三: 今天 CI 又挂了，谁知道怎么回事？"
```

LLM 看到 `[群聊]` 前缀自然知道这是 ambient 消息——这不是对 ta 说的。

### 4.3 Whisper 指令段

在群聊模式下一旦注入（首次消息时），内容如下：

```
## Group Chat Etiquette

You're in a group chat. You'll see two kinds of messages:
1. Messages **directly addressed to you** (@mention, /command) — reply as normal.
2. **Other people's conversation** (marked with [群聊]) — these are not directed at you.

For group chat messages:
- Stay silent most of the time. Don't reply to everything.
- Only speak when:
  a. Someone is discussing a problem you can help solve.
  b. A topic comes up where your context (session history, memory, skills)
     gives you a unique and useful perspective.
  c. Someone shares data, code, or results, and you spot something worth
     noting — a bug, a pattern, a concern.
- If the chat is casual and off-topic (work-unrelated small talk), stay quiet
  **unless** there's something fun to tease or a joke you can add — it's okay
  to chime in with a lighthearted remark now and then to liven things up.
- Keep replies short (≤3 sentences), straight to the point.
- When in doubt — don't say anything.
```

### 4.4 注入逻辑

```go
// channel/manager/agent_turn.go — runAgentTurn() 中
basePrompt := agent.BuildSystemPrompt(m.cfg.Language, "")
systemPrompt := basePrompt
if ta.groupChat && m.cfg.Channel.Whisper.Enabled {
    systemPrompt = basePrompt + "\n" + whisperPromptSuffix
}

eventCh := aiAgent.RunConversationStream(ctx, priorHistory, userContent,
    systemPrompt, llm.ChatOptions{...})
```

---

## 五、配置

### 5.1 Channel 级配置

在 `ChannelConfig` 下新增 `Whisper` 子配置，影响所有 channel 的非定向消息行为：

```yaml
# config.yaml
channel:
  weixin:
    enabled: true
    # ... existing fields ...
  whisper:
    enabled: true                    # 是否启用 channel whisper（默认 true）
    ambient_batch_window: 30s        # 非定向消息批处理窗口（默认 30s）
    ambient_max_iterations: 50       # ambient turn 最大迭代次数（默认 50）
    ambient_cooldown: 0              # 同一 thread 两次 ambient turn 最小间隔（默认 0，无冷却）
    silence_marker: "SILENCE"        # agent 表示沉默的回复内容（默认 "SILENCE"）
```

### 5.2 Config 结构

```go
// ChannelWhisperConfig holds channel-mode whisper settings.
type ChannelWhisperConfig struct {
    Enabled              bool          `yaml:"enabled" default:"true"`
    AmbientBatchWindow   time.Duration `yaml:"ambient_batch_window" default:"30s"`
    AmbientMaxIterations int           `yaml:"ambient_max_iterations" default:"50"`
    AmbientCooldown      time.Duration `yaml:"ambient_cooldown" default:"0"`
    SilenceMarker        string        `yaml:"silence_marker" default:"SILENCE"`
}
```

---

## 六、代码改动

### 6.1 `pkg/channel/channel.go` — IncomingMessage 扩展

```go
type IncomingMessage struct {
    // ... existing fields ...
    
    // Directed 表示本条消息是否明确指向 agent。
    // 由 channel 实现根据平台语义设置。
    // 默认 false（ambient）——不支持定向检测的 channel 走默认值，
    // manager 层只在 GroupChat=true 时才会将 !Directed 视为 ambient。
    Directed bool

    // GroupChat 表示当前 thread 是否处于群聊模式。
    // 由 channel 实现根据平台字段判断（如 WeChat 的 GroupID）。
    // 设为 true 后，manager 层会：
    //   1. 首次 turn 时注入 whisper system prompt（一次）
    //   2. 非定向消息走 ambient 管道
    // 不支持群聊的 channel 保持默认 false，whisper 不生效。
    GroupChat bool
}
```

### 6.2 `channel/manager/manager.go` — threadActivation 扩展

```go
type ambientMsg struct {
    content   string
    sender    string // 发送者标识，用于格式化
    timestamp time.Time
}

type threadActivation struct {
    // ... existing fields ...
    
    groupChat       bool              // 该 thread 是否群聊模式（首次消息时记录，不变）
    
    // Whisper 管道状态（仅 groupChat=true 时有效）
    ambientPending  []ambientMsg      // 非定向消息缓冲区（idle 状态批处理）
    ambientTimer    *time.Timer       // 批处理窗口计时器
    lastAmbient     time.Time         // 上次 ambient turn 结束时间
    silenceCount    int               // 连续 SILENCE 计数（递增退避用）
}
```

### 6.3 `channel/manager/agent_turn.go` — buildHandler() 分叉 + system prompt 注入

`buildHandler()` 中在消息进入现有管线之前加一层判断，同时首次消息时记录 `groupChat`：

```go
func (m *Manager) buildHandler() channel.MessageHandler {
    return func(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
        // ... existing /command detection ...

        // ---- Channel Whisper guard ----
        // 只有群聊模式下的非定向消息才走 ambient 管道。
        // 单聊中的消息即使 Directed=false 也不会进入 ambient 路径。
        if !msg.Directed && msg.GroupChat && m.cfg.Channel.Whisper.Enabled {
            return m.handleAmbientMessage(ctx, msg)
        }
        
        // ---- Existing directed message path (unchanged) ----
        // ...
        
        // 首次消息：记录 thread 的群聊模式，用于后续 system prompt 注入
        ta.mu.Lock()
        if ta.steerRespCh == nil {
            ta.groupChat = msg.GroupChat
        }
        // ...
    }
}
```

`runAgentTurn()` 中根据 `ta.groupChat` 条件注入 whisper system prompt：

```go
// runAgentTurn() 内，构建 system prompt
basePrompt := agent.BuildSystemPrompt(m.cfg.Language, "")
systemPrompt := basePrompt
if ta.groupChat && m.cfg.Channel.Whisper.Enabled {
    systemPrompt = basePrompt + "\n\n" + whisperPromptBlock
}

eventCh := aiAgent.RunConversationStream(ctx, priorHistory, userContent,
    systemPrompt, llm.ChatOptions{MaxTokens: resolved.MaxTokens})
```

### 6.4 `channel/manager/ambient.go` — 新增文件

核心逻辑文件：

```go
// handleAmbientMessage routes a non-directed message through the whisper pipeline.
func (m *Manager) handleAmbientMessage(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
    ta := m.activateThread(msg.ThreadID, ctx)
    
    ta.mu.Lock()
    defer ta.mu.Unlock()
    
    am := ambientMsg{
        content:   msg.Content,
        sender:    extractSender(msg),  // channel-specific
        timestamp: time.Now(),
    }
    
    // Case A: agent turn is active → inject via steer
    if ta.steerRespCh != nil {
        ta.ambientPending = append(ta.ambientPending, am)
        return channel.HandlerResult{Steered: true}
    }
    
    // Case B: agent turn is idle → batch
    ta.ambientPending = append(ta.ambientPending, am)
    
    if ta.ambientTimer != nil {
        ta.ambientTimer.Stop()
    }
    
    ta.ambientTimer = time.AfterFunc(
        m.cfg.Channel.Whisper.AmbientBatchWindow,
        func() { m.flushAmbientBatch(msg.ThreadID, ta) },
    )
    
    return channel.HandlerResult{Steered: true}
}

// flushAmbientBatch starts a lightweight agent turn with batched ambient messages.
func (m *Manager) flushAmbientBatch(threadID string, ta *threadActivation) {
    ta.mu.Lock()
    
    // Cooldown check
    if time.Since(ta.lastAmbient) < m.cfg.Channel.Whisper.AmbientCooldown {
        ta.ambientPending = nil
        ta.mu.Unlock()
        return
    }
    
    if len(ta.ambientPending) == 0 {
        ta.mu.Unlock()
        return
    }
    
    // Build ambient prompt
    userContent := m.buildAmbientPrompt(ta.ambientPending)
    pending := ta.ambientPending
    ta.ambientPending = nil
    
    // Acquire turn
    ta.steerRespCh = make(chan string)
    ta.resultCh = make(chan handlerResult, 1)
    ta.mu.Unlock()
    
    // Run ambient turn (lower iteration budget)
    go m.runAmbientTurn(context.Background(), threadID, userContent, ta)
    
    // ... result handling: if reply != "SILENCE" → send
}
```

### 6.5 Steer 点注入 ambient 消息

在 `agent_loop.go` 的 steer 点，除了处理 `ta.pending` 中的定向 steer 消息外，也注入 `ta.ambientPending`：

```
steer check → drain ta.pending (定向, 现有) 
           → drain ta.ambientPending (非定向, 新增)
           → 注入为 RoleSteer，带 [群聊] 前缀
```

---

## 七、边界情况

### 7.1 ambient turn 与定向消息的竞态

```
T0: 非定向消息 A 进入 buffer
T1: 计时器启动（30s 窗口）
T2: 定向消息 B（@tachi）到达
```

**处理**：定向消息到达时，如果存在 ambient timer，取消 timer，将已缓冲的 ambient 消息注入为 steer context（而非启动独立 ambient turn）。定向 turn 先执行。

### 7.2 ambient turn 期间新消息到达

```
T0: ambient turn 启动
T1: 新非定向消息 C 到达
```

**处理**：与现有 steer 机制一致——追加到 `ambientPending`，通过 `steerRespCh` 注入当前 turn。

### 7.3 连续 silence 后的"唤醒"

如果 agent 连续多次 ambient turn 都回复 SILENCE（可能是阈值太高或批处理窗口太短），需要一个递增退避：

```
第 1 次 ambient → SILENCE
第 2 次 ambient → SILENCE  
第 3 次 ambient → 窗口从 30s 扩展到 300s（不再那么频繁唤醒）
...以此类推，直到上限（如 600s）
```

定向消息到达时重置退避计数器。

### 7.4 ambient turn 的 session 记录

Ambient turn 仍然走正常的 session 记录（JSONL），但消息标记不同：

```json
{"role": "user", "content": "以下是最近 30 秒内群聊中的对话...", "ambient": true}
{"role": "assistant", "content": "SILENCE", "ambient": true}
```

Dream 系统在扫描 session 时可以过滤 `ambient: true` 的消息以减少噪音。

---

## 八、实现路径

### Phase 1: 骨架（pkg/channel + manager + weixin 扩展）

- [ ] `IncomingMessage.Directed` + `GroupChat` 字段
- [ ] `ChannelWhisperConfig` + 默认值（`config.go`）
- [ ] `BuildSystemPrompt` 可扩展接口（追加 whisper 指令段）
- [ ] `threadActivation` 新增 `groupChat` / `ambientPending` / `ambientTimer` / `lastAmbient` / `silenceCount`
- [ ] `buildHandler()` guard 分叉 + 首次消息记录 `groupChat`
- [ ] `runAgentTurn()` 条件式 system prompt 注入
- [ ] WeChat `processMessage()` 设置 `Directed` + `GroupChat`
- [ ] `ambient.go` 新文件：buffer + timer + flush 逻辑
- [ ] 单测（mock channel 发定向/非定向消息，验证 buffer/timer/steer 注入/gruop chat prompt）

### Phase 2: ambient turn 实现

- [ ] `buildAmbientPrompt()`：格式化批处理消息
- [ ] `runAmbientTurn()`：轻量 agent turn（减少 max_iterations）
- [ ] silence marker 检测：回复匹配 `SILENCE` → 不发送
- [ ] cooldown 机制
- [ ] session 记录标记 `ambient: true`

### Phase 3: steer 注入 ambient context

- [ ] agent loop steer 点同时 drain `ambientPending`
- [ ] `RoleSteer` 格式化带 `[群聊]` 前缀
- [ ] system prompt 新增 group chat whisper 指令

### Phase 4: 调优

- [ ] 递增退避（连续 SILENCE → 扩大窗口）
- [ ] ambient turn 与定向消息竞态处理
- [ ] thread 粒度统计（ambient 命中率：多少 ambient turn 产生了回复）
- [ ] 端到端手动验证 + prompt 迭代
