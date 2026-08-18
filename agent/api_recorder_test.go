package agent

import (
	"context"
	"encoding/json"
	"testing"

	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractSystemPrompt(t *testing.T) {
	tests := []struct {
		name     string
		messages []llm.Message
		want     string
	}{
		{
			name: "system first",
			messages: []llm.Message{
				{Role: "system", Content: "You are Tachi."},
				{Role: "user", Content: "hi"},
			},
			want: "You are Tachi.",
		},
		{
			name:     "no messages",
			messages: nil,
			want:     "",
		},
		{
			name: "user first (no system)",
			messages: []llm.Message{
				{Role: "user", Content: "hi"},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractSystemPrompt(tt.messages))
		})
	}
}

func TestToAPITools(t *testing.T) {
	tools := []llm.Tool{
		llm.NewTool("ReadFile", "Read a file", map[string]llm.ToolParameterProperty{
			"path": {Type: "string", Description: "file path"},
		}, []string{"path"}),
	}
	apiTools := toAPITools(tools)
	require.Len(t, apiTools, 1)
	assert.Equal(t, "ReadFile", apiTools[0].Name)
	assert.Equal(t, "Read a file", apiTools[0].Description)

	// Parameters carry the full schema as raw JSON.
	var schema map[string]any
	require.NoError(t, json.Unmarshal(apiTools[0].Parameters, &schema))
	assert.Equal(t, "object", schema["type"])
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "path")

	// Empty input → nil.
	assert.Nil(t, toAPITools(nil))
}

func TestExtractUserPrompt(t *testing.T) {
	tests := []struct {
		name     string
		messages []llm.Message
		want     string
	}{
		{
			name: "latest user after system",
			messages: []llm.Message{
				{Role: "system", Content: "sys"},
				{Role: "assistant", Content: "a"},
				{Role: "user", Content: "hello"},
			},
			want: "hello",
		},
		{
			name: "steer is picked up",
			messages: []llm.Message{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "a"},
				{Role: llm.RoleSteer, Content: "keep going"},
			},
			want: "keep going",
		},
		{
			name:     "no user message",
			messages: []llm.Message{{Role: "system", Content: "sys"}},
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractUserPrompt(tt.messages))
		})
	}
}

