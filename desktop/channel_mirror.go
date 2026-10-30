package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/session"
)

// 手机驱动一个 **channel 拥有的**会话：这一轮仍然由 channel 运行时跑，手机只是另一个看和说的出口。
//
// 为什么不能自己跑：两套运行时各自在内存里留着一份会话历史（channel 按线程、桌面按会话），谁写谁
// 才是唯一那个写的人。手机自己上轮会让两份历史分叉，磁盘上的记录变成两条并行的叙述 —— 而模型下一
// 次看到的是其中一条。所以手机发来的文字被交回这个会话所属的线程，走同一个 agent、同一份缓存、同一
// 把线程锁、同一份权限策略，事件再投影回手机的流。
//
// 这也决定了三件事：
//   - 手机**不能**对着一个 channel 会话发「同步命令」（/model、/stop 那些）——它们的答复走渠道。
//     拒绝（`channel_command`），而不是让读者在 discord 里看见一句自己没发的回复。
//   - 没有 channel 运行时（GUI 构建、或 channel 没开）时明确拒绝（`channel_offline`）：绝不退回
//     到自己跑一轮 —— 那正是这个文件要防的错。
//   - AskUser 不投给手机：问题归这段对话自己的渠道回答（见 observe 里的过滤）。

// channelTurns is the one thing this process needs from the IM side: run a turn on a conversation
// that channel owns, and name the things a phone screen has to say about it.
//
// It is an interface so the routing below can be tested without a channel runtime, and so the GUI
// build (which links no channel at all) has something honest to hold: nil.
type channelTurns interface {
	// SubmitTurn runs one message on threadID and blocks until the turn is over.
	SubmitTurn(ctx context.Context, threadID, content string, observe func(agent.AgentEvent)) (string, error)
	// ExternalRefusal is the reason this content cannot be run from another frontend, or nil.
	ExternalRefusal(content string) error
	// ThreadPlatform names the channel a thread belongs to ("discord", ...), or "".
	ThreadPlatform(threadID string) string
}

// setChannels hands this app the IM runtime of the process it lives in (main_headless, after the
// manager started). Nil is the normal value in every build that does not serve channels.
//
// Written once, before the phone listener exists, and read from request goroutines: the lock is for
// the read side being concurrent, not for it changing.
func (d *desktopApp) setChannels(turns channelTurns) {
	d.mu.Lock()
	d.channels = turns
	d.mu.Unlock()
}

func (d *desktopApp) channelRuntime() channelTurns {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.channels
}

