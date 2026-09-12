package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// SessionInfo is a lightweight session summary for the sidebar list.
type SessionInfo struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Provider  string    `json:"provider"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SessionMessage mirrors a raw session message (with iteration/seq/timestamp)
// so the frontend can reconstruct the true in-turn ordering and show timestamps.
type SessionMessage struct {
	Role       string       `json:"role"` // user/assistant/tool_call/tool_result/reminder
	Content    string       `json:"content"`
	Timestamp  string       `json:"timestamp,omitempty"` // RFC3339
	Iteration  int          `json:"iteration,omitempty"` // 1-based LLM call within the turn
	Seq        int          `json:"seq,omitempty"`       // session-wide request # (0 = not request-bound)
	Thinking   string       `json:"thinking,omitempty"`
	ToolCalls  []ToolCallVo `json:"toolCalls,omitempty"`
	ToolName   string       `json:"toolName,omitempty"`
	ToolResult string       `json:"toolResult,omitempty"`
	ToolCallID string       `json:"toolCallId,omitempty"`
	Title      string       `json:"title,omitempty"` // human-readable args summary
	Args       string       `json:"args,omitempty"`  // raw JSON args
	IsError    bool         `json:"isError,omitempty"`
	// Change is the file change this tool call set out to make (nil for tools that
	// change no text). It is DERIVED from Args, never persisted, so reloading any old
	// session shows the same diffs with no migration.
	Change *FileChangeVO `json:"change,omitempty"`
}

// SessionPage is a page of a session's raw messages plus whether older
// messages remain to be loaded.
type SessionPage struct {
	Messages []SessionMessage `json:"messages"`
	HasMore  bool             `json:"hasMore"`
}

// sessionPageSize is the default number of raw messages loaded per page.
const sessionPageSize = 100

// ToolCallVo is a minimal tool-call descriptor for rendering a tool card.
type ToolCallVo struct {
	Name      string `json:"name"`
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`     // human-readable args summary (reuses tools.ToolArgsSummary)
	Arguments string `json:"arguments,omitempty"` // raw JSON args for full view
}

// ListSessions returns all tachi sessions, most recently updated first.
func (s *AgentService) ListSessions() []SessionInfo {
	if s.desk.sm == nil {
		return nil
	}
	list, err := s.desk.sm.List()
	if err != nil {
		return nil
	}
	infos := make([]SessionInfo, 0, len(list))
	for _, ss := range list {
		infos = append(infos, toSessionInfo(ss))
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].UpdatedAt.After(infos[j].UpdatedAt) })
	return infos
}

// DeleteSession deletes a session and its per-session run state. If the
// deleted session is the currently displayed one, activeID is cleared so the
// UI falls back to choosing/creating a session.
func (s *AgentService) DeleteSession(id string) string {
	d := s.desk
	if d.sm == nil {
		return "no session manager"
	}
	if err := d.sm.Delete(id); err != nil {
		return err.Error()
	}
	d.mu.Lock()
	delete(d.runs, id)
	if d.activeID == id {
		d.activeID = ""
	}
	d.mu.Unlock()
	return "ok"
}

// RenameSession sets a session's title (used by the sidebar rename action).
func (s *AgentService) RenameSession(id, title string) string {
	d := s.desk
	if d.sm == nil {
		return "no session manager"
	}
	if strings.TrimSpace(title) == "" {
		return "empty title"
	}
	sess, err := d.sm.Load(id)
	if err != nil {
		return err.Error()
	}
	sess.Title = title
	if err := d.sm.UpdateMeta(sess); err != nil {
		return err.Error()
	}
	return "ok"
}

// CurrentSession returns the active (displayed) session (nil if none).
func (s *AgentService) CurrentSession() *SessionInfo {
	r := s.desk.activeRun()
	if r == nil || r.sm == nil {
		return nil
	}
	cur := r.sm.Current()
	if cur == nil {
		return nil
	}
	info := toSessionInfo(cur)
	return &info
}

