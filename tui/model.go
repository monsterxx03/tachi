package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
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
)

type toolCallDisplay struct {
	Name    string
	ID      string
	Args    string
	Preview string
	Result  string
	IsError bool
	Done    bool
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
	copyMode       bool
	cancelFunc     context.CancelFunc
	eventCh        <-chan agent.AgentEvent
	totalUsage     llm.Usage
	pendingConfirm *pendingConfirm
	askUserView    *AskUserView

	cfg            *config.Config
	providerItems  []config.ProviderConfig
	providerSelIdx int
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
}

func NewModel(cfg ModelConfig) *Model {
	m := &Model{
		statusbar:    NewStatusBar(cfg.ProviderInfo, cfg.ContextWindow),
		chatview:     NewChatView(),
		input:        NewInputArea(inputHistoryMax(cfg.Config), inputHistoryFilePath()),
		agent:        cfg.Agent,
		systemPrompt: cfg.SystemPrompt,
		chatOpts:     cfg.ChatOpts,
		state:        stateIdle,
		cfg:          cfg.Config,
	}

	if len(cfg.InitialHistory) > 0 {
		m.history = cfg.InitialHistory
		m.chatview.LoadHistory(cfg.InitialSessionMsgs)
	}

	return m
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
		return m.handleKeyMsg(msg)

	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		m.chatview, cmd = m.chatview.Update(msg)
		return m, cmd

	case InputSubmitMsg:
		text := string(msg)
		if cmd := findCommand(text); cmd != nil {
			return m, cmd.handler(m)
		}
		return m, m.sendMessage(text)

	case agentEventMsg:
		return m, m.handleAgentEvent(agent.AgentEvent(msg))

	case tea.PasteMsg:
		if m.state == stateIdle {
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
		if m.state != stateIdle {
			m.chatview.FinishStreaming()
			m.setState(stateIdle)
			m.cancelFunc = nil
			m.eventCh = nil
		}
	}

	return m, nil
}

func (m *Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m.handleCtrlC()
	}
	switch m.state {
	case stateSelectingModel:
		return m.handleKeySelectingModel(msg)
	case stateAwaitingConfirmation:
		return m.handleKeyConfirmation(msg)
	case stateAskUserQuestion:
		return m.handleKeyAskUser(msg)
	case stateIdle:
		return m.handleKeyIdle(msg)
	}
	return m, nil
}

func (m *Model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.state == stateSelectingModel {
		m.exitModelSelect("")
		return m, nil
	}
	if m.state != stateIdle && m.cancelFunc != nil {
		m.cancelFunc()
		return m, nil
	}
	return m, tea.Quit
}

func (m *Model) handleKeySelectingModel(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "ctrl+k", "ctrl+p":
		if m.providerSelIdx > 0 {
			m.providerSelIdx--
		}
	case "down", "ctrl+j", "ctrl+n":
		if m.providerSelIdx < len(m.providerItems)-1 {
			m.providerSelIdx++
		}
	case "enter":
		m.switchToProvider(m.providerSelIdx)
	case "esc":
		m.exitModelSelect("")
	}
	return m, nil
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
	case stateSelectingModel:
		inputHeight = min(len(m.providerItems)+1, m.height/2)
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
	m.input.SetEnabled(st == stateIdle)
}

