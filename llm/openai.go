package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/sashabaranov/go-openai"
)

// tachiTransport wraps an http.RoundTripper and injects
// User-Agent and x-tachi-session-id headers on every outgoing request.
type tachiTransport struct {
	base http.RoundTripper
}

func (t *tachiTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", userAgent())
	if id, ok := SessionIDFromCtx(req.Context()); ok && id != "" {
		req.Header.Set("x-tachi-session-id", id)
	}
	return t.base.RoundTrip(req)
}

type OpenAIProvider struct {
	client  *openai.Client
	model   string
	apiKey  string
	baseURL string
}

func NewOpenAIProvider(apiKey, baseURL, model string) *OpenAIProvider {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	// Wrap the default HTTP client with a transport that injects the session
	// ID header from context, allowing per-request session tracking.
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if client, ok := cfg.HTTPClient.(*http.Client); ok {
		baseTransport := client.Transport
		if baseTransport == nil {
			baseTransport = http.DefaultTransport
		}
		client.Transport = &tachiTransport{base: baseTransport}
	}
	client := openai.NewClientWithConfig(cfg)
	return &OpenAIProvider{
		client:  client,
		model:   model,
		apiKey:  apiKey,
		baseURL: cfg.BaseURL,
	}
}

func (p *OpenAIProvider) Name() string {
	return ProviderTypeOpenAI
}

func (p *OpenAIProvider) Model() string {
	return p.model
}

func (p *OpenAIProvider) convertMessages(messages []Message) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(messages))
	for _, msg := range messages {
		role := msg.Role
		if role == RoleSteer {
			role = "user" // OpenAI has independent "tool" role, steer as user is safe
		}
		m := openai.ChatCompletionMessage{
			Role:       role,
			Name:       msg.Name,
			ToolCallID: msg.ToolCallID,
		}

		// Reconstruct reasoning_content from stored thinking blocks for
		// assistant messages in the history. This preserves the model's
		// chain-of-thought across multi-turn conversations.
		if role == "assistant" && len(msg.ThinkingBlocks) > 0 {
			var sb strings.Builder
			for _, tb := range msg.ThinkingBlocks {
				if tb.Type == "thinking" {
					sb.WriteString(tb.Thinking)
				}
			}
			m.ReasoningContent = sb.String()
		}

		// Multi-modal content: use MultiContent when ContentParts are present.
		// Note: OpenAI "tool" role messages only support string content, not
		// multi-modal arrays. For tool messages carrying image content parts,
		// we fall back to a text-only representation with an embedded data URI
		// reference. The model can at least acknowledge the image was read,
		// though it cannot visually process it in the tool role.
		if len(msg.ContentParts) > 0 && role != "tool" {
			for _, part := range msg.ContentParts {
				switch part.Type {
				case ContentPartText:
					m.MultiContent = append(m.MultiContent, openai.ChatMessagePart{
						Type: openai.ChatMessagePartTypeText,
						Text: part.Text,
					})
				case ContentPartImage:
					// OpenAI expects data URIs: "data:<mediaType>;base64,<data>"
					dataURI := "data:" + part.MediaType + ";base64," + part.Data
					m.MultiContent = append(m.MultiContent, openai.ChatMessagePart{
						Type: openai.ChatMessagePartTypeImageURL,
						ImageURL: &openai.ChatMessageImageURL{
							URL:    dataURI,
							Detail: openai.ImageURLDetailAuto,
						},
					})
				}
			}
		} else {
			m.Content = msg.Content
		}

		// Tool messages with image content parts: append image data URI
		// references to the text content so the model is aware of them.
		if role == "tool" && len(msg.ContentParts) > 0 {
			for _, part := range msg.ContentParts {
				if part.Type == ContentPartImage {
					ref := fmt.Sprintf("\n[Image data: data:%s;base64,%s...(%d chars)]",
						part.MediaType, part.Data[:min(len(part.Data), 40)], len(part.Data))
					m.Content += ref
				}
			}
		}

		for _, tc := range msg.ToolCalls {
			args := tc.Function.Arguments
			if args != "" && !json.Valid([]byte(args)) {
				// Arguments may be incomplete (e.g. truncated by max_tokens);
				// degrade to empty object rather than sending malformed JSON.
				args = "{}"
			}
			m.ToolCalls = append(m.ToolCalls, openai.ToolCall{
				ID:   tc.ID,
				Type: openai.ToolType(tc.Type),
				Function: openai.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: args,
				},
			})
		}
		out = append(out, m)
	}
	return out
}

