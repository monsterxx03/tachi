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
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// SessionInfo is a lightweight session summary for the sidebar list.
type SessionInfo struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Provider  string    `json:"provider"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// CompactedParentID names the session this one was compacted FROM ("" when it was not). It is
	// the one piece of the compaction chain the sidebar needs: a chain is ONE conversation, and the
	// frontend folds a parent under the child it became. The child is the newest link, so a fresh
	// launch can rebuild the same shape from meta.json — which the frontend's own memory of the
	// live switch (agent:session_switched) could not do.
	CompactedParentID string `json:"compactedParentId,omitempty"`
	// ProjectID is the desktop project this session belongs to ("" = project-less). Only the ID
	// travels: the sidebar joins it against ListProjects() for the group's name, so renaming a
	// project relabels every row without touching a single session (design §3.3).
	ProjectID string `json:"projectId,omitempty"`
}

// SessionMessage mirrors a raw session message (with iteration/seq/timestamp)
// so the frontend can reconstruct the true in-turn ordering and show timestamps.
type SessionMessage struct {
	// Index is the message's position in the session's own record list
	// (messages.jsonl), NOT in the page it arrived with. A checkpoint boundary is
	// expressed the same way (checkpoint.Record.Records is the record index where
	// a turn's records begin), so this is what lets the transcript say "rewind to
	// the turn that started here" without duplicating any bookkeeping.
	Index   int    `json:"index"`
	Role    string `json:"role"` // user/assistant/tool_call/tool_result/reminder
	Content string `json:"content"`
	// DisplayContent is the user's own text when it differs from Content — only @-file
	// expansion does that today (Content is the file inlined for the model). The transcript
	// shows it for a user record; nothing else reads it.
	DisplayContent string `json:"displayContent,omitempty"`
	Timestamp      string `json:"timestamp,omitempty"` // RFC3339
	Iteration      int    `json:"iteration,omitempty"` // 1-based LLM call within the turn
	Seq            int    `json:"seq,omitempty"`       // session-wide request # (0 = not request-bound)
	// Turn and Changes are the footer's data, stamped on the record that BEGINS a checkpointed
	// turn: a transcript reloaded from disk then shows the same numbers a live one did, from
	// the same source, without the frontend mapping record indexes to turns itself.
	Turn       int            `json:"turn,omitempty"`
	Changes    *TurnChangesVO `json:"changes,omitempty"`
	Thinking   string         `json:"thinking,omitempty"`
	ToolCalls  []ToolCallVo   `json:"toolCalls,omitempty"`
	ToolName   string         `json:"toolName,omitempty"`
	ToolResult string         `json:"toolResult,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	Title      string         `json:"title,omitempty"` // human-readable args summary
	Args       string         `json:"args,omitempty"`  // raw JSON args
	IsError    bool           `json:"isError,omitempty"`
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
//
// A session with a turn in flight is REFUSED, not stopped: the turn owns a
// goroutine that keeps calling the model and running tools, and its writes to
// the session directory (AppendMessage) and to the run map (setSessionState via
// getRun) are keyed by nothing but this id — deleting underneath it loses the
// transcript silently and can leave a rebuilt directory (a meta.json with no
// messages.jsonl) behind. The stop is the user's call, made explicit.
func (s *AgentService) DeleteSession(id string) string {
	d := s.desk
	if d.sm == nil {
		return "no session manager"
	}
	// Read the flag and remove the run in ONE critical section: a turn starting
	// between the two would otherwise be cancelled out of the map mid-flight.
	d.mu.Lock()
	if r := d.runs[id]; r != nil && r.running {
		d.mu.Unlock()
		return refuseDeleteRunning
	}
	if err := d.sm.Delete(id); err != nil {
		d.mu.Unlock()
		return err.Error()
	}
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
//
// projectID is "" for an ordinary session (today's behavior, word for word). Otherwise the
// session is created INSIDE that project (design §6.1) and this is the ONE place a binding is
// ever made: the project's roots as they are right now are written into the record as the
// session's snapshot, alongside project_id. A project that cannot drive a session at this
// moment (deleted, or its primary directory gone) yields an ordinary session instead of one
// bound to nothing — §8.6's degradation is for a project that disappears UNDER a session,
// which is not the same as being asked for one that is not there.
func (s *AgentService) NewSession(projectID string) SessionInfo {
	d := s.desk
	pname := "default"
	if d.cfg != nil {
		if p := d.cfg.DefaultProviderName(); p != "" {
			pname = p
		}
	}
	// Where the new session starts. A project session takes the project's roots (that is what
	// the binding means); an ordinary one starts in the workspace the user last used, not at
	// $HOME: the home directory (or the filesystem root) is too wide to be an agent workspace —
	// the @-file index would cover everything, and relative paths would resolve against a
	// directory that is not a project. With nothing to inherit the session starts WITHOUT a
	// workspace and the composer asks for one (see defaultWorkspaceFor / wideRootReason).
	var snapshot []string
	boundID := ""
	wd := ""
	if p, primary, ok := d.projects.drives(projectID); ok {
		// The primary comes back cleaned/expanded, since projects.json is hand-editable.
		wd = primary
		snapshot = append([]string(nil), p.AdditionalDirs...)
		boundID = p.ID
	} else {
		wd = d.defaultWorkspaceFor()
	}
	sm := d.newSessionManager()
	if sm == nil {
		return SessionInfo{}
	}
	sess, err := sm.New(pname, wd)
	if err != nil {
		return SessionInfo{}
	}
	if boundID != "" {
		// sm.New carries only the provider and the primary directory, so the rest of the
		// snapshot — and the binding — are written by hand, in one update.
		sess.ProjectID = boundID
		sess.AdditionalDirs = snapshot
		if err := sm.UpdateMeta(sess); err != nil {
			// The session itself exists, and this API has no error channel to report into
			// (it answers with SessionInfo like every other creation path). Returning a zero
			// value would leave a session on disk that the UI never hears about, so log the
			// half-written binding and hand the session back: the panel shows it as
			// project-less, which is what it is until someone looks.
			logger.New("desktop").Warn(context.Background(),
				"NewSession: 绑定项目的 meta 写入失败，会话已创建但未绑定项目",
				"session", sess.ID, "project", boundID, "err", err)
		}
	}

	// Build this session's own agent. No config (bootstrap failed) → leave the
	// run agent-less so turns fall back to simulation.
	var a *agent.AIAgent
	if d.cfg != nil {
		a, err = d.buildAgentForSession(context.Background(), sess.ID, sm)
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
	raw, offset, hasMore := d.pageSessionMessages(r, id, "", limit)

	d.mu.Lock()
	r.history = msgs
	running := r.running
	d.mu.Unlock()
	st := AgentState{Status: StatusIdle, Label: "空闲", Detail: "已加载会话"}
	if running {
		st = r.state
	}
	d.setSessionState(id, st)

	return SessionPage{Messages: buildSessionMessages(raw, offset, sessionTurnStamps(r)), HasMore: hasMore}
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
	raw, offset, hasMore := d.pageSessionMessages(r, id, before, limit)
	return SessionPage{Messages: buildSessionMessages(raw, offset, sessionTurnStamps(r)), HasMore: hasMore}
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
	// The switch repaints the menu bar and re-reports the session on screen, so a state
	// the frontend never heard about (this session has not changed since the window
	// loaded) is not left as "whatever was on screen before".
	d.reflectActive(id)
	return "ok"
}

// buildSessionMessages converts raw session messages into the frontend payload,
// preserving the true in-turn ordering (assistant text / tool calls / tool
// results interleaved) and carrying each message's timestamp/iteration/seq.
func buildSessionMessages(raw []session.Message, offset int, stamps map[int]turnStamp) []SessionMessage {
	out := make([]SessionMessage, 0, len(raw))
	var pendingThinking []string
	// The turn each record belongs to, carried across the loop and SEEDED from the boundary in
	// force where the page starts — a page can open in the middle of a turn (see stampBefore).
	stamp := stampBefore(stamps, offset)
	// Every record of a turn carries the turn, not only the one that opens it: the transcript
	// builds its card from whichever record starts it, and a page that begins inside a turn has
	// no opening record of its own. The switch below stays about roles; the stamp rides along.
	emit := func(m SessionMessage, at turnStamp) SessionMessage {
		m.Turn, m.Changes = at.Turn, at.Changes
		return m
	}
	for i, rm := range raw {
		index := offset + i
		if at, ok := stamps[index]; ok {
			stamp = at
		}
		switch rm.Type {
		case session.MessageTypeUser:
			// Iteration is carried for the user role too: a steer message (typed
			// while the agent was working) is recorded as a user message, and this
			// is what tells the two apart — a turn's own prompt has no request
			// attached (Iteration 0), an interjection belongs to the call it was
			// injected before.
			out = append(out, emit(SessionMessage{
				Index: index, Role: "user", Content: rm.Content, DisplayContent: rm.DisplayContent,
				Iteration: rm.Iteration, Seq: rm.Seq, Timestamp: rm.Timestamp.Format(time.RFC3339),
			}, stamp))
		case session.MessageTypeReminder:
			out = append(out, emit(SessionMessage{
				Index: index, Role: "reminder", Content: rm.Content, Timestamp: rm.Timestamp.Format(time.RFC3339),
			}, stamp))
		case session.MessageTypeThinking:
			pendingThinking = append(pendingThinking, rm.Content)
		case session.MessageTypeAssistant:
			sm := SessionMessage{Index: index, Role: "assistant", Content: rm.Content, Timestamp: rm.Timestamp.Format(time.RFC3339), Iteration: rm.Iteration, Seq: rm.Seq}
			if len(pendingThinking) > 0 {
				sm.Thinking = strings.Join(pendingThinking, "\n")
				pendingThinking = nil
			}
			out = append(out, emit(sm, stamp))
		case session.MessageTypeToolCall:
			argsJSON := marshalArgs(rm.Args)
			out = append(out, emit(SessionMessage{
				Index: index, Role: "tool_call", ToolName: rm.Name, ToolCallID: rm.ToolCallID,
				Args: argsJSON, Title: tools.ToolArgsSummary(rm.Name, argsJSON),
				Change:    changeVO(rm.Name, argsJSON),
				Iteration: rm.Iteration, Seq: rm.Seq, Timestamp: rm.Timestamp.Format(time.RFC3339),
			}, stamp))
		case session.MessageTypeToolResult:
			out = append(out, emit(SessionMessage{
				Index: index, Role: "tool_result", ToolName: rm.Name, ToolCallID: rm.ToolCallID,
				ToolResult: rm.Result, IsError: rm.IsError,
				Iteration: rm.Iteration, Seq: rm.Seq, Timestamp: rm.Timestamp.Format(time.RFC3339),
			}, stamp))
		}
	}
	return out
}

// pageSessionMessages returns up to limit raw messages for a session, ordered
// oldest→newest. When before (RFC3339) is non-empty, only messages strictly
// older than it are considered (the "load earlier" page). Returns the page plus
// whether more older messages exist.
func (d *desktopApp) pageSessionMessages(r *sessionRun, id, before string, limit int) ([]session.Message, int, bool) {
	if r == nil || r.sm == nil {
		return nil, 0, false
	}
	if limit <= 0 {
		limit = sessionPageSize
	}
	raw, err := r.sm.LoadSessionMessages(id)
	if err != nil {
		return nil, 0, false
	}
	// Track WHERE each kept message sits in the session's full record list: a
	// checkpoint boundary is a record index, so the page has to carry the absolute
	// position rather than a position within itself (see SessionMessage.Index).
	kept := make([]int, 0, len(raw))
	for i, m := range raw {
		if before != "" {
			t, perr := time.Parse(time.RFC3339, before)
			if perr != nil || !m.Timestamp.Before(t) {
				continue
			}
		}
		kept = append(kept, i)
	}
	hasMore := len(kept) > limit
	if hasMore {
		kept = kept[len(kept)-limit:]
	}
	pool := make([]session.Message, 0, len(kept))
	for _, i := range kept {
		pool = append(pool, raw[i])
	}
	offset := 0
	if len(kept) > 0 {
		offset = kept[0]
	}
	return pool, offset, hasMore
}

func toSessionInfo(ss *session.Session) SessionInfo {
	return SessionInfo{
		ID:                ss.ID,
		Title:             ss.Title,
		Provider:          ss.ProviderName,
		CreatedAt:         ss.CreatedAt,
		UpdatedAt:         ss.UpdatedAt,
		CompactedParentID: ss.CompactedParentID,
		ProjectID:         ss.ProjectID,
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
	if reason := d.projectGuard(id); reason != "" {
		return reason
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
	// Skills move with the session: a store's scan roots are fixed when it is built
	// (sessionSkillStore), so without this a session that changes folder would keep
	// offering the OLD tree's project skills — and send Skill create's "project"
	// target there — until it was reloaded. Marked, not reloaded: beginTurn is the only
	// place that can prove no turn of this session is in flight, and a reload racing a
	// running turn would rewrite the store and the tool registry under it (the same
	// reason SetProjectRoots marks its members). A session with no agent yet is fine: its
	// agent will be built with the new directory, which is persisted above.
	d.markSkillsStale(id)
	// Remember the explicit choice: the next new session starts here.
	rememberWorkspace(abs)
	return "ok"
}
