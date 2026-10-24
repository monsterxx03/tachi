package main

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/fileindex"
	"github.com/monsterxx03/tachi/session"
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

// busy reports whether a status means "a turn is in flight here". It is the ONE definition of
// that question: RunningSessions (which the frontend's spinner, stop control and composer queue
// all read) and the menu bar's count of running turns both ask it, and the frontend counts these
// same three statuses as busy.
func (s AgentStatus) busy() bool {
	switch s {
	case StatusThinking, StatusToolRunning, StatusBusy:
		return true
	}
	return false
}

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
	// Waiting is the sentence a session parked on the user carries — "" when nothing is
	// being asked of them. It is FILLED AT EMISSION from the ask table (waitingSentence in
	// asks.go), not passed in by a caller: the park is registered by the tool's own
	// goroutine while tool events travel the event channel, so a state that carried its own
	// sentence could be overwritten — out of order — by a ToolCallArgs event for the very
	// call that is parked.
	//
	// A sentence rather than a flag because the sidebar's marker has to answer "what am I
	// needed for?", and that wording belongs where the wait is created, next to Label and
	// Detail.
	Waiting string `json:"waiting"`
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
//
// A run that has compacted answers with BOTH ids: the key it is filed under (the turn's stream
// is still landing in that session's transcript, which is the row the reader is watching) and the
// child the conversation has moved into. Both, because the sidebar renders whichever of the two
// its own session list happens to hold — nothing refreshes that list mid-turn, so the child's row
// may not exist yet, and reporting the child ALONE would leave the only row on screen unmarked
// while a turn is running. They are two rows of ONE conversation (the child's row carries the
// parent as 「压缩前」, collapsed), which is why this is a marker on a chain and not "two sessions
// running".
func (s *AgentService) RunningSessions() []string {
	s.desk.mu.Lock()
	defer s.desk.mu.Unlock()
	var out []string
	for id, r := range s.desk.runs {
		if !r.state.Status.busy() {
			continue
		}
		out = append(out, id)
		if r.compaction != nil && r.compaction.SessionID != "" {
			out = append(out, r.compaction.SessionID)
		}
	}
	return out
}

