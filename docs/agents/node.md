# Tachi —— 节点手册

`node/` 是唯一一处请求会从没人看着的机器上到达、跑一轮真 agent turn、还必须扛住调用方挂断的地方——所以它的不变量讲的是边界得活过什么：一次断开、一轮停在问题上的 turn、一次压缩、一次 `kill -9`、第二个进程启动。这份文件管它，也管任何会跟另一台机器说话的工具或提醒。

## 目录

| 章节 | 什么时候读它 |
| --- | --- |
| **自己起还是附着，以及那两个监听器** | 你动启动、`node.lock`、`local.json`、握手或监听地址 |
| **收到的一轮请求 —— 顺序就是保证** | 你动 `runner.go`、`executor.go`、`tasks.go` 或受理层 |
| **跨两次调用的一轮（停放）** | 你动接收侧的 AskUser、`park.go`、`question_timeout` / `max_parked` 或取消 |
| **发送侧** | 你动 `remoteclient.go`、`peers.go`、`delegations.go`、`sender.go`、`delegatorcache.go` 或某个委派提醒 |
| **持有与对账** | 你动节点的持有、`session/retention.go` 或任何会话删除 |
| **塑造了上面一切的三个 SDK 事实** | 某个协议形状的东西失败，而代码看着没问题 |
| **故意还没做** | 你正要加调用方鉴权、一个关闭 context 的入口，或一个受管会话标签 |

## 自己起还是附着，以及那两个监听器

