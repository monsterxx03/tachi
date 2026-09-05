package main

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
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
	Name string `json:"name"`
	ID   string `json:"id"`
}

// SessionMessage is a single conversation message from the LLM history. It
// carries the tool-call / tool-result and thinking relationship so the frontend
// can group an assistant turn (thinking + text + tool cards) back together
// after a restart.
type SessionMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	Thinking   string       `json:"thinking,omitempty"`
	ToolCalls  []ToolCallVo `json:"toolCalls,omitempty"`
	ToolName   string       `json:"toolName,omitempty"`
	ToolResult string       `json:"toolResult,omitempty"`
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

// CurrentSession returns the active session (nil if none).
func (s *AgentService) CurrentSession() *SessionInfo {
	if s.desk.sm == nil {
		return nil
	}
	cur := s.desk.sm.Current()
	if cur == nil {
		return nil
	}
	info := toSessionInfo(cur)
	return &info
}

// NewSession creates a fresh session and clears the in-memory history.
func (s *AgentService) NewSession() SessionInfo {
	if s.desk.sm == nil {
		return SessionInfo{}
	}
	pname := "default"
	if s.desk.aiAgent != nil {
		pname = s.desk.aiAgent.Provider().Name()
	}
	wd, _ := os.Getwd()
	sess, err := s.desk.sm.New(pname, wd)
	if err != nil {
		return SessionInfo{}
	}
	s.desk.mu.Lock()
	s.desk.history = nil
	s.desk.mu.Unlock()
	s.desk.setState(AgentState{Status: StatusIdle, Label: "空闲", Detail: "新会话"})
	return toSessionInfo(sess)
}

