package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/dream"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/session"
)

type state int

const (
	stateIdle state = iota
	stateWaiting
	stateStreaming
	stateSelectingModel
	stateAwaitingConfirmation
	stateAskUserQuestion
	stateSelectingSession
	stateManagingMCP
)

type toolCallDisplay struct {
	Name              string
	ID                string
	Args              string
	Preview           string
	Result            string
	IsError           bool
	IsSubagent        bool
	Done              bool
	Duration          time.Duration
	IterCount         int            // sub-agent iteration count
	SubagentToolCalls map[string]int // real-time subagent internal tool call counts
}

type pendingConfirm struct {
	toolName string
	toolID   string
	toolArgs string
	diff     string
}

type chatMessage struct {
	Role    string
	Content string
}

// pendingSwitchProvider stores provider switch info when switching to a
// smaller-context model that needs compaction first. The switch is deferred
// until after compaction completes, so the old (wider-context) provider can
// be used for the LLM summarization call.
type pendingSwitchProvider struct {
	provider       llm.Provider
	providerName   string // config provider name for session metadata
	providerInfo   string
	contextWindow  int64
	thinking       *bool  // target provider's thinking switch (nil = default)
	thinkingEffort string // target provider's thinking effort (normalized)
}

// reviewOrch tracks an in-flight /review run (single or multi-round).
// Non-nil reviewOrch + isReviewing=true means the orchestrator is between
// round 1 and the final round's TurnComplete. The orchestrator owns the
// report directory, every round's exact output path (written into the prompt
// before the round starts, verified with os.Stat after it ends) and the
// round/providers/reports bookkeeping — the TUI only drives Next()/Complete()
// and renders. See agent/commands/review.go.

// switchProviderMsg is sent by compactForModelSwitch's no-compaction early
// return path. Since tea.Cmd closures run in a separate goroutine, they
// MUST NOT mutate Model fields directly — instead they return a message
// that the Update function handles synchronously.
type switchProviderMsg struct {
	provider       llm.Provider
	providerName   string // config provider name for session metadata
	providerInfo   string
	contextWindow  int64
	thinking       *bool  // target provider's thinking switch (nil = default)
	thinkingEffort string // target provider's thinking effort (normalized)
}

type Model struct {
	statusbar StatusBar
	chatview  ChatView
	input     InputArea

	width  int
	height int

	agent        *agent.AIAgent
	systemPrompt string // effective system prompt (base + mode supplement)
	chatOpts     llm.ChatOptions
	history      []llm.Message

	baseSystemPrompt string // base prompt without mode supplement, used to rebuild systemPrompt

	state        state
	thinkingMode bool
	thinkingView ThinkingView
	copyMode     bool
	cancelFunc   context.CancelFunc
	eventCh      <-chan agent.AgentEvent
	steerCh      chan agent.SteerInput // agent → TUI: steer check requests read pending input from here
	// steerCh 无需在 TurnComplete/Error 时置 nil：事件顺序处理，旧 turn 的
	// loop 退出后不会再有 SteerCheck；steer 发送是 select+default 非阻塞，
	// 新 turn 会在 sendMessage 里重建 channel。
	totalUsage     llm.Usage
	pendingConfirm *pendingConfirm
	askUserView    *AskUserView

	savedHistory  []llm.Message            // conversation history saved before a one-off run (e.g. /commit)
	isCompacting  bool                     // true during compact LLM call (distinct from savedHistory)
	isResearching bool                     // true during deep research (blocks user input)
	isReviewing   bool                     // true during a review run (single or multi-round; blocks user input)
	reviewOrch    *cmds.ReviewOrchestrator // non-nil while a review is running (see reviewOrch doc above)
	forkedAgent   *agent.ForkedAgent       // active forked agent (e.g. /review), closed on TurnComplete/error

	pendingQueue []string // messages queued during streaming for auto-send on TurnComplete
	streamGen    int      // incremented on each new stream; used to ignore stale events

	// pendingSwitchProvider stores provider switch info when switching to a
	// smaller-context model that needs compaction first. When non-nil, the
	// switch is deferred until after the compaction completes.
	pendingSwitchProvider *pendingSwitchProvider
	// compactForSwitch is true when a compaction was triggered by an
	// auto-switch-to-smaller-model flow (rather than the /compact command).
	compactForSwitch bool

	cfg            *config.Config
	providerItems  []config.ProviderConfig
	providerSelIdx int

	mcpManager      *mcp.Manager
	mcpServers      []config.MCPServerConfig
	mcpView         MCPView // overlay for /mcp management
	subcommandInput string  // raw input text for subcommand parsing (e.g. "/mcp list")

	sessionList      []*session.Session // for /sessions selection
	sessionSelIdx    int
	sessionScrollOff int // scroll offset for session list

	notifyOnComplete bool // whether to send terminal notification on turn complete

	mcpReady bool // whether MCP background init has completed

	dreamOrch *dream.Orchestrator // active dream orchestrator (nil when idle)

	logger *logger.Logger
}