- **哪些入口才承载**：`StartOrAttachNode` 由 TUI、桌面和 channel 入口调用——没有别人。`-p` 故意从不碰节点服务（一次性的管道运行没有「谁在承载」这回事，也不该占住这台机器的身份），ACP 和 `web` 入口也不碰。所以一台没有终端的机器**只**能通过 channel 入口承载节点，而那个入口在没启用任何 channel 时拒绝启动：一个想不借任何 IM 账号就提供 A2A 的无头部署，需要一个惰性 channel 来点名（`channel/mock`，见 [itest.md](itest.md)）。
- **锁说了算，绝不是文件。** `StartOrAttach`（`StartOrAttachNode` 只是转发它）对 `<DataDir>/node.lock`（`<BaseDir>/node/`，模式 `0700`）取 `flock.TryAcquire`：成功 → `host`，`flock.ErrHeld` → `attach`。flock 是按打开的**文件描述**算的，所以同一个进程里两个描述符也会冲突——这正是并发启动恰好选出一个宿主的原因。永远不要删锁文件：下一个进程会去锁一个没人争的新 inode；关机时也不要删 `local.json`（它的注释写了为什么）——陈旧文件无害，因为每个读取方都会拿 `InstanceID` 与真正在应答的那个实例对照之后，才信任其中任何一个字段。
- **拿着锁却没有服务是 `ErrUnavailable`，不是接管。** `local.json`（原子、`0600`，里面有凭据：`instance_id` / `node_id` / `addr` / `pid` / `api_version` / `token` / `started_at`）只被**取锁失败**的进程读取，而且一个随后连不上服务的进程不得改写它。身份是**推导**出来的，从不随机：`NodeIDFor` = `sha256(pubkey)[:16]` 的十六进制，所以附着到错的数据目录是 `ErrIdentityMismatch`；另一个活着的实例是 `ErrInstanceChanged`；`APIVersion`（目前是 `1`）不符是 `ErrVersionMismatch`。只有 `ErrUnavailable` / `ErrUnreachable` / `ErrInstanceChanged` 会被重试——坏文件快速失败，而不是等满 `AttachTimeout`。
- **两个监听器、两个面，以及两条关于它们能绑到哪的「不同」规则。** 本地控制面是一条路由，`GET /local/v1/handshake`，用 `subtle.ConstantTimeCompare` 校验 Bearer；它返回身份，**从不返回 token**。它永远在 loopback 上（`checkLoopback`）——不是因为它没有凭据，而是因为它是这台机器上其他 Tachi 进程找到持锁那一个的途径：与别人无关，绑到别处只会凭空把那份凭据的端点交给网络。**`POST /local/v1/session:end` 是另一条路由**，它是机器主人自己停掉属于某个会话的一次执行（`runner.EndSessionWork`）的办法：它取一个**会话** id——读者手里有会话，从来不是 task id——通过绑定解析到那一轮活的 turn，并把任务解析为 `CANCELED` 带一个原因，因为被停掉的 turn 对自己没什么可说的（`nodeExecutor.publish`）。它和调用方毫无关系：调用方的取消是协议自己的事，走 A2A 面到达，归拥有那个任务的人管。这台机器上任何进程都用 `node.OpenLocalClient` 够到它，它读取已公布的东西，并证明应答来自同一个实例。远端 A2A 面是另一个监听器，有自己的 mux（`/message:send`、`/tasks/...`、well-known Card 路径）；它的地址是运维者给的（`node.listen`），而远端那个可以绑**任何**地址（`checkRemoteListen` 不再限制它），因为只要离开 loopback 它总是带客户端证书提供 TLS——所以调用方总被识别，至于谁能到达这个端口是网络的事。
- **证书就是节点身份，两个方向都是。** 远端面只要不是 loopback 就用 TLS 提供服务——**地址**就是那个开关，一个表达式，所以「加密」「要求客户端证书」「把身份带进请求」永远不会互相矛盾（`startA2ASurface`）——用的是从 `identity.json` 的 ed25519 密钥签出的自签证书（`node/tls.go`）。`NodeIDFor` 从同一把密钥推导节点 id，所以对端从证书里读到的 id 无法伪造：调用方可以整天生成密钥，也拿不出一个能推导出别人 id 的。调用方一侧记住它见过的东西，而不是信任两次——TOFU，在 `<BaseDir>/peer-identities.json`，第一把密钥被大声记为 UNVERIFIED，之后一次用不同密钥的握手会被拒，并点名那个文件。（所以第一次接触的可信度只等于到达它的那条路：一个会认证设备的网格，或 loopback。这就是那笔交易，也是那条记录写着 `first_seen` 的原因。）
- **这个节点识别调用方；它不授权它们，而这是一个决定而不是缺口。** 任何客户端证书都被接受（`serverTLSConfig`）：`checkRemoteListen` 不再限制地址，所以「谁可以调用」是网络的事——一个网格、一道防火墙，或 loopback。仍然保证的是没有东西是匿名的：握手**要求**证书，没有它就没有身份，也没有「谁问的」的记录。重新加回白名单是一件小而刻意的动作，就做在原来 `node.callers` 在的地方，而 `TestAnyCertificateIsAccepted` 摆在那儿就是为了它不能不小心发生。
- **身份走一个方向，「标签」走另一个，而记录把两者分开。** `CallInterceptor` 把证书的节点 id 写进 `CallContext.User`——必须是 interceptor 而不是 middleware，因为 REST 传输每个请求都建一个**全新的** `CallContext`（`a2asrv.NewCallContext` 会遮蔽任何之前挂上去的东西），所以在请求处理器外面设的 User 会被丢掉。`callerFrom`——任务记录、去重键、context 归属检查、List 的过滤、Get 的归属规则——是那一个值的下游，且未变。调用方自己的**名字**（`node.name`）以私有头 `X-Tachi-Node-Name` 进来，归档为 `taskRecord.SelfReportedName`：**任何密钥都能声称任何名字**，所以这个字段自己的名字已经说明了它是什么，而真正的身份在它旁边——那是密钥说了算的。一个请求不能给自己的身份命名（其中没有任何东西能到达 `CallerID`），而没有证书的连接会被拒。
- **在 `host()` 里顺序是锁 → 本地监听器 → store → 远端面 → 公布**，而 store 必须先加载完并完成对账，远端面才能接受工作；每个失败路径都释放锁。关机时先把监听器放倒，**再**释放锁，否则一个发现锁空了的调用方会和仍在应答的实例赛跑。

## 收到的一轮请求 —— 顺序就是保证

