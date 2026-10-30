package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// 另一个前端的入口：手机能对「channel 建的会话」说话，而那一轮仍然由 channel 运行时跑。
//
// 为什么必须是同一个运行时：两套运行时各自持有一份会话历史（channel 按线程缓存、桌面按会话缓存），
// 谁写谁就得是唯一那个写的人。手机自己上轮会让两份历史分叉，而磁盘上的记录会变成两条并行的叙述。
// 所以手机发来的文字不在这里变成一轮「桌面的」turn —— 它变成这个会话所属线程的一条消息，走的是
// 同一个 handleMessage，同一份缓存，同一把线程锁，同一份权限策略。
//
// 反过来不做：channel 不能接手机会话。线程与会话是一对一的（session.ThreadID），而 channel 没有
// 「打开某一段会话」这个动作。

// ErrExternalCommand is the refusal for a slash command that answers THROUGH the channel (a listing,
// a model switch, /stop ...): run from another frontend it would reply somewhere the reader is not
// looking, and leave this one with nothing.
var ErrExternalCommand = errors.New("this command answers in the conversation's own channel")

// TurnObserver receives every agent event of a turn another frontend asked for, in the order the
// channel's own card renderer would have seen them.
//
// An alias rather than a defined type: the caller (the phone's side of this process) hands over a
// plain func, and an interface over that same signature has to accept it unchanged.
type TurnObserver = func(agent.AgentEvent)

// ExternalRefusal is the reason content cannot be run on behalf of another frontend, or nil.
//
// It exists so a caller can refuse BEFORE it answers "accepted": the phone should get one clear
// refusal over HTTP, not a 200 followed by an error event. The rule itself lives in isTurnRequest,
// the same one the turn makes.
func (m *Manager) ExternalRefusal(content string) error {
	if m.cfg == nil {
		return fmt.Errorf("channel runtime has no configuration")
	}
	if !m.isTurnRequest(content) {
		return ErrExternalCommand
	}
	return nil
}

// SubmitTurn runs one message on threadID as if a person in that conversation had typed it, and hands
// every agent event to observe. It blocks until the turn is over (or this context is cancelled).
//
// What is deliberately NOT the same as a channel's own message:
//   - nothing is posted back through the channel: no progress lines, no reply. The caller renders the
//     turn from observe.
//   - a synchronous slash command is refused (ErrExternalCommand), not run.
//
// What IS the same: the thread's cached agent and history, its working directory, its session, its
// permission mode, and the steer queue — a message that lands while the thread is mid-turn joins that
// turn exactly as the channel's next message would.
func (m *Manager) SubmitTurn(ctx context.Context, threadID, content string, observe TurnObserver) (string, error) {
	if threadID == "" || content == "" {
		return "", fmt.Errorf("channel: external turn needs a thread and some text")
	}
	if err := m.ExternalRefusal(content); err != nil {
		return "", err
	}
	msg := channel.IncomingMessage{
		ThreadID: threadID,
		// A message id of its own: the turn uses it for reply tracking (ReplyTo), and it must not
		// collide with an id the platform handed out.
		MessageID: fmt.Sprintf("external-%d", time.Now().UnixNano()),
		Content:   content,
		// A phone message is always addressed TO the agent: it must never enter the ambient pipeline
		// (a group thread's chatter) — the reader typed it at the conversation on purpose.
		Directed:  true,
		GroupChat: false,
	}
	res := m.handleMessage(ctx, msg, turnOptions{external: true, observe: observe})
	if res.Dropped {
		return "", fmt.Errorf("channel: the message was dropped by this conversation's own routing")
	}
	return res.Reply.Content, res.Err
}

// ThreadPlatform names the channel a thread belongs to ("discord", "weixin", ...), or "" when this
// process has not seen a message from it yet.
//
// The mapping is written by the first inbound message of a thread, so it is empty after a restart
// until that conversation says something — the caller renders "unknown" rather than guessing, because
// a thread id alone does not name its platform (weixin's is "account:user", discord's is an id).
func (m *Manager) ThreadPlatform(threadID string) string {
	if threadID == "" {
		return ""
	}
	if ch := m.channelForThread(threadID); ch != nil {
		return ch.Name()
	}
	return ""
}
