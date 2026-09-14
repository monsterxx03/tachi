package main

import (
	"context"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// Session rewind for the desktop: the transcript offers "回退到这里" on the
// message that STARTED a turn, and the app forwards it to that session's own
// agent, then adopts the shorter history so the next turn sends it.
//
// The mapping from a transcript message to a checkpoint is deliberately not done
// here. A checkpoint records where a turn's records BEGIN (the reminder wrapper
// and the user message both belong to it), so "which turn does this message sit
// in" is a range question the transcript can answer directly — and the
// interjection case (a steer message typed while the agent was working) needs a
// UI decision this layer should not make. See rewindTargets in the frontend.

// RewindTurnVO is one checkpointed turn, as a picker or the transcript needs it.
type RewindTurnVO struct {
	Turn int `json:"turn"`
	// Records is the session record index where the turn's records begin. A
	// message belongs to this turn when Records <= index < the next turn's
	// Records. (It is NOT the index of the user message itself: a turn's reminder
	// wrapper is recorded first, and whether there is one varies per turn.)
	Records  int    `json:"records"`
	At       string `json:"at"`
	UserText string `json:"userText,omitempty"`
	// NoFiles marks a turn whose file state is unknown (it only read, a guard
	// refused the snapshot, or the snapshot was pruned). A rewind to it may share
	// a later turn's snapshot or be refused; the preview says which.
	NoFiles bool `json:"noFiles,omitempty"`
	// Reason explains NoFiles when the state is genuinely unknown, as opposed to "nothing
	// wrote during it". The two are different answers a chooser has to tell apart: one says
	// the workspace already is what a rewind would produce, the other says nobody recorded it.
	Reason string `json:"reason,omitempty"`
	// Diff is what this turn changed, counted from its own two checkpoint trees — nil when it
	// wrote nothing, Skipped when the numbers could not be taken. It rides along so a LIST of
	// turns can show real numbers without running git: a preview per row would mean one
	// `git diff` per row, which is why the chooser reads this instead.
	Diff *TurnChangesVO `json:"diff,omitempty"`
}

// RewindRootVO is one workspace root's part of a rewind preview.
type RewindRootVO struct {
	Root string `json:"root"`
	// Added are files that exist now and would be DELETED by the rewind,
	// Changed ones whose content would be restored, Deleted ones that would come
	// back. Added is listed first in the UI because it is the destructive half.
	Added   []string `json:"added,omitempty"`
	Changed []string `json:"changed,omitempty"`
	Deleted []string `json:"deleted,omitempty"`
	Stat    string   `json:"stat,omitempty"`
}

// RewindPreviewVO is what a rewind would do, for the confirmation card.
type RewindPreviewVO struct {
	Turn   int `json:"turn"`
	Target int `json:"target"`
	// UserText is the prompt that started the turn: the app puts it back in the
	// composer so the reader can edit and re-send it.
	UserText string         `json:"userText,omitempty"`
	Roots    []RewindRootVO `json:"roots,omitempty"`
	// FilesUnchanged means there is nothing to restore: this turn and everything
	// after it wrote no file, so the workspace is already in the state the rewind
	// would produce. The card says so instead of showing a list of zeroes.
	FilesUnchanged bool `json:"filesUnchanged,omitempty"`
	// NoFiles, when set, is why NO file was restored even though the rewind runs.
	// Shown on the card as a warning, never swallowed: the conversation moves back
	// and the workspace does not, and the reader has to know which of the two
	// happened.
	NoFiles string `json:"noFiles,omitempty"`
	// Irreversible lists side effects the rewind cannot take back — a git commit
	// the agent made being the one that matters. Shown on the card, never
	// swallowed.
	Irreversible []string `json:"irreversible,omitempty"`
	// Blocked is why this rewind cannot run at all (no checkpoint for that turn,
	// another turn in flight). Non-empty means the card is the whole answer.
	Blocked string `json:"blocked,omitempty"`
}

// RewindChainVO is what the rewind-chain surface needs in ONE call: the session's rewind
// points, and — when the chain as a whole is unusable — why.
type RewindChainVO struct {
	// Blocked is why no rewind of this session can run (it was compacted onwards), or "" when
	// the chain is usable. Non-empty means the list is there to READ: every row would be
	// refused identically, so the reason belongs at the top, once.
	Blocked string         `json:"blocked,omitempty"`
	Turns   []RewindTurnVO `json:"turns,omitempty"`
}

// RewindChain lists a session's rewind points together with whether the chain can be used at
// all. No git is run: the per-turn numbers were recorded when each turn ended, and the refusal
// is a session-link lookup.
func (s *AgentService) RewindChain(id string) RewindChainVO {
	d := s.desk
	ag, refuse := d.agentOf(id)
	if refuse != "" {
		return RewindChainVO{Blocked: refuse}
	}
	return RewindChainVO{Blocked: ag.RewindChainBlocked(), Turns: s.RewindTurns(id)}
}

// RewindTurns lists the session's checkpointed turns, oldest first. It returns
// an empty slice (not an error) when the session has no agent or no checkpoints:
// "there is nothing to go back to" is an answer the transcript renders as such.
func (s *AgentService) RewindTurns(id string) []RewindTurnVO {
	d := s.desk
	ag, refuse := d.agentOf(id)
	if refuse != "" {
		return nil
	}
	turns, err := ag.RewindTurns(context.Background())
	if err != nil {
		return nil
	}
	out := make([]RewindTurnVO, 0, len(turns))
	for _, t := range turns {
		out = append(out, RewindTurnVO{
			Turn:     t.Turn,
			Records:  t.Records,
			At:       t.At.Format("2006-01-02 15:04:05"),
			UserText: t.UserText,
			NoFiles:  t.NoFiles,
			Reason:   t.Reason,
			Diff:     turnChangesFromCheckpoint(t.Diff),
		})
	}
	return out
}

// PreviewRewind computes what rewinding to a turn would change, without changing
// anything. A Blocked preview is a normal answer the card shows, not an error.
func (s *AgentService) PreviewRewind(id string, turn int) RewindPreviewVO {
	d := s.desk
	ag, refuse := d.agentOf(id)
	if refuse != "" {
		return RewindPreviewVO{Turn: turn, Blocked: refuse}
	}
	if running, why := d.sessionRunning(id); running {
		return RewindPreviewVO{Turn: turn, Blocked: why}
	}
	p, err := ag.PreviewRewind(context.Background(), turn)
	if err != nil {
		return RewindPreviewVO{Turn: turn, Blocked: err.Error()}
	}
	vo := toRewindPreviewVO(p)
	logger.New("desktop").Info(context.Background(), "rewind preview",
		"session", id, "turn", turn, "blocked", vo.Blocked, "roots", len(vo.Roots))
	return vo
}

// ApplyRewind performs the rewind and adopts the shorter history for this
// session's run, so the next turn sends it rather than the history the rewind
// just discarded. Returns "ok", or the reason it refused (the desktop's
// convention for a mutating call: the frontend renders the string as it is).
func (s *AgentService) ApplyRewind(id string, turn int) string {
	d := s.desk
	ag, refuse := d.agentOf(id)
	if refuse != "" {
		return refuse
	}
	// The agent refuses this too; checking here as well is what lets the refusal
	// arrive before the confirmation card rather than after it.
	if running, why := d.sessionRunning(id); running {
		return why
	}

	res, err := ag.Rewind(context.Background(), turn)
	if err != nil {
		return err.Error()
	}
	if res.Preview.Blocked != "" {
		return res.Preview.Blocked
	}
	// The run's history is the one the agent rebuilt from the truncated records —
	// the same function a session load uses, so what the next turn sends is what
	// a reload would have produced.
	if res.History != nil {
		d.setRunHistory(id, res.History)
	}
	// The status bar has to be re-pushed, for the same reason it is pushed at turn end: the
	// rewind recomputed the estimate for the history that REMAINS and dropped the anchor
	// that measured the history it removed (agent.Rewind's reestimateAfterRewind), and the
	// frontend only ever learns these numbers from this push. Without it the context ring
	// kept pointing at the size of the conversation the rewind had just taken away, until
	// the next turn's first API call — the same ring-lags-behind asymmetry that a long turn
	// showed (see the emitUsage doc comment).
	d.emitUsage(id, d.currentID() == id, nil)
	if d.app != nil {
		// The whole preview travels with the event: the frontend composes the
		// notice from the numbers (and shows what was NOT restored), without a
		// second round trip that could straddle another turn.
		d.app.Event.Emit("agent:rewound", map[string]any{
			"sessionId": id,
			"preview":   toRewindPreviewVO(res.Preview),
		})
	}
	return "ok"
}

// sessionRunning reports whether a turn is in flight for a session, with the
// refusal to show. Mirrors the delete-session guard: the frontend may be looking
// at a stale row, so the backend has the last word.
func (d *desktopApp) sessionRunning(id string) (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.runs[id]; r != nil && r.running {
		return true, "对话正在运行，请先停止这一轮再回退"
	}
	return false, ""
}

// toRewindPreviewVO flattens the agent's preview into the binding's shape.
func toRewindPreviewVO(p agent.RewindPreview) RewindPreviewVO {
	out := RewindPreviewVO{
		Turn:           p.Turn,
		Target:         p.Target,
		UserText:       p.UserText,
		FilesUnchanged: p.FilesUnchanged,
		NoFiles:        p.NoFiles,
		Irreversible:   p.Irreversible,
		Blocked:        p.Blocked,
	}
	for _, r := range p.Roots {
		out.Roots = append(out.Roots, RewindRootVO{
			Root:    r.Root,
			Added:   r.Added,
			Changed: r.Changed,
			Deleted: r.Deleted,
			Stat:    r.Stat,
		})
	}
	return out
}