func (p *OpenAIProvider) convertTools(tools []Tool) []openai.Tool {
	out := make([]openai.Tool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
			},
		})
	}
	return out
}

func (p *OpenAIProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*Response, error) {
	req := openai.ChatCompletionRequest{
		Model:       p.model,
		Messages:    p.convertMessages(messages),
		Tools:       p.convertTools(tools),
		Temperature: 1,
	}

	// Reasoning families (o1/o3/o4/gpt-5) reject max_tokens — the go-openai
	// client validates this before sending. Non-reasoning models
	// (gpt-4o, DeepSeek, ...) expect max_tokens.
	if isReasoningModelPrefix(p.model) {
		req.MaxCompletionTokens = opts.MaxTokens
	} else {
		req.MaxTokens = opts.MaxTokens
	}

	if p.isDeepSeek() {
		// DeepSeek thinking mode is controlled via the top-level "thinking"
		// field (enabled/disabled) plus "reasoning_effort" for strength,
		// injected via ExtraBody (the fork merges it into the body root).
		//
		// Default (nil thinking / empty effort): send NO thinking field so
		// the server-side default applies (DeepSeek: thinking on, effort
		// high). Only explicit configuration sends the field.
		switch {
		case opts.Thinking != nil && !*opts.Thinking:
			req.ExtraBody = map[string]any{"thinking": map[string]string{"type": "disabled"}}
		case opts.Thinking != nil && *opts.Thinking:
			req.ExtraBody = map[string]any{"thinking": map[string]string{"type": "enabled"}}
		case opts.ThinkingEffort != "":
			req.ExtraBody = map[string]any{"thinking": map[string]string{"type": "enabled"}}
			req.ReasoningEffort = opts.ThinkingEffort
		}
	} else if opts.Thinking != nil && !*opts.Thinking {
		// Thinking explicitly disabled: don't set ReasoningEffort — other
		// models (o1/o3/o4, etc.) pick their default non-reasoning behavior.
		logger.FromContext(ctx).Info(ctx, "openai: thinking disabled", "model", p.model, "baseURL", p.baseURL)
	} else if opts.ThinkingEffort != "" {
		req.ReasoningEffort = opts.ThinkingEffort
	}

	if opts.SessionID != "" {
		ctx = WithSessionID(ctx, opts.SessionID)
	}

	resp, err := p.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return nil, err
	}

	if len(resp.Choices) == 0 {
		return &Response{
			Content:      "",
			FinishReason: "stop",
		}, nil
	}

	choice := resp.Choices[0]
	response := &Response{
		Content:      choice.Message.Content,
		FinishReason: string(choice.FinishReason),
	}

	// Read reasoning_content from the response (o1/o3/o4 and DeepSeek reasoning models)
	if choice.Message.ReasoningContent != "" {
		response.Reasoning = choice.Message.ReasoningContent
	}

	for _, tc := range choice.Message.ToolCalls {
		response.ToolCalls = append(response.ToolCalls, ToolCall{
			ID:   tc.ID,
			Type: string(tc.Type),
			Function: ToolCallFunction{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}

	return response, nil
}

// isDeepSeek reports whether the provider model belongs to the DeepSeek
// family. DeepSeek models need a top-level "thinking" field to enable/disable
// thinking mode.
func (p *OpenAIProvider) isDeepSeek() bool {
	return isDeepSeek(p.model)
}

func (p *OpenAIProvider) CreateChatStream(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (<-chan StreamEvent, error) {
	req := openai.ChatCompletionRequest{
		Model:               p.model,
		Messages:            p.convertMessages(messages),
		Tools:               p.convertTools(tools),
		MaxCompletionTokens: opts.MaxTokens,
		// Temperature:         0.7,
		Stream:        true,
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
	}

	if p.isDeepSeek() {
		// Same top-level "thinking" injection as CreateChat — ExtraBody is
		// merged into the stream request body root by the forked go-openai.
		// Default (nil/empty) sends nothing; the server-side default applies.
		switch {
		case opts.Thinking != nil && !*opts.Thinking:
			req.ExtraBody = map[string]any{"thinking": map[string]string{"type": "disabled"}}
		case opts.Thinking != nil && *opts.Thinking:
			req.ExtraBody = map[string]any{"thinking": map[string]string{"type": "enabled"}}
		case opts.ThinkingEffort != "":
			req.ExtraBody = map[string]any{"thinking": map[string]string{"type": "enabled"}}
			req.ReasoningEffort = opts.ThinkingEffort
		}
	} else if opts.Thinking != nil && !*opts.Thinking {
		// Thinking explicitly disabled: don't set ReasoningEffort.
	} else if opts.ThinkingEffort != "" {
		req.ReasoningEffort = opts.ThinkingEffort
	}

	if opts.SessionID != "" {
		ctx = WithSessionID(ctx, opts.SessionID)
	}

	stream, err := p.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return nil, err
	}

	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		defer stream.Close()

		var lastFinishReason string
		var lastUsage *Usage

		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				ch <- StreamEvent{Type: StreamEventDone, FinishReason: lastFinishReason, Usage: lastUsage}
				return
			}
			if err != nil {
				ch <- StreamEvent{Type: StreamEventError, Error: err}
				return
			}

			emitOpenAIStreamChunk(resp, &lastFinishReason, &lastUsage, ch)
		}
	}()

	return ch, nil
}