type desktopApp struct {
	// notify posts native notifications about a conversation the user is not looking at. It is an
	// interface so a process with no notification centre can answer every call with silence; see
	// notify_host.go. Installed by main.go in the windowed build, noNotifier in the headless one.
	notify notifierHost

	// windowHost carries the window-facing fields (app, tray, window, quitting, plus the device
	// link and the microphone). It is empty in a headless build, which is how the runtime below
	// compiles without a window at all — see ui_state_ui.go / ui_state_headless.go.
	windowHost

	// sm is a stable session manager (no bound current) used to list on-disk
	// sessions. It is created once in initAgent and is never repointed; the
	// "displayed" session is tracked separately via activeID (guarded by mu).
	sm  *session.Manager
	mcp *mcp.Manager // shared MCP manager (nil = no MCP configured)

	// cfg is the process-wide configuration. Read it through config() and change
	// it through withConfig(); see those for the two rules that make it safe.
	//
	// Guarded by cfgMu, NOT by mu: reads happen on turn goroutines (a turn reads
	// MaxTokens as it starts) and must never wait behind the app-wide lock.
	cfgMu sync.RWMutex
	cfg   *config.Config

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

	// delegator is this machine's sending side (peers + the delegated-task records), kept in
	// step with the peers config leaf. Guarded by delegatorMu, NOT by mu, for the same reason
	// cfgMu and storeMu are: a turn reads it as it starts, and it must never wait behind the
	// app-wide lock.
	delegatorMu sync.Mutex
	delegator   *node.DelegatorCache

	// peers is the '#' picker's node list (which nodes this machine may delegate to, and what the
	// last look found). It is kept across calls because the probe's results and its rate limit live
	// in it; its peer list and sender are read through providers, so it follows the config.
	// Guarded by peersMu, NOT by mu, for the same reason delegatorMu is.
	peersMu sync.Mutex
	peers   *node.PeerList

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

	// mobile is the LAN interface the phone app connects to (mobile.go): the same
	// conversations, a third screen. nil = the interface is off. It is replaced by
	// startMobile/stopMobile and read by every event path that asks "is anyone listening?".
	// Guarded by mu.
	mobile *mobileServer

	mu   sync.Mutex
	runs map[string]*sessionRun // key: session ID
	// trayCost memoizes today's spend for the menu bar (see todayCost): refreshTray runs on every
	// state change, and the ledger read is a file scan.
	trayCost trayCostCache
	simCh    chan struct{} // simulated-turn stop signal

	// asks holds every "a turn is parked on the user" record, keyed by session +
	// tool call (permKey): a bash policy ask and an AskUserQuestion alike. It is
	// both what the frontend answers through and what the device link reads (see
	// asks.go for why it is ONE table). Guarded by mu.
	asks map[string]*pendingAsk

	// settingsMu serializes SaveSettings end to end (snapshot → memory → file → pushes).
	// Two saves landing at once — a double-clicked button, two windows — would otherwise
	// interleave their snapshots and pushes: the second one's rollback would restore a
	// snapshot taken around the first one's writes.
	settingsMu sync.Mutex

	// mcpFileMu serializes every read-modify-write of an mcp.json. Three paths write that file —
	// the settings editor (upsert/delete an entry), the panel's toggle (one "enabled" flag) and a
	// profile switch through the editor's own re-read — and each of them reads the whole file,
	// changes it and writes it back. Atomic writes keep the FILE intact without this, but not the
	// CHANGE: two of them landing together would let the second one's read predate the first one's
	// write, and that edit would disappear with nothing to report it. It is NOT cfgMu: the lock
	// covers file I/O, and callers below take cfgMu only for the config swap that follows.
	mcpFileMu sync.Mutex

	// pushing, deferredThisSave and savingUpdates belong to the saving goroutine only (settingsMu
	// serializes saves): `pushing` marks the push step, which is what makes the fan-outs queue a
	// push for a session that is mid-turn instead of applying it; deferredThisSave collects those
	// sessions for the summary; and savingUpdates is the batch being pushed, which a row that OWNS
	// the write of its leaf (settingApply.OwnsWrite) reads its new value from. See
	// deferred_push.go for the whole rule.
	pushing          bool
	deferredThisSave []string
	savingUpdates    map[string]any

	// pushMu guards pendingPushes: the pushes a save deferred, per session, waiting for that
	// session's turn to end (drained by endTurn).
	pushMu        sync.Mutex
	pendingPushes map[string][]deferredPush

	// fileIndex backs @-file completion in the input area (one cached path
	// index per searched root).
	fileIndex *fileindex.Index

	// cron is the in-process scheduler for desktop jobs. It is created after the
	// config is loaded and stopped on shutdown; nil means cron is disabled or its
	// store could not be opened. Guarded by cronMu, NOT by mu: building it reads
	// the config, and a run must never wait behind the app-wide lock for that.
	cronMu sync.Mutex
	cron   *cron.Scheduler
	// cronRunning is the set of job IDs whose handler is in flight right now. It
	// is what the task list shows as 「运行中」; the scheduler's own semaphore is
	// not observable from a binding. Guarded by mu.
	cronRunning map[string]bool

	// dream is the background memory consolidation pass (desktop/dream.go). It is
	// created after the config is loaded and stopped on shutdown; dreamBusy is the
	// app-level gate shared by the timer and a typed /dream, so two passes can
	// never rewrite the same memory directory at once. A pass runs for minutes, so
	// dreamMu is NOT mu: nothing here may wait behind the app-wide lock.
	dreamMu     sync.Mutex
	dreamCancel context.CancelFunc
	dreamDone   chan struct{}
	dreamBusy   bool

	// projects is the desktop project table (docs/2026-09-14-desktop-project-design.md):
	// the named workspaces member sessions inherit their roots from. It guards its own
	// data with its own lock and never calls back into the app, which is what makes
	// d.mu → projects.mu a safe order. Lazy: the file is read on first use.
	projects *projectTable

	// mcpAuth is the desktop's per-server OAuth bookkeeping, guarded by mcpAuthMu (NOT
	// mu: it is written from a background authorization goroutine, and nothing here may
	// wait on the app-wide lock). It is deliberately not part of MCPServerConfig — that
	// struct is the user's file, rewritten by profile switches and DCR refreshes, while
	// "this server asked for authorization and has not got one yet" is a fact about THIS
	// process's connection attempts.
	//
	//   - needed: a connect attempt came back OAuthRequiredError and no token has worked
	//     since. Drives the panel's 需要授权 badge and its 授权 button.
	//   - inFlight: an authorization is running. Two would race the same callback port
	//     and the same token file.
	//   - cancels: the running flow's cancel, so a pasted redirect URL can end it first
	//     (both ways out exchange the same single-use authorization code).
	mcpAuthMu       sync.Mutex
	mcpAuthNeeded   map[string]bool
	mcpAuthInFlight map[string]bool
	mcpAuthCancels  map[string]context.CancelFunc
}

func newDesktopApp() *desktopApp {
	return &desktopApp{
		windowHost:   newWindowHost(),
		notify:       newNotifierHost(),
		runs:        make(map[string]*sessionRun),
		simCh:       nil,
		fileIndex:   newFileIndex(),
		projects:    &projectTable{},
		cronRunning: make(map[string]bool),
	}
}

