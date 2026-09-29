package main

import (
	"context"
	"math"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// 小窗（HUD）：把一轮的回复从窗口里拿出来，放到屏幕角上一块置顶的小面板上。
//
// 场景是这样的：人不在 Tachi 窗口前（在看视频、在写别的代码），用设备说了一句话，或者在窗口里
// 发完一条就走开了。回复本来只能回窗口看，而窗口可能压根不在前台、设备那块 240×320 的屏又装不下
// 一段话。所以这里把**最新一条回复**复制到一块不打断当前工作的面板上。
//
// 三件事决定了这块窗为什么长这样：
//
//  1. **它不抢焦点。** Wails 的普通窗口 Show() 走 makeKeyAndOrderFront，会激活 app、把正在全屏
//     放着的视频顶掉——而那恰好是这块窗最常出现的时刻。所以它用 NSPanel +
//     NSWindowStyleMaskNonactivatingPanel（Mac.WindowClass / PanelPreferences），Wails 对这条
//     路径的 Show() 是 orderFrontRegardless：出现、置顶，前台应用不动。这个属性在界面上看不出来，
//     只有「弹出来的时候打断了你正在做的事」才知道——所以它有单测钉着（hud_test.go）。
//  2. **它压在全屏 app 之上。** CollectionBehavior 那三个位的组合（CanJoinAllSpaces +
//     FullScreenAuxiliary + IgnoresCycle）是 AppKit 的「浮在别人的全屏空间上、且不进 ⌘` 循环」，
//     少了它窗口会出现在另一个 Space 里，或者干脆看不见。
//  3. **它不是第二个会话视图。** 载荷只有一段纯文本加一个标题：这里不放 transcript、工具卡、diff
//     面板——它回答的问题只有一个，「它刚才说了什么」。渲染的讲究（markdown、图片、代码高亮）留在
//     窗口里那份 transcript，那里才是读全文的地方。
//
// 什么时候弹见 hudWanted：脚本运行（smoke）不弹，主窗口在前台不弹，后台会话跑完不弹。
const (
	// 小窗的四个事件名。前两个是 Go→前端，后两个是前端→Go。
	//
	//	hudEventName       这次的载荷（{sessionId,title,text}）
	//	hudScrollEventName 上下翻一屏（{delta}，第二段的设备遥控会用到）
	//	hudReadyEventName  页面挂载完了：把手里的当前内容发过来
	//	hudCloseEventName  用户把它关掉了（点了 × 或按了 Esc）
	hudEventName       = "agent:hud"
	hudScrollEventName = "hud:scroll"
	hudReadyEventName  = "hud:ready"
	hudCloseEventName  = "hud:close"

	// hudName 是窗口自己的名字，也是 Wails 事件里 Sender 的取值。
	hudName = "hud"

	// 面板尺寸。560 是按「一行放下 34 个汉字 / 一行代码不折」定的，高度 300 是
	// 「正文区 250px ≈ 10 行」——超过这个高度它就变成一块挡在屏幕上的板子了。
	hudWidth  = 560
	hudHeight = 300
	// hudMargin 是它离屏幕工作区左上角那一条边的距离（贴右上角，见 placeHUD）。
	hudMargin = 16

	// hudPreviewEnv 是这块窗的开发开关：设了它，启动后就把面板叫出来一次，内容是编的。
	//
	// 它存在的理由只有一条：让它出现的三个条件（一轮跑完、窗口不在前台、是当前会话）一起凑齐，
	// 需要一个真模型、一次真调用，还得把视线移开——而 **smoke 套件够不着它**（driver 读的是主
	// 窗口的 DOM，这是另一个窗口的页面，见 docs/agents/desktop.md）。所以调版式、调手感、或者
	// 排查「它到底会不会弹」时，这是唯一不用跑一轮就能看见它的入口。
	hudPreviewEnv = "TACHI_HUD_PREVIEW"
)

// hudPreviewText 是开发开关显示的那段话：故意把正文里会出现的几样都放进去——标题、列表、行内
// 代码、代码块、一张 mermaid 图——好在手上一眼看出版式与渲染对不对（长行折不折、图缩没缩进框里）。
//
// 它由几段普通字符串拼起来，而**不是**一个反引号原始字符串：markdown 的围栏（三个反引号）会
// 把原始字符串截断（Go 编译报的是「语法错误」，而那行代码看着完全正常）。
const hudPreviewText = "已经帮你把那个视频关了，顺便说一下刚才那两条路：\n" +
	"\n" +
	"- 第一个问题是**顺带提起的**（正则只匹配到一个），所以我没有动它；\n" +
	"- 第二个才是你要的那句，已经处理完了 —— 用到的是 `git status --short`。\n" +
	"\n" +
	"命令是这样跑过去的：\n" +
	"\n" +
	"```mermaid\n" +
	"flowchart LR\n" +
	"    A[\"语音\"] --> B[\"转写\"]\n" +
	"    B --> C[\"当前会话\"]\n" +
	"    C --> D[\"回复上小窗\"]\n" +
	"```\n" +
	"\n" +
	"想让它自动跑就接进 hook —— 那需要改 ~/.tachi/config.yaml。"

// hudPayload 是小窗上那一屏的全部内容。字段是**格式化好的**：标题由主机查（设备与前端都不该
// 自己去找会话名），正文就是这条回复本身。
type hudPayload struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	Text      string `json:"text"`
}

