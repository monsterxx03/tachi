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
	// Irreversible lists side effects the rewind cannot take back — a git commit
	// the agent made being the one that matters. Shown on the card, never
	// swallowed.
	Irreversible []string `json:"irreversible,omitempty"`
	// Blocked is why this rewind cannot run. Non-empty means the card is the
	// whole answer.
	Blocked string `json:"blocked,omitempty"`
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
		Turn:         p.Turn,
		Target:       p.Target,
		UserText:     p.UserText,
		Irreversible: p.Irreversible,
		Blocked:      p.Blocked,
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