// NewSession creates a fresh session and its own per-session agent, making it
// active. The in-memory history starts empty.
func (s *AgentService) NewSession() SessionInfo {
	d := s.desk
	pname := "default"
	if d.cfg != nil {
		if p := d.cfg.DefaultProviderName(); p != "" {
			pname = p
		}
	}
	// A new session starts in the workspace the user last used, not at $HOME: the
	// home directory (or the filesystem root) is too wide to be an agent workspace —
	// the @-file index would cover everything, and relative paths would resolve
	// against a directory that is not a project. With nothing to inherit the session
	// starts WITHOUT a workspace and the composer asks for one (see
	// defaultWorkspaceFor / wideRootReason).
	wd := d.defaultWorkspaceFor()
	sm := d.newSessionManager()
	if sm == nil {
		return SessionInfo{}
	}
	sess, err := sm.New(pname, wd)
	if err != nil {
		return SessionInfo{}
	}

	// Build this session's own agent. No config (bootstrap failed) → leave the
	// run agent-less so turns fall back to simulation.
	var a *agent.AIAgent
	if d.cfg != nil {
		a, err = d.buildAgentForSession(context.Background(), sm)
		if err != nil {
			a = nil
		} else {
			if _, perr := a.SetResolvedProvider(sess.ProviderName); perr != nil {
				// ignore: fall back to the default provider
				_ = perr
			}
			applyThinking(a, sess.ThinkingLevel)
		}
	}

	d.mu.Lock()
	r := d.getRun(sess.ID)
	r.agent = a
	r.sm = sm
	r.agentProvider = sess.ProviderName
	r.history = nil
	r.running = false
	d.activeID = sess.ID
	d.mu.Unlock()
	d.setSessionState(sess.ID, AgentState{Status: StatusIdle, Label: "空闲", Detail: "新会话"})
	return toSessionInfo(sess)
}

// LoadSession loads the most recent `limit` raw messages of a session, makes it
// current (repointing the desktop's active session at its per-session
// agent/manager), restores its LLM history, and reports whether older messages
// remain. limit <= 0 falls back to the default page size.
func (s *AgentService) LoadSession(id string, limit int) SessionPage {
	d := s.desk
	if d.sm == nil {
		return SessionPage{}
	}
	// Switching sessions does NOT cancel in-flight turns (sessions run in
	// parallel); each session keeps its own run state keyed by session ID.
	// prepareSession builds/returns the session's own agent + manager, applying
	// its provider/thinking overrides.
	r, err := d.prepareSession(context.Background(), id)
	if err != nil {
		return SessionPage{}
	}
	d.mu.Lock()
	d.activeID = id
	d.mu.Unlock()

	// Restore the session's LLM history from its own manager (per-session).
	var msgs []llm.Message
	if r.agent != nil {
		if h, herr := r.agent.LoadSessionHistory(); herr == nil {
			msgs = h
		}
	}
	raw, hasMore := d.pageSessionMessages(r, id, "", limit)

	d.mu.Lock()
	r.history = msgs
	running := r.running
	d.mu.Unlock()
	st := AgentState{Status: StatusIdle, Label: "空闲", Detail: "已加载会话"}
	if running {
		st = r.state
	}
	d.setSessionState(id, st)

	return SessionPage{Messages: buildSessionMessages(raw), HasMore: hasMore}
}

// LoadSessionMore loads up to `limit` raw messages strictly older than `before`
// (RFC3339, the oldest already-loaded message), for the given session. It does
// NOT change the active session — it is the "scroll up" counterpart to
// LoadSession used after the initial page.
func (s *AgentService) LoadSessionMore(id, before string, limit int) SessionPage {
	d := s.desk
	r := d.getRun(id)
	if r == nil || r.sm == nil {
		return SessionPage{}
	}
	raw, hasMore := d.pageSessionMessages(r, id, before, limit)
	return SessionPage{Messages: buildSessionMessages(raw), HasMore: hasMore}
}

// ActivateSession makes id the displayed session and ensures its per-session
// agent/manager is ready, WITHOUT reloading message history. Lightweight
// counterpart to LoadSession for switching to a session already in the cache.
func (s *AgentService) ActivateSession(id string) string {
	d := s.desk
	if d.sm == nil {
		return "no session manager"
	}
	if _, err := d.prepareSession(context.Background(), id); err != nil {
		return err.Error()
	}
	d.mu.Lock()
	d.activeID = id
	d.mu.Unlock()
	return "ok"
}

