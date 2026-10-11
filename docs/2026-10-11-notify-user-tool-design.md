# NotifyUser —— 通过 channel 主动私信用户的工具

> 设计文档，尚未实施。目标：让 agent 在**任何入口**都能主动给用户的 IM 发一条私信；挂载与否只由本地配置决定（有一条启用的 channel 声明自己可作为通知工具）。

---

## 目录

1. [目标与非目标](#1-目标与非目标)
2. [现状：决定这份设计的事实](#2-现状决定这份设计的事实)
3. [总览](#3-总览)
4. [接口](#4-接口)
5. [逐 channel 的实现](#5-逐-channel-的实现)
6. [配置](#6-配置)
7. [挂载点与生命周期](#7-挂载点与生命周期)
8. [工具契约](#8-工具契约)
9. [边界与陷阱](#9-边界与陷阱)
10. [影响面](#10-影响面)
11. [测试计划](#11-测试计划)
12. [已定的与未决的](#12-已定的与未决的)

---

## 1. 目标与非目标

**目标**

- agent 在 `tui` / `desktop` / `acp` / `-p` / `channel` 任何一个入口，都能主动把一条消息送到用户手上，不依赖「用户刚发来一条消息」。
- 挂载条件只有一条，且来自配置：存在一条**启用**的 channel，它在该 channel 自己的配置里被打开「可作为通知工具」的开关，并且它的实现实现了通知接口。
- 通知是**私信给用户本人**。寻址属于 channel，不外泄到工具或调用方。

**非目标**

- **不做收信。** 通知路径不建立接收循环、不消费入站消息——不与正在跑的 `tachi channel` 抢消息。
- **不做跨进程投递。** 把消息交给另一个进程里活着的 channel 需要 IPC，而 `docs/2026-05-08-channel-tcp-remote.md` 里的通道协议**尚未实施**。
- **不取代 cron 的投递。** `deliverCronResponse` 解决的是「任务跑完把结果送回它自己的线程」，决策在 `judgeNotify`；NotifyUser 解决的是「agent 想找用户」。两者可以共用 channel 的发送实现，但决策层不同，不要把 `notify_gate` 的逻辑搬过来。
- **不做队列与重试。** 一次调用一次尝试（多通道时依次尝试），失败如实报给模型。

---

## 2. 现状：决定这份设计的事实

| 事实 | 位置 | 为什么关键 |
| --- | --- | --- |
| 活的 channel 实例只有 `tachi channel` 进程里有 | `main_channels.go`（全仓唯一的 `channelmgr.New`） | 其他入口要用 channel，只能自己按配置构造一个 |
| 构造 channel **不**连接，连接发生在 `Run()` | `channel/discord/channel.go`（`Run` 里才 `discordgo.New`+`Open`）、`channel/weixin/channel.go`（`loadOrLogin`） | 「按需连接」可行：构造不产生网络副作用 |
| discord 的 REST 发送**不需要**网关 | `discordgo.New` 只建 `http.Client`；`ChannelMessageSend`/`UserChannelCreate` 走 `RequestWithBucketID`→`RequestRaw`，其中没有 session 检查 | 只发实例不必 `Open()`：不会多一条网关在线、不抢事件 |
| weixin 的发送不需要登录往返，只依赖磁盘凭据 | `loadOrLogin` 读 `store.loadAccount`；`sendTextReply(userID, "", text)`（启动问候就是这么发的） | TUI 进程也能发；无凭据时给一条点明原因的错 |
| 发送路径**不写** store | 只有轮询会 `saveContextToken`（`channel/weixin/polling.go:134`） | 两个进程共用同一个 `state_dir` 不会互相覆盖 |
| 每个 channel 自己知道 DM 怎么寻址 | weixin `accountID:userID`、discord `dm:<userID>`（`channel/discord/dm.go`） | 接口只留一个方法，寻址不外泄 |
| 「用户是谁」的来源 | weixin：登录者即 `ch.userID`；discord：`allowed_users` | 决定通知目标从哪来 |
| 工具挂载点在 `RegisterTools`，只被 `Configure` 和 `ReloadTools` 调用 | `agent/agent_configure.go:104`、`agent/agent_reload.go:92` | 裸 agent（fork / 子代理 / dream / github）用 `SkipConfigure`，天然拿不到这个工具 |
| `ReloadTools` 只加不减，失去依据的工具要显式卸载 | `agent/agent_reload.go`（WebSearch 就是那个先例） | 配置关掉开关后必须把工具摘掉，否则留一个必然失败的工具 |
| node 远程轮次用 `remoteTaskTools` 白名单 | `node/runner.go`（`remoteTaskTools`） | 远程任务默认不带这个工具；实施后把它加进了白名单（见 §7） |
| desktop 设置表把 `channel` 标成 `applyUnused`（"desktop 不启用"） | `desktop/settings.go:601` | 这条说法在本设计下不再成立，必须同轮改掉 |
| in-process 通知的第一个先例是 cron | `channel/manager/cron.go` | 投递形态可参考，但决策层不同（见非目标） |

---

## 3. 总览

```
                配置（唯一的挂载依据）
   channel.channels.<name>.notify: true  +  实例实现 NotificationChannel
                            │
                            ▼
   agent.RegisterTools ──► pkg/channel.NotifiersFromConfig(ActiveChannels())
                            │        每命中一个就构造一个**只发不收**的实例
                            ▼
                     tools.NotifyUserTool(NotifySink)
                            │
        agent 在任意入口调用 NotifyUser(message)
                            ▼
            NotifierSet.Notify ──► 按名字升序，逐个尝试
                            ▼
   discord: UserChannelCreate(user) → ChannelMessageSend(dmChannel, text)
   weixin : sendTextReply(userID, contextToken, text)
                            ▼
                        用户的私信
```

一句话：**开关在配置里，能力在接口里，寻址在 channel 里，投递复用各自的 `Send` 实现，挂载点在 `RegisterTools`。**

---

## 4. 接口

新增 `pkg/channel/notify.go`。放这里的理由：仓库里所有可选的 channel 能力接口（`InteractiveChannel`、`ToolProvider`、`MessageSender`…）都在 `pkg/channel`，而 channel 注册表也在这里。

```go
// NotifyConfigKey 是每个 channel 配置里那个「可作为通知工具」的开关。
// 它是通用键（不在任何 channel 的私有配置结构里），由本包唯一读一次。
const NotifyConfigKey = "notify"

// NotificationChannel 由「能主动给用户发私信」的 channel 实现。
//
// 实现必须做到「只发不收」：不启动接收循环、不消费入站消息、不建常驻 goroutine
// —— 因为挂载它的进程未必是那个正在跑完整 channel 的进程。
type NotificationChannel interface {
	Channel

	// NotifyUser 把 text 送到本 channel 上用户的私信会话。
	//
	// 寻址属于 channel：只有它知道私信在本平台上是哪个 thread、用户是谁。
	// 没有可用发送能力时（凭据缺失、用户不在共同服务器…）返回错误，绝不假装成功。
	// 必须可并发调用。
	NotifyUser(ctx context.Context, text string) error
}

// NotifyRequested 读那个开关。键不在配置里 = 不打开。
func NotifyRequested(rawCfg map[string]any) bool

// NotifierSet 是配置里所有「开关打开且实现接口」的 channel，按名字升序持有。
type NotifierSet struct{ targets []notifierTarget }

func (s *NotifierSet) Empty() bool
func (s *NotifierSet) Channels() []string                                  // 按名字升序
func (s *NotifierSet) Notify(ctx context.Context, text string) error      // 逐个尝试，全失败则报齐原因

// NotifiersFromConfig 按配置解析可用的通知通道。
//
// raw 就是 config.ChannelConfig.ActiveChannels() 的返回值（channel 名 → 该 channel 的原始配置）。
// 只构造实例：不连接、不发消息。开关开着但工厂不存在 / 工厂报错 / 实例没实现接口，
// 都只是跳过并记一条日志（缺了通知能力不该让 agent 起不来）。
func NotifiersFromConfig(raw map[string]map[string]any, log *logger.Logger) *NotifierSet
```

三处刻意的选择：

- **参数是 `map[string]map[string]any`，不是 `*config.ChannelConfig`。** 直接 import `config` 其实不会成环（它已经躺在 `pkg/channel` 的依赖闭包里），所以这是刻意的选择而不是被迫的：`Factory` 的签名本来就是这份原始 map，解析器停在同一个契约上，`pkg/channel` 的直接依赖就只多一个 `pkg/logger`（也已在闭包里）。调用方 `agent.RegisterTools` 手里本来就有 `FullConfig`，把 `ActiveChannels()` 的结果递进来是一行。
- **工具不直接持有 `*NotifierSet`。** `pkg/channel` 已经 import `agent/tools`（`ToolProvider.ChannelTools() []tools.Tool`），反向 import 就是循环。按 `SendFileCallback` 的先例，工具只认自己包里定义的窄接口，`*NotifierSet` 天然满足：

  ```go
  // agent/tools/notifyuser.go
  type NotifySink interface {
      Channels() []string
      Notify(ctx context.Context, text string) error
  }
  ```

- **只有一个方法，不是 `Prepare` + `Target` + `MessageSender` 三件套。** 解析目标会碰网络（discord 的 `UserChannelCreate`）或磁盘（weixin 的凭据），把它拆成公开的两步会让调用方必须记住顺序。发送的实现复用 channel 私有的 `sendText` / `sendTextReply`，去重发生在那里，而不是在接口上。

---

## 5. 逐 channel 的实现

### weixin（必须实现）

`channel/weixin/notify.go`：

```go
func (ch *Channel) NotifyUser(ctx context.Context, text string) error
```

- **目标**：`ch.userID`（登录者本人，也就是扫码的那个人）。私信 thread = `accountID:userID`。
- **两种状态下都要能用**：
  - 进程里已经有活实例（channel 模式）：`accountID`/`userID`/`botToken`/`cli` 都就位，直接发。
  - 只发实例（TUI/desktop/acp/-p）：从磁盘读凭据——把 `loadOrLogin` 里"读已存账号"的那一半抽成一个不加网络、不弹二维码的辅助方法（`loadStoredAccount`），两条路径共用；`Run` 保留 QR 兜底。
- **绝不**调用 `qrLogin`（那是往终端打二维码的交互流程），也**绝不**发 `notifyStart`/`notifyStop`（那是收信会话的生命周期）。
- `contextToken` 用 store 里的（可能为空 —— 启动问候就是这么发的）。

### discord（必须实现）

`channel/discord/notify.go`：

```go
func (ch *DiscordChannel) NotifyUser(ctx context.Context, text string) error
```

- **目标**：`cfg.AllowedUsers[0]` —— 第一个被允许的用户就是机器人的主人。显式走 `UserChannelCreate(userID)` 拿到 DM channel ID，再 `ChannelMessageSend`；不要赌"把 user ID 当 channel ID 直接发"这个 API 捷径。
- **连接**：已有 session（channel 模式）就直接用；否则 `discordgo.New("Bot "+token)` —— 到此为止，**不 `Open()`**。没有网关就没有 presence、没有事件消费。
- **分片与 2000 字上限**：走既有的 `sendText`，规则与正常回复完全一致。
- **没配 `allowed_users` 又无法判定主人** → `NotifyUser` 报一条点明配置项的错误（而不是猜一个目标）。
- **不发频道。** `docs/2026-07-11-discord-channel-design.md` §Phase 4 设计过 `home_channel`（"主动消息发送频道（cron/通知）"），但字段从未落地、当前会被静默忽略。它的语义是发到**频道**，与这个工具"私信本人"的定位不同，所以不接进来：目标就是 `allowed_users[0]`。

### github / mock（不实现）

- **github**：它的"用户"是 PR 作者，"通知"是平台内的评论，没有私信概念 —— 不实现接口，因此永远不会成为通知通道。
- **mock**：什么都不连（`channel/mock/mock.go` 的定位就是"让一个无头进程活着承载 node"）—— 同样不实现。

---

## 6. 配置

```yaml
channel:
  channels:
    weixin:
      enabled: true
      notify: true                             # ← 可作为通知工具
    discord:
      enabled: true
      notify: true
      allowed_users: ["123456789012345678"]    # 私信目标取第一个
```

- 开关名 `notify`，位置在**该 channel 自己的配置子树**里，由 `pkg/channel` 唯一读一次（`NotifyRequested`）。不做第二处读取、不复制默认值。
- **两个条件都要满足**：开关打开**且**实例实现 `NotificationChannel`。只满足一个都不挂载，并各记一条日志说明原因（开关开了但接口没实现，多半是版本不匹配：配置比二进制新）。
- **legacy weixin 的第二处**：`channel.weixin.enabled` 走的是类型化字段，`ActiveChannels()` 会为它合成一份 map（`config/config.go` 的 `ActiveChannels`）。所以 `WeixinConfig` 要加 `Notify bool \`yaml:"notify"\``，且合成 map 时必须带上它 —— 否则 `channel.weixin.notify: true` 这一路会被静默忽略。
- **顺序必须确定**：`ActiveChannels()` 返回的是 map，迭代顺序随机。不排序的话，「第一个能成功的通道」和工具描述里的通道名都会随进程变化 —— `NotifiersFromConfig` 按名字升序排列，`Channels()` 也按升序返回。
- **开关存在的意义**：能力（接口）与政策（开关）分开。一个**能**通知的 channel 不一定**该**被 agent 拿去打扰用户；同一个 channel 也可能同时被 cron 等其他路径使用，用户需要一个独立的拒绝方式。

---

## 7. 挂载点与生命周期

挂载写在 `agent.RegisterTools`，与 WebSearch"只在配置成立时才注册"同一个形状：

```go
if a.Config.FullConfig != nil {
    set := channel.NotifiersFromConfig(
        a.Config.FullConfig.Channel.ActiveChannels(), a.Config.Logger)
    if !set.Empty() {
        a.Config.ToolRegistry.Register(tools.NewNotifyUserTool(set))
    }
}
```

### 各入口的覆盖

| 入口 | 有工具？ | 说明 |
| --- | --- | --- |
| tui（`main_agent.go:147`） | 有 | 长任务跑完时通知不在电脑前的人 —— 这是这个工具的主要用途 |
| desktop（`desktop/agent_driver.go:258`） | 有 | 同上 |
| acp（`agent/acp/agent.go`） | 有 | 同上 |
| `-p` / `tachi commit` / `tachi review` | 有 | 一次性短进程，模型基本不会用；代价是每次请求多一段 schema。按"只看配置"一条规则，不为它们加特例 —— 也不引入 `AgentConfig.DisableNotifications` 这类前端开关，否则挂载条件就有了第二个来源 |
| channel（`channel/manager/agent_cache.go:99`） | 有 | 群聊线程里私信本人。**用配置派生的独立实例**，不复用 manager 的活实例（见未决问题 1） |
| node 远程轮次（`node/runner.go`） | 有，且可见 | `remoteTaskTools` 含它（实施后加的）；但**能不能用仍取决于节点那台机器的配置**——白名单是天花板，不是授予 |
| fork / 子代理 / dream / github pr | 没有 | `SkipConfigure`，`RegisterTools` 根本不被调用 |
| `tachi web` | 没有 | 只读会话文件，不跑 agent |

### 生命周期

- **构造没有副作用**（不连接、不建 goroutine、不发消息），所以 `RegisterTools` 每次重建都安全，配置没变时重建出来的实例等价。
- **工具不持有需要关闭的资源。** 这是「只发不收」的推论，也是必须守住的不变量：一旦某个 channel 的 `NotifyUser` 需要常驻 goroutine 或长连接，这里就得多一条 Close 路径，而目前五条入口没有一条会去关它。
- **`ReloadTools` 必须显式卸载。** `RegisterTools` 只加不减，配置把开关关掉（或 channel 被禁用）之后工具还留着，就是一个"调用必然失败的工具"——按 WebSearch 的先例，在 `ReloadTools` 里判断不再有依据就 `UnregisterTool(tools.ToolNameNotifyUser)`。
- **desktop 设置表的 `channel` 行已经改过**（实施时定的做法与这里最初的推荐不同，理由留在这）：原本写 `applyUnused` / "channel 模式的 IM 后端，desktop 不启用"，而本设计下 desktop **会**读 `channel.channels.*.notify` 来决定挂不挂工具，这条说法成了假话。但**不能改成 `applyPush`**：那一档意味着表单提供这个键，而 channel 名是用户自定的 map 键、`config.PathsFor` 枚举不出来，页面画不出可用的控件（改档还会触发"offered 就必须有 Category"这条守卫）。所以：Mode 留在 `applyUnused`（它管的是**表单提供不提供**），Note 改成事实，并把 `resolveBatch` 的拒绝句从"桌面不读它"改成"页面不提供"——那句概括现在只为 `channel` 一行是假的。

---

## 8. 工具契约

```
NotifyUser(message: string) → string
```

- **名字**：`NotifyUser`（常量 `tools.ToolNameNotifyUser`），与 `SendFile` 同族的主动投递工具。
- **描述（给模型看）**：主动给用户发一条 IM 私信。用于"结果要送到人手上，而当前这轮不一定有人看着"的场景：长任务跑完、需要人做决定、定时之外的一次性提醒。**不是**用来回答当前这条消息的（那走正常回复）。可用的通道由部署配置决定，并且可能一个都没有。
- **参数**：只有 `message`（不加 channel 参数：多通道时按名字升序尝试第一个能成功的）。
- `Parallel(): false`，与 `SendFile` 一致 —— 两次投递的先后就是模型调用的先后。
- **成功**：`✅ 已通过 <channel> 通知用户`。**失败**：把尝试过的每个通道和各自的原因列出来（"weixin: 没有已存的登录凭据；discord: 用户不与机器人在同一服务器"），不吞错、不合并成一句"失败了"。
- **并发**：可能被并行子工具调用（`attachmentSink` 的注释记着同样的前提），`NotifierSet.Notify` 与各实现的懒初始化都要自己上锁。
- `arg_summary.go` 加两条 case（`ToolArgsSummary` / `ToolArgsTitle` → `message` 截断），让 TUI / desktop 的工具卡片显示通知正文。

---

## 9. 边界与陷阱

1. **顺序不确定。** 见 §6：`ActiveChannels()` 是 map。排序不是风格问题，是"同一份配置必须构造出同一份行为与同一段 schema"。
2. **两个进程、同一个账号。** 只发实例不 `Open()` 网关、不轮询、不写 store，所以不抢消息、不覆盖凭据。唯一待验证的是 weixin：只发实例没有走过 `notifyStart`，服务端是否要求先声明一次会话**需要在真机上验证一次**；若不通过，就让 channel 模式改用活实例（未决问题 1）。
3. **配置说可以，实际发不出去。** 凭据缺失、用户不在共同服务器、平台限流 —— 一律作为工具失败返回给模型，绝不静默成功。挂载时刻意不连接，所以"到底能不能发"只能在第一次调用时才知道：这是取舍（挂载不带网络副作用），不是遗漏。
4. **weixin 没有已存凭据**：报错，绝不触发 QR 登录。
5. **分片与过滤跟随既有实现。** discord 的 2000 字分片、weixin 的 markdown 过滤都复用各自的 `sendText` / `sendTextReply`，不在工具层另立一套规则。
6. **提示词缓存。** 工具列表一变，整段消息历史从头未命中（`docs/agents/prompt-cache.md`）。所以工具描述里的通道名必须来自排序后的集合，保证同一份配置构造出逐字节相同的 schema；配置变了换一次前缀，与反复开关 WebSearch 是同一个代价，可接受但要说明。
7. **channel 模式里"用户的私信"可能就是当前线程。** 在 DM 线程里调用等于又发一条。工具描述里点明"回答当前对话用正常回复"，但**不做去重**：判定"当前线程是不是该用户的 DM"要靠各平台的 thread 命名规则，得不偿失。
8. **正文会落进对方的 IM。** 描述里提醒不要放敏感内容；本设计不额外过滤 —— 与正常回复是同一条信任边界，另立一套只会制造不一致。

---

## 10. 影响面

**新增**

- `pkg/channel/notify.go`、`pkg/channel/notify_test.go`
- `agent/tools/notifyuser.go`、`agent/tools/notifyuser_test.go`
- `channel/weixin/notify.go`、`channel/discord/notify.go`

**修改**

- `agent/tools/tool.go`：`ToolNameNotifyUser` 常量
- `agent/tools/arg_summary.go`：两处 case
- `agent/agent.go`：`RegisterTools` 里的挂载
- `agent/agent_reload.go`：`ReloadTools` 里的显式卸载
- `config/config.go`：`WeixinConfig.Notify` + `ActiveChannels()` 合成 map 带上它
- `desktop/settings.go`：`channel` 行的 Mode 与 Note
- `docs/agents/channel.md`：新增一节（现状文档，实施时必须同一轮更新）
- `docs/agents/desktop.md`：设置表那条说法

---

## 11. 测试计划

| 测试 | 钉住 |
| --- | --- |
| `pkg/channel/notify_test.go` | 开关 off → 空集合；开关 on 但接口没实现 → 空集合（并记日志）；工厂报错 → 跳过；多条通道**按名字升序**；第一条失败时试第二条；全失败时错误里带齐每个通道的原因 |
| `agent/tools/notifyuser_test.go` | 没有 sink → 报错；sink 失败 → 报错文本原样带出；成功文本；`message` 为空 → 报错 |
| `agent/register_tools_test.go` 同处 | 配置能通知 → 工具在；配置不能 → 不在；`ReloadTools` 在配置失去依据后把它摘掉 |
| itest（可选） | 注册一个假 channel + 配置开开关，断言 `api_requests.jsonl` 的 `tools` 数组里有 `NotifyUser` |

假 channel 的写法沿用 `pkg/channel/registry_test.go`：`Register("testnotify", factory)`，测试间用 `cleanupRegistry()`（`Register` 对重名 panic，所以每个用例的名字要唯一）。

---

## 12. 已定的与未决的

**已定**

- **`-p` / `tachi commit` / `tachi review` 不摘掉这个工具。** 挂载条件只有"配置"一个来源，不引入 `AgentConfig.DisableNotifications` 这类前端开关。代价是多一段 schema，可接受。
- **discord 的目标就是 `allowed_users[0]`（私信）。** 不接 `home_channel`（见 §5）。
- **node 的远程轮次也允许它。** `remoteTaskTools` 里加了一行（`node/runner.go`），测试 `TestARemoteTurnIsOfferedTheHostsNotifyTool` 把两个方向都钉住：宿主配置说得通时工具出现在那一轮里，说不通时不出现。

**未决**

1. **channel 模式要不要复用 manager 的活实例？** 复用能省一次构造，也绕开 weixin 的 `notifyStart` 疑问；代价是多一个注入点（`AgentConfig` 字段，或 `buildAgent` 里按 AskUser / channel tools 的既有形状重注册一次）。若 §9.2 的真机验证不通过，这个就从"可选"变成"必须"。
2. **要不要更多参数？** 当前只有 `message`。`title` / `priority`（比如"这条必须看到"）留待有真实需求时再加 —— 参数越多，模型选错的余地越大。
