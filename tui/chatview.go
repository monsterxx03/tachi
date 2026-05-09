package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/glamour"

	"github.com/monsterxx03/tachi/session"
)

const chatMouseScrollLines = 5

// messageCacheItem is one committed chat row with a width-keyed render cache.
type messageCacheItem struct {
	msg          chatMessage
	cached       string
	cachedHeight int
	innerW       int
}

func (m *messageCacheItem) clearCache() {
	m.cached = ""
	m.cachedHeight = 0
	m.innerW = 0
}

type ChatView struct {
	list    ScrollList
	items   []*messageCacheItem
	width   int
	height  int
	content string

	streaming       bool
	currentText     strings.Builder
	currentThinking strings.Builder
	currentTools    []toolCallDisplay

	mdRenderer    *glamour.TermRenderer
	mdRenderWidth int
	userScrolled  bool
}

func NewChatView() ChatView {
	md, _ := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(80),
	)
	return ChatView{
		mdRenderer: md,
		list:       NewScrollList(1),
	}
}

func (c *ChatView) SetSize(w, h int) {
	c.width = w
	c.height = h
	c.list.SetHeight(h)

	newWrapWidth := w - 4
	if newWrapWidth != c.mdRenderWidth {
		md, _ := glamour.NewTermRenderer(
			glamour.WithAutoStyle(),
			glamour.WithWordWrap(newWrapWidth),
		)
		c.mdRenderer = md
		c.mdRenderWidth = newWrapWidth
	}
	c.invalidateAllCaches()
	c.refresh()
}

func (c *ChatView) SetStreaming(streaming bool) { c.streaming = streaming }

func (c *ChatView) AddMessage(msg chatMessage) {
	c.items = append(c.items, &messageCacheItem{msg: msg})
	c.userScrolled = false
	c.refresh()
}

func (c *ChatView) AppendTextDelta(s string) {
	if c.hasCompletedTools() {
		c.flushTurn()
	}
	c.currentText.WriteString(s)
	c.refresh()
}

func (c *ChatView) AppendThinkingDelta(s string) {
	if c.hasCompletedTools() {
		c.flushTurn()
	}
	c.currentThinking.WriteString(s)
	c.refresh()
}

func (c *ChatView) AddToolCall(name, id string) {
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

// MarkSubagent flags a tool call display as being a subagent invocation.
// This adds a visual indicator in the TUI.
func (c *ChatView) MarkSubagent(id string) {
	for i := range c.currentTools {
		if c.currentTools[i].ID == id {
			c.currentTools[i].IsSubagent = true
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
		c.items = append(c.items, &messageCacheItem{msg: chatMessage{
			Role:    "thinking",
			Content: c.currentThinking.String(),
		}})
		c.currentThinking.Reset()
	}

	if c.currentText.Len() > 0 {
		c.items = append(c.items, &messageCacheItem{msg: chatMessage{
			Role:    "assistant",
			Content: c.currentText.String(),
		}})
		c.currentText.Reset()
	}

	for _, tc := range c.currentTools {
		c.items = append(c.items, &messageCacheItem{msg: chatMessage{
			Role:    "tool_calls",
			Content: c.renderToolCall(tc),
		}})
	}
	c.currentTools = nil
}

func (c *ChatView) ResetStreaming() {
	c.currentText.Reset()
	c.currentThinking.Reset()
	c.currentTools = nil
}

func (c *ChatView) Clear() {
	c.items = nil
	c.ResetStreaming()
	c.userScrolled = false
	c.list.Reset()
	c.refresh()
}

// LoadHistory loads session messages into the chat view for display when
// resuming a previous session. Tool calls are paired with their results.
func (c *ChatView) LoadHistory(sessionMsgs []session.Message) {
	// First pass: index tool results by ToolCallID
	toolResults := make(map[string]session.Message)
	for _, msg := range sessionMsgs {
		if msg.Type == session.MessageTypeToolResult {
			toolResults[msg.ToolCallID] = msg
		}
	}

	for _, msg := range sessionMsgs {
		switch msg.Type {
		case session.MessageTypeUser:
			c.items = append(c.items, &messageCacheItem{msg: chatMessage{
				Role: "user", Content: msg.Content,
			}})
		case session.MessageTypeAssistant:
			c.items = append(c.items, &messageCacheItem{msg: chatMessage{
				Role: "assistant", Content: msg.Content,
			}})
		case session.MessageTypeThinking:
			c.items = append(c.items, &messageCacheItem{msg: chatMessage{
				Role: "thinking", Content: msg.Content,
			}})
		case session.MessageTypeToolCall:
			argsStr := convertSessionArgsToString(msg.Args)
			result, hasResult := toolResults[msg.ToolCallID]
			tc := toolCallDisplay{
				Name:    msg.Name,
				ID:      msg.ToolCallID,
				Args:    argsStr,
				Preview: getToolArgsPreview(msg.Name, argsStr),
				Done:    hasResult,
			}
			if hasResult {
				tc.Result = result.Result
				tc.IsError = result.IsError
			}
			c.items = append(c.items, &messageCacheItem{msg: chatMessage{
				Role: "tool_calls", Content: c.renderToolCall(tc),
			}})
		case session.MessageTypeToolResult, session.MessageTypeConfirm:
			// Tool results are rendered inline with their tool call; confirm is UI-only
		}
	}

	c.userScrolled = false
	c.refresh()
}

// convertSessionArgsToString converts the Args field from session (stored as any)
// to a JSON string for display.
func convertSessionArgsToString(args any) string {
	if args == nil {
		return "{}"
	}
	switch v := args.(type) {
	case string:
		return v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return "{}"
		}
		return string(data)
	}
}

func (c ChatView) Update(msg tea.Msg) (ChatView, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			c.list.ScrollBy(&c, -chatMouseScrollLines)
		case tea.MouseWheelDown:
			c.list.ScrollBy(&c, chatMouseScrollLines)
		default:
			return c, nil
		}
		c.userScrolled = !c.list.AtBottom(&c)
		c.refresh()
	case tea.KeyMsg:
		s := msg.String()
		switch s {
		case "pgup":
			c.list.ScrollBy(&c, -max(c.list.Height()/2, 1))
			c.userScrolled = !c.list.AtBottom(&c)
			c.refresh()
		case "pgdown":
			c.list.ScrollBy(&c, max(c.list.Height()/2, 1))
			c.userScrolled = !c.list.AtBottom(&c)
			c.refresh()
		case "ctrl+u":
			c.list.ScrollBy(&c, -chatMouseScrollLines)
			c.userScrolled = !c.list.AtBottom(&c)
			c.refresh()
		case "ctrl+d":
			c.list.ScrollBy(&c, chatMouseScrollLines)
			c.userScrolled = !c.list.AtBottom(&c)
			c.refresh()
		}
	}
	return c, nil
}

