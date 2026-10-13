# Tachi —— 会话存储手册

每一个读会话列表、写 `meta.json`、删会话的路径都经过这一份存储：每个前端的会话列表、桌面侧栏、`--resume`，以及一轮留在会话旁的附属文件。权威是 `session/store.go` 与 `session/retention.go`。

## 列表是记忆化的

- **`ListSessions` 用一次目录遍历回答所有调用者；`LoadMeta` 则必须保持不记忆化。** 一次 UI 刷新会在同一口气里反复要「全部会话」——桌面侧栏先要它那几行，再要每个项目的成员数，然后要某个项目的成员，是三次独立调用——所以 `ListSessions` 拿一份快照把三次都答了（没命中就付一次遍历，几乎全是 open/read/close）。快照会被这个存储的每一次写入丢掉（`CreateSession` / `UpdateMeta` / `DeleteSession`，都在写入**之后**才丢），也会在基目录的 mtime 变动时丢掉——后者覆盖的正是**另一个**进程创建或删掉的会话。
- **`LoadMeta` 绝不能记忆化**：它喂的是活记录，以及元数据「读-改-写」里读的那一半（`desktop/roots.go` 的 `updateSessionMeta` 会重写整份 `meta.json`），在那里读到一份旧的，就是丢掉一次写。`session/store.go` 的 `FileStore` 注释载着完整契约，包括它自己承认的那一个洞。

## 写入

- **`meta.json` 是原子写的（tmp + rename）。** 保持这样：就地重写会让并发的读者——`--resume` 新开的存储、桌面侧栏、另一个进程——读到截断的文件，而 `scanSessions` 只能跳过解析不了的东西，于是一轮活着的回合会把自己的会话从别人的列表里悄悄丢掉。`session/store_test.go` 的 `TestListSessionsNeverMissesASessionBeingRewritten` 钉住它（对着 `os.WriteFile`，几百轮之内就会失败）。
- **桌面把每个 manager 都建在同一个 store 上**（`d.sessionStore()`）：一轮通过它自己那个按轮的 manager 写入，而只有共享同一个 store，那次写入才能丢掉侧栏下一次读取本会被喂的那份快照。自己 new 一个 manager 就是那个错——它内部会另建一个 `NewFileStore`，拿不到 `d.sessionStore()` 那份快照（`desktop/` 下正确的写法是 `session.NewManagerWithStore(d.sessionStore(), nil)`）；[desktop.md](desktop.md)（桌面后端）有理由。
- **图片字节在 `AppendMessage` 里离开记录，这也是它们安全的原因。** 多模态消息的 parts 落在记录上（`Message.Parts`），字节不在：`AppendMessage`——会话记录**唯一**的写入者——把每张图片搬进该会话的 `images/` 目录，作为内容寻址的 blob（解码后字节的 SHA-256，所以同一张图发两次只花一个文件），只留下引用。两条规则由此成立：别的东西不许写会话记录（subagent 和一次性记录器有自己的文件，它们调用 `DropUnstoredImageParts`——一条它们解析不了的引用，比没有这个 part 更糟），以及磁盘上的记录永不携带 base64——这正是让一个满是截图的会话，仍是 `LoadMessages` 扫描缓冲区里可读 JSONL 的原因。读的那一半是 `LoadImage`，经 manager 以 `agent.ImageResolver` 抵达；blob 没了只让那一个 part 降级，绝不让转换失败（[prompt-cache.md](prompt-cache.md) 讲了为什么重建必须逐字节一致）。

## 分支是一次「先建好整棵树、再由 meta.json 发布」的写

