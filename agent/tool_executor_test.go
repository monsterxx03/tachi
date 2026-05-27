package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Helper: multi-tool-call sequence builder ----

// multiToolCallSeq builds a single stream sequence that emits multiple tool calls
// in one response (simulating parallel tool calls from the LLM).
func multiToolCallSeq(calls ...struct{ Name, ID, Args string }) []llm.StreamEvent {
	var events []llm.StreamEvent
	for idx, c := range calls {
		events = append(events, llm.StreamEvent{
			Type:      llm.StreamEventToolUseStart,
			ToolIndex: idx,
			ToolCall:  &llm.ToolCall{ID: c.ID, Type: "function", Function: llm.ToolCallFunction{Name: c.Name}},
		})
		events = append(events, llm.StreamEvent{
			Type:       llm.StreamEventInputJSONDelta,
			ToolIndex:  idx,
			InputDelta: c.Args,
		})
	}
	events = append(events, llm.StreamEvent{
		Type:         llm.StreamEventDone,
		FinishReason: "tool_calls",
		Usage:        &llm.Usage{InputTokens: 40, OutputTokens: 20},
	})
	return events
}

// ---- Stub helpers ----

// errorStub returns a Tool that always errors.
func errorStub() *stubTool {
	return &stubTool{
		name:     "ErrorTool",
		desc:     "Always errors",
		parallel: true,
		executeFn: func(ctx context.Context, args string) (string, error) {
			var m map[string]any
			json.Unmarshal([]byte(args), &m)
			if msg, ok := m["msg"]; ok {
				return "", fmt.Errorf("%s", msg)
			}
			return "", fmt.Errorf("error")
		},
	}
}

func slowStub(name string) *stubTool {
	return &stubTool{name: name, parallel: true, executeFn: func(ctx context.Context, args string) (string, error) {
		return name + ":done", nil
	}}
}

func seqStub(name string) *stubTool {
	return &stubTool{name: name, parallel: false, executeFn: func(ctx context.Context, args string) (string, error) {
		return name + ":done", nil
	}}
}

// ---- groupToolCalls Tests ----

func TestGroupToolCalls_AllParallel(t *testing.T) {
	a := newTestAgent(nil)
	a.RegisterTool(slowStub("Read"))
	a.RegisterTool(slowStub("Grep"))
	a.RegisterTool(slowStub("Glob"))

	toolCalls := []llm.ToolCall{
		{ID: "c1", Type: "function", Function: llm.ToolCallFunction{Name: "Read", Arguments: `{"path":"a"}`}},
		{ID: "c2", Type: "function", Function: llm.ToolCallFunction{Name: "Grep", Arguments: `{"pattern":"x"}`}},
		{ID: "c3", Type: "function", Function: llm.ToolCallFunction{Name: "Glob", Arguments: `{"pattern":"*.go"}`}},
	}

	groups := a.groupToolCalls(toolCalls)
	require.Len(t, groups, 1)
	assert.True(t, groups[0].parallel)
	assert.Len(t, groups[0].calls, 3)
}

func TestGroupToolCalls_AllSequential(t *testing.T) {
	a := newTestAgent(nil)
	a.RegisterTool(seqStub("EditFile"))
	a.RegisterTool(seqStub("Bash"))

	toolCalls := []llm.ToolCall{
		{ID: "c1", Type: "function", Function: llm.ToolCallFunction{Name: "EditFile", Arguments: "{}"}},
		{ID: "c2", Type: "function", Function: llm.ToolCallFunction{Name: "Bash", Arguments: "{}"}},
	}

	groups := a.groupToolCalls(toolCalls)
	require.Len(t, groups, 2)
	assert.False(t, groups[0].parallel)
	assert.Len(t, groups[0].calls, 1)
	assert.False(t, groups[1].parallel)
	assert.Len(t, groups[1].calls, 1)
}

func TestGroupToolCalls_Mixed(t *testing.T) {
	a := newTestAgent(nil)
	a.RegisterTool(slowStub("Read"))
	a.RegisterTool(slowStub("Grep"))
	a.RegisterTool(seqStub("EditFile"))
	a.RegisterTool(slowStub("Glob"))

	toolCalls := []llm.ToolCall{
		{ID: "c1", Type: "function", Function: llm.ToolCallFunction{Name: "Read", Arguments: "{}"}},
		{ID: "c2", Type: "function", Function: llm.ToolCallFunction{Name: "Grep", Arguments: "{}"}},
		{ID: "c3", Type: "function", Function: llm.ToolCallFunction{Name: "EditFile", Arguments: "{}"}},
		{ID: "c4", Type: "function", Function: llm.ToolCallFunction{Name: "Glob", Arguments: "{}"}},
	}

	groups := a.groupToolCalls(toolCalls)
	require.Len(t, groups, 3)

	// Group 1: Read + Grep (parallel)
	assert.True(t, groups[0].parallel)
	assert.Len(t, groups[0].calls, 2)
	assert.Equal(t, "Read", groups[0].calls[0].Function.Name)
	assert.Equal(t, "Grep", groups[0].calls[1].Function.Name)

	// Group 2: EditFile (sequential)
	assert.False(t, groups[1].parallel)
	assert.Len(t, groups[1].calls, 1)
	assert.Equal(t, "EditFile", groups[1].calls[0].Function.Name)

	// Group 3: Glob (parallel, but single element group)
	assert.True(t, groups[2].parallel)
	assert.Len(t, groups[2].calls, 1)
	assert.Equal(t, "Glob", groups[2].calls[0].Function.Name)
}