- **受理跑在任何副作用之前。** 链条是：处理器前面的 `busyGuard` → `admission.Before` interceptor（对调用方/消息 id/摘要取指纹；重放把**已存下的**任务作为结果返回）→ 任务记录落盘 → 执行器产出 `SUBMITTED`+`WORKING` → `beginTurn` → `contextRun`（绑定或创建会话）→ 持有 → 这一轮的 ctx → `LoadSessionHistory` / `RunConversationStream`。在那一点之前的任何东西都不得写会话、构造提醒、生成标题、建检查点、触发 hook 或调用模型。本地一侧是对称的：`RunConversationStream` / `RunOneOffStream` 在 goroutine 起来之前就拒绝受管会话，而 `managedSessionWriteRefusal` 覆盖 rewind，因为 rewind 没有可信执行授权。
- **我们的拒绝由我们自己渲染。** `google.rpc.Status` 加 `ErrorInfo{domain: "tachi", reason, metadata}`——因为 SDK 会把无法识别的错误在协议 domain 下映射成 `500 INTERNAL_ERROR`，那会把一个私有原因呈现成标准原因。这些 reason 就是调用方据以匹配的词表：`CONTEXT_BUSY`、`MESSAGE_ID_REUSED`、`CONTEXT_PARKED`、`TASK_BUSY`、`TASK_FINISHED`、`UNKNOWN_TASK`、`NO_WAITING_QUESTION`、`QUESTION_MISMATCH`、`ANSWER_SHAPE`。
- **停放不是 busy。** `INPUT_REQUIRED` / `AUTH_REQUIRED` 被排除在「busy」之外：发给那个 context 的下一条消息恰恰就是它在等的答案，把它算成 busy 就会把调用方挡在自己刚被问到的问题外面。带 `taskId` 的消息同样跳过创建去重——它什么都不创建。
- **记录在响应之前就在磁盘上，而索引从不做决定。** `<DataDir>/tasks/<id>.json` 是权威（`putLocked` = 原子 `writeRecord` → map → `reindex` → `writeIndex`）；`index.json` 是派生物，靠扫描重建，所以丢了它损失的只是速度，不是正确性。去重键是消息 id（`CallerID` 填上之后调用方也算进去——这就是为什么含 `|` 的调用方身份在写入时被拒）；同一个 id 配同样的摘要是返回**第一个**任务且什么都不跑，而同一个 id 内容不同则被 `MESSAGE_ID_REUSED` 拒绝，而不是悄悄把旧任务交回去。
- **启动恢复是一种状态，不是一次重写。** `resolveInterrupted` 把每个非终态任务——运行中和停放中的都一样——用 `interruptReason` 判失败，并把问题保留在状态里。崩溃的宿主不会恢复工作，也没有任何东西假装它恢复了。
- **远端的一轮跑的配置比本地「窄」，而收窄只在一个函数里。** agent 构造时带 `SkipMemoryRecall`（召回提醒会把**这台机器**的记忆注入任何到来的提示词，而只跳过工具拦不住它），并且这次 run 的工具视图丢掉 `hostOnlyTools`（`RecordMemory` / `MemoryRecall`），无论 `node.tools` 怎么说——运维者把它们列出来也打不开宿主的记忆给调用方（§2.3）。两半都在 `node/runner.go`；一次 reload 两半都撤不掉（召回标志是 `ReloadMemory` 唯一保留的那部分 `MemoryState`，而工具视图是按轮的）。
- **一轮做出的每次模型调用都能归因到引起它的那个任务。** `llm.WithUsageSource` 把来源（`a2a`）连同任务和 context id 放到这一轮的 context 上，所以账本行带着**谁的**工作，而 `Kind` 继续说这次调用**为了什么**（§9.5）——远端一轮里的压缩仍然是一行 `compact`，两个和（「这个远端任务花了多少」/「这台机器在起标题上花了多少」）都仍然答得出来。这条和上面的收窄都在 `turnContext` / `toolViewOption` 里：各一处，因为它们就是远端一轮与本地一轮不同的两种方式。

## 跨两次调用的一轮（停放）

