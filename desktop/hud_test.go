package main

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/desktop/link"
	"github.com/monsterxx03/tachi/llm"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// 小窗的窗口参数里有两条属性在界面上完全看不出来，而丢了它们的表现只是「这窗口有时候挺烦」：
// NonActivating 决定它出现时会不会把前台的 app（正在全屏放着的视频）顶掉，CollectionBehavior 里
// 的那两位决定它会不会出现在另一个 Space 上、会不会进 ⌘` 的循环。所以钉在这里，而不是等人去查
// 窗口类。
func TestHudWindowStaysOutOfTheWay(t *testing.T) {
	opts := hudWindowOptions(application.NewRGB(1, 2, 3), application.DefaultAppearance)

	if opts.Mac.WindowClass != application.MacWindowClassPanel {
		t.Errorf("WindowClass = %v, want MacWindowClassPanel（普通 NSWindow 的 Show 会激活 app）", opts.Mac.WindowClass)
	}
	if !opts.Mac.PanelPreferences.NonActivating {
		t.Error("PanelPreferences.NonActivating = false —— 它会抢走前台应用的焦点")
	}
	if !opts.Mac.PanelPreferences.FloatingPanel {
		t.Error("PanelPreferences.FloatingPanel = false —— 它不再浮在别的窗口之上")
	}
	for _, want := range []application.MacWindowCollectionBehavior{
		application.MacWindowCollectionBehaviorCanJoinAllSpaces,
		application.MacWindowCollectionBehaviorFullScreenAuxiliary,
	} {
		if opts.Mac.CollectionBehavior&want == 0 {
			t.Errorf("CollectionBehavior = %d, 缺了 %d —— 它会在别的 Space 上，或者压不住全屏",
				opts.Mac.CollectionBehavior, want)
		}
	}
	if opts.Mac.CollectionBehavior&application.MacWindowCollectionBehaviorParticipatesInCycle != 0 {
		t.Error("小窗参与了 ⌘` 的窗口循环，但它不是一个可以「切过去」的窗口")
	}
	if !opts.Hidden {
		t.Error("Hidden = false —— 它应当在启动时建好但不出现，由 showHUD 决定什么时候拉出来")
	}
	if !opts.Frameless {
		t.Error("Frameless = false —— 一块提示板不该带着标题栏")
	}
	if !opts.DisableResize {
		t.Error("DisableResize = false —— 尺寸是设计好的，不是给人调的窗口")
	}
}

// 什么情况不弹。这几条组合在真机上试一遍的代价是每次都要「跑一轮 + 把视线移开 + 插上/拔掉设备」，
// 所以逐个钉住：设备不在线、脚本运行（跑在别人的机器上）、你正看着窗口、后台会话跑完、这一轮没有正文。
func TestHudWanted(t *testing.T) {
	cases := []struct {
		name       string
		isCurrent  bool
		focused    bool
		device     bool
		noActivate bool
		text       string
		want       bool
	}{
		{"设备连着、人不在窗口前、就是当前会话", true, false, true, false, "已经关掉了", true},
		{"没连设备", true, false, false, false, "已经关掉了", false},
		{"你正看着窗口", true, true, true, false, "已经关掉了", false},
		{"跑完的是别的会话", false, false, true, false, "已经关掉了", false},
		{"脚本运行", true, false, true, true, "已经关掉了", false},
		{"这一轮没有正文", true, false, true, false, "   \n ", false},
		{"没有正文，你也没在看", true, false, true, false, "", false},
	}
	for _, c := range cases {
		got := hudWanted(c.isCurrent, c.focused, c.device, c.noActivate, c.text)
		if got != c.want {
			t.Errorf("%s: hudWanted(%v, %v, %v, %v, %q) = %v, want %v",
				c.name, c.isCurrent, c.focused, c.device, c.noActivate, c.text, got, c.want)
		}
	}
}

