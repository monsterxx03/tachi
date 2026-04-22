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
	statusbar    StatusBar
	chatview     ChatView
	input        InputArea
	spinner      spinner.Model

	width  int
	height int

	agent        *agent.AIAgent
	systemPrompt string
	chatOpts     llm.ChatOptions
	history      []llm.Message

	state             state
	copyMode         bool
	cancelFunc       context.CancelFunc
	eventCh          <-chan agent.AgentEvent
	totalUsage       llm.Usage
	pendingConfirm   *pendingConfirm
	askUserView      *AskUserView

	cfg            *config.Config
	providerItems  []config.ProviderConfig
	providerSelIdx int
}

type ModelConfig struct {
	Agent        *agent.AIAgent
	SystemPrompt string
	ChatOpts     llm.ChatOptions
	ProviderInfo string
	Config       *config.Config
}

func NewModel(cfg ModelConfig) *Model {
	return &Model{
		statusbar: NewStatusBar(cfg.ProviderInfo),
		chatview:  NewChatView(),
		input:     NewInputArea(),
		spinner:   spinner.New(spinner.WithSpinner(spinner.Dot)),
		agent:     cfg.Agent,
		systemPrompt: cfg.SystemPrompt,
		chatOpts:     cfg.ChatOpts,
		state:        stateIdle,
		cfg:          cfg.Config,
	}
}

func (m *Model) Init() tea.Cmd {
	return nil
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
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
		if m.state == stateSelectingModel {
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
		if m.state == stateAwaitingConfirmation {
			switch msg.String() {
			case "y", "Y", "enter":
				m.agent.ConfirmTool(true)
				m.pendingConfirm = nil
				m.setState(stateStreaming)
				m.layout()
				return m, m.nextEvent()
			case "n", "N", "esc":
				m.agent.ConfirmTool(false)
				m.pendingConfirm = nil
				m.setState(stateStreaming)
				m.layout()
				return m, m.nextEvent()
			}
			return m, nil
		}
		if m.state == stateAskUserQuestion {
			if m.askUserView == nil {
				m.setState(stateStreaming)
				return m, nil
			}
			submit, cancelled := m.askUserView.HandleKey(msg.String())
			if cancelled {
				m.agent.RespondToAskUser(nil, nil)
				m.askUserView = nil
				m.setState(stateStreaming)
				m.layout()
				return m, m.nextEvent()
			}
			if submit {
				answers := m.askUserView.GetAnswers()
				m.agent.RespondToAskUser(answers, nil)
				m.askUserView = nil
				m.setState(stateStreaming)
				m.layout()
				return m, m.nextEvent()
			}
			return m, nil
		}
		if m.state == stateIdle {
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
				cmds = append(cmds, cmd)
				return m, tea.Batch(cmds...)
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			cmds = append(cmds, cmd)
			m.layout()
		}

	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		m.chatview, cmd = m.chatview.Update(msg)
		cmds = append(cmds, cmd)

	case InputSubmitMsg:
		text := string(msg)
		if cmd := findCommand(text); cmd != nil {
			return m, cmd.handler(m)
		}
		return m, m.sendMessage(text)

	case agentEventMsg:
		cmd := m.handleAgentEvent(agent.AgentEvent(msg))
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case tea.PasteMsg:
		if m.state == stateIdle {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			cmds = append(cmds, cmd)
		}

	case streamDoneMsg:
		// no-op

	case spinner.TickMsg:
		if m.state == stateWaiting {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}

	case tickMsg:
		// Re-render when in confirmation or ask-user state, continue ticking
		if m.state == stateAwaitingConfirmation || m.state == stateAskUserQuestion {
			cmds = append(cmds, m.tick())
		}
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) layout() {
	statusHeight := 1
	inputHeight := m.input.Height()
	if m.state == stateSelectingModel {
		inputHeight = len(m.providerItems) + 1
	} else if m.state == stateAwaitingConfirmation {
		// Estimate height based on diff content (roughly 1 line per 80 chars + header/footer)
		if m.pendingConfirm != nil {
			diffLines := strings.Count(m.pendingConfirm.diff, "\n") + 1
			inputHeight = min(10+diffLines, m.height/3) // Min 10, max 1/3 of screen
		}
	} else if m.state == stateAskUserQuestion {
		// Estimate height based on AskUserView
		if m.askUserView != nil {
			// Use a fixed estimate since we can't easily count options without access to the view's questions
			inputHeight = 15 // enough for a few options
		}
	}
	chatHeight := m.height - inputHeight - statusHeight
	if chatHeight < 1 {
		chatHeight = 1
	}

	m.statusbar.SetWidth(m.width)
	m.chatview.SetSize(m.width, chatHeight)
	m.input.SetWidth(m.width)
}

func (m *Model) setState(st state) {
	m.state = st
	m.statusbar.SetState(st)
	m.chatview.SetState(st)
	m.input.SetEnabled(st == stateIdle)
}

func (m *Model) sendMessage(text string) tea.Cmd {
	m.chatview.AddMessage(chatMessage{Role: "user", Content: text})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	m.eventCh = m.agent.RunConversationStream(ctx, m.history, text, m.systemPrompt, m.chatOpts)

	return tea.Batch(
		m.spinner.Tick,
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

type tickMsg struct{}

func (m *Model) tick() tea.Cmd {
	return func() tea.Msg {
		return tickMsg{}
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
		m.askUserView = NewAskUserView(event.Questions)
		m.setState(stateAskUserQuestion)
		// Build question display message
		var b strings.Builder
		b.WriteString("Questions for you:\n\n")
		for i, q := range event.Questions {
			b.WriteString(fmt.Sprintf("[%d] %s (%s)\n", i+1, q.Question, q.Header))
			for j, opt := range q.Options {
				b.WriteString(fmt.Sprintf("    %d. %s - %s\n", j+1, opt.Label, opt.Description))
			}
			b.WriteString("\n")
		}
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: b.String(),
		})
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

	if m.state == stateWaiting {
		content.WriteString("\n")
		content.WriteString(assistantMsgStyle.Width(m.width - 2).Render(m.spinner.View()))
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

func Run(cfg ModelConfig) error {
	m := NewModel(cfg)
	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}
