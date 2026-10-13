# Tachi —— 检查点手册

`agent/checkpoint/` 与所有会解析工作区根目录的地方（换目录、增删附加根、桌面项目编辑、切 worktree）都在这份文件里，压缩那条路也在——**一轮能不能写文件，取决于这里写着什么**。

## 一个根目录的身份是路径，绝不是位置

- **一个工作区根目录的身份是它的路径，不是它在集合里的序号。** 检查点记下自己覆盖的是哪棵树、哪个 store（`RootState.Root` / `.Store`），凡是要读已有检查点的地方都走 `repoFor(state)`；`currentRepo` 只给**本轮**打快照用。根集是**排过序、可变的**（换目录、增删附加根、桌面项目编辑、切 worktree），所以按序号去解析 store 或工作树，会悄悄把一个工作区的文件写进另一个——`parentRef` 按「路径 + store」匹配也是这个道理。`agent/checkpoint/rootchange_test.go` 钉住它，`docs/2026-09-14-desktop-project-design.md` §14 有完整交代。

## 一轮的绑定是会话级的

- **一轮的检查点绑定是会话级的，而自动压缩会在轮中途把会话换掉。** manifest 在会话自己的目录下，所以在父会话里开始的那一轮，对子会话等于不存在：这一轮剩下的时间里，每个能写的工具都被拒绝（「turn N was never begun」，设计使然——没被记下来的写，就是回退收不回来的写），直到这一轮结束。所以 `maybeAutoCompact` 在它刷新其他会话级取值（SDK session id）的地方顺手把这一轮重新绑定（`rebindCheckpointTurn`）；只换根目录不需要这一步，因为 manifest 本来就是每个会话共用的。`TestCheckpointRebindsAfterCompaction` 钉住它。


## 分支（fork）的切口也来自 manifest，但它不带走检查点

- **一个分支点就是一个轮次的 `Records`/`APIRecords`**（`agent.AIAgent.ForkSession` 读 `Record(turn)`）：和回退用的是同一个边界，所以两轮之间那条 reminder 包装记录不会在分支尾部留成孤块。分支只**读**这份 manifest。
- **分支不带任何检查点**：manifest 与快照 store 都住在会话自己的目录里，所以新会话从 turn 1 重新起算，拷来的那些轮次没有文件状态、回退不了，页脚也没有改动数字。把源会话的 manifest 拷过去是**错的**（那会让它的ref 指向另一个会话目录里的快照），而这也正是「不动检查点」的含义。
- **被拒绝的三种情形**（那一轮在跑、该轮没有记录、检查点没启用）都是当场给出的理由，不是等动作失败：分支只读历史，所以它不受「已被压缩接续」与「子系统拥有的会话」那两条回退禁令限制。

## 回退默认动两半，conversation-only 是唯一例外

- **`Rewind` 把工作区文件与对话一起退；`RewindConversationOnly` 只退对话、工作区一动不动**（新增文件不删、改过的文件不还原）。它是给「答案不对、但改好的代码要留着」的读者用的，是一条**显式的**路，不是失败模式的伪装。
- **这个模式必须由动作自己打戳**：`RewindPreview.ConversationOnly` 在 `Rewind`/`RewindConversationOnly` 里写上去——`PreviewRewind` 不知道（模式属于动作，不属于读取），而卡片与 `agent:rewound` 的提示都只认这个字段。两处都不能让「对话退回去了」读成「工作被撤销了」；文件那一半的失败文案也随模式改口（conv-only 根本没有「已还原文件」这回事）。
- **裁索引（`DropAfter`）与截断对话两半照旧属于 conv-only**：这是决定 17 的同一条不变量——对话已经不在，那些轮次就不能再被文件回退，否则就是「文件跳到放弃分支、对话截断是空操作、却报成功」。代价（之后不能把文件单独退回那几轮）是明知故犯。

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md)（项目与根集：桌面后端、桌面会话树）、[session-store.md](session-store.md)（一个会话的文件住在哪）。