// hudState 是这块窗的全部状态。
//
// win 在启动时写一次（initHUD），之后再不改；另外三个字段每次开关都动，所以整个结构由 d.mu
// 看着。open 有两个读者：hideHUD 自己的幂等判断，以及**设备**——它决定设备底栏写什么、上下键
// 发不发上来（见 badge.go 的 hudView 与设备侧的按键路由）。
//
// last 与 content 的分工是「叫回来」这件事的全部：content 是此刻显示的那一条，关窗即清；last 是
// **最近一条**，关掉也不清——因为设备上那个「叫出小窗」的手势问的就是它（关早了、或者当时没看
// 清，再叫一次还是同一条）。
type hudState struct {
	win     *application.WebviewWindow
	open    bool
	content *hudPayload
	last    *hudPayload
}

// hudLog 是小窗这条路径的日志出口。
//
// 与 badge 那条同理，走 pkg/logger 而不是标准库的 log：app 是从 Finder（或 .app）起来的，那里
// 没有 stderr 的看客，而「窗口该弹的时候弹没弹出来」只有这几行能回答。
func hudLog() *logger.Logger { return logger.New("desktop") }

// hudWindowOptions 是这块窗的构造参数，抽出来是为了能把它钉在单测里：NonActivating 与
// FullScreenAuxiliary 这两项决定了它会不会打断你、会不会在另一个 Space 上出现，而它们**在界面上
// 完全看不出来**——丢了它们，表现只是「有时候这窗口挺烦」，没人会想到去查窗口类。
func hudWindowOptions(background application.RGBA, appearance application.MacAppearanceType) application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Name:      hudName,
		Title:     hudName,
		Width:     hudWidth,
		Height:    hudHeight,
		Frameless: true,
		// 尺寸是设计好的（一行行读的正文，不是给人调的窗口），缩放它没有意义。
		DisableResize: true,
		// AlwaysOnTop 与 FloatingPanel 都指向同一个 AppKit 层级（floating）；两个都写是因为
		// 前者是跨平台的那句声明，后者是 panel 的默认，去掉任何一个都会让这块窗掉回普通层级。
		AlwaysOnTop:      true,
		Hidden:           true, // 建好但不出现：该出现的时候由 showHUD 拉出来
		URL:              "/?hud=1",
		BackgroundColour: background,
		Mac: application.MacWindow{
			Backdrop: application.MacBackdropNormal,
			// 与主窗口同一套外观（用户记着的那个选择，或跟随系统）。
			Appearance: appearance,
			// 这两项是「不抢焦点」的全部：WindowClass 把它变成 NSPanel，NonActivating 给它
			// NSWindowStyleMaskNonactivatingPanel——Wails 的 windowShow 对这类窗口走的是
			// orderFrontRegardless（见 webview_window_darwin.go），而不是激活 app 的
			// makeKeyAndOrderFront。
			WindowClass: application.MacWindowClassPanel,
			PanelPreferences: application.MacPanelPreferences{
				FloatingPanel: true,
				NonActivating: true,
			},
			// 浮在别人的全屏空间之上（看视频时它才看得见），且不参与 ⌘` 的窗口循环——它是
			// 一块提示板，不是一个可以「切过去」的窗口。
			CollectionBehavior: application.MacWindowCollectionBehaviorCanJoinAllSpaces |
				application.MacWindowCollectionBehaviorFullScreenAuxiliary |
				application.MacWindowCollectionBehaviorIgnoresCycle,
		},
	}
}