// MCPReadyMsg is sent to the TUI when MCP background initialization completes.
type MCPReadyMsg struct{}

type ModelConfig struct {
	Agent              *agent.AIAgent
	SystemPrompt       string
	ChatOpts           llm.ChatOptions
	ProviderInfo       string
	Config             *config.Config
	ContextWindow      int64
	InitialHistory     []llm.Message
	InitialSessionMsgs []session.Message
	InitialSessionList []*session.Session // for --resume: show session selection UI on startup
	MCPManager         *mcp.Manager
	MCPServers         []config.MCPServerConfig
}

func NewModel(cfg ModelConfig) *Model {
	m := &Model{
		statusbar:        NewStatusBar(cfg.ProviderInfo, cfg.ContextWindow),
		chatview:         NewChatView(),
		input:            NewInputArea(inputHistoryMax(cfg.Config), inputHistoryFilePath(), cfg.Agent.Logger()),
		agent:            cfg.Agent,
		systemPrompt:     cfg.SystemPrompt,
		baseSystemPrompt: cfg.SystemPrompt,
		chatOpts:         cfg.ChatOpts,
		state:            stateIdle,
		cfg:              cfg.Config,
		mcpManager:       cfg.MCPManager,
		mcpServers:       cfg.MCPServers,
		thinkingView:     NewThinkingView(),
		notifyOnComplete: cfg.Config.TUI.NotifyEnabled(),
	}

	if len(cfg.InitialHistory) > 0 {
		m.history = cfg.InitialHistory
		m.chatview.LoadHistory(cfg.InitialSessionMsgs)
		m.rebuildTotalUsage(cfg.InitialSessionMsgs)
	}

	if len(cfg.InitialSessionList) > 0 {
		m.sessionList = cfg.InitialSessionList
		m.sessionSelIdx = 0
		m.clampSessionScroll()
		m.setState(stateSelectingSession)
	} else if cfg.InitialSessionList != nil {
		// --resume with no sessions available
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No sessions found to resume.",
		})
	}

	// Sync session info to the statusbar if there's already a current session.
	m.syncSessionInfo()

	// Show the initial thinking level (provider config default until /thinking
	// or /model changes it).
	m.syncThinkingBadge()

	// Start watching for MCP background init completion (async connect).
	// This returns immediately; the actual wait happens in the background.
	if len(m.mcpServers) > 0 || m.agent.MCPReady() != nil {
		m.statusbar.SetMCPEnabled(true)
	}
	m.waitForMCP()

	m.refreshSkillCompletions()

	// Sync initial mode to statusbar.
	m.statusbar.SetMode(m.agent.Mode())

	return m
}

func (m *Model) syncSessionInfo() {
	sm := m.agent.SessionManager()
	if sm == nil {
		return
	}
	if curr := sm.Current(); curr != nil {
		m.statusbar.SetSessionInfo(curr.Title, curr.ID)
	} else {
		m.statusbar.SetSessionInfo("", "")
	}
}

// syncThinkingBadge refreshes the statusbar's thinking-level indicator.
// It shows the USER-SELECTED level (raw, not model-normalized): the per-session
// override set via /thinking, else the provider's configured thinking_level,
// else "default". The agent's Config.Thinking/ThinkingEffort hold the
// normalized effective values (e.g. "max" degrades to "high" on models that
// don't support it) — those are what's actually sent to the API, but the
// statusbar reflects what the user configured.
func (m *Model) syncThinkingBadge() {
	m.statusbar.SetThinkingLevel(m.currentThinkingLevel())
}

