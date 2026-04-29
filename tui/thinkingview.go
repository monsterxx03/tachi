package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// ThinkingView is a full-height scrollable view that shows the complete,
// live thinking output while the agent is streaming. It replaces the chat
// view when the user presses Ctrl+O during streaming.
type ThinkingView struct {
	list       ScrollList
	width      int
	height     int
	content    string
	all        strings.Builder // complete thinking text accumulated
	userScrolled bool
}

func NewThinkingView() ThinkingView {
	return ThinkingView{
		list: NewScrollList(0),
	}
}

func (tv *ThinkingView) SetSize(w, h int) {
	tv.width = w
	tv.height = h
	tv.list.SetHeight(h)
	tv.refresh()
}

func (tv *ThinkingView) Reset() {
	tv.all.Reset()
	tv.content = ""
	tv.userScrolled = false
	tv.list.Reset()
}

// Append appends a delta to the live thinking output.
func (tv *ThinkingView) Append(s string) {
	tv.all.WriteString(s)
	tv.content = tv.all.String()
	if !tv.userScrolled {
		tv.list.ScrollToBottom(tv)
	}
}

func (tv *ThinkingView) Update(msg tea.Msg) (ThinkingView, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			tv.list.ScrollBy(tv, -chatMouseScrollLines)
		case tea.MouseWheelDown:
			tv.list.ScrollBy(tv, chatMouseScrollLines)
		default:
			return *tv, nil
		}
		tv.userScrolled = !tv.list.AtBottom(tv)
		tv.refresh()
	case tea.KeyMsg:
		s := msg.String()
		switch s {
		case "pgup":
			tv.list.ScrollBy(tv, -max(tv.list.Height()/2, 1))
			tv.userScrolled = !tv.list.AtBottom(tv)
			tv.refresh()
		case "pgdown":
			tv.list.ScrollBy(tv, max(tv.list.Height()/2, 1))
			tv.userScrolled = !tv.list.AtBottom(tv)
			tv.refresh()
		case "ctrl+u":
			tv.list.ScrollBy(tv, -chatMouseScrollLines)
			tv.userScrolled = !tv.list.AtBottom(tv)
			tv.refresh()
		case "ctrl+d":
			tv.list.ScrollBy(tv, chatMouseScrollLines)
			tv.userScrolled = !tv.list.AtBottom(tv)
			tv.refresh()
		}
	}
	return *tv, nil
}

// ViewString returns the visible portion of the thinking content, padded to
// fill the component height.
func (tv *ThinkingView) ViewString() string {
	rendered := tv.list.Render(tv)
	if rendered == "" {
		return ""
	}

	if tv.height <= 0 {
		return rendered
	}

	lines := strings.Count(rendered, "\n") + 1
	if rendered == "" {
		lines = 0
	}
	if lines >= tv.height {
		return rendered
	}

	var b strings.Builder
	b.Grow(len(rendered) + tv.height)
	b.WriteString(rendered)
	for range tv.height - lines {
		b.WriteByte('\n')
	}
	return b.String()
}

// ListItemProvider 实现

func (tv *ThinkingView) ListLen() int {
	if tv.content == "" {
		return 0
	}
	return 1
}

func (tv *ThinkingView) ListItem(idx int) ListItem {
	if idx != 0 || tv.content == "" {
		return ListItem{}
	}
	// Split into lines so the scroll list can scroll within the block.
	// We need to count lines ourselves since the scroll list works
	// line-by-line.
	s := tv.content
	return ListItem{Content: s, Height: strings.Count(s, "\n") + 1}
}

func (tv *ThinkingView) ListItemHeight(idx int) int {
	if idx != 0 || tv.content == "" {
		return 0
	}
	return strings.Count(tv.content, "\n") + 1
}

func (tv *ThinkingView) refresh() {
	tv.content = tv.all.String()
}
