# 小窗（HUD）：把一轮的回复摆到屏幕角上

> 状态: **两段都已落地**（主机 + 前端 + 设备固件），协议版本从 1 提到 2。设备侧的门禁
>       （`validate.sh --static` / `--firmware`）都过了，**只剩烧录与真机验收**（§6.5）。
> 关联: [desktop/hud.go](../desktop/hud.go)、[desktop/frontend/src/hud.tsx](../desktop/frontend/src/hud.tsx)、
>       [desktop/agent_turn.go](../desktop/agent_turn.go)（`AgentEventTurnComplete` 那个分支）、
>       [desktop/link/link.go](../desktop/link/link.go)、[2026-09-28-agent-badge-link-design.md](2026-09-28-agent-badge-link-design.md)、
>       [2026-09-29-badge-voice-answers-design.md](2026-09-29-badge-voice-answers-design.md)
> 设备侧仓库: `FoloToy/ai-passport`（`main/main.c` 的 `handle_key`、`main/badge_proto.h`）

## 1. 要解决的问题

设备那条路（语音发消息、按键批权限）打通之后，人不在 Tachi 窗口前也能**说**和**批**，但
**看不到结果**：

- 设备那块 240×320 的屏装不下一段回复（状态副标题只有 96 字节，而且是给「跑到哪一步」用的）；
- Tachi 的窗口可能不在前台——macOS 通知只说「回合完成」，那是回窗口的信号，不是内容。

所以补一块**主机侧**的置顶小面板：一轮跑完时，把这条回复摆到屏幕角上，看一眼就知道它说了什么，
不用切窗口、也不占设备那块小屏。

## 2. 它长什么样

- 位置：主屏工作区右上角（每次显示时按当前 WorkArea 摆，见 `placeHUD`）。
- 尺寸：560×300，无边框、不可缩放（宽度按「一行放下 34 个汉字 / 一行代码不折」定的）。
- 内容：一行会话名 + 这条回复的正文（纯文本、保持换行、可滚动）。
- 关闭：面板上的 ×，或 Esc。设备遥控是第二段的事。

正文走**和 transcript 同一套渲染管线**（`MarkdownBlock`：react-markdown + gfm + 代码高亮，
` ```mermaid ` 由 markdown.tsx 懒加载画成图），排版也是同一批 CSS（`widgets.css` 的
`.assistant-text`），`hud.css` 只按面板尺寸把行距/段距各收一档。

```mermaid 画成图，但**不给放大手势**（`zoomDiagrams={false}`）：那个「点击全屏查看」的覆盖层是长在
容器里的，而面板只有 560×300——开出来挤得没法看，不如没有这个手势（transcript 与文件预览照旧可点）。

图片按**这条回复所属会话的工作目录**解析（`AgentService.GetSessionWorkingDir(sessionId)`，与
transcript 用的是同一个 binding）——两处对「这张图在哪」的答案必须由同一处算出来，否则会出现
「窗口里能看到、小窗里是个破图」。

## 3. 三条性质（做的时候最容易做错的地方）

| 性质 | 为什么 | 怎么保证 |
| --- | --- | --- |
| **不抢焦点** | 它出现的时刻正是用户在别处全屏看东西的时刻。普通 NSWindow 的 `Show()` 是 `makeKeyAndOrderFront`，会激活 app、把全屏视频顶掉 | `MacWindowClassPanel` + `PanelPreferences.NonActivating`（NSPanel + `NSWindowStyleMaskNonactivatingPanel`）；Wails 对这类窗口的 `Show()` 是 `orderFrontRegardless` |
| **压在全屏 app 之上** | 否则它会在另一个 Space 上出现，等于没弹 | `CollectionBehavior` = `CanJoinAllSpaces｜FullScreenAuxiliary｜IgnoresCycle` |
| **不是第二个会话视图** | 载荷越像 transcript，就越会和 transcript 不一致 | 载荷只有 `{sessionId,title,text}`；渲染是 `white-space: pre-wrap` |