// 「这一轮」的边界：往回找时必须停在 user 上。
//
// ev.Messages 是整条会话的历史，所以在里面找「最后一条有文字的 assistant」会翻到上一轮的回答上——
// 表现是模型这一轮只调了工具没说话时，小窗把上一轮的话重念一遍。那比不弹更像坏了。
func TestLastTurnReplyStopsAtTheUserMessage(t *testing.T) {
	user := func(s string) llm.Message { return llm.Message{Role: "user", Content: s} }
	assistant := func(s string) llm.Message { return llm.Message{Role: "assistant", Content: s} }
	tool := func(s string) llm.Message { return llm.Message{Role: "assistant", Content: s, ToolCalls: []llm.ToolCall{{ID: "1", Function: llm.ToolCallFunction{Name: "Bash"}}}} }

	cases := []struct {
		name     string
		messages []llm.Message
		want     string
	}{
		{
			name:     "一轮里跑过工具，取最后那条回答",
			messages: []llm.Message{user("关了"), tool(""), llm.Message{Role: "tool", Content: "ok"}, assistant("已经关掉了。")},
			want:     "已经关掉了。",
		},
		{
			name:     "这一轮只调了工具，不能翻到上一轮去",
			messages: []llm.Message{user("上一句"), assistant("上一轮的回答"), user("这一句"), tool("")},
			want:     "",
		},
		{
			name:     "steer 不是边界（它是这一轮中途插进来的话）",
			messages: []llm.Message{user("先做这个"), tool(""), llm.Message{Role: llm.RoleSteer, Content: "再顺手看一下那个"}, assistant("两个都做完了。")},
			want:     "两个都做完了。",
		},
		{
			name:     "正文在 ContentParts 里",
			messages: []llm.Message{user("说点啥"), {Role: "assistant", ContentParts: []llm.ContentPart{{Type: llm.ContentPartText, Text: "在多模态里"}}}},
			want:     "在多模态里",
		},
		{
			name:     "前后的空白不留到小窗上",
			messages: []llm.Message{user("嗯"), assistant("\n  已经关掉了。\n\n")},
			want:     "已经关掉了。",
		},
		{
			name:     "最后一轮中间的助手消息没正文，继续往前一条",
			messages: []llm.Message{user("嗯"), assistant("先看一眼这个文件。"), tool(""), assistant("看完了。")},
			want:     "看完了。",
		},
	}
	for _, c := range cases {
		if got := lastTurnReply(c.messages); got != c.want {
			t.Errorf("%s: lastTurnReply = %q, want %q", c.name, got, c.want)
		}
	}
}

// 摆位的算术：贴工作区右上角，而且**单位必须是点**。
//
// 这条测试存在的理由是一次实测：Retina 上 work area 报的是 3840 宽的物理像素，而 SetPosition 要
// 440×300 的点——不换算，算出来的 x 是 3384，窗口被系统拉回屏幕边缘，看起来完全像「摆位代码没跑」。
func TestHudPlacementConvertsPhysicalPixelsToPoints(t *testing.T) {
	cases := []struct {
		name     string
		workArea application.Rect
		scale    float64
		wantX    int
		wantY    int
	}{
		{
			// 1920×1080 的 Retina 屏：工作区 3840×2100 物理、上边距 60（菜单栏 30 点）
			name:     "Retina 2x",
			workArea: application.Rect{X: 0, Y: 60, Width: 3840, Height: 2100},
			scale:    2,
			wantX:    1920 - hudWidth - hudMargin,
			wantY:    30 + hudMargin,
		},
		{
			name:     "非 Retina",
			workArea: application.Rect{X: 0, Y: 30, Width: 1920, Height: 1050},
			scale:    1,
			wantX:    1920 - hudWidth - hudMargin,
			wantY:    30 + hudMargin,
		},
		{
			// 副屏在主屏右边：工作区原点不是 0，摆的仍然是「那块屏工作区的右上角」
			name:     "副屏（原点是主屏的宽度）",
			workArea: application.Rect{X: 3840, Y: 60, Width: 2560, Height: 1400},
			scale:    2,
			wantX:    1920 + 1280 - hudWidth - hudMargin,
			wantY:    30 + hudMargin,
		},
		{
			// 问不到缩放时按 1:1 算：宁可偏一点，也不要把窗口算到屏幕外
			name:     "没有缩放信息",
			workArea: application.Rect{X: 0, Y: 30, Width: 1920, Height: 1050},
			scale:    0,
			wantX:    1920 - hudWidth - hudMargin,
			wantY:    30 + hudMargin,
		},
	}
	for _, c := range cases {
		x, y := hudPlacement(c.workArea, c.scale, hudWidth, hudHeight, hudMargin)
		if x != c.wantX || y != c.wantY {
			t.Errorf("%s: hudPlacement = (%d, %d), want (%d, %d)", c.name, x, y, c.wantX, c.wantY)
		}
	}
}

