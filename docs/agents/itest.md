# Tachi —— 集成测试手册

`itest/` 下的套件、用例和 harness 都在这份文件里。最该先看的是**等待必须锚在什么上**：这里出现的偶发失败，多半是等待锚在了错的东西上，而不是产品 bug。

## 套件与 target

- 套件：`itest/run`、`itest/tui`、`itest/acp`、`itest/headless`、`itest/mockllm`（其余四个都依赖 mockllm）。
- `make itest`（先过 `web-check`，再 `make -j5` 跑五个套件；run/tui/acp 用 `ginkgo -p`，headless 与 mockllm 跑的是 `go test`）；单个套件：`make itest-run|itest-tui|itest-acp|itest-headless|itest-mockllm`。
- **`itest/headless` 是唯一不需要 GUI 的端到端套件**：它编 `desktop` 模块的 `-tags headless` 二进制（`GOWORK=off`、`CGO_ENABLED=0`，所以 Linux runner 上也能跑，不需要 gtk/webkit），然后起真进程、走手机的 HTTP/WS。八个用例：一轮真的流到手机**且**重订阅的快照里有它（实时与记录分开断言，只对一半会说另一半像丢了消息）、没有令牌是 401（`/v1/ping` 是配对页要的那个例外）、同一 HOME 的第二个宿主退出并说明原因（`host_lock.go` 那条规则的外部视角），以及审批卡的允许与拒绝 —— 命令带文件副作用，所以「真的执行了 / 真的没执行」由磁盘回答，而不是由模型说了什么；拒绝还钉住「这一轮继续、模型被再问一次」这个与中止不同的语义；还有一个**没有手机**的宿主 —— 只开 `channel: mock`，等的是日志里「IM channels running」而不是一次 ping，并断言它没绑手机端口（[desktop.md](desktop.md) 的无头宿主）；以及一对「手机对 channel 的会话说话」：一段手写的 `thread_id` 会话（`meta.json` + 空的 `messages.jsonl` —— 少那个文件快照读不出来），发给它的消息由 `mock` 那个渠道跑完，同一轮的 `text`/`turn`/`idle` 回到手机、回复也落进同一份记录；宿主里没有那个渠道时则是 409 `channel_offline`（[mobile-protocol.md](mobile-protocol.md) 不变量 13）。它和 `itest/desktop` 的分工是「要不要窗口」：那个驱动真窗口因此进不了 CI，这个只要一个能监听回环的进程。
- **无头套件的端口从日志读，不从配置猜**：`listen: 127.0.0.1:0` 让内核分配，宿主打自己实际绑定的地址，用例等的是那一行。别改成「先 Listen 拿号再关掉」——那是下面节点 fixture 已经写过的临时端口陷阱。
- `itest/tui` 在进程内用 `tea.NewProgram` 驱动真实的 `tui.Model` + `AIAgent`（按键经 `p.Send(tea.KeyPressMsg)`）；ANSI 变化流经 `itest/tui/vt.go` 回放，所以 `Screen()` 等于真实终端。
- **`itest/` 只在 `-tags=integration` 下编译**：`go test ./...`（CI 的 test job）和 build job 都不看它。所以改了 `node/`、`agent/` 那类被四个入口共用的**导出签名**时，`make test` 全绿也不代表 CI 会绿 —— 编译这道门在 `make itest` 里。改这类签名后至少跑一次 `go vet -tags=integration ./itest/...`。`desktop/` 是独立 module，**它自己的测试不在根 `go test ./...` 里**——无头宿主那条路只有 `itest/headless`（行为）与 CI 的 `desktop-headless` job（能不能编）在看着。

## 用例必须锚在什么上

- **用例的等待锚在 AGENT 的单轮状态上，绝不只看屏幕**：driver 的 `WaitIdle`/`Stop` 等的是 `AIAgent.TurnInFlight()`（每次会话写入背后那条 run goroutine）清零。渲染出的 idle 帧滞后于状态机，而 mock 的请求计数在它自己的 goroutine 上增长，所以「请求已计数 + 屏幕看着仍 idle」在一轮中途是可达到的状态——照这两样等就会提前返回，接着打字（被排进 pending 而不是开新一轮）、退出程序、或列举会话的用例，就只在负载高的 runner 上随机失败。
- **`config.SetBaseDir` / `tools.SetWorkingDir` 是进程级全局**——之所以安全，只是因为 ginkgo 的并行节点是独立进程、且用例串行；每个用例钉一个新的工作目录（目录被删掉会让 Bash 的 fork/exec 失败）。
- **密闭**：harness 的配置禁用了 `herdr`；绝不碰真实的 `~/.tachi`。
- **fixture 的 provider 是写成一个 `provider.mock.json` 的，不是 config.yaml 里的 `providers:`**（`NewHome` → `providerFiles`）：装载器把那份 yaml 里的 `providers:` 块直接丢掉（见仓库根的 `.tachi.md`「配置在哪」），所以一条还在模板里写它的 fixture 会让**每一个**用例在 BeforeEach 里就红，报的是 `NewAIAgentWithConfig: provider not found` —— 看起来像产品 bug，而不是 fixture 落后。给两个前端加 provider 时同理：`WithSecondProvider` / `WithExtraProviders` 是往那个 JSON 里加条目，不是往 yaml 模板里加。

