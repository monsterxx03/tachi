# 任务完成与唤醒（agent/tasks、各入口的 taskwake.go）

这份文件管：一个后台任务结束时，消息怎么到达模型。现状是**本机后台进程、子 agent 和远端委派**会走这条路；desktop、tui、channel 三个入口都能在空闲时被叫醒，`-p` 不能。MCP 工具还没有接进来（见文末「还没做的」）。

## 一条不变量：完成事件只有一个出口

`agent/tasks.Queue` 是每个**逻辑会话**一个的待投递集合。`Pending()` / `PendingFor(conversation)` 是取走式（取走即删除），所以两个读取方永远不会报告同一件事：

- **轮内**：`BackgroundTaskReminder` 在每次请求模型之前取走，作为 reminder 拼进消息。
- **空闲**：入口取走并开一轮新的。

每个事件都带 `Conversation`（生产者从 tool ctx 的 `LogicalConversation` 取）。desktop 和 channel 的 agent 各自只服务一个会话，过滤是恒等的；TUI 的 agent 是进程级、会话靠 `sm.Load` 切换，队列会同时装着几个会话的事件，所以它必须用 `PendingFor(当前会话)`。

`Restore()` 只在「取走了但没能交付」时用（会话忙、入口不会开轮），放回队首。`Post` 和 `Restore` 都会敲一下 `Signals()`，消费方因此不用轮询。

## 生产者：只有自然退出才算新闻

`tools.ProcessManager` 在进程**自己结束**时投递事件，然后把它从跟踪表里删掉——事件就是快照（名字、状态、退出码、输出尾部），finished 的进程留在表里会让 `list_bg` 越积越多。

- **被 `stop_name` 停掉的进程不投递**：调用方要的就是它停，再报一次是噪音。`stopProcess` 先 CAS 到 killed，退出协程的 CAS 就输了。
- 事件在唤醒等待方被叫醒**之前**投递（`setTerminal` 的 `beforeClose`），所以谁看到「已结束」，谁就能看到表和队列里的一致状态。
- `BindSink` 是**先绑先生效**：fork 出来的子 agent 共享父的 manager，不能把父对话的消息挪到子 agent 那个一次性的队列上。

## 子 agent：同一套出口

`SubAgent` 在**能唤醒的入口**（desktop / tui / channel）上是派发即返回：工具调用立刻给出任务 id，子 agent 在后台跑完时投一个 `KindSubagent` 事件，走的是和后台进程完全相同的队列与退出规则。

- **后台执行必须脱离这一轮的取消**（`context.WithoutCancel` 保留 working dir / session / usage kind）：子 agent 属于会话，不属于轮次。用户按停止，它继续跑（然后被「停止按住唤醒」挡住，直到用户再开口）。
- **每轮的实时转发通道要摘掉**（`tools.WithoutSubagentEventSink`）：那个 sink 写的是这一轮的 event channel，轮结束就被关闭，后台子 agent 往上写会 panic。异步子 agent 的进度看它自己的会话记录。
- **id 按调用携带**（`WithSubagentMetaCarrier`），不是工具上的「最后一个」字段：同一批可以并行派多个子 agent，共享字段会把 B 的 id 链到 A 的会话记录上。
- **顺序活由模型自己声明**：`wait=true` 让这一次调用阻塞到子 agent 结束（和以前一样，用这一轮的生命周期，用户按停止会连它一起取消）；不写才走后台。这个字段只在异步入口的 schema 里出现——阻塞入口本来就等，给它一个「等不等」的开关是在描述一个它没有的选择。
- **不能唤醒的入口保持同步**（`AgentConfig.WakeCapable=false`，今天只有 `-p`）：派发即返回会让结果在没人会读的时候到达；`-p` 更糟，进程直接退出。

## 远端任务：同一个出口，靠轮询发现