// 关掉小窗**不能**把「最近一条回复」也清掉：设备上那个「叫出小窗」的手势问的就是它
// （关早了、或者当时没看清，再叫一次还是同一条）。这正是 content 与 last 分成两个字段的理由。
func TestClosingTheHudKeepsTheLastReply(t *testing.T) {
	last := &hudPayload{SessionID: "s1", Title: "修 lint", Text: "已经关掉了。"}
	d := &desktopApp{runs: map[string]*sessionRun{}}
	d.hud.open, d.hud.content, d.hud.last = true, last, last

	if err := d.Key(link.Key{Action: link.KeyOK}); err != nil {
		t.Fatalf("Key(ok) 返回了错误：%v", err)
	}

	if d.hud.open {
		t.Error("按了确定之后小窗还是开着的")
	}
	if d.hud.content != nil {
		t.Error("关掉之后 content 还留着：下次显示时它会先闪一下上一条")
	}
	if d.hud.last != last {
		t.Error("最近一条被清掉了：设备再也叫不回来它了")
	}
}

// 「叫出小窗」显示哪一条：会话历史优先，屏幕上显示过的那条兜底。
//
// 这个优先级就是这件事的全部，所以钉住它。反了的表现很轻——面板弹出上上条，你不会觉得是 bug，
// 只会以为自己记错了（真实场景：面板停在 R1 上，你回窗口又跑了一轮 R2，那次窗口在前台所以没弹）。
func TestHudSummonPrefersTheSessionHistory(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "A"},
		{Role: "assistant", Content: "R1"},
		{Role: "user", Content: "B"},
		{Role: "assistant", Content: "R2"},
	}
	displayed := &hudPayload{SessionID: "s1", Title: "修 lint", Text: "R1"}

	got, ok := hudSummon("s1", "修 lint", displayed, history)
	if !ok || got.Text != "R2" {
		t.Errorf("hudSummon = (%+v, %v)，期望历史里的 R2（面板上停着的那条是旧的）", got, ok)
	}

	// 历史还没装进来（这个进程里没 Load 过这个会话）：退回屏幕上显示过的那一条。
	got, ok = hudSummon("s1", "修 lint", displayed, nil)
	if !ok || got.Text != "R1" {
		t.Errorf("hudSummon 无历史时 = (%+v, %v)，期望兜底到 R1", got, ok)
	}

	// 兜底只认当前会话：别的会话的回答不摆到这块屏上（标题和内容会指两个地方）。
	other := &hudPayload{SessionID: "s2", Title: "别的会话", Text: "R9"}
	if got, ok := hudSummon("s1", "修 lint", other, nil); ok {
		t.Errorf("hudSummon 拿别的会话的回复兜底了：%+v", got)
	}

	// 什么都没有（新会话、一次都没跑完）：什么都不显示，由调用方记一行日志。
	if _, ok := hudSummon("s1", "修 lint", nil, nil); ok {
		t.Error("hudSummon 在没有内容时说「有东西可显示」")
	}
	// 连活跃会话都没有。
	if _, ok := hudSummon("", "", displayed, history); ok {
		t.Error("hudSummon 在没有活跃会话时给了内容")
	}
}

// 事件名与前端约定死了（desktop/frontend/src/hud.tsx 里是同一套字符串）。
// 一个拼错的字在这里只是一个「窗口弹出来但永远空着」的 bug，而那样的 bug 在真机上很难和
// 「载荷没发出去」分开。
func TestHudEventNamesMatchTheFrontend(t *testing.T) {
	for _, name := range []string{hudEventName, hudScrollEventName, hudReadyEventName, hudCloseEventName} {
		if strings.TrimSpace(name) == "" {
			t.Fatal("事件名是空的")
		}
	}
	// 前端按这个查询参数分叉（main.tsx 的 isHud），窗口 URL 里少一个 `hud` 就渲染成整个应用。
	if !strings.Contains(hudWindowOptions(application.NewRGB(0, 0, 0), application.DefaultAppearance).URL, "hud=1") {
		t.Error("小窗的 URL 里没有 hud=1 —— 它会渲染成主窗口那一套")
	}
}
