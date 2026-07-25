package tui

import (
	"strings"
)

// ListItem 是一条已渲染的可见条目（无尾部换行）。
type ListItem struct {
	Content     string
	Height      int
	LineOffsets []int // 每行在 Content 中的字节起始偏移；用于 Render 快速跳跃到可见行
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
// 如果 item 提供了 LineOffsets，可直接跳跃到 `currentOffset` 位置，
// 避免逐字节扫描 item 的全部内容（对长 markdown 内容很关键）。
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
		i := 0

		// 利用 LineOffsets 跳过已滚过的行
		if off := item.LineOffsets; len(off) > currentOffset && currentOffset > 0 {
			i = off[currentOffset]
			lineNo = currentOffset
			lineStart = off[currentOffset]
		}

		for ; i <= len(content); i++ {
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

	// 归一化：lastLn 可能越过 item 内容行（包含了 gap 的部分），
	// 需要推进到下一个 item，与 ScrollBy 的跨 item 逻辑保持一致。
	for lastIdx < n-1 && lastLn >= p.ListItemHeight(lastIdx) {
		lastLn -= p.ListItemHeight(lastIdx)
		lastLn = max(0, lastLn-l.gap)
		lastIdx++
	}

	atBottom = l.scrollOffIdx > lastIdx || (l.scrollOffIdx == lastIdx && l.scrollOffLn >= lastLn)
	return atBottom, lastIdx, lastLn
}
