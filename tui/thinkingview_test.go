package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
)

func TestThinkingView_AppendAndViewString(t *testing.T) {
	tv := NewThinkingView()
	tv.SetSize(80, 20)

	if v := tv.ViewString(); v != "" {
		t.Errorf("initial ViewString should be empty, got %q", v)
	}

	tv.Append("Let me think about this...\n")
	tv.Append("First, I need to understand the problem.\n")
	tv.Append("Then I can propose a solution.\n")

	v := tv.ViewString()
	if v == "" {
		t.Fatal("ViewString should not be empty after appending content")
	}
	if !strings.Contains(v, "Let me think") {
		t.Errorf("ViewString should contain appended thinking, got: %s", v)
	}
	if !strings.Contains(v, "propose a solution") {
		t.Errorf("ViewString should contain all thinking content, got: %s", v)
	}
}

func TestThinkingView_EmptyAfterReset(t *testing.T) {
	tv := NewThinkingView()
	tv.SetSize(80, 20)

	tv.Append("some content")
	tv.Reset()

	if v := tv.ViewString(); v != "" {
		t.Errorf("ViewString should be empty after Reset, got %q", v)
	}
}

// Bug fix: Append before SetSize should not corrupt scroll position.
func TestThinkingView_AppendBeforeSetSize(t *testing.T) {
	tv := NewThinkingView()

	tv.Append("line one\n")
	tv.Append("line two\n")
	tv.Append("line three\n")

	tv.SetSize(80, 36)

	v := tv.ViewString()
	if v == "" {
		t.Fatal("ViewString should show content even when Append was called before SetSize")
	}
	if !strings.Contains(v, "line one") {
		t.Errorf("ViewString should contain all lines, got: %s", v)
	}
	if !strings.Contains(v, "line three") {
		t.Errorf("ViewString should contain all lines, got: %s", v)
	}
}

func TestThinkingView_ScrollToBottomOnAppend(t *testing.T) {
	tv := NewThinkingView()
	tv.SetSize(80, 5)

	for range 20 {
		tv.Append("line\n")
	}

	v := tv.ViewString()
	if v == "" {
		t.Fatal("ViewString should show last lines after ScrollToBottom")
	}
	if strings.Count(v, "\n") >= 5 {
		t.Errorf("ViewString should fit within height (5 lines), got %d lines",
			strings.Count(v, "\n")+1)
	}
}

func TestThinkingView_DuringStreaming(t *testing.T) {
	m := testModel()
	m.thinkingView = NewThinkingView()
	m.setState(stateStreaming)
	m.streamGen = 1
	ch := make(chan agent.AgentEvent)
	close(ch)
	m.eventCh = ch

	m.Update(agentEventMsg{
		event: agent.AgentEvent{
			Type:          agent.AgentEventThinkingDelta,
			ThinkingDelta: "I need to analyze the codebase carefully...\nLooking at the relevant files...\nFound the issue!",
		},
		gen: 1,
	})

	m.thinkingMode = true
	v := viewContent(m.View())

	if !strings.Contains(v, "analyze the codebase") {
		t.Errorf("thinking view should show thinking content when switched to during streaming, got:\n%s", v)
	}
}

// Test that the thinking view is reset when a tool call starts,
// so it only shows thinking from the current turn.
func TestThinkingView_ResetOnToolCallStart(t *testing.T) {
	m := testModel()
	m.thinkingView = NewThinkingView()
	m.setState(stateStreaming)
	m.streamGen = 1
	ch := make(chan agent.AgentEvent)
	close(ch)
	m.eventCh = ch

	// First, append some thinking (as if from turn 1)
	m.Update(agentEventMsg{
		event: agent.AgentEvent{
			Type:          agent.AgentEventThinkingDelta,
			ThinkingDelta: "Thinking from turn 1...\nLet me check the code.",
		},
		gen: 1,
	})

	if !strings.Contains(m.thinkingView.content, "turn 1") {
		t.Fatal("thinking view should contain turn 1 thinking")
	}

	// Now tool call starts — should reset the thinking view
	m.Update(agentEventMsg{
		event: agent.AgentEvent{
			Type:     agent.AgentEventToolCallStart,
			ToolName: "Bash",
			ToolID:   "tc_1",
		},
		gen: 1,
	})

	if m.thinkingView.content != "" {
		t.Errorf("thinking view should be empty after tool call start, got: %q", m.thinkingView.content)
	}
}

// Test word wrapping in the thinking view
func TestThinkingView_WordWrap(t *testing.T) {
	tv := NewThinkingView()
	// Narrow width forces wrapping
	tv.SetSize(30, 40)

	longLine := "This is a very long line of thinking that should be wrapped at word boundaries automatically"
	tv.Append(longLine)

	v := tv.ViewString()
	if v == "" {
		t.Fatal("ViewString should not be empty")
	}

	// The wrapped output should contain newlines (from wrapping)
	if !strings.Contains(v, "\n") {
		t.Errorf("long line should be word-wrapped (no newlines found), got:\n%s", v)
	}

	// The content should be complete (all words present)
	if !strings.Contains(v, "wrapped at word boundaries") {
		t.Errorf("wrapped content should contain all original words, got:\n%s", v)
	}
}

// Test that very wide terminals don't wrap short lines
func TestThinkingView_NoWrapWhenFits(t *testing.T) {
	tv := NewThinkingView()
	tv.SetSize(120, 20)

	shortLine := "Short line that fits"
	tv.Append(shortLine)

	// Use ListItem directly to check wrapping (ViewString pads to height)
	item := tv.ListItem(0)
	if item.Content == "" {
		t.Fatal("ListItem(0) should have content")
	}

	// Should not contain extra newlines that weren't in the original
	if strings.Count(item.Content, "\n") != 0 {
		t.Errorf("short line should not be wrapped, got:\n%s", item.Content)
	}
}

// Test that multiple lines with existing newlines are preserved
func TestThinkingView_MultiLinePreservation(t *testing.T) {
	tv := NewThinkingView()
	tv.SetSize(80, 20)

	tv.Append("Line one\nLine two\nLine three\n")

	v := tv.ViewString()
	if !strings.Contains(v, "Line one") || !strings.Contains(v, "Line two") || !strings.Contains(v, "Line three") {
		t.Errorf("all original lines should be present, got:\n%s", v)
	}
}

func viewContent(v tea.View) string {
	return v.Content
}
