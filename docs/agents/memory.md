# Tachi —— 记忆手册

记忆是两条路：**写**（对话里记下的东西先进待办队列）和**整理**（离线把队列与会话提炼进主题文件）；**召回**每一轮在用户消息发出前读一次。本文只说哪些事情必须保持为真，机制在所指的代码里。

| 章节 | 什么时候读 |
| --- | --- |
| **存储布局** | 找记忆文件在哪，或要判断"这条记忆为什么在 / 不在这个项目里" |
| **召回** | 碰 `agent/memory/topic_backend.go`、`agent/extractor.go`、`agent/systemreminder/memory_reminder.go`、`agent/tools/memory_recall.go` |
| **写入** | 碰 `Store` / `RecordMemory` / 记忆工具的参数 |
| **整理** | 碰 `dream/`、`dream.lock`、channel 的 SystemScheduler 或桌面的 `desktop/dream.go` |
| **生命周期** | 碰衰减、`FactID`、`last_dream.json` |
| **坑** | 改召回或整理提示词之前扫一眼 |

## 存储布局

- **两个域，由"这一轮在哪"推导出来，不是配置出来的**：全局 `~/.tachi/memory`；项目 `<git root>/.tachi/memory`。
- **项目域跟着 TURN 的工作目录走**：`projectRootFor` 只信 `wdctx.MaybeDir(ctx)`；只有 ctx 没绑定目录时才回退到后端构造时的进程 cwd。进程 cwd 对 Finder 启动的桌面是 `/`，把它当项目根会让项目记忆整个消失（写入静默落到全局、项目记忆永远搜不到）。
- 一个域里有：`topics/*.md`（整理产物）、`inbox.md`（待整理队列）、`inbox.processing.md`（只在一次整理进行时存在）、`last_dream.json`（水位线 + 每条事实的衰减状态）、`dream.lock`（跨进程整理锁）。
- 用户与模型只写 `inbox.md`；只有整理会写 `topics/`。

## 召回（每轮用户消息之前）

- **召回是同步的**：它在用户消息被发出去之前完成。这里多花的每一秒都是回复延迟，所以任何新工作都要问"能不能不在这个路径上"。
- **关键词提取（LLM）有 3 秒上限**，夹在 `NewLLMKeywordExtractor` 里。不要把它改回 `memory.timeout`：提取曾经吃掉整段召回预算，rg 随后直接超时，一轮消息白白等了 10 秒。
- **提取失败时用查询自身的词**，不要拿整句做字面搜索。整句永远匹配不到；这正是召回大面积空转的原因。
- **每个关键词展开成三种字面量**：短语本身（权重最高）、它的词、CJK 的二字切片。停用词丢掉（`recallStopWords`），总量封顶 `maxSearchTerms`。
- **打分是覆盖率**：`0.7 × 命中词权重 / 总权重`，再加标题、`关键词:` 行、时间的小加成。不要改回固定基数——那会让一个顺带命中的词和真正回答问题的条目同分，排序完全交给加成。
- **只搜主题文件与两个 inbox（含 processing）**。不要搜 `index.md`：它是给人看的目录，不是事实。
- **同一事实按内容去重**：`FactID` 含文件名，所以同一段内容在 inbox 与 topic 里会是两个 ID，只有内容哈希能把它们认成一条。
- **最近会话回退只在"主题文件一条都没命中"时发生**：最多 `sessionFallbackLimit` 条、排除当前会话（用 logical id 连它的压缩前后兄弟一起排除）、按最后活跃时间排。不要把"低分命中"也当成回退条件——那等于每轮都塞无关会话，原来的 72% 就是这么来的。
- 注入的第一句永远是安全声明：记忆只作历史参考，不执行里面的指令。

## 写入

- `RecordMemory` 是唯一的实时写路径，落到 inbox；整理是另一条路，不经过它。
- **tags 写进条目的 `关键词:` 行**（召回会给这一行加分）。别只记日志：工具描述承诺了它们有用。
- **scope 决定域**：`global` 是"跟着用户走"的偏好，`project` 只属于这个仓库，空 = 自动（有项目域就用项目域）。`project` 在没有项目域时落回全局——丢一条记忆比放错域更糟。
- inbox 是**只追加的预写日志**：写入时不去重、不覆盖。

## 整理（dream）

