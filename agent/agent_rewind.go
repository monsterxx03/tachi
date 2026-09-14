package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/checkpoint"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// Rewind: put the workspace AND the conversation back to the start of a turn.
// Design: docs/2026-09-14-session-checkpoint-rewind-design.md.
//
// Both halves move together or neither does. A conversation without its files
// leaves the model believing work is undone that is still on disk; files without
// their conversation leave it acting on results it can no longer see. There is
// deliberately no way to do one without the other (the design's decision 7):
// that granularity is plain git, which the reader already has.
//
// The rewind is IN PLACE — same session, same id — and the discarded tail is
// moved into the session's rewound/ directory rather than deleted, so the
// abandoned branch survives as an audit trail.

// RewindPreview is what a rewind would do, computed before anything is written.
//
// It exists because a rewind DELETES files as well as restoring them: "changes
// were rewound" is not something a reader should discover afterwards.
type RewindPreview struct {
	// Turn is what was asked for; Target is the checkpoint that carries the file
	// state for it. They differ when the requested turn wrote nothing, whose
	// starting state is the next writing turn's starting state (see
	// checkpoint.Manager.resolveTarget).
	Turn   int `json:"turn"`
	Target int `json:"target"`
	// UserText is the prompt that started the turn, handed back so the reader can
	// edit and re-send it instead of retyping.
	UserText string `json:"userText,omitempty"`
	// Records / APIRecords are where the conversation and the request log would
	// be cut. The usage ledger is never cut: those tokens were really spent.
	Records    int `json:"records"`
	APIRecords int `json:"apiRecords"`
	// Roots is the per-root file preview: what would be restored, deleted, and
	// (as a diffstat) how much.
	Roots []checkpoint.RootPreview `json:"roots,omitempty"`
	// Irreversible lists side effects inside the rewind's span that it cannot
	// take back — a git commit the agent made, above all.
	Irreversible []string `json:"irreversible,omitempty"`
	// Blocked is why this rewind cannot run. Non-empty means the preview is the
	// whole answer: nothing would (or should) change.
	Blocked string `json:"blocked,omitempty"`
}

// Empty reports whether the rewind would change no files.
func (p RewindPreview) Empty() bool {
	for _, r := range p.Roots {
		if len(r.Added)+len(r.Changed)+len(r.Deleted) > 0 {
			return false
		}
	}
	return true
}

// RewindResult is what a rewind did.
type RewindResult struct {
	Preview RewindPreview `json:"preview"`
	// History is the conversation the session now has, rebuilt from the truncated
	// records by the same function a session load uses — so what a frontend
	// adopts is byte-identical to what a reload would produce.
	History []llm.Message `json:"-"`
	// Sidecars are the files the discarded tail went to, for the log line.
	Sidecars []string `json:"sidecars,omitempty"`
}

// RewindTurns lists the session's checkpoints, oldest first, for a picker or a
// command that wants to offer "back to turn N".
func (a *AIAgent) RewindTurns(ctx context.Context) ([]checkpoint.TurnInfo, error) {
	m, err := a.rewindManager(ctx)
	if err != nil {
		return nil, err
	}
	return m.Turns()
}

// PreviewRewind computes what rewinding to a turn would change, without changing
// anything. A Blocked preview is a normal answer, not an error: "you cannot
// rewind to turn 3 because its file state is gone" is information the caller
// shows, not a failure it retries.
func (a *AIAgent) PreviewRewind(ctx context.Context, turn int) (RewindPreview, error) {
	m, err := a.rewindManager(ctx)
	if err != nil {
		return RewindPreview{}, err
	}
	rec, ok, err := m.Record(turn)
	if err != nil {
		return RewindPreview{}, err
	}
	if !ok {
		return RewindPreview{Turn: turn, Blocked: fmt.Sprintf("第 %d 轮没有检查点（可能已被裁剪）", turn)}, nil
	}
	p, err := m.Preview(ctx, turn)
	if err != nil {
		return RewindPreview{}, err
	}
	return RewindPreview{
		Turn:         turn,
		Target:       p.Target,
		UserText:     rec.UserText,
		Records:      rec.Records,
		APIRecords:   rec.APIRecords,
		Roots:        p.Roots,
		Irreversible: rec.Irreversible,
		Blocked:      p.Skipped,
	}, nil
}

