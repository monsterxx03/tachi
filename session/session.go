package session

import (
	"encoding/json"
	"time"
)

type Session struct {
	ID           string    `json:"id"`
	ThreadID     string    `json:"thread_id,omitempty"` // channel ThreadID for session lookup
	Title        string    `json:"title"`
	ProviderName string    `json:"provider_name,omitempty"` // config provider name (e.g. "deepseek-v4-flash"); empty = default provider
	WorkingDir   string    `json:"working_dir,omitempty"`   // working directory at session creation time
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	SkipDream    bool      `json:"skip_dream,omitempty"` // exclude this session from Dream memory consolidation

	// Session mode: "auto" (default), "chat", or "plan".
	// Controls tool visibility: auto = full access, chat/plan = read-only.
	Mode string `json:"mode,omitempty"`

	// ThinkingLevel is the per-session thinking override, set via /thinking.
	// One of "", "none", "low", "medium", "high", "xhigh", "max".
	// Empty = use the provider/model default from config. Only affects
	// this session — other sessions keep their own setting (or the default).
	ThinkingLevel string `json:"thinking_level,omitempty"`

	// Compact-related fields: link to child/parent sessions after /compact.
	CompactedChildID     string `json:"compacted_child_id,omitempty"`
	CompactedParentID    string `json:"compacted_parent_id,omitempty"`
	CompactedParentTitle string `json:"compacted_parent_title,omitempty"`
}

type MessageType string

const (
	MessageTypeUser       MessageType = "user"
	MessageTypeAssistant  MessageType = "assistant"
	MessageTypeThinking   MessageType = "thinking"
	MessageTypeToolCall   MessageType = "tool_call"
	MessageTypeToolResult MessageType = "tool_result"
	MessageTypeConfirm    MessageType = "confirm"
	MessageTypeReminder   MessageType = "reminder"
)

// Usage records token usage from a single LLM API response.
type Usage struct {
	InputTokens              int64 `json:"input_tokens,omitempty"`
	OutputTokens             int64 `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	EstimatedInputTokens     int64 `json:"estimated_input_tokens,omitempty"` // chars/4 local estimate — matches what statusbar showed during conversation
}

// Message represents a message in the session history.
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
	Usage      *Usage      `json:"usage,omitempty"`       // token usage from the LLM response that produced this message
	Iteration  int         `json:"iteration,omitempty"`   // 1-based LLM API call that produced this message (0 = not request-bound)
	// Seq is the session-wide request sequence number: monotonic across
	// turns (unlike Iteration, which resets to 1 per turn). API-request-bound
	// messages carry the same Seq as their api_requests.jsonl record, giving
	// consumers a stable cross-turn link. 0 = not request-bound (user /
	// reminder) or legacy data written before Seq existed.
	Seq       int       `json:"seq,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// APITool is a tool definition as sent to the LLM in one API request.
// Parameters carries the full JSON schema (llm.ToolParameters) so transcript
// viewers can inspect exactly what the model was offered.
type APITool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// APIRequest captures one LLM API call's request payload — the system prompt,
// tool schemas, and the user prompt that triggered it. Stored in
// api_requests.jsonl, one line per request, and consumed by the /transcript
// report to show what the agent saw on each call.
type APIRequest struct {
	Timestamp time.Time `json:"timestamp"`
	// Iteration is the 1-based API call sequence number within the turn that
	// made this request (it restarts at 1 for every fresh user prompt).
	// tool_call / tool_result messages carry the same number, linking each
	// tool execution to the request that produced it.
	Iteration int `json:"iteration,omitempty"`
	// Seq is the session-wide request sequence number: monotonic across
	// turns. Request-bound session messages (thinking / assistant /
	// tool_call / tool_result) carry the same Seq, so consumers can link a
	// request record to its messages even when Iteration repeats across
	// turns. 0 = legacy record written before Seq existed.
	Seq int `json:"seq,omitempty"`
	// SystemPrompt is the system message content sent with this request.
	SystemPrompt string `json:"system_prompt"`
	// UserPrompt is the latest user (or steer) message content in the request
	// — the input this call was answering.
	UserPrompt string `json:"user_prompt,omitempty"`
	// Model is the model name this request was sent to (e.g. "deepseek-v4-flash").
	Model string `json:"model,omitempty"`
	// Provider is the config provider name backing the model ("" when unknown).
	Provider string `json:"provider,omitempty"`
	// Thinking captures the thinking mode actually used for this request:
	// "none" (disabled), a reasoning effort ("low"/"high"/...), or "" for the
	// provider default.
	Thinking string    `json:"thinking,omitempty"`
	Tools    []APITool `json:"tools,omitempty"`
}
