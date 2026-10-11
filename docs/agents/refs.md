# Tachi —— 引用手册（`@` 与 `#`）

`agent/atfile/`（语法与展开）、`agent/sessionref` 与 `agent/agentref`（两个选择器的行从哪来）、两个前端的引用选择器，以及粘贴图片这条路（`session.SavePastedImage`、`pkg/clipboard`、输入框的粘贴处理）。这里的麻烦都源于同一件事：一个引用在一个地方写出来、在另一个地方展开，两边必须对得上。

## 两个 mark，一套语法，一个路由规则

- **`@` 名字是路径，`#` 名字是实体。** 边界规则只有一条（`atfile.IsRefBoundary`：行首或空白之后开始，到下一个空白为止），但 `#` **只由 `AgentRefPrefix`（`a:`）和 `SessionRefPrefix`（`s:`）开门**——别的一律是普通文本，不查磁盘、不开弹窗。这就是 `#include`、`#123`、一个 Markdown 标题、`C#` 不会被误伤的原因，也意味着加第三种实体引用要同时改拼写前缀与选择器的候选列表。
- **语法归 Go 所有，前端只负责插入与路由。** 引用文本由 `RefForPath` / `RefForAgent` / `RefForSession` 渲染，展开由 `atfile.Expand` 做（调用点在 `tui/commands_agent.go`、`desktop/agent_turn.go`——**加一种引用要同时接进两个前端**）；而「这串 keystroke 属于哪个命名空间」只有 `atfile.HashQueryNamespace` 一个答案，桌面那份是 `lib.ts` 的 `hashNamespace` 镜像。**一份东西两个说法，就是这两边开始互相不认的方式。**
- **一条消息里可以有任意多个引用，两种 mark 混着也行**：扫描器逐词处理，所以每个引用各自展开；前端的游标扫描（`atfile.LastRefStart` / `lib.ts` 的 `atRefAt`）反过来找**光标所在的那一个**——多引用不是特例，只有在弹窗里假设「只有一个引用」的代码才会把它当特例。计数（选择器那句「已引用 N 个」）与 `#` 用同一条前缀规则，否则 `#include` 会把数字算虚。

## `#s:` 与 `#a:` 都是指针，绝不是内容

- **`#s:<id>` 变成指向会话存储的一个指针**：id、标题、记录条数、会话全文的路径，绝不含会话全文（`agent/sessionref`，经 `atfile.Options.Sessions` 注入）。把另一个对话内联进来，等于在这个会话往后的整段对话里，拿它自己的上下文去装没人要求读的文本。
- **`#a:<alias>` 变成一行**：别名（`DelegateTask` 的 `node` 参数要填的就是它，所以必须原样给出）、本机 pin 住的那个 `node_id`（对端唯一不能自称的东西，design §4.2）、以及「用 `DelegateTask(node=…)` 派活」这句提示。解析出来的 id 为空时**如实说「身份未知」**，绝不用对端自报的名字填这一格——任何密钥都能声称任何名字。
- **可达性不进展开文本。** 那是**一刻**的事实，而展开文本会在会话历史里躺到天荒地老，后来读到的人只会当成仍然成立。它只出现在选择器的行上（`agentref.ReachWord`：`可用` / `联系不上` / `检查中…`——第三项不是「连不上」的委婉说法）。

## 节点列表：本地即时，可达性在后台

- **选择器一次都不等网络。** 行来自 `node.PeerList.Snapshot`：配置里的别名与 note、`peer-identities.json` 里 pin 住的身份，都是本地文件；可达性是**上次后台探测**的结果（`Checked=false` 就是「还没看过」）。
- **按键就是催探测的时刻**（`agentref.Source.Warm`，TUI 在 `atRowsFor` 的 `#` 分支、桌面在 `syncAtRef` 里打 `#` 时调）：一次探测每个节点一个往返，而读者接下来敲的那几下正好花在它上面。`PeerList.Probe` 自带 in-flight 去重与 `probeInterval`，所以一串 keystroke 不会变成一串请求。
- **探测并行取 card**（`peerDirectory.Snapshot`，每个 peer 一个 goroutine、结果保持配置顺序）。串行的代价全在**失败**那一侧：一个下线的节点要靠超时才知道，三个就是三份超时接起来。
- **探测函数由入口点注入**（它已有的 sender，而不是在这里另建一个）：新建 sender 会顺带**创建身份与自签证书**，为了画一个列表去发密钥是把顺序搞反了。
- **选择器与节点面板读的是同一份 `PeerList`**（`Snapshot` 给行、`Details` 给卡片），所以两者不可能对「谁活着」各说一套。面板的「刷新」是**同步**的并且先作废 card 缓存：一个人按了按钮，用五分钟前的缓存回答等于回答另一个问题（`node.md` 有机制，`desktop.md` 有面板那一侧）。

## 一次粘贴是无路径的字节

- **两个前端都把它存进会话自己的目录，再用 `@` 引用挂上去**（`session.SavePastedImage` → `<SessionDir>/<id>/pasted/`，还没有会话时用操作系统的临时目录）——拖进来的文件走的是同一条路：发送时展开成一个 content part，并随消息一起记下（所以重新加载后图片还在——引用只展开一次）。

## 终端的粘贴通道只承载文本

- **剪贴板里的图片永远到不了 `tea.PasteMsg`**——终端把这个手势报成不可执行，什么都不发——所以 TUI 自己去读操作系统的剪贴板（`pkg/clipboard`；macOS 上走 osascript/AppKit，因为 `pbpaste` 只处理文本——它的 `-Prefer` 只认 txt/rtf/ps——Linux 上走 wl-paste/xclip，字节按 magic number 校验）。`ctrl+v` 就是这次读取，也顺带粘贴文本；一个**空的** `tea.PasteMsg` 走同一条路，为的是那些真的会上报这个手势的终端。

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md)（选择器、输入框的粘贴处理）、[node.md](node.md)（这个机器有哪些节点、pin 住的身份是什么）、[session-store.md](session-store.md)（一个会话的记录和 blob 放在哪里）。
