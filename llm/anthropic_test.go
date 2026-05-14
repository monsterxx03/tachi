package llm

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestBuildRequest_SystemPrompt(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	// System prompt should be in System, not in Messages
	if len(req.System) != 1 || req.System[0].Text != "You are a helpful assistant." {
		t.Errorf("expected system prompt in req.System, got: %+v", req.System)
	}

	// Only one user message in Messages
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("expected user role, got %s", req.Messages[0].Role)
	}
}

func TestBuildRequest_BasicUserAssistant(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi, how can I help?"},
		{Role: "user", Content: "Tell me a joke"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(req.Messages))
	}

	if req.Messages[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("msg 0: expected user, got %s", req.Messages[0].Role)
	}
	if req.Messages[1].Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("msg 1: expected assistant, got %s", req.Messages[1].Role)
	}
	if req.Messages[2].Role != anthropic.MessageParamRoleUser {
		t.Errorf("msg 2: expected user, got %s", req.Messages[2].Role)
	}
}

func TestBuildRequest_UserWithSystem_SystemInSystemField(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "system", Content: "System instructions"},
		{Role: "user", Content: "Query"},
		{Role: "assistant", Content: "Answer"},
		{Role: "user", Content: "Follow-up"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	if len(req.System) != 1 {
		t.Errorf("expected 1 system block, got %d", len(req.System))
	}
	// System message should be excluded from Messages
	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages (system excluded), got %d", len(req.Messages))
	}
}

func TestBuildRequest_ToolMessagesOnly_NoSteer(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Do something"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls"}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "file1.txt\nfile2.txt"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	// Messages: user, assistant, user (tool results)
	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(req.Messages))
	}

	// Third message should be user-role with tool result blocks
	toolMsg := req.Messages[2]
	if toolMsg.Role != anthropic.MessageParamRoleUser {
		t.Errorf("tool result message: expected user role, got %s", toolMsg.Role)
	}
	if len(toolMsg.Content) != 1 {
		t.Fatalf("expected 1 content block in tool result message, got %d", len(toolMsg.Content))
	}
}

func TestBuildRequest_MultipleToolMessages_MergedIntoOneUserMessage(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Run two commands"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls"}`}},
			{ID: "tc2", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: `{"cmd":"pwd"}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "result1"},
		{Role: "tool", ToolCallID: "tc2", Content: "result2"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(req.Messages))
	}

	// Both tool results should be in a single user message
	toolMsg := req.Messages[2]
	if toolMsg.Role != anthropic.MessageParamRoleUser {
		t.Errorf("expected user role for merged tool results, got %s", toolMsg.Role)
	}
	if len(toolMsg.Content) != 2 {
		t.Errorf("expected 2 content blocks (2 tool results), got %d", len(toolMsg.Content))
	}
}

func TestBuildRequest_ToolMessagesFollowedBySteer_MergedIntoSameUserMessage(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Do something"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls"}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "file1.txt"},
		{Role: RoleSteer, Content: "Continue with the next step."},
		{Role: "assistant", Content: "Next response"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	// Messages: user, assistant, user(tool+steer merged), assistant
	if len(req.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(req.Messages))
	}

	// The tool+steer merged message should be user-role with 2 blocks
	mergedMsg := req.Messages[2]
	if mergedMsg.Role != anthropic.MessageParamRoleUser {
		t.Errorf("merged message: expected user role, got %s", mergedMsg.Role)
	}
	if len(mergedMsg.Content) != 2 {
		t.Errorf("expected 2 content blocks (tool result + steer text), got %d", len(mergedMsg.Content))
	}
}

func TestBuildRequest_MultipleToolsWithSteer(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Run commands"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: `{}`}},
			{ID: "tc2", Type: "function", Function: ToolCallFunction{Name: "read", Arguments: `{}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "r1"},
		{Role: "tool", ToolCallID: "tc2", Content: "r2", IsError: true},
		{Role: RoleSteer, Content: "The second tool failed. Handle this."},
		{Role: "assistant", Content: "I see an error."},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	if len(req.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(req.Messages))
	}

	// merged: 2 tool results + 1 steer text = 3 blocks
	mergedMsg := req.Messages[2]
	if len(mergedMsg.Content) != 3 {
		t.Errorf("expected 3 content blocks (2 tool results + 1 steer), got %d", len(mergedMsg.Content))
	}
}

func TestBuildRequest_ToolMessagesNoSteer_ThenUserFollowUp(t *testing.T) {
	// Verify no steer after tool → next message consumed correctly
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Do something"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: `{}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "result"},
		{Role: "user", Content: "Now do something else"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	// Messages: user, assistant, user(tool), user
	if len(req.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(req.Messages))
	}

	// Third message is tool result merged to user
	if req.Messages[2].Role != anthropic.MessageParamRoleUser {
		t.Errorf("msg 2: expected user (tool result), got %s", req.Messages[2].Role)
	}
	// Fourth message is the follow-up user message
	if req.Messages[3].Role != anthropic.MessageParamRoleUser {
		t.Errorf("msg 3: expected user, got %s", req.Messages[3].Role)
	}
	// Follow-up message content should be intact
	if len(req.Messages[3].Content) != 1 {
		t.Errorf("msg 3: expected 1 content block, got %d", len(req.Messages[3].Content))
	}
}

func TestBuildRequest_AssistantWithThinkingBlocks(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Think about this"},
		{Role: "assistant", Content: "Here is my answer", ThinkingBlocks: []ThinkingBlock{
			{Type: "thinking", Thinking: "Let me think...", Signature: "sig123"},
			{Type: "redacted_thinking", Data: "redacted_data"},
		}},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(req.Messages))
	}

	// Assistant message should have 3 blocks: thinking, redacted_thinking, text
	asstMsg := req.Messages[1]
	if asstMsg.Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("expected assistant role, got %s", asstMsg.Role)
	}
	if len(asstMsg.Content) != 3 {
		t.Errorf("expected 3 content blocks, got %d", len(asstMsg.Content))
	}
}

