# Tachi —— Channel 模式手册

Channel 模式是 agent 在没人看着的地方跑的一种入口模式：消息从 IM 后端进来，一个 `*Manager` 判断它是什么意思，答案再发出去。它的不变量大多关于**活过一轮的状态**——按线程的 agent、缓存下来的 ambient 消息、被搁置的 AskUser 问题——这种状态一旦被粗心的改动弄坏，没有哪个单轮测试会发现。

## 目录

| 章节 | 什么时候读 |
| --- | --- |
| **接线：谁造什么** | 你碰启动、channel 注册，或 `MessageHandler` 时 |
| **按线程的 agent 池** | 你碰 agent 缓存、淘汰、`/new`、`/model`、`/cd`，或任何关于加锁顺序的东西时 |
| **agent 之外、按线程的状态** | 你新增按线程存活的状态，或碰 whisper / AskUser / steer 时 |
| **Ambient（whisper）路由** | 你碰群聊处理、沉默，或批处理窗口时 |
| **斜杠命令** | 你新增命令、改分发，或碰 `/compact` 时 |
| **一次性命令（`/commit`、`/review`）** | 你碰侧信道运行或它们的取消时 |
| **cron 通知（`when_relevant`）** | 你碰一个跑完的 cron 任务投递什么，或 `[SILENT]` 标记时 |
| **channel 作为通知工具（NotifyUser）** | 你碰 `pkg/channel/notify.go`、`channel/*/notify.go`、`NotifyUser` 工具的挂载，或改一个 channel 的配置结构时 |

## 接线：谁造什么

- **一个 `Manager` 服务所有 channel。** 造它的有两个入口：`main_channels.go`（`channelmgr.New(cfg)`）和无头宿主（`channelmgr.NewWith(cfg, deps)`，见下一条和 [desktop.md](desktop.md) 的无头宿主）。配置启用的每个 channel 由**同一个**函数加进来（`Manager.AddEnabled`，`manager.go` 里紧挨 `Add` 的那个：注册表里找不到的名字作为「missing」返回，工厂报错带上 channel 名），`channel.Register` 写在 `channel/discord`、`weixin` 和 `channel/mock` 里——最后那个是惰性的那个，用例点名它来拿一个能承载 node 的进程：[itest.md](itest.md)。然后调 `mgr.Start(ctx)`。manager 给每个 channel 只有一个 handler——`buildHandler()`（`agent_turn.go`）——所以产品里每一个路由决策都活在那个闭包后面。它是一串很短的决策，四个，每个都是自己的函数：`routeGroupMessage`（ambient 还是丢弃）、`classifyTurnInput`（是哪种轮次，还是一条同步命令）、`activateDirectedTurn` + `joinOrStartTurn`（认领线程，然后加入一轮正在跑的轮次或起一轮）、`replyForTurnResult`（构造回复，收尾 `/compact`）。
- **会话记录上写着「这段对话属于哪个 channel」**（`Session.ChannelName`，`turn_helpers.go` 的 `recordThreadChannel`）：本进程的 thread→channel 映射是由进来的消息建的，重启之后是空的，而手机要靠这个名字给对话打 tag。写在这里是因为每条 channel 轮次都从这里过、手里正好有会话；名字没变就不写（`UpdatedAt` 不白动），不知道 channel 就什么都不写。
- **同一段对话可以有第二个门，但写的人只有一个**（`SubmitTurn`，`external.go`）。手机读着一段 channel 的会话时，它发来的文字被交回**这个** manager —— 同一份按线程缓存的历史、同一个 agent、同一把线程锁、同一份权限策略，事件通过 `TurnObserver` 交给那边投影。被抽出来的 `handleMessage(ctx, msg, turnOptions)` 就是这件事的地基：渠道自己的消息和外来消息走同一段代码，差别只在 `turnOptions`（外来：不往渠道发进度、同步命令拒绝 `ErrExternalCommand`）。**反过来不做**：线程与会话一对一（`session.ThreadID`），channel 没有「打开某一段会话」这个动作。（`Dependencies`，`NewWith`）：发送侧（`DelegatorCache`）和无头宿主共用一个——两个各自建就是一个进程两个发现浏览器、两份账本，而进程级那条 gossip 路由只会从最后登记的那个答复；**借来的不归 `Close` 管**（`ownsDelegator`：`DelegatorCache.Close` 是终态，关掉它宿主剩下的轮次就再没有委派）。记忆整理同一个道理：宿主已经在跑，manager 就不注册（`SkipDream`）。定时任务不用分享——两个调度器读同一个 `crons.json`，`Owns` 只认自己的目标类型（`channel` / `desktop`），`cron.Store` 每次操作都读盘加锁，所以谁也不会跑别人的任务、也不会看到对方写之前的旧状态。
- **handler 对每个 channel 是同步的。** `channel.Channel.Run` 调用它并等 `HandlerResult`；想要并发的 channel 自己起 goroutine，但 manager 已经把 agent 轮次跑在一个 goroutine 上、并把 handler 阻塞在一个结果 channel 上，所以一轮慢的轮次不会永远卡住 channel 的读循环——它只卡住那个线程的下一条消息。
- **`PermissionModeSkip` 一路到底**：channel 的 agent 会**放行**一个 bash `ask`，而不是把它搁置——控制台前没人，所以一条要人回答的规则得到的回答是「跑吧」（`.tachi.md` 的入口模式表；`deny` 仍然说了算）。两个例外都来自 *channel 自己*的能力，而不是 manager 的选择：
  - 对非交互式 channel，`AskUserQuestion` 被注销；对实现了 `InteractiveChannel` 且 `Interactive() == true` 的 channel，它在 `buildAgent` 里被**重新注册**。`PermissionModeSkip` 设上、`AskUserEnabled` 为 false 时 `NewAIAgentWithConfig` 会把它丢掉，所以正是这次重新注册让这个工具存在——不是走过场。
  - 实现了 `ToolProvider` / `ThreadReminderChannel` 的 channel 在构建时（`buildAgent`）把自己拥有的工具和一条按线程的提醒注入缓存 agent，所以这两样天生就是按线程的。

