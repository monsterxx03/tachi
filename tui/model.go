package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
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
	Name       string
	ID         string
	Args       string
	Preview    string
	Result     string
	IsError    bool
	IsSubagent bool
	Done       bool
	Duration   time.Duration
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

type Model struct {
	statusbar StatusBar
	chatview  ChatView
	input     InputArea

	width  int
	height int

	agent        *agent.AIAgent
	systemPrompt string
	chatOpts     llm.ChatOptions
	history      []llm.Message

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

	savedHistory []llm.Message // conversation history saved before a one-off run (e.g. /commit)
	savedTools   map[string]tools.Tool // tool registry saved before a one-off run (e.g. /commit)
	isCompacting bool          // true during compact LLM call (distinct from savedHistory)

	pendingQueue []string // messages queued during streaming for auto-send on TurnComplete
	streamGen    int      // incremented on each new stream; used to ignore stale events

	cfg            *config.Config
	providerItems  []config.ProviderConfig
	providerSelIdx int

	mcpManager      *mcp.Manager
	mcpServers      []config.MCPServerConfig
	mcpView         MCPView // overlay for /mcp management
	subcommandInput string // raw input text for subcommand parsing (e.g. "/mcp list")

	sessionList      []*session.Session // for /sessions selection
	sessionSelIdx    int
	sessionScrollOff int // scroll offset for session list

	notifyOnComplete bool // whether to send terminal notification on turn complete

	logger *debuglog.Logger
}

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
		statusbar:   NewStatusBar(cfg.ProviderInfo, cfg.ContextWindow),
		chatview:    NewChatView(),
		input:       NewInputArea(inputHistoryMax(cfg.Config), inputHistoryFilePath()),
		agent:       cfg.Agent,
		systemPrompt: cfg.SystemPrompt,
		chatOpts:    cfg.ChatOpts,
		state:       stateIdle,
		cfg:         cfg.Config,
		mcpManager:  cfg.MCPManager,
		mcpServers:  cfg.MCPServers,
		thinkingView: NewThinkingView(),
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

func (m *Model) Init() tea.Cmd {
	return nil
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
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
	submit, cancelled := m.askUserView.HandleKey(msg.String())
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
		maxInputHeight := m.height - statusHeight - separatorsHeight - minChatHeight
		if maxInputHeight < 1 {
			maxInputHeight = 1
		}
		m.input.SetMaxHeight(maxInputHeight)
		inputHeight = m.input.Height()
	}

	chatHeight := m.height - inputHeight - statusHeight - separatorsHeight
	if chatHeight < minChatHeight {
		chatHeight = minChatHeight
	}

	m.chatview.SetSize(m.width, chatHeight)
}

func (m *Model) setState(st state) {
	m.state = st
	m.statusbar.SetState(st)
	m.chatview.SetStreaming(st == stateStreaming)
	m.input.SetEnabled(st == stateIdle || st == stateStreaming)
}

