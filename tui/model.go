package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/llm"
)

type state int

const (
	stateIdle      state = iota
	stateWaiting
	stateStreaming
)

type toolCallDisplay struct {
	Name    string
	ID      string
	Args    string
	Result  string
	IsError bool
	Done    bool
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

	state      state
	cancelFunc context.CancelFunc
	eventCh    <-chan agent.AgentEvent
	totalUsage llm.Usage
}

type ModelConfig struct {
	Agent        *agent.AIAgent
	SystemPrompt string
	ChatOpts     llm.ChatOptions
	ProviderInfo string
}

func NewModel(cfg ModelConfig) *Model {
	return &Model{
		statusbar:    NewStatusBar(cfg.ProviderInfo),
		chatview:     NewChatView(),
		input:        NewInputArea(),
		agent:        cfg.Agent,
		systemPrompt: cfg.SystemPrompt,
		chatOpts:     cfg.ChatOpts,
		state:        stateIdle,
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
			if m.state != stateIdle && m.cancelFunc != nil {
				m.cancelFunc()
				return m, nil
			}
			return m, tea.Quit
		}
		if m.state == stateIdle {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			cmds = append(cmds, cmd)
			m.layout()
		}

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

	case streamDoneMsg:
		// no-op

	default:
		var cmd tea.Cmd
		m.chatview, cmd = m.chatview.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) layout() {
	statusHeight := 1
	chatHeight := m.height - m.input.Height() - statusHeight
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
		m.chatview.SpinnerTick(),
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
		errMsg := "Unknown error"
		if event.Result != nil && event.Result.Error != nil {
			errMsg = event.Result.Error.Error()
		}
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: toolResultErrStyle.Render("Error: " + errMsg),
		})
		m.setState(stateIdle)
		m.cancelFunc = nil
		m.eventCh = nil
		return nil
	}

	return m.nextEvent()
}

func (m *Model) View() tea.View {
	if m.width == 0 {
		return tea.NewView("Loading...")
	}

	v := tea.NewView(lipgloss.JoinVertical(lipgloss.Top,
		m.chatview.View(),
		m.input.View(),
		m.statusbar.View(),
	))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func Run(cfg ModelConfig) error {
	m := NewModel(cfg)
	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}
