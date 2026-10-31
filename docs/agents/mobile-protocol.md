# Tachi —— 移动端协议

手机和桌面之间那层线上的东西：HTTP 端点、WebSocket 事件、每个字段的意思。**它是这一层的唯一出处。**

三份东西照它实现，缺一份就是漂：

| 谁 | 在哪 |
| --- | --- |
| 手机端 | `mobile/src/api/`（`types.ts` 是本文的 TypeScript 镜像） |
| 桌面端 | `desktop/mobile*.go` |
| 假服务端（给手机端单独开发用） | `mobile/mockapi/`（Go，`main.go` 可执行） |

**改协议 = 同一轮里改这四处**：本文 → 桌面实现 → `mobile/src/api/types.ts` → `mobile/mockapi/`。只改其中一处，表现是「某个字段永远空着」或「某类事件永远不来」，不会报错 —— 这是这条链路上最难查的一类故障。字段名、事件名一律**不缩写、不改写**：两边照着抄，不要各自"顺手改顺眼一点"。

东西还没实现的部分（桌面侧的接口）见 [mobile.md](mobile.md)。

## 目录

| 章节 | 什么时候读 |
| --- | --- |
| **传输与鉴权** | 你要加一个端点、改连接方式、碰令牌 |
| **HTTP 端点** | 你要让手机做一件新事 |
| **WebSocket** | 你要加一类事件、改订阅方式、动背压 |
| **线上类型** | 你要加/改一个字段 |
| **不变量** | 你在实现或改这条链路上的任何东西 |

## 传输与鉴权

- 命令走 **HTTP**（能独立重试、能用 `curl` 调），过程走**一条 WebSocket**。
- 除 `/v1/ping` 外都要 `Authorization: Bearer <token>`；WebSocket 的令牌放在查询串里（`?token=`），因为 React Native 的 WebSocket 设不了握手头。
- 令牌是凭据，不是配置：单独的文件、`0600`、可一键作废，作废时断开所有活连接。
- V1 是**局域网明文**。拿到令牌就能让那台电脑执行命令 —— 这是被接受的代价，代价写在使用它的那个界面上。
- 地址：`0.0.0.0:8787`（默认）。**一台电脑对外的地址可能不止一条**，局域网的和 Tailscale 的 100.x 常常同时在 —— 见不变量 11。

## HTTP 端点

全部在 `/v1` 下。

| 方法 | 路径 | 请求 | 返回 |
| --- | --- | --- | --- |
| GET | `/v1/ping` | — | `{ok, name, hostname, version, urls, maxUploadMb}`，配对页用它验证地址与令牌 |
| GET | `/v1/sessions` | — | `{sessions: [SessionSummary]}` |
| POST | `/v1/sessions` | `{projectId?, workingDir?}` | `{session: SessionSummary}` |
| GET | `/v1/projects` | — | `{projects: [ProjectRef]}`，新建会话时的项目下拉框 |
| GET | `/v1/providers` | — | `{providers: [{name, model}]}`，可切换的模型 |
| GET | `/v1/awake` | — | `{awake: AwakeState}`，这台电脑的「保持唤醒」开关 |
| POST | `/v1/awake` | `{on, durationMin?}` | `{awake: AwakeState}`，改完立刻广播 |
| GET | `/v1/sessions/{id}/page` | `?before=<index>&limit=<n>` | `{messages: [Message], hasMore, session}` |
| POST | `/v1/sessions/{id}/files` | multipart，一个 `file` 部件 | `{file: UploadedFile}`，把文件存进这段会话自己的目录 |
| POST | `/v1/sessions/{id}/messages` | `{text, mode, attachments?}` | `{accepted: true}` |
| POST | `/v1/sessions/{id}/stop` | — | `{accepted: true}` |
| DELETE | `/v1/sessions/{id}` | — | `{accepted: true}` |
| POST | `/v1/sessions/{id}/pin` | `{pinned}` | `{accepted: true}` |
| POST | `/v1/sessions/{id}/title` | `{title}` | `{accepted: true}` |
| POST | `/v1/sessions/{id}/provider` | `{name}` | `{accepted: true}` |
| POST | `/v1/sessions/{id}/skip-approval` | `{skip}` | `{accepted: true}` |
| POST | `/v1/sessions/{id}/permissions/{toolId}` | `{decision}` | `{accepted: true}` |
| POST | `/v1/sessions/{id}/questions` | `{answers}` 或 `{decline: true}` | `{accepted: true}` |
| GET | `/v1/sessions/{id}/files/{attachmentId}` | — | 文件本身 |
| GET | `/v1/sessions/{id}/panels` | — | `{panels: Panels}`，MCP / agent / 定时任务三个只读面板 |
| POST | `/v1/cron/{id}/run` | — | `{accepted: true}`，立刻跑一次（异步） |
| POST | `/v1/cron/{id}/pause` | `{paused}` | `{accepted: true}` |
| DELETE | `/v1/cron/{id}` | — | `{accepted: true}` |

