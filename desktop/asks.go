package main

import (
	"sort"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/desktop/link"
)

// pendingAskKind 区分两类等待，它同时就是发给设备的 kind 字段。
type pendingAskKind string

const (
	// askKindPermission：命中 ask 规则的 Bash 命令，等一个 允许/拒绝。
	askKindPermission pendingAskKind = "permission"
	// askKindAskUser：模型提的问题，等一组选项。
	askKindAskUser pendingAskKind = "ask_user"
)

// 一个会话「在等人」时侧栏那枚标记说的话（AgentState.Waiting）。写在这里，跟那两类等待
// 放在一起：标记要回答的是「等我做什么」，而这句话只有造出这次等待的地方说得对——
// 与菜单栏的 Label/Detail 同一条规矩，前端只渲染，不自己拼。
//
// 它也是 macOS 通知正文的前缀（notify.go 读同一对常量），因为那两处说的是同一件事。
const (
	waitingPermission = "等待你的授权"
	waitingQuestion   = "等待你的回答"
)

// pendingAsk 是一条「一个 turn 停在这里等人」的记录。
//
// 为什么一张表管两件事：这张表有两个读者——回答它的前端（用 answer 唤醒 agent），
// 和 link 层（把「在等的是什么」画到设备上）。两类等待本来就是同一件事——一个 turn
// 停着等人——所以增删时机也完全一样（挂起的那一刻登记，被回答/取消的那一刻注销）。
// 拆成两张表，就得让两处各自维护一套增删，而它们没有任何独立的理由。
//
// 键是 session + tool call id（permKey）：工具调用 id 由模型生成，两个会话完全可能
// 生成同一个，只按它索引会把一个答案送到另一个会话的等待上。
type pendingAsk struct {
	kind      pendingAskKind
	sessionID string
	toolID    string
	// toolName is the tool whose gate raised this wait. The phone needs it to pick the same
	// card chrome the window does (permEventKind), and it is recorded HERE, where the wait is
	// created — a second source would be free to disagree with the gate about what is asked.
	toolName string

	// preview 是权限等待的展示内容：agent 自己渲染的「$ 命令 + 命中的规则」
	// （askPreview）。原样透传给设备，不在这里二次解析——那份格式的第二个
	// 解析器早晚会和写它的那个漂移。
	preview string

	// questions 是 AskUserQuestion 的问题原文。
	questions []tools.Question

	// answer 是权限等待的唤醒通道。AskUser 的等待挂在 agent 自己的
	// Channels.AskUserResp 上（RespondToAskUser），所以那里为 nil——这张表记的是
	// 「在等什么」，不是「谁在等」，唤醒方式各归各的。
	answer chan string
}

