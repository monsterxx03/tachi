package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// One-off transcript recording — sidecar JSONL files capturing the full
// execution of side-channel LLM runs (/commit, /review, channel ambient,
// dream, github bot) without touching the main session history.
// See docs/2026-07-24-oneoff-transcript-design.md.

// OneOffMeta describes a one-off (side-channel) execution to be recorded.
// An empty Kind disables recording entirely.
type OneOffMeta struct {
	// Kind classifies the execution: "commit" | "review" | "ambient" |
	// "dream" | "compact" | "github-discussion" | "github-pr" | ...
	Kind string

	// SessionID anchors the record under <session>/<id>/oneoff/.
	// Empty → resolved from the agent's current session; if there is none,
	// the record goes to the global <home>/oneoff/<kind>/ dir.
	SessionID string

	// SystemPrompt is recorded in the meta header line — the first thing
	// to inspect when debugging ambient interjections, whisper silence, or
	// review false negatives. RunOneOffStream fills it from its own arg.
	SystemPrompt string

	// Extra holds optional kind-specific context (e.g. dream domain,
	// github repo) recorded in the meta header line.
	Extra map[string]string
}

// oneoffMetaLine is the first line of every one-off transcript file.
// Type is always "meta"; renderers treat unknown types with a default branch.
type oneoffMetaLine struct {
	Type         string            `json:"type"`
	Kind         string            `json:"kind"`
	SessionID    string            `json:"session_id,omitempty"`
	CWD          string            `json:"cwd,omitempty"`
	Provider     string            `json:"provider,omitempty"`
	Model        string            `json:"model,omitempty"`
	StartedAt    time.Time         `json:"started_at"`
	SystemPrompt string            `json:"system_prompt,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
}

// Injectable for tests (mirrors subagent/recorder.go's sessionDirFn).
var (
	oneoffSessionDirFn = config.SessionDir
	oneoffHomeDirFn    = config.OneoffDir
)

// oneoffRecorder appends session.Message lines plus a meta header to a
// single JSONL file. One instance per one-off run; not shared across
// goroutines (each run owns its recorder for its lifetime).
type oneoffRecorder struct {
	file      *os.File
	path      string
	kind      string
	startedAt time.Time
	bytes     int64
}

// newOneoffRecorder creates the recorder, writes the meta header line, and
// (for global-dir records) sweeps files older than retentionDays.
// sessionID empty → global dir; non-empty → per-session dir.
func newOneoffRecorder(
	meta OneOffMeta,
	sessionID string,
	provider llm.Provider,
	cwd string,
	retentionDays int,
) (*oneoffRecorder, error) {
	var dir string
	if sessionID != "" {
		sessionDir, err := oneoffSessionDirFn()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(sessionDir, sessionID, "oneoff")
	} else {
		dir = filepath.Join(oneoffHomeDirFn(), meta.Kind)
		// Lazy retention sweep — only the global dir is age-managed;
		// per-session oneoff dirs die with session eviction.
		sweepOneoffDir(dir, retentionDays)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}

	name := fmt.Sprintf("%s-%s-%s.jsonl",
		meta.Kind, time.Now().Format("20060102-150405"), uuid.NewString()[:4])
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}

	r := &oneoffRecorder{file: f, path: path, kind: meta.Kind, startedAt: time.Now()}

	header := oneoffMetaLine{
		Type:         "meta",
		Kind:         meta.Kind,
		SessionID:    sessionID,
		CWD:          cwd,
		StartedAt:    r.startedAt,
		SystemPrompt: meta.SystemPrompt,
		Extra:        meta.Extra,
	}
	if provider != nil {
		header.Provider = provider.Name()
		header.Model = provider.Model()
	}
	if err := r.writeLine(header); err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

// record appends a session.Message as a JSON line. Best-effort: a write
// failure is swallowed — recording must never break the run itself (the
// agent wiring logs close-time stats, and creation failures are Warn'ed).
func (r *oneoffRecorder) record(msg *session.Message) {
	msg.Timestamp = time.Now()
	_ = r.writeLine(msg)
}

func (r *oneoffRecorder) writeLine(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	n, err := r.file.Write(append(data, '\n'))
	r.bytes += int64(n)
	return err
}

// close closes the file and returns final stats for the debug.log index line.
func (r *oneoffRecorder) close() (path string, size int64, dur time.Duration) {
	_ = r.file.Close()
	return r.path, r.bytes, time.Since(r.startedAt)
}

// sweepOneoffDir deletes *.jsonl files in dir older than retentionDays.
// Best-effort: individual failures are ignored — a future sweep will retry.
func sweepOneoffDir(dir string, retentionDays int) {
	if retentionDays <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // dir may not exist yet
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// --- AIAgent wiring ---

// oneoffEnabled reports whether one-off recording is on (config default true).
func (a *AIAgent) oneoffEnabled() bool {
	return a.cfg == nil || a.cfg.Oneoff.IsEnabled()
}

// oneoffRetentionDays returns the configured global-dir retention (default 30).
func (a *AIAgent) oneoffRetentionDays() int {
	if a.cfg != nil && a.cfg.Oneoff.RetentionDays > 0 {
		return a.cfg.Oneoff.RetentionDays
	}
	return 30
}

// resolveOneoffSessionID picks the anchor session for a one-off record:
// explicit meta.SessionID wins, then the agent's current session.
func (a *AIAgent) resolveOneoffSessionID(meta OneOffMeta) string {
	if meta.SessionID != "" {
		return meta.SessionID
	}
	if a.sessionManager != nil {
		if cur := a.sessionManager.Current(); cur != nil {
			return cur.ID
		}
	}
	return ""
}

// startOneoffRecorder creates and attaches a recorder for this run.
// Returns nil (recording disabled or failed — logged, never fatal).
func (a *AIAgent) startOneoffRecorder(ctx context.Context, meta OneOffMeta, provider llm.Provider) *oneoffRecorder {
	if meta.Kind == "" || !a.oneoffEnabled() {
		return nil
	}
	sessionID := a.resolveOneoffSessionID(meta)
	rec, err := newOneoffRecorder(meta, sessionID, provider, oneoffCWD(ctx), a.oneoffRetentionDays())
	if err != nil {
		a.logWarn(ctx, "oneoff: failed to create recorder", err, "kind", meta.Kind)
		return nil
	}
	a.oneoffRec = rec
	a.logInfo(ctx, "oneoff transcript opened", "kind", meta.Kind, "path", rec.path, "session_id", sessionID)
	return rec
}

// stopOneoffRecorder detaches and closes the recorder, writing the debug.log
// index line (kind + path + trace_id + duration + size) for discoverability.
// The path is kept on lastOneoffPath so frontends (TUI) can surface it after
// the run completes.
func (a *AIAgent) stopOneoffRecorder(ctx context.Context) {
	rec := a.oneoffRec
	if rec == nil {
		return
	}
	a.oneoffRec = nil
	path, size, dur := rec.close()
	a.lastOneoffPath = path
	a.logInfo(ctx, "oneoff transcript written",
		"kind", rec.kind, "path", path, "trace_id", a.turn.trace(),
		"duration", dur.Round(time.Millisecond).String(), "size", size)
}

// LastOneoffTranscriptPath returns the sidecar file path of the most recent
// one-off run ("" if none or recording disabled). Frontends use it to point
// users at the full execution record after /commit or /review.
func (a *AIAgent) LastOneoffTranscriptPath() string {
	return a.lastOneoffPath
}

// AttachOneOffRecorder lets RunConversationStream-based side paths (channel
// ambient) attach a recorder explicitly. Call before the run; the caller must
// detach via stopOneoffRecorder when the run finishes.
func (a *AIAgent) AttachOneOffRecorder(ctx context.Context, meta OneOffMeta) {
	a.startOneoffRecorder(ctx, meta, a.provider)
}

// DetachOneOffRecorder is the exported counterpart of stopOneoffRecorder for
// external callers (channel ambient manager).
func (a *AIAgent) DetachOneOffRecorder(ctx context.Context) {
	a.stopOneoffRecorder(ctx)
}

func (a *AIAgent) logInfo(ctx context.Context, msg string, attrs ...any) {
	if a.logger != nil {
		a.logger.Info(ctx, msg, attrs...)
	}
}

func (a *AIAgent) logWarn(ctx context.Context, msg string, err error, attrs ...any) {
	if a.logger != nil {
		a.logger.Warn(ctx, msg, append(attrs, "error", err)...)
	}
}

// oneoffCWD resolves the working directory for the meta header.
func oneoffCWD(ctx context.Context) string {
	if dir := wdctx.Dir(ctx); dir != "" {
		return dir
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}