// currentThinkingLevel resolves the user-facing thinking level for the current
// session: the per-session override (session.ThinkingLevel, set via /thinking)
// wins; otherwise the active provider's configured thinking_level is shown;
// "default" when neither is set.
//
// Reading the RAW spec value (pCfg.Spec.ThinkingLevel) is deliberate and
// differs from the ACP side (currentThinkingValue reads the agent's resolved
// config): the statusbar reflects what the USER configured, not the resolved
// runtime value. Since client-side normalization was removed, the two agree
// for concrete levels ("max" stays "max"); they only differ for "none"/""
// which the raw field expresses directly.
func (m *Model) currentThinkingLevel() string {
	sm := m.agent.SessionManager()
	if sm != nil {
		if curr := sm.Current(); curr != nil && curr.ThinkingLevel != "" {
			return curr.ThinkingLevel
		}
	}
	// No active session — reflect a pending override set via /thinking at
	// startup (before the first message creates a session).
	if m.agent.PendingSessionThinking() != "" {
		return m.agent.PendingSessionThinking()
	}
	// Fall back to the active provider's configured thinking_level (raw).
	if m.cfg != nil {
		providerName := ""
		if sm != nil {
			if curr := sm.Current(); curr != nil && curr.ProviderName != "" {
				providerName = curr.ProviderName
			}
		}
		if providerName == "" {
			providerName = config.ResolveProviderName(m.cfg)
		}
		if pCfg := m.cfg.FindProvider(providerName); pCfg != nil && pCfg.Spec.ThinkingLevel != "" {
			return pCfg.Spec.ThinkingLevel
		}
	}
	return "default"
}

// effectiveSystemPrompt returns the system prompt including mode-specific
// supplements. In plan mode, the plan mode instructions are appended.
func (m *Model) effectiveSystemPrompt() string {
	if m.agent.Mode() == agent.ModePlan && m.baseSystemPrompt != "" {
		return m.baseSystemPrompt + "\n\n" + agent.BuildPlanModePrompt()
	}
	return m.baseSystemPrompt
}

// rebuildSystemPrompt refreshes m.systemPrompt to reflect the current mode.
// Called after switching modes so subsequent turns use the correct prompt.
func (m *Model) rebuildSystemPrompt() {
	m.systemPrompt = m.effectiveSystemPrompt()
}

// modeCycle returns the next mode in the rotation: auto → chat → auto.
// Plan mode is excluded from the TUI cycle — it's only available via ACP.
func modeCycle(current string) string {
	switch current {
	case agent.ModeAuto:
		return agent.ModeChat
	case agent.ModeChat:
		return agent.ModeAuto
	default:
		return agent.ModeAuto
	}
}

// cycleMode switches to the next session mode in the rotation and updates
// the UI: statusbar badge, system prompt, and session metadata.
func (m *Model) cycleMode() {
	current := m.agent.Mode()
	next := modeCycle(current)
	if next == current {
		return // shouldn't happen, but guard
	}

	// The agent handles tool save/restore internally.
	if err := m.agent.SetMode(next); err != nil {
		m.logger.Error(context.Background(), "TUI: failed to switch mode", err, "model", next)
		return
	}

	// Update UI.
	m.rebuildSystemPrompt()
	m.statusbar.SetMode(next)
}