func (m *Model) sendMessage(text string) tea.Cmd {
	m.chatview.AddMessage(chatMessage{Role: "user", Content: text})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	// Expand @path references: inject file/directory contents into the
	// message sent to the LLM, but keep the TUI display unexpanded.
	expandedText := ExpandAtReferences(text)

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	// Set up steer channel so pending input can be injected at tool-call boundaries.
	m.steerRespCh = make(chan string)
	m.agent.SetSteerChannel(m.steerRespCh)

	m.streamGen++
	m.eventCh = m.agent.RunConversationStream(ctx, m.history, expandedText, m.systemPrompt, m.chatOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// sendCommitCommand 使用干净的对话上下文（不继承历史）把任务说明发给 LLM，
// 由模型用 Bash 工具自行执行 git 并提交（不在此处 exec 任何命令）。
// 如果配置了 commit_provider，使用专用 provider；否则回退到主 provider。
func (m *Model) sendCommitCommand() tea.Cmd {
	m.chatview.AddMessage(chatMessage{Role: "user", Content: "/commit"})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	// Save conversation history so we can restore it after the one-off
	// commit run completes (RunOneOffStream overwrites m.history).
	m.savedHistory = make([]llm.Message, len(m.history))
	copy(m.savedHistory, m.history)

	// Save tool registry: /commit should only use the Bash tool.
	// Save all tools, then unregister everything except Bash.
	m.savedTools = m.agent.SaveToolRegistry()
	for _, name := range m.agent.ToolNames() {
		if name != tools.ToolNameBash {
			m.agent.UnregisterTool(name)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	commitProvider := m.agent.CommitProvider()
	commitModel := m.agent.Model()

	// Disable thinking for /commit: the commit message task is simple and
	// avoiding thinking saves tokens/latency.
	commitOpts := m.chatOpts
	thinkingDisabled := false
	commitOpts.Thinking = &thinkingDisabled

	m.streamGen++
	m.eventCh = m.agent.RunOneOffStream(ctx, commitProvider, m.systemPrompt,
		commitUserPrompt(commitModel), commitOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// sendInitCommand sends the init prompt to LLM to generate .tachi.md
func (m *Model) sendInitCommand() tea.Cmd {
	m.chatview.AddMessage(chatMessage{Role: "user", Content: "/init"})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	m.streamGen++
	m.eventCh = m.agent.RunConversationStream(ctx, m.history, InitPromptTemplate, m.systemPrompt, m.chatOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// handleCompactCommand handles the /compact slash command.
// It appends a compact instruction to the current conversation so the LLM
// can summarize using its existing context window (no history re-embedding).
// After the turn completes, a new session is created with the summary.
func (m *Model) handleCompactCommand() tea.Cmd {
	// 1. Pre-checks
	sm := m.agent.SessionManager()
	if sm == nil || !sm.HasCurrent() {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "没有活跃的 session 可以压缩",
		})
		return nil
	}
	if len(m.history) == 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "对话历史为空，无需压缩",
		})
		return nil
	}

	// 2. Show user intent and set state
	m.chatview.AddMessage(chatMessage{Role: "user", Content: "/compact"})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	// 3. Save state for rollback
	m.savedHistory = make([]llm.Message, len(m.history))
	copy(m.savedHistory, m.history)
	m.isCompacting = true

	// 3.5 Store current session memory before compaction
	m.agent.StoreCompactMemory()

	// 4. Clear tools so the LLM doesn't call tools during compact.
	// Prompt also instructs "不要调用任何工具" as a double safeguard.
	m.savedTools = m.agent.SaveToolRegistry()
	m.agent.ClearToolRegistry()

	// 5. Build compact instruction (no history — LLM sees history as context)
	instruction := agent.BuildCompactInstruction()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	m.streamGen++
	// Use RunConversationStream so the LLM sees the current session as
	// structured history (role alternation, tool calls, etc.).
	m.eventCh = m.agent.RunConversationStream(ctx, m.history, instruction, m.systemPrompt, m.chatOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// formatCompactSummary formats the compact result for display in the chatview.
func formatCompactSummary(summary string, oldMsgCount int) string {
	var sb strings.Builder
	sb.WriteString("🔍 **对话已压缩**\n\n")
	sb.WriteString(fmt.Sprintf("旧消息数: %d 条\n", oldMsgCount))
	sb.WriteString("\n---\n\n")
	sb.WriteString(summary)
	sb.WriteString("\n\n---\n")
	sb.WriteString("💡 使用 `/sessions` 可查看旧会话的完整历史。\n")
	return sb.String()
}

// rollbackCompact restores the pre-compact state (history + tools) and displays
// an error in the chatview. Used when the compact LLM call fails or
// FinalizeCompact returns an error.
func (m *Model) rollbackCompact(errMsg string) {
	m.history = m.savedHistory
	m.savedHistory = nil
	if m.savedTools != nil {
		m.agent.RestoreToolRegistry(m.savedTools)
		m.savedTools = nil
	}
	m.chatview.AddMessage(chatMessage{Role: "error", Content: errMsg})
	m.chatview.FinishStreaming()
	m.setState(stateIdle)
	m.cancelFunc = nil
	m.eventCh = nil
}

// handleSkillCommand handles the /skill slash command.
// /skill              → list all available skills
// /skill <name>       → activate a specific skill
// /skill reload       → re-scan skill directories
func (m *Model) handleSkillCommand() tea.Cmd {
	store := m.agent.SkillStore()
	if store == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Skill system not available",
		})
		return nil
	}

	parts := strings.Fields(m.subcommandInput)
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch sub {
	case "", "list":
		metas := store.List()
		if len(metas) == 0 {
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: "No skills found. Create a skill by adding a `SKILL.md` file in `.tachi/skills/<name>/` or `~/.tachi/skills/<name>/`.",
			})
			return nil
		}

		var sb strings.Builder
		sb.WriteString("**Available Skills:**\n\n")
		for _, meta := range metas {
			sourceTag := ""
			if meta.Source == "project" {
				sourceTag = " 🏠"
			}
			sb.WriteString(fmt.Sprintf("- **%s**%s\n", meta.Name, sourceTag))
			sb.WriteString(fmt.Sprintf("  %s\n", meta.Description))
			if len(meta.Tags) > 0 {
				sb.WriteString(fmt.Sprintf("  Tags: %s\n", strings.Join(meta.Tags, ", ")))
			}
			sb.WriteString(fmt.Sprintf("  Use `/ %s` to activate\n\n", meta.Name))
		}
		sb.WriteString(fmt.Sprintf("\n%d skill(s) total", len(metas)))
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: sb.String(),
		})
		return nil

	case "reload":
		// Re-create the store to pick up new/modified skills
		m.agent.ReloadSkills()
		metas := store.List()
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Skills reloaded — %d skill(s) found", len(metas)),
		})
		return nil

	default:
		// /skill <name> — activate a specific skill
		return m.sendSkillMessage(sub, "")
	}
}