`mode` 是 `send` / `steer` / `interrupt`；`decision` 是 `allow_once` / `deny`。`attachments` 是上传时拿到的**名字**（不是路径），见下面「附件」那条。

- **翻页游标是记录序号**：`before` 传上一次拿到的最旧一条的 `index`，服务端返回序号**严格小于**它的那 `limit` 条。不用时间戳 —— 消息没有自己的 id，而时间戳会撞，同一刻的两条落在页边界上会被整段跳过。
- **新建会话不改变桌面正在看的那一段**（不变量 7）：`projectId` 走项目的目录快照；没有项目时按 `workingDir` 起一段，空 = `~/workspace`，`~` 与 `~/x` 由**那台电脑**展开（手机没有文件选择器），目录不存在会建出来。新建成功同样广播 `sessions_changed`。
- **重命名与桌面同一条写入方**（`RenameSession`），成功即广播 `sessions_changed`：标题是行在列表里的身份，两块屏都靠重读它 —— 手机上改的名字，桌面上那一行立刻跟着变（反过来也一样）。空标题是 `400 bad_request`；标题是装饰性的，所以只读会话也改得。
- **置顶是列表的事实**：`pinned` 写进会话记录，成功即广播 `sessions_changed` —— 两块屏的次序都由重新读列表得出，不各算一份。
- **`hostname` 是那台机器自己的名字**（`os.Hostname()`，见 `mobile_http.go`），它和 `name` 不是一回事：`name` 是产品名，每台安装都一样。「记住多台电脑」的手机拿它当默认显示名（去掉 macOS 的 `.local`），用户可以在手机上改名 —— **改名只在手机上生效，协议里没有写回它的端点**。
- **附件端点不接受路径**：`attachmentId` 就是那次 `SendFile` 调用的 id，只回记录里反查，查不到就是 404。记录里的 `path` 可能是相对的，按那次调用所属会话的工作目录解析。这样「令牌 = 任意文件读取」这个洞从一开始就不存在。
- **手机发文件是两步，而且第二步只认名字。** 上传（`POST .../files`）把字节写进**这段会话自己的目录**（`uploads/`，与 `pasted/` 并排：删会话一起删，绝不落进用户的工作区），回的是它存下来的 `name`、大小和 `kind`。发送时 `attachments` 递的就是那个 `name` —— 服务端只在**这段会话的 `uploads/` 里**按名字解析，递一个 `../../x` 或一个不存在的名字都是拒绝（`bad_request` / `no_such_upload`），不是跳过。所以令牌仍然不等于「读那台机器上任意文件」。
- **文件不经过另一条路进模型**：解析出来的名字变成一句 `@<绝对路径>`，和桌面上拖进来的文件是同一个引用，展开（文本内联 / 图片变多模态 / 其它让 agent 用工具读）走 `atfile.Expand`。附件只是「这句话里带着几个文件」，没有第二种传输。
- **气泡显示文件名，不显示路径**：记录里的 `displayContent` 是「用户的话 + 每行一个 `📎 <名字>`」（服务端 `mobileMessageWithFiles` 那一句，手机本地回声镜像同一条规则）。两边读的是同一条记录，所以桌面上看这段对话也是这句话。
- **图片有自己的上限（5 MB，`llm.MaxImageSize`），在上传时就被拒**：它要作为多模态内容交给模型，存下来而没人看得见不是一种结果。HEIC/HEIF 也拒（`unsupported_image`）—— iPhone 的相机默认拍这个，而模型只认 png/jpg/gif/webp；相册那条路用 `compatible` 转成 JPEG。`maxUploadMb`（ping 里那一项）是这台机器对**非图片**文件的上限，手机在挑完文件、开始上传之前就能拒绝一个太大的。

**拒绝的原因要原样透传**，不要笼统成「操作失败」—— 手机屏幕小，用户唯一能行动的就是那句话。