// initHUD 建起小窗，并挂上它那两个「到主机来」的事件。
//
// 它在启动时（主窗口之后）调用一次，且**一直隐藏着**——不用「第一次要显示时再建」的懒创建，是因为
// 一个还没加载完的页面收不到 hudEventName，而「弹出来是空的」比不弹更糟。代价是一块常驻的 webview；
// 换来的是「该弹的时候一定弹得出来」。
//
// 脚本运行（smoke）也照样建：它全程不会 Show，所以不会出现在别人的屏幕上（见 hudWanted）。
func (d *desktopApp) initHUD(app *application.App, background application.RGBA, appearance application.MacAppearanceType) {
	d.hud.win = app.Window.NewWithOptions(hudWindowOptions(background, appearance))

	// 页面挂载完 → 把它该显示的那条重发一遍。这一条不是多余的：启动后第一轮如果跑得比页面加载还快
	// （脚本运行、恢复会话后的一次自动发送），那次载荷是发进一个还没有监听者的页面的。重发一句是
	// 这里最便宜的兜底，也盖住「页面重新加载」那种情况。
	app.Event.On(hudReadyEventName, func(*application.CustomEvent) { d.replayHUD() })
	// 关掉：隐藏这件事由主机做（它是「开着没有」的唯一所有者），前端只负责说一声。
	app.Event.On(hudCloseEventName, func(*application.CustomEvent) { d.hideHUD() })
}

// hudTurnDone 在一轮跑完时决定要不要弹，要弹就把这条回复送上去。
//
// 调用点只有一个：handleEventIn 的 AgentEventTurnComplete，且与提示音同一个闸门（对话 lane、
// 真的跑完了、不是被打断）。这里再补上小窗自己的四个条件，见 hudWanted。
func (d *desktopApp) hudTurnDone(id string, isCurrent bool, ev agent.AgentEvent) {
	text := lastTurnReply(ev.Messages)
	if !hudWanted(isCurrent, d.windowFocused(), d.badgeSender() != nil, demoNoActivate(), text) {
		return
	}
	d.showHUD(hudPayload{SessionID: id, Title: d.sessionTitle(id), Text: text})
}

// hudWanted 说出「这一轮跑完要不要弹小窗」。
//
// 纯函数，因为这里判的是「会不会多一块东西挡在屏幕上」：真机上一次误弹的代价是你正在看的东西被盖上，
// 而这五个条件（设备 / 脚本运行 / 前台 / 当前会话 / 有没有正文）的组合在机器上试一遍要跑满一整套场景。
//
//   - deviceConnected：**这块面板是设备的遥控屏**（说话、上下翻页、确定关窗都在 badge 上），
//     设备不在线时它只是一个你没法操作的浮层——那正是最该省掉的那一块。
//   - noActivate：脚本运行**一次都不该弹**。它跑在别人的机器上（smoke 套件），而且它的窗口本来就
//     被设计成不抢前台——一块浮在最上面的面板正好是这里最不该出现的东西。
//   - focused：你正看着窗口。屏幕上的 transcript 已经写着这东西了，再弹一块只是噪声——这也是
//     macOS 通知和小窗提示音共用的那条判据（见 notify 与 alertWanted）。
//   - isCurrent：后台会话跑完不弹。它归通知管（那条通知带着会话标题），而小窗上没有位置摆一个
//     「这是哪个会话」，更重要的是**从设备发出去的话只进当前会话**，所以「刚才那句话的回音」
//     永远在当前会话上。
//   - text 为空：能走到这里的回合本来就少见（模型只调工具、一句话不说，或者正好撞上迭代上限）。
//     弹一块空面板只会让人以为坏了——那一声「跑完了」的提示音已经说了该说的话。
func hudWanted(isCurrent, focused, deviceConnected, noActivate bool, text string) bool {
	if noActivate || focused || !isCurrent || !deviceConnected {
		return false
	}
	return strings.TrimSpace(text) != ""
}