// startTurn prepares the Model for a new agent stream and returns the context
// that stream must run under.
//
// Every path that assigns m.eventCh has to call this first. It does two things
// that are easy to get wrong separately:
//
//  1. Cancels any in-flight turn. Without this the previous agent goroutine can
//     sit at a steer point waiting on the steer channel that the TUI has already
//     stopped reading, while the TUI waits on the replaced eventCh — a deadlock
//     with no error and no output.
//
//  2. Bumps streamGen, so late events from the cancelled stream are recognised
//     as stale and dropped (see the msg.gen checks in Update).
//
// These were previously open-coded at seven call sites; the cancel half was
// missing from all of them until it caused exactly the hang described above.
//
// Note /research is deliberately not a caller: it owns a WithTimeout context,
// streams progress over its own channel rather than eventCh, and is guarded by
// isResearching instead of streamGen.
func (m *Model) startTurn() context.Context {
	if m.cancelFunc != nil {
		m.cancelFunc()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel
	m.streamGen++
	return ctx
}

// resolveModelPrice resolves the effective pricing for the current model.
// Checks provider config overrides first, then falls back to built-in pricing.
func (m *Model) resolveModelPrice() *llm.ModelPrice {
	providerName := ""
	if m.cfg != nil {
		providerName = m.cfg.Provider
	}
	return cmds.ResolveModelPrice(m.cfg, providerName, m.agent.Model())
}

// accumulateUsage merges an llm.Usage into totalUsage and refreshes the
// status bar and cost display. Called after each API call (tool-call rounds
// and TurnComplete) to keep the status bar in sync.
func (m *Model) accumulateUsage(u *llm.Usage) {
	if u == nil {
		return
	}
	m.totalUsage.InputTokens += u.InputTokens
	m.totalUsage.OutputTokens += u.OutputTokens
	m.totalUsage.CacheCreationInputTokens += u.CacheCreationInputTokens
	m.totalUsage.CacheReadInputTokens += u.CacheReadInputTokens
	m.statusbar.SetUsage(&m.totalUsage)
}

// waitForMCP starts a background goroutine that waits for MCP async init
// and sends an MCPReadyMsg to the TUI when complete. Does nothing if
// MCP isn't configured.
func (m *Model) waitForMCP() {
	if m.agent == nil {
		return
	}
	m.mcpReady = false

	// Use a tea.Cmd for proper lifecycle — Bubble Tea will run it in a goroutine.
	// But since we call waitForMCP from NewModel before the program starts,
	// we return a command via the Init() method instead.
}

// mcpReadyCmd returns a tea.Cmd that waits for MCP background init to complete.
func (m *Model) mcpReadyCmd() tea.Cmd {
	if m.agent == nil {
		return nil
	}
	readyCh := m.agent.MCPReady()
	return func() tea.Msg {
		<-readyCh
		return MCPReadyMsg{}
	}
}

// handleMCPReady is called when MCP background init completes.
// It updates the model state and logs completion.
func (m *Model) handleMCPReady() {
	m.mcpReady = true
	m.statusbar.SetMCPReady(true)
	if m.logger != nil {
		m.logger.Info(context.Background(), "TUI: MCP background init completed")
	}

	// Read MCP connection errors and display in status bar.
	if m.agent != nil {
		if errs := m.agent.MCPInitErrors(); len(errs) > 0 {
			msg := formatMCPInitErrors(errs)
			m.statusbar.SetMCPError(msg)
		}
	}
}

// formatMCPInitErrors formats MCP connection errors into a compact string
// suitable for the status bar. Single errors show a short summary; multiple
// errors show a count with the first server name.
func formatMCPInitErrors(errs []error) string {
	if len(errs) == 1 {
		msg := errs[0].Error()
		// Truncate to keep status bar compact
		if len(msg) > 40 {
			msg = strutil.Truncate(msg, 37)
		}
		return msg
	}
	return fmt.Sprintf("%d servers failed", len(errs))
}

// refreshSkillCompletions queries the agent's skill store and pushes skill
// names/descriptions into the input area for slash-command autocompletion.
func (m *Model) refreshSkillCompletions() {
	store := m.agent.SkillStore()
	if store == nil {
		m.input.SetSkills(nil, nil)
		return
	}
	metas := store.List()
	names := make([]string, 0, len(metas))
	descs := make(map[string]string, len(metas))
	for _, meta := range metas {
		if !meta.Enabled {
			continue // disabled skills are not activatable — no completion
		}
		names = append(names, meta.Name)
		descs[meta.Name] = meta.Description
	}
	m.input.SetSkills(names, descs)
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.mcpReadyCmd())
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil

	case MCPReadyMsg:
		m.handleMCPReady()
		return m, nil

	case tea.KeyMsg:
		// In thinking mode, route scroll keys to the thinking view
		if m.thinkingMode {
			s := msg.String()
			if s == "pgup" || s == "pgdown" || s == "ctrl+u" || s == "ctrl+d" {
				var cmd tea.Cmd
				m.thinkingView, cmd = m.thinkingView.Update(msg)
				return m, cmd
			}
		}
		return m.handleKeyMsg(msg)

	case tea.MouseWheelMsg:
		if m.thinkingMode {
			var cmd tea.Cmd
			m.thinkingView, cmd = m.thinkingView.Update(msg)
			return m, cmd
		}
		var cmd tea.Cmd
		m.chatview, cmd = m.chatview.Update(msg)
		return m, cmd

	case InputSubmitMsg:
		text := string(msg)

		// During adversarial review: block ALL input BEFORE command/skill
		// resolution. Must be the first gate — streaming-time input would
		// otherwise enter pendingQueue and get injected into the running
		// review fork at the next steer check (breaking round isolation),
		// and /skill-style inputs would slip through while state ==
		// stateWaiting (round transitions), cancelling the round via
		// startTurn in sendSkillMessage.
		if m.isReviewing {
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: "⚔️ 对抗式审查进行中，请等待完成",
			})
			return m, nil
		}

		cmd := findCommand(text)
		if cmd == nil {
			cmd = findCommandByPrefix(text)
		}

		// If no command matched but input starts with "/", try skill resolution.
		if cmd == nil && len(text) > 1 && text[0] == '/' {
			store := m.agent.SkillStore()
			if store != nil {
				// Extract skill name and extra args (e.g., "/code-review main.go")
				parts := strings.SplitN(text, " ", 2)
				skillName := strings.TrimPrefix(parts[0], "/")
				if name, found := store.ResolveCommand(skillName); found {
					extraArgs := ""
					if len(parts) > 1 {
						extraArgs = parts[1]
					}
					// During streaming: can't activate skills mid-turn
					if m.state == stateStreaming {
						m.chatview.AddMessage(chatMessage{
							Role:    "assistant",
							Content: "请等待当前回合完成后再执行命令",
						})
						return m, nil
					}
					m.subcommandInput = text
					return m, m.sendSkillMessage(name, extraArgs)
				}
			}
		}

		// During deep research: block all user input
		if m.isResearching {
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: "🔬 深度研究进行中，请等待完成后输入",
			})
			return m, nil
		}

		// During streaming: queue regular messages, allow /new, block other commands.
		if m.state == stateStreaming {
			if cmd != nil && cmd.Name == "/new" {
				// /new during streaming: clear queue, cancel current stream, reset.
				m.pendingQueue = nil
				m.chatview.RemovePendingItems()
				m.statusbar.SetPendingCount(0)
				if m.cancelFunc != nil {
					m.cancelFunc()
				}
				m.subcommandInput = text
				return m, cmd.handler(m)
			}
			if cmd != nil {
				// Other slash commands: show a hint instead of queueing.
				m.chatview.AddMessage(chatMessage{
					Role:    "assistant",
					Content: "请等待当前回合完成后再执行命令",
				})
				return m, nil
			}
			// Queue regular text for auto-send after TurnComplete.
			if text == "" {
				return m, nil
			}
			m.pendingQueue = append(m.pendingQueue, text)
			m.chatview.AddPendingItem(text)
			m.statusbar.SetPendingCount(len(m.pendingQueue))
			return m, nil
		}

		// Normal (idle) state.
		if cmd != nil {
			m.subcommandInput = text
			return m, cmd.handler(m)
		}
		return m, m.sendMessage(text)

	case agentEventMsg:
		if msg.gen != m.streamGen {
			return m, nil // stale event from a previous stream
		}
		return m, m.handleAgentEvent(msg.event)

	case tea.PasteMsg:
		if m.state == stateIdle || m.state == stateStreaming {
			oldHeight := m.input.Height()
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			if m.input.Height() != oldHeight {
				m.layout()
			}
			return m, cmd
		}

	case spinner.TickMsg:
		if m.state == stateWaiting || m.state == stateStreaming {
			return m, m.statusbar.Update(msg)
		}

	case streamDoneMsg:
		if msg.gen != m.streamGen {
			return m, nil // stale stream-close from a previous stream
		}
		if m.state != stateIdle {
			m.pendingQueue = nil
			m.chatview.RemovePendingItems()
			m.statusbar.SetPendingCount(0)
			m.chatview.FinishStreaming()
			m.setState(stateIdle)
			m.cancelFunc = nil
			m.eventCh = nil
		}

	case mcpStatusMsg:
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: msg.content,
		})
		if msg.nextCh != nil {
			return m, readNextMCPStatus(msg.nextCh)
		}

	case dreamStatusMsg:
		// Sentinel: clean up orchestrator reference without displaying.
		if msg.content == dreamDoneSentinel {
			m.dreamOrch = nil
			if msg.nextCh != nil {
				return m, readNextDreamStatus(msg.nextCh)
			}
			return m, nil
		}
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: msg.content,
		})
		if msg.nextCh != nil {
			return m, readNextDreamStatus(msg.nextCh)
		}

	case researchStatusMsg:
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: msg.content,
		})
		if msg.nextCh != nil {
			return m, readNextResearchStatus(msg.nextCh)
		}

	case researchDoneMsg:
		m.isResearching = false
		m.cancelFunc = nil

	case switchProviderMsg:
		m.pendingSwitchProvider = &pendingSwitchProvider{
			provider:       msg.provider,
			providerName:   msg.providerName,
			providerInfo:   msg.providerInfo,
			contextWindow:  msg.contextWindow,
			thinking:       msg.thinking,
			thinkingEffort: msg.thinkingEffort,
		}
		m.applyPendingSwitch()
		return m, nil

	case mcpOverlayMsg:
		if m.state == stateManagingMCP {
			m.mcpView.SetMessage(msg.content)
			m.refreshMCPServerItems()
			if msg.nextCh != nil {
				return m, readNextMCPOverlayMsg(msg.nextCh)
			}
		}
	}

	return m, nil
}

