package agent

import (
	"reflect"
	"testing"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// String args for tool calls as stored in session messages.
const bashArgs = `{"command": "ls"}`

func TestConvertSessionToLLMMessages_Anthropic_SimpleExchange(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "hello"},
		{Type: session.MessageTypeThinking, Content: "User says hi", Signature: "sig-1"},
		{Type: session.MessageTypeAssistant, Content: "Hi there!"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "hello"},
		{
			Role:    "assistant",
			Content: "Hi there!",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "User says hi", Signature: "sig-1"},
			},
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("Anthropic simple exchange mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_Anthropic_ToolCall(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "list files"},
		{Type: session.MessageTypeThinking, Content: "Need to list files", Signature: "sig-1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: bashArgs, ToolCallID: "call_1"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "output", ToolCallID: "call_1"},
		{Type: session.MessageTypeThinking, Content: "Got file list", Signature: "sig-2"},
		{Type: session.MessageTypeAssistant, Content: "Here are the files: output"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "list files"},
		{
			Role:    "assistant",
			Content: "", // no text — pure tool_use
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "Need to list files", Signature: "sig-1"},
			},
			ToolCalls: []llm.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: llm.ToolCallFunction{
						Name:      "Bash",
						Arguments: bashArgs,
					},
				},
			},
		},
		{Role: "tool", Content: "output", ToolCallID: "call_1", Name: "Bash"},
		{
			Role:    "assistant",
			Content: "Here are the files: output",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "Got file list", Signature: "sig-2"},
			},
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("Anthropic tool call mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_OpenAI_PrependsThinking(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "hello"},
		{Type: session.MessageTypeThinking, Content: "User says hi", Signature: "sig-1"},
		{Type: session.MessageTypeAssistant, Content: "Hi there!"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "openai")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "hello"},
		{
			Role:           "assistant",
			Content:        "User says hi\n\nHi there!",
			ThinkingBlocks: nil, // thinking embedded in Content
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("OpenAI prepend mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_OpenAI_ToolCall(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "list files"},
		{Type: session.MessageTypeThinking, Content: "Need ls", Signature: "sig-1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: bashArgs, ToolCallID: "call_1"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "output", ToolCallID: "call_1"},
		{Type: session.MessageTypeThinking, Content: "Summarize result", Signature: "sig-2"},
		{Type: session.MessageTypeAssistant, Content: "Files: output"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "openai")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "list files"},
		{
			Role:    "assistant",
			Content: "Need ls", // thinking only, no assistant text
			ToolCalls: []llm.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: llm.ToolCallFunction{
						Name:      "Bash",
						Arguments: bashArgs,
					},
				},
			},
		},
		{Role: "tool", Content: "output", ToolCallID: "call_1", Name: "Bash"},
		{
			Role:    "assistant",
			Content: "Summarize result\n\nFiles: output",
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("OpenAI tool call mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_MultipleThinkingBlocks(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "think carefully"},
		{Type: session.MessageTypeThinking, Content: "First thought", Signature: "sig-1"},
		{Type: session.MessageTypeThinking, Content: "Second thought", Signature: "sig-2"},
		{Type: session.MessageTypeAssistant, Content: "Answer"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}

	assistant := result[1]
	if len(assistant.ThinkingBlocks) != 2 {
		t.Errorf("expected 2 thinking blocks, got %d", len(assistant.ThinkingBlocks))
	}
}

func TestConvertSessionToLLMMessages_ContinuationAfterMaxTokens(t *testing.T) {
	// When the LLM hits max_tokens, the session records:
	//   thinking → assistant (partial) → user ("Please continue...")
	// This should reconstruct as two separate messages.
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeThinking, Content: "This is a long", Signature: "sig-1"},
		{Type: session.MessageTypeAssistant, Content: "partial response"},
		{Type: session.MessageTypeUser, Content: "Please continue where you left off. Break your output into smaller chunks to avoid hitting the output token limit."},
		{Type: session.MessageTypeThinking, Content: "Continuing", Signature: "sig-2"},
		{Type: session.MessageTypeAssistant, Content: "more text"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{
			Role:    "assistant",
			Content: "partial response",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "This is a long", Signature: "sig-1"},
			},
		},
		{Role: "user", Content: "Please continue where you left off. Break your output into smaller chunks to avoid hitting the output token limit."},
		{
			Role:    "assistant",
			Content: "more text",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "Continuing", Signature: "sig-2"},
			},
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("continuation mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_ParallelToolCalls(t *testing.T) {
	// When the LLM calls multiple tools in a single response, the session records:
	//   thinking → tool_call_A → tool_result_A → tool_call_B → tool_result_B → thinking → assistant
	// Both tool calls should be grouped under ONE assistant message.
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "run two commands"},
		{Type: session.MessageTypeThinking, Content: "Need both status and diff", Signature: "sig-1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: `{"command": "git status"}`, ToolCallID: "call_1"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "status output", ToolCallID: "call_1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: `{"command": "git diff"}`, ToolCallID: "call_2"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "diff output", ToolCallID: "call_2"},
		{Type: session.MessageTypeThinking, Content: "Summarize results", Signature: "sig-2"},
		{Type: session.MessageTypeAssistant, Content: "Here's the summary"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "run two commands"},
		{
			Role:    "assistant",
			Content: "",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "Need both status and diff", Signature: "sig-1"},
			},
			ToolCalls: []llm.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: llm.ToolCallFunction{Name: "Bash", Arguments: `{"command": "git status"}`},
				},
				{
					ID:   "call_2",
					Type: "function",
					Function: llm.ToolCallFunction{Name: "Bash", Arguments: `{"command": "git diff"}`},
				},
			},
		},
		{Role: "tool", Content: "status output", ToolCallID: "call_1", Name: "Bash"},
		{Role: "tool", Content: "diff output", ToolCallID: "call_2", Name: "Bash"},
		{
			Role:    "assistant",
			Content: "Here's the summary",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "Summarize results", Signature: "sig-2"},
			},
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("parallel tool calls mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_SkipsConfirm(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "hello"},
		{Type: session.MessageTypeConfirm},
		{Type: session.MessageTypeAssistant, Content: "Hi!"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Errorf("expected 2 messages (confirm skipped), got %d: %+v", len(result), result)
	}
}