- **执行器在 `INPUT_REQUIRED` 上返回；活的 turn 留在我们这一侧。** SDK 把那个状态当作终态，停止读事件流，所以阻塞的 `message/send` 到那里就结束。带 `taskId` 到达的消息重新进入执行器（`continueTask`）并读**同一个** turn 的流。没有别的东西会结束一轮停放的 turn——不是 HTTP 请求，也不是执行器。
- **turn 的 ctx 不能来自请求。** SDK 在 errgroup 里跑执行器，执行器一返回就取消那个 ctx——恰恰是一轮停放的 turn 必须继续活着的时刻。turn 派生 `context.WithoutCancel(parent)`（保留 trace/调用方的值，丢掉寿命）；turn 自己、取消或节点关闭决定它何时结束。
- **问题 id 就是 AskUserQuestion 工具调用的 id**，公布在状态消息的 `metadata.questionId` 里；答案在同一个键下回来，id 缺失或不对会被拒，并把当前那个交回去，好让调用方重试而不是猜。形状：一个待答问题 → 一个文本
  part；多个 → 一个以**完整问题原文**为键的 `data` part（TUI 和桌面用的同一形状），而无法映射的答案会被拒——绝不按位置匹配。
- **槽恰好被消费一次，而应答通道在公布之前先被清空。** agent 的应答通道只缓冲一个值、不带请求 id，所以残留值会被当成下一个问题的答案；节点在公布之前丢弃它（`DiscardPendingAskUserAnswer`），并用会返回「是否送达」的 `TryRespondToAskUser` 知道答案真的被接走了。`parking.take` 发生在唤醒之前，所以两个答案不可能都声称自己答上了那个问题。
- **两个上限都是配置，而两者都不拒绝新工作。** `question_timeout`（默认 30m）结束这一轮并把任务写成 `FAILED`，问题保留——`INPUT_REQUIRED` 的意思是「本进程有一个 turn 在等这个答案」，到点之后这句话不再为真。`max_parked`（默认 16）判**那次提问**失败：模型被告知问不出去，并用手头的东西收尾。
- **流必须总被读完。** 一个活的 turn 由 `lt.advance()` 读取，而 `drainIfIdle` 最多起一个后台读者，因为 loop 自己的会话写入和检查点发生在流的**末尾**。取消在产出 `CANCELED` **之前**就调用 `stopTurn`（对一轮停放的 turn 来说，那是唯一能结束它的东西），而 `stopTurn` → `lt.stop()` + `forgetTurn` 一起丢掉槽和任务持有。已知限制：当取消落在一轮停放的 turn 的收尾竞争窗口里时，SDK 自己的并发取消路径可能给调用方回「不可取消」，而这一侧其实已经停了它——SDK 行为，不在本侧修。

## 发送侧