func (m *Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m.handleCtrlC()
	}
	// Ctrl+O toggles thinking-only view anytime
	if msg.String() == "ctrl+o" {
		m.thinkingMode = !m.thinkingMode
		if !m.thinkingMode {
			// Return to chat; auto-scroll
			m.chatview.userScrolled = false
			m.chatview.refresh()
		}
		return m, nil
	}
	switch m.state {
	case stateSelectingModel:
		return m.handleKeySelectingModel(msg)
	case stateSelectingSession:
		return m.handleKeySelectingSession(msg)
	case stateAwaitingConfirmation:
		return m.handleKeyConfirmation(msg)
	case stateAskUserQuestion:
		return m.handleKeyAskUser(msg)
	case stateManagingMCP:
		return m.handleKeyManagingMCP(msg)
	case stateIdle, stateStreaming:
		return m.handleKeyIdle(msg)
	}
	return m, nil
}

func (m *Model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.state == stateSelectingModel {
		m.exitModelSelect("")
		return m, nil
	}
	if m.state == stateSelectingSession {
		m.exitSessionSelect("")
		return m, nil
	}
	if m.state == stateManagingMCP {
		m.exitMCPOverlay()
		return m, nil
	}
	if m.isResearching && m.cancelFunc != nil {
		m.cancelFunc()
		m.isResearching = false
		m.cancelFunc = nil
		if m.agent != nil {
			m.agent.KillBackgroundProcesses()
		}
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "⏹️ 深度研究已取消",
		})
		return m, nil
	}
	if m.isReviewing && m.cancelFunc != nil {
		m.cancelFunc()
		// Key: do NOT restore savedHistory, do NOT setState(idle), do NOT clear
		// reviewOrch here. The cancelled stream tails an
		// AgentEventError(ExitReasonInterrupted); the error branch does the
		// full cleanup (savedHistory restore, fork close, reviewOrch clear,
		// setState(idle)). Restoring savedHistory early would let the trailing
		// event's m.history = event.Messages write the cancelled fork's
		// messages (git diff, tool calls) into the main history.
		m.pendingQueue = nil
		m.chatview.RemovePendingItems()
		m.statusbar.SetPendingCount(0)
		// Immediate visual feedback: mark in-progress tool calls as interrupted.
		m.chatview.MarkPendingToolsInterrupted()
		// Clear modals — the review fork's toolset includes Bash, which can
		// trigger a permission confirmation modal; the error branch does not
		// clear modals, so leaving them would leave the UI dirty until the
		// next ToolConfirmation.
		m.pendingConfirm = nil
		m.askUserView = nil
		// Kill background processes — the review fork shares the parent's
		// ProcessManager, so background=true processes started inside a round
		// must be terminated too.
		if m.agent != nil {
			m.agent.KillBackgroundProcesses()
		}
		// Keep reading the event channel: the trailing AgentEventError needs
		// to be picked up by nextEvent to trigger the error branch cleanup.
		if m.eventCh != nil {
			return m, m.nextEvent()
		}
		return m, nil
	}
	if m.state != stateIdle && m.cancelFunc != nil {
		m.cancelFunc()
		// Long-running commands are often started as background processes
		// (background=true → ProcessManager, which deliberately uses
		// context.Background so it survives the tool call). Ctrl+C is the
		// user's "stop everything" signal: kill them too, otherwise the
		// http server / watcher keeps running after the turn is cancelled.
		if m.agent != nil {
			m.agent.KillBackgroundProcesses()
		}
		m.pendingQueue = nil
		m.chatview.RemovePendingItems()
		m.statusbar.SetPendingCount(0)
		// Immediate visual feedback: mark in-progress tool calls as interrupted.
		m.chatview.MarkPendingToolsInterrupted()
		// Dismiss any modal waiting on the agent (confirmation prompt,
		// AskUserQuestion form) — the turn is being cancelled and the
		// agent is about to emit AgentEventError.
		m.pendingConfirm = nil
		m.askUserView = nil
		// Keep reading the event channel: after cancellation the agent
		// emits AgentEventError (or closes the channel), and we need that
		// to reach handleAgentEvent so the UI returns to stateIdle.
		// Returning nil here leaves the UI stuck in a modal (or in
		// stateStreaming) because no nextEvent cmd is queued to receive
		// the terminal event.
		if m.eventCh != nil {
			return m, m.nextEvent()
		}
		return m, nil
	}
	return m, tea.Quit
}

