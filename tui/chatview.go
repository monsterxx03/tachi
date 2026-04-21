package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"

	"github.com/monsterxx03/tachi/agent"
)

type ChatView struct {
	viewport viewport.Model
	spinner  spinner.Model
	messages []chatMessage
	width    int
	height   int
	ready    bool

	state           state
	currentText     strings.Builder
	currentThinking strings.Builder
	currentTools    []toolCallDisplay

	mdRenderer    *glamour.TermRenderer
	mdRenderWidth int
	renderedCache strings.Builder
}

func NewChatView() ChatView {
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	md, _ := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(80),
	)
	return ChatView{
		spinner:    sp,
		mdRenderer: md,
	}
}

func (c *ChatView) SetSize(w, h int) {
	c.width = w
	c.height = h

	if !c.ready {
		c.viewport = viewport.New(viewport.WithWidth(w), viewport.WithHeight(h))
		c.ready = true
	} else {
		c.viewport.SetWidth(w)
		c.viewport.SetHeight(h)
	}

	newWrapWidth := w - 4
	if newWrapWidth != c.mdRenderWidth {
		md, _ := glamour.NewTermRenderer(
			glamour.WithAutoStyle(),
			glamour.WithWordWrap(newWrapWidth),
		)
		c.mdRenderer = md
		c.mdRenderWidth = newWrapWidth
	}
	c.invalidateCache()
	c.refresh()
}

func (c *ChatView) SetState(st state) { c.state = st }

func (c *ChatView) AddMessage(msg chatMessage) {
	c.messages = append(c.messages, msg)
	c.invalidateCache()
	c.refresh()
}

func (c *ChatView) AppendTextDelta(s string) {
	c.state = stateStreaming
	c.currentText.WriteString(s)
	c.refresh()
}

func (c *ChatView) AppendThinkingDelta(s string) {
	c.state = stateStreaming
	c.currentThinking.WriteString(s)
	c.refresh()
}

func (c *ChatView) AddToolCall(name, id string) {
	c.state = stateStreaming
	c.currentTools = append(c.currentTools, toolCallDisplay{Name: name, ID: id})
	c.refresh()
}

func (c *ChatView) UpdateToolArgs(id, args string) {
	for i := range c.currentTools {
		if c.currentTools[i].ID == id {
			c.currentTools[i].Args = args
			break
		}
	}
	c.refresh()
}

func (c *ChatView) UpdateToolResult(id, result string, isError bool) {
	for i := range c.currentTools {
		if c.currentTools[i].ID == id {
			c.currentTools[i].Result = result
			c.currentTools[i].IsError = isError
			c.currentTools[i].Done = true
			break
		}
	}
	c.refresh()
}

func (c *ChatView) FinishStreaming() {
	content := c.currentText.String()
	rendered := c.renderMarkdown(content)

	if c.currentThinking.Len() > 0 {
		c.messages = append(c.messages, chatMessage{
			Role:    "thinking",
			Content: c.currentThinking.String(),
		})
	}

	for _, tc := range c.currentTools {
		c.messages = append(c.messages, chatMessage{
			Role:    "tool_calls",
			Content: c.renderToolCall(tc),
		})
	}

	c.messages = append(c.messages, chatMessage{
		Role:    "assistant",
		Content: rendered,
	})

	c.ResetStreaming()
	c.invalidateCache()
	c.refresh()
}

func (c *ChatView) ResetStreaming() {
	c.currentText.Reset()
	c.currentThinking.Reset()
	c.currentTools = nil
}

func (c ChatView) Update(msg tea.Msg) (ChatView, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg.(type) {
	case spinner.TickMsg:
		if c.state == stateWaiting {
			var cmd tea.Cmd
			c.spinner, cmd = c.spinner.Update(msg)
			cmds = append(cmds, cmd)
			c.refresh()
		}
	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		c.viewport, cmd = c.viewport.Update(msg)
		cmds = append(cmds, cmd)
	}

	return c, tea.Batch(cmds...)
}

func (c *ChatView) SpinnerTick() tea.Cmd {
	return c.spinner.Tick
}

func (c ChatView) View() string {
	return c.viewport.View()
}

// --- internal rendering ---

func (c *ChatView) invalidateCache() {
	c.renderedCache.Reset()
}

func (c *ChatView) rebuildCache() {
	c.renderedCache.Reset()
	for _, msg := range c.messages {
		c.renderMessageTo(&c.renderedCache, msg)
	}
}

func (c *ChatView) renderMessageTo(b *strings.Builder, msg chatMessage) {
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

func (c *ChatView) refresh() {
	if c.renderedCache.Len() == 0 && len(c.messages) > 0 {
		c.rebuildCache()
	}

	var b strings.Builder
	b.WriteString(c.renderedCache.String())

	if c.state == stateWaiting {
		fmt.Fprintf(&b, "%s %s\n",
			assistantLabelStyle.Render("tachi:"),
			c.spinner.View())
	} else if c.state == stateStreaming {
		if c.currentThinking.Len() > 0 {
			thinking := c.currentThinking.String()
			if len(thinking) > 300 {
				thinking = thinking[:300] + "..."
			}
			fmt.Fprintf(&b, "%s\n", thinkingStyle.Render("Thinking: "+thinking))
		}
		for _, tc := range c.currentTools {
			fmt.Fprintf(&b, "%s\n", c.renderToolCall(tc))
		}
		if c.currentText.Len() > 0 {
			fmt.Fprintf(&b, "%s\n%s\n",
				assistantLabelStyle.Render("tachi:"),
				c.currentText.String())
		}
	}

	content := lipgloss.NewStyle().Width(c.width).Render(b.String())
	c.viewport.SetContent(content)
	c.viewport.GotoBottom()
}

func (c *ChatView) renderMarkdown(text string) string {
	if c.mdRenderer == nil || text == "" {
		return text
	}
	rendered, err := c.mdRenderer.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimRight(rendered, "\n")
}

func (c *ChatView) renderToolCall(tc toolCallDisplay) string {
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