- **那三条 REST 路由在这里逐条写明是故意的。** SDK 的 REST 客户端会**丢弃**任何 domain 不是 `a2a-protocol.org` 的 `google.rpc.ErrorInfo`，而我们的拒绝刻意归档在 `tachi` 下——所以通过 SDK 读它们的调用方分不清「被拒了，可以安全重试」和「任务已经结束」。因此发送方自己讲协议（`/message:send`、`/tasks/<id>`、`/tasks/<id>:cancel`、媒体类型 `application/a2a+json`、`A2A-Version` 头），并自己解析 `google.rpc.Status` 信封，那样任何 domain 都留得住。
- **`PeerRefusal` 保住了 SDK 扔掉的那个区分。** `Transport`（拨号失败、超时、读不出 body）意味着对端根本没考虑过这个请求——「未知」不是「可以安全重试」。`Domain == "tachi"` 意味着对端的受理层答复了我们；协议 domain 意味着是别的东西答复的。`NotAccepted()` 只在 `CONTEXT_BUSY` / `MESSAGE_ID_REUSED`（或裸的 503/409）时为真。
- **一个答案是一个 Task 或一个 Message，而其中只有一个是把手。** `send` 返回一个覆盖 spec 的 `SendMessageResponse` oneOf 两臂的 `sendResult`，因为第二臂是合法答案（「对不需要任务跟踪的简单交互的直接 Message 响应」）——这个节点以前会拒掉它，那既丢掉了对端的文本，又让记录读起来像「我们不知道它有没有被接受」：一次成功交换的三种不同说法。直接 Message 记为 `DirectAnswer` + `completed` + 它的文本而 `RemoteTaskID` 为空（这样 `Get`/`Wait` 不用往返、直接从记录作答，也就是它们在「没什么可问对端」时本来就有的形状），而对端发来的 `contextId` 会被保留——正是它让 `continue_from` 即使没有任务也能继续那**段对话**，而 `referenceTaskIds` 保持为空（没有任务可引用）。**`noHandle` 在每一次拒绝里都把 `RemoteTaskID` 为空的两种原因分开**：「从未被接受，结果未知」不是「被直接答复了，没建任务」——一个说重试，另一个说已经做完了。只有桩才会对我们产出这种形状：一个 Tachi 对端总是用 Task 作答。
- **`SUBMITTED` 和 `WORKING` 都算「在飞」**，`reply` 也一样等。`returnImmediately` 响应常常在 SDK 写下 `WORKING` 之前就到达，所以把 `SUBMITTED` 当失败会重复发送；而作答会恢复同一个 turn，所以一个说 `WORKING` 的把手报的是一个还没人到达的状态。拿到把手和拿到结果是两个不同的动作，而第二个总是走同一次有界等待。等超时不会取消任务。
- **本地记录在请求发出去「之前」就写好了**（先 `put` 后 `send`），`LastKnown.State` 留作未指定：「我们不知道它有没有被接受」是记录被允许处于的状态，不是需要去猜的东西。对端的消息 id 被保留，所以重试是重放，不是第二个任务。未知 id 会被拒，并指向 `RemoteTask(action="list")`；`find` 从不猜一个附近的记录。
- **一条记录绑定一个「身份」，不是它当初在的那个别名**（§4.1）。`PeerNodeID` 由验证过的握手写入，而 `continue_from` 比较的**就是它**——所以改一个对端的名字就是改名，而把一个别名指到另一台机器会被拒，拒绝里点名两个节点 id。比较发生在消息离开**之前**（`verifyPeerIdentity` 为这个证据发一次 card 请求）：跑在之后的检查早就把错误的机器读进这段对话了。对于这台机器完全没有身份的对端（明文 loopback），别名仍是退路。两个值得知道的后果：显示用的名字来自**当前**配置（`Sender.displayName`，通过 TOFU 记录解析），因为一个用配置已经不认识的别名点名的提醒会把模型送进一次拒绝；而只要去掉身份检查，这些测试各自都会失败（套件钉住的是行为，不是代码形状）。
- **一次续跑可以完全不说对端**，而那正是这个工具产出的形状：它的契约恰好是 `node` / `continue_from` 之一（§8.1 自己的续跑例子就没有 `node`），所以记录属于哪台机器在这里解析，三步，最省的先来——pin 记录（`currentAliasFor`，不发请求）、记录当初在的那个别名（日常情况），然后**逐个问**每个已配置的对端它是哪个身份（`verifyPeerIdentity`，每个一次 card 请求）。第三步是唯一能在**改名**后活下来的一步：pin 是按对端当初被够到时的别名作键的，所以配置一分钟前才认下的名字在任何 pin 里都还没有，而旧别名已经没了——没有这一步那段对话就够不到，因为模型不得在 `continue_from` 旁边点一个节点名。身份优先也正是**顺序**之所以重要的原因：先问记下的别名会把改名变成「已经不再配置」，丢掉还在那儿的工作。身份比较本身（上面那条）仍然发生在消息离开之前——这一步只说明记录的对端在哪里够得到。
- **从对端回来的东西按「字符」设界，绝不切在字符中间——而装不下的那部分留在「这台机器」上。** `boundAnswer` 保留前 `answerExcerptLen` 个 rune（`strutil.TruncatePlain`），并把全文外溢到 `ToolResultsDir`（`remote_<local id>.txt`），所以被缩短的文本点出一个模型真能 `ReadFile` 的路径。头部被保留，不像 Bash /
  WebFetch / SubAgent 那样用自己的指针替换掉全部输出：远端答案的头几个字符通常就是答案，而同一段文本也正是提醒渲染的东西——那里放指针会让每个长答案都花一次 `ReadFile`，并让摘要变成一个文件路径。外溢文件名来自**本地记录**而不是时间戳（一个每轮都变的名字会给提醒塞进一行移动的文本，那是每轮都要付的提示词前缀开销：`docs/agents/prompt-cache.md`），而且这也让重复外溢覆盖掉自己的文件。外溢失败（没有目录、磁盘满、权限不足）回退到裸的摘录——一次失败的工具调用比一次答得少一点的更糟。**没有外溢，剩下的部分在任何代价下都够不到**：它在这里被解码一瞬然后丢掉，而对端自己那份又卡在同一个界内（一次 `get` 会再截一次）。这**不是**那件被禁止的事——把一个远端**绝对路径**当作本地可读的交出去；这个文件是副本，就在这台机器上，而说明也这么说。存下的请求（`promptExcerptLen`）遵循同一条按字符的规则；而声明的节点名按**字节**切（`strutil.TruncateBytes`），因为那里的上限真的是字节数。