func TestGroupToolCalls_Empty(t *testing.T) {
	a := newTestAgent(nil)
	groups := a.groupToolCalls(nil)
	assert.Nil(t, groups)

	groups = a.groupToolCalls([]llm.ToolCall{})
	assert.Nil(t, groups)
}

func TestGroupToolCalls_SingleParallel(t *testing.T) {
	a := newTestAgent(nil)
	a.RegisterTool(slowStub("Read"))

	toolCalls := []llm.ToolCall{
		{ID: "c1", Type: "function", Function: llm.ToolCallFunction{Name: "Read", Arguments: "{}"}},
	}

	groups := a.groupToolCalls(toolCalls)
	require.Len(t, groups, 1)
	assert.True(t, groups[0].parallel)
	assert.Len(t, groups[0].calls, 1)
}

// ---- executeToolCalls Parallel Tests ----

func TestExecuteToolCalls_ParallelGroup(t *testing.T) {
	a := newTestAgent(nil)
	a.RegisterTool(slowStub("Read"))
	a.RegisterTool(slowStub("Grep"))
	a.RegisterTool(slowStub("Glob"))

	toolCalls := []llm.ToolCall{
		{ID: "c1", Type: "function", Function: llm.ToolCallFunction{Name: "Read", Arguments: `{"path":"a"}`}},
		{ID: "c2", Type: "function", Function: llm.ToolCallFunction{Name: "Grep", Arguments: `{"pattern":"x"}`}},
		{ID: "c3", Type: "function", Function: llm.ToolCallFunction{Name: "Glob", Arguments: `{"pattern":"*.go"}`}},
	}

	ch := make(chan AgentEvent, 32)
	msgs, err := a.executeToolCalls(t.Context(), toolCalls, ch)
	require.NoError(t, err)
	require.Len(t, msgs, 3)

	// All three should succeed
	assert.False(t, msgs[0].IsError)
	assert.False(t, msgs[1].IsError)
	assert.False(t, msgs[2].IsError)
	assert.Equal(t, wrapToolOutput("Read", "Read:done"), msgs[0].Content)
	assert.Equal(t, wrapToolOutput("Grep", "Grep:done"), msgs[1].Content)
	assert.Equal(t, wrapToolOutput("Glob", "Glob:done"), msgs[2].Content)

	close(ch)

	// Verify events: should have 3 ToolCallArgs + 3 ToolResult
	var argsEvents, resultEvents int
	for e := range ch {
		switch e.Type {
		case AgentEventToolCallArgs:
			argsEvents++
		case AgentEventToolResult:
			resultEvents++
		}
	}
	assert.Equal(t, 3, argsEvents)
	assert.Equal(t, 3, resultEvents)
}

func TestExecuteToolCalls_MixedSequentialAndParallel(t *testing.T) {
	a := newTestAgent(nil)
	a.RegisterTool(slowStub("Read"))
	a.RegisterTool(seqStub("EditFile"))
	a.RegisterTool(slowStub("Glob"))

	toolCalls := []llm.ToolCall{
		{ID: "c1", Type: "function", Function: llm.ToolCallFunction{Name: "Read", Arguments: "{}"}},
		{ID: "c2", Type: "function", Function: llm.ToolCallFunction{Name: "EditFile", Arguments: "{}"}},
		{ID: "c3", Type: "function", Function: llm.ToolCallFunction{Name: "Glob", Arguments: "{}"}},
	}

	ch := make(chan AgentEvent, 32)
	msgs, err := a.executeToolCalls(t.Context(), toolCalls, ch)
	require.NoError(t, err)
	require.Len(t, msgs, 3)

	assert.False(t, msgs[0].IsError)
	assert.Equal(t, wrapToolOutput("Read", "Read:done"), msgs[0].Content)
	assert.False(t, msgs[1].IsError)
	assert.Equal(t, wrapToolOutput("EditFile", "EditFile:done"), msgs[1].Content)
	assert.False(t, msgs[2].IsError)
	assert.Equal(t, wrapToolOutput("Glob", "Glob:done"), msgs[2].Content)

	close(ch)
}