func (c ChatView) View() string {
	if c.height <= 0 {
		return c.content
	}
	lines := strings.Count(c.content, "\n") + 1
	if c.content == "" {
		lines = 0
	}
	if lines >= c.height {
		return c.content
	}
	var b strings.Builder
	b.Grow(len(c.content) + c.height)
	b.WriteString(c.content)
	for range c.height - lines {
		b.WriteByte('\n')
	}
	return b.String()
}

// ListItemProvider 实现

func (c *ChatView) ListLen() int {
	n := len(c.items)
	if c.streamVisible() {
		n++
	}
	return n
}

func (c *ChatView) ListItem(idx int) ListItem {
	n := c.ListLen()
	if idx < 0 || idx >= n {
		return ListItem{}
	}
	if idx < len(c.items) {
		s, h := c.renderItemCached(c.items[idx])
		return ListItem{Content: s, Height: h}
	}
	s := strings.TrimRight(c.renderStreamBlock(), "\n")
	if s == "" {
		return ListItem{}
	}
	return ListItem{Content: s, Height: strings.Count(s, "\n") + 1}
}

func (c *ChatView) ListItemHeight(idx int) int {
	if idx < len(c.items) {
		_, h := c.renderItemCached(c.items[idx])
		return h
	}
	return c.ListItem(idx).Height
}

func (c *ChatView) streamVisible() bool {
	if !c.streaming {
		return false
	}
	if c.currentThinking.Len() > 0 {
		return true
	}
	if c.currentText.Len() > 0 {
		return true
	}
	return len(c.currentTools) > 0
}

func (c *ChatView) renderItemCached(m *messageCacheItem) (string, int) {
	inner := c.width - 2
	if inner < 1 {
		inner = 1
	}
	if m.cached != "" && m.innerW == inner {
		return m.cached, m.cachedHeight
	}
	s := strings.TrimRight(c.renderMessageContent(m.msg, inner), "\n")
	h := 0
	if s != "" {
		h = strings.Count(s, "\n") + 1
	}
	m.cached = s
	m.cachedHeight = h
	m.innerW = inner
	return s, h
}