- **空的 `LogicalID` 是一个「作用域」，不是通配符。** 它是无会话运行（`-p`）归档的地方，所以一个脚本后续的运行只看得到自己的委派，看不到任何真实会话的；`Sender.RemoteTasks` 总是按它过滤，而那个过滤是无条件的——没有它，一次 `list` 会显示这台机器跑过的每一个任务，那既没用，也是会话之间的泄漏。
- **委派器按入口各一份，对端集合变了就重建，而「失败」的打开不会被缓存**（瞬时问题会自愈）。缓存键是 name+URL+**note**，因为对不可达节点的一份陈旧描述比一次重建更糟。没有对端 → `nil` 委派器 → 没有工具也没有提醒。对端 Card 的查找连**失败**也缓存（5 分钟），所以一个宕掉的节点不会每轮都花一次超时，而够不到的对端是沉默而不是错误——调用方是在提醒里知道的。
- **门禁就是谁附着。** 两个委派工具都是 run 级附加件（`agent.WithExtraTools`），从不进注册表，所以 cron 和 ambient 结构上就够不到它们——没有需要保持同步的拒绝名单。四个入口会附着它们（TUI 用户轮、桌面用户轮、channel 定向轮、`-p`）；`/compact` 和一次性的 `--commit` / `--review` 运行不会。

## 持有与对账

- **只有一种持有，只有一个原因：有一次执行正在跑。** `reason=task`，没有期限，在终态被撤走。一段远端对话的会话**没有**自己的保留窗口——它按会话 store 的普通生命周期活着和死去（按数量的清理、置顶、一次显式删除），跟读者自己的一段对话一样。顺序是会话存在 → context 绑定写下 → 持有落盘 → loop 启动，所以一个输给清理的会话会可见地失败，而不是无保护地跑起来。
- **上一条规则，以及它为什么没了。** 一个 context 曾经从它最后一次使用起把会话持有 `node.context_retention`（168h），叠在任务的持有之上。它被去掉了：那个窗口在 A2A spec 里没有地位（`contextId` 只是一个不透明的分组 id，仅此而已），而且它让保留层成了会话 store 旁边的第二个时钟——「这东西活多久」有两个答案、删除规则有两套。代价：主人的删除现在是收回一段没人要的对话的唯一办法，而一个在清理之后回来的发送方会拿到一个显式失败（绝不悄悄开一段新对话），这个失败可能比以前来得更早。
- **结果不住在会话里。** 产物和任务状态在 `<DataDir>/tasks/` 下，所以一个被收回或被压缩的会话带不走节点仍承诺要提供的结果。
- **压缩是搬走保护，不是丢掉它。** `FinalizeCompact` 在旧会话自己的簿记被清掉**之前**通知宿主；节点把运行中任务的持有搬到新会话、把绑定落盘，然后才释放被搬走的那个 ref。
- **启动时对账残留**：每一条持有的任务已终态或已消失就释放，而带已退役的 `context` 原因（`session.ReasonContextRetired`——一段为保留窗口留着的对话，这个版本不做这件事）的持有也释放，所以一个旧版本写下的数据目录会收敛到仅存的那一个原因上。持有没有期限，所以这个 owner 是唯一能撤走一条残留持有的东西。代码从没听说过的原因不一样：**保留**并报告，因为释放它会丢掉另一个版本正持有的会话。
- **`Close` 等每一轮的流被读到末尾**（以 `shutdownGrace = 3s` 为界），因为那个末尾正是 loop 写自己的会话和检查点的地方。一次超时的排空是预期结果；关闭从不重写任务记录——下次启动把那些解析为中断。

