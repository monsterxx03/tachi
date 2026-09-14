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
	// FilesUnchanged is set when the rewind needs no file work at all: no turn
	// at or after the target wrote anything, so the workspace already is what a
	// rewind to it would produce. It is an ANSWER, not a warning — and it is the
	// state of every turn in a session that never wrote a file, which is why it
	// must not stop the conversation from going back. The reader wants the
	// CONVERSATION, and the files happen to need nothing.
	FilesUnchanged bool `json:"filesUnchanged,omitempty"`
	// NoFiles, when set, is why NO file was restored even though the rewind ran:
	// the recorded state is gone (pruned) or was never taken (a guard refused the
	// snapshot, git is missing, the recorded workspace is no longer there). The
	// conversation still moves — but the card and the notice say this plainly
	// rather than implying the workspace moved with it (the design's "must not
	// silently report success").
	NoFiles string `json:"noFiles,omitempty"`
	// RootMismatch, when set, says the files this rewind would restore belong to a
	// different workspace than the session has now: the checkpoint's roots are not
	// the current ones (the session was pointed at another folder, a root was added
	// or removed, a desktop project edit, a git worktree checkout). The rewind is
	// still the right one for those turns — each recorded tree goes back — but the
	// reader has to know the directory on screen is not the one being written, or
	// "the workspace went back" reads as "the files I am looking at moved".
	RootMismatch string `json:"rootMismatch,omitempty"`
	// Irreversible lists side effects inside the rewind's span that it cannot
	// take back. A git commit the agent made is the one that is detected (the
	// roots' own HEAD, recorded by the checkpoints, compared against now — see
	// checkpoint.Manager.committedSince); the other items on the design's §6 list
	// are not, so an empty list is not a promise that nothing else happened.
	Irreversible []string `json:"irreversible,omitempty"`
	// Blocked is why this rewind cannot run AT ALL. Non-empty means the preview
	// is the whole answer: nothing would (or should) change. It is NOT set for a
	// missing file state — that is NoFiles, which lets the conversation rewind
	// while the files stay put.
	Blocked string `json:"blocked,omitempty"`
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
	// The whole-conversation refusals come first: a rewind that cannot happen at all must be
	// reported as such by the PREVIEW too, or the card would be built (file lists, diffstat and
	// all) and the reader would only learn otherwise from the failure of the action it offered.
	if why := a.rewindBlockedByCompaction(); why != "" {
		return RewindPreview{Turn: turn, Blocked: why}, nil
	}
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
	out := RewindPreview{
		Turn:         turn,
		Target:       p.Target,
		UserText:     rec.UserText,
		Records:      rec.Records,
		APIRecords:   rec.APIRecords,
		Roots:        p.Roots,
		RootMismatch: p.RootMismatch,
		Irreversible: p.Irreversible,
	}
	// The file half's three outcomes, kept apart on purpose: "here is what to
	// restore", "nothing to restore because nothing wrote" (a fact, the rewind
	// just goes), and "the state is unknown" (the rewind still goes, and says
	// that no file was restored). Only a turn with no checkpoint at all blocks.
	switch p.State {
	case checkpoint.FilesUnchanged:
		out.FilesUnchanged = true
	case checkpoint.FilesUnknown:
		out.NoFiles = p.Skipped
	}
	return out, nil
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
		return RewindResult{Preview: preview}, fmt.Errorf("还原文件失败（部分文件可能已被改动），会话未改动: %w", err)
	}

	sm := a.Config.SessionManager
	if sm == nil {
		return RewindResult{Preview: preview}, errors.New("no session manager")
	}
	msgs, reqs, err := sm.TruncateTo(preview.Records, preview.APIRecords, rewindTag(turn))
	if err != nil {
		return RewindResult{Preview: preview}, fmt.Errorf("已还原文件但截断会话失败（可用 git 自行恢复）: %w", err)
	}

	// The conversation now ends at this turn, so the checkpoints AFTER it index a
	// conversation that is gone: their cut point sits past the new end and their
	// refs hold the branch that was just abandoned. Dropping them is what keeps
	// "回退到第 N 轮" honest a second time — a stale record would restore the
	// FILES to the discarded branch's point in time while the cut did nothing.
	// Reported as a failure because the index and the conversation now disagree;
	// the rewind itself has happened either way.
	if dropped, derr := m.DropAfter(turn); derr != nil {
		return RewindResult{Preview: preview}, fmt.Errorf(
			"已还原文件并截断会话，但检查点索引未同步（被撤销的轮次仍可被回退）: %w", derr)
	} else if dropped > 0 {
		a.Config.Logger.Info(ctx, "Agent: dropped checkpoints left behind by the rewind", "turn", turn, "dropped", dropped)
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
// It also refuses a conversation that has been compacted ONWARDS (see
// compactionSuccessor) — that one is not about safety but about crossing a
// boundary: the files would go back to before the summary while the
// conversation that is really running continues in the successor session.
func (a *AIAgent) rewindAllowed() error {
	a.mu.RLock()
	rs := a.currentRun
	a.mu.RUnlock()
	if rs != nil && !rs.isFinished() {
		return errors.New("有回合正在运行，请先停止再回退")
	}
	if why := a.rewindBlockedByCompaction(); why != "" {
		return errors.New(why)
	}
	return nil
}

// RewindChainBlocked explains why NO rewind of this conversation can run, or "".
//
// Exported for the desktop's chain surface, which lists every rewind point at once: a refusal
// that is true of all of them has to be said once, at the top, instead of being rediscovered by
// clicking row after row and reading the same sentence each time.
func (a *AIAgent) RewindChainBlocked() string { return a.rewindBlockedByCompaction() }

// rewindBlockedByCompaction explains why this conversation cannot be rewound at all, or "".
//
// Compaction does not rewrite this session — it starts a NEW one that continues from a
// summary (agent/compact.go), so what this session's checkpoints describe is the state before
// that point, and the conversation the user is living in is the successor. Rewinding here
// would move the workspace backwards under a successor that has kept writing since, with the
// two sessions' checkpoints then describing inconsistent trees and neither knowing it. So it
// is refused by name, and the successor is pointed at — that is where a rewind belongs.
func (a *AIAgent) rewindBlockedByCompaction() string {
	child := a.compactionSuccessor()
	if child == nil {
		return ""
	}
	where := child.ID
	if child.Title != "" {
		where = fmt.Sprintf("「%s」", child.Title)
	}
	return fmt.Sprintf("这个会话已经被压缩接续（%s）：对话在那边继续，回退会把工作区退到摘要之前，因此不再支持回退。要回退请在那边做。", where)
}

// compactionSuccessor returns the session this conversation was compacted INTO, or nil.
//
// Two sources, and the order is not arbitrary: the link this session holds (CompactedChildID),
// then a scan for a session naming this one as its parent. The scan is not redundant —
// compact.go writes the parent's side BEST-EFFORT (a failed UpdateMeta is logged and ignored,
// and the new session's own parent link is enough for the sidebar), so a store can hold a
// child whose predecessor does not point back. The stored link is returned even when the child
// is not in the list (a store we can read but that no longer holds it), because "this
// conversation moved on" is exactly what must not be forgotten.
func (a *AIAgent) compactionSuccessor() *session.Session {
	sm := a.Config.SessionManager
	if sm == nil {
		return nil
	}
	cur := sm.Current()
	if cur == nil {
		return nil
	}
	child := &session.Session{ID: cur.CompactedChildID}
	if list, err := sm.List(); err == nil {
		for _, s := range list {
			if s == nil || s.ID == cur.ID {
				continue
			}
			if s.CompactedParentID == cur.ID {
				return s
			}
			if child.ID != "" && s.ID == child.ID {
				child = s // the full record, so the refusal can name the successor
			}
		}
	}
	if child.ID == "" {
		return nil
	}
	return child
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

// rewindTag names a rewind's sidecars. It carries a nanosecond timestamp because
// a session can be rewound to the same turn more than once (work happens in
// between, and a rewind is cheap to repeat) — with second granularity two of them
// land on the same file and the older abandoned branch is overwritten.
func rewindTag(turn int) string {
	return fmt.Sprintf("turn-%d-%d", turn, time.Now().UnixNano())
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
//
// It names the two outcomes that are NOT a file change out loud, because the one
// thing this feature must never do is let "the conversation moved and the files
// did not" read as "everything went back".
func describeRewindPreview(p RewindPreview) string {
	if p.Blocked != "" {
		return "不能回退：" + p.Blocked
	}
	if p.NoFiles != "" {
		return "回退（未还原任何文件：" + p.NoFiles + "）"
	}
	if p.FilesUnchanged {
		return "回退：这一轮及之后没有文件改动，工作区无需还原"
	}
	var parts []string
	for _, r := range p.Roots {
		parts = append(parts, fmt.Sprintf("%s: 改 %d / 删 %d / 增 %d",
			r.Root, len(r.Changed), len(r.Deleted), len(r.Added)))
	}
	if p.RootMismatch != "" {
		// The tree being written is not the one on screen: the summary has to carry
		// that, or a log line reads as "the workspace I am looking at went back".
		parts = append(parts, "注意："+p.RootMismatch)
	}
	if len(parts) == 0 {
		return "回退：文件无变化"
	}
	return "回退：" + strings.Join(parts, "；")
}