func (m *Model) handleKeyConfirmation(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		m.agent.ConfirmTool(agent.ConfirmAllowOnce)
	case "a", "A":
		// Always allow: for Bash policy asks, remembers the exact command
		// for this session; for other tools behaves as allow-once.
		m.agent.ConfirmTool(agent.ConfirmAllowAlways)
	case "n", "N", "esc":
		m.agent.ConfirmTool(agent.ConfirmDeny)
	default:
		return m, nil
	}
	m.pendingConfirm = nil
	m.setState(stateStreaming)
	m.layout()
	return m, m.nextEvent()
}

func (m *Model) handleKeyAskUser(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.askUserView == nil {
		m.setState(stateStreaming)
		return m, nil
	}
	// Forward scroll keys to chatview so the user can read context
	// messages that may have been pushed up by the AskUser form.
	s := msg.String()
	if s == "pgup" || s == "pgdown" || s == "ctrl+u" || s == "ctrl+d" {
		m.chatview, _ = m.chatview.Update(msg)
		return m, nil
	}
	submit, cancelled := m.askUserView.HandleKey(s)
	if cancelled {
		m.agent.RespondToAskUser(nil, nil)
	} else if submit {
		m.agent.RespondToAskUser(m.askUserView.GetAnswers(), nil)
	} else {
		return m, nil
	}
	m.askUserView = nil
	m.setState(stateStreaming)
	m.layout()
	return m, m.nextEvent()
}