## 塑造了上面一切的三个 SDK 事实

1. **两端的 `ErrorInfo` domain 过滤。** 客户端只保留 `a2a-protocol.org`；服务端把无法识别的错误在同一个 domain 下渲染成 `INTERNAL_ERROR`。因此：路由我们逐条自己写，而门禁自己渲染我们的拒绝。私有原因从不穿过 SDK 的错误映射。
2. **`INPUT_REQUIRED` 对 SDK 是终态**（它就此停止读事件流），而 SDK 自己的 `Terminal()` **不**包含它。节点的等待落在终态**或** `INPUT_REQUIRED` **或** `AUTH_REQUIRED` 上；执行器必须返回，停放的恢复才可能发生；而 `AUTH_REQUIRED` 完全不用于工具审批。
3. **任务内容在进 store 的路上会经 gob 克隆。** 只有内建类型或已注册类型可以待在 `any` 槽里——塞给它 `[]tools.Question` 会让**整个**事件以 `gob: type not registered for interface` 失败，任务最终是 `FAILED`。SDK 注册了 `map[string]any` 和 `[]any`，所以结构化那部分是用它们手工搭出来的。

## 故意还没做

每一条都是决定，不是疏忽——设计的次序是定死的，缺的只是接线：

- **用一个 `Message` 而不是 `Task` 回答一个简单的询问。** 接收侧总是先产出一个 Task，所以对端总能拿到一个把手。spec 给这个策略起了名字（**task-generating agents**：「总是用 `Task` 对象响应……这避开了 `Task` 对 `Message` 的抉择，但即使对简单交互也会造出已完成的任务对象」），而最后那半句就是这里的要点：记录**本身就是**那个功能——审计轨迹、保留期持有、用量归因、跨入口引用、调用方挂断后仍活着的答案，全都挂在这个任务上。三件事得一起改，而其中一件是硬规则而不是偏好：
  - **这个分叉在「第一次产出」时就提交了，没有回头路**（`internal/taskupdate`：`message not allowed after task was stored`，而在任务或消息之前任何事件都是 `first event must be a Task or a message`）。所以这个选择永远不可能跟着这一轮的结果走；它只能在 loop 跑起来之前、由请求和一条节点策略做出——是对「这份工作会不会保持简单」的**猜**。（协议论坛自己的混合例子用一个 router 来决定；它的「动态升级」变体把工作跟一个计时器赛跑，然后才产出两者之一——而那个窗口会阻塞调用方，因为那时什么都还没发出去。）
  - **一个 `Message` 不能提问**（`INPUT_REQUIRED`/`AUTH_REQUIRED` 是 Task 状态；spec 的
    它的分类法说只做消息的 agent「不能支持中断状态」），而那正是我们整个 §7.4 链条。
  - 一个有时用这个答、有时用那个答的节点，会让**每个调用方**都要分支，而只做消息的那一支既不能等、不能取消，也不能把答案送回去。
  所以做这件事的第三方对端是在**发送侧**被照顾的（见「发送侧」），而这个节点自己的答案形状保持单一。