## harness

- **`itest/harness.BuildBinary` 按 git HEAD 哈希缓存二进制**，放在 `os.TempDir()/tachi-itest-<hash>`；工作树脏了就退化成每次调用都构建——所以保持工作树干净。
- **mockllm 讲 3 种线上协议**（openai / anthropic / openai-res）；再加一种要写 route + normalize + render + 契约测试。
- **`run.Binary` 等进程，`run.Spawn` 不等。** 承载型入口（hosting）必须一直活着（它的节点服务随它一起死），所以和这类入口对话的用例用 `Spawn` 并自己持有那个进程——在 `DeferCleanup` 里 `Stop`；否则一个残留宿主占着节点锁，会让同一个 worker 的下一个用例因为毫不相干的原因失败。
- **`WithNodeDiscovery` 与 `WithGossipDiscovery` 是两个独立的开关**（harness 的 `NodeDiscoveryAny` 只决定那个 `discovery:` 块渲不渲染）：前者是组播，后者是 `node.discovery.gossip.enabled`。一个用例要两个端口时**不要连续调两次 `nodePort`**——它每次都从 worker 块的起点扫，两次会给出同一个号（后果是安静的：第二个宿主绑不上，而 channel 入口照旧活着，只有节点服务在日志里放弃）。

## 没有终端的节点（两个真进程）

