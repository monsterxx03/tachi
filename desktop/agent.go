package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// AgentStatus enumerates the runtime states the agent can be in. These are
// derived from the real AgentEvent stream.
type AgentStatus string

const (
	StatusIdle        AgentStatus = "idle"
	StatusThinking    AgentStatus = "thinking"
	StatusToolRunning AgentStatus = "tool_running"
	StatusBusy        AgentStatus = "busy"
	StatusError       AgentStatus = "error"
)

// AgentState is the payload pushed to the frontend (and shown in the menu bar).
type AgentState struct {
	Status AgentStatus `json:"status"`
	Label  string      `json:"label"`  // short menu-bar text, e.g. "思考"
	Detail string      `json:"detail"` // one-line human description
}

// AgentService is a Wails-bound service. In S2 it drives the REAL tachi agent
// (RunConversationStream) when configured, falling back to a simulated turn
// otherwise.
type AgentService struct {
	desk *desktopApp
}

// GetState returns the current agent state.
func (s *AgentService) GetState() AgentState {
	return s.desk.currentState()
}

// SendMessage starts a turn. It returns immediately; state changes are
// streamed to the frontend via the "agent:state" event and to the menu bar.
func (s *AgentService) SendMessage(text string) string {
	if len(text) == 0 {
		return "empty"
	}
	s.desk.startTurn(text)
	return "ok"
}

// Stop aborts the current turn (real agent via cancel, or simulated) and
// returns to idle.
func (s *AgentService) Stop() string {
	s.desk.stopTurn()
	return "ok"
}

// SessionInfo is a lightweight session summary for the sidebar list.
type SessionInfo struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Provider  string    `json:"provider"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ToolCallVo is a minimal tool-call descriptor for rendering a tool card.
type ToolCallVo struct {
	Name      string `json:"name"`
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`     // human-readable args summary (reuses tools.ToolArgsSummary)
	Arguments string `json:"arguments,omitempty"` // raw JSON args for full view
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
}

// MCPToolVO describes one tool under an MCP server for the status-bar panel.
type MCPToolVO struct {
	Name        string `json:"name"`     // full name "mcp__server__tool"
	ToolName    string `json:"toolName"` // original MCP name (no prefix)
	Description string `json:"description,omitempty"`
	Loaded      bool   `json:"loaded"`
}

// MCPServerVO describes an MCP server and its tools for the status-bar panel.
type MCPServerVO struct {
	Name      string      `json:"name"`
	Type      string      `json:"type"`
	Connected bool        `json:"connected"`
	Tools     []MCPToolVO `json:"tools"`
}

// RunningSessions returns the IDs of sessions with an in-flight turn.
func (s *AgentService) RunningSessions() []string {
	s.desk.mu.Lock()
	defer s.desk.mu.Unlock()
	var out []string
	for id, r := range s.desk.runs {
		if r.running {
			out = append(out, id)
		}
	}
	return out
}

// ListProviders returns the configured providers (priority-ordered).
func (s *AgentService) ListProviders() []config.ProviderConfig {
	if s.desk.cfg == nil {
		return nil
	}
	return s.desk.cfg.Providers
}

// SwitchProvider switches the active session's agent to the given config
// provider, persisting it to that session's metadata (like TUI does).
func (s *AgentService) SwitchProvider(name string) string {
	d := s.desk
	if d.cfg == nil {
		return "agent not ready"
	}
	id := d.currentID()
	if id == "" {
		return "no current session"
	}
	r, err := d.prepareSession(context.Background(), id)
	if err != nil {
		return err.Error()
	}
	if r.agent == nil {
		return "agent not ready"
	}
	if _, err := r.agent.SetResolvedProvider(name); err != nil {
		return err.Error()
	}
	// Re-apply this session's thinking override (or its default) against the new
	// provider, so switching models doesn't inherit a stale thinking setting.
	if r.sm != nil {
		if curr := r.sm.Current(); curr != nil {
			applyThinking(r.agent, curr.ThinkingLevel)
			if curr.ProviderName != name {
				curr.ProviderName = name
				_ = r.sm.UpdateMeta(curr) // best-effort
			}
		}
	}
	d.mu.Lock()
	r.agentProvider = name
	d.mu.Unlock()
	return "ok"
}

