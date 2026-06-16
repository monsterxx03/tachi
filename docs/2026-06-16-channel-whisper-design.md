# Channel Whisper — IM 群聊选择性回复

> 版本: 1.0 | 日期: 2026-06-16 | 状态: 设计阶段
> 关联: [Whisper Mode（独立 CLI）](./2026-05-22-whisper-mode-design.md),
>       [Channel 架构](../channel/manager/manager.go),
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

独立 `tachi whisper` CLI 的核心思想——"listen to everything, speak only when it matters"——在 channel 场景下同样适用，但不需要独立的 signal gate。Channel whisper 的关键洞察是：

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

```
IncomingMessage
  │
  ├── "定向消息" (directed)
  │    条件: @mention tachi、/command 前缀、或由 channel 实现判定
  │    行为: 立即启动 agent turn（现有行为，不变）
  │
  └── "非定向消息" (undirected / ambient)
       条件: 群聊中的普通对话，不是发给 tachi 的
       行为: 进入 ambient pending 缓冲区 → 按批处理窗口注入
```

Channel 实现通过 `IncomingMessage` 的一个新字段来标记是否为定向消息：

```go
// IncomingMessage 新增字段
type IncomingMessage struct {
    // ... existing fields ...

    // Directed is set by the channel implementation to indicate this message
    // is explicitly addressed to the agent (e.g., @mention, /command prefix,
    // DM channel). When false, the message is treated as ambient — it enters
    // the whisper pipeline and may or may not trigger a response.
    //
    // Default (false / zero value) means ambient, which is the safe default
    // for channels that don't implement directed detection.
    Directed bool
}
```

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
        → 启动批处理窗口计时器（如 60s）
        → 窗口到期后，将累积的消息打包为一条 ambient turn
        → agent 决定是否回复
```

---

## 三、数据流

```
                              ┌─────────────────────────────────────┐
                              │        Channel 实现 (weixin/...)      │
                              │                                      │
                              │  IncomingMessage{                    │
                              │    ThreadID, Content,                │
                              │    Directed: true/false  ← 新增      │
                              │  }                                   │
                              └──────────────┬──────────────────────┘
                                             │
                              ┌──────────────▼──────────────────────┐
                              │         buildHandler()               │
                              │                                      │
                              │  if Directed:                        │
                              │    → 现有流程（立即启动 turn）         │
                              │                                      │
                              │  if !Directed:                       │
                              │    → ambient 管道 ──────────────┐    │
                              └─────────────────────────────────│───┘
                                                                │
                    ┌───────────────────────────────────────────┘
                    ▼
          ┌────────────────────┐
          │  agent turn 活跃?   │
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
   │ 立即返回 │     │ (默认 60s)        │
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
user: "以下是最近 60 秒内群聊「{{.ChannelName}}」中的对话：

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

Channel mode 的 system prompt 需要新增 whisper 指令段：

```
## 群聊环境须知

你处在一个群聊环境中。你会看到两种消息：
1. 直接发给你的消息（用户会 @你 或以 / 命令开头）——这些你应该正常回复。
2. 群聊中其他人的对话（标记为 [群聊]）——这些不是直接发给你的。

对于群聊中的对话：
- 绝大多数时候保持沉默。不要对每句话都回复。
- 只在以下情况回复：
  a. 有人明确讨论了一个你了解且能帮助解决的问题
  b. 对话中出现了你基于已有上下文（session 历史、memory、skill）
     能提供独特见解的机会
  c. 有人分享了数据、代码或结果，而你发现了其中值得注意的问题或模式
- 回复必须简短（≤3 句话），直接切入重点。
- 如果你不确定该不该说话——那就别说。
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
    ambient_batch_window: 60s        # 非定向消息批处理窗口（默认 60s）
    ambient_max_iterations: 3        # ambient turn 最大迭代次数（默认 3）
    ambient_cooldown: 300s           # 同一 thread 两次 ambient turn 最小间隔（默认 300s）
    silence_marker: "SILENCE"        # agent 表示沉默的回复内容（默认 "SILENCE"）
```