// mobileSendToChannel is mobileSend's branch for a conversation a channel owns. It answers the same
// (status, code) shape, and returns 0 when the turn was accepted — the turn itself runs off the
// request, because a turn can take minutes and the phone watches it over the WebSocket.
func (d *desktopApp) mobileSendToChannel(ss *session.Session, id, text, mode string, attachments []string) (int, string) {
	if text == "" && len(attachments) == 0 {
		return http.StatusBadRequest, "bad_request"
	}
	turns := d.channelRuntime()
	if turns == nil {
		// Nothing in this process can run a channel conversation's turn. Saying so is the point:
		// running one here would write this session from a second history.
		return http.StatusConflict, "channel_offline"
	}
	switch mode {
	case "send":
		if running, _ := d.mirrorRunning(id); running {
			return http.StatusConflict, "turn_running"
		}
	case "steer":
		if running, _ := d.mirrorRunning(id); !running {
			return http.StatusConflict, "not_running"
		}
	case "interrupt":
		// The new text is what this conversation answers next, not steer text into a turn being
		// cancelled — so the running turn is stopped and waited for first. The wait is bounded: the
		// channel's cancellation is cooperative, and a turn that ignores it must not wedge the phone.
		if running, turn := d.mirrorRunning(id); running && turn != nil {
			turn.cancel()
			select {
			case <-turn.done:
			case <-time.After(mirrorStopWait):
			}
		}
	default:
		return http.StatusBadRequest, "bad_request"
	}

	// Same attachment route as an ordinary turn: uploads become @-references, which the turn's own
	// expansion resolves (a text file into content, an image into a multi-modal part).
	refs, status, code := d.mobileUploadRefs(id, attachments)
	if code != "" {
		return status, code
	}
	full, _ := mobileMessageWithFiles(text, refs, attachments)
	if err := turns.ExternalRefusal(full); err != nil {
		return http.StatusConflict, "channel_command"
	}

	ctx, cancel := context.WithCancel(context.Background())
	turn := d.mirrorBegin(id, cancel)
	go func() {
		defer d.mirrorEnd(id, turn)
		endReason := "complete"
		_, err := turns.SubmitTurn(ctx, ss.ThreadID, full, func(ev agent.AgentEvent) {
			// The conversation's own channel answers a question (it may be an interactive one) —
			// projecting it here too would open a second place to answer, and the turn is waiting
			// on the first.
			if ev.Type == agent.AgentEventAskUser || ev.Type == agent.AgentEventSteerCheck {
				return
			}
			if ev.Type == agent.AgentEventTurnComplete {
				// The same words the phone's own turns end with: this is what stops its spinner
				// and flushes what it queued behind the turn.
				endReason = mobileTurnEnd(ev.Result)
			}
			d.handleEventIn(id, ev, laneTranscript)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			d.mobileNotify(id, map[string]any{"type": mobileEvError, "message": err.Error()})
			endReason = "error"
		}
		if errors.Is(err, context.Canceled) && endReason == "complete" {
			endReason = "interrupted"
		}
		d.setSessionState(id, AgentState{Status: StatusIdle, Label: "空闲", Detail: "已回复"})
		d.emitIdle(id, endReason)
	}()
	return 0, ""
}

// mirrorTurn is one turn the phone asked a channel to run.
//
// done closes when that turn is over — the phone's "stop and send" waits on it, because the new text
// must reach a conversation that is no longer answering the old one.
type mirrorTurn struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// mirrorStopWait bounds how long "stop and send" waits for the channel's cancellation to land.
const mirrorStopWait = 5 * time.Second

// mirrorBegin / mirrorEnd bound one such turn. The record is what makes the phone's composer behave:
// while it is set the session reads as running (so a second message becomes steer, not a second
// turn), and Stop reaches the turn's context.
func (d *desktopApp) mirrorBegin(id string, cancel context.CancelFunc) *mirrorTurn {
	turn := &mirrorTurn{cancel: cancel, done: make(chan struct{})}
	d.mu.Lock()
	if d.mirrors == nil {
		d.mirrors = make(map[string]*mirrorTurn)
	}
	d.mirrors[id] = turn
	d.mu.Unlock()
	d.setSessionState(id, AgentState{Status: StatusBusy, Label: "处理", Detail: "在处理这段对话…"})
	return turn
}

// mirrorEnd retires ONE turn — the one it names.
//
// It must, because "stop and send" (mode interrupt) can start a new turn on the same session before
// the old one's goroutine has finished: a turn that ignores cancellation is waited for at most
// mirrorStopWait. An end that removed "whatever is registered under this id" would then unregister
// the NEW turn (and close its done channel), and the phone would go back to believing a running
// conversation is idle — the one state it must never be wrong about.
func (d *desktopApp) mirrorEnd(id string, turn *mirrorTurn) {
	if turn == nil {
		return
	}
	d.mu.Lock()
	if d.mirrors[id] == turn {
		delete(d.mirrors, id)
	}
	d.mu.Unlock()
	close(turn.done)
}

// mirrorRunning reports whether a channel turn is in flight for this session.
func (d *desktopApp) mirrorRunning(id string) (bool, *mirrorTurn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	turn, ok := d.mirrors[id]
	return ok, turn
}