// GetProviderInfo returns the active session's provider/model/context-window.
func (s *AgentService) GetProviderInfo() map[string]any {
	d := s.desk
	r := d.activeRun()
	if r == nil || r.agent == nil {
		return nil
	}
	provider := ""
	if r.sm != nil {
		if curr := r.sm.Current(); curr != nil && curr.ProviderName != "" {
			provider = curr.ProviderName
		}
	}
	if provider == "" && d.cfg != nil {
		provider = d.cfg.DefaultProviderName()
	}
	// Context estimate: prefer the agent's live estimate; when the session was
	// just opened (no turn yet, estimate is 0) fall back to the most recent
	// message's persisted estimate (usage.estimated_input_tokens) so the
	// context ring shows a sensible value for a resumed session.
	est := r.agent.LastInputEstimate()
	if est <= 0 {
		est = d.estimateFromMessages(r)
	}
	return map[string]any{
		"provider":        provider,
		"model":           r.agent.Model(),
		"contextWindow":   r.agent.ContextWindow(),
		"contextEstimate": est,
	}
}

// estimateFromMessages returns the most recent persisted input-token estimate
// (usage.estimated_input_tokens on a session message, chars/4 heuristic), or 0
// when the session has never carried an estimate. Mirrors TUI's session-restore
// recovery (tui/session_selector.go).
func (d *desktopApp) estimateFromMessages(r *sessionRun) int64 {
	if r == nil || r.sm == nil {
		return 0
	}
	msgs, err := r.sm.LoadMessages()
	if err != nil {
		return 0
	}
	var est int64
	for _, m := range msgs {
		if m.Usage != nil && m.Usage.EstimatedInputTokens > 0 {
			est = m.Usage.EstimatedInputTokens
		}
	}
	return est
}

// GetThinkingLevel returns the active session's thinking level. Empty =
// follow the provider default (surfaced as "default").
func (s *AgentService) GetThinkingLevel() string {
	r := s.desk.activeRun()
	if r == nil || r.sm == nil {
		return "default"
	}
	curr := r.sm.Current()
	if curr == nil || curr.ThinkingLevel == "" {
		return "default"
	}
	return curr.ThinkingLevel
}