| 情况 | HTTP | 手机显示 |
| --- | --- | --- |
| 没带令牌 / 令牌错 | 401 `unauthorized` | 令牌已失效，请在电脑上重新配对 |
| 会话不存在（被删了） | 404 `no_session` | 这段对话已经不在了 |
| 这一轮正在跑（`send`） | 409 `turn_running` | 它正在跑；插话或打断？ |
| 没在跑（`steer`） | 409 `not_running` | 这一轮已经结束了，直接发送？ |
| 定时任务轮不接受插话 | 409 `steer_unsupported` | 这轮不接受插话，等它结束或打断 |
| 待答项已被处理 | 409 `no_such_pending_request` | 已经在别处回答过了 |
| 只读会话（子系统所有） | 409 `read_only` | 这段对话由别的任务占用 |
| 节点任务正在执行 | 409 `held_by_task` | 有一项远端任务正在使用它 |
| 这段对话属于某个 IM channel，而宿主里没有那个运行时 | 409 `channel_offline` | 这台机器现在没有跑那个渠道，等它起来 |
| 对着 channel 的会话发了一条「答复走渠道」的命令（/model、/stop…） | 409 `channel_command` | 这类命令在原渠道里发 |
| 对着 channel 的会话发了消息，但这一轮已被渠道那边结束 | 409 `turn_running` / `not_running` | 同普通会话 |
| 删除一段正在跑的对话 | 409 `turn_running` | 它正在跑；插话或打断？ |
| 删除失败（写盘出错等） | 500 `delete_failed` | 删不掉这段对话 |
| 这台电脑建不出 agent（还没配 provider） | 409 `agent_unavailable` | 这台电脑还没配好模型，先在电脑上配置 |
| 项目已经不在了（或它的目录不可用） | 409 `no_project` | 这个项目已经不在了（或它的目录不可用） |
| 工作目录不能用 | 400 `bad_working_dir` | 这个目录用不了，换一个路径试试 |
| 建会话失败（写盘出错等） | 500 `create_failed` | 没能建出这段对话，在电脑上看看日志 |
| 置顶没写进去 | 500 `pin_failed` | 置顶没保存成功，再试一次 |
| 标题是空的 | 400 `bad_request` | 名字不能是空的 |
| 改名没写进去 | 500 `rename_failed` | 没改成功，在电脑上看看日志 |
| 定时任务不存在 | 404 `no_job` | 这个定时任务已经不在了 |
| 定时任务在设置里被关掉 | 409 `cron_disabled` | 定时任务在电脑的设置里被关掉了 |
| 任务不属于这台电脑 | 409 `not_desktop` | 这个任务不归这台电脑管 |
| 定时任务的写入失败 | 500 `cron_failed` | 操作没成功，在电脑上看看日志 |
| 那台电脑的操作系统不肯「别睡」 | 500 `awake_failed` | 这台电脑没能保持唤醒 |
| 上传的文件超过这台机器的上限 | 413 `文件太大（60 MB，上限 50 MB）` | 原样显示这句话 |
| 上传的图片超过 5 MB | 413 `图片太大（6 MB，上限 5 MB）` | 原样显示这句话 |
| 传的是 HEIC/HEIF 照片 | 415 `unsupported_image` | 这张是 HEIC，先转成 JPG/PNG |
| 文件没存下来（磁盘出错等） | 500 `store_failed` | 文件没存下来，在电脑上看看日志 |
| 发送时点了一个这个会话里不存在的文件 | 409 `no_such_upload` | 文件没传上去，删掉它重新选 |
| 「保持唤醒」的时长越界 | 400 `bad_request` | 时长要在这台电脑接受的范围内 |

## WebSocket

连上之后，客户端先订阅一个会话，然后收增量。

```
手机 → {"type":"open","sessionId":"…"}
服务端 → {"type":"snapshot", …}      ← 先给一份完整快照
服务端 → {"type":"text", …}          ← 然后是增量
手机 → {"type":"ping"}               ← 每 20 秒
服务端 → {"type":"pong"}
```

- **一次只订阅一个会话**；第二次 `open` 替换订阅。`sessionId` 为**空串**表示「退订，我回列表了」：电脑停掉这条流、不回快照。少了这一步，读者离开的那段对话还在源源不断推过来（一轮在跑就是每秒几十条小事件），全落在一个没人看的画面上 —— 列表页划起来卡，源头往往在这儿。
- `snapshot` 就是下面那个 `Snapshot`。
- **只有订阅了某个会话的连接才收它的事件**；例外是 `ask_answered`（很小，且可能属于任何会话）。
- 两分钟没有心跳就关连接（客户端切前台时重连）。