- **一个显式的「关闭这个 context」入口。** 它的调用方会是「发送方说这段对话结束了」和「主人手动收拾」。**主人的那一半现在换了种方式被回答**——显式删除已经完全不再尊重一段对话，只尊重一次正在跑的执行，所以想让这个会话消失的读者就删掉它（`session/retention.go`）。这剩下**发送方**那一半：它需要一个对端能调用的东西，而现在这一半**更**要紧——没有保留窗口，这一侧没有任何东西会因为一段对话安静下来就收回它——它的会话等着普通的按数量清理。语义已经定死（拒绝新的续跑、有任务还在跑时拒绝、最后才释放）；缺的只是接线。
- **清理 context 绑定。** `<DataDir>/node/contexts/` 下的绑定只会被写——每个这个节点见过的 context 一个小 JSON，随对话增长，不随任何被收走的东西缩小。保留窗口也从没清理过它们，但它至少让你能说「活着的都在窗口里」。这个绑定值得留（正是它让 `continue_from` 认出自己那段对话，也正是它告诉一次删除这是谁的会话），所以修法是跟着会话走的清理，而不是设一个过期。
- **争用测试**（create/compact/delete 重入，以及节点在线时另一个进程来拿协调锁）。锁的顺序是定死的，写在注释和设计里；测试在等一个真实的并发场景来驱动它们。
- **文件 `Part`**（`raw` / `url`）和一个产物下载端点，带拉回方向需要的 SSRF 检查（§6.4）。`url` 是真的在等鉴权——一个下载把手必须带鉴权且会过期，而一个敞开的就等于敞开的 agent 端点，是同一种错误。
- **一个白名单，如果某个部署想要的话。** 今天任何证书都被接受，谁能到达这个端口由网络决定。想要更锋利规则的集群把它加回原来 `node.callers` 在的地方：握手已经交出一个节点 id，所以检查就是在 `verifyCallerCertificate` 里做一次成员判断，别处不动。
- **把对端的「名字」显示在这里持有的一条记录上。** 一轮远端请求落到的会话已经记下 `source_node_id`——那个来自调用方证书的身份，在节点打开这段对话时写入，并像 `Kind` 和 `SkipDream` 一样被带到压缩后的后继会话——所以这个事实已经在磁盘上，只是还没有任何东西显示它。要把它变成可读的东西，就得拿这个 id 去查**这台机器**的对端目录（`node/knownpeers.go`）；调用方自报的名字绝不用于此，因为任何密钥都能声称任何名字。动手之前要知道两件事：那个目录是**出站**接触写下的，所以一个只接收不发送的节点谁都没 pin，每个 id 都解析成「未知」（一个正确的答案，但也是最常见的那个）；而措辞必须在三个前端之间保持同一个事实的同一句话，就像 `Kind` 标签已经做到的那样。

- **`-p` 里的提醒。** 一次性运行按构造就带 `DisableSystemReminders`；它改从 `RemoteTask(action="list")` 或从某次拒绝里的对端名字拿到引用。
- **跨入口的完成通知**和空闲自动续跑。一个完成的对端任务会被存下来，提醒会在拥有它的那段对话里显示它；还没有人被推送到另一个窗口的消息，也没有哪个窗口排空的队列会吃掉另一个窗口随后会错过的事件。

## 受管会话在哪里显示出来（三个前端现在都有）

一个子系统拥有的会话，在读者遇到它的每一处都被标注——规则本身在 agent 的入口强制（`agent.Managed`），而下面这些都是让读者不至于撞进一次拒绝的礼貌：

- **那一行**：`tui/session_selector.go`（以及 `@` 选择器的行，`tui/at_file.go` 的 `managedRowSuffix`）、`web/frontend/src/lib/managed.ts`、桌面侧栏的 `.session-tag.is-managed`。同一个事实的同一句话，而且每一种情况下它都来自 `session.Session.Kind`。
- **TUI 拒绝切进去**（`loadSession`，在当前会话被结束之前——先切过去再让 agent 拒绝开场那一轮，会为了显示一个错误而结束读者自己的对话）。
- **桌面只读地打开它**：`SessionInfo.Kind` 会传过去，会话全文面板显示一条横幅，输入框被那句原因**替换掉**（`.composer-readonly`）而不是置灰——置灰的输入框仍然在邀人点击。冒烟：`managed-session`。
- **Web 控制台按构造就是只读的**，所以徽章就是它在那里的全部义务。
- 桌面的 `Kind` 字段是一个绑定：改完 `SessionInfo` 之后要重新生成 wails bindings。

Related: [`.tachi.md`](../../.tachi.md), [session-store.md](session-store.md)（持有、清理决定和它那把锁）、[compact.md](compact.md)（压缩后的会话必须带走什么）、
[itest.md](itest.md)（`itest/run` 的委派用例和 harness 的对端支持）。