// lastTurnReply 取出**这一轮**最后一条有正文的助手消息。
//
// 「这一轮」是这段代码存在的全部理由：ev.Messages 是整条会话的历史（它同时还要写回 r.history），
// 所以在里面往回找「最后一条有文字的 assistant」会翻到**上一轮**的回答上去。模型这一轮只调了工具
// 没说话时，小窗就会把上一轮的话重新念一遍——那比不弹更像坏了。所以往回走的时候撞到 user 就停：
// 那条 user 就是这一轮的起点（steer 不算边界，它是这一轮中途插进来的话，边界之后的部分仍然是
// 这一轮的回答）。
//
// 空字符串是正常结果，由 hudWanted 处理。
func lastTurnReply(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == roleUser {
			return ""
		}
		if m.Role != roleAssistant {
			continue
		}
		if text := strings.TrimSpace(messageText(m)); text != "" {
			return text
		}
	}
	return ""
}

// roleUser / roleAssistant 是 llm 消息的两个角色。llm 包只为 steer 那个内部角色给了常量，别的
// 都是字面量（整个仓库如此），这里跟着它。
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// messageText 是消息里的正文：Content 优先，没有才去拼多模态的文本分段。
//
// 顺带说明为什么小窗只取文本：ContentParts 里还可能有图片，而这里既没有解码它的地方（那是前端
// transcript 的活），也没有必要——小窗回答的是「它说了什么」。
func messageText(m llm.Message) string {
	if strings.TrimSpace(m.Content) != "" {
		return m.Content
	}
	parts := make([]string, 0, len(m.ContentParts))
	for _, p := range m.ContentParts {
		if p.Type != llm.ContentPartText || strings.TrimSpace(p.Text) == "" {
			continue
		}
		parts = append(parts, p.Text)
	}
	return strings.Join(parts, "\n")
}

// showHUD 把这块窗（带着这条回复）放到屏幕上。
//
// 它是「小窗开着」这件事的唯一入口：设备的那两个手势（ok 关掉 / ok_double 叫回来）最终也走到
// 这里，所以窗口显示、位置、载荷广播这三件事只有一处。
func (d *desktopApp) showHUD(p hudPayload) {
	if d.app == nil || d.hud.win == nil {
		return
	}
	d.mu.Lock()
	d.hud.open, d.hud.content, d.hud.last = true, &p, &p
	d.mu.Unlock()

	// 顺序是要紧的：**先 Show 再摆位置**。窗口的 native 实现是在第一次 Show 时建出来的，而
	// WebviewWindow.SetPosition 在 impl 还是 nil 时直接返回（它的第一行）——反过来写就是「位置
	// 算了、窗口却出现在屏幕中间」。两次调用都在主线程上的一次 runloop 里跑完，所以看不到「先
	// 出现在中间、再跳到角上」那一下。
	d.hud.win.Show()
	d.placeHUD()
	d.app.Event.Emit(hudEventName, p)
	// 设备要知道它开着：底栏那一行提示、以及上下键从此有了意思（见 badge.go 的 hudView）。
	d.notifyLink()
	hudLog().Info(context.Background(), "hud: shown",
		"session", p.SessionID, "chars", len([]rune(p.Text)))
}

