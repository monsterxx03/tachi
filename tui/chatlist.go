package tui

import (
	"strings"
)

// ListItem 是一条已渲染的可见条目（无尾部换行）。
type ListItem struct {
	Content string
	Height  int
}

// ListItemProvider 由外部实现，向 ScrollList 提供条目数量与内容。
type ListItemProvider interface {
	ListLen() int
	ListItem(idx int) ListItem
	ListItemHeight(idx int) int
}

// ScrollList 管理基于行的虚拟滚动，不持有业务数据，通过调用方传入的 ListItemProvider 读取条目。
type ScrollList struct {
	gap          int
	height       int
	scrollOffIdx int
	scrollOffLn  int
}

func NewScrollList(gap int) ScrollList {
	return ScrollList{gap: gap}
}

func (l *ScrollList) SetHeight(h int) { l.height = h }
func (l *ScrollList) Height() int     { return l.height }

// AtBottom 返回当前滚动位置是否已在最底部。
func (l *ScrollList) AtBottom(p ListItemProvider) bool {
	ab, _, _ := l.bottomOffset(p)
	return ab
}

// ScrollToBottom 滚动到最底部。
func (l *ScrollList) ScrollToBottom(p ListItemProvider) {
	if p.ListLen() == 0 {
		return
	}
	_, idx, ln := l.bottomOffset(p)
	l.scrollOffIdx = idx
	l.scrollOffLn = ln
}

// ScrollToTop 滚动到最顶部。
func (l *ScrollList) ScrollToTop() {
	l.scrollOffIdx = 0
	l.scrollOffLn = 0
}

// ScrollBy 按行滚动，正数向下，负数向上。
func (l *ScrollList) ScrollBy(p ListItemProvider, lines int) {
	n := p.ListLen()
	if n == 0 || lines == 0 {
		return
	}
	if lines > 0 {
		atBottom, lastIdx, lastLn := l.bottomOffset(p)
		if atBottom {
			return
		}
		l.scrollOffLn += lines
		for l.scrollOffLn >= p.ListItemHeight(l.scrollOffIdx) {
			l.scrollOffLn -= p.ListItemHeight(l.scrollOffIdx)
			l.scrollOffLn = max(0, l.scrollOffLn-l.gap)
			l.scrollOffIdx++
			if l.scrollOffIdx > n-1 {
				l.scrollOffIdx = lastIdx
				l.scrollOffLn = lastLn
				return
			}
		}
		if l.scrollOffIdx > lastIdx || (l.scrollOffIdx == lastIdx && l.scrollOffLn > lastLn) {
			l.scrollOffIdx = lastIdx
			l.scrollOffLn = lastLn
		}
	} else {
		l.scrollOffLn += lines
		for l.scrollOffLn < 0 {
			l.scrollOffIdx--
			if l.scrollOffIdx < 0 {
				l.ScrollToTop()
				return
			}
			l.scrollOffLn += p.ListItemHeight(l.scrollOffIdx) + l.gap
		}
	}
}

// Reset 清空滚动位置。
func (l *ScrollList) Reset() {
	l.scrollOffIdx = 0
	l.scrollOffLn = 0
}

// Render 返回当前可见区域的文本（恰好 height 行，用 \n 连接）。
func (l *ScrollList) Render(p ListItemProvider) string {
	n := p.ListLen()
	if n == 0 {
		return ""
	}

	var b strings.Builder
	currentIdx := l.scrollOffIdx
	currentOffset := l.scrollOffLn
	linesWritten := 0

	for linesWritten < l.height && currentIdx < n {
		item := p.ListItem(currentIdx)
		content := item.Content
		lineStart := 0
		lineNo := 0
		for i := 0; i <= len(content); i++ {
			if i == len(content) || content[i] == '\n' {
				if lineNo >= currentOffset && linesWritten < l.height {
					if linesWritten > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(content[lineStart:i])
					linesWritten++
				}
				lineNo++
				lineStart = i + 1
			}
		}
		for g := 0; g < l.gap && linesWritten < l.height; g++ {
			if linesWritten > 0 {
				b.WriteByte('\n')
			}
			linesWritten++
		}
		currentIdx++
		currentOffset = 0
	}

	return b.String()
}

func (l *ScrollList) bottomOffset(p ListItemProvider) (atBottom bool, lastIdx int, lastLn int) {
	n := p.ListLen()
	if n == 0 {
		return true, 0, 0
	}

	var totalHeight int
	var idx int
	for idx = n - 1; idx >= 0; idx-- {
		h := p.ListItemHeight(idx)
		if idx < n-1 {
			h += l.gap
		}
		totalHeight += h
		if totalHeight > l.height {
			break
		}
	}
	lastIdx = max(idx, 0)
	lastLn = max(totalHeight-l.height, 0)

	atBottom = l.scrollOffIdx > lastIdx || (l.scrollOffIdx == lastIdx && l.scrollOffLn >= lastLn)
	return atBottom, lastIdx, lastLn
}