// sendSkillMessage activates a skill and sends its instructions as a user message.
// skillName is the skill to activate. extraArgs are additional text from the
// command line (e.g., "main.go" from "/code-review main.go").
func (m *Model) sendSkillMessage(skillName string, extraArgs string) tea.Cmd {
	// Prevent duplicate activation within the same session.
	if m.agent.IsSkillActive(skillName) {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Skill **%s** is already active in this session.", skillName),
		})
		return nil
	}

	msg, err := m.agent.ActivateSkill(skillName, extraArgs)
	if err != nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Skill **%s** not found. Use `/skill` to see available skills.", skillName),
		})
		return nil
	}

	// Add the activation message as a system-style user message
	m.chatview.AddMessage(chatMessage{
		Role:    "user",
		Content: fmt.Sprintf("/%s %s", skillName, extraArgs),
	})

	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	m.streamGen++
	m.eventCh = m.agent.RunConversationStream(ctx, m.history, msg, m.systemPrompt, m.chatOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

func (m *Model) nextEvent() tea.Cmd {
	ch := m.eventCh
	gen := m.streamGen
	return func() tea.Msg {
		event, ok := <-ch
		if !ok {
			return streamDoneMsg{gen: gen}
		}
		return agentEventMsg{event: event, gen: gen}
	}
}

func (m *Model) handleAgentEvent(event agent.AgentEvent) tea.Cmd {
	switch event.Type {
	case agent.AgentEventTextDelta:
		m.setState(stateStreaming)
		m.chatview.AppendTextDelta(event.TextDelta)
		return m.nextEvent()

	case agent.AgentEventThinkingDelta:
		m.setState(stateStreaming)
		m.chatview.AppendThinkingDelta(event.ThinkingDelta)
		m.thinkingView.Append(event.ThinkingDelta)
		return m.nextEvent()

	case agent.AgentEventToolCallStart:
		m.setState(stateStreaming)
		m.chatview.AddToolCall(event.ToolName, event.ToolID)
		return m.nextEvent()

	case agent.AgentEventToolCallArgs:
		m.chatview.UpdateToolArgs(event.ToolID, event.ToolArgs)
		return m.nextEvent()

	case agent.AgentEventToolConfirmation:
		m.logger.Log("TUI: Received AgentEventToolConfirmation, diff length: %d", len(event.ToolDiff))
		m.pendingConfirm = &pendingConfirm{
			toolName: event.ToolName,
			toolID:   event.ToolID,
			toolArgs: event.ToolArgs,
			diff:     event.ToolDiff,
		}
		m.setState(stateAwaitingConfirmation)
		// Show diff as a message in chatview
		m.chatview.AddMessage(chatMessage{
			Role:    "tool_confirmation",
			Content: "Edit File Confirmation\n" + event.ToolDiff,
		})
		if len(event.ToolDiff) > 100 {
			m.logger.Log("TUI: diff preview: %s...", event.ToolDiff[:100])
		} else {
			m.logger.Log("TUI: diff: %s", event.ToolDiff)
		}
		return nil

	case agent.AgentEventAskUser:
		m.logger.Log("TUI: Received AgentEventAskUser, %d questions", len(event.Questions))
		m.askUserView = NewAskUserView(event.Questions, m.width)
		m.setState(stateAskUserQuestion)
		m.layout()
		return nil

	case agent.AgentEventToolResult:
		m.chatview.UpdateToolResult(event.ToolID, event.ToolResult, event.ToolIsError, event.ToolDuration)
		return m.nextEvent()

	case agent.AgentEventSubagentStart:
		// Sub-agent started — mark the tool call as having a subagent.
		m.chatview.MarkSubagent(event.ToolID)
		return m.nextEvent()

	case agent.AgentEventSubagentDone:
		// Sub-agent completed — refresh cost to include subagent usage.
		m.refreshSessionCost()
		return m.nextEvent()

	case agent.AgentEventSteerCheck:
		if len(m.pendingQueue) > 0 {
			combined := strings.Join(m.pendingQueue, "\n\n")
			m.pendingQueue = nil
			m.chatview.RemovePendingItems()
			m.statusbar.SetPendingCount(0)
			// Expand @-file references before sending to the LLM.
			expanded := ExpandAtReferences(combined)
			// Add as a normal user message in chatview for visual continuity.
			m.chatview.AddMessage(chatMessage{Role: "user", Content: combined})
			// Send expanded steer text to agent (non-blocking with select).
			select {
			case m.steerRespCh <- expanded:
			default:
			}
		} else {
			select {
			case m.steerRespCh <- "":
			default:
			}
		}
		return m.nextEvent()

	case agent.AgentEventTurnComplete:
		m.steerRespCh = nil
		if event.Messages != nil {
			m.history = event.Messages
		}

		// Compact handling — before one-off restore
		if m.isCompacting {
			m.isCompacting = false
			if event.Result != nil && event.Result.Error != nil {
				m.rollbackCompact("压缩失败: " + event.Result.Error.Error())
				return nil
			}

			summary := event.Result.Response
			sm := m.agent.SessionManager()

			// Save old ThreadID before FinalizeCompact (sm.New changes current)
			oldThreadID := ""
			if oldSess := sm.Current(); oldSess != nil {
				oldThreadID = oldSess.ThreadID
			}

			oldMsgCount := len(m.savedHistory)
			newHistory, err := agent.FinalizeCompact(sm, m.systemPrompt, summary)
			if err != nil {
				m.rollbackCompact("压缩失败: " + err.Error())
				return nil
			}

			// Migrate ThreadID to new session
			if oldThreadID != "" {
				sm.SetThreadID(oldThreadID)
			}

			m.history = newHistory
			m.savedHistory = nil

			// Restore tools (cleared before compact)
			if m.savedTools != nil {
				m.agent.RestoreToolRegistry(m.savedTools)
				m.savedTools = nil
			}

			// Update usage (compact LLM call's tokens count toward the session)
			if event.Usage != nil {
				m.totalUsage.InputTokens = event.Usage.InputTokens
				m.totalUsage.OutputTokens += event.Usage.OutputTokens
				m.totalUsage.CacheCreationInputTokens += event.Usage.CacheCreationInputTokens
				m.totalUsage.CacheReadInputTokens += event.Usage.CacheReadInputTokens
				m.statusbar.SetUsage(&m.totalUsage)
				m.refreshSessionCost()
			}

			// Rebuild chatview for the new session
			m.chatview.Clear()
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: formatCompactSummary(summary, oldMsgCount),
			})
			m.chatview.FinishStreaming()
			m.syncSessionInfo()
			m.setState(stateIdle)
			m.pendingQueue = nil
			m.chatview.RemovePendingItems()
			m.statusbar.SetPendingCount(0)
			m.cancelFunc = nil
			m.eventCh = nil
			return nil
		}

		isOneOff := m.savedHistory != nil
		if isOneOff {
			m.history = m.savedHistory
			m.savedHistory = nil
		} else if event.Usage != nil {
			// InputTokens from the API already reflects the total context size
			// (all prior messages included), so we take the latest value instead
			// of accumulating (which would produce a nonsense inflated number).
			m.totalUsage.InputTokens = event.Usage.InputTokens
			m.totalUsage.OutputTokens += event.Usage.OutputTokens
			m.totalUsage.CacheCreationInputTokens += event.Usage.CacheCreationInputTokens
			m.totalUsage.CacheReadInputTokens += event.Usage.CacheReadInputTokens
			m.statusbar.SetUsage(&m.totalUsage)
			m.refreshSessionCost()
		}
		if m.savedTools != nil {
			m.agent.RestoreToolRegistry(m.savedTools)
			m.savedTools = nil
		}
		m.chatview.FinishStreaming()
		m.syncSessionInfo()

		// Send terminal notification when a turn completes (not for one-offs like /commit).
		if m.notifyOnComplete && !isOneOff {
			notifyTerminal("tachi", "Reply ready")
		}

		// Drain pending queue if not in a one-off context (e.g. /commit, /init).
		if len(m.pendingQueue) > 0 && !isOneOff {
			combined := strings.Join(m.pendingQueue, "\n\n")
			m.pendingQueue = nil
			m.chatview.RemovePendingItems()
			m.statusbar.SetPendingCount(0)
			m.cancelFunc = nil
			m.eventCh = nil
			return m.sendMessage(combined)
		}

		// Discard pending queue for one-off contexts (savedHistory was set).
		if isOneOff {
			m.pendingQueue = nil
			m.chatview.RemovePendingItems()
			m.statusbar.SetPendingCount(0)
		}

		m.setState(stateIdle)
		m.cancelFunc = nil
		m.eventCh = nil
		return nil

	case agent.AgentEventSessionTitle:
		// Title generated early: refresh statusbar immediately without
		// waiting for TurnComplete.
		m.syncSessionInfo()
		return m.nextEvent()

	case agent.AgentEventError:
		m.steerRespCh = nil
		if event.Messages != nil {
			m.history = event.Messages
		}
		if m.savedHistory != nil {
			m.history = m.savedHistory
			m.savedHistory = nil
		}
		if m.savedTools != nil {
			m.agent.RestoreToolRegistry(m.savedTools)
			m.savedTools = nil
		}
		// Clear pending queue on error (Ctrl+C clears it earlier in handleCtrlC,
		// this handles non-interrupt errors like API failures).
		m.pendingQueue = nil
		m.chatview.RemovePendingItems()
		m.statusbar.SetPendingCount(0)
		if event.Result != nil && event.Result.ExitReason == "interrupted" {
			m.chatview.FinishStreaming()
		} else {
			errMsg := "Unknown error"
			if event.Result != nil && event.Result.Error != nil {
				errMsg = event.Result.Error.Error()
			}
			m.chatview.AddMessage(chatMessage{Role: "error", Content: errMsg})
			// Notify on error (but not for user-initiated interruptions).
			if m.notifyOnComplete {
				notifyTerminal("tachi", "Error — "+errMsg)
			}
		}
		m.setState(stateIdle)
		m.cancelFunc = nil
		m.eventCh = nil
		return nil
	}

	return m.nextEvent()
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