**事件（服务端 → 手机）**，全部是投影，没有整段历史：

| type | 字段 | 什么时候 |
| --- | --- | --- |
| `snapshot` | `snapshot` | 每次 `open` |
| `text` | `text` | 助手的文字，增量 |
| `thinking` | `text` | 思考增量（默认折叠） |
| `tool_start` | `toolId`, `toolName`, `title` | 工具开始 |
| `tool_args` | `toolId`, `args` | 参数（可能分次） |
| `tool_result` | `toolId`, `isError`, `result`, `truncated` | 工具结束，结果**截断到 2 KiB** |
| `turn` | `durationMs`, `iterations`, `end` | 一轮结束，`end` 是 `complete`/`interrupted`/`error` |
| `error` | `message`, `interrupted` | 出错 |
| `idle` | `end` | 真的一轮结束了（手机据此补发排队中的消息） |
| `compact_start` / `compact_done` | `error?` | 压缩开始 / 结束。**成功之后还有一步换轨，见 `session_moved`** |
| `session_moved` | `from`, `to`, `end` | 这段对话搬到另一个会话 id 了（压缩成功把它写进子会话）。订阅是跟着 id 走的：手机要把 `openId`、WS 订阅、还没发出去的队列一起搬过去。`end` 是这一轮为什么结束 —— 换轨之后的 `idle` 发往新 id，手机多半收不到，队列要靠它决定发不发（`complete` 就发，被打断/出错就留着） |
| `state` | `state` | 会话状态变了 |
| `tps` | `tps`, `lastTps` | 实时速度（工具边界之后变暗） |
| `usage` / `cost` | 见 `UsageInfo` | 会话金额、缓存命中 |
| `ask` | `ask` | 新增一条等待（权限或提问） |
| `ask_answered` | `toolId` | 一条等待已被回答 |
| `user_message` | `text`, `displayText`, `steer`, `origin` | **另一块屏**发来的用户消息 |
| `session_prefs` | `provider`, `skipApproval` | 这段对话的模型 / 审批开关被**某一块屏**改了（桌面或手机）；两种开关一起发 |
| `sessions_changed` | — | 会话列表变了：任何一块屏新建 / 删除 / **重命名 / 置顶**了一段对话。**广播给每一条连接**（列表不属于某一段会话的流）；手机收到后要重读列表，并把**打开着的那段**的标题、项目、置顶一并修掉 |
| `cron_changed` | `id` | 定时任务表变了：任意一块屏新建 / 改 / 暂停 / 恢复 / 删除，或者调度器跑完一次。**广播给每一条连接**；手机上重读面板（没打开就等下次打开，别去轮询） |
| `awake_changed` | `awake` | 这台电脑的「保持唤醒」变了：桌面托盘勾的、另一块屏改的、或者到点自己关的。**广播给每一条连接**，带着完整的 `AwakeState` —— 收到就照它画，不用再读什么 |
| `lagged` | — | 你跟不上了，重读快照 |

- 参数与结果都截断：一条 `ReadFile` 的结果可以有几十 KB，而手机屏幕本来就会折叠它们。**完整输出是桌面的事。**
- `origin` 是 `desktop` / `mobile` / `badge` / `cron`：一块屏改了共享状态要告诉另一块，收到时**自己发的那条**不重复画。
- `session_prefs` 没有 origin：两个开关是**幂等**的共享状态，谁改的都要落到每一块屏上，收到就照它更新即可（开关写的是会话记录，事件只是让它立刻可见）。

## 线上类型

字段名就是这些，两边照抄。`?` 表示可省。