// ---------------------------------------------------------------------------
// Configuration access (cfgMu)
// ---------------------------------------------------------------------------
//
// The config is read concurrently — a turn reads MaxTokens and Language as it
// starts, a panel reads MCPServers as it renders, every agent holds the pointer
// as its FullConfig — so it has one lock, one reader and one writer.

// config returns the process-wide configuration, or nil when bootstrap failed
// (the app then runs agent-less and browses on-disk sessions only).
//
// Callers must NOT hold the returned pointer across an await: another goroutine
// may be applying a settings change. Reading the fields it needs during one
// synchronous operation is what every current caller does.
func (d *desktopApp) config() *config.Config {
	d.cfgMu.RLock()
	defer d.cfgMu.RUnlock()
	return d.cfg
}

// withConfig applies fn to the live configuration under the write lock.
//
// ⚠️ fn MUST MUTATE THE EXISTING OBJECT — never assign d.cfg = something else.
// Replacing the pointer looks cleaner and is silently WRONG here, because the
// pointer is captured by things that outlive this call:
//
//   - every provider is wrapped by wrapForUsage, whose price/credit resolvers
//     CLOSE OVER the *config.Config and dereference it at each LLM call, so a
//     replaced object leaves the usage ledger billing at the old prices;
//   - each session's agent holds it as AgentConfig.FullConfig (26 read sites);
//   - the shared MCP manager and its background refresher hold it too.
//
// Those holders resolve the fields LAZILY, which is what makes an in-place edit
// take effect everywhere at once — and what makes a pointer swap fail
// everywhere at once, with no error anywhere to notice.
//
// fn runs under the write lock, so it must be quick and must not call back into
// anything that reads the config (that would deadlock on cfgMu). One settings
// save is one field: see settings.go.
func (d *desktopApp) withConfig(fn func(*config.Config)) {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	if d.cfg != nil {
		fn(d.cfg)
	}
}

// setConfig installs the bootstrap result. Called once from initAgent, before
// any turn can exist; it takes the write lock like any other writer so the
// rules above hold for this write too.
func (d *desktopApp) setConfig(cfg *config.Config) {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	d.cfg = cfg
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
	if r := d.ownerOfLocked(id); r != nil {
		return r
	}
	r := &sessionRun{state: AgentState{Status: StatusIdle, Label: "空闲", Detail: "就绪"}}
	d.runs[id] = r
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
	return d.ownerOfLocked(id)
}