`node/delegationwatch.go` 的 `Sender.PollDelegations` 每 20 秒问一次所有未结算的委派：有 `RemoteTaskID`、`LastKnown.State` 不是终态、最近 24 小时内有观察。**只有状态变了才报告**：

- 完成 / 失败 / 取消 → `KindRemote` 事件。`Name` 是本地委派 id（`d_…`，模型手里的就是它），带上节点名和结果文本。
- `input_required` / `auth_required` → `StatusWaiting`，带 `question_id`。等回答不是「结束」，但一个空闲的会话必须知道——否则没人能回答它。渲染时直接给出回答的调用形状（`RemoteTask(action="reply", …)`）。
- 对端不可达不是状态变化：记录保留上次看到的状态，下一个 tick 再试。

**谁开这个轮询**：入口能唤醒才开（`DelegatorCache.SetTaskSink`；desktop、tui 各自拥有缓存时都设了，channel 在独立进程里拥有缓存时也设）。事件只投给**本进程真的工作过**的会话（有队列的那些）：委派记录在 `~/.tachi` 下被所有入口共享，一个会话可能正被另一个进程驱动，从这边唤醒它就是同一份会话起第二轮。没打开过的会话，下一轮靠 `RemoteTasksReminder` 照样能看到。

**合并宿主（headless）只有一份缓存**，sink 也只有一个：desktop 的 sink 先问 channel 的 `PostTask`——这段会话归 channel 就交给它，否则再走 desktop 自己的队列。两个运行时同时写一份会话是 channel_mirror 专门防的错误。

## 队列归入口，不归 agent

- **desktop**：agent 按会话懒建、可能被回收，队列由 `desktopApp` 持有，按 `LogicalConversationID` 索引（压缩换了叶子会话，键不变），作为 `AgentConfig.TaskQueue` 注入。
- **tui**：一个 agent 跨所有会话，队列就挂在 agent 上；事件靠 `Conversation` 区分，读取按当前会话过滤。
- **channel**：agent 按线程缓存、compact 轮还会建临时 agent，所以队列由 `Manager` 按**线程**持有（`taskQueues`），共享的 `ProcessManager` 的 sink 在 `NewWith` 里一次绑定到路由器，事件按会话找到线程。`prepareThreadSession` 记录 logical → thread 的映射——没有它，一个线程的后台进程会报进另一个线程的会话。

## 唤醒（desktop）

`desktop/taskwake.go`：

- **合并窗 800ms**：同时结束的一批任务只开一轮。窗口里再来事件就重置计时。
- **等空闲**：`waitForIdle` 解析当前叶子会话并等 `r.turnDone`；压缩后重新解析。忙时不拒绝事件，只等。
- **让给用户 250ms**：前端在 `agent:idle` 时会把用户在这一轮里排队的消息发出去；唤醒抢在前面会让那条消息被拒为「会话正在运行」。
- **不设次数上限**：该唤醒就唤醒。任务链（测试→修→再跑）是正常工作，掐断它比多花几轮更糟。每轮唤醒写一行 INFO（`task wake: continuing the conversation`），失控可以事后看出来。
- **唯一的刹车是用户的停止**：一轮因为用户按停止而结束时，这个会话的唤醒被按住（`tasks.Queue.Pause`），直到用户下一条消息或一次 steer 才恢复。没有这条，不限次数的唤醒在「停」之后还会自己接着跑。注意 `Pause` 只按住**空闲唤醒**，轮内的提醒照常取事件——正在跑的一轮该知道的还是要让它知道。
- **唤醒轮就是普通一轮**：同一份历史、同一套工具、`laneTranscript`、接受 steer。区别只有通知：**成功不推送**（用户可能在睡觉，结果已经在会话里），**失败照推**（`handleEventIn` 里按 `run.wakeTurn` 判断）。
- 唤醒的消息里，子 agent 显示成「子任务 <id>」，后台进程显示成「<name>」，因为子 agent 的名字就是一个不透明的 id。
- **消息形状**：模型看到完整的 `<system-reminder>` 块（`systemreminder.TaskLines`，与轮内路径同一份渲染），会话里用 `DisplayContent` 存一行摘要（🔔 …）——抄的是 cron 的 `cronModelPrompt`/`cronDisplayText`，不新增消息类型。`ConvertSessionToLLMMessages` 靠**位置**区分两种提醒，加新类型是在给历史重建挖坑。
- **不触发记忆召回**：那块内容是任务结果，不是用户在问什么。
- **权限走交互那条路，但带一个上限**：唤醒轮用会话自己的策略，`ask` 照常弹卡；区别是这一次运行带 2 分钟的提问上限（`agent.WithAskTimeout`，设在 turn ctx 上）。超时后权限按**拒绝**处理（命令没有执行）、`AskUserQuestion` 返回错误，模型被要求把问题写进回复然后结束。普通轮没有这个上限——用户刚发消息，等他是应该的。上限跟着 RUN 走而不是跟着 agent 走，就是为了让同一个会话在两种轮次里行为不同。三个入口的唤醒轮都设了它；channel 是 `PermissionModeSkip`，本来就不会弹卡。