- **`FileStore.ForkSession` 是这个存储的第二个写入者形态**，它的次序是承重的：先写 messages.jsonl 的**逐字节前缀**、再拷被引用的图片 blob、再写 api_requests 的前缀，**meta.json 最后**原子写。`ListSessions` 扫的是 meta.json，所以半途失败留下的是一个读不到的目录（失败路径会把它删掉），而不是列表里一个历史断在中间的会话。
- **记录只能拷字节，不能重放**：重放要写这份记录就要过 `Manager.AppendMessage`，它把 `Timestamp` 改成现在 —— 而桌面分页按时间戳**严格早于**过滤，同一戳的一批记录会被整段跳过。
- **段落切分只有一处实现**（`splitJSONLAt`）：回退把尾巴搬进 sidecar、分支把前段拷进新会话，两者对「第 N 条记录从哪开始」（含末行没有换行符那种）必须给出同一个答案。

## 清理允许删掉什么

- **会话可以被持有，而这条持有在磁盘上，不在取它的那个 manager 的内存里。** `session/retention.go` 是权威：`<sessionDir>/retention/holds/` 下一个会话一个文件，每个写明 owner 和 reason，由同一对 (owner, ref) 释放。**理由只有一个——`task`，即此刻正在这个会话里跑的一次执行——每条删除路径问的是同一个问题、得到的是同一个答案**：`Manager.CleanupOldSessions` 跳过被持有的会话，`Manager.Delete` 用点名该结束什么的 `SessionHeldError` 拒绝，而删除会连同会话一起放开剩下的持有（`Retention.protected` 说了为什么）。曾经有第二个理由和两套策略：一个 `context` 持有把远端对话的会话留一个它自己的窗口，清理尊重它，而读者自己点的那下删除不尊重。窗口没了（`docs/2026-10-10-a2a-node-design.md` §9.4）：远端对话的会话就是普通会话，所以 `desktop.DeleteSession`、ACP 的删除和另一个进程里的清理，谁也绕不过这条规则。`Pinned`、`ThreadID`、`Kind` **不是**这个机制——pin 是读者自己的声明，抬掉它绝不能丢掉一个正在跑的任务（同一份设计文档 §9.4）。`Retention.BusySessions` 一次扫描就答出「哪些会话有正在跑的执行」，桌面侧栏要提供「停掉一个」靠的正是它。
- **每个以删除收尾的决定，都在同一把跨进程锁下做出**（`<sessionDir>/retention/retention.lock`，`Retention.Lock`），它与节点的生命周期锁不是同一把，也从不共享路径：节点在线不得阻止另一个进程合法清理。候选在锁**外**枚举（目录扫描不该待在跨进程临界区里）；「它被持有吗、它还是当前的吗」的复查在锁**内**。锁文件释放时不删，和本仓库其它每一把 flock 一样。
- **进程内锁排在第二。** 固定次序是文件锁 → `Manager`/store 锁，所以调用者去拿保留锁时不得持有 manager 的 mutex；`Manager.Delete` 和 `Manager.CleanupOldSessions` 就是这么写的，`Manager.New` 也是：先取 `m.mu`（`newLocked` 的名字指的是**这个**锁——它写 `m.current`，而 `Current`/`HasCurrent` 都持着它），**放掉之后**才跑清理。反过来，就是清理与节点绑定 context 之间的死锁。
- **一条持有还会说明它已经不可能知道的事。** 文件读不出来的持有，保留那个会话（跳过并上报），绝不释放：读不出来的保护不等于没有保护。持有生命周期的拥有者在启动时收拾残留（`node/runner.go` 的 `reconcile`），因为一次崩溃可能留下一个谁也不会再撤销的理由。**三种读法，不是两种**：读不出来 → 保留并上报；一个谁也没听说过的理由 → 保留并上报（更新的版本可能在持有它）；一个**已退休**的理由（`session.ReasonContextRetired`，退休的 `context`）→ 看得懂、什么也不保护，所以这一层回答的问题忽略它（`liveHolds`——读者的删除不会被一条写着「为这段对话的窗口保留」的记录挡住，侧栏也不会为一条不存在的执行去提供「停掉」），它的拥有者在下次启动时释放它，删除会带着会话一起拿走那条记录（`Manager.clearRetiredLocked`）。

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md)（桌面后端、侧栏的读取路径）、[checkpoints.md](checkpoints.md)（一轮写在会话旁的附属文件）。