// buildSessionMessages converts raw session messages into the frontend payload,
// preserving the true in-turn ordering (assistant text / tool calls / tool
// results interleaved) and carrying each message's timestamp/iteration/seq.
func buildSessionMessages(raw []session.Message) []SessionMessage {
	out := make([]SessionMessage, 0, len(raw))
	var pendingThinking []string
	for _, rm := range raw {
		switch rm.Type {
		case session.MessageTypeUser:
			out = append(out, SessionMessage{Role: "user", Content: rm.Content, Timestamp: rm.Timestamp.Format(time.RFC3339)})
		case session.MessageTypeReminder:
			out = append(out, SessionMessage{Role: "reminder", Content: rm.Content, Timestamp: rm.Timestamp.Format(time.RFC3339)})
		case session.MessageTypeThinking:
			pendingThinking = append(pendingThinking, rm.Content)
		case session.MessageTypeAssistant:
			sm := SessionMessage{Role: "assistant", Content: rm.Content, Timestamp: rm.Timestamp.Format(time.RFC3339), Iteration: rm.Iteration, Seq: rm.Seq}
			if len(pendingThinking) > 0 {
				sm.Thinking = strings.Join(pendingThinking, "\n")
				pendingThinking = nil
			}
			out = append(out, sm)
		case session.MessageTypeToolCall:
			argsJSON := marshalArgs(rm.Args)
			out = append(out, SessionMessage{
				Role: "tool_call", ToolName: rm.Name, ToolCallID: rm.ToolCallID,
				Args: argsJSON, Title: tools.ToolArgsSummary(rm.Name, argsJSON),
				Change:    changeVO(rm.Name, argsJSON),
				Iteration: rm.Iteration, Seq: rm.Seq, Timestamp: rm.Timestamp.Format(time.RFC3339),
			})
		case session.MessageTypeToolResult:
			out = append(out, SessionMessage{
				Role: "tool_result", ToolName: rm.Name, ToolCallID: rm.ToolCallID,
				ToolResult: rm.Result, IsError: rm.IsError,
				Iteration: rm.Iteration, Seq: rm.Seq, Timestamp: rm.Timestamp.Format(time.RFC3339),
			})
		}
	}
	return out
}

// pageSessionMessages returns up to limit raw messages for a session, ordered
// oldest→newest. When before (RFC3339) is non-empty, only messages strictly
// older than it are considered (the "load earlier" page). Returns the page plus
// whether more older messages exist.
func (d *desktopApp) pageSessionMessages(r *sessionRun, id, before string, limit int) ([]session.Message, bool) {
	if r == nil || r.sm == nil {
		return nil, false
	}
	if limit <= 0 {
		limit = sessionPageSize
	}
	raw, err := r.sm.LoadSessionMessages(id)
	if err != nil {
		return nil, false
	}
	pool := raw
	if before != "" {
		if t, perr := time.Parse(time.RFC3339, before); perr == nil {
			pool = make([]session.Message, 0, len(raw))
			for _, m := range raw {
				if m.Timestamp.Before(t) {
					pool = append(pool, m)
				}
			}
		}
	}
	hasMore := len(pool) > limit
	if hasMore {
		pool = pool[len(pool)-limit:]
	}
	return pool, hasMore
}

func toSessionInfo(ss *session.Session) SessionInfo {
	return SessionInfo{
		ID:        ss.ID,
		Title:     ss.Title,
		Provider:  ss.ProviderName,
		CreatedAt: ss.CreatedAt,
		UpdatedAt: ss.UpdatedAt,
	}
}

// marshalArgs renders a tool call's args into a stable JSON string.
func marshalArgs(v any) string {
	if v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// GetSessionWorkingDir returns the session's working directory ("" if unset).
func (s *AgentService) GetSessionWorkingDir(id string) string {
	return s.desk.sessionWorkDir(id)
}

// SetSessionWorkingDir changes the session's working directory and persists it
// to session meta. The NEXT turn runs tools under this directory (per-turn
// wdctx injection, like channel agent_turn.go). Returns "ok" on success.
func (s *AgentService) SetSessionWorkingDir(id, dir string) string {
	d := s.desk
	if d.sm == nil {
		return errNoSessionManager.Error()
	}
	if strings.TrimSpace(dir) == "" {
		return "empty dir"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err.Error()
	}
	// The primary is guarded too, not just the additional roots: $HOME used to be
	// every new session's default, and a session pointed at it indexes the whole home
	// directory for @-completion.
	if reason := wideRootReason(abs); reason != "" {
		return reason
	}
	if err := d.updateSessionMeta(id, func(sess *session.Session) {
		sess.WorkingDir = abs
		// A directory that just BECAME the primary is no longer an additional root:
		// re-normalizing drops it (and any duplicate), so the root set stays
		// truthful no matter whether the change came from the folder picker or from
		// the root list.
		if roots, nerr := agent.NormalizeAdditionalRoots(abs, sess.AdditionalDirs); nerr == nil {
			sess.AdditionalDirs = roots
		}
	}); err != nil {
		return err.Error()
	}
	// Remember the explicit choice: the next new session starts here.
	rememberWorkspace(abs)
	return "ok"
}