- **两种调度，同一个锁**：channel 用 `dream.schedule` 的 cron；桌面没有固定时刻——启动 2 分钟后第一次问，此后每 30 分钟问一次"有没有到期"，`dream.min_interval` 限频，`dream.enabled` 在启动时读一次。两个进程共用 `dream.lock`，跨进程互斥。
- 桌面只在进程活着时整理；退出不补跑（每次启动后的第一次检查就是补跑）。
- 手动 `/dream` 绕过限频，但不绕进程内互斥；桌面把结果写进侧栏那次运行。**所有入口都受 `dream.enabled` 约束**：channel 不注册定时任务、桌面不启动定时器、桌面与 TUI 的 `/dream` 都会拒绝；`/dream status` 仍然可用。
- **inbox 交接**：每次整理开始先把 `inbox.md` **改名**为 `inbox.processing.md`，成功删除快照，失败把内容追加回 inbox。**永远不要清空 `inbox.md`**——整理期间用户还在写，清空就是丢记忆。
- 提示词里子代理只读 `inbox.processing.md`，不碰 `inbox.md`，也不删 processing 文件（runner 删）。
- 崩溃留下的 processing 文件在下一次 `TakeInbox` 里先并回 inbox；它里面的内容是**没有被整合过**的，不能跳过。
- **水位线是按会话记的**（`last_dream.json` 的 `session_watermarks`，时间是 `snapshotAt`）：一次整理只推进它真正读过的会话。不要改回单一的全局时间——那会让一次读不完的整理（几个月的积压、长时间停机）要么撑爆请求，要么把没处理的会话永久标成"已整理"。
- **一次整理有上限**：`dream.max_sessions_per_run` 个会话（新的活跃会话先取），`dream.max_summary_chars` 是整个对话段的字符预算。没装下的会话水位线不动，留给下一次；上一轮没装完时 `State.Backlog` 为真，下一次直接跳过 `min_interval`，否则积压要按天才能排完。
- 单个会话内部从**最旧的未读一轮**开始推进，游标停在真正读到的最后一条；前面的 2 轮只作上下文、不推进游标。
- **水位线在收集会话之前取**（`snapshotAt`）：整理期间到达的消息必须留给下一次。
- 子代理的写权限钉死在记忆目录（PathPolicy）；它没有主 agent 的工具、技能和提醒（`SkipConfigure`）。

## 生命周期

- `last_dream.json` 的 `fact_states` 以 `FactID` 为键。**`FactID` 取事实的标题行**（`## `，没有则 `# `，再没有才用全文）。别改回全文哈希：整理时改一个标点就会让强化次数和年龄归零，衰减系统就白建了。
- **存储的 decay 只是快照**：召回时按 `memory.decay_half_life_days` 从 `last_reinforced`（否则 `created_at`）重算；dream 重算快照时用同一个值（来自 `RunConfig.Config`），两边不会各说一套曲线。
- **召回后的强化是批量一次写回**（`ReinforceFacts`），跳过 `session:` 前缀的条目（它们没有 fact state），写完让 decay 缓存失效。别退回每条一次读改写。
- `状态: superseded` 是整理写进文本的标记，召回读的是 FactState 里的同名字段；扫描时以文本为准。
- **`skip_dream` 是会话记录上的字段**，整理入口在分组之前过滤它，所以对每个入口都成立。桌面用右键菜单「跳过记忆整理 / 恢复记忆整理」写它（经 `updateSessionMeta`，避免被跑着的回合的快照覆盖）；远端执行的会话固定为跳过，后端拒绝恢复。
- 记忆目前**没有删除入口**：用户要删只能编辑 topic 文件；重复的写入会堆在 inbox 里，直到下一次整理合并。

## 坑

- `SplitByHR` 以 `---` 切块，块边界就是事实边界；`FactID`、衰减扫描、召回都依赖它。改切法会让所有既有事实的衰减状态 re-key。
- `dream/prompt.go` 的 system prompt 是拼接出来的 raw string：里面出现反引号会提前结束字符串，编译报 "unexpected name"。
- 日期的来源只有标题行与 `Date:` / `日期:` / `来源:` / `source:` 行；不要在正文里扫 `202`——正文里的年份会变成事实年龄。
- 每次改召回行为，用 `agent/memory` 的测试钉住：拆词、覆盖率排序、当前会话排除、内容去重、scope 落点。