// emitOpenAIStreamChunk converts a single streamed chunk into StreamEvents,
// tracking the rolling finish reason and usage.
func emitOpenAIStreamChunk(resp openai.ChatCompletionStreamResponse, lastFinishReason *string, lastUsage **Usage, ch chan<- StreamEvent) {
	// Capture usage from the last chunk (when stream_options.include_usage is set)
	if resp.Usage != nil {
		*lastUsage = &Usage{
			InputTokens:  int64(resp.Usage.PromptTokens),
			OutputTokens: int64(resp.Usage.CompletionTokens),
		}
	}

	if len(resp.Choices) == 0 {
		return
	}

	choice := resp.Choices[0]
	delta := choice.Delta

	// Emit reasoning_content from streaming deltas as thinking events
	// (o1/o3/o4, DeepSeek reasoning models, etc.)
	if delta.ReasoningContent != "" {
		ch <- StreamEvent{Type: StreamEventThinkingDelta, ThinkingDelta: delta.ReasoningContent}
	}

	if delta.Content != "" {
		ch <- StreamEvent{Type: StreamEventTextDelta, TextDelta: delta.Content}
	}

	for _, tc := range delta.ToolCalls {
		idx := 0
		if tc.Index != nil {
			idx = *tc.Index
		}
		if tc.ID != "" {
			ch <- StreamEvent{
				Type:      StreamEventToolUseStart,
				ToolIndex: idx,
				ToolCall: &ToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: ToolCallFunction{
						Name: tc.Function.Name,
					},
				},
			}
		}
		if tc.Function.Arguments != "" {
			ch <- StreamEvent{Type: StreamEventInputJSONDelta, ToolIndex: idx, InputDelta: tc.Function.Arguments}
		}
	}

	if choice.FinishReason != "" {
		*lastFinishReason = string(choice.FinishReason)
		ch <- StreamEvent{
			Type:         StreamEventMessageDelta,
			FinishReason: *lastFinishReason,
		}
	}
}
