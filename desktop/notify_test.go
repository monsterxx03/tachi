package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// The notification bodies are deliberately terse and never quote the model —
// they say WHAT happened, not what was said. These tests pin that down, so a
// future "let's preview the reply" change has to be deliberate.
func TestTurnDoneBody(t *testing.T) {
	tests := []struct {
		name       string
		iterations int
		took       time.Duration
		want       string
	}{
		{"counts iterations and duration", 3, 1500 * time.Millisecond, "回合完成 · 3 次迭代 · 1.5s"},
		{"rounds to milliseconds", 1, 250 * time.Millisecond, "回合完成 · 1 次迭代 · 250ms"},
		{"no iteration count to show", 0, 2 * time.Second, "回合完成"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := turnDoneBody(tt.iterations, tt.took); got != tt.want {
				t.Errorf("turnDoneBody(%d, %s) = %q, want %q", tt.iterations, tt.took, got, tt.want)
			}
		})
	}
}

func TestAskBody(t *testing.T) {
	if got, want := askBody(3), "等待你的回答（3 个问题）"; got != want {
		t.Errorf("askBody(3) = %q, want %q", got, want)
	}
	// A single question needs no count — "（1 个问题）" is noise — and neither
	// does a question set that arrived empty.
	for _, n := range []int{0, 1} {
		if got, want := askBody(n), notifyAskWaiting; got != want {
			t.Errorf("askBody(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestOneOffDoneBody(t *testing.T) {
	// The copy NAMES the run: there was no turn on screen, so 回合完成 would leave the reader
	// wondering what finished. What came of it is the reason to come back.
	tests := []struct {
		name     string
		kind     string
		findings int
		outcome  string
		want     string
	}{
		{"review with findings", commandReview, 2, "complete", "评审完成 · 2 条意见"},
		{"review with nothing to report", commandReview, 0, "complete", "评审完成 · 未报问题"},
		{"a later round is still a review", "review-round-2", 1, "complete", "评审完成 · 1 条意见"},
		{"a commit has no findings", commandCommit, 0, "complete", "提交完成"},
		{"stopped by the reader", commandReview, 0, "interrupted", "评审已停止"},
		{"a failed run says so", commandReview, 0, "error", "评审未完成（见日志）"},
		{"an unknown kind still says something", "dream", 0, "complete", "旁路运行完成 · 未报问题"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := oneOffDoneBody(tt.kind, tt.findings, tt.outcome); got != tt.want {
				t.Errorf("oneOffDoneBody(%q, %d, %q) = %q, want %q", tt.kind, tt.findings, tt.outcome, got, tt.want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	// The subtitle is cut on character boundaries, never mid-CJK-character, and
	// marked when it was cut.
	if got, want := truncateRunes(strings.Repeat("中", notifySubtitleMaxRune+5), notifySubtitleMaxRune), strings.Repeat("中", notifySubtitleMaxRune)+"…"; got != want {
		t.Errorf("truncateRunes(long) = %q, want %q", got, want)
	}
	if got, want := truncateRunes("短标题", notifySubtitleMaxRune), "短标题"; got != want {
		t.Errorf("truncateRunes(short) = %q, want %q", got, want)
	}
	if got := truncateRunes("任意", 0); got != "" {
		t.Errorf("truncateRunes(max=0) = %q, want empty", got)
	}
}

// 一条判据，两个读者（macOS 通知与设备提示音）：你正看着这个会话吗。
//
// 它决定的是「会不会吵到人」，所以每种组合都值得在主机上钉住——真机上试这些的代价是
// 一次真通知、一次切会话，和一次跑到设备跟前按一下。
func TestWatchingFollowsFocusAndTheActiveSession(t *testing.T) {
	cases := []struct {
		name    string
		focused bool
		active  string
		event   string
		want    bool
	}{
		{"前台 + 就是当前会话", true, "s1", "s1", true},
		{"前台但是别的会话", true, "s1", "s2", false},
		{"不在前台", false, "s1", "s1", false},
		{"不在前台 + 别的会话", false, "s1", "s2", false},
		// 没开着任何会话：屏幕上没有会被这条通知重复的会话全文，所以算「在看别处」。
		{"前台但没开着任何会话", true, "", "s1", false},
	}
	for _, c := range cases {
		if got := watching(c.focused, c.active, c.event); got != c.want {
			t.Errorf("%s: watching(%v, %q, %q) = %v, want %v",
				c.name, c.focused, c.active, c.event, got, c.want)
		}
	}
}

// 点一条通知要落到它说的那个会话，所以每条通知 id 都要记着它是关于谁的——这份记忆有上限，
// 因为一条没人点过的通知不该为一个常驻进程攒下状态：滑出去的那些退回到「只把窗口带到前台」，
// 也就是这次改动之前每次点击的行为。
func TestSessionForIDRemembersPostedNotifications(t *testing.T) {
	n := &notifier{}
	n.remember("tachi-1", "s1")
	n.remember("tachi-2", "s2")

	if got := n.sessionForID("tachi-2"); got != "s2" {
		t.Errorf("sessionForID(tachi-2) = %q, want the session it was posted for", got)
	}
	// 不是我们发的 id（别的应用的、或者平台回传了个怪值）答空。
	if got := n.sessionForID("some-other-app"); got != "" {
		t.Errorf("sessionForID(foreign) = %q, want empty", got)
	}

	// 挤满上限之后，最老的那条不再认得出来，最新的仍然认得出来。
	for i := 0; i < notifyRecallDepth; i++ {
		n.remember(fmt.Sprintf("tachi-%d", 100+i), fmt.Sprintf("filler-%d", i))
	}
	if got := n.sessionForID("tachi-1"); got != "" {
		t.Errorf("sessionForID(evicted) = %q, want empty so the click just brings the window up", got)
	}
	newest := fmt.Sprintf("tachi-%d", 100+notifyRecallDepth-1)
	if got, want := n.sessionForID(newest), fmt.Sprintf("filler-%d", notifyRecallDepth-1); got != want {
		t.Errorf("sessionForID(%s) = %q, want %q", newest, got, want)
	}
	if held := len(n.recall); held > notifyRecallDepth {
		t.Errorf("recall holds %d entries, want at most %d", held, notifyRecallDepth)
	}
}

// 一个会话被 park 在用户身上时，它自己的状态里带着「在等你做什么」——侧栏那枚标记的唯一来源
// （AgentState.Waiting）。它由**等待表**在发状态的那一刻填出来（waitingSentence），所以这里钉的
// 是那条判据的两半：
//
//   - 表里有一条等待，且这一轮还在跑 → 带那句话；
//   - 等待被注销（答复、或回合结束时扫掉）→ 之后任何状态都不再带它；
//   - 回合结束的状态即使抢在注销之前到达，也不带它（「在等你」蕴含这一轮还在跑）。
//
// 前两条合起来才是「标记的寿命就是这次等待的寿命」；第二条是必需的，因为 park 与工具事件来自
// 两条路，谁先到不由调用方决定（冒烟 sidebar-wait 在真窗口上验的是同一个事实的渲染）。
func TestAParkedSessionCarriesItsWaitingSentence(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}
	d.handleEventIn("s1", agent.AgentEvent{
		Type:      agent.AgentEventAskUser,
		ToolID:    "call_1",
		Questions: []tools.Question{{Question: "用哪个方案？"}},
	}, laneTranscript)

	d.mu.Lock()
	parked := d.getRun("s1").state
	d.mu.Unlock()
	if parked.Waiting != waitingQuestion {
		t.Fatalf("提问停住时的 Waiting = %q, want %q", parked.Waiting, waitingQuestion)
	}

	// 回合结束（或用户答复）会注销这条等待；注销之后，工具接着跑的那条状态不再带标记。
	d.dropAsks("s1", askKindAskUser)
	d.handleEventIn("s1", agent.AgentEvent{
		Type: agent.AgentEventToolCallStart, ToolName: "Bash",
	}, laneTranscript)

	d.mu.Lock()
	resumed := d.getRun("s1").state
	d.mu.Unlock()
	if resumed.Waiting != "" {
		t.Errorf("等待注销之后 Waiting = %q, want empty（标记没有跟着这次等待一起结束）", resumed.Waiting)
	}
}

// 另一半：表里还留着一条等待，但这一轮已经结束了 —— 标记仍然不该出现。
//
// 这不是假想的顺序问题：权限那条等待由工具自己的 goroutine 在 defer 里注销，而回合结束的状态
// 由事件通道那边写下，两者谁先到不由谁决定。落在「已经空闲、还挂着在等你」上的标记没有任何东西
// 会再来清掉它，而那是这个功能唯一不能错的方向。
func TestAFinishedTurnNeverCarriesTheWaitingSentence(t *testing.T) {
	d := &desktopApp{runs: map[string]*sessionRun{}}
	d.registerAsk(&pendingAsk{
		kind: askKindPermission, sessionID: "s1", toolID: "call_1",
		preview: "$ ls", answer: make(chan string, 1),
	})

	d.handleEventIn("s1", agent.AgentEvent{
		Type: agent.AgentEventTurnComplete, Result: &agent.RunResult{},
	}, laneTranscript)

	d.mu.Lock()
	finished := d.getRun("s1").state
	d.mu.Unlock()
	if finished.Waiting != "" {
		t.Errorf("回合结束后的 Waiting = %q, want empty（一个已经空闲的会话不能说有人在等你）", finished.Waiting)
	}
}

// A backend whose startup never ran must swallow a post instead of making it:
// on Linux the real call reaches godbus with a nil connection and panics the
// process, which is how the app used to die on any turn that ended while the
// window was unfocused (the service was never registered, so its Startup — and
// with it the D-Bus connection — never happened). Losing the notification is
// the acceptable end of "this machine cannot show notifications".
func TestPostBeforeStartupIsSilent(t *testing.T) {
	svc := newNotificationService()
	if svc.ready.Load() {
		t.Fatal("a fresh service must not report that it started")
	}
	svc.post(notifications.NotificationOptions{ID: "tachi-test", Title: notifyAppTitle})
	if svc.ready.Load() {
		t.Error("a swallowed post must not count as a started backend")
	}
}