func (m *Model) sendMessage(text string) tea.Cmd {
	m.chatview.AddMessage(chatMessage{Role: "user", Content: text})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()

	// Expand @path references: inject file/directory contents into the
	// message sent to the LLM, but keep the TUI display unexpanded.
	expandedText := ExpandAtReferences(text)

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	m.eventCh = m.agent.RunConversationStream(ctx, m.history, expandedText, m.systemPrompt, m.chatOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// sendCommitCommand 只把任务说明发给 LLM，由模型用 Bash 工具自行执行 git 并提交（不在此处 exec 任何命令）。
func (m *Model) sendCommitCommand() tea.Cmd {
	m.chatview.AddMessage(chatMessage{Role: "user", Content: "/commit"})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	m.eventCh = m.agent.RunConversationStream(ctx, m.history, commitUserPrompt(m.agent.Model()), m.systemPrompt, m.chatOpts)

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

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	m.eventCh = m.agent.RunConversationStream(ctx, m.history, InitPromptTemplate, m.systemPrompt, m.chatOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

func (m *Model) nextEvent() tea.Cmd {
	ch := m.eventCh
	return func() tea.Msg {
		event, ok := <-ch
		if !ok {
			return streamDoneMsg{}
		}
		return agentEventMsg(event)
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
		return m.nextEvent()

	case agent.AgentEventToolCallStart:
		m.setState(stateStreaming)
		m.chatview.AddToolCall(event.ToolName, event.ToolID)
		return m.nextEvent()

	case agent.AgentEventToolCallArgs:
		m.chatview.UpdateToolArgs(event.ToolID, event.ToolArgs)
		return m.nextEvent()

	case agent.AgentEventToolConfirmation:
		debuglog.Log("TUI: Received AgentEventToolConfirmation, diff length: %d", len(event.ToolDiff))
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
			debuglog.Log("TUI: diff preview: %s...", event.ToolDiff[:100])
		} else {
			debuglog.Log("TUI: diff: %s", event.ToolDiff)
		}
		return nil

	case agent.AgentEventAskUser:
		debuglog.Log("TUI: Received AgentEventAskUser, %d questions", len(event.Questions))
		m.askUserView = NewAskUserView(event.Questions, m.width)
		m.setState(stateAskUserQuestion)
		m.layout()
		return nil

	case agent.AgentEventToolResult:
		m.chatview.UpdateToolResult(event.ToolID, event.ToolResult, event.ToolIsError)
		return m.nextEvent()

	case agent.AgentEventTurnComplete:
		if event.Messages != nil {
			m.history = event.Messages
		}
		if event.Usage != nil {
			m.totalUsage.InputTokens += event.Usage.InputTokens
			m.totalUsage.OutputTokens += event.Usage.OutputTokens
			m.totalUsage.CacheCreationInputTokens += event.Usage.CacheCreationInputTokens
			m.totalUsage.CacheReadInputTokens += event.Usage.CacheReadInputTokens
			m.statusbar.SetUsage(&m.totalUsage)
		}
		m.chatview.FinishStreaming()
		m.setState(stateIdle)
		m.cancelFunc = nil
		m.eventCh = nil
		return nil

	case agent.AgentEventError:
		if event.Messages != nil {
			m.history = event.Messages
		}
		if event.Result != nil && event.Result.ExitReason == "interrupted" {
			m.chatview.FinishStreaming()
		} else {
			errMsg := "Unknown error"
			if event.Result != nil && event.Result.Error != nil {
				errMsg = event.Result.Error.Error()
			}
			m.chatview.AddMessage(chatMessage{Role: "error", Content: errMsg})
		}
		m.setState(stateIdle)
		m.cancelFunc = nil
		m.eventCh = nil
		return nil
	}

	return m.nextEvent()
}

func (m *Model) exitModelSelect(msg string) {
	if msg != "" {
		m.chatview.AddMessage(chatMessage{Role: "assistant", Content: msg})
	}
	m.providerItems = nil
	m.providerSelIdx = 0
	m.setState(stateIdle)
	m.layout()
}

func (m *Model) switchToProvider(idx int) {
	pCfg := &m.providerItems[idx]
	resolved, err := config.ResolveProviderConfig(pCfg)
	if err != nil {
		m.exitModelSelect("Error: " + err.Error())
		return
	}
	provider, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		m.exitModelSelect("Error: " + err.Error())
		return
	}
	m.agent.SetProvider(provider, resolved.Model)
	providerInfo := fmt.Sprintf("%s (%s)", resolved.Type, resolved.Model)
	m.statusbar.SetProviderInfo(providerInfo)
	m.statusbar.SetContextWindow(resolved.ContextWindow)
	m.exitModelSelect(fmt.Sprintf("Switched to %s", providerInfo))
}

func (m *Model) renderProviderSelection() string {
	var b strings.Builder
	b.WriteString(dimStyle.Render("Select provider (↑↓ Enter Esc)") + "\n")

	currentInfo := m.statusbar.ProviderInfo()
	maxNameLen := 0
	for _, p := range m.providerItems {
		if len(p.Name) > maxNameLen {
			maxNameLen = len(p.Name)
		}
	}
	for idx, p := range m.providerItems {
		info := fmt.Sprintf("%s (%s)", p.Type, p.Model)
		active := " "
		if info == currentInfo {
			active = "*"
		}
		line := fmt.Sprintf(" %s %-*s  %s", active, maxNameLen, p.Name, info)
		if idx == m.providerSelIdx {
			b.WriteString(completionSelectedStyle.Width(m.width).Render(line))
		} else {
			b.WriteString(completionNormalStyle.Width(m.width).Render(line))
		}
		b.WriteString("\n")
	}
	return b.String()
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
	} else if m.state == stateAwaitingConfirmation {
		inputSection = m.renderConfirmPrompt()
	} else if m.state == stateAskUserQuestion {
		inputSection = m.renderAskUserPrompt()
	}

	var content strings.Builder
	content.WriteString(m.chatview.View())
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
		return config.DefaultTUIInputHistoryLimit
	}
	return c.TUIInputHistoryMax()
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