// --- MCP overlay ---

// mcpOverlayMsg delivers an async status message to the MCP overlay.
type mcpOverlayMsg struct {
	content string
	nextCh  <-chan string
}

// readNextMCPOverlayMsg reads the next message from ch and returns an mcpOverlayMsg.
func readNextMCPOverlayMsg(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		content, ok := <-ch
		if !ok {
			return nil
		}
		return mcpOverlayMsg{content: content, nextCh: ch}
	}
}

// enterMCPOverlay builds server items and opens the overlay.
func (m *Model) enterMCPOverlay() tea.Cmd {
	if len(m.mcpServers) == 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No MCP servers configured in ~/.tachi/config.yaml",
		})
		return nil
	}

	items := m.buildMCPServerItems()
	m.mcpView.SetServers(items)
	m.mcpView.SetSize(m.width, m.height)
	m.mcpView.SetMessage("")
	m.setState(stateManagingMCP)
	return nil
}

// exitMCPOverlay dismisses the overlay and returns to idle.
func (m *Model) exitMCPOverlay() {
	m.setState(stateIdle)
	m.layout()
}

// buildMCPServerItems collects current server state into display items.
func (m *Model) buildMCPServerItems() []MCPServerItem {
	items := make([]MCPServerItem, 0, len(m.mcpServers))
	for _, srv := range m.mcpServers {
		enabled := srv.IsEnabled()
		connected := false
		if m.mcpManager != nil {
			connected = m.mcpManager.IsConnected(srv.Name)
		}

		typeStr := string(srv.Type)

		// Gather tools for this server
		prefix := fmt.Sprintf("mcp__%s__", srv.Name)
		var tools []MCPToolItem
		for _, schema := range m.agent.ToolSchemas() {
			if strings.HasPrefix(schema.Name, prefix) {
				params := make([]MCPParamItem, 0, len(schema.Parameters.Properties))
				reqSet := make(map[string]bool, len(schema.Parameters.Required))
				for _, r := range schema.Parameters.Required {
					reqSet[r] = true
				}
				for pName, p := range schema.Parameters.Properties {
					params = append(params, MCPParamItem{
						Name:        pName,
						Type:        p.Type,
						Description: p.Description,
						Required:    reqSet[pName],
					})
				}
				tools = append(tools, MCPToolItem{
					Name:        strings.TrimPrefix(schema.Name, prefix),
					Description: schema.Description,
					Parameters:  params,
				})
			}
		}

		items = append(items, MCPServerItem{
			Name:      srv.Name,
			Type:      typeStr,
			Enabled:   enabled,
			Connected: connected,
			ToolCount: len(tools),
			Tools:     tools,
			HasOAuth:  srv.HasOAuth(),
			Profile:   srv.Profile,
		})
	}
	return items
}