// waitingSentence 说出一个会话该不该带「在等你」那枚标记，以及那句话是什么（"" 表示不必带）。
//
// 判据有两半，两半都要：
//
//   - **等待表里确有这个会话的一条等待**（d.asks）。表就是「一个 turn 停在这里等人」这件事本身
//     —— 谁在等、等什么，只有它说得准。它也是唯一个不会错序的来源：park 由工具自己的 goroutine
//     写下，工具事件走事件通道，所以同一调用的 ToolCallArgs（「执行 · 调用 Bash」）可能晚于 park
//     到达；把这句话交给调用方传进状态，就等于让后到的那条状态把它抹掉 —— 一个确实停在用户身上
//     的会话看起来没人在等，而那条晚到的状态还可能正是那个调用的参数。
//   - **这一轮还在跑**（st.Status.busy()）。「在等你」蕴含着一轮停在中途，而一个回合结束/出错
//     的状态可能抢在等待被注销之前到达（权限那条是工具 goroutine 自己 defer 注销的），所以只看
//     表的话，一次「停掉正等着的这一轮」会留下一个再也不会被清掉的标记 —— 那个方向的错不能犯。
func (d *desktopApp) waitingSentence(id string, st AgentState) string {
	if !st.Status.busy() {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.asks {
		if a.sessionID != id {
			continue
		}
		if a.kind == askKindPermission {
			return waitingPermission
		}
		return waitingQuestion
	}
	return ""
}

// registerAsk 登记一条等待，并叫醒 link 层。
//
// 登记必须发生在**这个会话的下一条状态之前**（permission 与 AskUserQuestion 各自都做到了）：
// 状态里的标记是照着这张表填的（waitingSentence），所以先写状态后登记，那一瞬间就没有标记。
func (d *desktopApp) registerAsk(a *pendingAsk) {
	d.mu.Lock()
	if d.asks == nil {
		d.asks = make(map[string]*pendingAsk)
	}
	d.asks[permKey(a.sessionID, a.toolID)] = a
	d.mu.Unlock()
	// 提示音在**登记这一刻**判一次（见 watching）：你正看着这个会话、窗口也在前台就
	// 不出声。之后你切走了也不补响——那会把一条已经看过的等待再说一遍。
	d.alertIfUnwatched(a.sessionID, link.AlertKindAsk)
	d.notifyLink()
	// The phone is the third reader of this table (design §4.6): a card appears there exactly
	// when a row is registered here.
	d.notifyMobileAsk(a)
}

// alertIfUnwatched 在「你不在看」时攒下一次提示音请求。
//
// 判据与 macOS 通知是同一条（notify.go 的 watching）：窗口失焦，或者你看的是别的会话。
// 它决定的是「会不会吵到人」，所以判错的方向要是「多响一声」，而不是「人在别处、什么都
// 没发生」——那正是这个功能要消灭的情况。
func (d *desktopApp) alertIfUnwatched(sessionID, kind string) {
	if !d.alertsEnabled() {
		return
	}
	if watching(d.windowFocused(), d.activeSessionID(), sessionID) {
		return
	}
	d.enqueueAlert(kind)
}

// alertsEnabled 读设置页那个开关（`badge.alert`，默认开）。配置读不到时按开：一个读不到
// 配置的时刻不该顺手把提示音关掉。
func (d *desktopApp) alertsEnabled() bool {
	cfg := d.config()
	return cfg == nil || cfg.Badge.AlertsEnabled()
}

// windowFocused 说窗口现在有没有焦点——watching 的一半（另一半是「看的是哪个会话」），
// 设备的提示音与 macOS 通知都读它。
//
// 拿不到窗口句柄时返回 false（当作「没在看」）：一块提示音的代价是被多响一声，而
// 猜错的另一个方向是「人在别处、什么都没发生」——那正是这个功能要消灭的情况。
func (d *desktopApp) windowFocused() bool {
	if d.notify == nil || d.notify.window == nil {
		return false
	}
	return d.notify.window.IsFocused()
}

// pendingAskToolID reports the tool call id of a session's wait of this kind, or "" when there
// is none. It exists for the answer paths that address a wait by SESSION (AskUserQuestion): the
// broadcast that follows still has to name the card the other screens should drop.
func (d *desktopApp) pendingAskToolID(sessionID string, kind pendingAskKind) string {
	for _, a := range d.snapshotAsks() {
		if a.sessionID == sessionID && a.kind == kind {
			return a.toolID
		}
	}
	return ""
}

// dropAsk 注销一条等待。调用方不必先确认真有一条：这条路径的重入是正常的
// （回答与 ctx 取消可能同时发生），而 delete 掉一个不存在的键没有副作用。
func (d *desktopApp) dropAsk(sessionID, toolID string) {
	d.mu.Lock()
	delete(d.asks, permKey(sessionID, toolID))
	d.mu.Unlock()
	d.notifyLink()
}

// dropAsks 注销一个会话里某一类等待的全部记录。
//
// 它存在是因为 AskUser 的回答不点名任何一个 tool call——模型提的问题由 sessionID
// 回答（RespondToAskUser 走的是 agent 自己的 channel）。一个会话同时只可能有一条
// turn 在等，所以「这个会话的 AskUser 记录」就是这一条。turn 结束时也走它兜底：
// 回答和取消是两条不同的路径，兜底要盖住的是没被回答的那条。
func (d *desktopApp) dropAsks(sessionID string, kind pendingAskKind) {
	d.mu.Lock()
	for key, a := range d.asks {
		if a.sessionID == sessionID && a.kind == kind {
			delete(d.asks, key)
		}
	}
	d.mu.Unlock()
	d.notifyLink()
}

// snapshotAsks 返回当前所有等待，按 (session, tool) 排序。
//
// 排序是为了让调用方能逐字节比较两次快照——map 的迭代顺序是随机的，不排的话
// 同一份内容每次都长得不一样，link 层的去抖就永远不会命中。
func (d *desktopApp) snapshotAsks() []*pendingAsk {
	d.mu.Lock()
	out := make([]*pendingAsk, 0, len(d.asks))
	for _, a := range d.asks {
		out = append(out, a)
	}
	d.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].sessionID != out[j].sessionID {
			return out[i].sessionID < out[j].sessionID
		}
		return out[i].toolID < out[j].toolID
	})
	return out
}