### 5.2 Config 结构

```go
// ChannelWhisperConfig holds channel-mode whisper settings.
type ChannelWhisperConfig struct {
    Enabled              bool          `yaml:"enabled" default:"true"`
    AmbientBatchWindow   time.Duration `yaml:"ambient_batch_window" default:"60s"`
    AmbientMaxIterations int           `yaml:"ambient_max_iterations" default:"3"`
    AmbientCooldown      time.Duration `yaml:"ambient_cooldown" default:"300s"`
    SilenceMarker        string        `yaml:"silence_marker" default:"SILENCE"`
}
```

---

## 六、代码改动

### 6.1 `pkg/channel/channel.go` — IncomingMessage 扩展

```go
type IncomingMessage struct {
    // ... existing fields ...
    
    // Directed indicates this message is explicitly addressed to the agent.
    // When false (default), the message enters the ambient whisper pipeline.
    Directed bool
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
    
    ambientPending []ambientMsg        // 非定向消息缓冲区（idle 状态批处理）
    ambientTimer   *time.Timer         // 批处理窗口计时器
    lastAmbient    time.Time           // 上次 ambient turn 结束时间
}
```

### 6.3 `channel/manager/agent_turn.go` — buildHandler() 分叉

`buildHandler()` 中在消息进入现有管线之前加一层判断：

```go
func (m *Manager) buildHandler() channel.MessageHandler {
    return func(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
        // ... existing /command detection ...

        // ---- Channel Whisper: non-directed messages ----
        if !msg.Directed && m.cfg.Channel.Whisper.Enabled {
            return m.handleAmbientMessage(ctx, msg)
        }
        
        // ---- Existing directed message path (unchanged) ----
        // ...
    }
}
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
T1: 计时器启动（60s 窗口）
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
第 3 次 ambient → 窗口从 60s 扩展到 300s（不再那么频繁唤醒）
...以此类推，直到上限（如 600s）
```

定向消息到达时重置退避计数器。

### 7.4 ambient turn 的 session 记录

Ambient turn 仍然走正常的 session 记录（JSONL），但消息标记不同：

```json
{"role": "user", "content": "以下是最近 60 秒内群聊中的对话...", "ambient": true}
{"role": "assistant", "content": "SILENCE", "ambient": true}
```

Dream 系统在扫描 session 时可以过滤 `ambient: true` 的消息以减少噪音。

---

## 八、与独立 `tachi whisper` CLI 的关系

| | `tachi whisper` (独立 CLI) | Channel Whisper (嵌入 channel) |
|---|---|---|
| 事件源 | 文件系统 fsnotify | IM 群聊消息 |
| gate 方式 | 独立 LLM 调用打分 | agent 自身判断 |
| 上下文 | 文件内容 + memory | session 历史 + memory + skill |
| 回复方式 | stderr 打印 + macOS 通知 | IM 消息回复 |
| 运行态 | 独立进程 | channel 进程内 |
| 共享 | memory backend (recall) | memory backend (recall + store) |

二者共享设计哲学（"listen to everything, speak only when it matters"），但实现路径不同。彼此独立，互不依赖。

---

## 九、实现路径

### Phase 1: 骨架（pkg/channel + manager 扩展）

- [ ] `IncomingMessage.Directed` 字段
- [ ] `ChannelWhisperConfig` + 默认值
- [ ] `threadActivation` 新增 `ambientPending` / `ambientTimer` / `lastAmbient`
- [ ] `buildHandler()` 分叉：定向 → 现有路径，非定向 → `handleAmbientMessage()`
- [ ] `ambient.go` 新文件：buffer + timer + flush 逻辑
- [ ] 单测（mock channel 发非定向消息，验证 buffer/timer/steer 注入）

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
