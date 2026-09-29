package main

import (
	"net"
	"sync"
	"testing"
	"time"

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

// 设备链路可以在运行时改：设置页保存后停旧起新。测的是「端口真的开了又真的关了」，
// 而不是「config 对象里的字符串变了」——一个只改了字段的 Apply 就是这样骗过自己的。
func TestTheBadgeConfigCanBeSwitchedAtRuntime(t *testing.T) {
	t.Setenv(envBadge, "") // 这条测的是 config，别让环境变量抢答
	d := &desktopApp{runs: map[string]*sessionRun{}}

	// 先听上再放开，拿到一个空闲端口。有一点竞态，换来的是不必把端口写死在测试里。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	cfg := &config.Config{}
	cfg.Badge.Transport = "tcp"
	cfg.Badge.Address = addr

	if err := applyBadgeLink(d, cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(d.stopBadge)

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("the link did not come up on %s: %v", addr, err)
	}
	_ = conn.Close()

	cfg.Badge.Transport = "off"
	if err := applyBadgeLink(d, cfg); err != nil {
		t.Fatalf("apply off: %v", err)
	}

	if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		_ = c.Close()
		t.Error("the link is still listening after being turned off")
	}
}

// 单链路是**我们自己的**不变量，不是「操作系统会拒绝第二次打开」。
//
// 串口与 TCP 恰好会拒绝（第二次 bind/open 失败），BLE 不会：两个 central 可以同时连上
// 同一个 peripheral。而两条活链路就是两本 ref 账——同一条等待会被登记两次、各发一份，
// 其中一份按下去没有任何反应。
//
// 这条用例把 8 次「换链路」挤在一起：每个地址都能被打开，所以任何一条被漏掉的 listener
// 都会一直听着、并且在结束时能连上（这正是「漏了」的样子）。收尾必须恰好剩一条。
func TestOnlyOneBadgeLinkSurvivesConcurrentSwitches(t *testing.T) {
	t.Setenv(envBadge, "") // 这条测的是 config，别让环境变量抢答
	d := &desktopApp{runs: map[string]*sessionRun{}}

	const switches = 8
	addrs := make([]string, 0, switches)
	for i := 0; i < switches; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addrs = append(addrs, l.Addr().String())
		_ = l.Close()
	}

	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			cfg := &config.Config{}
			cfg.Badge.Transport = "tcp"
			cfg.Badge.Address = addr
			_ = applyBadgeLink(d, cfg)
		}(addr)
	}
	wg.Wait()
	t.Cleanup(d.stopBadge)

	live := 0
	for _, addr := range addrs {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err != nil {
			continue
		}
		live++
		_ = c.Close()
	}
	if live != 1 {
		t.Errorf("%d links are live after %d concurrent switches, want exactly 1", live, switches)
	}
}

// 环境变量是覆盖，不是回退：它必须能盖住文件里的持久设置（这正是它作为调试通道的
// 用途——临时把 app 指到一台假设备上）。
func TestTheEnvironmentOverridesTheBadgeConfig(t *testing.T) {
	t.Setenv(envBadge, "tcp:127.0.0.1:9")
	cfg := &config.Config{}
	cfg.Badge.Transport = "serial"
	cfg.Badge.Address = "/dev/nope"

	transport, address := badgeTarget(cfg)
	if transport != "tcp" || address != "127.0.0.1:9" {
		t.Errorf("target = %q/%q, want the environment's tcp:127.0.0.1:9", transport, address)
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
		{"tcp", "", "a missing address"},
		{"tcp", "not an address", "an address that is not one"},
		{"bluetooth", "whatever", "a transport that does not exist yet"},
	} {
		if ln, desc := openBadgeLink(tc.transport, tc.address); ln != nil {
			t.Errorf("openBadgeLink(%q, %q) opened a link (%s): %s must be refused, not guessed at",
				tc.transport, tc.address, desc, tc.why)
		}
	}
}

// 提示音的判据：窗口不在前台，或者等的不是你正开着的那个会话。
//
// 它决定的是「会不会吵到人」，所以每种组合都值得在主机上钉住——真机上试这些的代价是
// 每次都跑到设备跟前按一下。
func TestAskAlertFollowsFocusAndTheActiveSession(t *testing.T) {
	cases := []struct {
		name      string
		focused   bool
		active    string
		ask       string
		wantAlert bool
	}{
		{"前台 + 就是这个会话", true, "s1", "s1", false},
		{"前台但是别的会话", true, "s1", "s2", true},
		{"不在前台", false, "s1", "s1", true},
		{"不在前台 + 别的会话", false, "s1", "s2", true},
		{"前台但没开着任何会话", true, "", "s1", true},
	}
	for _, c := range cases {
		if got := askAlert(c.focused, c.active, c.ask); got != c.wantAlert {
			t.Errorf("%s: askAlert(%v, %q, %q) = %v, want %v",
				c.name, c.focused, c.active, c.ask, got, c.wantAlert)
		}
	}
}

// 判出来的那个值必须真的走到设备：registerAsk 记下它，快照把它带出去。
func TestRegisterAskCarriesTheAlertDecision(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}
	// 没有窗口句柄时 windowFocused() 是 false（当作「没在看」），所以这条会被标记。
	d.registerAsk(&pendingAsk{
		kind: askKindPermission, sessionID: "s1", toolID: "call_1",
		preview: "$ ls", answer: make(chan string, 1),
	})

	snap := d.Snapshot()

	if len(snap.Asks) != 1 {
		t.Fatalf("asks = %+v, want one", snap.Asks)
	}
	if !snap.Asks[0].Alert {
		t.Error("这条等待没有被标记：设备上不会响一声")
	}
}
