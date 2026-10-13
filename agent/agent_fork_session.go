package agent

import (
	"context"
	"fmt"

	"github.com/monsterxx03/tachi/session"
)

// Forking a conversation, agent side: where does a fork at turn N cut, and may it
// happen now at all. The copy itself belongs to the store (session.ForkSession,
// which owns the layout) — this file owns only the two facts the agent knows and
// the store cannot: a turn's boundary in the manifest, and whether this
// conversation is busy.
//
// A fork is deliberately NOT a rewind with a different destination. The two share
// the manifest and the turn numbering, and nothing else:
//
//   - a rewind may not leave a conversation that was compacted onwards (its
//     successor is where the reader lives now), while a fork is exactly what a
//     compacted conversation CAN do — the summary and its continuation are just
//     history to copy;
//   - a rewind refuses a managed session, because it rewrites the records a
//     subsystem owns. A fork only READS them, and "take a copy and work here
//     instead" is a legitimate thing to want, so it is allowed;
//   - a rewind's cut point is a destination the workspace goes back to. A fork's is
//     a prefix, and the workspace never moves.

// ForkSession copies this conversation's history up to the start of `turn` into a
// new session and returns it. The workspace, the checkpoints and the usage ledger
// are not touched (see session.FileStore.ForkSession for what that means for the
// fork).
//
// title is the new session's title: the caller passes the words the reader sees.
func (a *AIAgent) ForkSession(ctx context.Context, turn int, title string) (*session.Session, error) {
	// The same refusal a rewind makes, for the same reason: a turn in flight is
	// appending to the very file this would read. A fork cannot corrupt it (it only
	// reads), but the copy would be a conversation with its last turn amputated —
	// and the pairing of a tool call with its result is exactly what a half-written
	// turn does not have yet.
	if a.TurnInFlight() {
		return nil, fmt.Errorf("有回合正在运行，请先停止再分支")
	}
	sm := a.Config.SessionManager
	if sm == nil || !sm.HasCurrent() {
		return nil, fmt.Errorf("当前没有会话")
	}
	// The cut point comes from the checkpoint manifest, so a conversation without
	// checkpoints has no turn numbering to fork at all — the same reason the
	// transcript's rewind list is empty there. No second way to number turns: the
	// frontend resolves the reader's bubble to a turn through THIS list
	// (RewindTurns), so asking a different question here could only disagree.
	if !a.checkpointsEnabled() {
		return nil, fmt.Errorf("检查点未启用（agent.checkpoints.enabled）：无法定位轮次边界")
	}
	m := a.checkpointManager(ctx)
	if m == nil {
		return nil, fmt.Errorf("当前会话没有检查点存储")
	}
	rec, ok, err := m.Record(turn)
	if err != nil {
		return nil, fmt.Errorf("读取检查点失败: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("第 %d 轮没有检查点（可能已被裁剪），不能作为分支点", turn)
	}
	return sm.Fork(rec.Records, rec.APIRecords, title)
}