## 按线程的 agent 池

**每个 ThreadID 一个长命的 `*AIAgent`**（`agent_pool.go`），于是那些值得跨轮次留着的状态——skill 激活、MCP 已发现集合、提醒节奏——就这样活着跨过轮次，同时线程之间保持隔离。agent 在线程的第一条消息上惰性构建，并在 provider 变了或该线程被淘汰（`/new`、`/model`、一次压缩的收尾）时重建。

**三把守卫，没有一把嵌套在另一把里。** 这就是要守住的不变量：

| 守卫 | 保护 | 持有多久 |
| --- | --- | --- |
| `agentPool.mu` | 只管那张 map | 微秒级：map 读/写，绝不跨进对 agent 的一次调用 |
| `cachedAgent.mu` | 那个线程的轮次（把它们串行化；`release` 结束该轮） | 一整轮 |
| `cachedAgent.evicted`（atomic） | 「这个槽位已死」 | 在 agent 被关闭之前设上，由等待者读取 |

**持有一把锁时不得再取另一把。** 把两把嵌套起来就是 ABBA 死锁：一边正拿着线程 T 的槽位锁在 acquire，另一边在淘汰 T，两边都挂住。正是那个 atomic 的 `evicted` 标志，让等待者不必碰 map 就能拒掉一个注定要死的槽位，所以两个方向都不需要嵌套。`TestAgentPool_ConcurrentAcquireEvictDoesNotDeadlock` 钉住它——嵌套一旦回来，它几秒内就失败。

**其余的都是从这里推出来的：**

