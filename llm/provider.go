package llm

import (
	"context"
)

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
	Role         string     `json:"role"`
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID   string     `json:"tool_call_id,omitempty"`
	Name         string     `json:"name,omitempty"`
	Reasoning    string     `json:"reasoning,omitempty"`
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
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason"`
	Reasoning    string     `json:"reasoning,omitempty"`
}

// Provider defines the interface for LLM providers
type Provider interface {
	// Name returns the provider name
	Name() string
	// CreateChat sends a chat request to the LLM
	CreateChat(ctx context.Context, messages []Message, tools []Tool, maxTokens int) (*Response, error)
}