// hideHUD 把这块窗收起来。已经收着的时候是个空操作，所以「前端叫关」和「设备叫关」可以都调它。
//
// content 清空、last 留着：关掉之后设备那一下「叫回来」要的仍然是这条（见 hudState.last）。
func (d *desktopApp) hideHUD() {
	d.mu.Lock()
	open := d.hud.open
	d.hud.open, d.hud.content = false, nil
	win := d.hud.win
	d.mu.Unlock()

	if !open || win == nil {
		return
	}
	win.Hide()
	d.notifyLink() // 设备也要知道它关了：模式切回去，底栏那一行提示撤掉
	hudLog().Info(context.Background(), "hud: hidden")
}

// summonHUD 把小窗叫回来（设备双击确定时走这里），内容是**当前会话最近一条回复**。
//
// 两个来源是有优先级的，而且顺序不能反：
//
//   - 会话历史（r.history，会话打开时从磁盘恢复、每一轮结束时更新）**优先**。它是「它最近说了
//     什么」的真相：一块面板停在 R1 上、之后你回到窗口又跑了一轮 R2（那次没弹面板，因为窗口在
//     前台），这时「叫出小窗」该给的是 R2。
//   - 屏幕上显示过的那一条（hud.last）只作**兜底**：历史还没装进来时（这个会话在这个进程里
//     还没被 Load 过）它是唯一拿得到的东西。
//
// 两条都没有就什么都不做，只留一行日志：这种时刻在屏幕上弹一块空面板，比「按了没反应」更让人
// 以为坏了。日志里那句话也是排查的起点——设备上按一下，主机这边有没有反应，看的就是它。
func (d *desktopApp) summonHUD() {
	d.mu.Lock()
	id := d.activeID
	last := d.hud.last
	var history []llm.Message
	if r := d.getRun(id); r != nil {
		history = r.history
	}
	d.mu.Unlock()

	payload, ok := hudSummon(id, d.sessionTitle(id), last, history)
	if !ok {
		hudLog().Info(context.Background(),
			"hud: 叫不出小窗：这个会话还没有可显示的回复（一次回合都还没跑完）", "session", id)
		return
	}
	d.showHUD(payload)
}

// hudSummon 决定「叫出小窗」该显示哪一条：会话历史优先，屏幕上显示过的兜底。
//
// 抽成纯函数是因为这里的**优先级**就是这件事的全部（见 summonHUD 的注释），而错了的表现很轻：
// 面板弹出来的还是上上条，你不会立刻觉得是 bug，只会以为自己记错了。
func hudSummon(activeID, title string, last *hudPayload, history []llm.Message) (hudPayload, bool) {
	if activeID == "" {
		return hudPayload{}, false
	}
	if text := lastTurnReply(history); text != "" {
		return hudPayload{SessionID: activeID, Title: title, Text: text}, true
	}
	// 兜底：屏幕上刚显示过的那一条。只认当前会话的——别的会话的回答摆到这块屏上，标题和
	// 内容指的是两个地方，读的人只会困惑。
	if last != nil && last.SessionID == activeID && strings.TrimSpace(last.Text) != "" {
		out := *last
		if out.Title == "" {
			out.Title = title
		}
		return out, true
	}
	return hudPayload{}, false
}

// scrollHUD 让**前端**把小窗内容翻一下：delta > 0 往下读，< 0 往回读。
//
// 翻多少由前端决定（一屏多高是那边的排版事实），主机只说方向——设备按一下上下，走的就是这里。
func (d *desktopApp) scrollHUD(delta int) {
	d.mu.Lock()
	open := d.hud.open
	d.mu.Unlock()

	if !open || d.app == nil {
		return // 关着的时候翻页没有意义（设备侧本来也不会发，见 main.c 的按键路由）
	}
	d.app.Event.Emit(hudScrollEventName, map[string]any{"delta": delta})
}

