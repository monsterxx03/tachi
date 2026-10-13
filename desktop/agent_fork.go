package main

import (
	"context"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// Session fork for the desktop: the transcript offers 「从这里分支」 on the message
// that STARTED a turn, and the app copies the conversation up to that turn into a
// NEW session and switches the reader into it.
//
// The difference from a rewind, in the reader's terms: a rewind takes THIS
// conversation back to that point (files and all), while a fork leaves the
// conversation on screen — and the workspace — exactly as they are, and starts
// another one that ends where the chosen turn begins. It is the move for "that
// answer went wrong, but I want to keep what happened after it", and for asking the
// same question a second way.
//
// Which turn a bubble belongs to is NOT decided here: the transcript resolves it
// (rewindTargetForMessage, the same rule the rewind menu uses), so one gesture can
// never mean two different turns depending on which menu item was clicked.

// forkTitleSuffix marks a forked session's row. Two conversations with the same
// title and the same workspace are otherwise indistinguishable in the sidebar —
// and the fork carries the source's title on purpose, because it IS that
// conversation, up to a point.
const forkTitleSuffix = " · 分支"

// ForkResultVO is what a fork did: the new session, ready to be opened, or why it
// did not happen.
//
// It is a VO rather than the "ok"/reason string the other mutating bindings use
// because the reader needs the new session's ID to switch to it — and a bare
// SessionInfo would have no way to say why the row they expected never appeared.
type ForkResultVO struct {
	// Session is a POINTER: a struct value would still be marshalled when empty (omitempty does not
	// apply to structs), and the frontend reads "no session" as "this did not happen" — the shape a
	// refusal must have, since Error is what it renders.
	Session *SessionInfo `json:"session,omitempty"`
	Error   string       `json:"error,omitempty"`
}

// ForkSession copies the conversation up to the start of `turn` into a new session
// and returns it ("ok" is implied by Error being empty). Nothing about the source
// session, the workspace or the checkpoints moves: see session.FileStore.ForkSession
// for what the fork does and does not take with it.
//
// The fork is NOT made active here. Making a session active means preparing its
// agent, its manager and the frontend's whole transcript — which lives in the
// frontend (currentId, the group it opens, the page it loads), so the new session's
// ID travels back and the frontend switches the way it does for any other session.
func (s *AgentService) ForkSession(id string, turn int) ForkResultVO {
	d := s.desk
	ag, refuse := d.agentOf(id)
	if refuse != "" {
		return ForkResultVO{Error: refuse}
	}
	// The agent refuses this too; checking here as well is what lets the refusal
	// reach the reader in the words of the action they asked for (the agent's own
	// sentence says 分支 as well, but the busy check has to happen before anything
	// else is resolved).
	if why := d.forkBusyRefusal(id); why != "" {
		return ForkResultVO{Error: why}
	}
	title := forkTitle(d.sessionTitle(id))
	fork, err := ag.ForkSession(context.Background(), turn, title)
	if err != nil {
		logger.New("desktop").Info(context.Background(), "fork refused",
			"session", id, "turn", turn, "err", err)
		return ForkResultVO{Error: err.Error()}
	}
	logger.New("desktop").Info(context.Background(), "session forked",
		"session", id, "turn", turn, "fork", fork.ID)
	info := toSessionInfo(fork)
	return ForkResultVO{Session: &info}
}

// forkTitle is the fork's row title: the source's, marked. A source that has no
// title yet (a conversation whose first turn never named it) stays nameless, so the
// fork's own first message names it the way any new session's does — inventing one
// here would make 「未命名会话」 and 「··· · 分支」 two names for the same nothing.
func forkTitle(base string) string {
	if base == "" {
		return ""
	}
	return base + forkTitleSuffix
}

// forkBusyRefusal is this action's own wording for "the source session has a turn
// in flight". sessionRunning's sentence is the rewind's (「请先停止这一轮再回退」),
// and a refusal is read as the answer to the thing that was asked — a fork that
// answers with the word 回退 reads as a bug in the menu.
func (d *desktopApp) forkBusyRefusal(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	// ownerOfLocked, not d.runs[id]: forking the child a live turn is producing into
	// would copy a history that is still growing.
	if r := d.ownerOfLocked(id); r != nil && r.running {
		return "对话正在运行，请先停止这一轮再分支"
	}
	return ""
}