```
SessionSummary  { id, title, projectName?, updatedAt, running, waiting, readOnly, heldByTask, pinned?,
                  channelSession?, channelName?, cron? }
                waiting 只有两句：「等待你的授权」「等待你的回答」，空串表示没有在等
                channelSession 表示这段对话属于某个 IM channel（手机在行上和标题旁边贴 tag，
                并**不允许**编辑/删除之外的操作）；channelName 是哪个（discord / weixin /
                插件自己的名字）。名字有两个来源，宿主取第一个能答的：会话记录上的
                `channel_name`（那个 channel 在这段对话里跑过一轮就写下了，所以熬得过重启），
                然后是本进程见过的 thread→channel 映射；两处都没有时为空 —— 那时 tag 只写「渠道」
                cron 是有几个**定时任务**写进这段对话（桌面侧栏那颗徽章的同一个数，按逻辑 id
                算，所以压缩链上数的是整条对话）；缺省 = 没有。手机在行上画一个小闹钟，
                多于一小时带条数
                updatedAt 是 **RFC3339**（带时区）。手机自己折成「刚刚 / N 分钟前 / HH:mm /
                昨天 / M月D日」——**别改成相对时间发过来**：那样「几点」只在发出去那一刻成立，
                列表停在手机上几分钟就会说谎
Message         { index, role, content, displayContent?, timestamp?, iteration?, seq?, turn?,
                  toolName?, toolCallId?, args?, title?, toolResult?, isError?, thinking? }
                role ∈ user / assistant / tool_call / tool_result / reminder
                index 是它在会话记录文件里的绝对位置 —— 翻页游标就是它
Panels          { mcp:[{name, type, connected, needsAuth, loaded, tools}],
                  agents:{peers:[{name, label, url, note, reach, skills[], err}], checkedAt, anyChecked},
                  cron:[{id, name, scheduleText, status, targetKind, targetTitle,
                         nextRun, lastRunStatus, lastRunError, running}] }
                mcp.loaded / mcp.tools 是「这段对话载入几个 / 这个 server 一共几个」
                agents.anyChecked=false 表示还没问过任何节点（不是「没有节点」）
                cron.targetKind ∈ session / pending / missing / channel
                channel 的那几条是**这段对话所属线程**的定时任务（只出现在那个会话的面板里），
                这一屏只读：跑一次需要拥有那个线程的渠道运行时，改它属于那段对话自己的命令
AwakeState      { on, durationMin, until }
                on 是开关；durationMin 是偏好（下次开启用多少分钟，0 = 不自动关）；
                until 是这次开着时到点自动关的时刻（RFC3339），空串 = 关着或不自动关。
                它属于**那台电脑**，不属于任何会话
ProjectRef      { id, name, primary, runnable }
                primary 是这个项目会带给会话的工作目录；runnable=false 表示目录已经不在了
PendingAsk      = PermissionAsk | QuestionAsk
PermissionAsk   { kind:"permission", toolId, toolName, permissionKind, preview }
                permissionKind ∈ bash / files / mcp_search（决定卡片外壳）
                preview 是 agent 的原文，手机不再重新拼
QuestionAsk     { kind:"question", toolId, questions:[{question, options?, multiSelect?}] }
AttachmentRef   { id, name, mimeType, size, downloadable }
                id 就是那次 SendFile 调用的 id
UploadedFile    { name, size, kind }
                name 是宿主存下来的名字（重名会加 -1、-2），发送时递的就是它
                kind ∈ image / text / binary —— @-展开会把它当成什么（只用来挑图标）
SessionState    { status, label, detail, waiting }
WorkspaceInfo   { primary, additional[] }
UsageInfo       { cost, credit?, cacheHitRate? }
ContextInfo     { estimate, window, parts:[{label, tokens}] }
Snapshot        { session, messages[], hasMore, state, asks[], model, skipApproval,
                  workspace, usage, context, attachments[] }
```

**答案的形状沿用桌面的约定**：键是**问题原文**，值是选中项（加自由文本）用 `", "` 连接 —— `answers[q.question] = parts.join(", ")`。不要发明第二种形状：模型看到的必须是同一段话。「跳过」是 `{decline: true}`，不是编一个选择出来。

**数组字段空的时候是 `[]`，不是 `null`。** Go 的 nil 切片默认序列化成 `null`，而客户端把它当数组用 —— 一边写出 `null`，另一边就会在某次渲染里崩掉（不是在类型检查里）。服务端每个数组字段都要保证非 nil。

## 不变量

