package llm

import (
	"context"
	"fmt"
)

// Version is set from main at startup (populated via ldflags at build time).
var Version string

// userAgent returns the User-Agent header value for LLM API requests.
func userAgent() string {
	if Version != "" {
		return "tachi/" + Version
	}
	return "tachi/dev"
}

// ctxKeySessionID is the context key for the x-tachi-session-id header value.
type ctxKeySessionID struct{}

// WithSessionID injects a session ID into the context so that HTTP transports
// can attach it as an x-tachi-session-id header on outgoing API requests.
func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeySessionID{}, id)
}

// SessionIDFromCtx extracts the session ID previously stored via WithSessionID.
func SessionIDFromCtx(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKeySessionID{}).(string)
	return id, ok
}

// Provider type constants.
const (
	ProviderTypeOpenAI    = "openai"
	ProviderTypeAnthropic = "anthropic"
)

type ThinkingBlock struct {
	Type      string // "thinking" | "redacted_thinking"
	Thinking  string
	Signature string
	Data      string // for redacted_thinking
}

type Usage struct {
	// InputTokens is the cumulative total of input tokens across all API calls
	// in the session (summed, not replaced).
	InputTokens int64
	// LastInputTokens is the input token count from the most recent API call.
	// This is the "current context size" — used for context-window percentage display.
	LastInputTokens          int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

type ChatOptions struct {
	MaxTokens int
	// Thinking controls the thinking/reasoning mode.
	// nil = provider default (adaptive for Anthropic, enabled for DeepSeek)
	// true = enabled, false = disabled
	Thinking *bool
	// ThinkingEffort sets the reasoning effort when thinking is enabled.
	// Supported values: "low", "medium", "high", "xhigh", "max".
	// Empty string falls back to the provider/model default ("high" for
	// Anthropic and DeepSeek).
	//   - Anthropic provider: sent as output_config.effort.
	//   - OpenAI provider: sent as reasoning_effort (DeepSeek included).
	ThinkingEffort string
	// SessionID, when non-empty, is sent as the x-tachi-session-id HTTP header
	// on API calls to the LLM provider.
	SessionID string
}

// ToolParameterProperty describes a single property in a tool's parameter schema.
type ToolParameterProperty struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Items       any    `json:"items,omitempty"`
}

// ToolParameters describes the JSON Schema for a tool's input parameters.
type ToolParameters struct {
	Type       string                           `json:"type"`
	Properties map[string]ToolParameterProperty `json:"properties"`
	Required   []string                         `json:"required"`
}

// Tool represents a function tool that can be called by the LLM.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  ToolParameters `json:"parameters"`
}

// NewTool constructs a Tool from its component parts.
// OpenAI requires "required" to be an array (never null), so we normalize
// nil slices to an empty slice here at the single construction point.
func NewTool(name, description string, properties map[string]ToolParameterProperty, required []string) Tool {
	if required == nil {
		required = []string{}
	}
	return Tool{
		Name:        name,
		Description: description,
		Parameters: ToolParameters{
			Type:       "object",
			Properties: properties,
			Required:   required,
		},
	}
}

// Message role constants.
const (
	RoleSteer = "steer" // Internal role: steer input injected at tool-call boundaries.
	// Provider converters handle this differently based on API protocol.
)

// ContentPartType identifies the type of a content part.
type ContentPartType string

const (
	ContentPartText  ContentPartType = "text"
	ContentPartImage ContentPartType = "image"
)

// ContentPart represents a single part of a multi-modal message.
// When Message.ContentParts is non-empty, providers use it instead of
// Message.Content to construct the API request (enabling image inputs).
type ContentPart struct {
	Type      ContentPartType `json:"type"`
	Text      string          `json:"text,omitempty"`       // for ContentPartText
	MediaType string          `json:"media_type,omitempty"` // e.g. "image/jpeg", "image/png"
	Data      string          `json:"data,omitempty"`       // base64-encoded image data
}

// Message represents a chat message
type Message struct {
	Role           string          `json:"role"`
	Content        string          `json:"content"`
	ContentParts   []ContentPart   `json:"content_parts,omitempty"` // multi-modal content; when set, providers prefer this over Content
	ToolCalls      []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID     string          `json:"tool_call_id,omitempty"`
	Name           string          `json:"name,omitempty"`
	IsError        bool            `json:"is_error,omitempty"`
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
	StreamEventTextDelta      = "text_delta"
	StreamEventThinkingDelta  = "thinking_delta"
	StreamEventSignatureDelta = "signature_delta"
	StreamEventToolUseStart   = "tool_use_start"
	StreamEventInputJSONDelta = "input_json_delta"
	StreamEventMessageDelta   = "message_delta"
	StreamEventDone           = "done"
	StreamEventError          = "error"
)

type StreamEvent struct {
	Type           string
	TextDelta      string
	ThinkingDelta  string
	SignatureDelta string
	ToolCall       *ToolCall
	ToolIndex      int // OpenAI parallel tool call index
	InputDelta     string
	FinishReason   string
	Usage          *Usage
	Error          error
}

// Provider defines the interface for LLM providers
type Provider interface {
	Name() string
	Model() string
	CreateChat(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*Response, error)
	CreateChatStream(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (<-chan StreamEvent, error)
}

func NewProvider(providerType, apiKey, baseURL, model string) (Provider, error) {
	switch providerType {
	case ProviderTypeOpenAI:
		// go-openai has no built-in retry; wrap with RetryProvider so
		// transient failures (connection reset, 429/5xx) don't abort
		// the whole turn.
		return NewRetryProvider(
			NewOpenAIProvider(apiKey, baseURL, model),
			RetryConfig{MaxRetries: 2},
		), nil
	case ProviderTypeAnthropic:
		// anthropic-sdk-go already retries internally (default MaxRetries=2),
		// so no extra wrapping is needed here.
		return NewAnthropicProvider(apiKey, baseURL, model), nil
	default:
		return nil, fmt.Errorf("unsupported provider type: %s", providerType)
	}
}
