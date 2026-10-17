# Tachi —— 压缩手册

压缩这条路都在这份文件里：`agent/agent_compact.go`（`doCompact`）、`agent/agent_loop.go`（`maybeAutoCompact`，自动压缩的那次触发）、`agent/compact.go`（`CompleteCompact` / `FinalizeCompact`）、`agent.CompactRunOptions`、任何前端的 `/compact`（tui、desktop、acp、channel），以及 `AgentEventAutoCompact*` 的处理器。

## 目录

| 章节 | 什么时候读 |
| --- | --- |
| **两个触发器，一套机制** | 你动任何前端的 `/compact`、自动压缩那次触发，或一次把工具全藏起来的运行 |
| **子会话的记录** | 你动 `FinalizeCompact`，或给会话 meta 加字段 |
| **前端欠这次交接什么** | 你动某个前端的压缩命令，或 `AgentEventAutoCompact*` 的处理器 |

## 两个触发器，一套机制

- **手打的 `/compact` 就是一轮普通对话**，它的用户消息本身就是压缩指令（`agent/commands.BuildCompactInstruction`）——模型必须**看见**整段历史才总结得出来，侧信道调用做不到这一点。循环的自动压缩（`maybeAutoCompact`，阈值在 `compact.threshold`）是另一个触发器。两者都止于 `FinalizeCompact`：对话搬进一个**子**会话，子会话的记录以摘要和「请基于以上摘要继续对话。」开头，父会话留着压缩前的历史，两者互链（`compacted_child_id` / `compacted_parent_id`）。
- **一次压缩运行带着 `agent.CompactRunOptions()`——两半都要，而第二半才是承重的那半。** 隐藏工具让回复保持为文本；**把本循环自己的自动压缩关掉**才是让指令留在模型眼前的那一半。陷阱：压缩运行的历史**按定义**就已经越过阈值（不然读者也不会打 `/compact`），于是循环会先在自己这一轮的顶上压缩、用摘要替换掉 `rs.Messages`——请求里就再也没有那条指令了。模型于是回答一段延续，而前端把这个回复当成摘要收下，又压缩了**第二**次：两个新会话，读者最终落在的那个，摘要是对话的一个片段。桌面冒烟场景 `compact-race` 钉住它。
- **一次把工具全藏起来的请求，可能连模型自己的工具调用标记都当文本收回来**（某款 DeepSeek 系模型会把它原生的 `｜｜DSML｜｜` 块写进普通内容，而 Tachi 逐字渲染——没有清洗器）。无工具的运行没东西拿它去解析，所以一次无工具运行必须是「答案按构造就是文本」的那一种；`WithNoTools()` 在全仓库只有 `CompactRunOptions` 一个调用点（另一类隐藏工具的运行——节点那次受管执行——限制工具集用的是 `WithToolSet(names…)`），而它并不是把一次运行标记为压缩的东西。

## 子会话的记录

- **`FinalizeCompact` 不拷贝的会话 meta 字段，全都被悄悄置零**，而且不会报出来。每一个都有自己的坏法：`AdditionalDirs`/`ProjectID`（工具、提示词和检查点在一棵错的树里彼此自洽）、`Pinned`（折叠掉的那一行上有个看不见的状态，外加对会话清理的永久豁免）、`Kind`/`SkipDream`/`SourceNodeID`（一个远端任务回来时变成用户自己的本地对话——就来源而言，是一份不再说明是谁干的活的记录）。给 meta 加字段，就是要决定它随不随对话走：`agent/compact_test.go` 钉住路径字段，`agent/managed_test.go` 钉住那三个把会话标记为远端执行的字段。 `SkipApproval` 是**故意**不跟着走的那一个（一次新的对话，没人被问过它要不要审批），而它的坏法是反过来的：agent 那一半不会自己知道记录变了，于是新会话顶着「需要审批」却被照旧放行——所以它由**桌面**在搬家那一刻重新指向（`followCompaction`），钉在 `desktop/compaction_test.go`。
- **`ThreadID` 和 `Pinned` 是「搬走」，不是拷贝**——父会话在同一步把它们放开。一个绑定，只由它仍然有意义的那个记录持有：一份留在父会话上的 thread 绑定，会让 `FindByThreadID` 解析到压缩前的历史，压缩之后每一轮过去都会再触发一次压缩，就是这么来的。

## 前端欠这次交接什么

- **前端的压缩命令，绝不能去完成一次会话已经搬走的压缩。** 它们共用一个形状——跑压缩那一轮、把回复当摘要、调 `CompleteCompact`——而当循环反正已经压缩过时，每一个都得拒绝后半段：
  - desktop 盯着该 run 自己的流，等一个成功的 `AgentEventAutoCompactDone`，然后不做交接就返回；事件处理器已经写下的那条记录，就是 `endTurn` 的 `followCompaction` 认的那条（处理器在 `desktop/commands.go`，`followCompaction` 在 `desktop/compaction.go`）；
  - TUI 留着 `Model.autoCompacted`——同一个事件置位，轮开始时清空——并在 `TurnComplete` 丢弃压缩前的 `savedHistory` 而不是恢复它（`tui/model_events.go`）。恢复就是把这**旧**历史摆到一个在磁盘上已是子会话的会话前面：下一个请求会把它发出去、再次越过阈值，读者的 `/compact` 看起来就像什么都没干。
  有 `CompactRunOptions` 在，这两条路都到不了；它们是它底下的兜底，而不是那个修复。
- **`compact`、`compact-race`、`auto-compact` 是这条路线的冒烟场景**——这里任何改动都要三个都跑（`make desktop-smoke ARGS="-run compact,compact-race,auto-compact"`）。
