package manager

import (
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDrainEvents_SegmentBreakBetweenToolRounds verifies that consecutive LLM
// iterations (text → tool call → text) are separated by a newline when the
// previous round's text doesn't end in one, so the reply doesn't fuse into a
// run-on line ("让我先搜索一下搜索结果是…").
func TestDrainEvents_SegmentBreakBetweenToolRounds(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "drain-seg-thread")

	ch := make(chan agent.AgentEvent, 16)
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "让我先搜索一下"}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "web_search", ToolID: "t1"}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallArgs, ToolName: "web_search", ToolID: "t1", ToolArgs: `{"q":"x"}`}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolResult, ToolName: "web_search", ToolResult: "ok"}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "搜索结果是：x"}
	close(ch)

	text, err := mgr.drainEvents(t.Context(), ch, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "让我先搜索一下\n搜索结果是：x", text)
}

// TestDrainEvents_NoDoubleBreakWhenEndsWithNewline verifies a round already
// ending in a newline doesn't get an extra one inserted.
func TestDrainEvents_NoDoubleBreakWhenEndsWithNewline(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "drain-nl-thread")

	ch := make(chan agent.AgentEvent, 16)
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "让我先搜索一下\n"}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "web_search", ToolID: "t1"}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "搜索结果是：x"}
	close(ch)

	text, err := mgr.drainEvents(t.Context(), ch, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "让我先搜索一下\n搜索结果是：x", text)
}

// TestDrainEvents_NoLeadingBreakWhenToolCalledFirst verifies a tool call made
// before any text (LLM went straight to tools) doesn't leave a stray leading
// newline in the reply.
func TestDrainEvents_NoLeadingBreakWhenToolCalledFirst(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "drain-first-tool-thread")

	ch := make(chan agent.AgentEvent, 16)
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "web_search", ToolID: "t1"}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "结果是 x"}
	close(ch)

	text, err := mgr.drainEvents(t.Context(), ch, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "结果是 x", text)
}

// TestDrainEvents_SegmentBreakAcrossMultipleRounds verifies every tool round
// boundary gets a newline.
func TestDrainEvents_SegmentBreakAcrossMultipleRounds(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "drain-multi-thread")

	ch := make(chan agent.AgentEvent, 16)
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "第一轮"}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "t1", ToolID: "t1"}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "第二轮"}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "t2", ToolID: "t2"}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "第三轮"}
	close(ch)

	text, err := mgr.drainEvents(t.Context(), ch, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "第一轮\n第二轮\n第三轮", text)
}

// TestDrainEvents_SegmentBreakOnSteerBoundary verifies the steer boundary
// also breaks the segment (ta == nil → steer is skipped, but the boundary
// still applies).
func TestDrainEvents_SegmentBreakOnSteerBoundary(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "drain-steer-thread")

	ch := make(chan agent.AgentEvent, 16)
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "前半"}
	ch <- agent.AgentEvent{Type: agent.AgentEventSteerCheck}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "后半"}
	close(ch)

	text, err := mgr.drainEvents(t.Context(), ch, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "前半\n后半", text)
}
