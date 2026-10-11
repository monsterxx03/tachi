# Tachi —— @ 引用手册

`agent/atfile/`、`agent/sessionref`、前端的 `@` 选择器与它的展开那一步，以及粘贴图片这条路（`session.SavePastedImage`、`pkg/clipboard`、输入框的粘贴处理）。这里的麻烦都源于同一件事：一个引用在一个地方写出来、在另一个地方展开，两边必须对得上。

## 语法归一个扫描器所有，展开由前端做

- **语法只有一个人在认，而展开发生在调用 agent 之前**（`tui/commands_agent.go`、`desktop/agent_turn.go`）——所以加一种新的引用要同时接进**两个**前端，而选择器只能提供 `atfile.Expand` 读得回来的东西（`RefForPath`/`RefForSession` 就是为此存在：拼写归展开那一侧所有）。

## `@session:` 是指针，绝不是会话全文

- 文件会被内联（或被标注成 `[@file: path]`）；**`@session:<id>` 变成指向会话存储的一个指针——id、标题、记录条数、会话全文的路径，绝不含会话全文**（`agent/sessionref`，经 `atfile.Options.Sessions` 注入）。把另一个对话内联进来，等于在这个会话往后的整段对话里，拿它自己的上下文去装没人要求读的文本。

## 一次粘贴是无路径的字节

- **两个前端都把它存进会话自己的目录，再用 `@` 引用挂上去**（`session.SavePastedImage` → `<SessionDir>/<id>/pasted/`，还没有会话时用操作系统的临时目录）——拖进来的文件走的是同一条路：发送时展开成一个 content part，并随消息一起记下（所以重新加载后图片还在——引用只展开一次）。

## 终端的粘贴通道只承载文本

- **剪贴板里的图片永远到不了 `tea.PasteMsg`**——终端把这个手势报成不可执行，什么都不发——所以 TUI 自己去读操作系统的剪贴板（`pkg/clipboard`；macOS 上走 osascript/AppKit，因为 `pbpaste` 只处理文本——它的 `-Prefer` 只认 txt/rtf/ps——Linux 上走 wl-paste/xclip，字节按 magic number 校验）。`ctrl+v` 就是这次读取，也顺带粘贴文本；一个**空的** `tea.PasteMsg` 走同一条路，为的是那些真的会上报这个手势的终端。

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md)（选择器、输入框的粘贴处理）、[session-store.md](session-store.md)（一个会话的记录和 blob 放在哪里）。
