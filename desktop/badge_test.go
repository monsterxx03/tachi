package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/desktop/link"
)

// 设备要看到的是「谁在跑、谁在等」：一个桌面上躺着的空闲会话只是噪声。
func TestSnapshotCarriesTheRunningSessionsAndThePendingAsks(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}
	d.setSessionState("s1", AgentState{Status: StatusToolRunning, Label: "执行", Detail: "调用 Bash"})
	d.setSessionState("s2", AgentState{Status: StatusIdle, Label: "空闲", Detail: "就绪"})
	d.registerAsk(&pendingAsk{
		kind: askKindPermission, sessionID: "s1", toolID: "call_1",
		preview: "$ rm -rf build/", answer: make(chan string, 1),
	})

	snap := d.Snapshot()

	if len(snap.Sessions) != 1 || snap.Sessions[0].ID != "s1" {
		t.Fatalf("sessions = %+v, want only the running one", snap.Sessions)
	}
	if got := snap.Sessions[0].Detail; got != "调用 Bash" {
		t.Errorf("detail = %q", got)
	}
	if len(snap.Asks) != 1 {
		t.Fatalf("asks = %+v, want one", snap.Asks)
	}
	ask := snap.Asks[0]
	if ask.Kind != string(askKindPermission) || ask.SessionID != "s1" || ask.ToolID != "call_1" {
		t.Errorf("ask = %+v", ask)
	}
	if ask.Body != "$ rm -rf build/" {
		t.Errorf("ask body = %q, want the preview the agent rendered", ask.Body)
	}
	if len(ask.Options) != 3 {
		t.Errorf("ask carries %d option(s), want the three decisions", len(ask.Options))
	}
}

// 设备拿列表里的第一个当「现在这个」，所以正在屏幕上显示的那个必须排最前。
// 会话 ID 带时间戳，按 ID 排等于把最早创建的那个摆上去。
func TestSnapshotPutsTheDisplayedSessionFirst(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}
	d.setSessionState("2026-09-01-aaa", AgentState{Status: StatusThinking, Label: "思考"})
	d.setSessionState("2026-09-28-zzz", AgentState{Status: StatusThinking, Label: "思考"})

	d.mu.Lock()
	d.activeID = "2026-09-28-zzz"
	d.mu.Unlock()

	snap := d.Snapshot()
	if len(snap.Sessions) != 2 {
		t.Fatalf("sessions = %+v, want both", snap.Sessions)
	}
	if snap.Sessions[0].ID != "2026-09-28-zzz" {
		t.Errorf("first session = %s, want the displayed one", snap.Sessions[0].ID)
	}
	// 其余仍按 ID 排，好让两次快照可以逐字节比较（link 层的去抖依赖这个）。
	if snap.Sessions[1].ID != "2026-09-01-aaa" {
		t.Errorf("second session = %s", snap.Sessions[1].ID)
	}
}

func TestSnapshotCarriesAskUserQuestions(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}
	d.registerAsk(&pendingAsk{
		kind: askKindAskUser, sessionID: "s1", toolID: "call_2",
		questions: []tools.Question{{
			Header:      "方向",
			Question:    "走哪条路？",
			Options:     []tools.QuestionOption{{Label: "A"}, {Label: "B"}},
			MultiSelect: true,
		}},
	})

	ask := d.Snapshot().Asks[0]
	if len(ask.Questions) != 1 || ask.Questions[0].Question != "走哪条路？" {
		t.Fatalf("questions = %+v", ask.Questions)
	}
	if !ask.Questions[0].MultiSelect {
		t.Error("multiSelect was lost on the way out")
	}
	if ask.Title != "方向" {
		t.Errorf("title = %q, want the question's header", ask.Title)
	}
}

func TestAnswerReachesThePermissionWait(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}
	answer := make(chan string, 1)
	d.registerAsk(&pendingAsk{
		kind: askKindPermission, sessionID: "s1", toolID: "call_1",
		preview: "x", answer: answer,
	})

	err := d.Answer(link.Answer{
		SessionID: "s1", ToolID: "call_1",
		Kind: string(askKindPermission), Decision: link.DecisionAllowSession,
	})
	if err != nil {
		t.Fatalf("the answer was refused: %v", err)
	}

	select {
	case got := <-answer:
		if got != permAllowSession {
			t.Errorf("the waiter was handed %q, want %q", got, permAllowSession)
		}
	default:
		t.Fatal("the waiting goroutine was never woken")
	}
}

// 一条答给别的类型的回答必须被拒：设备上的界面可能停在一条权限确认上，而它已经被
// 一个提问替换掉了。
func TestAnAnswerOfTheWrongKindIsRefused(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}
	d.registerAsk(&pendingAsk{
		kind: askKindAskUser, sessionID: "s1", toolID: "call_1",
		questions: []tools.Question{{Question: "?"}},
	})

	err := d.Answer(link.Answer{
		SessionID: "s1", ToolID: "call_1",
		Kind: string(askKindPermission), Decision: link.DecisionDeny,
	})
	if err == nil {
		t.Error("a permission answer was accepted for an AskUserQuestion row")
	}
}