- **同一条 goroutine 上，一次借用必须在一次淘汰之前释放。** `acquireAgent` 持有槽位锁，`evictAgent` 等它——跨 goroutine 时这是对的（正在跑的轮次不会被硬拽走），在一条 goroutine 内则是自我死锁。`/compact` 的收尾路径是唯一必须既借用又淘汰的调用方，所以它在一个作用域里先释放、再淘汰；`compact_deadlock_test.go` 就是那个回归测试。
- **`agent_pool.go` 之外没有任何东西碰这个池的守卫。** 调用方用 `slot`（记账式读取，绝不创建；一个死条目读作不存在）、`ensure`（不存在或已死就创建；它的闭包不得回调进 Manager——先把值造好，那把短锁才不会变成一个顺序问题）、`acquire`/`release`、`evict`/`evictAll`、`each`（先快照，再往外调用——一次慢的 `ReloadSkills` 不得占着 map）、`count`，以及 `withAgent`（在槽位锁下对一个线程的 agent 跑一个回调——一条斜杠命令够到一个 agent 走的就是它，而不是自己拿 `ca.mu`、自创一套顺序）。
- **一次失败的构建留下一块墓碑**：槽位被标记为死，并留在 map 里，直到该线程的下一个调用方替换它（把它删掉，就等于持有槽位锁时去拿 map 锁）。它在这期间是**不可见的**——`slot` 和 `ensure` 把一个被淘汰的条目当作不存在，所以 `/cd` 会创建一个活槽位，而不是把一个目录写进死槽位；`getThreadWorkDir` 会回退到会话，而不是拿一个永远没人会用的值作答（`TestAgentPool_TombstonesAreInvisible`）。一个已经有 agent 的槽位永远不会原地变成墓碑：detach、`evictAll` 和 `acquire` 的 phase 1 都在标记它的同一个临界区里把它删掉，而删掉它的那一方拥有关闭权——哪怕它已经离开 map、`evict` 再也找不到它（`TestAgentPool_ReplacedSlotClosesItsAgent`）。
- **`workDir` 是两个生命周期都会写的唯一字段**，所以它自带一把叶子锁（`cachedAgent.WorkDir`/`SetWorkDir`），而不是用轮次锁：一轮在开头读它，并可能从会话里持久化的值把它写回，而 `/cd`——一条不得等一整轮跑完的无状态命令——直接写它。用轮次锁守它，`/cd` 就要阻塞一整轮；用 map 锁守它，就是拿一把为保护 map 而存在的锁去守别的东西。

## agent 之外、按线程的状态

**一张表，一把锁**（`thread_state.go`）：`threadTable.byID[threadID] → *threadState`，装着

- **`ch`**——拥有该线程的 channel，好让 `buildAgent` 能问对的那个 channel 要交互性、工具和提醒。由按 channel 的 handler 包装器在第一条消息上写一次。
- **`defaults`**——来自平台元数据（比如一条群公告）的 workdir/provider 默认值，在创建一个**全新**会话时应用。空字段回退到通常的默认值。
- **`turn`**——那个**活着**的轮次：steer 队列 + steer 响应 channel、handler 阻塞在其上的结果 channel、cancel 函数、`isCompact`、AskUser 状态，以及 whisper 的 ambient 状态。由 `beginTurn` 创建，由 `endTurn` 丢掉。

**一条记录，一把叶子锁，它的方法从不往外调用：**

- **表锁是一把叶子锁**：持有它时不取任何东西，它的方法也从不往外调用。`beginTurn` 的 `make` 回调之所以只做分配，就是这个原因——里面一次 provider 查找会嵌套进 thread-defaults 锁。
- **第二条消息会加入正在跑的那一轮**（`beginTurn` 返回那个活着的 activation）；调用方读 activation 自己的标记来区分「是我起的」和「我在 steer 它」。两个对立的 activation 意味着一个线程有两套 cancel、两个 steer 队列、两个 AskUser 槽位。
- **`endTurn` 只清掉它自己的那个 activation**（指针相等）。一条在 `/stop` 拿走该轮之后才完成的 handler goroutine，不得丢掉下一条消息装上的 activation——`TestThreadTable_EndTurnOnlyClearsItsOwnTurn`。`takeTurn` 把 activation 交出来，于是**取消**发生在表锁之外：`cancel()` 收拾那个轮次 goroutine。
- **只读查找从不创建记录**：`channelFor` / `defaults` / `turn` 正是无状态命令和 ambient 定时器会在一个可能从没见过的线程上调用的东西。
- **轮次的内部自带一把锁**（`threadActivation.mu`）——那个对象与一条正在跑的 goroutine 共享，所以它不是记录的一个字段。`cancelled` 是一个 atomic：`/stop` 从命令 goroutine 写它，轮次 goroutine 读它。
- **`oneoffCancels`**（Mutex）与 **`oneoffSem`**（Semaphore）保持分开：一次性运行既没有 activation 也没有 agent 槽位，所以它们不是线程记录的一部分。

一项新的按线程需求，加进 `threadState`；等出现第五张 map 的时候，就是停下来问为什么的时候。

## Ambient（whisper）路由

一个功能，三个入口点——改动其中任何一个，都得考虑另外两个：

