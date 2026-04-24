package tui

import (
	"strings"
	"testing"
)

// testListProvider 为单元测试提供固定 [ListItem] 切片。
type testListProvider struct {
	items []ListItem
}

func (p *testListProvider) ListLen() int { return len(p.items) }
func (p *testListProvider) ListItem(i int) ListItem { return p.items[i] }
func (p *testListProvider) ListItemHeight(i int) int { return p.items[i].Height }

func listFromLines(items ...string) *testListProvider {
	out := make([]ListItem, len(items))
	for i, s := range items {
		s = strings.TrimRight(s, "\n")
		h := 0
		if s != "" {
			h = strings.Count(s, "\n") + 1
		}
		out[i] = ListItem{Content: s, Height: h}
	}
	return &testListProvider{items: out}
}

func TestScrollList_Empty(t *testing.T) {
	p := &testListProvider{items: nil}
	l := NewScrollList(0)
	l.SetHeight(5)
	if got := l.Render(p); got != "" {
		t.Errorf("Render empty: got %q", got)
	}
	if !l.AtBottom(p) {
		t.Error("AtBottom: want true for len 0")
	}
}

func TestScrollList_Render_TwoItems_Gap0(t *testing.T) {
	p := listFromLines("a", "b")
	l := NewScrollList(0)
	l.SetHeight(2)
	// 顶部：两行刚好是第一、二条
	if got := l.Render(p); got != "a\nb" {
		t.Errorf("Render: got %q want a\\nb", got)
	}
}

func TestScrollList_Render_ViewportOneLine(t *testing.T) {
	p := listFromLines("a", "b")
	l := NewScrollList(0)
	l.SetHeight(1)
	if got := l.Render(p); got != "a" {
		t.Errorf("Render: got %q want a", got)
	}
}

func TestScrollList_ScrollToBottom_Partial(t *testing.T) {
	// 三条一行消息，视口 2 行，到底应显示 C 和 … 不，三行总高，视口2，最底是第2、3 行
	p := listFromLines("A", "B", "C")
	l := NewScrollList(0)
	l.SetHeight(2)
	l.ScrollToBottom(p)
	if !l.AtBottom(p) {
		t.Error("AtBottom after ScrollToBottom: want true")
	}
	got := l.Render(p)
	// 底部 2 行: B, C
	if got != "B\nC" {
		t.Errorf("Render at bottom: got %q want B\\nC", got)
	}
}

func TestScrollList_Gap1(t *testing.T) {
	p := listFromLines("a", "b")
	l := NewScrollList(1)
	l.SetHeight(3)
	// 第一行 a，空行（gap），第二行 b
	if got := l.Render(p); got != "a\n\nb" {
		t.Errorf("Render with gap: got %q", got)
	}
}

func TestScrollList_ScrollBy_UpFromBottom(t *testing.T) {
	p := listFromLines("A", "B", "C", "D")
	l := NewScrollList(0)
	l.SetHeight(2)
	l.ScrollToBottom(p)
	if !l.AtBottom(p) {
		t.Fatal("expected at bottom")
	}
	l.ScrollBy(p, -1) // 向上 1 行
	if l.AtBottom(p) {
		t.Error("AtBottom: want false after scroll up 1 from bottom")
	}
	// 视口 2 行，从底部上移 1：原先 B/C，上移后 A/B（取决于 offset）
	got := l.Render(p)
	// 手算: 4 行 A B C D，高 2，最底是 C、D。上移 1 行得 B、C
	if got != "B\nC" {
		t.Errorf("Render after scroll up 1: got %q want B\\nC", got)
	}
}

func TestScrollList_Reset(t *testing.T) {
	p := listFromLines("A", "B", "C")
	l := NewScrollList(0)
	l.SetHeight(1)
	l.ScrollToBottom(p)
	l.Reset()
	if got := l.Render(p); got != "A" {
		t.Errorf("Render after Reset: got %q want A", got)
	}
}

func TestScrollList_MultilineItem_Offset(t *testing.T) {
	// 一条多行，视口 1 行，先顶再滚到底
	p := listFromLines("line0\nline1\nline2")
	l := NewScrollList(0)
	l.SetHeight(1)
	if got := l.Render(p); got != "line0" {
		t.Errorf("top: got %q", got)
	}
	l.ScrollToBottom(p)
	if got := l.Render(p); got != "line2" {
		t.Errorf("bottom: got %q want line2", got)
	}
}

func TestScrollList_ScrollBy_DownAtBottomNoOp(t *testing.T) {
	p := listFromLines("a", "b")
	l := NewScrollList(0)
	l.SetHeight(1)
	l.ScrollToBottom(p)
	before := l.Render(p)
	l.ScrollBy(p, 3)
	after := l.Render(p)
	if before != after {
		t.Errorf("scroll down at bottom should not change: before %q after %q", before, after)
	}
}
