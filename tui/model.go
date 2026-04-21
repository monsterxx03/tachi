package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"

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
	Role    string // "user", "assistant", "tool_calls", "thinking"
	Content string
}

type Model struct {
	viewport viewport.Model
	textarea textarea.Model
	spinner  spinner.Model

	messages []chatMessage
	width    int
	height   int
	ready    bool

	agent        *agent.AIAgent
	systemPrompt string
	chatOpts     llm.ChatOptions
	history      []llm.Message
	providerInfo string

	state           state
	currentText     strings.Builder
	currentThinking strings.Builder
	currentTools    []toolCallDisplay
	cancelFunc      context.CancelFunc
	eventCh         <-chan agent.AgentEvent

	mdRenderer    *glamour.TermRenderer
	mdRenderWidth int
	renderedCache strings.Builder // cached rendered messages up to streaming point
}

type ModelConfig struct {
	Agent        *agent.AIAgent
	SystemPrompt string
	ChatOpts     llm.ChatOptions
	ProviderInfo string
}

func NewModel(cfg ModelConfig) *Model {
	ta := textarea.New()
	ta.Placeholder = "Send a message... (Enter to send, Shift+Enter for newline)"
	ta.Prompt = "> "
	ta.CharLimit = 0
	ta.SetHeight(1)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetEnabled(false)
	styles := textarea.DefaultDarkStyles()
	styles.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(styles)
	ta.Focus()

	sp := spinner.New(spinner.WithSpinner(spinner.Dot))

	md, _ := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(80),
	)

	return &Model{
		textarea:     ta,
		spinner:      sp,
		agent:        cfg.Agent,
		systemPrompt: cfg.SystemPrompt,
		chatOpts:     cfg.ChatOpts,
		providerInfo: cfg.ProviderInfo,
		state:        stateIdle,
		mdRenderer:   md,
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

		inputHeight := 3
		statusHeight := 1
		vpHeight := m.height - inputHeight - statusHeight
		if vpHeight < 1 {
			vpHeight = 1
		}

		if !m.ready {
			m.viewport = viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(vpHeight))
			m.ready = true
		} else {
			m.viewport.SetWidth(m.width)
			m.viewport.SetHeight(vpHeight)
		}
		m.textarea.SetWidth(m.width - 2)

		newWrapWidth := m.width - 4
		if newWrapWidth != m.mdRenderWidth {
			md, _ := glamour.NewTermRenderer(
				glamour.WithAutoStyle(),
				glamour.WithWordWrap(newWrapWidth),
			)
			m.mdRenderer = md
			m.mdRenderWidth = newWrapWidth
		}
		m.invalidateCache()
		m.refreshViewport()

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			if m.cancelFunc != nil {
				m.cancelFunc()
			}
			return m, tea.Quit
		case "shift+enter":
			if m.state == stateIdle {
				m.textarea.InsertString("\n")
			}
		case "enter":
			if m.state != stateIdle {
				return m, nil
			}
			text := strings.TrimSpace(m.textarea.Value())
			if text == "" {
				return m, nil
			}
			m.textarea.Reset()
			return m, m.sendMessage(text)
		default:
			if m.state == stateIdle {
				var cmd tea.Cmd
				m.textarea, cmd = m.textarea.Update(msg)
				cmds = append(cmds, cmd)
			}
		}

	case spinner.TickMsg:
		if m.state == stateWaiting {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
			m.refreshViewport()
		}

	case agentEventMsg:
		cmd := m.handleAgentEvent(agent.AgentEvent(msg))
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case streamDoneMsg:
		// no-op
	}

	if m.state == stateIdle {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) sendMessage(text string) tea.Cmd {
	m.messages = append(m.messages, chatMessage{Role: "user", Content: text})
	m.invalidateCache()
	m.state = stateWaiting
	m.currentText.Reset()
	m.currentThinking.Reset()
	m.currentTools = nil
	m.refreshViewport()

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

func (m *Model) handleAgentEvent(event agent.AgentEvent) tea.Cmd {
	switch event.Type {
	case agent.AgentEventTextDelta:
		m.state = stateStreaming
		m.currentText.WriteString(event.TextDelta)
		m.refreshViewport()
		return m.nextEvent()

	case agent.AgentEventThinkingDelta:
		m.state = stateStreaming
		m.currentThinking.WriteString(event.ThinkingDelta)
		m.refreshViewport()
		return m.nextEvent()

	case agent.AgentEventToolCallStart:
		m.state = stateStreaming
		m.currentTools = append(m.currentTools, toolCallDisplay{
			Name: event.ToolName,
			ID:   event.ToolID,
		})
		m.refreshViewport()
		return m.nextEvent()

	case agent.AgentEventToolCallArgs:
		for i := range m.currentTools {
			if m.currentTools[i].ID == event.ToolID {
				m.currentTools[i].Args = event.ToolArgs
				break
			}
		}
		m.refreshViewport()
		return m.nextEvent()

	case agent.AgentEventToolResult:
		for i := range m.currentTools {
			if m.currentTools[i].ID == event.ToolID {
				m.currentTools[i].Result = event.ToolResult
				m.currentTools[i].IsError = event.ToolIsError
				m.currentTools[i].Done = true
				break
			}
		}
		m.refreshViewport()
		return m.nextEvent()

	case agent.AgentEventTurnComplete:
		if event.Messages != nil {
			m.history = event.Messages
		}
		m.finishStreaming()
		return nil

	case agent.AgentEventError:
		if event.Messages != nil {
			m.history = event.Messages
		}
		errMsg := "Unknown error"
		if event.Result != nil && event.Result.Error != nil {
			errMsg = event.Result.Error.Error()
		}
		m.messages = append(m.messages, chatMessage{
			Role:    "assistant",
			Content: toolResultErrStyle.Render("Error: " + errMsg),
		})
		m.invalidateCache()
		m.state = stateIdle
		m.currentText.Reset()
		m.currentThinking.Reset()
		m.currentTools = nil
		m.cancelFunc = nil
		m.eventCh = nil
		m.refreshViewport()
		return nil
	}

	return m.nextEvent()
}

func (m *Model) finishStreaming() {
	content := m.currentText.String()
	rendered := m.renderMarkdown(content)

	if m.currentThinking.Len() > 0 {
		m.messages = append(m.messages, chatMessage{
			Role:    "thinking",
			Content: m.currentThinking.String(),
		})
	}

	for _, tc := range m.currentTools {
		m.messages = append(m.messages, chatMessage{
			Role:    "tool_calls",
			Content: m.renderToolCall(tc),
		})
	}

	m.messages = append(m.messages, chatMessage{
		Role:    "assistant",
		Content: rendered,
	})

	m.invalidateCache()
	m.state = stateIdle
	m.currentText.Reset()
	m.currentThinking.Reset()
	m.currentTools = nil
	m.cancelFunc = nil
	m.eventCh = nil
	m.refreshViewport()
}

func (m *Model) invalidateCache() {
	m.renderedCache.Reset()
}

func (m *Model) renderMarkdown(text string) string {
	if m.mdRenderer == nil || text == "" {
		return text
	}
	rendered, err := m.mdRenderer.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimRight(rendered, "\n")
}

func (m *Model) renderToolCall(tc toolCallDisplay) string {
	preview := agent.GetToolArgsPreview(tc.Name, tc.Args)
	var b strings.Builder

	if tc.Done {
		if tc.IsError {
			fmt.Fprintf(&b, "%s %s(%s)\n",
				toolResultErrStyle.Render("x"),
				toolCallStyle.Render(tc.Name),
				dimStyle.Render(preview))
			fmt.Fprintf(&b, "  %s", toolResultErrStyle.Render(truncate(tc.Result, 200)))
		} else {
			fmt.Fprintf(&b, "%s %s(%s)\n",
				toolResultOKStyle.Render("v"),
				toolCallStyle.Render(tc.Name),
				dimStyle.Render(preview))
			fmt.Fprintf(&b, "  %s", dimStyle.Render(truncate(tc.Result, 200)))
		}
	} else {
		fmt.Fprintf(&b, "%s %s(%s)",
			toolCallStyle.Render("~"),
			toolCallStyle.Render(tc.Name),
			dimStyle.Render(preview))
	}

	return b.String()
}

func (m *Model) rebuildCache() {
	m.renderedCache.Reset()
	for _, msg := range m.messages {
		m.renderMessageTo(&m.renderedCache, msg)
	}
}

func (m *Model) renderMessageTo(b *strings.Builder, msg chatMessage) {
	switch msg.Role {
	case "user":
		fmt.Fprintf(b, "%s\n%s\n\n",
			userLabelStyle.Render("You:"),
			msg.Content)
	case "assistant":
		fmt.Fprintf(b, "%s\n%s\n\n",
			assistantLabelStyle.Render("tachi:"),
			msg.Content)
	case "thinking":
		thinking := msg.Content
		if len(thinking) > 300 {
			thinking = thinking[:300] + "..."
		}
		fmt.Fprintf(b, "%s\n\n",
			thinkingStyle.Render("Thinking: "+thinking))
	case "tool_calls":
		fmt.Fprintf(b, "%s\n\n", msg.Content)
	}
}

func (m *Model) refreshViewport() {
	if m.renderedCache.Len() == 0 && len(m.messages) > 0 {
		m.rebuildCache()
	}

	var b strings.Builder
	b.WriteString(m.renderedCache.String())

	if m.state == stateWaiting {
		fmt.Fprintf(&b, "%s %s\n",
			assistantLabelStyle.Render("tachi:"),
			m.spinner.View())
	} else if m.state == stateStreaming {
		if m.currentThinking.Len() > 0 {
			thinking := m.currentThinking.String()
			if len(thinking) > 300 {
				thinking = thinking[:300] + "..."
			}
			fmt.Fprintf(&b, "%s\n", thinkingStyle.Render("Thinking: "+thinking))
		}
		for _, tc := range m.currentTools {
			fmt.Fprintf(&b, "%s\n", m.renderToolCall(tc))
		}
		if m.currentText.Len() > 0 {
			fmt.Fprintf(&b, "%s\n%s\n",
				assistantLabelStyle.Render("tachi:"),
				m.currentText.String())
		}
	}

	content := lipgloss.NewStyle().Width(m.width).Render(b.String())
	m.viewport.SetContent(content)
	m.viewport.GotoBottom()
}

func (m *Model) View() tea.View {
	if !m.ready {
		return tea.NewView("Loading...")
	}

	statusLeft := fmt.Sprintf(" tachi | %s", m.providerInfo)
	statusRight := ""
	switch m.state {
	case stateWaiting:
		statusRight = "waiting... "
	case stateStreaming:
		statusRight = "streaming... "
	case stateIdle:
		statusRight = "ready "
	}
	statusGap := m.width - lipgloss.Width(statusLeft) - lipgloss.Width(statusRight)
	if statusGap < 0 {
		statusGap = 0
	}
	status := statusBarStyle.Width(m.width).Render(
		statusLeft + strings.Repeat(" ", statusGap) + statusRight,
	)

	input := inputBorderStyle.Width(m.width).Render(m.textarea.View())

	v := tea.NewView(lipgloss.JoinVertical(lipgloss.Top,
		status,
		m.viewport.View(),
		input,
	))
	v.AltScreen = true
	return v
}

func truncate(s string, maxLen int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 1 {
		s = lines[0]
	}
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

func Run(cfg ModelConfig) error {
	m := NewModel(cfg)
	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}
