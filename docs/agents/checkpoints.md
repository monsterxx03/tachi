# Tachi —— 检查点手册

`agent/checkpoint/` 与所有会解析工作区根目录的地方（换目录、增删附加根、桌面项目编辑、切 worktree）都在这份文件里，压缩那条路也在——**一轮能不能写文件，取决于这里写着什么**。

## 一个根目录的身份是路径，绝不是位置

- **一个工作区根目录的身份是它的路径，不是它在集合里的序号。** 检查点记下自己覆盖的是哪棵树、哪个 store（`RootState.Root` / `.Store`），凡是要读已有检查点的地方都走 `repoFor(state)`；`currentRepo` 只给**本轮**打快照用。根集是**排过序、可变的**（换目录、增删附加根、桌面项目编辑、切 worktree），所以按序号去解析 store 或工作树，会悄悄把一个工作区的文件写进另一个——`parentRef` 按「路径 + store」匹配也是这个道理。`agent/checkpoint/rootchange_test.go` 钉住它，`docs/2026-09-14-desktop-project-design.md` §14 有完整交代。

## 一轮的绑定是会话级的

- **一轮的检查点绑定是会话级的，而自动压缩会在轮中途把会话换掉。** manifest 在会话自己的目录下，所以在父会话里开始的那一轮，对子会话等于不存在：这一轮剩下的时间里，每个能写的工具都被拒绝（「turn N was never begun」，设计使然——没被记下来的写，就是回退收不回来的写），直到这一轮结束。所以 `maybeAutoCompact` 在它刷新其他会话级取值（SDK session id）的地方顺手把这一轮重新绑定（`rebindCheckpointTurn`）；只换根目录不需要这一步，因为 manifest 本来就是每个会话共用的。`TestCheckpointRebindsAfterCompaction` 钉住它。

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md)（项目与根集：桌面后端、桌面会话树）、[session-store.md](session-store.md)（一个会话的文件住在哪）。
