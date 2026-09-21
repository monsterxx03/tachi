package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/monsterxx03/tachi/config"
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
	client   *openai.Client
	model    string
	apiKey   string
	baseURL  string
	name     string // config provider name ("" = unknown); see Provider.ProviderName
	retryMax int    // retries after the initial attempt, consumed by the outer RetryProvider (see NewNamedProvider)
}

// NewOpenAIProvider constructs the go-openai provider. go-openai has no
// built-in retry, so MaxRetries is stored on the returned provider's retryMax
// field and consumed by NewNamedProvider when wrapping RetryProvider; Timeout
// sets the HTTP client's per-request timeout, Proxy the transport it dials
// through.
func NewOpenAIProvider(apiKey, baseURL, model string, opts ...config.ProviderOption) *OpenAIProvider {
	o := applyOptions(opts)
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	// The client carries the session-ID transport and the configured proxy;
	// this path has no per-request timeout of its own, so the option lands on
	// the client-wide timeout (0 = no timeout, this path's default).
	hc := httpClient(o)
	hc.Timeout = o.Timeout
	cfg.HTTPClient = hc
	// Default 2 retries keeps the pre-options behavior; explicit 0 disables.
	retryMax := 2
	if o.MaxRetries != nil {
		retryMax = max(0, *o.MaxRetries)
	}
	client := openai.NewClientWithConfig(cfg)
	return &OpenAIProvider{
		client:   client,
		model:    model,
		apiKey:   apiKey,
		baseURL:  cfg.BaseURL,
		retryMax: retryMax,
	}
}

func (p *OpenAIProvider) Name() string {
	return config.ProviderTypeOpenAI
}

func (p *OpenAIProvider) ProviderName() string { return p.name }

func (p *OpenAIProvider) Model() string {
	return p.model
}

func (p *OpenAIProvider) convertMessages(messages []Message) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(messages))

	// Images from tool results are collected here and flushed onto the next
	// message (see toolImageNote). The buffer holds the CURRENT tool block:
	// consecutive tool messages accumulate, and a non-tool message flushes them.
	var pendingToolImages []ContentPart

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
		// Role "tool" is the exception — see toolImageNote for why its images
		// are buffered rather than converted here.
		switch {
		case role == "tool" && len(msg.ContentParts) > 0:
			m.Content = msg.Content
			for _, part := range msg.ContentParts {
				if part.Type == ContentPartImage {
					pendingToolImages = append(pendingToolImages, part)
				}
			}
		case len(msg.ContentParts) > 0:
			m.MultiContent = multiContentParts(msg.ContentParts)
		default:
			m.Content = msg.Content
		}

		// Images a tool returned cannot ride the tool message itself (OpenAI
		// allows only text there), so they are buffered and emitted as a user
		// message once the tool block is complete. Flushing BEFORE the next
		// message keeps them directly behind the results they belong to; the
		// tail flush after the loop covers the common case, where the history
		// ends with the tool block that just ran.
		if role != "tool" && len(pendingToolImages) > 0 {
			out = append(out, toolImageMessage(pendingToolImages))
			pendingToolImages = nil
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

	if len(pendingToolImages) > 0 {
		out = append(out, toolImageMessage(pendingToolImages))
	}
	return out
}

// toolImageNote introduces the images lifted out of a tool result, so the
// model reads them as the output of the call above rather than as something
// the user said.
const toolImageNote = "Images returned by the tool call above:"

// toolImageMessage wraps tool-returned images as a user message.
func toolImageMessage(images []ContentPart) openai.ChatCompletionMessage {
	parts := make([]ContentPart, 0, 1+len(images))
	parts = append(parts, ContentPart{Type: ContentPartText, Text: toolImageNote})
	parts = append(parts, images...)
	return openai.ChatCompletionMessage{
		Role:         "user",
		MultiContent: multiContentParts(parts),
	}
}

// multiContentParts converts content parts to the OpenAI wire form.
//
// An image becomes a data URI. `detail` is deliberately left unset (empty):
// OpenAI defaults it to "auto" anyway, so omitting it changes nothing on
// official models, while OpenAI-compatible gateways (litellm → MiniMax, Kimi,
// ...) reject the field outright (HTTP 400 "invalid image detail: auto"). The
// omitempty tag keeps it off the wire.
func multiContentParts(parts []ContentPart) []openai.ChatMessagePart {
	out := make([]openai.ChatMessagePart, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case ContentPartText:
			out = append(out, openai.ChatMessagePart{
				Type: openai.ChatMessagePartTypeText,
				Text: part.Text,
			})
		case ContentPartImage:
			out = append(out, openai.ChatMessagePart{
				Type: openai.ChatMessagePartTypeImageURL,
				ImageURL: &openai.ChatMessageImageURL{
					URL: "data:" + part.MediaType + ";base64," + part.Data,
				},
			})
		}
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
		usage := &Usage{
			InputTokens:  int64(resp.Usage.PromptTokens),
			OutputTokens: int64(resp.Usage.CompletionTokens),
		}
		// Cache-hit tokens: DeepSeek (and some other OpenAI-compatible
		// providers) report them via prompt_tokens_details.cached_tokens.
		// Without this, hit tokens would be billed at the full input price
		// instead of the cache-read price.
		if details := resp.Usage.PromptTokensDetails; details != nil {
			usage.CacheReadInputTokens = int64(details.CachedTokens)
		}
		*lastUsage = usage
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
