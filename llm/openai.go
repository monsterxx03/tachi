package llm

import (
	"context"
	"errors"
	"io"

	"github.com/sashabaranov/go-openai"
)

type OpenAIProvider struct {
	client *openai.Client
	model  string
}

func NewOpenAIProvider(apiKey, baseURL, model string) *OpenAIProvider {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	client := openai.NewClientWithConfig(cfg)
	return &OpenAIProvider{
		client: client,
		model:  model,
	}
}

func (p *OpenAIProvider) Name() string {
	return "openai"
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
		Stream: true,
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

		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				ch <- StreamEvent{Type: StreamEventDone, FinishReason: lastFinishReason}
				return
			}
			if err != nil {
				ch <- StreamEvent{Type: StreamEventError, Error: err}
				return
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
						Type: StreamEventToolUseStart,
						ToolCall: &ToolCall{
							ID:   tc.ID,
							Type: "function",
							Function: ToolCallFunction{
								Name: tc.Function.Name,
							},
						},
					}
					_ = idx
				}
				if tc.Function.Arguments != "" {
					ch <- StreamEvent{Type: StreamEventInputJSONDelta, InputDelta: tc.Function.Arguments}
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
