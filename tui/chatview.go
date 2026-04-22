package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
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
	renderedCache string
	userScrolled  bool
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
	c.userScrolled = false
	c.invalidateCache()
	c.refresh()
}

func (c *ChatView) AppendTextDelta(s string) {
	c.state = stateStreaming
	if c.hasCompletedTools() {
		c.flushTurn()
	}
	c.currentText.WriteString(s)
	c.refresh()
}

func (c *ChatView) AppendThinkingDelta(s string) {
	c.state = stateStreaming
	if c.hasCompletedTools() {
		c.flushTurn()
	}
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
			c.currentTools[i].Preview = getToolArgsPreview(c.currentTools[i].Name, args)
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
	c.flushTurn()
	c.userScrolled = false
	c.refresh()
}

func (c *ChatView) hasCompletedTools() bool {
	if len(c.currentTools) == 0 {
		return false
	}
	for _, tc := range c.currentTools {
		if !tc.Done {
			return false
		}
	}
	return true
}

func (c *ChatView) flushTurn() {
	if c.currentThinking.Len() == 0 && c.currentText.Len() == 0 && len(c.currentTools) == 0 {
		return
	}

	if c.currentThinking.Len() > 0 {
		c.messages = append(c.messages, chatMessage{
			Role:    "thinking",
			Content: c.currentThinking.String(),
		})
		c.currentThinking.Reset()
	}

	if c.currentText.Len() > 0 {
		c.messages = append(c.messages, chatMessage{
			Role:    "assistant",
			Content: c.currentText.String(),
		})
		c.currentText.Reset()
	}

	for _, tc := range c.currentTools {
		c.messages = append(c.messages, chatMessage{
			Role:    "tool_calls",
			Content: c.renderToolCall(tc),
		})
	}
	c.currentTools = nil

	c.invalidateCache()
}

func (c *ChatView) ResetStreaming() {
	c.currentText.Reset()
	c.currentThinking.Reset()
	c.currentTools = nil
}

func (c *ChatView) Clear() {
	c.messages = nil
	c.ResetStreaming()
	c.userScrolled = false
	c.invalidateCache()
	c.refresh()
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
		c.userScrolled = !c.viewport.AtBottom()
	case tea.KeyMsg:
		var cmd tea.Cmd
		c.viewport, cmd = c.viewport.Update(msg)
		cmds = append(cmds, cmd)
		c.userScrolled = !c.viewport.AtBottom()
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
	c.renderedCache = ""
}

func (c *ChatView) rebuildCache() {
	var b strings.Builder
	for _, msg := range c.messages {
		c.renderMessageTo(&b, msg)
	}
	c.renderedCache = b.String()
}

func (c *ChatView) renderMessageTo(b *strings.Builder, msg chatMessage) {
	inner := c.width - 2
	if inner < 1 {
		inner = 1
	}
	switch msg.Role {
	case "user":
		fmt.Fprintf(b, "%s\n\n", userMsgStyle.Width(inner).Render(msg.Content))
	case "assistant":
		rendered := c.renderMarkdown(msg.Content)
		fmt.Fprintf(b, "%s\n\n", assistantMsgStyle.Width(inner).Render(rendered))
	case "thinking":
		thinking := truncateThinking(msg.Content, 500)
		fmt.Fprintf(b, "%s\n\n",
			thinkingStyle.Render("Thinking: "+thinking))
	case "tool_calls":
		fmt.Fprintf(b, "%s\n\n", msg.Content)
	case "tool_confirmation":
		fmt.Fprintf(b, "%s\n\n", toolConfirmStyle.Width(inner).Render(renderDiffWithHighlight(msg.Content, inner)))
	}
}

func (c *ChatView) refresh() {
	if c.renderedCache == "" && len(c.messages) > 0 {
		c.rebuildCache()
	}

	var b strings.Builder
	b.WriteString(c.renderedCache)

	inner := c.width - 2
	if inner < 1 {
		inner = 1
	}
	if c.state == stateWaiting {
		fmt.Fprintf(&b, "%s\n", assistantMsgStyle.Width(inner).Render(c.spinner.View()))
	} else if c.state == stateStreaming {
		if c.currentThinking.Len() > 0 {
			thinking := truncateThinking(c.currentThinking.String(), 500)
			fmt.Fprintf(&b, "%s\n", thinkingStyle.Render("Thinking: "+thinking))
		}
		if c.currentText.Len() > 0 {
			fmt.Fprintf(&b, "%s\n", assistantMsgStyle.Width(inner).Render(c.currentText.String()))
		}
		for _, tc := range c.currentTools {
			fmt.Fprintf(&b, "%s\n", c.renderToolCall(tc))
		}
	}

	content := lipgloss.NewStyle().Width(c.width).Render(b.String())
	c.viewport.SetContent(content)
	if !c.userScrolled {
		c.viewport.GotoBottom()
	}
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
	preview := tc.Preview
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

func truncateThinking(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	half := (maxLen - 5) / 2
	return s[:half] + "\n...\n" + s[len(s)-half:]
}

func getToolArgsPreview(name, argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return argsJSON
	}
	switch name {
	case "ReadFile":
		if p, ok := args["path"].(string); ok {
			return p
		}
	case "WriteFile":
		if p, ok := args["path"].(string); ok {
			return p
		}
	case "EditFile":
		if p, ok := args["file_path"].(string); ok {
			return p
		}
	case "Glob":
		if p, ok := args["pattern"].(string); ok {
			return p
		}
	case "Grep":
		if p, ok := args["pattern"].(string); ok {
			return p
		}
	case "Bash":
		if c, ok := args["command"].(string); ok {
			if len(c) > 60 {
				return c[:60] + "..."
			}
			return c
		}
	case "WebSearch":
		if q, ok := args["query"].(string); ok {
			if len(q) > 60 {
				return q[:60] + "..."
			}
			return q
		}
	}
	return argsJSON
}

// renderDiffWithHighlight renders diff content with syntax highlighting for +/- lines
func renderDiffWithHighlight(content string, width int) string {
	lines := strings.Split(content, "\n")
	var b strings.Builder

	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case '-':
			// Deleted line
			b.WriteString(diffDeletedStyle.Render(line))
			b.WriteString("\n")
		case '+':
			// Added line
			b.WriteString(diffAddedStyle.Render(line))
			b.WriteString("\n")
		case '@':
			// Diff header (@@ -x,y +x,y @@)
			b.WriteString(diffHeaderStyle.Render(line))
			b.WriteString("\n")
		default:
			// Context line
			b.WriteString(diffContextStyle.Render(line))
			b.WriteString("\n")
		}
	}
	return b.String()
}