// refreshMCPServerItems rebuilds server data and re-injects into the view,
// preserving selection position.
func (m *Model) refreshMCPServerItems() {
	oldSel := m.mcpView.SelectedServer()
	items := m.buildMCPServerItems()
	m.mcpView.SetServers(items)
	// Try to restore selection
	if oldSel != "" {
		for i := range items {
			if items[i].Name == oldSel {
				m.mcpView.selIdx = i
				break
			}
		}
	}
	m.mcpView.SetSize(m.width, m.height)
}

// handleKeyManagingMCP dispatches actions from the overlay.
func (m *Model) handleKeyManagingMCP(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	act := m.mcpView.HandleKey(msg.String())

	switch act {
	case MCPActionDismiss:
		m.exitMCPOverlay()
		return m, nil

	case MCPActionToggle:
		name := m.mcpView.SelectedServer()
		if name == "" {
			return m, nil
		}
		return m, m.mcpOverlayToggle(name)

	case MCPActionReconnect:
		name := m.mcpView.SelectedServer()
		if name == "" || m.mcpManager == nil {
			return m, nil
		}
		return m, m.mcpOverlayReconnect(name)

	case MCPActionAuth:
		name := m.mcpView.SelectedServer()
		if name == "" || m.mcpManager == nil {
			return m, nil
		}
		return m, m.mcpOverlayAuth(name)
	}

	return m, nil
}