前两条**在界面上看不出来**：丢了它们，表现只是「这窗口有时候挺烦」或者「有时候看不见」，
而没人会想到去查窗口类——所以 `hud_test.go` 钉的是这两组参数，不是像素。

## 4. 什么时候弹（`hudWanted`）

五个条件同时成立才弹，每一条都是「不弹」的理由：

| 条件 | 不弹的理由 |
| --- | --- |
| **连着 badge 设备** | 这块面板是**设备的遥控屏**（说话、上下翻页、确定关窗都在 badge 上）。设备不在线时它只是一个没法操作的浮层——那正是最该省掉的那一块 |
| 不是脚本运行（`demoNoActivate`） | smoke 套件跑在别人的机器上，而它本来就设计成不抢前台；一块浮在最上面的面板正好是那里最不该出现的东西 |
| 主窗口**没有**焦点 | 你正看着窗口：transcript 上已经写着它了，再弹一块只是噪声（与通知、提示音同一条判据） |
| 跑完的是**当前会话** | 后台会话归通知管；而且从设备发出去的话只进当前会话，「刚才那句话的回音」永远在当前会话上 |
| 这一轮**有正文** | 模型只调工具没说话时弹一块空面板，只会让人以为坏了——那一声「跑完了」的提示音已经说完了该说的话 |

正文取的是**这一轮**最后一条有文字的 assistant 消息：`lastTurnReply` 往回走时撞到 `user` 就停。
`ev.Messages` 是整条会话的历史（它同时还要写回 `r.history`），所以不设这个边界就会在「这一轮没有
文字」时把**上一轮**的回答重新念一遍——那比不弹更像坏了。

## 5. 事件（两个窗口之间的全部接口）

| 事件 | 方向 | 载荷 |
| --- | --- | --- |
| `agent:hud` | Go → 前端 | `{sessionId, title, text}` |
| `hud:scroll` | Go → 前端 | `{delta}`（一次翻一屏，第二段的设备遥控用） |
| `hud:ready` | 前端 → Go | 无：页面挂载完了，把手里的当前内容发过来 |
| `hud:close` | 前端 → Go | 无：用户点了 × 或按了 Esc |

`hud:ready` 不是多余的：小窗在启动时就创建好并一直隐藏着（页面因此早就挂上了监听），但启动后
第一轮如果跑得比页面加载还快，那次载荷是发进一个没有监听者的页面的。

**开/关的状态归主机**：前端只发 `hud:close`，隐藏由 `hideHUD` 做——因为第二段的设备遥控要读它
（「设备现在按上下的意思是翻小窗，还是翻它自己的屏」全看这里），两处各存一份就是两处会不一致。

两个窗口用的是**同一个页面**：`main.tsx` 按 `?hud=1` 分叉。主题、字体、CSS 变量因此天然只有一份。

## 6. 第二段：用设备遥控小窗（已落地）

### 6.1 那些手势现在归谁

设备状态屏（没有待答项时）的按键分工：`上/下` 短按**空着**、`下` 长按空着、`上` 长按 = 忘记配对的
电脑、`确定` 短按 = 说话（按下开始、松开发送）、`确定` 双击 = ~~看一眼这个会话的账~~ → **叫出小窗**。
（`main.c` 的 `handle_key`。）

### 6.2 两条协议消息（这是 `Proto` 从 1 提到 2 的原因）

- **host → device `{"t":"hud","seq":N,"open":bool}`**：小窗开着没有。它是一条**要同步的状态**，
  挂在 `Snapshot.Hud` 上，走和会话列表同一套推送去抖；设备据此决定底栏那一句写什么，以及
  短上/短下要不要发上来。
- **device → host `{"t":"key","key":"up|down|ok|ok_double"}`**：按键**手势**。含义在主机
  （`desktop/badge.go` 的 `Key`）：up/down 翻内容、ok 关窗、ok_double 叫回来。

`info` / `badge_info` 连同设备那张账目屏一起删掉了（那是这次不兼容的原因）。