// SetThinkingLevel sets the active session's thinking level. "default"/""
// means DON'T set it — follow the provider's own default; "none" disables
// thinking. Persisted to the session's metadata.
func (s *AgentService) SetThinkingLevel(level string) string {
	d := s.desk
	if d.cfg == nil {
		return "agent not ready"
	}
	id := d.currentID()
	if id == "" {
		return "no current session"
	}
	r, err := d.prepareSession(context.Background(), id)
	if err != nil {
		return err.Error()
	}
	if r.agent == nil {
		return "agent not ready"
	}
	applyThinking(r.agent, level)
	store := level
	if level == "default" || level == "" {
		store = ""
	}
	if r.sm != nil {
		if curr := r.sm.Current(); curr != nil {
			curr.ThinkingLevel = store
			_ = r.sm.UpdateMeta(curr) // best-effort
		}
	}
	return "ok"
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
	wd, err := os.UserHomeDir()
	if err != nil {
		wd, _ = os.Getwd()
	}
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

// SessionPage is a page of a session's raw messages plus whether older
// messages remain to be loaded.
type SessionPage struct {
	Messages []SessionMessage `json:"messages"`
	HasMore  bool             `json:"hasMore"`
}

// sessionPageSize is the default number of raw messages loaded per page.
const sessionPageSize = 100

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

// rebuildCostCredit recomputes the session's cumulative cost (CNY) and ledger
// credit ("积分") from the usage ledger — the same aggregation the TUI /usage
// report uses (row.Cost + row.CreditValue(rate)). Rebuilt on session switch
// and after each usage event; O(rows), and rows is small per session.
// Callers must NOT hold d.mu.
func (d *desktopApp) rebuildCostCredit(r *sessionRun) {
	if r == nil || r.agent == nil || r.sm == nil || !r.sm.HasCurrent() {
		return
	}
	rec := r.agent.UsageRecorder()
	if rec == nil {
		return
	}
	curr := r.sm.Current()
	rows, err := rec.Rows(curr.ID, curr.CreatedAt)
	if err != nil {
		return
	}
	var cost, credit float64
	var lastInTok, lastCrTok int64
	var have bool
	for _, row := range rows {
		cost += row.Cost()
		credit += row.CreditValue(llm.ResolveCreditRate(d.cfg, row.Provider, row.Model))
		lastInTok = row.InputTokens
		lastCrTok = row.CacheReadInputTokens
		have = true
	}
	var rate float64
	// "Recent" cache-hit rate = the LAST call's cache-read / (cache-miss + cache-read).
	// Cache-creation tokens (writing new cache entries) are excluded. Ledger rows are
	// appended in call order, so the last row is the most recent API call.
	if have {
		if total := lastInTok + lastCrTok; total > 0 {
			rate = float64(lastCrTok) / float64(total)
		}
	}
	d.mu.Lock()
	r.cost = cost
	r.credit = credit
	r.cacheHitRate = rate
	d.mu.Unlock()
}

// GetSessionUsage returns the session's cumulative cost (CNY) and ledger
// credit ("积分") for the status bar. Both are 0 when no usage is recorded.
func (s *AgentService) GetSessionUsage(id string) map[string]any {
	d := s.desk
	d.mu.Lock()
	r := d.getRun(id)
	d.mu.Unlock()
	d.rebuildCostCredit(r)
	d.mu.Lock()
	cost, credit := r.cost, r.credit
	rate := r.cacheHitRate
	d.mu.Unlock()
	return map[string]any{"cost": cost, "credit": credit, "cacheHitRate": rate}
}

// GetSessionWorkingDir returns the session's working directory ("" if unset).
func (s *AgentService) GetSessionWorkingDir(id string) string {
	d := s.desk
	d.mu.Lock()
	r := d.getRun(id)
	d.mu.Unlock()
	if r == nil || r.sm == nil {
		return ""
	}
	if cur := r.sm.Current(); cur != nil {
		return cur.WorkingDir
	}
	return ""
}

// SetSessionWorkingDir changes the session's working directory and persists it
// to session meta. The NEXT turn runs tools under this directory (per-turn
// wdctx injection, like channel agent_turn.go). Returns "ok" on success.
func (s *AgentService) SetSessionWorkingDir(id, dir string) string {
	d := s.desk
	if d.sm == nil {
		return "no session manager"
	}
	if strings.TrimSpace(dir) == "" {
		return "empty dir"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err.Error()
	}
	d.mu.Lock()
	r := d.getRun(id)
	d.mu.Unlock()
	if r != nil && r.sm != nil {
		if cur := r.sm.Current(); cur != nil {
			cur.WorkingDir = abs
			_ = r.sm.UpdateMeta(cur)
			return "ok"
		}
	}
	// Fallback via the stable manager when the per-session sm is not bound.
	sess, err := d.sm.Load(id)
	if err != nil {
		return err.Error()
	}
	sess.WorkingDir = abs
	if err := d.sm.UpdateMeta(sess); err != nil {
		return err.Error()
	}
	return "ok"
}

// ListMCPServers returns all configured MCP servers with their tools and, for
// each tool, whether it is loaded (discovered/opt-in) in the CURRENT session.
// Only connected servers expose their tools (the pool); disabled ones show
// with no tools until connected.
func (s *AgentService) ListMCPServers() []MCPServerVO {
	d := s.desk
	if d.cfg == nil || d.mcp == nil {
		return nil
	}
	d.mu.Lock()
	sid := d.activeID
	d.mu.Unlock()
	set := d.mcp.SetFor(sid) // nil when sid is empty (no session context)

	byServer := make(map[string][]*mcp.DeferredTool)
	for _, t := range d.mcp.Pool().All() {
		byServer[t.ServerName] = append(byServer[t.ServerName], t)
	}

	out := make([]MCPServerVO, 0, len(d.cfg.MCPServers))
	for _, srv := range d.cfg.MCPServers {
		vo := MCPServerVO{Name: srv.Name, Type: string(srv.Type), Connected: d.mcp.IsConnected(srv.Name)}
		for _, t := range byServer[srv.Name] {
			loaded := set != nil && set.Contains(t.Name)
			vo.Tools = append(vo.Tools, MCPToolVO{
				Name:        t.Name,
				ToolName:    strings.TrimPrefix(t.Name, "mcp__"+t.ServerName+"__"),
				Description: t.Description,
				Loaded:      loaded,
			})
		}
		out = append(out, vo)
	}
	return out
}

// SetMCPServerEnabled connects (enabled=true) or disconnects (enabled=false) a
// configured MCP server at runtime. It does NOT persist to mcp.json — the
// change is runtime-only (restart reverts to config).
func (s *AgentService) SetMCPServerEnabled(name string, enabled bool) string {
	d := s.desk
	if d.cfg == nil || d.mcp == nil {
		return "mcp not configured"
	}
	var srv *config.MCPServerConfig
	for i := range d.cfg.MCPServers {
		if d.cfg.MCPServers[i].Name == name {
			srv = &d.cfg.MCPServers[i]
			break
		}
	}
	if srv == nil {
		return "server not found"
	}
	if enabled {
		tools, err := d.mcp.Reconnect(context.Background(), srv)
		if err != nil {
			return err.Error()
		}
		for _, t := range tools {
			hint := ""
			if srv.SearchHints != nil {
				hint = srv.SearchHints[t.ToolName()]
			}
			d.mcp.Pool().Add(mcp.NewDeferredToolFromMCPTool(t, hint))
		}
		return "ok"
	}
	if err := d.mcp.Disconnect(name); err != nil {
		return err.Error()
	}
	d.mcp.Pool().RemoveByServer(name)
	return "ok"
}

// SetMCPToolEnabled toggles a single MCP tool for the CURRENT session:
// enabled marks it discovered/opt-in (and registers it with the session's
// agent so it is immediately usable); disabled removes it (and unregisters it).
// Per-session only — no global persistence.
func (s *AgentService) SetMCPToolEnabled(name string, enabled bool) string {
	d := s.desk
	if d.cfg == nil || d.mcp == nil {
		return "mcp not configured"
	}
	d.mu.Lock()
	sid := d.activeID
	r := d.getRun(sid)
	d.mu.Unlock()
	if sid == "" {
		return "no current session"
	}
	set := d.mcp.SetFor(sid)
	if set == nil {
		return "no session context"
	}
	if enabled {
		set.Add(name)
		if r != nil && r.agent != nil {
			if dt := d.mcp.Pool().Get(name); dt != nil && dt.Tool != nil {
				r.agent.RegisterTool(dt.Tool)
			}
		}
		return "ok"
	}
	set.Remove(name)
	if r != nil && r.agent != nil {
		r.agent.UnregisterTool(name)
	}
	return "ok"
}

// ListMCPProfiles returns the active MCP profile and all available profiles
// (mcp.<name>.json in global/project scope).
func (s *AgentService) ListMCPProfiles() map[string]any {
	d := s.desk
	if d.cfg == nil {
		return nil
	}
	return map[string]any{
		"active":    d.cfg.ActiveMCPProfile,
		"available": config.ListMCPProfiles(config.FindProjectRoot()),
	}
}

// SetMCPProfile switches the active MCP profile at runtime (reverts on restart).
// It uses the current session's agent (whose manager is the shared one) and
// reconciles the connected servers via SwitchMCPProfile.
func (s *AgentService) SetMCPProfile(name string) string {
	d := s.desk
	if d.cfg == nil || d.mcp == nil {
		return "mcp not configured"
	}
	d.mu.Lock()
	sid := d.activeID
	r := d.getRun(sid)
	d.mu.Unlock()
	if sid == "" || r == nil || r.agent == nil {
		return "no current session agent"
	}
	if _, err := r.agent.SwitchMCPProfile(context.Background(), name, config.FindProjectRoot()); err != nil {
		return err.Error()
	}
	return "ok"
}

// tpsAdd accumulates the estimated output-token count for a live tokens/sec
// display and pushes the rate to the frontend ("agent:tps"). It mirrors TUI
// StatusBar's AddStreamedOutput + currentTPS semantics (text + thinking
// deltas, guarded against sub-second spikes).
func (d *desktopApp) tpsAdd(id, text string) {
	if text == "" {
		return
	}
	d.mu.Lock()
	r := d.getRun(id)
	if r.tpsStart.IsZero() {
		r.tpsStart = time.Now()
	}
	r.tpsTokens += agent.EstimateTokenCount(text)
	tps := tpsRate(r.tpsTokens, r.tpsStart)
	d.mu.Unlock()
	if d.app != nil && tps > 0 {
		d.app.Event.Emit("agent:tps", map[string]any{"sessionId": id, "tps": tps})
	}
}

// tpsReset freezes the current live rate into tpsLast (for a dimmed "paused"
// readout) and clears the timing segment. Called at tool boundaries / turn end —
// same as TUI StatusBar.ResetTPS.
func (d *desktopApp) tpsReset(id string) {
	d.mu.Lock()
	r := d.getRun(id)
	if tps := tpsRate(r.tpsTokens, r.tpsStart); tps > 0 {
		r.tpsLast = tps
	}
	r.tpsTokens = 0
	r.tpsStart = time.Time{}
	last := r.tpsLast
	d.mu.Unlock()
	if d.app != nil && last > 0 {
		d.app.Event.Emit("agent:tps", map[string]any{"sessionId": id, "tps": 0, "lastTps": last})
	}
}

// tpsRate returns tokens/sec for a segment, or 0 before a full second elapses
// (avoids wild spikes from the first instants) — the same guard as TUI's
// currentTPS.
func tpsRate(tokens int64, start time.Time) int64 {
	if tokens <= 0 || start.IsZero() {
		return 0
	}
	elapsed := time.Since(start)
	if elapsed < time.Second {
		return 0
	}
	return int64(float64(tokens) / elapsed.Seconds())
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

func toSessionInfo(ss *session.Session) SessionInfo {
	return SessionInfo{
		ID:        ss.ID,
		Title:     ss.Title,
		Provider:  ss.ProviderName,
		CreatedAt: ss.CreatedAt,
		UpdatedAt: ss.UpdatedAt,
	}
}

// desktopApp holds everything that outlives a single request: the Wails app,
// the main window, the menu-bar tray, and (in S2) the real tachi agent with its
// session manager and usage ledger.
// sessionRun holds per-session runtime state so multiple conversations can run
// independently (each keeps its own history / in-flight turn / status).
type sessionRun struct {
	// agent is this session's OWN AIAgent (per-session isolation, like channel's
	// per-thread cachedAgent). Lazily created; provider/thinking switches are
	// applied in place via SetResolvedProvider/SetThinking.
	agent *agent.AIAgent
	// sm is this session's OWN session manager (current bound to this session),
	// so concurrent turns never steal another session's "current".
	sm *session.Manager
	// agentProvider is the config provider name the agent was last configured
	// for. Maintained for introspection/debugging; switches are applied in place.
	agentProvider string

	running    bool
	history    []llm.Message
	turnCtx    context.Context
	turnCancel context.CancelFunc
	state      AgentState

	// cost/credit are the session's cumulative CNY cost and ledger credit
	// ("积分"), rebuilt from the usage ledger (see rebuildCostCredit) — the
	// same ledger aggregation the TUI /usage report uses.
	cost   float64
	credit float64

	// cacheHitRate is the session's cumulative prompt-cache hit rate (0..1),
	// rebuilt from the usage ledger alongside cost/credit.
	cacheHitRate float64

	// Live output rate (tokens/sec) for the status bar, following the TUI
	// semantics: tpsTokens accumulates agent.EstimateTokenCount of text +
	// thinking deltas; the rate is frozen into tpsLast at segment boundaries.
	tpsTokens int64
	tpsStart  time.Time
	tpsLast   int64

	// Per-turn footer bookkeeping: cost/credit snapshot at turn start so the
	// turn's incremental cost/credit (run.cost - turnStartCost) can be shown.
	turnStartCost   float64
	turnStartCredit float64
	turnCost        float64
	turnCredit      float64
}

type desktopApp struct {
	app    *application.App
	tray   *application.SystemTray
	window *application.WebviewWindow

	// sm is a stable session manager (no bound current) used to list on-disk
	// sessions. It is created once in initAgent and is never repointed; the
	// "displayed" session is tracked separately via activeID (guarded by mu).
	sm           *session.Manager
	mcp          *mcp.Manager // shared MCP manager (nil = no MCP configured)
	cfg          *config.Config
	systemPrompt string

	// activeID is the ID of the currently displayed session ("" when none).
	// Written under mu by New/Load; read under mu everywhere except where the
	// caller already holds mu.
	activeID string

	mu    sync.Mutex
	runs  map[string]*sessionRun // key: session ID
	simCh chan struct{}          // simulated-turn stop signal
}

func newDesktopApp() *desktopApp {
	return &desktopApp{
		runs:  make(map[string]*sessionRun),
		simCh: nil,
	}
}

func (d *desktopApp) currentID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.activeID
}

func (d *desktopApp) getRun(id string) *sessionRun {
	if d.runs == nil {
		d.runs = make(map[string]*sessionRun)
	}
	r := d.runs[id]
	if r == nil {
		r = &sessionRun{state: AgentState{Status: StatusIdle, Label: "空闲", Detail: "就绪"}}
		d.runs[id] = r
	}
	return r
}

// activeRun returns the run for the currently displayed session (nil when no
// session is active). It is the per-session source of truth for provider/agent
// reading in the UI.
func (d *desktopApp) activeRun() *sessionRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runs[d.activeID]
}

