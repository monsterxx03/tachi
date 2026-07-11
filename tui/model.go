package tui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/dream"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
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
	provider      llm.Provider
	providerInfo  string
	contextWindow int64
}

// switchProviderMsg is sent by compactForModelSwitch's no-compaction early
// return path. Since tea.Cmd closures run in a separate goroutine, they
// MUST NOT mutate Model fields directly — instead they return a message
// that the Update function handles synchronously.
type switchProviderMsg struct {
	provider      llm.Provider
	providerInfo  string
	contextWindow int64
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

	state          state
	thinkingMode   bool
	thinkingView   ThinkingView
	copyMode       bool
	cancelFunc     context.CancelFunc
	eventCh        <-chan agent.AgentEvent
	steerRespCh    chan string // agent → TUI: steer check requests use this to get pending input
	totalUsage     llm.Usage
	sessionCost    float64
	pendingConfirm *pendingConfirm
	askUserView    *AskUserView

	savedHistory  []llm.Message         // conversation history saved before a one-off run (e.g. /commit)
	savedTools    map[string]tools.Tool // tool registry saved before a one-off run (e.g. /commit)
	isCompacting  bool                  // true during compact LLM call (distinct from savedHistory)
	isResearching bool                  // true during deep research (blocks user input)
	forkedAgent   *agent.ForkedAgent    // active forked agent (e.g. /review), closed on TurnComplete/error

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

	logger *debuglog.Logger
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
		input:            NewInputArea(inputHistoryMax(cfg.Config), inputHistoryFilePath()),
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
		m.refreshSessionCost()
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

// modeCycle returns the next mode in the rotation: auto → plan → chat → auto.
func modeCycle(current string) string {
	switch current {
	case agent.ModeAuto:
		return agent.ModePlan
	case agent.ModePlan:
		return agent.ModeChat
	case agent.ModeChat:
		return agent.ModeAuto
	default:
		return agent.ModeAuto
	}
}

// modeDisplayName returns a human-readable name for a mode.
func modeDisplayName(mode string) string {
	switch mode {
	case agent.ModeAuto:
		return "Auto"
	case agent.ModePlan:
		return "Plan"
	case agent.ModeChat:
		return "Chat"
	default:
		return mode
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
		m.logger.Log("TUI: failed to switch mode to %s: %v", next, err)
		return
	}

	// Update UI.
	m.rebuildSystemPrompt()
	m.statusbar.SetMode(next)
}

// modeDescription returns a short description for a mode.
func modeDescription(mode string) string {
	switch mode {
	case agent.ModeAuto:
		return "完整工具权限：可编辑文件、运行命令、浏览网页等"
	case agent.ModePlan:
		return "只读规划模式：仅允许探索代码和保存计划"
	case agent.ModeChat:
		return "只读对话模式：仅允许搜索、浏览和提问"
	default:
		return ""
	}
}

// persistMode writes the current session mode to the session's metadata on disk.
func (m *Model) persistMode(mode string) {
	sm := m.agent.SessionManager()
	if sm == nil {
		return
	}
	curr := sm.Current()
	if curr == nil {
		return
	}
	curr.Mode = mode
	if err := sm.UpdateMeta(curr); err != nil {
		m.logger.Log("TUI: failed to persist mode %s: %v", mode, err)
	}
}

// resolveModelPrice resolves the effective pricing for the current model.
// Checks provider config overrides first, then falls back to built-in pricing.
func (m *Model) resolveModelPrice() *llm.ModelPrice {
	model := m.agent.Model()

	// Check for provider-level price overrides
	if m.cfg != nil && m.cfg.Provider != "" {
		pCfg := m.cfg.FindProvider(m.cfg.Provider)
		if pCfg != nil {
			return llm.ResolveModelPrice(
				model,
				pCfg.InputPrice,
				pCfg.OutputPrice,
				pCfg.CacheReadInputPrice,
				pCfg.CacheCreationInputPrice,
			)
		}
	}

	return llm.ResolveModelPrice(model, nil, nil, nil, nil)
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
	m.refreshSessionCost()
}

// refreshSessionCost recalculates the total session cost from stored messages
// (including subagent messages) and updates the statusbar.
func (m *Model) refreshSessionCost() {
	sm := m.agent.SessionManager()
	if sm == nil {
		return
	}
	price := m.resolveModelPrice()
	if price == nil {
		m.sessionCost = 0
		m.statusbar.SetCost(0)
		return
	}

	// Recalculate from cumulative main conversation usage (tracked in totalUsage)
	cost := llm.CalculateCost(&m.totalUsage, price)

	// Add subagent costs by scanning subagent JSONL files
	if curr := sm.Current(); curr != nil {
		subMsgs, err := sm.LoadSubagentMessages(curr.ID)
		if err == nil {
			for _, msgs := range subMsgs {
				for _, msg := range msgs {
					if msg.Usage != nil {
						usage := &llm.Usage{
							InputTokens:              msg.Usage.InputTokens,
							OutputTokens:             msg.Usage.OutputTokens,
							CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
							CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
						}
						cost += llm.CalculateCost(usage, price)
					}
				}
			}
		}
	}

	m.sessionCost = cost
	m.statusbar.SetCost(cost)
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
		m.logger.Log("TUI: MCP background init completed")
	}
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
	names := make([]string, len(metas))
	descs := make(map[string]string, len(metas))
	for i, meta := range metas {
		names[i] = meta.Name
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
			provider:      msg.provider,
			providerInfo:  msg.providerInfo,
			contextWindow: msg.contextWindow,
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
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "⏹️ 深度研究已取消",
		})
		return m, nil
	}
	if m.state != stateIdle && m.cancelFunc != nil {
		m.cancelFunc()
		m.pendingQueue = nil
		m.chatview.RemovePendingItems()
		m.statusbar.SetPendingCount(0)
		// Immediate visual feedback: mark in-progress tool calls as interrupted.
		m.chatview.MarkPendingToolsInterrupted()
		return m, nil
	}
	if m.agent != nil {
		m.agent.StoreSessionMemory()
	}
	return m, tea.Quit
}

func (m *Model) handleKeyConfirmation(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		m.agent.ConfirmTool(true)
	case "n", "N", "esc":
		m.agent.ConfirmTool(false)
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
	b.WriteString(confirmStyle.Render("Apply this edit? [y/n]: "))
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
	if m.state == stateSelectingModel {
		inputSection = m.renderProviderSelection()
	} else if m.state == stateSelectingSession {
		inputSection = m.renderSessionSelection()
	} else if m.state == stateAwaitingConfirmation {
		inputSection = m.renderConfirmPrompt()
	} else if m.state == stateAskUserQuestion {
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