**「叫出小窗」显示哪一条**：当前会话历史里最后一条回复**优先**，屏幕上显示过的那条兜底
（`hudSummon`）。先说反的那一面：如果只看「显示过的」，面板停在 R1 上、你回窗口又跑了一轮 R2
（那次窗口在前台、所以没弹），叫出来的会是 R1——而这种错很轻，你不会觉得是 bug，只会以为自己
记错了。只按历史也不行：会话在这个进程里还没 Load 过时它是空的，那时 `hud.last` 是唯一拿得到的
东西。（这一条是实测出来的：重启之后一次面板都没弹过，设备上按双击确定，主机答的是「还没有可
叫回来的回复」——链路是通的，只是我原来只认「显示过的那条」。）

### 6.3 设备侧的按键规则

- **小窗开着**：短上/短下 = 翻内容；**确定（短按）= 关窗**；双击确定也 = 关窗（同一件事的反方向）。
  底栏那一格改成 `上下翻 · 确定关小窗`——它同时是「它开着」的状态说明（屏幕上本来没有别的地方能说）。
- **小窗关着**：短上/短下**什么都不发**（主机那边无事可做）；双击确定 = 叫出小窗。
- **按住确定照旧说话**，小窗开着也一样：靠的是固件本来就有的那条规则——短于
  `BADGE_VOICE_MIN_MS`（400ms）的按住整段丢掉，连 `voice_begin` 都不发（`badge_voice.h`）。
  所以「点一下」是空的、可以当按键用，而「按住」仍然是说话。
- 只有主屏（没有待答项）上做这套路由：有人等你回答时那几个键的意思是**回答**。

### 6.4 拆掉信息屏（整块）

设备：`BADGE_KEY_INFO` 手势、`badge_state_info_*`、`BADGE_UI_INFO` 那一屏与排版、
`badge_proto_info_request`、`BADGE_MSG_INFO` 的解析、`BADGE_INFO_MS`。
主机：`link.Source.SessionInfo`、`desktop/sessioninfo.go`、`link` 里那条 `info` 入站消息与
`badge_info` 回包。

`desktop/sessioninfo.go` 里活下来的只有菜单栏用的 `trayContext`/`formatCost`/`contextPercent`/
`toFloat`，它们搬到了 `desktop/tray.go`。

## 7. 未做的取舍

- **多个显示器**：面板贴在**它自己所在那块屏**的工作区右上角（`GetScreen` 问的是 AppKit）。新窗口
  默认出现在主屏，所以第一次弹就是主屏右上角；不跟随鼠标走的那个屏。
- **markdown 与图片**：不渲染（见 §2）。
- **smoke 覆盖**：driver 读的是**主窗口**的 DOM，而这是另一个窗口的页面，套件够不着。所以覆盖停在
  Go 侧单测（`hud_test.go`：闸门、取正文的边界、窗口参数、摆位的两处单位换算），要人眼看的只有
  一件事——它出现在全屏视频之上、且没有抢走前台。手工看它用 `TACHI_HUD_PREVIEW=1`（不用跑一轮）。
- **TTS**：把回复念出来（`say`）在「人不在电脑前」这个场景下更彻底，但那和「在电脑前只是没看窗口」
  是两种需求，先不做。

## 8. 落地时踩到的两个坑（都已修，记在这里免得再踩）

1. **`SetPosition` 写在 `Show()` 之前等于没写**：窗口的 native 实现是第一次 `Show()` 时创建的，而
   `SetPosition` 在 impl 还是 nil 时**直接返回**。表现是面板停在系统给的默认位置（屏幕正中），
   看起来完全像「摆位那段代码没跑」。
2. **`GetScreen()` 返回的 `Screen` 是物理像素，而 `SetPosition` 要的是点**：Wails 的 `cScreenToScreen`
   先乘了 `backingScaleFactor`，等 `applyDPIScaling` 再除回去——而只有走 ScreenManager 缓存那条路
   才会过 `applyDPIScaling`。Retina 上不除这个 scale，算出来的 x=3384 会被系统拉回屏幕边缘。
   （`app.Screen.GetPrimary()` 是点，但那份缓存只在屏幕参数变化时填，启动后第一次弹窗时是 nil——
   两个来源**不能混用**。）
