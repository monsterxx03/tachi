package tui

import (
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
	tea "charm.land/bubbletea/v2"
)

// ThinkingView is a full-height scrollable view that shows the live thinking
// output of the current turn. It replaces the chat view when the user presses
// Ctrl+O during streaming. After each tool call boundary, the view is reset
// so it only shows in-progress thinking, not historical content.
type ThinkingView struct {
	list         ScrollList
	width        int
	height       int
	content      string
	all          strings.Builder // complete thinking text accumulated
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
	if !tv.userScrolled {
		tv.list.ScrollToBottom(tv)
	}
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
	// Wrap text to the available width so long lines don't overflow.
	wrapped := tv.wrapContent(tv.content)
	return ListItem{Content: wrapped, Height: strings.Count(wrapped, "\n") + 1}
}

func (tv *ThinkingView) ListItemHeight(idx int) int {
	if idx != 0 || tv.content == "" {
		return 0
	}
	return tv.wrappedLineCount(tv.content)
}

func (tv *ThinkingView) refresh() {
	tv.content = tv.all.String()
}

// wrapContent wraps text to tv.width - 2 characters (gutter), measuring
// display width via runewidth for CJK support.
func (tv *ThinkingView) wrapContent(text string) string {
	w := tv.width - 2 // gutter
	if w < 1 {
		w = 1
	}
	return wordWrap(text, w)
}

// wrappedLineCount returns the number of lines after wrapping text.
func (tv *ThinkingView) wrappedLineCount(text string) int {
	w := tv.width - 2
	if w < 1 {
		w = 1
	}
	return wordWrapLineCount(text, w)
}

// wordWrap wraps text at word boundaries to fit within maxWidth display
// columns, using runewidth to correctly measure CJK and wide characters.
func wordWrap(text string, maxWidth int) string {
	if maxWidth <= 0 || text == "" {
		return text
	}

	var out strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(wrapLineToWidth(line, maxWidth))
	}
	return out.String()
}

// wrapLineToWidth wraps a single line (no embedded newlines) to maxWidth.
// Uses greedy word-wrap: accumulate words until they exceed the width,
// then break at the preceding space.
func wrapLineToWidth(line string, maxWidth int) string {
	if runewidth.StringWidth(line) <= maxWidth {
		return line
	}

	var out strings.Builder
	var seg strings.Builder
	segWidth := 0

	flush := func(forceBreak bool) {
		if seg.Len() == 0 {
			return
		}
		s := seg.String()
		w := runewidth.StringWidth(s)
		if segWidth > 0 {
			w++ // space before this segment
		}
		if segWidth > 0 && segWidth+w > maxWidth || forceBreak {
			out.WriteByte('\n')
			out.WriteString(s)
			segWidth = runewidth.StringWidth(s)
		} else {
			if segWidth > 0 {
				out.WriteByte(' ')
				segWidth++
			}
			out.WriteString(s)
			segWidth += runewidth.StringWidth(s)
		}
		seg.Reset()
	}

	for _, r := range line {
		if unicode.IsSpace(r) {
			flush(false)
		} else {
			rw := runewidth.RuneWidth(r)
			if segWidth > 0 && segWidth+1+rw > maxWidth {
				flush(true)
			}
			seg.WriteRune(r)
		}
	}
	flush(false)

	return out.String()
}

// wordWrapLineCount estimates the number of lines after word wrapping.
func wordWrapLineCount(text string, maxWidth int) int {
	if maxWidth <= 0 || text == "" {
		return strings.Count(text, "\n") + 1
	}

	n := 0
	for _, line := range strings.Split(text, "\n") {
		w := runewidth.StringWidth(line)
		if w <= maxWidth || line == "" {
			n++
			continue
		}

		// Rough estimate: each segment is at most maxWidth chars
		n += (w + maxWidth - 1) / maxWidth
	}
	return n
}