func TestRecordAPIRequestIntegration(t *testing.T) {
	// Two API calls: first triggers a Bash tool call, second answers with text.
	provider := &mockStreamProvider{
		name:         "openai",
		providerName: "deepseek-v4-flash",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"ls"}`),
			textSeq("done"),
		},
	}

	fake := &fakeSessionManager{}
	fake.SetCurrent(&session.Session{ID: "api-rec-test", Title: "pre-set"})

	a := newTestAgent(t, provider, func(ag *AIAgent) {
		ag.SetSessionManager(fake)
	})
	a.Config.ToolRegistry.Register(&stubTool{
		name: "Bash",
		desc: "Run a command",
		props: map[string]agenttools.PropertySchema{
			"command": {Type: "string", Description: "command to run"},
		},
		required: []string{"command"},
		executeFn: func(ctx context.Context, args string) (string, error) {
			return "ok", nil
		},
	})

	ch := a.RunConversationStream(context.Background(), nil, "hello", "You are Tachi.", llm.ChatOptions{
		MaxTokens:      1024,
		ThinkingEffort: "high",
	})
	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	require.NoError(t, result.Error)
	assert.Equal(t, ExitReasonStop, result.ExitReason)

	// One APIRequest per LLM call (2 calls above).
	require.Len(t, fake.apiRequests, 2, "one api request record per API call")

	for i, req := range fake.apiRequests {
		assert.Equal(t, "You are Tachi.", req.SystemPrompt, "call %d system prompt", i)
		assert.Equal(t, "hello", req.UserPrompt, "call %d user prompt", i)
		assert.Equal(t, i+1, req.Iteration, "call %d iteration", i)
		assert.Equal(t, i+1, req.Seq, "call %d seq", i)
		assert.Equal(t, "mock-model", req.Model, "call %d model", i)
		assert.Equal(t, "deepseek-v4-flash", req.Provider, "call %d provider", i)
		assert.Equal(t, "high", req.Thinking, "call %d thinking", i)
		require.NotEmpty(t, req.Tools, "call %d tools", i)
		assert.Equal(t, "Bash", req.Tools[0].Name)
		assert.NotEmpty(t, req.Tools[0].Parameters, "call %d schema", i)
	}

	// Tool_call/tool_result messages carry the iteration of the request that
	// produced them, linking execution back to the API call — and the same
	// session-wide Seq as the api_requests record.
	msgs, err := fake.LoadMessages()
	require.NoError(t, err)
	iterByType := map[session.MessageType]int{}
	seqByType := map[session.MessageType]int{}
	for _, m := range msgs {
		if m.Type == session.MessageTypeToolCall || m.Type == session.MessageTypeToolResult {
			iterByType[m.Type] = m.Iteration
			seqByType[m.Type] = m.Seq
		}
	}
	assert.Equal(t, 1, iterByType[session.MessageTypeToolCall], "tool_call belongs to iteration 1")
	assert.Equal(t, 1, iterByType[session.MessageTypeToolResult], "tool_result belongs to iteration 1")
	assert.Equal(t, 1, seqByType[session.MessageTypeToolCall], "tool_call shares seq 1 with its api request")
	assert.Equal(t, 1, seqByType[session.MessageTypeToolResult], "tool_result shares seq 1 with its api request")
}

func TestRecordAPISeqContinuesAcrossTurns(t *testing.T) {
	// Two turns, two API calls each (tool call then text answer). Iteration
	// restarts at 1 per turn, but Seq must keep counting session-wide
	// (1,2,3,4) so request-bound messages and api_requests records stay
	// linkable across turns.
	provider := &mockStreamProvider{
		name: "openai",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"ls"}`), textSeq("a2"), // turn 1
			toolCallSeq("Bash", "call-2", `{"command":"pwd"}`), textSeq("b2"), // turn 2
		},
	}

	fake := &fakeSessionManager{}
	fake.SetCurrent(&session.Session{ID: "api-rec-seq", Title: "pre-set"})

	a := newTestAgent(t, provider, func(ag *AIAgent) {
		ag.SetSessionManager(fake)
	})
	a.Config.ToolRegistry.Register(&stubTool{
		name: "Bash",
		desc: "Run a command",
		props: map[string]agenttools.PropertySchema{
			"command": {Type: "string", Description: "command to run"},
		},
		required: []string{"command"},
		executeFn: func(ctx context.Context, args string) (string, error) {
			return "ok", nil
		},
	})

	// Turn 1: two calls.
	ch := a.RunConversationStream(context.Background(), nil, "first", "You are Tachi.", llm.ChatOptions{MaxTokens: 1024})
	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	require.NoError(t, result.Error)

	// Turn 2: two more calls on the same session.
	ch = a.RunConversationStream(context.Background(), nil, "second", "You are Tachi.", llm.ChatOptions{MaxTokens: 1024})
	result, _ = drainAgentEvents(ch)
	require.NotNil(t, result)
	require.NoError(t, result.Error)

	require.Len(t, fake.apiRequests, 4, "two API requests per turn")

	// Iteration resets per turn; Seq keeps counting session-wide.
	iters := make([]int, len(fake.apiRequests))
	seqs := make([]int, len(fake.apiRequests))
	for i, r := range fake.apiRequests {
		iters[i] = r.Iteration
		seqs[i] = r.Seq
	}
	assert.Equal(t, []int{1, 2, 1, 2}, iters, "iteration restarts at 1 each turn")
	assert.Equal(t, []int{1, 2, 3, 4}, seqs, "seq is monotonic across turns")

	// Request-bound messages carry the same Seq as their api_requests record:
	// the first occurrence of iteration 1 belongs to turn 1 (seq 1), the
	// second to turn 2 (seq 3) — Seq disambiguates the repeated iteration.
	msgs, err := fake.LoadMessages()
	require.NoError(t, err)
	seqByIter := map[int][]int{} // iteration -> seqs of its assistant messages
	for _, m := range msgs {
		if m.Type == session.MessageTypeAssistant && m.Iteration > 0 {
			seqByIter[m.Iteration] = append(seqByIter[m.Iteration], m.Seq)
		}
	}
	assert.Equal(t, []int{1, 3}, seqByIter[1], "iteration 1 appears in turns 1 and 2")
	assert.Equal(t, []int{2, 4}, seqByIter[2], "iteration 2 appears in turns 1 and 2")
}

func TestRecordAPIRequestSkippedForOneOff(t *testing.T) {
	// One-off runs (SkipSessionWrites) must not write API requests to the
	// main session's api_requests.jsonl — without an attached sidecar
	// recorder they leave no request trail at all (see
	// TestRunOneOffStream_RecordsSidecar for the sidecar path).
	provider := &mockStreamProvider{
		name:      "openai",
		sequences: [][]llm.StreamEvent{textSeq("done")},
	}

	fake := &fakeSessionManager{}
	fake.SetCurrent(&session.Session{ID: "api-rec-skip", Title: "pre-set"})

	a := newTestAgent(t, provider, func(ag *AIAgent) {
		ag.SetSessionManager(fake)
	})

	ch := a.RunOneOffStream(context.Background(), provider, "You are Tachi.", "hello", llm.ChatOptions{MaxTokens: 1024})
	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	require.NoError(t, result.Error)

	assert.Empty(t, fake.apiRequests, "one-off runs must not record api requests to the main session")
}

func TestRequestThinking(t *testing.T) {
	enabled := true
	disabled := false
	tests := []struct {
		name string
		opts llm.ChatOptions
		want string
	}{
		{name: "default", opts: llm.ChatOptions{}, want: ""},
		{name: "disabled", opts: llm.ChatOptions{Thinking: &disabled}, want: "none"},
		{name: "enabled no effort", opts: llm.ChatOptions{Thinking: &enabled}, want: ""},
		{name: "effort", opts: llm.ChatOptions{ThinkingEffort: "high"}, want: "high"},
		{name: "disabled ignores effort", opts: llm.ChatOptions{Thinking: &disabled, ThinkingEffort: "high"}, want: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, requestThinking(tt.opts))
		})
	}
}
