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
- 地址：`0.0.0.0:8787`（默认）。只在自己家的网络里用。

## HTTP 端点

全部在 `/v1` 下。

| 方法 | 路径 | 请求 | 返回 |
| --- | --- | --- | --- |
| GET | `/v1/ping` | — | `{ok, name, version}`，配对页用它验证地址与令牌 |
| GET | `/v1/sessions` | — | `{sessions: [SessionSummary]}` |
| GET | `/v1/providers` | — | `{providers: [{name, model}]}`，可切换的模型 |
| GET | `/v1/sessions/{id}/page` | `?before=<index>&limit=<n>` | `{messages: [Message], hasMore, session}` |
| POST | `/v1/sessions/{id}/messages` | `{text, mode}` | `{accepted: true}` |
| POST | `/v1/sessions/{id}/stop` | — | `{accepted: true}` |
| POST | `/v1/sessions/{id}/provider` | `{name}` | `{accepted: true}` |
| POST | `/v1/sessions/{id}/skip-approval` | `{skip}` | `{accepted: true}` |
| POST | `/v1/sessions/{id}/permissions/{toolId}` | `{decision}` | `{accepted: true}` |
| POST | `/v1/sessions/{id}/questions` | `{answers}` 或 `{decline: true}` | `{accepted: true}` |
| GET | `/v1/sessions/{id}/files/{attachmentId}` | — | 文件本身 |

`mode` 是 `send` / `steer` / `interrupt`；`decision` 是 `allow_once` / `deny`。

- **翻页游标是记录序号**：`before` 传上一次拿到的最旧一条的 `index`，服务端返回序号**严格小于**它的那 `limit` 条。不用时间戳 —— 消息没有自己的 id，而时间戳会撞，同一刻的两条落在页边界上会被整段跳过。
- **附件端点不接受路径**：`attachmentId` 就是那次 `SendFile` 调用的 id，只回记录里反查，查不到就是 404。记录里的 `path` 可能是相对的，按那次调用所属会话的工作目录解析。这样「令牌 = 任意文件读取」这个洞从一开始就不存在。

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
| 这台电脑建不出 agent（还没配 provider） | 409 `agent_unavailable` | 这台电脑还没配好模型，先在电脑上配置 |

## WebSocket

连上之后，客户端先订阅一个会话，然后收增量。

```
手机 → {"type":"open","sessionId":"…"}
服务端 → {"type":"snapshot", …}      ← 先给一份完整快照
服务端 → {"type":"text", …}          ← 然后是增量
手机 → {"type":"ping"}               ← 每 20 秒
服务端 → {"type":"pong"}
```

- **一次只订阅一个会话**；第二次 `open` 替换订阅。
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
| `compact_start` / `compact_done` | `error?` | 压缩开始 / 结束 |
| `state` | `state` | 会话状态变了 |
| `tps` | `tps`, `lastTps` | 实时速度（工具边界之后变暗） |
| `usage` / `cost` | 见 `UsageInfo` | 会话金额、缓存命中 |
| `ask` | `ask` | 新增一条等待（权限或提问） |
| `ask_answered` | `toolId` | 一条等待已被回答 |
| `user_message` | `text`, `displayText`, `steer`, `origin` | **另一块屏**发来的用户消息 |
| `session_prefs` | `provider`, `skipApproval` | 这段对话的模型 / 审批开关被**某一块屏**改了（桌面或手机）；两种开关一起发 |
| `lagged` | — | 你跟不上了，重读快照 |

- 参数与结果都截断：一条 `ReadFile` 的结果可以有几十 KB，而手机屏幕本来就会折叠它们。**完整输出是桌面的事。**
- `origin` 是 `desktop` / `mobile` / `badge` / `cron`：一块屏改了共享状态要告诉另一块，收到时**自己发的那条**不重复画。
- `session_prefs` 没有 origin：两个开关是**幂等**的共享状态，谁改的都要落到每一块屏上，收到就照它更新即可（开关写的是会话记录，事件只是让它立刻可见）。

## 线上类型

字段名就是这些，两边照抄。`?` 表示可省。

```
SessionSummary  { id, title, projectName?, updatedAt, running, waiting, readOnly, heldByTask }
                waiting 只有两句：「等待你的授权」「等待你的回答」，空串表示没有在等
Message         { index, role, content, displayContent?, timestamp?, iteration?, seq?, turn?,
                  toolName?, toolCallId?, args?, title?, toolResult?, isError?, thinking? }
                role ∈ user / assistant / tool_call / tool_result / reminder
                index 是它在会话记录文件里的绝对位置 —— 翻页游标就是它
PendingAsk      = PermissionAsk | QuestionAsk
PermissionAsk   { kind:"permission", toolId, toolName, permissionKind, preview }
                permissionKind ∈ bash / files / mcp_search（决定卡片外壳）
                preview 是 agent 的原文，手机不再重新拼
QuestionAsk     { kind:"question", toolId, questions:[{question, options?, multiSelect?}] }
AttachmentRef   { id, name, mimeType, size, downloadable }
                id 就是那次 SendFile 调用的 id
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
6. **手机不改变桌面正在显示的会话。**