1. **`routeGroupMessage`**，由 `buildHandler`（`agent_turn.go`）最先调用，判断一条消息到底算不算 ambient：非指向 + 群聊 + whisper 开启 → `handleAmbientMessage`；非指向 + 群聊 + whisper 关闭 → 静默丢弃（永远不会变成一轮 agent 轮次）。
2. **`handleAmbientMessage`**（同一个文件）把消息缓存在线程 activation 上，并（重新）装上批处理定时器；而一条指向消息会取消待处理的 ambient 轮次，把缓存的消息作为 steer 上下文转过去，并重置沉默退避。
3. **`ambient.go`** 跑这一批：`flushAmbientBatch` → `runAmbientTurn` → `[SILENT]` 判定，其连续计数驱动指数级的批处理窗口（`ambientBatchWindow`）。

回复本身就是那一轮的主要产物，所以任何挪动沉默判定的改动（比如在生成之前加一道门禁），也会挪动退避计数器是在哪里被喂进去的。

## 斜杠命令

三个地方，必须彼此一致：

- **`agent/commands` 是注册表**（名字、描述、模式、输入提示）——只是元数据。未知命令的帮助列的就是它，前端也从它构建自己的补全。
- **`executeSlashCommand`（`commands.go`）是分发**——一个 switch，每个命令一个 case，全都通过 `textHandlerResult` 返回 `(text, error)`。
- **`/compact` 和 skill 激活是特殊的**：`classifyTurnInput`——`buildHandler` 做的第二个决策——把它们送上 **agent 轮次**那条路（它们需要活着的会话上下文），所以它们永远到不了那个 switch；其余每个命令都由 `executeSlashCommand` 同步作答。

`TestEveryChannelCommandIsHandled`（`agent_pool_test.go`）把这三者绑在一起：一个为 channel 模式声明的命令，若既没有 case、也没有一条被记录在案的特殊路径，构建就会失败。这是一项源码检查——逐个调用每个 handler 会要求每个命令都有一个活着的会话，还会跑起那些有破坏性的命令（`/restart` 调 systemctl，`/new` 结束一段对话）。

## 一次性命令（`/commit`、`/review`）

它们不是轮次：不创建 `threadActivation`，不碰 agent 缓存，并被记录在 `oneoffCancels` 里，好让 `/stop` 和 `/new` 能取消它们。`oneoffSem` 封顶全局同时能跑几个（一阵 `/review N` 否则会撑爆 provider 配额——每次运行都是一次完整 fork），拿不到槽位的命令会被**拒绝**并给出提示，而不是静默排队。

## cron 通知（`when_relevant`）

**调度归属先说清楚：channel 的 `Scheduler` 只 `Owns` `TargetType == "channel"`（空也算老任务），desktop 的任务它既不建定时项也不触发。** 两个进程共享同一个 `crons.json`，所以 `cron.Store` 的写路径带一把跨进程文件锁（`pkg/flock`）；`CronTool` 也不再知道 channel，它通过一个 `tools.CronTarget` 绑定当前线程，list 的范围和 create 写下的目标在同一处声明。**任务的归属是「这个群的 cron home」，不是当前会话**：实现了 `channel.CronThreadChannel` 的通道（wave 把 `…:topic:…` 抬到 `wave:group:<chat_id>`）给出 home，`Manager.cronScope` 用这一个判断同时喂给斜杠命令和工具（看不见 = 不存在），于是话题里建的任务在主区和那个话题里都看得见、管得了；`CronTarget.ID` 仍是真实线程，`CreatedBy` 因此保留「它是在哪个话题里被建的」这条审计线索。没实现该接口的通道、以及查不到归属线程时，home 就是它自己——与话题出现之前的行为一致。marker 政策的文字搬到了 `cron.MarkerNotificationPolicy`（desktop 也用它），channel 只决定「这一次用门禁那套还是 marker 那套」。

**一个跑完的任务的结果，靠 `judgeNotify`（`notify_gate.go`）里的一个决策抵达 channel——而不是靠碰巧投递它的那个分支。** 每一次 cron 运行都经过它，`notify=always` 也在内，所以这条策略只有一个家。它决定的是：

| 情形 | 结果 | 决定者 |
| --- | --- | --- |
| `notify=always` | 投递报告（轮次失败时什么都不投） | `always` |
| `when_relevant`，什么都没报 | 抑制，不调用 | `none` |
| `when_relevant`，报告**就是** `[SILENT]` | 抑制，不调用 | `marker` |
| `when_relevant`，判定 p ≥ `cron.jev_threshold` | 投递，剥掉开头标记 | `jev` |
| `when_relevant`，判定 p < 阈值 | 抑制 | `jev` |
| 没有门禁（matcher 为 `marker`，或没有可用的 `jev` 块），或没有判定（出错、超时、未作答） | `cron.Job.ShouldSuppressResult`——那个标记 | `marker` |