func TestAnAnswerForNothingIsRefused(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}

	err := d.Answer(link.Answer{
		SessionID: "s1", ToolID: "call_1",
		Kind: string(askKindPermission), Decision: link.DecisionDeny,
	})
	if err == nil {
		t.Error("an answer with no waiting ask was accepted")
	}
}

func TestDroppingAnAskTakesItOutOfTheSnapshot(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}
	d.registerAsk(&pendingAsk{
		kind: askKindAskUser, sessionID: "s1", toolID: "call_1",
		questions: []tools.Question{{Question: "?"}},
	})
	d.registerAsk(&pendingAsk{
		kind: askKindAskUser, sessionID: "s1", toolID: "call_2",
		questions: []tools.Question{{Question: "?"}},
	})

	d.dropAsk("s1", "call_1")
	if got := len(d.Snapshot().Asks); got != 1 {
		t.Fatalf("%d ask(s) left, want 1", got)
	}

	// 按类型清一个会话：turn 结束时兜底用它——AskUser 的回答不点名 tool call，
	// 而一条没被回答的等待只能靠这条路收拾。
	d.dropAsks("s1", askKindAskUser)
	if got := len(d.Snapshot().Asks); got != 0 {
		t.Fatalf("%d ask(s) left after the sweep, want 0", got)
	}
}

func TestALinkSubscriberHearsAboutChanges(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}
	calls := make(chan struct{}, 8)
	cancel := d.Subscribe(func() { calls <- struct{}{} })

	d.setSessionState("s1", AgentState{Status: StatusThinking, Label: "思考"})
	drain(calls)

	d.registerAsk(&pendingAsk{kind: askKindAskUser, sessionID: "s1", toolID: "c", questions: nil})
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("a new ask did not reach the link")
	}

	cancel()
	drain(calls)
	d.setSessionState("s1", AgentState{Status: StatusToolRunning, Label: "执行"})
	select {
	case <-calls:
		t.Error("a cancelled subscriber was still notified")
	case <-time.After(100 * time.Millisecond):
	}
}