func TestExecuteToolCalls_ResultOrderPreserved(t *testing.T) {
	// Even though tools run concurrently, results must be in call order.
	a := newTestAgent(nil)
	a.RegisterTool(slowStub("ToolA"))
	a.RegisterTool(slowStub("ToolB"))
	a.RegisterTool(slowStub("ToolC"))

	toolCalls := []llm.ToolCall{
		{ID: "a", Type: "function", Function: llm.ToolCallFunction{Name: "ToolA", Arguments: "{}"}},
		{ID: "b", Type: "function", Function: llm.ToolCallFunction{Name: "ToolB", Arguments: "{}"}},
		{ID: "c", Type: "function", Function: llm.ToolCallFunction{Name: "ToolC", Arguments: "{}"}},
	}

	ch := make(chan AgentEvent, 32)
	msgs, err := a.executeToolCalls(t.Context(), toolCalls, ch)
	require.NoError(t, err)
	require.Len(t, msgs, 3)

	assert.Equal(t, wrapToolOutput("ToolA", "ToolA:done"), msgs[0].Content)
	assert.Equal(t, wrapToolOutput("ToolB", "ToolB:done"), msgs[1].Content)
	assert.Equal(t, wrapToolOutput("ToolC", "ToolC:done"), msgs[2].Content)
	close(ch)
}

// ---- Agent Loop Integration Tests for Parallel Tool Calls ----

func TestAgentLoop_ParallelToolCalls(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			multiToolCallSeq(
				struct{ Name, ID, Args string }{"ToolA", "a", `{"key":"1"}`},
				struct{ Name, ID, Args string }{"ToolB", "b", `{"key":"2"}`},
				struct{ Name, ID, Args string }{"ToolC", "c", `{"key":"3"}`},
			),
			textSeq("All done"),
		},
	}

	a := newTestAgent(mp)
	a.RegisterTool(slowStub("ToolA"))
	a.RegisterTool(slowStub("ToolB"))
	a.RegisterTool(slowStub("ToolC"))

	ch := a.RunConversationStream(t.Context(), nil, "run three in parallel", "", llm.ChatOptions{MaxTokens: 4096})

	result, events := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "All done", result.Response)
	assert.Equal(t, "stop", result.ExitReason)

	// Should have received 3 ToolCallStart, 3 ToolCallArgs, 3 ToolResult events
	var starts, args, results int
	for _, e := range events {
		switch {
		case e.Type == AgentEventToolCallStart:
			starts++
		case e.Type == AgentEventToolCallArgs:
			args++
		case e.Type == AgentEventToolResult:
			results++
		}
	}
	assert.Equal(t, 3, starts, "expected 3 ToolCallStart events")
	assert.Equal(t, 3, args, "expected 3 ToolCallArgs events")
	assert.Equal(t, 3, results, "expected 3 ToolResult events")
}

func TestAgentLoop_MixedToolGroup(t *testing.T) {
	// Two parallel tools, then a sequential one, then another parallel one
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			multiToolCallSeq(
				struct{ Name, ID, Args string }{"Read", "a", "{}"},
				struct{ Name, ID, Args string }{"Grep", "b", "{}"},
				struct{ Name, ID, Args string }{"EditFile", "c", "{}"},
				struct{ Name, ID, Args string }{"Glob", "d", "{}"},
			),
			textSeq("Done"),
		},
	}

	a := newTestAgent(mp)
	a.RegisterTool(slowStub("Read"))
	a.RegisterTool(slowStub("Grep"))
	a.RegisterTool(seqStub("EditFile"))
	a.RegisterTool(slowStub("Glob"))

	ch := a.RunConversationStream(t.Context(), nil, "mixed tools", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "Done", result.Response)
	assert.Equal(t, "stop", result.ExitReason)
}

func TestAgentLoop_ParallelToolError(t *testing.T) {
	// One parallel tool errors; it should not block the others
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			multiToolCallSeq(
				struct{ Name, ID, Args string }{"ToolA", "a", "{}"},
				struct{ Name, ID, Args string }{"ErrorTool", "b", `{"msg":"boom"}`},
				struct{ Name, ID, Args string }{"ToolC", "c", "{}"},
			),
			textSeq("partial success"),
		},
	}

	a := newTestAgent(mp)
	a.RegisterTool(slowStub("ToolA"))
	a.RegisterTool(errorStub())
	a.RegisterTool(slowStub("ToolC"))

	ch := a.RunConversationStream(t.Context(), nil, "do parallel with one error", "", llm.ChatOptions{MaxTokens: 4096})

	result, events := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "partial success", result.Response)

	// Count tool results
	var errorResults, successResults int
	for _, e := range events {
		if e.Type == AgentEventToolResult {
			if e.ToolIsError {
				errorResults++
			} else {
				successResults++
			}
		}
	}
	assert.Equal(t, 1, errorResults, "expected 1 error tool result")
	assert.Equal(t, 2, successResults, "expected 2 successful tool results")
}

func TestAgentLoop_SingleParallelTool(t *testing.T) {
	// A single parallel tool should work (degenerate case, serial path)
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("ToolA", "a", `{"key":"1"}`),
			textSeq("done"),
		},
	}

	a := newTestAgent(mp)
	a.RegisterTool(slowStub("ToolA"))

	ch := a.RunConversationStream(t.Context(), nil, "single tool", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "done", result.Response)
}