// prepareSession lazily builds (or returns) the per-session agent + manager for
// id, mirroring channel's prepareThreadSession. Each session owns its own
// AIAgent and Manager, so concurrent turns never share agent turn state.
// Callers must NOT hold d.mu.
//
// When no config is available (bootstrap failed) the run is returned with a nil
// agent, and the UI falls back to simulated turns. On a lost build race the
// loser's agent/mcp are closed and the winner is returned.
func (d *desktopApp) prepareSession(ctx context.Context, id string) (*sessionRun, error) {
	d.mu.Lock()
	r := d.getRun(id)
	if r.agent != nil || r.sm != nil {
		d.mu.Unlock()
		return r, nil
	}
	d.mu.Unlock()

	sm := d.newSessionManager()
	if sm == nil {
		return nil, fmt.Errorf("session manager: creation failed")
	}
	sess, err := sm.Load(id)
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}

	// Build the per-session agent only when config is available; otherwise the
	// run is kept agent-less so the UI browses the session and falls back to
	// simulated turns.
	var a *agent.AIAgent
	if d.cfg != nil {
		a, err = d.buildAgentForSession(ctx, sm)
		if err != nil {
			return nil, err
		}
		if sess.ProviderName != "" {
			if _, perr := a.SetResolvedProvider(sess.ProviderName); perr != nil {
				// ignore: fall back to the default provider
				_ = perr
			}
		}
		applyThinking(a, sess.ThinkingLevel)
	}

	d.mu.Lock()
	r = d.getRun(id)
	if r.agent != nil {
		// A concurrent builder won the race; discard ours. (No MCP manager to
		// close here — it is the shared desktop manager, owned by desktopApp.)
		a.Close()
		d.mu.Unlock()
		return r, nil
	}
	r.agent = a
	r.sm = sm
	r.agentProvider = sess.ProviderName
	d.mu.Unlock()
	return r, nil
}