// mcpOverlayToggle handles toggle from within the overlay.
func (m *Model) mcpOverlayToggle(name string) tea.Cmd {
	idx := -1
	for i := range m.mcpServers {
		if m.mcpServers[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		m.mcpView.SetMessage(fmt.Sprintf("Server %s not found", name))
		return nil
	}

	srv := &m.mcpServers[idx]
	wasEnabled := srv.Enabled
	if wasEnabled == nil || *wasEnabled {
		// Disable
		disabled := false
		srv.Enabled = &disabled
		if m.mcpManager != nil {
			_ = m.mcpManager.Disconnect(srv.Name)
			m.unregisterMCPTools(srv.Name)
		}
		m.refreshMCPServerItems()
		m.mcpView.SetMessage(fmt.Sprintf("✓ %s disabled", name))
		return nil
	}

	// Enable asynchronously
	enabled := true
	srv.Enabled = &enabled

	if m.mcpManager == nil {
		m.mcpView.SetMessage(fmt.Sprintf("✓ %s enabled (no manager)", name))
		m.refreshMCPServerItems()
		return nil
	}

	m.mcpView.SetMessage(fmt.Sprintf("Enabling %s...", name))

	ch := make(chan string, 1)
	go m.mcpOverlayConnectAndRegister(srv, ch)
	return readNextMCPOverlayMsg(ch)
}

// mcpOverlayReconnect handles reconnect from within the overlay.
func (m *Model) mcpOverlayReconnect(name string) tea.Cmd {
	if m.mcpManager == nil {
		m.mcpView.SetMessage("No MCP manager available")
		return nil
	}

	var srv *config.MCPServerConfig
	for i := range m.mcpServers {
		if m.mcpServers[i].Name == name {
			srv = &m.mcpServers[i]
			break
		}
	}
	if srv == nil {
		m.mcpView.SetMessage(fmt.Sprintf("Server %s not found", name))
		return nil
	}

	if !srv.IsEnabled() {
		m.mcpView.SetMessage(fmt.Sprintf("%s is disabled — toggle first", name))
		return nil
	}

	m.unregisterMCPTools(name)
	m.mcpView.SetMessage(fmt.Sprintf("Reconnecting %s...", name))

	ch := make(chan string, 1)
	go m.mcpOverlayReconnectAndRegister(srv, ch)
	return readNextMCPOverlayMsg(ch)
}

// mcpOverlayAuth starts the OAuth flow from within the overlay.
func (m *Model) mcpOverlayAuth(name string) tea.Cmd {
	if m.mcpManager == nil {
		m.mcpView.SetMessage("No MCP manager available")
		return nil
	}

	var srv *config.MCPServerConfig
	for i := range m.mcpServers {
		if m.mcpServers[i].Name == name {
			srv = &m.mcpServers[i]
			break
		}
	}
	if srv == nil {
		m.mcpView.SetMessage(fmt.Sprintf("Server %s not found", name))
		return nil
	}

	if srv.Type != config.MCPTransportHTTP {
		m.mcpView.SetMessage(fmt.Sprintf("OAuth only for HTTP servers (%s is stdio)", name))
		return nil
	}

	m.mcpView.SetMessage(fmt.Sprintf("Starting OAuth for %s...", name))

	ch := make(chan string, 1)
	go func() {
		defer close(ch)

		errFn := func(msg string) {
			select {
			case ch <- msg:
			default:
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		if err := mcp.RunOAuthFlow(ctx, srv, errFn); err != nil {
			m.logger.Log("MCP: OAuth flow failed for %q: %v", srv.Name, err)
			if _, ok := errors.AsType[*mcp.OAuthRequiredError](err); !ok {
				ch <- fmt.Sprintf("OAuth failed: %v", err)
			}
			return
		}

		ch <- fmt.Sprintf("OAuth OK for %s — reconnecting...", srv.Name)

		reconnectCtx, reconnectCancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
		defer reconnectCancel()

		tools, err := m.mcpManager.Reconnect(reconnectCtx, srv)
		if err != nil {
			ch <- fmt.Sprintf("Reconnect failed: %v", err)
			return
		}

		for _, t := range tools {
			m.agent.RegisterTool(t)
		}

		ch <- fmt.Sprintf("✓ %s connected with %d tool(s)", srv.Name, len(tools))
	}()

	return readNextMCPOverlayMsg(ch)
}

// mcpOverlayConnectAndRegister connects and registers tools, then sends result.
func (m *Model) mcpOverlayConnectAndRegister(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	tools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		ch <- fmt.Sprintf("Failed to connect %s: %v", srv.Name, err)
		return
	}

	for _, t := range tools {
		m.agent.RegisterTool(t)
	}

	ch <- fmt.Sprintf("✓ %s connected with %d tool(s)", srv.Name, len(tools))
}

// mcpOverlayReconnectAndRegister reconnects and registers tools, then sends result.
func (m *Model) mcpOverlayReconnectAndRegister(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	tools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		ch <- fmt.Sprintf("Failed to reconnect %s: %v", srv.Name, err)
		return
	}

	for _, t := range tools {
		m.agent.RegisterTool(t)
	}

	ch <- fmt.Sprintf("✓ %s reconnected with %d tool(s)", srv.Name, len(tools))
}

func Run(cfg ModelConfig) error {
	m := NewModel(cfg)
	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}
