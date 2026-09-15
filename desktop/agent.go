package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileindex"
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
//
// SessionID names the session this state is about, and it is what makes the payload
// usable: the desktop runs each session's turn in its own goroutine (switching sessions
// cancels nothing), so the frontend keeps one state per conversation and files what it
// receives by this id. A payload without one could only be applied to whatever happened to
// be on screen when it landed — which is how a running session's spinner followed the user
// into an idle one.
type AgentState struct {
	SessionID string      `json:"sessionId"`
	Status    AgentStatus `json:"status"`
	Label     string      `json:"label"`  // short menu-bar text, e.g. "思考"
	Detail    string      `json:"detail"` // one-line human description
}

// AgentService is a Wails-bound service. In S2 it drives the REAL tachi agent
// (RunConversationStream) when configured, falling back to a simulated turn
// otherwise.
type AgentService struct {
	desk *desktopApp
}

// GetState returns the current agent state — the DISPLAYED session's, named by that
// session's id so the frontend can file it under the conversation it belongs to.
func (s *AgentService) GetState() AgentState {
	return s.desk.currentState()
}

// RunningSessions returns the IDs of sessions with an in-flight turn.
//
// The answer is the per-session STATE, not the busy flag (`sessionRun.running`, the guard that
// keeps a second turn from starting; `endTurn` clears it). Both describe the same fact, but they
// are cleared at different moments, and the frontend re-reads this list exactly once — on the
// turn's own terminal event (`agent:event` turn_complete / error). A turn publishes that event
// from INSIDE its event loop and only then exits to `endTurn`, so answering from the flag told
// the frontend "the turn is over" and "this session is still running" in the same breath, with
// nothing later to correct it: the sidebar's spinner, the stop control and the composer's queue
// (which flushes on `agent:idle` only for a session that is not running) stayed stuck behind a
// turn that had already finished — the intermittent full-suite failure in `rewind` and
// `transcript-fold`. The state is set BEFORE the event is emitted (`setSessionState` runs while
// handling that same event), so this answer can never contradict what the frontend was just
// told. The three statuses are the same ones the frontend counts as busy.
func (s *AgentService) RunningSessions() []string {
	s.desk.mu.Lock()
	defer s.desk.mu.Unlock()
	var out []string
	for id, r := range s.desk.runs {
		switch r.state.Status {
		case StatusThinking, StatusToolRunning, StatusBusy:
			out = append(out, id)
		}
	}
	return out
}

type desktopApp struct {
	app    *application.App
	tray   *application.SystemTray
	window *application.WebviewWindow
	// notify posts native notifications for turns that finish (or questions that
	// appear) while the window is unfocused. Set once during startup, before any
	// turn can run (see newNotifier).
	notify *notifier

	// sm is a stable session manager (no bound current) used to list on-disk
	// sessions. It is created once in initAgent and is never repointed; the
	// "displayed" session is tracked separately via activeID (guarded by mu).
	sm  *session.Manager
	mcp *mcp.Manager // shared MCP manager (nil = no MCP configured)
	cfg *config.Config

	// store is the ONE session FileStore this app reads and writes through: sm and
	// every per-run manager are built on it (see sessionStore). Sharing it is not
	// an optimization of its own — it is what keeps the store's memoized session
	// list honest, because a turn appends through ITS manager and that write has to
	// drop the snapshot the sidebar's next read would otherwise be served from.
	//
	// Guarded by storeMu, not mu: it is created lazily (a turn can be the first
	// thing to need it) and must never wait on the app-wide lock.
	storeMu sync.Mutex
	store   *session.FileStore

	// promptCache memoizes built system prompts, keyed by the exact build inputs
	// (working directory + session ID) — see systemPromptFor. Guarded by
	// promptMu, NOT by mu: the build is slow (it probes git) and must never run
	// under the app-wide lock.
	promptMu    sync.Mutex
	promptCache map[promptKey]string

	// activeID is the ID of the currently displayed session ("" when none).
	// Written under mu by New/Load; read under mu everywhere except where the
	// caller already holds mu.
	activeID string

	mu    sync.Mutex
	runs  map[string]*sessionRun // key: session ID
	simCh chan struct{}          // simulated-turn stop signal

	// perms holds the bash policy asks that are parked on a user decision, keyed
	// by session + tool call (permKey). A parked turn waits on its own channel
	// until the frontend answers via AgentService.AnswerPermission. Guarded by mu.
	perms map[string]*pendingPermission

	// fileIndex backs @-file completion in the input area (one cached path
	// index per searched root).
	fileIndex *fileindex.Index

	// projects is the desktop project table (docs/2026-09-14-desktop-project-design.md):
	// the named workspaces member sessions inherit their roots from. It guards its own
	// data with its own lock and never calls back into the app, which is what makes
	// d.mu → projects.mu a safe order. Lazy: the file is read on first use.
	projects *projectTable
}