// ownerOfLocked is the ONE answer to "which run is this session's?" — the exit getRun and
// runOf both resolve through. Callers must hold d.mu.
//
// It answers nil for a session nothing has prepared. The one alias it resolves is the CHILD
// a compaction created while the turn that compacted is still in flight: the agent loop has
// already moved the conversation into that child (agent.FinalizeCompact) and keeps appending
// to it, while the run stays keyed to the parent until the turn settles (followCompaction).
// Without the alias the child is an ORDINARY session to every reader — it resolves to no run
// (so RunningSessions does not report it, and prepareSession would build a SECOND agent for
// one conversation), and it is not busy (so a send starts a second turn whose records
// interleave with the first one's in a single session file, under two spinning sidebar rows).
//
// The alias is read off the run's own pending record rather than kept in a table of its own —
// one fact, one place, and it is what makes the alias need no clearing: followCompaction
// consumes r.compaction, and the alias goes with it.
func (d *desktopApp) ownerOfLocked(id string) *sessionRun {
	if r := d.runs[id]; r != nil {
		return r
	}
	for _, r := range d.runs {
		if r.compaction != nil && r.compaction.SessionID == id {
			return r
		}
	}
	return nil
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

// liveRun pairs a session with its built agent. The session identity is the point: a bare
// []*agent.AIAgent drops it, and "2 个会话失败" without saying which ones is not something a
// reader can act on. Running carries the session's busy flag as it was at SNAPSHOT time, which is
// what a settings push consults before touching that session's agent (see deferred_push.go).
type liveRun struct {
	ID      string
	Agent   *agent.AIAgent
	Running bool
}

// liveRuns snapshots every session that has a built agent, sorted by ID so a failure list
// reads the same way twice (map order is random).
//
// It exists for facts about the SHARED layers — the MCP pool and connections, the config
// object, the system-prompt cache are one instance for the whole app — so enabling a server
// or changing a setting is not a per-session event: a session sitting in the background has
// to learn about it too. It is deliberately a snapshot (the lock is released before
// returning) because the callers then invoke per-agent code, which takes each agent's own
// locks; holding d.mu across that would put the app-wide lock under a per-agent one, the
// same inversion the rest of this file avoids. A run created after the snapshot is not
// missed in practice: its agent sees the new state as it is built.
//
// Callers must NOT hold d.mu.
func (d *desktopApp) liveRuns() []liveRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]liveRun, 0, len(d.runs))
	for id, r := range d.runs {
		if r != nil && r.agent != nil {
			out = append(out, liveRun{ID: id, Agent: r.agent, Running: r.running})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// applyToLiveRuns runs fn for every session that has a built agent, and returns the failures as
// user-facing lines ("「标题」：原因") — nil when every session took it. It is the primitive;
// applyToLiveAgents is the same thing for a caller that does not need the session itself.
//
// A row needs the session (not just its agent) whenever the per-session facts around it matter:
// a permission policy is compiled against the session's own tree, a provider re-resolve has to
// re-apply that session's thinking override.
//
// During a settings save's push step (`pushing`), a session whose turn is in flight is SKIPPED
// and the push is queued for its turn end instead of being applied under a running turn — see
// deferred_push.go for why, and for what deliberately does not defer.
func (d *desktopApp) applyToLiveRuns(fn func(liveRun) error) []string {
	var failures []string
	for _, r := range d.liveRuns() {
		if d.pushing && r.Running {
			d.deferPush(r.ID, fn)
			continue
		}
		err := fn(r)
		if err == nil {
			continue
		}
		label := d.sessionTitle(r.ID)
		if label == "" {
			label = r.ID
		}
		failures = append(failures, fmt.Sprintf("「%s」%v", label, err))
	}
	return failures
}

// applyToLiveAgents pushes a change into every session that has a built agent, and returns
// the failures as user-facing lines ("「标题」：原因") — nil when every session took it.
//
// It is the fan-out a config change needs, and the reason is the same one the MCP toggle
// has: `d.cfg` is ONE object for the whole process while an AIAgent is one PER SESSION, so
// any change that is more than "mutate the config object" has to reach each of them — a
// session sitting in the background would otherwise keep the old behaviour. `fn` runs
// outside d.mu, so a row may call back into the app.
//
// The label is the session's title when it has one, its id otherwise: the strings end up in
// the post-save summary, which has no session list of its own to point at.
func (d *desktopApp) applyToLiveAgents(fn func(*agent.AIAgent) error) []string {
	return d.applyToLiveRuns(func(r liveRun) error { return fn(r.Agent) })
}

// appAgentForMCP picks the agent that drives an operation on a SHARED layer: the displayed
// session's when it has one, else any live session's, else none with the reason to report.
//
// The MCP manager (and its connections, its refresher and its pool) is one per process, so the
// choice only decides whose callback and whose registry the operation lands on — and the session
// the user is looking at is the one whose view they expect to change. It returns an error string
// rather than an error because every caller turns it into a line of the post-save summary.
func (d *desktopApp) appAgentForMCP() (*agent.AIAgent, string) {
	if a, refuse := d.agentOf(d.currentID()); refuse == "" && a != nil {
		return a, ""
	}
	if runs := d.liveRuns(); len(runs) > 0 {
		return runs[0].Agent, ""
	}
	return nil, "没有已打开的会话，这一项要等下次开对话时才生效"
}

// sessionThinkingLevel is a session's stored thinking override ("" = follow the provider's own
// default). It lives here rather than inline because three callers ask the same question —
// /model's SwitchProvider, /thinking, and the settings reload that re-resolves a provider.
func (d *desktopApp) sessionThinkingLevel(id string) string {
	r := d.runOf(id)
	if r == nil || r.sm == nil {
		return ""
	}
	if cur := r.sm.Current(); cur != nil {
		return cur.ThinkingLevel
	}
	return ""
}

// activeRun returns the run for the currently displayed session (nil when no
// session is active). It is the per-session source of truth for provider/agent
// reading in the UI. It resolves through ownerOfLocked, so the displayed session being the
// child of a compaction still in flight reads the run that is producing into it.
func (d *desktopApp) activeRun() *sessionRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ownerOfLocked(d.activeID)
}

// activeSessionID returns the id of the session the window is showing ("" when none is
// loaded). Read side of d.activeID, which is guarded by mu.
func (d *desktopApp) activeSessionID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.activeID
}

// prepareSession lazily builds (or returns) the per-session agent + manager for
// id, mirroring channel's prepareThreadSession. Each session owns its own
// AIAgent and Manager, so concurrent turns never share agent turn state.
// Callers must NOT hold d.mu.
//
// It resolves through getRun, so opening the child of a compaction that is still in flight
// returns the run that is producing into it — one agent per conversation, not one per link of
// the chain. Its manager's Current() is that child already, which is what makes the loaded
// history and the file it reads agree.
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
	if d.config() != nil {
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
	// This session's tree has just joined the set a roots-aware MCP server is told about
	// (mcpRoots answers with every live session's workspace). Only here: the early return above
	// means a session that was already prepared does not re-announce itself on every switch.
	d.notifyMCPRootsChanged()
	return r, nil
}