关于它有四件事很容易被无意中弄反：

- **追加的提示词跟着决定者走；用哪一段由 `buildCronPrompt` 决定。** 门禁在作答期间，agent 被告知要**报告**（`gateNotificationPolicy`）——一个已经把一个发现判为不值得提的 agent，交给门禁去判的东西就是零，而这种预过滤恰恰是「开市了就总结」这类任务活不下来的原因（用例集里的 `market-open-flat` 就是那个任务）。门禁缺席、或已停止作答（`degraded`，每次判定重新算）期间，agent 拿回它**原来**的指令（`cron.MarkerNotificationPolicy`，桌面也共用的那一段），因为兜底是一个标记，而一个标记必须被点名要求。不要把两段文字合并，也不要丢掉第二段：一个没有 `jev` 块的安装，必须表现得同门禁存在之前一模一样。
- **快路径是刻意比兜底更紧的。** `isMarkerOnly` 问的是报告*是不是*那个标记；兜底仍用 `ShouldSuppressResult` 那套宽松的 `strings.Contains`。不要把两者「统一」：那套宽松读法正是门禁要替换掉的行为，兜底必须是旧的那套，否则一个降级的服务会悄悄改变「被抑制」的含义。
- **「没有判定」意味着标记，绝不是「投递」。** 这与 MCP 工具搜索那道门禁相反——那里缺失答案意味着「不要提供」，因为代价是工具列表被塞满，而这里的代价是一条被吞掉的通知。
- **失败的轮次和任何别的轮次一样被判定**，而且状态里带着这个失败，因为轮次出错正是用户想听到的事。兜底在它上面保持沉默（那是门禁之前的行为，而且失败的运行归调度器：它记下状态并重试）。所以门禁改变了失败运行的行为，这就是它被放在 `jev` 块后面、作为可选项的原因。

每次调用留一行 Info（`channel: cron notify judged`，概率在最后）和一行 `llm.UsageKindJev` 账本记录，经由 `llm.RecordJevUsage`——与工具搜索用的是同一个写入者，所以一道门禁无法把自己的成本变得不可见。问题、状态和阈值的出处属于 `channel/manager/notifygate` 和 [jev.md](jev.md)；碰那个问题之前先读后者。

## channel 作为通知工具（NotifyUser）

**agent 在任何一个入口（tui / desktop / acp / `-p` / channel）都能主动给用户发一条 IM 私信** —— 不是回答某条消息，而是「结果要送到人手上」。三条事实撑起这条路，改其中任何一条都要重新读这一节：

- **挂载条件是三件事的合取，只有一处判定**：channel 在 `ActiveChannels()` 里（即启用）、它自己的配置子树里 `notify: true`、并且工厂造出的实例实现了 `channel.NotificationChannel`。判定在 `pkg/channel/notify.go` 的 `NotifiersFromConfig`，**挂载点在 `agent.RegisterTools`**（所以 `SkipConfigure` 的裸 agent——fork / 子代理 / dream——天然没有这个工具），**卸载点在 `ReloadTools`**：`RegisterTools` 只加不减，开关关掉后不显式 `UnregisterTool` 就会留一个必然失败的工具（与 WebSearch 同一形状）。
- **解析要构造实例，但构造不连接**：这是整条路的前提（channel 在 `Run()` 才连）。所以挂载不带网络副作用，代价是「到底能不能发」只有第一次调用才知道 —— 于是失败一律从工具冒泡（没有目标、平台拒绝、无凭据），绝不静默成功。工厂拒绝配置 / 名字没注册 / 接口没实现，都只是跳过 + 一条 warning：缺一个通知通道不该让 agent 起不来。
- **通道顺序按名字升序，不许依赖 map 顺序**：`ActiveChannels()` 是 map，不排序的话「第一个能成功的通道」和工具 schema 里的通道名都会随进程变（工具在最前缀，见 [prompt-cache.md](prompt-cache.md)）。

每个实现的形状是「只发不收」，这条不是风格而是被调用方强加的：**挂载它的进程未必是那个正在跑完整 channel 的进程，而且没有人会去 Close 它**。所以 ——