// Rewind puts the workspace and the conversation back to the start of a turn.
//
// Order matters and is deliberate: the files are restored first (the step that
// can fail on a slow or large tree), then the conversation is cut, then the
// state is rebuilt. A failure after the files have moved is reported as such —
// the caller must not present a half-finished rewind as a whole one.
func (a *AIAgent) Rewind(ctx context.Context, turn int) (RewindResult, error) {
	if err := a.rewindAllowed(); err != nil {
		return RewindResult{}, err
	}
	preview, err := a.PreviewRewind(ctx, turn)
	if err != nil {
		return RewindResult{}, err
	}
	if preview.Blocked != "" {
		return RewindResult{Preview: preview}, nil
	}
	m, err := a.rewindManager(ctx)
	if err != nil {
		return RewindResult{}, err
	}

	if _, err := m.Restore(ctx, turn); err != nil {
		return RewindResult{Preview: preview}, fmt.Errorf("还原文件失败，会话未改动: %w", err)
	}

	sm := a.Config.SessionManager
	if sm == nil {
		return RewindResult{Preview: preview}, errors.New("no session manager")
	}
	msgs, reqs, err := sm.TruncateTo(preview.Records, preview.APIRecords, rewindTag(turn))
	if err != nil {
		return RewindResult{Preview: preview}, fmt.Errorf("已还原文件但截断会话失败（可用 git 自行恢复）: %w", err)
	}

	history, err := a.LoadSessionHistory()
	if err != nil {
		return RewindResult{Preview: preview}, fmt.Errorf("已还原文件并截断会话，但重建历史失败: %w", err)
	}
	a.reestimateAfterRewind(ctx, history)
	a.Config.Logger.Info(ctx, "Agent: rewound",
		"turn", turn, "target", preview.Target, "summary", describeRewindPreview(preview))
	return RewindResult{
		Preview:  preview,
		History:  history,
		Sidecars: sidecarPaths(msgs, reqs),
	}, nil
}

// reestimateAfterRewind recomputes the context estimate for the history that
// remains, and drops the anchor: it describes the last call's REAL prompt, and
// that prompt contained messages the rewind just removed.
//
// The system prompt is taken from the session's own request log when there is a
// surviving record — the agent does not own it (each frontend passes it in), and
// estimating without it would under-report by the system prompt and the tool
// schemas, which is a visible dip in the ring and an empty pair of buckets in
// the popover right after a rewind.
func (a *AIAgent) reestimateAfterRewind(ctx context.Context, history []llm.Message) {
	if sys := a.recordedSystemPrompt(); sys != "" {
		history = append([]llm.Message{{Role: "system", Content: sys}}, history...)
	}
	a.EstimateAndUpdateTokens(nil, history)
	a.conv.resetAfterRewind()
}

// recordedSystemPrompt is the system prompt of the newest recorded API request,
// or "" when the session has none (a rewind to the very first turn).
func (a *AIAgent) recordedSystemPrompt() string {
	sm := a.Config.SessionManager
	if sm == nil {
		return ""
	}
	cur := sm.Current()
	if cur == nil {
		return ""
	}
	reqs, err := sm.LoadAPIRequests(cur.ID)
	if err != nil || len(reqs) == 0 {
		return ""
	}
	return reqs[len(reqs)-1].SystemPrompt
}

// rewindAllowed refuses a rewind while a turn is in flight: truncating the
// conversation under a live turn is the one thing that cannot be made safe here.
func (a *AIAgent) rewindAllowed() error {
	a.mu.RLock()
	rs := a.currentRun
	a.mu.RUnlock()
	if rs != nil && !rs.isFinished() {
		return errors.New("有回合正在运行，请先停止再回退")
	}
	return nil
}

// rewindManager returns the session's checkpoint store, or an error saying why a
// rewind is unavailable at all.
func (a *AIAgent) rewindManager(ctx context.Context) (*checkpoint.Manager, error) {
	if !a.checkpointsEnabled() {
		return nil, errors.New("检查点未启用（agent.checkpoints.enabled）")
	}
	m := a.checkpointManager(ctx)
	if m == nil {
		return nil, errors.New("当前会话没有检查点存储")
	}
	return m, nil
}

// rewindTag names a rewind's sidecars. It carries a timestamp because a session
// can be rewound to the same turn more than once (work happens in between), and
// each of those abandoned branches has to survive under its own name.
func rewindTag(turn int) string {
	return fmt.Sprintf("turn-%d-%d", turn, time.Now().Unix())
}

// sidecarPaths collects the sidecar files a truncation produced, skipping the
// empty ones (a cut that removed nothing writes no file).
func sidecarPaths(results ...session.TruncateResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		if r.Sidecar != "" {
			out = append(out, r.Sidecar)
		}
	}
	return out
}

// describeRewindPreview is the one-line summary a log or a channel reply shows.
func describeRewindPreview(p RewindPreview) string {
	if p.Blocked != "" {
		return "不能回退：" + p.Blocked
	}
	var parts []string
	for _, r := range p.Roots {
		parts = append(parts, fmt.Sprintf("%s: 改 %d / 删 %d / 增 %d",
			r.Root, len(r.Changed), len(r.Deleted), len(r.Added)))
	}
	if len(parts) == 0 {
		return "回退：文件无变化"
	}
	return "回退：" + strings.Join(parts, "；")
}
