# Discord Channel 设计

> 版本: 1.5 | 日期: 2026-07-11 | 状态: 已实现（Phase 1-3）
> 关联: [Channel 接口](../pkg/channel/channel.go),
>       [Channel Manager](../channel/manager/manager.go),
>       [Weixin Channel 实现](../channel/weixin/channel.go)（参考模式）

---

## 目录

1. [动机与目标](#1-动机与目标)
2. [总体架构](#2-总体架构)
3. [技术选型](#3-技术选型)
4. [会话模型](#4-会话模型)
5. [消息处理流程](#5-消息处理流程)
6. [配置设计](#6-配置设计)
7. [文件与附件处理](#7-文件与附件处理)
8. [交互式组件](#8-交互式组件)
9. [访问控制](#9-访问控制)
10. [生命周期管理](#10-生命周期管理)
11. [实现阶段](#11-实现阶段)
12. [未解决问题](#12-未解决问题)

---

## 1. 动机与目标

### 1.1 动机

Tachi 目前支持两种 IM 通道：**Weixin**（企业微信 iLink Bot，HTTP 长轮询）和 **Chrome**（本地 WebSocket）。Discord 是全球最活跃的开发者社区之一，添加 Discord 支持可以让用户：

- 在常用的 Discord 服务器中直接与 Tachi 交互
- 利用 Discord 的频道系统组织不同话题的对话（`#coding`、`#research` 等）
- 通过 Discord 的私信功能随时随地使用 Tachi

### 1.2 目标

**第一版（MVP）目标**：

- 通过 Discord Gateway WebSocket 建立持久连接
- 支持 DM 私信和 Guild 服务器频道消息收发
- 支持 @mention 触发回复（以及 DM 自动回复）
- 支持文件附件接收与发送
- 支持 Typing indicator 处理状态反馈
- 基本访问控制（allowlist）

**后续版本**：

- Thread 自动创建隔离对话
- Slash Commands 原生支持
- Interactive Components（按钮 / Select Menu / Modal）
- Voice 语音消息支持
- Multi-account 多机器人

---

## 2. 总体架构

### 2.1 架构总览

```
┌─────────────────────────────────────────────────────────┐
│                    Discord API                             │
│  ┌──────────────────────────────────────────────────┐    │
│  │  Gateway (wss://gateway.discord.gg)              │    │
│  │  · 实时事件：MESSAGE_CREATE, INTERACTION_CREATE   │    │
│  │  · 心跳保活 / Resume 断线重连                     │    │
│  │  · Intents 权限声明                               │    │
│  └──────────────────────┬───────────────────────────┘    │
│  ┌──────────────────────▼───────────────────────────┐    │
│  │  REST API (https://discord.com/api/v10)          │    │
│  │  · 发送消息 / 上传文件 / 创建线程                 │    │
│  │  · Slash Command 注册                            │    │
│  └──────────────────────────────────────────────────┘    │
└──────────────────────────┬──────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────┐
│                        tachi                              │
│                                                          │
│  ┌──────────────────────────────────────────────────┐    │
│  │              channel/manager                       │    │
│  │  ┌────────────────┐  ┌────────────────────────┐   │    │
│  │  │  WeChat Channel │  │ Discord Channel (NEW)  │   │    │
│  │  │  (长轮询)        │  │ (Gateway + REST)       │   │    │
│  │  ├────────────────┤  ├────────────────────────┤   │    │
│  │  │  Chrome Channel │  │ 未来: Slack/Telegram... │   │    │
│  │  └────────────────┘  └────────────────────────┘   │    │
│  │                                                   │    │
│  │  ┌──────────────────────────────────────────────┐ │    │
│  │  │              Agent 实例                        │ │    │
│  │  │  · LLM 推理                                   │ │    │
│  │  │  · Tool 执行 (Bash, ReadFile, WebSearch, ...) │ │    │
│  │  │  · Memory / MCP / Session                     │ │    │
│  │  └──────────────────────────────────────────────┘ │    │
│  └──────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────┘
```

### 2.2 Gateway vs REST

| 层面 | 协议 | 用途 |
|------|------|------|
| **Gateway** | WebSocket (wss://) | 接收实时事件（消息、互动、成员变更） |
| **REST API** | HTTPS (api/v10) | 发送消息、上传文件、查询信息、注册命令 |

Gateway 是 Discord Bot 的核心——必须保持长连接才能收到消息事件。REST API 用于"发出去"的操作。

### 2.3 重连策略

Gateway 可能因网络波动或服务器维护断开。`discordgo` 内置了自动重连（`AutoReconnect` 默认开启，无限重连），包括 Resume 机制（利用 session ID 和 sequence number 恢复事件流）。tachi 层面需要明确两层重连的职责划分：

| 层级 | 负责内容 | 上限 |
|------|---------|------|
| **discordgo 内置** | WebSocket 断线重连 + Resume 恢复 | 默认无限，可通过 `sess.AutoReconnect` 控制 |
| **tachi Manager** | 长时间断连后重新调度 `Run()` | `ReconnectMaxAttempts` / `ReconnectBackoff` |

Resume 机制的 session ID 和 sequence number 由 discordgo 内部维护，tachi 不直接操作。

```go
type reconnectConfig struct {
    MaxAttempts int           // 最大重连尝试次数（0 = 无限，由 discordgo 处理）
    Backoff     time.Duration // 初始退避间隔（默认 1s）
    MaxBackoff  time.Duration // 最大退避间隔（默认 30s）
}
```

> **注意**：discordgo 的 `AddHandler` 会在 **每次** `Run()` 调用时追加事件处理器。如果 Manager 因重连多次调用 `Run()`，会导致 handler 重复注册、同一事件触发多次。需要在 `Run()` 开始前清理旧 handler。
> 
> `sess.RemoveHandler()` 通过函数指针（handler reference）删除，需要在首次 `AddHandler()` 时保存返回的 handler ID。如果使用匿名函数注册，每次 `AddHandler` 都会生成新指针，无法通过早前的 reference 删除。推荐方案：将 handler 注册放到一次性初始化流程中（如 `OnStart`），`Run()` 只负责 `Open()` 连接和等待 ctx.Done()，避免重入问题。

---

## 3. 技术选型

### 3.1 discordgo

使用 [`github.com/bwmarrin/discordgo`](https://github.com/bwmarrin/discordgo)（v0.29+），Go Discord 生态中最成熟的库：

| 特性 | 支持情况 |
|------|----------|
| Gateway v10 | ✅ 完整支持 |
| Intents | ✅ 所有 Gateway Intents |
| REST API v10 | ✅ 完整支持 |
| Slash Commands | ✅ 注册、响应、组件 |
| Message Components | ✅ Buttons, Select Menus |
| Modal | ✅ Modal 交互 |
| Threads | ✅ 创建与管理 |
| Voice | ✅ 通过 discordgv |
| File Attachments | ✅ File / io.Reader |
| Rate Limiting | ✅ 内置 |

> ⚠️ **特权 Intents**：`IntentsMessageContent`（消息内容）和 `IntentsGuildMembers`（成员列表）需要在 [Discord Developer Portal] → Bot 设置页面手动开启。新用户首次部署时最容易遗漏此步骤，建议在 channel 启动时校验 Intent 是否生效，或给出清晰的错误提示。

### 3.2 依赖声明

```go
// channel/discord/discord.go
package discord

import (
    "github.com/bwmarrin/discordgo"
)
```

在 `go.mod` 中添加：

```
require github.com/bwmarrin/discordgo v0.29.0
```

---

## 4. 会话模型

### 4.1 ThreadID 设计

Discord 中会话隔离策略取决于消息场景。与 DM 或 Thread 不同，**Guild 频道中多人 @bot 时，bot 应该共享上下文**，知道频道中前后谁说了什么。因此 ThreadID 的默认设计按场景区分：

| 场景 | ThreadID 格式 | 会话策略 | 说明 |
|------|--------------|---------|------|
| **DM** | `dm:<userID>` | 每人独立 | 私信天然隔离 |
| **Guild 频道** | `guild:<guildID>:channel:<channelID>` | 全频道**共享** | 所有人共用一个 session |
| **Thread** | `guild:<guildID>:thread:<threadID>` | 共享 | thread 内一个 session |
| **Forum 帖子** | `guild:<guildID>:thread:<threadID>` | 共享 | 与 thread 相同 |

**设计决策：**

- Guild 频道默认**整个频道共享一个 session**，bot 能看到频道中所有 @mention 和非定向消息（通过 Whisper），理解完整的对话上下文
- DM 始终独立 session
- 若需要按用户隔离（如客服频道中每人一个独立对话），可通过配置 `groupSessionsPerUser: true` 覆盖

**消息归属标注：**

共享 session 下，LLM 收到的每条消息需要附带发送者信息，格式为：

```
[用户 A]: 这段代码有什么问题？
[用户 B]: 我觉得可能是并发问题
[用户 A]: @Tachi 能确认一下吗？
```

这样 LLM 区分谁说了什么，避免混淆。

### 4.2 会话生命周期

```
首次消息 → session 不存在 → 创建新 session → 启动 agent turn
后续消息 → session 存在 → 加载 session → 继续对话
/model   → 更新 provider 覆盖 → evict 缓存的 agent → 下次消息重建
/new     → 清空 session 历史 → evict 缓存的 agent → 全新开始
/compact → 运行 summarization → 替换 session 内容
```

**共享 session 下的并发语义：**

若用户 A 的 agent turn 正在进行中，用户 B 在同一频道发来消息：

- 默认：B 的消息通过 **steer 机制**注入到 A 正在进行的 turn 中（类似群聊中插话补充）
- Agent 回复时将同时看到 A 和 B 的输入，给出综合回复
- 这符合群聊场景的自然交互——"有人在对话中补充信息"

> ⚠️ 注意：steer 注入的消息不会独立触发新的回复。如果 B 的问题与 A 无关（完全独立的话题），可能会出现不期望的回复混杂。这是共享 session 模型的固有限制，后续可通过 `/new` 命令主动开启新 session。

> ⚠️ **steer 注入的 UX 影响**：B 的消息通过 steer 注入后，不会启动新的 handler 流程或发送任何反馈，B 需要等待 A 的整个 turn 结束才能看到 bot 回复（包括 LLM 推理 + 多轮 tool call，可能达数分钟）。在此期间 B 没有任何反馈，体验类似于"消息已发送但 bot 没看到"。建议给 steer 注入的短暂等待后启动独立的 lightweight typing indicator。若需要完全隔离的并发响应，需依赖 `/new` 新建 session。

### 4.3 Agent 缓存

遵循 Manager 现有模式（`agentCache`）：每个 ThreadID 对应一个缓存 AIAgent，跨消息复用。共享 session 下所有用户共用同一个 agent 缓存，因此 agent 的状态（MCP discoveredSet、技能激活、token 计数）在频道内所有用户的对话间持续累积。

---

## 5. 消息处理流程

### 5.1 完整消息流

```
1. Gateway 收到 MESSAGE_CREATE 事件
   │
2. 权限检查
   │  ├─ DM: 检查 allowlist / DM policy
   │  └─ Guild: 检查 @mention / 频道 allowlist / 角色权限
   │
2.5 消息去重
   │  维护一个 recentMessageIDs 集合（固定大小 ring buffer + TTL），
   │  对刚刚处理过的 messageID 去重。Gateway 在网络抖动或 Resume
   │  恢复后可能重复投递 MESSAGE_CREATE，此步骤可避免重复触发。
   │
3. 消息路由
   │  ├─ 忽略: bot 自己的消息 / ignored_channels / 其他 bot 消息
   │  ├─ 定向 (DM / @mention): → 步骤 4
   │  └─ 非定向 (ambient): → whisper 缓冲管道（共享 session 下提供上下文）
   │
4. 构造 IncomingMessage
   │  ├─ ThreadID = 按 §4.1 规则确定
   │  ├─ Content = 消息文本（共享 session 下附带 `[发送者]: ` 前缀）
   │  ├─ MessageID = Discord message ID
   │  ├─ Attachments = 下载后的附件列表
   │  ├─ Sender = 发送者用户名 / 显示名称
   │  ├─ Directed = DM || @mention
   │  └─ GroupChat = Guild 频道中
   │
5. 调用 handler(ctx, msg)
   │
6. 处理结果
   │  ├─ Steered → stop typing indicator, continue
   │  ├─ Buffered → no action (whisper)
   │  └─ Reply → 发送回复（可能分段）
   │
7. 发送回复
   ├─ 文本 → 发送到频道/thread，@mention 触发者
   ├─ 附件 → 上传文件后发送
   └─ 组件 → 附加交互组件
```

### 5.2 提及策略

| 模式 | 行为 |
|------|------|
| **DM** | 始终回复，不检查 @mention |
| **Guild 频道（默认）** | 只在被 @mention 时回复 |
| **Free-response channels** | 无需 @mention，所有消息都回复 |
| **Thread（Bot 已参与）** | 默认无需 @mention（可通过 `threadRequireMention` 关闭） |

**@mention 格式：** Discord 有两种 mention 格式，均需正确处理：
- `<@USER_ID>` — 普通 mention
- `<@!USER_ID>` — Nickname mention（用户设置了频道昵称时使用；注意：Discord API v10 中此格式已弃用，但部分客户端仍可能发送，解析时需兼容）

Bot 在解析时应比较去掉 `<@>` 和 `<@!>` 前缀后的 ID 是否与自己的 `botUserID` 匹配。

**共享上下文下的 Ambient 消息：**
在共享 session 模型中，非定向消息（没有 @bot）通过 Whisper 缓冲管道积累。当 bot 被 @mention 触发时，缓冲期内的 ambient 消息随消息上下文一起注入给 LLM，让 bot 感知到 trigger 之前的频道讨论。

```go
type mentionConfig struct {
    RequireMention       bool     // 默认 true：需要 @mention
    ThreadRequireMention bool     // 默认 false：Thread 内免 @mention
    FreeResponseChannels []string // 免 @mention 频道 ID 列表
    IgnoreOtherMentions  bool     // 默认 true：@了别人但没 @bot 时不回复
}
```

### 5.4 打字指示

| 反馈 | 实现方式 | 时机 |
|------|---------|------|
| Typing | `Channel.ChannelTyping()` | handler 阻塞期间定期触发 |

Typing indicator 作为 handler 处理期间的可见反馈。discordgo 的 `ChannelTyping()` 调用有频率限制，每个 channel 每 10 秒最多一次，实现中按 10 秒间隔发送。

---

## 6. 配置设计

### 6.1 YAML 配置

遵循 tachi 现有模式，通过 `channel.channels.discord` 通用配置字段接入：

```yaml
channel:
  channels:
    discord:
      enabled: true
      token: "DISCORD_BOT_TOKEN"           # bot token，支持 env 引用
      application_id: "1234567890"          # Application ID（可选，加速启动）

      # --- 访问控制 ---
      allowed_users:                        # 允许使用的用户 ID 列表
        - "284102345871466496"
      allowed_roles:                        # 允许使用的角色 ID 列表
        - "987654321098765432"
      allow_all_users: false                # 是否允许所有用户（仅用于开发/私服）

      # --- 提及策略 ---
      require_mention: true                 # Guild 频道需要 @mention
      thread_require_mention: false         # Thread 内免 @mention
      free_response_channels:               # 免 @mention 频道
        - "123456789012345678"
      ignore_other_mentions: true           # 忽略 @别人但没 @bot 的消息

      # --- 频道路由 ---
      ignored_channels: []                  # 永不回复的频道
      allowed_channels: []                  # 只允许回复的频道（空 = 全部允许）
      home_channel: ""                      # 主动消息发送频道（cron/通知）

      # --- Thread ---
      auto_thread: false                    # @mention 时不自动创建 thread（默认 false）
      no_thread_channels: []                # 不要自动 thread 的频道

      # --- 附件 ---
      max_attachment_bytes: 33554432        # 最大附件字节数（默认 32MiB）
      allow_any_attachment: false           # 是否允许任意文件类型

      # --- 交互 ---
      typing: true                          # 启用 typing indicator

      # --- Session ---
      group_sessions_per_user: false          # 频道内是否按用户隔离 session（默认 false = 共享）

      # --- 渠道提示词 ---
      channel_prompts:                      # 按频道的额外 system prompt
        "123456789012345678": |
          This channel is for code review.
          Be thorough and point out security issues.

      # --- 问候语 ---
      greeting: "👋 Tachi 已上线！"
```

### 6.2 配置结构体

```go
// channel/discord/config.go

type DiscordConfig struct {
    Enabled        bool              `yaml:"enabled"`
    Token          string            `yaml:"token"`
    ApplicationID  string            `yaml:"application_id"`

    // Access control
    AllowedUsers   []string          `yaml:"allowed_users"`
    AllowedRoles   []string          `yaml:"allowed_roles"`
    AllowAllUsers  bool              `yaml:"allow_all_users"`

    // Mention
    RequireMention      bool   `yaml:"require_mention" default:"true"`
    ThreadRequireMention bool  `yaml:"thread_require_mention" default:"false"`
    FreeResponseChannels []string `yaml:"free_response_channels"`
    IgnoreOtherMentions  bool   `yaml:"ignore_other_mentions" default:"true"`

    // Channels
    IgnoredChannels []string `yaml:"ignored_channels"`
    AllowedChannels []string `yaml:"allowed_channels"`
    HomeChannel     string   `yaml:"home_channel"`

    // Thread
    AutoThread      bool     `yaml:"auto_thread" default:"false"`
    NoThreadChannels []string `yaml:"no_thread_channels"`

    // Attachments
    MaxAttachmentBytes int64 `yaml:"max_attachment_bytes" default:"33554432"`
    AllowAnyAttachment bool  `yaml:"allow_any_attachment" default:"false"`

    // Typing indicator
    Typing bool `yaml:"typing" default:"true"`

    // Session
    GroupSessionsPerUser bool `yaml:"group_sessions_per_user" default:"false"`

    // Per-channel prompts
    ChannelPrompts map[string]string `yaml:"channel_prompts"`

    // Greeting
    Greeting string `yaml:"greeting"`

    // Slash commands
    DevGuildID string `yaml:"dev_guild_id"`   // 开发环境 guild ID，用于注册即时生效的 guild-level commands

    // Embed
    EmbedEnabled bool `yaml:"embed_enabled" default:"false"` // 是否支持 Embed 消息发送（Phase 2+）

    // Reconnect
    ReconnectMaxAttempts int           `yaml:"reconnect_max_attempts" default:"0"`
    ReconnectBackoff     time.Duration `yaml:"reconnect_backoff" default:"1s"`
    ReconnectMaxBackoff  time.Duration `yaml:"reconnect_max_backoff" default:"30s"`
}
```

> **关于 `default` tag**：上述结构中使用了 `yaml` tag 中的 `default` 字段标注默认值。⚠️ **Go 标准库 `yaml.Unmarshal` 不支持解析 `default` tag**，所有默认值**必须**在 `NewChannel()` 构造函数中手动设置（`yaml` 包的 `default` tag 仅在 `yaml.UnmarshalStrict` 中有特殊含义，且与这里用法不同）。实现时应将默认值集中配置在构造函数中，而非依赖 tag 解析。

### 6.3 环境变量引用

参考 Weixin 的 token 处理方式（`source: env` 结构），支持环境变量引用：

```yaml
channel:
  channels:
    discord:
      token:
        source: env
        provider: default
        id: DISCORD_BOT_TOKEN
```

或直接字符串（简单情况）：

```yaml
channel:
  channels:
    discord:
      token: "MTIzNDU2Nzg5MDEyMzQ1Njc4OQ.Gabcde..."
```

---

## 7. 文件与附件处理

### 7.1 接收附件

Discord 消息中可能包含 attachments（图片、文件、视频等）和 embeds（链接预览）。处理流程：

1. 检查大小限制（`max_attachment_bytes`，默认 32MiB）
2. 从 Discord CDN URL 下载文件内容（**需要 `Authorization: Bot <token>` header**，CDN 对非公开资源会拒绝未认证请求）
3. 探测 MIME 类型，分类为 text/image/file
4. 文本文件尝试 UTF-8 解码，内容注入 prompt（上限 100KiB）
5. 所有文件保存到本地缓存目录 `~/.tachi/discord/cache/`
6. 构造 `channel.Attachment` 传入 handler

**文件类型限制：** 当 `allow_any_attachment: false`（默认）时，只允许安全类型的文件。Phase 1 以简单后缀白名单实现（`.txt, .md, .pdf, .png, .jpg, .gif`），后续可配置。超过限制的文件跳过内容注入，仅保留路径供 LLM 感知。

> **类型检测方式**：Discord CDN URL 的格式为 `https://cdn.discordapp.com/attachments/{channelID}/{attachmentID}/{filename}`，其中 filename 由上传者指定，后缀可能被伪造。建议不仅依赖后缀，同时用 `mime.TypeByExtension` + 文件头 magic bytes 双重校验，提高安全性。

discordgo 的 `Session` 未直接提供下载 attachment 的方法，需使用 `http.Client` 自行请求 CDN URL（需带上 `Authorization: Bot <token>` header）。

### 7.2 发送附件

当 agent 回复中包含附件时：

1. 通过 `discordgo.ChannelFileSendWithMessage` 上传到 Discord CDN
2. 在同一消息中引用 attachment ID
3. 支持 `OutgoingAttachment` 的 Data 和 LocalPath 两种模式

### 7.3 MEDIA 标签（Phase 2+）

参考 Hermes 的做法：agent 可以在回复中输出 `MEDIA:/path/to/file` 标签，adapter 自动将其替换为 Discord 附件：

```
回复文本：
  这是生成的报表 MEDIA:/tmp/report.pdf

实际发送：
  文本 + 附件（report.pdf）
```

---

## 8. 交互式组件

### 8.1 组件类型（Phase 2+）

| 组件 | 用途 | 对应 tachi 场景 |
|------|------|-----------------|
| **Button** | 选择/确认 | AskUserQuestion 选择、EditFile 确认 |
| **Select Menu** | 选项列表 | 模型选择、频道选择 |
| **Modal** | 表单输入 | 复杂参数输入 |
| **Action Row** | 组件容器 | 组织多个组件 |

**CustomID 命名约定：** Discord 中 CustomID 是 bot 全局唯一的字符串，跨所有组件共享命名空间。为避免组件冲突，使用带前缀的格式：

```
tachi:<type>:<sessionID>:<optionID>
```

例如 `tachi:ques:abc123:opt:0`、`tachi:confirm:def456`。解析时按前缀路由到对应 handler。

### 8.2 AskUserQuestion 交互

当 agent 调用 AskUserQuestion 时，Discord channel 可以将其渲染为组件消息：

- 有选项 → 按钮列表
- 无选项 → 文本等待（"请回复此消息来回答"）
- 多选 → Checkbox 组件

用户交互通过 Gateway 的 `INTERACTION_CREATE` 事件接收，结果通过 `AskUserAnswers` 字段回传给 agent。

> **goroutine 桥接：** Agent 调用 AskUserQuestion 后 `PresentQuestions()` 阻塞等待用户响应；而用户点击按钮触发的 `INTERACTION_CREATE` 在 discordgo 的独立 goroutine 中到达。因此 channel 层需要一个 `chan AskUserAnswers`（或 callback 注册表 + `sync.Cond`）来桥接两个 goroutine。多个并发的 AskUserQuestion 交互需要依赖 CustomID 正确路由到对应的等待者。参见 §12.4 的并发安全讨论。

### 8.3 组件生命周期

```
Agent 调用 AskUserQuestion
  → PresentQuestions() 发送带按钮的消息
  → 用户点击按钮
  → Gateway 收到 INTERACTION_CREATE
  → 3 秒内发送 Deferred ACK（否则 Discord 显示"交互失败"）
  → 解析 CustomID → 构造 AskUserAnswers
  → 调用 handler(msg) → 返回给等待中的 agent
  → 按钮置灰（避免重复点击）
```

**注意（重要）：**

1. **3 秒 ACK 窗口**：Discord 要求交互触发后 3 秒内必须响应（`InteractionRespond`），否则用户端显示"交互失败"。若处理时间较长，先发送 `DeferredMessage` ACK，后续再更新内容（15 分钟内可更新）。
2. **15 分钟生命周期**：组件回调最多 15 分钟有效。AskUserQuestion 等待用户回复可能超过此限制，需设置超时兜底——超时后将按钮置灰并回退到文本等待模式（"交互已超时，请直接回复此消息"）。
3. **组件不持久化**：bot 重启后组件状态丢失，用户点击后会收到"交互失败"提示。建议重启场景通过文本兜底。

---

## 9. 访问控制

### 9.1 三级授权体系（Guild 频道）

| 级别 | 控制粒度 | 实现 |
|------|---------|------|
| **User** | 单个用户 | `allowed_users` ID 列表 |
| **Role** | 角色级 | `allowed_roles` 角色 ID 列表 |
| **Global** | 全局开关 | `allow_all_users`（仅限开发/私服） |

### 9.2 判定逻辑

**Guild 频道**按三级授权判定；**DM 私信**只检查 `allowed_users` 列表（角色体系不适用）。

```go
func (ch *DiscordChannel) isAuthorized(userID string, memberRoles []string, isDM bool) bool {
    if isDM {
        // DM 只检查用户级 allowlist
        for _, id := range ch.cfg.AllowedUsers {
            if id == userID {
                return true
            }
        }
        return false
    }
    if ch.cfg.AllowAllUsers {
        return true
    }
    for _, id := range ch.cfg.AllowedUsers {
        if id == userID {
            return true
        }
    }
    for _, role := range memberRoles {
        for _, allowedRole := range ch.cfg.AllowedRoles {
            if role == allowedRole {
                return true
            }
        }
    }
    return false
}
```

### 9.3 频道隔离

- `allowed_channels`: 只在这些频道回复（空 = 全部允许）
- `ignored_channels`: 即使 @mention 也不回复

### 9.4 成员信息缓存

避免每次消息都通过 REST API 查询成员信息：

```go
type memberCache struct {
    mu      sync.RWMutex
    members map[string]map[string]*guildMemberEntry // guildID → userID → entry
}

type guildMemberEntry struct {
    Roles []string
    TTL   time.Time
}
```

- 缓存 TTL 默认 5 分钟
- 通过 Gateway 的 `GUILD_MEMBER_UPDATE` 事件增量更新
- **按 guild_id 隔离**：同一用户在不同服务器中可能有不同角色，缓存 key 应包含 `guildID + userID`
- Gateway 断连期间角色可能发生变化，5 分钟 TTL 提供了合理的时效性；若角色变更频繁，可缩短 TTL 或将其配置化

**冷启动回填：** Bot 刚启动时 memberCache 为空，此时任何消息的权限检查都无法命中缓存。需要 fallback 路径：cache miss 时通过 `sess.GuildMember(guildID, userID)` 同步查询 REST API，查询结果写入缓存并设置 TTL。同时监听 `GUILD_MEMBER_UPDATE` 和 `GUILD_CREATE`（首次加入 guild 时全量拉取）来预热缓存。

---

## 10. 生命周期管理

### 10.1 Channel 结构体

```go
// channel/discord/channel.go

type DiscordChannel struct {
    cfg     DiscordConfig
    session *discordgo.Session
    logger  *debuglog.Logger

    // 运行时状态
    mu          sync.RWMutex
    botUserID   string                 // Bot 自己的 UserID（用于 @mention 检测）
    memberCache *memberCache           // 用户信息缓存

    // 组件回调注册
    componentHandlers map[string]componentHandler // CustomID → handler

    // 附件缓存目录
    cacheDir string
}
```

### 10.2 工厂函数（init() 注册）

```go
func init() {
    channel.Register("discord", func(rawCfg map[string]any) (channel.Channel, error) {
        b, err := yaml.Marshal(rawCfg)
        if err != nil {
            return nil, fmt.Errorf("discord: marshal config: %w", err)
        }
        var cfg DiscordConfig
        if err := yaml.Unmarshal(b, &cfg); err != nil {
            return nil, fmt.Errorf("discord: unmarshal config: %w", err)
        }
        return NewChannel(cfg)
    })
}
```

### 10.3 Run 循环

```go
func (ch *DiscordChannel) Run(ctx context.Context, handler channel.MessageHandler) error {
    // 1. 创建 discordgo session
    sess, err := discordgo.New("Bot " + ch.resolveToken())
    if err != nil {
        return fmt.Errorf("discord: create session: %w", err)
    }
    ch.session = sess

    // 2. 注册 event handlers
    // 注意：AddHandler 会在每次 Run() 调用时追加 handler。如果 Run()
    // 因重连被多次调用，需确保 handler 不重复注册（参见 §2.3 注意）
    sess.AddHandler(ch.onMessageCreate(handler))
    sess.AddHandler(ch.onInteractionCreate(handler))
    sess.AddHandler(ch.onReady)

    // 3. 配置 Intents
    // 注意：IntentsGuilds 和 IntentsGuildMembers 都是 Privileged Intent，
    // 需要在 Discord Developer Portal → Bot 设置页面手动开启。
    // IntentsGuilds 本身不是消息接收的必要条件——IntentsGuildMessages 已足够。
    // 这里声明 IntentsGuilds 主要用于 GUILD_MEMBER_UPDATE（成员缓存更新）。
    // 如果只做基础消息收发，可以只声明 IntentsGuildMessages + IntentsDirectMessages + IntentsMessageContent。
    sess.Identify.Intents = discordgo.IntentsGuilds |
        discordgo.IntentsGuildMessages |
        discordgo.IntentsDirectMessages |
        discordgo.IntentsMessageContent |
        discordgo.IntentsGuildMembers

    // 4. 建立 Gateway 连接
    if err := sess.Open(); err != nil {
        return fmt.Errorf("discord: gateway open: %w", err)
    }
    defer sess.Close()

    // 5. 等待 context 取消
    <-ctx.Done()
    ch.logger.Log("discord: context cancelled, shutting down")
    return nil
}
```

> **Gateway handler goroutine 模型：** discordgo 的 `AddHandler` 为每个事件类型启动一个独立的 goroutine 来执行 handler。因此 `MESSAGE_CREATE` 和 `INTERACTION_CREATE` 事件可能并发到达。tachi 的 Manager 层通过 `threadActivation`（per-thread 串行化）和 steer 机制处理并发，但 channel 层仍需要注意共享状态的并发安全。

> **API 版本：** `discordgo.New()` 默认使用 Discord API v9。文档架构图引用的是 v10，若需显式使用 v10 需设置 `sess.DiscordAPIVersion` 或在 `New()` 后更新 API endpoint。建议在 Gateway 初始化阶段统一确认版本设置。

### 10.4 OnStart 钩子

```go
func (ch *DiscordChannel) OnStart(ctx context.Context) error {
    // 1. 解析 token（env 引用或直接字符串）
    // 2. 创建附件缓存目录 ~/.tachi/discord/cache/
    // 3. 初始化成员缓存（预热：首次连接后通过 GUILD_CREATE 事件获取成员列表）
    // 4. 初始化组件回调注册表
    // 5. 如配置了 DevGuildID 且非空，注册 guild-level Slash Commands（即时生效）
    //    否则注册 global commands（最多 1 小时缓存延迟）
    return nil
}
```

### 10.5 连接状态管理

```go
const (
    stateDisconnected = iota
    stateConnecting
    stateConnected
    stateReconnecting
)

type connectionState int32 // atomic
```

discordgo 内置了 `sess.Close()` 后的重连逻辑（`sess.AutoReconnect = true`）。tachi 层监控连接状态，在长时间无法重连时通过 manager 重新调度。

---

## 11. 实现阶段

### Phase 1：MVP（2-3 周）

核心目标：跑通 Gateway 连接 + 消息收发的完整链路。

| 功能 | 文件 | 说明 |
|------|------|------|
| Channel 骨架 | `channel/discord/channel.go` | Channel 接口实现 + init() 注册 |
| 配置结构 | `channel/discord/config.go` | DiscordConfig 结构体 |
| Gateway 连接 | `channel/discord/gateway.go` | discordgo session 初始化 + Intents |
| 消息接收 | `channel/discord/handler.go` | MESSAGE_CREATE → IncomingMessage |
| @mention 检测 | `channel/discord/mention.go` | mention 解析 + 策略判定 |
| DM 支持 | `channel/discord/dm.go` | 私信路由 |
| 文本回复 | `channel/discord/send.go` | ChannelMessageSend |
| 文件附件 | `channel/discord/attachment.go` | 下载/上传/缓存 |
| Typing indicator | `channel/discord/feedback.go` | 打字指示 |
| 访问控制 | `channel/discord/auth.go` | user/role allowlist |

**测试要点**：
- Gateway 连接与重连
- @mention 检测准确性（注意 `@!` nickname 前缀）
- 附件下载与大小限制
- 多用户并发消息（共享 session 下的 steer 行为）

> **Phase 1 完成状态**：所有功能均已实现并通过集成测试验证。Gateway 连接、消息收发、@mention 触发、Typing indicator、文件附件、访问控制全部就绪。

### Phase 2：增强交互（2 周）

| 功能 | 说明 |
|------|------|
| Thread 自动创建 | @mention 时自动创建 thread 隔离对话 |
| 频道配置 | ignored_channels / allowed_channels（Phase 1 已实现） |
| Channel prompts | 按频道注入不同的 system prompt（Phase 1 已实现） |

> **状态**：Phase 2 需求已收敛。Free-response channels、Typing indicator、角色权限、成员缓存等功能已在 Phase 1 中随核心实现一并完成。Thread 自动创建（`auto_thread`）默认关闭，如有需要可在配置中开启。

### Phase 3：Slash Commands + 组件（2-3 周）

| 功能 | 说明 |
|------|------|
| Slash Command 注册 | /model, /new, /mcp 等 Discord Application Commands ✅ |
| Slash Command Autocomplete | /model provider 输入时下拉选择可用模型 ✅ |
| 消息分段 | 超过 4000 字符的消息自动分段 ✅ |
| MEDIA 标签 | LLM 输出 `MEDIA:/path` 自动上传文件 ✅ |
| Embed 消息 | LLM 输出 `EMBED:title\|desc\|color` 渲染卡片 ✅ |
| AskUserQuestion 组件 | 按钮 / Select Menu 渲染 |
| InteractiveChannel 接口 | 实现 PresentQuestions() |

### Phase 4：高级特性（可选）

| 功能 | 说明 |
|------|------|
| Voice 语音消息 | STT 转写 + TTS 回复 |
| Voice 语音频道 | 加入频道、收听、回话 |
| Multi-account | 多个 Discord Bot 同时连接 |
| Channel prompts | 按频道注入不同的 system prompt |
| 定时通知 | home_channel 主动消息（cron 输出） |
| Forum 频道 | 论坛帖子创建与管理 |

---

## 12. 未解决问题

### 12.1 消息分段策略

Discord Bot 单条消息文本内容上限 **4000 字符**（2022 年底从 2000 提升至 4000）。当 agent 回复较长时，需要分段发送。若使用 Embed，文本 + Embed 字段总长度另有约束（Embed title 256 字符，description 4096 字符，field name 256 字符，field value 1024 字符，合计最多 6000 字符）。

**已实现**：在 `send.go` 中通过 `splitMessage()` 实现，优先在段落/换行处断句，无合适断点时在空格处切割，最后才硬切。同时 `sendTextWithMedia()` 支持 MEDIA 标签替换 + Embed 卡片发送。

### 12.2 交互组件超时

Discord 的交互组件（按钮、菜单）有 15 分钟的生命周期限制（3 秒内需 ACK，15 分钟内可更新）。AskUserQuestion 等待用户回复的时间可能超过 15 分钟。

**方案**：组件发送后，在 15 分钟前将按钮置灰并回退到文本等待模式（"请回复此消息来回答"）。

### 12.3 Rate Limiting

Discord API 有严格的频率限制。discordgo 内置了 rate limiter，但高并发回复（特别是带文件上传）可能触发全局限制。

**需要**：
- 在 send 路径上添加本地 rate limit 队列，**按 channelID 分桶**（Discord 的 rate limit 按 resource bucket 分配，不同频道通常不互相影响，全局队列会过度保守）
- 对上传操作（文件、图片）做更保守的限制

### 12.4 消息并发安全

discordgo 的 `AddHandler` 为每个事件在独立 goroutine 中执行 handler，因此 `MESSAGE_CREATE` 和 `INTERACTION_CREATE` 可能并发到达。

**方案**：tachi 的 Manager 层已有 per-thread 串行化保护（`threadActivation` 锁 + steer 机制）。共享 session 下，同一频道的并发消息会通过 steer 注入到进行中的 agent turn，而非并发执行两个 handler。但 channel 层自身的共享状态（如 memberCache、componentHandlers）仍需要使用 `sync.RWMutex` 保护。

### 12.5 消息历史与上下文窗口

Discord 频道可能有很多用户，每个 agent turn 都会消耗 token。主要开销来源：

- 配置文件附件
- 长对话 session

**方案**：
- 大文件附件不注入内容（只传路径）
- 利用 tachi 已有的 compact 功能压缩长 session

### 12.6 共享 session 消息归属

共享 session 下，LLM 收到的消息需要标注发送者（`[用户 A]: xxx`）。需要考虑：

- **显示名称 vs 用户名**：Discord 有 username 和 display name（频道昵称），使用哪个？
- **@mention 解析**：LLM 回复时如果需要 @某人，需要知道对方的 user ID
- **历史兼容**：切换 `groupSessionsPerUser` 配置后，已有 session 需要迁移吗？

**已实现**：在 `handler.go` 的 `buildIncomingMessage()` 中，Guild 频道的消息自动添加 `[发送者名]: ` 前缀。`Sender` 字段同时传入 manager 层供 ambient 格式化使用。切换 `groupSessionsPerUser` 配置后，已有 session 无需迁移——每次 turn 都会从当前消息重新提取发送者信息。

### 12.7 消息编辑处理

Discord 用户可编辑已发送的消息。如果 Bot 已回复编辑前的版本，需要处理 `MESSAGE_UPDATE` 事件吗？

**方案（MVP）**：忽略 `MESSAGE_UPDATE` 事件。handler 入口处的消息取 `MessageCreate`（不可变），编辑后的版本不会影响正在处理的 turn。编辑内容会在下次 @mention 或 free-response 触发时自然反映在频道上下文中。

> 边界情况：如果用户在 bot 处理其消息的过程中编辑了消息，bot 仍使用编辑前的内容。这是可接受的——用户可以在 bot 回复后再次编辑触发。较完善的方案可追踪 `MESSAGE_UPDATE` 并比较时间戳，超出 MVP 范围。

### 12.8 Slash Command 注册延迟

Discord 的全局 Application Commands 注册后最多有 1 小时缓存延迟。开发期间建议使用 Guild-level commands（即时生效）。

**方案**：通过配置项 `dev_guild_id` 指定开发服务器的 guild ID，当此项非空时注册 guild-level commands（即时生效）；否则注册 global commands（最多 1 小时缓存延迟）。

```yaml
# config.yaml
channel:
  channels:
    discord:
      dev_guild_id: "987654321098765432"  # 非空时使用 guild-level commands
```

---

## 附录 A：文件结构

```
channel/discord/
├── channel.go          # Channel 接口实现 + init() 注册
├── config.go           # DiscordConfig 结构体
├── gateway.go          # Gateway 连接与事件分发
├── handler.go          # 消息事件处理器 (MESSAGE_CREATE)
├── interaction.go      # 交互事件处理器 (INTERACTION_CREATE)
├── mention.go          # @mention 检测与路由策略
├── dm.go               # 私信路由
├── send.go             # 消息发送
├── attachment.go       # 文件附件下载/上传/缓存
├── feedback.go         # Typing indicator
├── auth.go             # 访问控制 (user/role allowlist)
├── member_cache.go     # 成员信息缓存
├── components.go       # 交互组件构建与回调管理
├── split.go            # 长消息分段
└── discord_test.go     # 单元测试
```

## 附录 B：discordgo 关键 API 参考

```go
// 创建 session
sess, err := discordgo.New("Bot " + token)

// Gateway 连接
err = sess.Open()

// 注册事件处理器
sess.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
    // m.Message 包含内容、附件、作者等信息
})

// 注册交互处理器
sess.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
    // i.Interaction 包含组件交互数据
})

// 发送消息
sess.ChannelMessageSend(channelID, content)

// 发送带附件的消息
sess.ChannelFileSend(channelID, fileName, reader)

// 发送复杂消息（含组件）
sess.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
    Content:    "Hello",
    Components: []discordgo.MessageComponent{...},
    Files:      []*discordgo.File{...},
})

// 发送 Typing indicator
sess.ChannelTyping(channelID)

// Gateway Intents 定义
sess.Identify.Intents = discordgo.IntentsGuildMessages |
    discordgo.IntentsDirectMessages |
    discordgo.IntentsMessageContent |
    discordgo.IntentsGuildMembers
```