- **只有三个入口承载节点**（`node.StartOrAttachNode`：TUI、desktop、channel）。一台无终端的机器用**无头宿主**（`-tags headless`，它不需要任何 channel）；channel 入口是另一条路，而它在没启用任何 channel 时拒绝启动。`channel/mock` 就是满足这一条的惰性 channel：`Name`/`OnStart`/`Run`，其中 `Run` 阻塞到关闭为止。它不面向用户（只有 `Manager.AddEnabled` 读注册表，所以没有任何东西会列出或提供它）。
- **fixture 必须自己写出 A2A 端口。** 承载进程的远端地址没有任何东西会公布——`local.json` 带的是本机控制地址、节点 id、实例 id 和令牌（外加 `pid`、`api_version`、`started_at`），没有任何远端地址——所以把对端放在独立进程里的用例要自己挑一个空闲端口（`0.0.0.0:<port>`，这同时让它离开 loopback、因而落在 mTLS 后面）并写进配置。**挑法本身是个陷阱：绝不能用 `:0` 拿号再关掉**——那个号来自内核的临时端口池，也就是全机每一次出站连接分配源端口的地方，所以关掉与宿主绑定之间任何一次 dial 都可能拿到同一个号；宿主绑不上，而 channel 入口**按设计**不会因此退出（它记一行日志就继续服务 channel），于是没有任何东西在监听，等待只能超时。`nodePort` 改成按 ginkgo worker 分块、取临时端口范围以下的号。
- **等到 `local.json` 不等于等到了一个节点。** 这个文件按设计比它的写入者活得久（`Close` 会留下它，因为附着路径会重新核对实例 id 和握手，所以那里放着一份旧文件是无害的）——而一个刚发布完就死掉的入口也会把它留在原地。就绪 = 「文件出现了」且「端口上有人应答」（`net.Dial`；非 loopback 面上握手**本来就该**失败，所以能查的只有 TCP 连接）。进程在等待期间退出时，把它**自己的**输出报出来：「从未就绪」会让人往错的地方找，而答案——一个启用了却没注册的 channel、一个被占的端口——就摆在那儿。**降级的节点服务更隐蔽：它只写日志、不上 stderr**（channel 入口继续服务），所以这个等待在失败时也报进程自己的日志行，并在一行 `node service unavailable` 上立刻失败，而不是把 15 秒预算烧完。
- 正是这一对让跨进程用例成为可能：两份身份、两份 TOFU pin 记录、两个数据目录、两份日志，各由自己的进程写出（`itest/run/node_host_test.go`）。**其余一切仍用进程内对端**：它不需要第二个进程就能证明 config→surface 这条路径，而只有进程边界才需要这套机制。
- **那个文件里三个用例各自覆盖什么**，免得被重写：(1) 一个真进程承载，另一个真进程通过 mTLS 委派给它，两侧的记录都在磁盘上核对；(2) 同一 `--home` 上的第二个进程**附着**，而不是起第二套服务——断言落在它自己的 `attached to local node service` 那一行上（按 pid 归属，因为共享一个 home 的两个进程共享同一份日志），外加锁被别的进程持有、已发布的实例 id 是第一个进程的；(3) 一个宿主在某个任务停在问题上时被 `-9` 杀掉——下次启动取走被释放的锁（flock 随持有者一起死），把那个任务判为中断，并保住那个问题。保留期那把**协调锁**的争用测试仍**未做**（`node.md`）；这三个讲的是节点锁。
- **这三个各有一种值得知道的坏法**：少了那个惰性 channel，宿主发布完就立刻退出（上面那个就绪陷阱）；少了 kill，重启就会附着，中断任务的断言永不触发；换一个 home，「第二个」进程就成了承载方，附着断言永不触发。三条都是靠把它们弄坏来核对的，而且每次失败都报进程自己的输出，不是一个不给原因的超时。
- **第四个用例（`itest/run/warmup_test.go`）借的是同一套 fixture，问的是另一个问题**：入口**启动的时候**会不会自己把配置里的节点问一遍（预热，见 [node.md](node.md) 的发送侧）。断言写成「谁都没要求过它」：SUT 一条 IM 消息都没收到、模型一次都没被调用，而 `a2a/peer-identities.json` 里已经写着对端的身份——把那次 `WarmUp` 关掉它就是红的（核对过一次）。它**测不了**「不阻塞启动」：一个在回答的对端，在进程这一层分不出「等了」和「没等」，那一条是 node 包单测（`node/delegatorcache_test.go`）钉的。
- **`itest/run/discovery_test.go` 是唯一靠真组播的用例，开头有一道环境闸**：先用**库本身**（`zeroconf` 注册一次 + 浏览一次，走的就是 `node.LANInterfaces` 给的那批接口 —— 它是为这道闸导出的）证明这台机器能收到组播，证明不了就 `Skip` 并把试过的接口名字说出来；闸过了再等不到公告就是**失败**。探针必须是库而不是 Tachi 的浏览：用被测代码当探针，「这个网络不行」和「我们坏了」就分不开了，而闸要分的正是这两件事。系统解析器也不是答案——macOS 的 mDNSResponder 在进程自己拿不到组播的机器上照样看得见整个网络。（这台机器上它过不过闸是环境的事：同一个 checkout 上一次能组播的机器，换一个接入点就可能不行——所以本地看到 skip 不代表代码坏了。）
- **`itest/run/gossip_test.go` 问的是第三个来源：转述**（`node.md` 的 gossip）。它跑的是**三台都开着节点服务**的样子，而配置里只有两条：A 配了 B、B 配了 C，**C 什么都没配**；组播关着。要证明的是三张表最后都满 —— A 的表里有 B（配置）和 C（转述），B 的表里有 C（配置）和 A，C 的表里有 B 和 A（它只能靠「别人来问它时自报的地址」和「它问 B 时听到的转述」）。B、C 是两个真进程（同上一条那个 `channel` fixture）；A 在测试进程里，但**它是一台完整的机器**：同一份 config 既起了它的节点服务（所以 B、C 拨得到它），也起了它的发送侧（所以它会去问 B）—— 能在 itest 里被驱动的常驻入口只有 channel（接收不了指令）与 ACP（不挂发送侧），而这条链的落点是一次点名委派。各台「认识谁」的证据：A 读它的目录，B、C 读它们磁盘上的 TOFU 记录（那份文件里每一行都是「这个进程自己拨通过一次」，pin 由出站握手的证书检查写下）。它**不依赖组播**，所以在跑不了 mDNS 的机器上照样真跑。核对过它有区分力两次：把 B 的 gossip 关掉，它卡在「A 认识 C」；把 A 自报的地址换成对方拨不到的形状，它卡在「C 认识 A」—— 也就是这一条同时钉着「听别人说」和「自己得能被拨到」这两半。

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md)（冒烟套件，那是另一套 harness）、[prompt-cache.md](prompt-cache.md)（为什么 mock 的请求计数不是单轮边界）、[node.md](node.md)（节点服务怎么对待它所在的那个进程）。
