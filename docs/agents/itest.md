# Tachi —— 集成测试手册

`itest/` 下的套件、用例和 harness 都在这份文件里。最该先看的是**等待必须锚在什么上**：这里出现的偶发失败，多半是等待锚在了错的东西上，而不是产品 bug。

## 套件与 target

- 套件：`itest/run`、`itest/tui`、`itest/acp`、`itest/mockllm`（run/tui/acp 依赖 mockllm）。
- `make itest`（先过 `web-check`，再 `make -j4` 跑四个套件；run/tui/acp 用 `ginkgo -p`，mockllm 那个跑的是 `go test`）；单个套件：`make itest-run|itest-tui|itest-acp|itest-mockllm`。
- `itest/tui` 在进程内用 `tea.NewProgram` 驱动真实的 `tui.Model` + `AIAgent`（按键经 `p.Send(tea.KeyPressMsg)`）；ANSI 变化流经 `itest/tui/vt.go` 回放，所以 `Screen()` 等于真实终端。

## 用例必须锚在什么上

- **用例的等待锚在 AGENT 的单轮状态上，绝不只看屏幕**：driver 的 `WaitIdle`/`Stop` 等的是 `AIAgent.TurnInFlight()`（每次会话写入背后那条 run goroutine）清零。渲染出的 idle 帧滞后于状态机，而 mock 的请求计数在它自己的 goroutine 上增长，所以「请求已计数 + 屏幕看着仍 idle」在一轮中途是可达到的状态——照这两样等就会提前返回，接着打字（被排进 pending 而不是开新一轮）、退出程序、或列举会话的用例，就只在负载高的 runner 上随机失败。
- **`config.SetBaseDir` / `tools.SetWorkingDir` 是进程级全局**——之所以安全，只是因为 ginkgo 的并行节点是独立进程、且用例串行；每个用例钉一个新的工作目录（目录被删掉会让 Bash 的 fork/exec 失败）。
- **密闭**：harness 的配置禁用了 `herdr`；绝不碰真实的 `~/.tachi`。

## harness

- **`itest/harness.BuildBinary` 按 git HEAD 哈希缓存二进制**，放在 `os.TempDir()/tachi-itest-<hash>`；工作树脏了就退化成每次调用都构建——所以保持工作树干净。
- **mockllm 讲 3 种线上协议**（openai / anthropic / openai-res）；再加一种要写 route + normalize + render + 契约测试。
- **`run.Binary` 等进程，`run.Spawn` 不等。** 承载型入口（hosting）必须一直活着（它的节点服务随它一起死），所以和这类入口对话的用例用 `Spawn` 并自己持有那个进程——在 `DeferCleanup` 里 `Stop`；否则一个残留宿主占着节点锁，会让同一个 worker 的下一个用例因为毫不相干的原因失败。

## 没有终端的节点（两个真进程）

- **只有三个入口承载节点**（`node.StartOrAttachNode`：TUI、desktop、channel），且**只有 channel 入口能无头跑**——但没启用任何 channel 时它拒绝启动。`channel/mock` 就是满足这一条的惰性 channel：`Name`/`OnStart`/`Run`，其中 `Run` 阻塞到关闭为止。它不面向用户（只有 `main_channels.go` 读注册表，所以没有任何东西会列出或提供它）。
- **fixture 必须自己写出 A2A 端口。** 承载进程的远端地址没有任何东西会公布——`local.json` 带的是本机控制地址、节点 id、实例 id 和令牌（外加 `pid`、`api_version`、`started_at`），没有任何远端地址——所以把对端放在独立进程里的用例要自己挑一个空闲端口（`0.0.0.0:<port>`，这同时让它离开 loopback、因而落在 mTLS 后面）并写进配置。**挑法本身是个陷阱：绝不能用 `:0` 拿号再关掉**——那个号来自内核的临时端口池，也就是全机每一次出站连接分配源端口的地方，所以关掉与宿主绑定之间任何一次 dial 都可能拿到同一个号；宿主绑不上，而 channel 入口**按设计**不会因此退出（它记一行日志就继续服务 channel），于是没有任何东西在监听，等待只能超时。`nodePort` 改成按 ginkgo worker 分块、取临时端口范围以下的号。
- **等到 `local.json` 不等于等到了一个节点。** 这个文件按设计比它的写入者活得久（`Close` 会留下它，因为附着路径会重新核对实例 id 和握手，所以那里放着一份旧文件是无害的）——而一个刚发布完就死掉的入口也会把它留在原地。就绪 = 「文件出现了」且「端口上有人应答」（`net.Dial`；非 loopback 面上握手**本来就该**失败，所以能查的只有 TCP 连接）。进程在等待期间退出时，把它**自己的**输出报出来：「从未就绪」会让人往错的地方找，而答案——一个启用了却没注册的 channel、一个被占的端口——就摆在那儿。**降级的节点服务更隐蔽：它只写日志、不上 stderr**（channel 入口继续服务），所以这个等待在失败时也报进程自己的日志行，并在一行 `node service unavailable` 上立刻失败，而不是把 15 秒预算烧完。
- 正是这一对让跨进程用例成为可能：两份身份、两份 TOFU pin 记录、两个数据目录、两份日志，各由自己的进程写出（`itest/run/node_host_test.go`）。**其余一切仍用进程内对端**：它不需要第二个进程就能证明 config→surface 这条路径，而只有进程边界才需要这套机制。
- **那个文件里三个用例各自覆盖什么**，免得被重写：(1) 一个真进程承载，另一个真进程通过 mTLS 委派给它，两侧的记录都在磁盘上核对；(2) 同一 `--home` 上的第二个进程**附着**，而不是起第二套服务——断言落在它自己的 `attached to local node service` 那一行上（按 pid 归属，因为共享一个 home 的两个进程共享同一份日志），外加锁被别的进程持有、已发布的实例 id 是第一个进程的；(3) 一个宿主在某个任务停在问题上时被 `-9` 杀掉——下次启动取走被释放的锁（flock 随持有者一起死），把那个任务判为中断，并保住那个问题。保留期那把**协调锁**的争用测试仍**未做**（`node.md`）；这三个讲的是节点锁。
- **这三个各有一种值得知道的坏法**：少了那个惰性 channel，宿主发布完就立刻退出（上面那个就绪陷阱）；少了 kill，重启就会附着，中断任务的断言永不触发；换一个 home，「第二个」进程就成了承载方，附着断言永不触发。三条都是靠把它们弄坏来核对的，而且每次失败都报进程自己的输出，不是一个不给原因的超时。
- **第四个用例（`itest/run/warmup_test.go`）借的是同一套 fixture，问的是另一个问题**：入口**启动的时候**会不会自己把配置里的节点问一遍（预热，见 [node.md](node.md) 的发送侧）。断言写成「谁都没要求过它」：SUT 一条 IM 消息都没收到、模型一次都没被调用，而 `a2a/peer-identities.json` 里已经写着对端的身份——把那次 `WarmUp` 关掉它就是红的（核对过一次）。它**测不了**「不阻塞启动」：一个在回答的对端，在进程这一层分不出「等了」和「没等」，那一条是 node 包单测（`node/delegatorcache_test.go`）钉的。

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md)（冒烟套件，那是另一套 harness）、[prompt-cache.md](prompt-cache.md)（为什么 mock 的请求计数不是单轮边界）、[node.md](node.md)（节点服务怎么对待它所在的那个进程）。
