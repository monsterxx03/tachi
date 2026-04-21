package llm

import (
	"context"
	"fmt"
)

type ThinkingBlock struct {
	Type      string // "thinking" | "redacted_thinking"
	Thinking  string
	Signature string
	Data      string // for redacted_thinking
}

type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

type ChatOptions struct {
	MaxTokens      int
	ThinkingBudget int64 // 0 = disabled, >0 = token budget
}

// Tool represents a function tool that can be called by the LLM
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	} `json:"parameters"`
}

// Message represents a chat message
type Message struct {
	Role           string         `json:"role"`
	Content        string         `json:"content"`
	ToolCalls      []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID     string         `json:"tool_call_id,omitempty"`
	Name           string         `json:"name,omitempty"`
	IsError        bool           `json:"is_error,omitempty"`
	ThinkingBlocks []ThinkingBlock `json:"thinking_blocks,omitempty"`
}

// ToolCallFunction represents the function called by the LLM
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall represents a tool call from the LLM
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// Response represents an LLM response
type Response struct {
	Content        string          `json:"content"`
	ToolCalls      []ToolCall      `json:"tool_calls,omitempty"`
	FinishReason   string          `json:"finish_reason"`
	Reasoning      string          `json:"reasoning,omitempty"`
	ThinkingBlocks []ThinkingBlock `json:"thinking_blocks,omitempty"`
	Usage          *Usage          `json:"usage,omitempty"`
}

const (
	StreamEventTextDelta     = "text_delta"
	StreamEventThinkingDelta = "thinking_delta"
	StreamEventToolUseStart  = "tool_use_start"
	StreamEventInputJSONDelta = "input_json_delta"
	StreamEventMessageDelta  = "message_delta"
	StreamEventDone          = "done"
	StreamEventError         = "error"
)

type StreamEvent struct {
	Type          string
	TextDelta     string
	ThinkingDelta string
	ToolCall      *ToolCall
	InputDelta    string
	FinishReason  string
	Usage         *Usage
	Error         error
}

// Provider defines the interface for LLM providers
type Provider interface {
	Name() string
	CreateChat(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*Response, error)
	CreateChatStream(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (<-chan StreamEvent, error)
}

func NewProvider(providerType, apiKey, baseURL, model string) (Provider, error) {
	switch providerType {
	case "openai":
		return NewOpenAIProvider(apiKey, baseURL, model), nil
	case "anthropic":
		return NewAnthropicProvider(apiKey, baseURL, model), nil
	default:
		return nil, fmt.Errorf("unsupported provider type: %s", providerType)
	}
}