// replayHUD 把当前该显示的那条再发一遍（页面刚挂载好时用）。
//
// 收着的时候什么都不发：那会让一块隐藏的窗口里堆着一份内容，而它下次显示时应当由那一次 showHUD
// 决定摆什么。
func (d *desktopApp) replayHUD() {
	d.mu.Lock()
	p, open := d.hud.content, d.hud.open
	d.mu.Unlock()

	if !open || p == nil || d.app == nil {
		return
	}
	d.app.Event.Emit(hudEventName, *p)
}

// placeHUD 把它摆到屏幕工作区的右上角。
//
// 每次显示都重新摆一次（而不是建的时候定死）：屏幕换了、分辨率变了、Dock 挪到左右了，工作区都会变，
// 而这块窗比屏幕活得久。调用点必须在 Show() **之后**（见 showHUD 里的那段注释）。
func (d *desktopApp) placeHUD() {
	screen, err := d.hud.win.GetScreen()
	if err != nil || screen == nil {
		// 拿不到屏幕就不摆：默认位置也比把它摆到屏幕外强。这条日志是故意的——屏幕上表现为
		// 「小窗出现在屏幕正中」，而这里只有一行字能说明是屏幕没问到，还是摆位那一步没生效。
		hudLog().Warn(context.Background(), "hud: 问不到屏幕，位置没摆", "err", err)
		return
	}
	x, y := hudPlacement(screen.WorkArea, float64(screen.ScaleFactor), hudWidth, hudHeight, hudMargin)
	d.hud.win.SetPosition(x, y)
	if gotX, gotY := d.hud.win.Position(); gotX != x || gotY != y {
		hudLog().Warn(context.Background(), "hud: 摆位没生效",
			"want_x", x, "want_y", y, "got_x", gotX, "got_y", gotY)
	}
}

// hudPlacement 是「贴工作区右上角、留 margin 边距」这一件事的全部算术，抽出来是因为**两个单位必须
// 在这里对齐**，而那件事在代码上完全看不出来：
//
//	GetScreen() → Screen 的 WorkArea 是**物理像素**（Wails 的 cScreenToScreen 先乘了
//	backingScaleFactor，等 applyDPIScaling 再除回去——而 GetScreen 这条路不过 applyDPIScaling，
//	只有 ScreenManager 缓存那条过），而 SetPosition 收的是**点**。
//
// 不除这个 scale，在 Retina 上算出来的 x 是屏幕宽的两倍，窗口会被系统拉回屏幕边缘（实测：摆到
// 3384，实际落在 1880——看起来像「摆位没生效」，而实际上是用错了单位）。
//
// 屏幕这块**只问窗口自己**（GetScreen 直接问 AppKit 它在哪块屏上），不去读 app.Screen 的缓存：
// 那份缓存只在「屏幕参数变了」的事件里填，启动后第一次弹窗时是空的（GetPrimary() 返回 nil），
// 而且它装的是点、和这里的物理像素不是一套——两个来源混着用，就是上面那个 bug 的另一种写法。
func hudPlacement(workArea application.Rect, scale float64, width, height, margin int) (int, int) {
	if scale <= 0 {
		scale = 1 // 问不到缩放时按 1:1 算：宁可偏，也不要把窗口算到屏幕外
	}
	points := func(v int) int { return int(math.Round(float64(v) / scale)) }
	return points(workArea.X+workArea.Width) - width - margin, points(workArea.Y) + margin
}

// previewHUD 是 hudPreviewEnv 的那只手：把面板叫出来一次，内容是编的。
//
// 走 showHUD 这条正常的路（而不是自己去 Show 窗口），是为了让这个开关验的确实是产品路径——
// 「窗口参数对不对、页面渲染成什么样、事件送不送得到」这几件事它一个都不绕过。
func (d *desktopApp) previewHUD() {
	d.showHUD(hudPayload{
		SessionID: "preview",
		Title:     "预览 · 小窗长这样",
		Text:      hudPreviewText,
	})
}