func drain(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// 设备链路可以在运行时改：设置页保存后停旧起新。
//
// 它曾经用 tcp 来验：起一个监听、从外面连一下、关掉之后再连不上——那是当时唯一
// 「看得见、摸得着」的假设备。tcp 那一档去掉之后，这里只能看链路对象本身起来没起来，
// 比连一下弱，但仍然抓得住「apply 改了字段却没真动链路」这类错。
func TestTheBadgeConfigCanBeSwitchedAtRuntime(t *testing.T) {
	t.Setenv(envBadge, "") // 这条测的是 config，别让环境变量抢答
	d := &desktopApp{runs: map[string]*sessionRun{}}

	cfg := &config.Config{}
	cfg.Badge.Transport = "serial"
	cfg.Badge.Address = "" // 自动查找：不给具体设备，只验证链路被建起来了

	if err := applyBadgeLink(d, cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(d.stopBadge)

	if !badgeLinkUp(d) {
		t.Fatal("the link did not come up")
	}

	cfg.Badge.Transport = "off"
	if err := applyBadgeLink(d, cfg); err != nil {
		t.Fatalf("apply off: %v", err)
	}
	if badgeLinkUp(d) {
		t.Error("the link is still up after being turned off")
	}
}

// badgeLinkUp 说链路此刻建起来没有。读 linkLn 必须持那把锁——它保护的是这条链路的
// 整个生命周期（见 agent.go 里 linkMu 的注释）。
func badgeLinkUp(d *desktopApp) bool {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	return d.linkLn != nil
}

// 环境变量是覆盖，不是回退：它必须能盖住文件里的持久设置（这正是它作为调试通道的
// 用途——临时把 app 指到一台假设备上）。
func TestTheEnvironmentOverridesTheBadgeConfig(t *testing.T) {
	t.Setenv(envBadge, "serial:/dev/nope")
	cfg := &config.Config{}
	cfg.Badge.Transport = "serial"
	cfg.Badge.Address = "/dev/nope"

	transport, address := badgeTarget(cfg)
	if transport != "serial" || address != "/dev/nope" {
		t.Errorf("target = %q/%q, want the environment's serial:/dev/nope", transport, address)
	}
}

func TestTheBadgeIsOffByDefault(t *testing.T) {
	t.Setenv(envBadge, "")
	if ln, desc := openBadgeLink(badgeTarget(&config.Config{})); ln != nil {
		t.Errorf("an empty config opened a link (%s): the serial port must not be taken by default", desc)
	}
}

// 配置写错了不该拦住任何事：一行日志，然后什么都不做。设备链路是个附加物，它连不上
// 既不该让 desktop 起不来，也不该让一次设置保存失败。
func TestAMisconfiguredBadgeLinkIsRefusedQuietly(t *testing.T) {
	for _, tc := range []struct{ transport, address, why string }{
		{"bluetooth", "whatever", "a transport that does not exist yet"},
	} {
		if ln, desc := openBadgeLink(tc.transport, tc.address); ln != nil {
			t.Errorf("openBadgeLink(%q, %q) opened a link (%s): %s must be refused, not guessed at",
				tc.transport, tc.address, desc, tc.why)
		}
	}
}

// 提示音的判据：窗口不在前台，或者这件事不属于你正开着的那个会话。
//
// 它决定的是「会不会吵到人」，所以每种组合都值得在主机上钉住——真机上试这些的代价是
// 每次都跑到设备跟前按一下。
func TestAlertWantedFollowsFocusAndTheActiveSession(t *testing.T) {
	cases := []struct {
		name      string
		focused   bool
		active    string
		event     string
		wantAlert bool
	}{
		{"前台 + 就是当前会话", true, "s1", "s1", false},
		{"前台但是别的会话", true, "s1", "s2", true},
		{"不在前台", false, "s1", "s1", true},
		{"不在前台 + 别的会话", false, "s1", "s2", true},
		{"前台但没开着任何会话", true, "", "s1", true},
	}
	for _, c := range cases {
		if got := alertWanted(c.focused, c.active, c.event); got != c.wantAlert {
			t.Errorf("%s: alertWanted(%v, %q, %q) = %v, want %v",
				c.name, c.focused, c.active, c.event, got, c.wantAlert)
		}
	}
}

// 两件事共用同一条判据：出现了一条等待，以及一个回合跑完了。判出来的结果必须真的进
// 到那条队列里——link 层从那里取（见 badge.go 的 TakeAlerts）。
func TestBothEventsQueueAnAlert(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}
	// 没有窗口句柄时 windowFocused() 是 false（当作「没在看」），所以两条都会被攒下。

	d.registerAsk(&pendingAsk{
		kind: askKindPermission, sessionID: "s1", toolID: "call_1",
		preview: "$ ls", answer: make(chan string, 1),
	})
	d.alertIfUnwatched("s1", link.AlertKindDone)

	alerts := d.TakeAlerts()
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v, want two (ask + done)", alerts)
	}
	if alerts[0].Kind != link.AlertKindAsk || alerts[1].Kind != link.AlertKindDone {
		t.Errorf("alerts = %+v, want ask then done", alerts)
	}
	if again := d.TakeAlerts(); len(again) != 0 {
		t.Errorf("取过之后应当为空，实际 %+v", again)
	}
}

// 设置页那个开关（`badge.alert`）关掉就一条都不攒——包括「已经攒下的还没发出去」那一种：
// 用户刚刚说不要提示音，链路那边不该还放出一声来。
func TestAlertsEnabledSwitchesTheQueueOff(t *testing.T) {
	off := false
	d := &desktopApp{runs: map[string]*sessionRun{}, cfg: &config.Config{}}
	d.cfg.Badge.Alert = &off

	d.alertIfUnwatched("s1", link.AlertKindDone)

	if alerts := d.TakeAlerts(); len(alerts) != 0 {
		t.Errorf("开关关着仍然攒了 %+v", alerts)
	}
}

// 状态屏那一行要说的是「在拿它干什么」，而不只是「在用哪个工具」——参数摘要复用前端
// 标题用的同一个函数（tools.ToolArgsSummary）。
func TestToolDetailCarriesTheArguments(t *testing.T) {
	got := toolDetail("Bash", `{"command":"rm -rf build/"}`)
	if !strings.Contains(got, "Bash") || !strings.Contains(got, "rm -rf build/") {
		t.Errorf("detail = %q，应当同时带上工具名与参数", got)
	}
	if bare := toolDetail("SomeTool", ""); bare != "调用 SomeTool" {
		t.Errorf("没有参数时 detail = %q", bare)
	}
}

// 设备那条副标题的字段只有 96 字节，而设备是按**字节**截断的：主机必须自己按字符边界
// 裁到上限以内，否则屏幕上是一串乱码（切出半个汉字）。
func TestDeviceDetailFitsTheDeviceField(t *testing.T) {
	got := deviceDetail(strings.Repeat("汉", 200))

	if len(got) > deviceDetailLimit {
		t.Errorf("裁完 %d 字节，超过上限 %d", len(got), deviceDetailLimit)
	}
	if !utf8.ValidString(got) {
		t.Errorf("裁出了半个字符：%q", got)
	}
	if short := "调用 Bash"; deviceDetail(short) != short {
		t.Errorf("放得下时不该动它，实际 %q", deviceDetail(short))
	}
}
