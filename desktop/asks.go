package main

import (
	"sort"

	"github.com/monsterxx03/tachi/agent/tools"
)

// pendingAskKind 区分两类等待，它同时就是发给设备的 kind 字段。
type pendingAskKind string

const (
	// askKindPermission：命中 ask 规则的 Bash 命令，等一个 允许/拒绝。
	askKindPermission pendingAskKind = "permission"
	// askKindAskUser：模型提的问题，等一组选项。
	askKindAskUser pendingAskKind = "ask_user"
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

	// preview 是权限等待的展示内容：agent 自己渲染的「$ 命令 + 命中的规则」
	// （bashAskPreview）。原样透传给设备，不在这里二次解析——那份格式的第二个
	// 解析器早晚会和写它的那个漂移。
	preview string

	// questions 是 AskUserQuestion 的问题原文。
	questions []tools.Question

	// answer 是权限等待的唤醒通道。AskUser 的等待挂在 agent 自己的
	// Channels.AskUserResp 上（RespondToAskUser），所以那里为 nil——这张表记的是
	// 「在等什么」，不是「谁在等」，唤醒方式各归各的。
	answer chan string
}

// registerAsk 登记一条等待，并叫醒 link 层。
func (d *desktopApp) registerAsk(a *pendingAsk) {
	d.mu.Lock()
	if d.asks == nil {
		d.asks = make(map[string]*pendingAsk)
	}
	d.asks[permKey(a.sessionID, a.toolID)] = a
	d.mu.Unlock()
	d.notifyLink()
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
