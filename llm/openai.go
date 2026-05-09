package llm

import (
	"context"
	"errors"
	"io"
	"net/http"

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
	client *openai.Client
	model  string
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
		client: client,
		model:  model,
	}
}

func (p *OpenAIProvider) Name() string {
	return ProviderTypeOpenAI
}

func (p *OpenAIProvider) convertMessages(messages []Message) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(messages))
	for _, msg := range messages {
		m := openai.ChatCompletionMessage{
			Role:       msg.Role,
			Content:    msg.Content,
			Name:       msg.Name,
			ToolCallID: msg.ToolCallID,
		}
		for _, tc := range msg.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, openai.ToolCall{
				ID:   tc.ID,
				Type: openai.ToolType(tc.Type),
				Function: openai.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
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
		MaxTokens:   opts.MaxTokens,
		Temperature: 0.7,
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

			// Capture usage from the last chunk (when stream_options.include_usage is set)
			if resp.Usage != nil {
				lastUsage = &Usage{
					InputTokens:  int64(resp.Usage.PromptTokens),
					OutputTokens: int64(resp.Usage.CompletionTokens),
				}
			}

			if len(resp.Choices) == 0 {
				continue
			}

			choice := resp.Choices[0]
			delta := choice.Delta

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
				lastFinishReason = string(choice.FinishReason)
				ch <- StreamEvent{
					Type:         StreamEventMessageDelta,
					FinishReason: lastFinishReason,
				}
			}
		}
	}()

	return ch, nil
}