### 各入口怎么接

- **desktop**（`desktop/taskwake.go`）：等叶子会话的 `turnDone`，`beginTurnWith` 开一轮（`laneTranscript`）。成功不推送、失败照推；失败的通知按 `run.wakeTurn` 判断。「全停」是 `/stopall`：它由输入框**直接执行**（不走命令回合，否则会话一忙就会被拒），杀掉本会话所有后台进程并按住唤醒。
- **tui**（`tui/taskwake.go`）：watcher 在 `Run` 里启动，队列信号 → `tea.Msg` → 只有 `stateIdle` 才开一轮；忙/弹窗时事件留给下一轮的提醒。成功的唤醒轮不发终端通知（`wakeTurn`），失败照常。`Ctrl+C` 只停这一轮（不再杀后台进程）并按住唤醒，`Ctrl+X` 是「全停」，任何一次提交都会松开按住。
- **channel**（`channel/manager/taskwake.go`）：每个线程的队列自带 watcher，合并后走 `OnCronTrigger` 的现成机制（acquire agent、加载线程会话、跑一轮、投递）。合成的 job 只用来命名线程和文本，不会入队/落盘。`/stop` 按住该线程的唤醒，下一条入站消息松开。

## 在跑的任务：只在集合变化时提醒

`RunningTasksReminder` 把还没结束的后台进程告诉模型——忘了 `bg_name` 也还能 `wait_name`/`stop_name`。它按**会话**记住上一次说过的内容，集合不变就沉默：同一条列表每轮重发是白花的 token，而带运行时长会让文本每轮都变（prompt 缓存和 `freshReminders` 的去重都会因此失效）。

只列后台进程。运行中的子 agent 没有任何按名字的动作（没有 wait、没有 stop），给模型一个用不上的 handle 只是噪音；它的结束会照常作为任务事件到达。

## 入口能力决定工具行为

`-p` 跑完即退，没有「空闲」可言，所以**不能唤醒**。异步工具必须能读到这个能力并退回同步，否则进程退出时活还在跑——`SubAgent` 就是按它走的（`WakeCapable`），desktop / tui / channel 设 true，`-p` 保持阻塞。今天 `background=true` 在 `-p` 下还有这个洞（进程被留下），那是下一个要补的。

## 还没做的（设计见 `docs/2026-11-09-async-task-wakeup-design.md`）

- `KillAll` 的调用点按「谁有权放弃」重划（「全停」的入口已经有了：TUI 是 `Ctrl+X`，desktop 是 `/stopall`）。
- 异步子 agent 的实时进度不转发到父卡片（sink 被摘掉了，防止向已关闭的 channel 发送）；看进度去它自己的会话记录。
- MCP 工具异步化——先要有可靠的取消，现在只做了每次调用的超时（见 `docs/agents/mcp.md`）。
