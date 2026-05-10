package session

import (
	"time"
)

type Session struct {
	ID         string    `json:"id"`
	ThreadID   string    `json:"thread_id,omitempty"` // channel ThreadID for session lookup
	Title      string    `json:"title"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	WorkingDir string    `json:"working_dir,omitempty"` // working directory at session creation time
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type MessageType string

const (
	MessageTypeUser       MessageType = "user"
	MessageTypeAssistant MessageType = "assistant"
	MessageTypeThinking  MessageType = "thinking"
	MessageTypeToolCall   MessageType = "tool_call"
	MessageTypeToolResult MessageType = "tool_result"
	MessageTypeConfirm    MessageType = "confirm"
)

type Message struct {
	Type       MessageType `json:"type"`
	Content    string      `json:"content,omitempty"`
	Name       string      `json:"name,omitempty"`
	Signature  string      `json:"signature,omitempty"`
	Args       any         `json:"args,omitempty"`
	Result     string      `json:"result,omitempty"`
	IsError    bool        `json:"is_error,omitempty"`
	Diff       string      `json:"diff,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	SubagentID string      `json:"subagent_id,omitempty"` // shortID for SubAgent tool_result → subagent/<id>.jsonl
	Timestamp  time.Time   `json:"timestamp"`
}
