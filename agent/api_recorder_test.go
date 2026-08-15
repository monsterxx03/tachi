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
		assert.Equal(t, "mock-model", req.Model, "call %d model", i)
		assert.Equal(t, "deepseek-v4-flash", req.Provider, "call %d provider", i)
		assert.Equal(t, "high", req.Thinking, "call %d thinking", i)
		require.NotEmpty(t, req.Tools, "call %d tools", i)
		assert.Equal(t, "Bash", req.Tools[0].Name)
		assert.NotEmpty(t, req.Tools[0].Parameters, "call %d schema", i)
	}

	// Tool_call/tool_result messages carry the iteration of the request that
	// produced them, linking execution back to the API call.
	msgs, err := fake.LoadMessages()
	require.NoError(t, err)
	iterByType := map[session.MessageType]int{}
	for _, m := range msgs {
		if m.Type == session.MessageTypeToolCall || m.Type == session.MessageTypeToolResult {
			iterByType[m.Type] = m.Iteration
		}
	}
	assert.Equal(t, 1, iterByType[session.MessageTypeToolCall], "tool_call belongs to iteration 1")
	assert.Equal(t, 1, iterByType[session.MessageTypeToolResult], "tool_result belongs to iteration 1")
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