func (m *Model) handleKeyIdle(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	oldHeight := m.input.Height()
	switch msg.String() {
	case "ctrl+s":
		m.copyMode = !m.copyMode
		m.statusbar.SetCopyMode(m.copyMode)
		return m, nil
	case "ctrl+m":
		return m, m.enterMCPOverlay()
	case "shift+tab":
		if m.state == stateIdle {
			m.cycleMode()
		}
		return m, nil
	case "esc":
		if m.copyMode {
			m.copyMode = false
			m.statusbar.SetCopyMode(false)
			return m, nil
		}
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		m.chatview, cmd = m.chatview.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Height() != oldHeight {
		m.layout()
	}
	return m, cmd
}

func (m *Model) layout() {
	const (
		statusHeight     = 1
		separatorsHeight = 2 // two "\n" in View() between sections
		minChatHeight    = 3
	)

	// Set widths first — Height() depends on textarea width for wrapping
	m.statusbar.SetWidth(m.width)
	m.input.SetWidth(m.width)

	inputHeight := m.input.Height()

	switch m.state {
	case stateManagingMCP:
		// Overlay uses full screen — just set its size
		m.mcpView.SetSize(m.width, m.height)
		return
	case stateSelectingModel:
		inputHeight = min(len(m.providerItems)+1, m.height/2)
	case stateSelectingSession:
		inputHeight = min(len(m.sessionList)+2, m.height/2)
	case stateAwaitingConfirmation:
		// Estimate height based on diff content (roughly 1 line per 80 chars + header/footer)
		if m.pendingConfirm != nil {
			diffLines := strings.Count(m.pendingConfirm.diff, "\n") + 1
			inputHeight = min(10+diffLines, m.height/3)
		}
	case stateAskUserQuestion:
		if m.askUserView != nil {
			inputHeight = min(m.askUserView.Height(), m.height/2)
		}
	default:
		// For regular text input, dynamically cap the textarea height so the
		// statusbar stays anchored at the bottom. Content that doesn't fit
		// scrolls internally inside the textarea.
		maxInputHeight := max(m.height-statusHeight-separatorsHeight-minChatHeight, 1)
		m.input.SetMaxHeight(maxInputHeight)
		inputHeight = m.input.Height()
	}

	chatHeight := max(m.height-inputHeight-statusHeight-separatorsHeight, minChatHeight)

	m.chatview.SetSize(m.width, chatHeight)
}

func (m *Model) setState(st state) {
	m.state = st
	m.statusbar.SetState(st)
	m.chatview.SetStreaming(st == stateStreaming)
	m.input.SetEnabled(st == stateIdle || st == stateStreaming)
}

func (m *Model) renderConfirmPrompt() string {
	var b strings.Builder
	if m.pendingConfirm != nil && m.pendingConfirm.toolName == tools.ToolNameBash {
		b.WriteString(confirmStyle.Render("Run this command? [y]once [a]lways(session) [n]deny: "))
	} else {
		b.WriteString(confirmStyle.Render("Apply this edit? [y]es [a]lways(session) [n]o: "))
	}
	return b.String()
}

func (m *Model) renderAskUserPrompt() string {
	if m.askUserView == nil {
		return ""
	}
	return m.askUserView.Render()
}

func (m *Model) View() tea.View {
	if m.width == 0 {
		return tea.NewView("Loading...")
	}

	inputSection := m.input.View()
	switch m.state {
	case stateSelectingModel:
		inputSection = m.renderProviderSelection()
	case stateSelectingSession:
		inputSection = m.renderSessionSelection()
	case stateAwaitingConfirmation:
		inputSection = m.renderConfirmPrompt()
	case stateAskUserQuestion:
		inputSection = m.renderAskUserPrompt()
	}

	var content strings.Builder

	if m.state == stateManagingMCP {
		// Overlay fills the whole screen — no input/statusbar below
		v := tea.NewView(m.mcpView.View())
		v.AltScreen = true
		return v
	}

	if m.thinkingMode {
		// Thinking-only view: replaces chat with full thinking output
		// Height matches what layout() gives to chatview (m.height - input - statusbar - 2 separators)
		m.thinkingView.SetSize(m.width, m.height-m.input.Height()-3)
		content.WriteString(m.thinkingView.ViewString())
	} else {
		content.WriteString(m.chatview.View())
	}
	content.WriteString("\n")
	content.WriteString(inputSection)
	content.WriteString("\n")
	content.WriteString(m.statusbar.View())

	v := tea.NewView(content.String())
	v.AltScreen = true
	if !m.copyMode {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

func inputHistoryMax(c *config.Config) int {
	if c == nil {
		return 10
	}
	return c.TUI.InputHistoryLimit
}

func inputHistoryFilePath() string {
	if p, err := config.InputHistoryPath(); err == nil {
		return p
	}
	return ""
}

func Run(cfg ModelConfig) error {
	m := NewModel(cfg)
	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}