| channel | 只发实例做什么 | 不做什么 |
| --- | --- | --- |
| weixin（`channel/weixin/notify.go`） | 从 state dir 现读凭据，**自己的 `client`**，发给登录者本人（`ch.userID`） | 不碰 `ch.cli` / `ch.accountID`（`Run` 会写它们 → 数据竞争）、不 QR 登录、不发 `notifyStart`/`notifyStop`、不轮询 |
| discord（`channel/discord/notify.go`） | 已经有 session（channel 模式）就用它，否则 `discordgo.New` 建一个**不 `Open()`** 的 REST-only session；目标是 `allowed_users[0]` 的私信，先 `UserChannelCreate` 再发 | 不开网关（无 presence、不消费事件）；不猜收件人 —— 没配 `allowed_users` 就报错 |
| mock | 不实现接口 | 永远不是通知通道 |

两个容易踩的坑：**discord 的 `sendTextWithReference` 在没有 session 时是静默返回 nil**（回复路径要这个行为，通知路径绝对不能要——那会把没发出去说成发出去了），所以通知走 `sendTextOn(sess, …)` 那个显式接受 session 的版本；**legacy weixin 路径（`channel.weixin.enabled`）的 `notify` 开关必须出现在 `ActiveChannels()` 合成的 map 里**（`config/config.go`），否则那一行配置会被静默忽略。

没接进来的是这一处：**发到频道而不是私信**（discord 设计文档里的 `home_channel`）没有实现，字段当前被忽略 —— 通知只走私信。**node 的远程轮次**则是接进来的（`remoteTaskTools` 里有一行），但它同样受挂载条件约束：节点那台机器自己的配置里没有 `notify: true` 的 channel 时，工具照样不会出现在那一轮里 —— 白名单是天花板，不是一个授予。

## 测试

```sh
go test -race ./channel/manager/     # the package is written for -race; the field-level races it covers are real
```

| 测试 | 钉住 |
| --- | --- |
| `agent_pool_test.go` `TestAgentPool_ConcurrentAcquireEvictDoesNotDeadlock` | 这个池不嵌套任何守卫（嵌套一旦回来，几秒内就失败） |
| `agent_pool_test.go` `TestAgentPool_EachDoesNotHoldThePoolLock` | `each` 在 map 锁已释放的情况下往外调用 |
| `agent_pool_test.go` `TestAgentPool_TombstonesAreInvisible` | 一个死槽位读作不存在，所以 `/cd` 落在活的那个上 |
| `agent_pool_test.go` `TestAgentPool_ReplacedSlotClosesItsAgent` | 被 acquire 替换掉的槽位仍会被关闭（它已离开 map） |
| `agent_pool_test.go` `TestEveryChannelCommandIsHandled` | 注册表 ↔ 分发 ↔ 特殊路径 |
| `compact_deadlock_test.go` | 先借用再淘汰必须先释放；淘汰会等一个被持有的 agent |
| `usage_race_test.go` | `/usage` 在一轮正在跑时读它的 agent |
| `agent_cache_test.go` | workDir 的优先级（缓存 agent 胜过持久化的会话） |
| `askuser_regression_test.go` `TestHandler_AskUserAnswerRouting` | 一条消息回答被搁置的 AskUser 等待的三种方式（结构化 / 原始文本兜底 / 取消）抵达那个等待，且永不变成一轮 |
| `notify_gate_test.go` | 通知决策的顺序和方向：always 任务从不被判定、那条紧的快路径、「没有判定」意味着标记、投递前剥掉开头标记 |
| `pkg/channel/notify_test.go` | 挂载判定的合取（开关 off / 没实现接口 / 工厂报错 / 名字没注册都为空集）、通道按名字升序、第一条失败时试第二条、全失败时错误里带齐每个原因 |
| `channel/weixin/notify_test.go` | 没有已存登录是拒绝而不是 QR 登录；真发一次（走 httptest）落在登录者身上、带凭据、只碰 sendmessage（没有 getupdates / notifystart）；平台拒绝要冒泡 |
| `channel/discord/notify_test.go` | DM 两跳的顺序（先开 DM 再发）、平台拒绝冒泡、没配 `allowed_users` 时一个请求都不发、只发 session 不 `Open()` 且被复用 |
| `agent/register_notify_test.go` | 挂载的四个分支 + `ReloadTools` 在开关关掉后把工具摘掉（再打开又回来） |