func (c *ChatView) renderMessageContent(msg chatMessage, inner int) string {
	switch msg.Role {
	case "user":
		return userMsgStyle.Width(inner).Render(msg.Content)
	case "assistant":
		rendered := c.renderMarkdown(msg.Content)
		return assistantMsgStyle.Width(inner).Render(rendered)
	case "thinking":
		thinking := truncateThinking(msg.Content, 5)
		return thinkingStyle.Render("Thinking: " + thinking)
	case "tool_calls":
		return msg.Content
	case "error":
		return toolResultErrStyle.Width(inner).Render("Error: " + msg.Content)
	case "tool_confirmation":
		return toolConfirmStyle.Width(inner).Render(renderDiffWithHighlight(msg.Content, inner))
	default:
		return msg.Content
	}
}

func (c *ChatView) renderStreamBlock() string {
	inner := c.width - 2
	if inner < 1 {
		inner = 1
	}
	var b strings.Builder
	if c.currentThinking.Len() > 0 {
		thinking := truncateThinking(c.currentThinking.String(), 5)
		b.WriteString(thinkingStyle.Render("Thinking: " + thinking))
		b.WriteString("\n")
	}
	if c.currentText.Len() > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(assistantMsgStyle.Width(inner).Render(c.currentText.String()))
	}
	for _, tc := range c.currentTools {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(c.renderToolCall(tc))
	}
	return b.String()
}

func (c *ChatView) invalidateAllCaches() {
	for _, it := range c.items {
		if it != nil {
			it.clearCache()
		}
	}
}

func (c *ChatView) refresh() {
	if !c.userScrolled {
		c.list.ScrollToBottom(c)
	}
	c.content = c.list.Render(c)
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

	nameTag := tc.Name
	if tc.IsSubagent {
		nameTag = tc.Name + " ⊞" // subagent indicator
	}

	if tc.Done {
		if tc.IsError {
			fmt.Fprintf(&b, "%s %s(%s)\n",
				toolResultErrStyle.Render("x"),
				toolCallStyle.Render(nameTag),
				dimStyle.Render(preview))
			fmt.Fprintf(&b, "  %s", toolResultErrStyle.Render(truncate(tc.Result, 200)))
		} else {
			fmt.Fprintf(&b, "%s %s(%s)\n",
				toolResultOKStyle.Render("v"),
				toolCallStyle.Render(nameTag),
				dimStyle.Render(preview))
			fmt.Fprintf(&b, "  %s", dimStyle.Render(truncate(tc.Result, 200)))
		}
	} else {
		spinnerChar := "~"
		if tc.IsSubagent {
			spinnerChar = "⊡" // running subagent indicator
		}
		fmt.Fprintf(&b, "%s %s(%s)",
			toolCallStyle.Render(spinnerChar),
			toolCallStyle.Render(nameTag),
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

// truncateThinking collapses a thinking block to show only the last N lines,
// with a "… (N more lines)" indicator when truncation occurs.
// This keeps the chat view readable when thinking blocks are very long.
func truncateThinking(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	hidden := len(lines) - maxLines
	lastLines := strings.Join(lines[hidden:], "\n")
	return fmt.Sprintf("… (%d more lines)", hidden) + "\n" + lastLines
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
		if p, ok := args["path"].(string); ok {
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
		if cmd, ok := args["command"].(string); ok {
			runes := []rune(cmd)
			if len(runes) > 60 {
				return string(runes[:57]) + "…"
			}
			return cmd
		}
	case "WebSearch":
		if q, ok := args["query"].(string); ok {
			runes := []rune(q)
			if len(runes) > 60 {
				return string(runes[:57]) + "…"
			}
			return q
		}
	case "WebFetch":
		if u, ok := args["url"].(string); ok {
			runes := []rune(u)
			if len(runes) > 60 {
				return "WebFetch: " + string(runes[:57]) + "…"
			}
			return "WebFetch: " + u
		}
	case "SubAgent":
		prompt, _ := args["prompt"].(string)
		branch, _ := args["worktree_branch"].(string)
		if prompt == "" {
			return argsJSON
		}
		runes := []rune(prompt)
		if len(runes) > 60 {
			prompt = string(runes[:57]) + "…"
		}
		if branch != "" {
			return fmt.Sprintf("SubAgent [%s]: %s", branch, prompt)
		}
		return prompt
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
			b.WriteString(diffDeletedStyle.Render(line))
			b.WriteString("\n")
		case '+':
			b.WriteString(diffAddedStyle.Render(line))
			b.WriteString("\n")
		case '@':
			b.WriteString(diffHeaderStyle.Render(line))
			b.WriteString("\n")
		default:
			b.WriteString(diffContextStyle.Render(line))
			b.WriteString("\n")
		}
	}
	return b.String()
}