func newDesktopApp() *desktopApp {
	return &desktopApp{
		runs:      make(map[string]*sessionRun),
		simCh:     nil,
		fileIndex: newFileIndex(),
		projects:  &projectTable{},
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

// runOf returns a session's run WITHOUT creating one: nil means nothing has been
// prepared for that id yet. getRun is for callers that are about to put something in the
// run; a lookup that only asks "is there one?" must not leave an empty entry behind
// (the guards that follow `getRun` were dead code precisely because it never returns
// nil). Callers must NOT hold d.mu.
func (d *desktopApp) runOf(id string) *sessionRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runs[id]
}

// refuseNoSession is the single refusal for "this needs a session with a live agent and
// there is none". The desktop UI is Chinese and shows these strings as they are (the
// mode selector renders one verbatim), so it is written once — the three English
// variants it replaces ("no current session", "no current session agent", "agent not
// ready") were leaking into a Chinese window.
const refuseNoSession = "没有活跃会话"

// refuseTurnRunning is SendMessage's refusal for a session whose turn is still in flight. It is
// rendered in the bubble the composer had already drawn for the message (see composer.tsx), so it
// has to read as "your message did not go out" rather than as a backend error — the reader is
// looking at what they typed, and the whole point of the refusal is that it is not a lost message.
const refuseTurnRunning = "会话正在运行，这条消息没有发出去；可以先停止这一轮再发送"

// refuseDeleteRunning is DeleteSession's refusal for a session whose turn is still in
// flight. The frontend renders it in the confirmation box that raised the delete, so it
// has to name the way out rather than only the refusal.
const refuseDeleteRunning = "会话正在运行，请先停止这一轮再删除"

// refuseDeleteProjectRunning is DeleteProject's version of the same refusal: a project
// delete DETACHES every member (design §6.2), which rewrites their meta, so it waits for
// the same thing a session delete waits for. Same sentence shape on purpose — the user is
// being asked to do the same thing.
const refuseDeleteProjectRunning = "有会话正在运行，请先停止后再删除项目"

// agentOf is runOf plus the readiness rule the UI bindings share: the session exists AND
// its agent has been built. The second return is the refusal to hand back (empty when
// there is an agent). Callers must NOT hold d.mu.
func (d *desktopApp) agentOf(id string) (*agent.AIAgent, string) {
	r := d.runOf(id)
	if r == nil || r.agent == nil {
		return nil, refuseNoSession
	}
	return r.agent, ""
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
		a, err = d.buildAgentForSession(ctx, id, sm)
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
		// Honour the mode the session was left in. meta.json is the record (an editor or
		// an earlier desktop turn may have written it), so starting in auto regardless
		// would make the recorded mode and the live one disagree — and the system prompt
		// would drop the plan-mode rules the session is supposed to be running under.
		if sess.Mode != "" && sess.Mode != agent.ModeAuto {
			_ = a.SetMode(sess.Mode)
		}
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