// LoadSession loads a session by ID, makes it current, restores its LLM history
// into the in-memory conversation, and returns the messages for rendering.
func (s *AgentService) LoadSession(id string) []SessionMessage {
	if s.desk.sm == nil {
		return nil
	}
	if _, err := s.desk.sm.Load(id); err != nil {
		return nil
	}
	var msgs []llm.Message
	if s.desk.aiAgent != nil {
		if h, err := s.desk.aiAgent.LoadSessionHistory(); err == nil {
			msgs = h
		}
	}
	// Collect the standalone thinking blocks from the raw session, so we can
	// re-attach them to assistant turns even when the provider merges them into
	// content (OpenAI family).
	var thinkings []string
	if raw, err := s.desk.sm.LoadSessionMessages(id); err == nil {
		for _, rm := range raw {
			if rm.Type == session.MessageTypeThinking {
				thinkings = append(thinkings, rm.Content)
			}
		}
	}
	s.desk.mu.Lock()
	s.desk.history = msgs
	s.desk.mu.Unlock()
	s.desk.setState(AgentState{Status: StatusIdle, Label: "空闲", Detail: "已加载会话"})

	out := make([]SessionMessage, 0, len(msgs))
	ti := 0
	for _, m := range msgs {
		sm := SessionMessage{Role: m.Role, Content: m.Content}
		for _, tc := range m.ToolCalls {
			sm.ToolCalls = append(sm.ToolCalls, ToolCallVo{Name: tc.Function.Name, ID: tc.ID})
		}
		if m.Role == "tool" {
			sm.ToolName = m.Name
			sm.ToolResult = m.Content
		}
		// Re-associate thinking with the assistant turn.
		if m.Role == "assistant" {
			if len(m.ThinkingBlocks) > 0 {
				var sb strings.Builder
				for _, tb := range m.ThinkingBlocks {
					sb.WriteString(tb.Thinking)
					sb.WriteString("\n")
				}
				sm.Thinking = strings.TrimSpace(sb.String())
				// OpenAI merged thinking into content; strip it back out.
				if strings.HasPrefix(m.Content, sm.Thinking) {
					rest := strings.TrimPrefix(m.Content, sm.Thinking)
					rest = strings.TrimSpace(strings.TrimPrefix(rest, "\n\n"))
					sm.Content = strings.TrimSpace(rest)
				}
			} else if ti < len(thinkings) {
				sm.Thinking = thinkings[ti]
				ti++
				if strings.HasPrefix(m.Content, sm.Thinking) {
					rest := strings.TrimPrefix(m.Content, sm.Thinking)
					rest = strings.TrimSpace(strings.TrimPrefix(rest, "\n\n"))
					sm.Content = strings.TrimSpace(rest)
				}
			}
		}
		out = append(out, sm)
	}
	return out
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
type desktopApp struct {
	app    *application.App
	tray   *application.SystemTray
	window *application.WebviewWindow

	aiAgent      *agent.AIAgent
	mcp          *mcp.Manager
	sm           *session.Manager
	cfg          *config.Config
	systemPrompt string

	mu         sync.Mutex
	state      AgentState
	running    bool          // a real agent turn is in progress
	history    []llm.Message // running conversation history (feeds next turn)
	turnCtx    context.Context
	turnCancel context.CancelFunc
	simCh      chan struct{} // simulated-turn stop signal
	quitCh     chan struct{}
}

func newDesktopApp() *desktopApp {
	return &desktopApp{
		state:  AgentState{Status: StatusIdle, Label: "空闲", Detail: "就绪"},
		quitCh: make(chan struct{}),
	}
}

func (d *desktopApp) currentState() AgentState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

func (d *desktopApp) setState(st AgentState) {
	d.mu.Lock()
	d.state = st
	d.mu.Unlock()

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

// startTurn dispatches to the real agent when configured, otherwise to the
// simulated fallback so the UI always responds.
func (d *desktopApp) startTurn(text string) {
	if d.aiAgent == nil {
		d.startSimulatedTurn(context.Background(), text)
		return
	}

	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return
	}
	d.running = true
	history := d.history
	ctx, cancel := context.WithCancel(context.Background())
	d.turnCtx, d.turnCancel = ctx, cancel
	d.mu.Unlock()

	go func() {
		defer func() {
			d.mu.Lock()
			d.running = false
			d.mu.Unlock()
		}()
		d.setState(AgentState{Status: StatusThinking, Label: "思考", Detail: "理解中…"})

		ch := d.aiAgent.RunConversationStream(ctx, history, text, d.systemPrompt, llm.ChatOptions{
			MaxTokens: d.cfg.MaxTokens,
		})
		for ev := range ch {
			d.handleEvent(ev)
		}
	}()
}

func (d *desktopApp) stopTurn() {
	d.mu.Lock()
	c := d.turnCancel
	d.mu.Unlock()
	if c != nil {
		c()
	}
	d.stopSimulatedTurn()
}

// handleEvent maps AgentEvent types to the running state, and forwards the raw
// event to the frontend so it can do streaming rendering.
func (d *desktopApp) handleEvent(ev agent.AgentEvent) {
	switch ev.Type {
	case agent.AgentEventThinkingDelta:
		d.setState(AgentState{Status: StatusThinking, Label: "思考", Detail: "推理中…"})
	case agent.AgentEventTextDelta:
		if d.currentState().Status != StatusThinking {
			d.setState(AgentState{Status: StatusThinking, Label: "思考", Detail: "生成中…"})
		}
	case agent.AgentEventToolCallStart, agent.AgentEventToolCallArgs:
		d.setState(AgentState{Status: StatusToolRunning, Label: "执行", Detail: "调用 " + ev.ToolName})
	case agent.AgentEventToolResult:
		d.setState(AgentState{Status: StatusToolRunning, Label: "执行", Detail: "工具完成"})
	case agent.AgentEventAutoCompactStart:
		d.setState(AgentState{Status: StatusBusy, Label: "处理", Detail: "压缩上下文…"})
	case agent.AgentEventUsage:
		if d.app != nil {
			d.app.Event.Emit("agent:usage", ev.Usage)
		}
	case agent.AgentEventTurnComplete:
		d.setState(AgentState{Status: StatusIdle, Label: "空闲", Detail: "已回复"})
		d.mu.Lock()
		if ev.Messages != nil {
			d.history = ev.Messages
		}
		d.mu.Unlock()
		if d.app != nil && ev.Result != nil {
			d.app.Event.Emit("agent:result", ev.Result)
		}
	case agent.AgentEventError:
		d.setState(AgentState{Status: StatusError, Label: "出错", Detail: "见日志"})
	}
	// Forward the raw event so the frontend can render streaming deltas.
	if d.app != nil {
		d.app.Event.Emit("agent:event", ev)
	}
}

// ── Simulated fallback (used only when the real agent failed to bootstrap) ──

func (d *desktopApp) startSimulatedTurn(ctx context.Context, _ string) {
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
				d.setState(AgentState{Status: StatusIdle, Label: "空闲", Detail: "已停止"})
				d.endSimulatedTurn(stop)
				return
			case <-time.After(1400 * time.Millisecond):
			}
			d.setState(st)
		}
		select {
		case <-stop:
		case <-time.After(1200 * time.Millisecond):
		}
		d.setState(AgentState{Status: StatusIdle, Label: "空闲", Detail: "已完成回答"})
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

func (d *desktopApp) shutdown() {
	close(d.quitCh)
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