func (d *desktopApp) stateFor(id string) AgentState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.runs[id]; r != nil {
		return r.state
	}
	return AgentState{Status: StatusIdle, Label: "空闲", Detail: "就绪"}
}

func (d *desktopApp) currentState() AgentState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.runs[d.activeID]; r != nil {
		return r.state
	}
	return AgentState{Status: StatusIdle, Label: "空闲", Detail: "就绪"}
}

func (d *desktopApp) reflectState(st AgentState) {
	if d.tray != nil {
		d.tray.SetLabel(st.Label)
		if icon := trayIcon(st.Status); icon != nil {
			d.tray.SetTemplateIcon(icon)
		}
	}
	if d.app != nil {
		d.app.Event.Emit("agent:state", st)
	}
}

func (d *desktopApp) setSessionState(id string, st AgentState) {
	d.mu.Lock()
	r := d.getRun(id)
	r.state = st
	isCurrent := d.activeID == id
	d.mu.Unlock()
	if isCurrent {
		d.reflectState(st)
	}
}

// startTurn dispatches to the active session's real agent when configured,
// otherwise to the simulated fallback so the UI always responds.
func (d *desktopApp) startTurn(text string) {
	d.mu.Lock()
	id := d.activeID
	if id == "" {
		d.mu.Unlock()
		return
	}
	r := d.getRun(id)
	d.mu.Unlock()

	// Ensure this session has its own agent (lazy per-session build). No config
	// (bootstrap failed) → fall back to the simulated turn.
	if r.agent == nil {
		if d.cfg == nil {
			d.startSimulatedTurn(context.Background(), id, text)
			return
		}
		pr, err := d.prepareSession(context.Background(), id)
		if err != nil || pr.agent == nil {
			d.startSimulatedTurn(context.Background(), id, text)
			return
		}
		r = pr
	}

	d.mu.Lock()
	if r.running {
		d.mu.Unlock()
		return
	}
	r.running = true
	// Snapshot cost/credit at turn start so the footer can show this turn's
	// incremental cost/credit when it completes.
	r.turnStartCost = r.cost
	r.turnStartCredit = r.credit
	history := r.history
	// An interrupted session may end with a user message and no matching
	// assistant reply. Merge that trailing user message into the new user
	// message (instead of appending an artificial assistant reply) so the
	// provider never sees consecutive user messages.
	if n := len(history); n > 0 && history[n-1].Role == "user" {
		text = history[n-1].Content + "\n" + text
		history = history[:n-1]
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Inject the session's working directory into the turn context so tool
	// execution (wdctx.Dir) uses it — per-session, like channel agent_turn.go.
	if r.sm != nil {
		if cur := r.sm.Current(); cur != nil && cur.WorkingDir != "" {
			ctx = wdctx.WithDir(ctx, cur.WorkingDir)
		}
	}
	r.turnCtx, r.turnCancel = ctx, cancel
	d.mu.Unlock()

	go func() {
		defer func() {
			d.mu.Lock()
			r.running = false
			d.mu.Unlock()
		}()
		d.setSessionState(id, AgentState{Status: StatusThinking, Label: "思考", Detail: "理解中…"})

		ch := r.agent.RunConversationStream(ctx, history, text, d.systemPrompt, llm.ChatOptions{
			MaxTokens: d.cfg.MaxTokens,
		})
		for ev := range ch {
			d.handleEvent(id, ev)
		}
	}()
}

func (d *desktopApp) stopTurn() {
	d.mu.Lock()
	var c context.CancelFunc
	if id := d.activeID; id != "" {
		if r := d.runs[id]; r != nil {
			c = r.turnCancel
		}
	}
	d.mu.Unlock()
	if c != nil {
		c()
	}
	d.stopSimulatedTurn()
}

// handleEvent maps AgentEvent types to the running state, and forwards the raw
// event to the frontend so it can do streaming rendering.
func (d *desktopApp) handleEvent(id string, ev agent.AgentEvent) {
	isCurrent := d.currentID() == id
	switch ev.Type {
	case agent.AgentEventThinkingDelta:
		d.tpsAdd(id, ev.ThinkingDelta)
		d.setSessionState(id, AgentState{Status: StatusThinking, Label: "思考", Detail: "推理中…"})
	case agent.AgentEventTextDelta:
		d.tpsAdd(id, ev.TextDelta)
		if d.stateFor(id).Status != StatusThinking {
			d.setSessionState(id, AgentState{Status: StatusThinking, Label: "思考", Detail: "生成中…"})
		}
	case agent.AgentEventToolCallStart, agent.AgentEventToolCallArgs:
		d.tpsReset(id)
		d.setSessionState(id, AgentState{Status: StatusToolRunning, Label: "执行", Detail: "调用 " + ev.ToolName})
		// Push a lightweight tool event carrying the human-readable args summary
		// (reuses tools.ToolArgsSummary) so the frontend can render the title
		// and full args.
		if d.app != nil && isCurrent {
			d.app.Event.Emit("agent:tool", map[string]any{
				"name":  ev.ToolName,
				"title": tools.ToolArgsSummary(ev.ToolName, ev.ToolArgs),
				"args":  ev.ToolArgs,
			})
		}
	case agent.AgentEventToolResult:
		d.tpsReset(id)
		d.setSessionState(id, AgentState{Status: StatusToolRunning, Label: "执行", Detail: "工具完成"})
	case agent.AgentEventAutoCompactStart:
		d.setSessionState(id, AgentState{Status: StatusBusy, Label: "处理", Detail: "压缩上下文…"})
	case agent.AgentEventUsage:
		// Recompute the session's cumulative cost/credit from the usage ledger
		// and push it to the status bar (frontend listens for "agent:cost").
		r := d.getRun(id)
		d.rebuildCostCredit(r)
		if d.app != nil {
			d.mu.Lock()
			cost, credit := r.cost, r.credit
			rate := r.cacheHitRate
			d.mu.Unlock()
			if isCurrent {
				d.app.Event.Emit("agent:usage", ev.Usage)
			}
			d.app.Event.Emit("agent:cost", map[string]any{"sessionId": id, "cost": cost, "credit": credit, "cacheHitRate": rate})
		}
	case agent.AgentEventTurnComplete:
		d.tpsReset(id)
		d.setSessionState(id, AgentState{Status: StatusIdle, Label: "空闲", Detail: "已回复"})
		d.mu.Lock()
		r := d.getRun(id)
		if ev.Messages != nil {
			r.history = ev.Messages
		}
		r.turnCost = r.cost - r.turnStartCost
		r.turnCredit = r.credit - r.turnStartCredit
		turnCost, turnCredit := r.turnCost, r.turnCredit
		d.mu.Unlock()
		if d.app != nil && ev.Result != nil && isCurrent {
			d.app.Event.Emit("agent:result", ev.Result)
			d.app.Event.Emit("agent:turn", map[string]any{
				"sessionId":  id,
				"durationMs": ev.Result.Duration.Milliseconds(),
				"iterations": ev.Result.IterationsUsed,
				"cost":       turnCost,
				"credit":     turnCredit,
			})
		}
	case agent.AgentEventError:
		d.tpsReset(id)
		detail := "见日志"
		if ev.Result != nil && ev.Result.Error != nil {
			detail = ev.Result.Error.Error()
		}
		d.setSessionState(id, AgentState{Status: StatusError, Label: "出错", Detail: detail})
		if d.app != nil {
			d.app.Event.Emit("agent:error", map[string]any{"sessionId": id, "error": detail})
		}
	}
	// Forward the raw event for every session so the frontend can keep a
	// per-session message cache (background sessions keep streaming too).
	if d.app != nil {
		d.app.Event.Emit("agent:event", map[string]any{"sessionId": id, "event": ev})
	}
}

// ── Simulated fallback (used only when the real agent failed to bootstrap) ──

func (d *desktopApp) startSimulatedTurn(ctx context.Context, id, _ string) {
	d.mu.Lock()
	if d.simCh != nil {
		select {
		case <-d.simCh:
		default:
			d.mu.Unlock()
			return
		}
	}
	stop := make(chan struct{})
	d.simCh = stop
	d.mu.Unlock()

	sequence := []AgentState{
		{Status: StatusThinking, Label: "思考", Detail: "理解用户意图…"},
		{Status: StatusToolRunning, Label: "执行", Detail: "调用工具 grep"},
		{Status: StatusBusy, Label: "处理", Detail: "组织回答…"},
	}

	go func() {
		for _, st := range sequence {
			select {
			case <-stop:
				d.setSessionState(id, AgentState{Status: StatusIdle, Label: "空闲", Detail: "已停止"})
				d.endSimulatedTurn(stop)
				return
			case <-time.After(1400 * time.Millisecond):
			}
			d.setSessionState(id, st)
		}
		select {
		case <-stop:
		case <-time.After(1200 * time.Millisecond):
		}
		d.setSessionState(id, AgentState{Status: StatusIdle, Label: "空闲", Detail: "已完成回答"})
		d.endSimulatedTurn(stop)
	}()
}

func (d *desktopApp) stopSimulatedTurn() {
	d.mu.Lock()
	stop := d.simCh
	d.mu.Unlock()
	if stop != nil {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
}

func (d *desktopApp) endSimulatedTurn(stop chan struct{}) {
	d.mu.Lock()
	if d.simCh == stop {
		d.simCh = nil
	}
	d.mu.Unlock()
}

// trayIcon returns the menu-bar (template) icon bytes for a given status.
func trayIcon(status AgentStatus) []byte {
	switch status {
	case StatusThinking:
		return iconThinking
	case StatusToolRunning:
		return iconTool
	case StatusBusy:
		return iconBusy
	case StatusError:
		return iconError
	default:
		return iconIdle
	}
}