1. **记录是权威的，实时流只延长尾巴。** 轮次结束、重连成功、收到 `lagged`，这三个时刻都要重读快照。这样不需要事件重放、不需要给增量编号，也不会拼错半句话。
2. **手机绝不能让 agent 卡住。** 每条连接一个 256 条事件的缓冲队列，满了发 `lagged` 并断开；最多接受 16 条连接（第 17 条在握手时拒绝）。发事件的 goroutine 不等任何人。
3. **线上只走投影**，没有 `Messages`，没有完整工具输出。
4. **手机改的每一件事都按会话寻址**，落在那段被点名的对话上，不碰桌面正在看的那一段。
5. **一块屏改了共享状态，要告诉另一块**（消息用 `user_message`、回答用 `ask_answered`）。
6. **列表的增删改都是广播，不属于某一段会话的流。** 任何一块屏新建、删除、重命名、置顶对话都发 `sessions_changed`，收到的人重读 `/v1/sessions`（并把打开着的那段的标题等一并修掉）。删除走桌面同一条 `DeleteSession`：正在跑的那一轮**被拒绝**，不是被停下来；置顶走同一条 `SetSessionPinned`。
7. **手机不改变桌面正在显示的会话。**
8. **一段对话可以换 id，订阅要跟着走。** 通知里带着这一轮的结束原因：换轨与 `idle` 之间没有别的延迟，换订阅一定晚一步，`idle` 靠不住。 压缩成功时 agent 把历史搬进**子会话**，这一轮剩下的部分接着在子会话里跑。桌面在这一刻换轨（`agent:session_switched`），手机收到 `session_moved` 后把画面、订阅、排队中的消息一起搬到新 id。不搬的手机停在旧那一半：画面不动、状态永远「在跑」、队列发不出去（`idle` 已经发往子会话了）。换轨发生在**一轮结束之后**，不是压缩那一刻 —— 中途搬会把正在写的消息从画面底下抽走。
9. **定时任务表是广播的。** 它不属于任何一段会话（唯一的例外是 channel 的：那些跟着**打开的那段**会话出现在面板里，见线上类型的 `targetKind`）：任何一块屏（窗口、手机、调度器自己）改了任务，都发 `cron_changed`，另一块屏重读 `/panels`。手机上三个动作走桌面同一条写入路径（`RunCronJobNow` / `SetCronJobPaused` / `DeleteCronJob`），所以两块屏对「这个任务现在是什么状态」不会各说一套。
10. **「保持唤醒」是一台机器的一个值，所以它也是广播的。** 它不属于任何一段会话，手机发的 `POST /v1/awake` 与桌面托盘的勾选走同一个写入方（桌面的 `setAwake`），改完立刻广播 `awake_changed`、带完整的 `AwakeState` —— 两块屏照它画，没有第二条同步路径。**开着的时候关不关得掉由那台电脑说了算**：系统的断言被拒时，答案是「关着」加一句原因（500 `awake_failed`），而**不是**把开关画成开着的。`durationMin` 是偏好、落在那台电脑上，所以它熬得过重启；`on` 不是 —— 进程没了，断言就没了，重启后的状态永远是关。
11. **手机送进来的文件只认名字，不认路径**（见「HTTP 端点」里附件那条）：上传回的 `name` 是发送时唯一能递的东西，服务端在那段会话的 `uploads/` 里解析。少这一步，「有令牌」就等于「能读这台机器上任何文件」。
12. **一台电脑可能有几条地址，手机全部记住、并发试。** 配对码是 `{"v":1,"url":…,"urls":[…],"token":…}`：`url` 是最先用的那条（旧客户端只读它，所以它不能删），`urls` 是那台电脑当前**全部**可达地址，局域网在前、Tailscale 在后。手机把它们一起存下来，连的时候**同时**去 ping，谁先应答用谁（`mobile/src/api/addresses.ts`）—— 顺序试的代价是「人在外面时每次都要先等局域网那条超时」。`/v1/ping` 的 `urls` 是同一份，每次连上都报一遍，所以桌面换了网络、开了 Tailscale，手机不用重新扫码就知道。**配对页走的是这同一条探测**（`probePairing`），而且它不是门禁：一条都不应答时也用完整地址表把设备存下来，只用来挑地址和取名字。**加一种新的地址 = 改桌面那一个发现函数**（`desktop/mobile_token.go` 的 `lanUsable` / `lanCandidateKind`），两块屏不要各自判一遍「哪些地址能用」。
13. **channel 拥有的会话（`channelSession`）：手机能读、能说，但那一轮由那个渠道的运行时跑。** 两边各有一份内存历史（channel 按线程、宿主按会话），谁写谁就必须是唯一那个写的人 —— 所以手机发来的消息被**交回**那个线程，走同一个 agent、同一份缓存、同一把线程锁，事件再投影回连接。由此得出的三条：① **没有那个运行时**（窗口版、或那个渠道没开）时明确拒绝 `channel_offline`，绝不退回自己跑一轮；② **同步命令**（`/model`、`/status` …，答复走渠道的那些）拒绝 `channel_command`；`/compact` 和技能激活是轮次形状的，照常可以发；③ 一轮在跑时这条会话报 `running`，手机的下一条会**并入**（steer），Stop 打到这一轮的 context 上；AskUser 的提问仍归渠道回答，不在手机上画第二张卡。手机**不能**反过来把这段对话的归属改成自己 —— 线程与会话是一对一的。