func TestBuildRequest_AssistantWithToolCalls(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Run ls"},
		{Role: "assistant", Content: "Let me run that", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls"}`}},
			{ID: "tc2", Type: "function", Function: ToolCallFunction{Name: "read", Arguments: `{"path":"f.txt"}`}},
		}},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	asstMsg := req.Messages[1]
	// Text block + 2 tool_use blocks = 3 blocks
	if len(asstMsg.Content) != 3 {
		t.Errorf("expected 3 content blocks (text + 2 tool_use), got %d", len(asstMsg.Content))
	}
}

func TestBuildRequest_AssistantWithInvalidToolArgsJSON(t *testing.T) {
	// When tool call args are not valid JSON (e.g. truncated stream),
	// buildRequest should degrade gracefully with an empty map.
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Do something"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls"`}},
		}},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	// Should not error; tool call with empty map should still produce 1 content block
	asstMsg := req.Messages[1]
	if len(asstMsg.Content) != 1 {
		t.Errorf("expected 1 content block, got %d", len(asstMsg.Content))
	}
}

func TestBuildRequest_ThinkingDisabled(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Hello"},
	}

	disabled := false
	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{
		MaxTokens: 100,
		Thinking:  &disabled,
	})
	if err != nil {
		t.Fatal(err)
	}

	if req.Thinking.OfDisabled == nil {
		t.Error("expected thinking to be disabled")
	}
}

func TestBuildRequest_ThinkingDefaultAdaptive(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Hello"},
	}

	// No Thinking option → should be adaptive
	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	if req.Thinking.OfAdaptive == nil {
		t.Error("expected adaptive thinking by default")
	}
}

func TestBuildRequest_Tools(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Hello"},
	}

	tools := []Tool{
		NewTool("bash", "Run a bash command", map[string]ToolParameterProperty{
			"cmd": {Type: "string", Description: "The command"},
		}, []string{"cmd"}),
	}

	req, err := p.buildRequest(context.Background(), msgs, tools, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	if len(req.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(req.Tools))
	}

	tool := req.Tools[0]
	if tool.OfTool.Name != "bash" {
		t.Errorf("expected tool name 'bash', got %s", tool.OfTool.Name)
	}
	if len(tool.OfTool.InputSchema.Required) != 1 || tool.OfTool.InputSchema.Required[0] != "cmd" {
		t.Errorf("expected required ['cmd'], got %v", tool.OfTool.InputSchema.Required)
	}
}

func TestBuildRequest_SystemPromptWithCacheControl(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "system", Content: "Cached system prompt"},
		{Role: "user", Content: "Query"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	// System prompt should have cache control
	if len(req.System) != 1 {
		t.Fatalf("expected 1 system block, got %d", len(req.System))
	}
	if req.System[0].CacheControl.Type == "" {
		t.Error("expected cache control on system prompt")
	}
}

func TestBuildRequest_NoSystemPrompt_NoSystemField(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Hello"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	if len(req.System) != 0 {
		t.Errorf("expected no system blocks, got %d", len(req.System))
	}
}

func TestBuildRequest_BaseURL(t *testing.T) {
	p := NewAnthropicProvider("key", "https://api.example.com", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Hello"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	if string(req.Model) != "claude-sonnet-4-6" {
		t.Errorf("expected model claude-sonnet-4-6, got %s", req.Model)
	}
}

func TestBuildRequest_MaxTokens(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "user", Content: "Hello"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}

	if req.MaxTokens != 4096 {
		t.Errorf("expected MaxTokens 4096, got %d", req.MaxTokens)
	}
}

// Edge case: tool messages at the very start (shouldn't happen in practice, but test correctness)
func TestBuildRequest_ToolMessagesAtStart_NoCrash(t *testing.T) {
	p := NewAnthropicProvider("key", "", "claude-sonnet-4-6")
	msgs := []Message{
		{Role: "tool", ToolCallID: "tc1", Content: "orphan result"},
		{Role: "user", Content: "Hello"},
	}

	req, err := p.buildRequest(context.Background(), msgs, nil, ChatOptions{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	// Tool result at start → should be a user message, then the real user message
	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("msg 0: expected user, got %s", req.Messages[0].Role)
	}
}