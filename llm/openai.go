package llm

import (
	"context"

	"github.com/sashabaranov/go-openai"
)

// OpenAIProvider implements Provider for OpenAI Chat Completions API
type OpenAIProvider struct {
	client *openai.Client
	model  string
}

// NewOpenAIProvider creates a new OpenAI provider
// baseURL can be empty for default OpenAI API, or set to a custom endpoint (e.g., proxy URL)
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

// Name returns the provider name
func (p *OpenAIProvider) Name() string {
	return "openai"
}

// CreateChat sends a chat request to OpenAI
func (p *OpenAIProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*Response, error) {
	// Convert messages to OpenAI format
	openaiMessages := make([]openai.ChatCompletionMessage, 0, len(messages))
	for _, msg := range messages {
		m := openai.ChatCompletionMessage{
			Role:         msg.Role,
			Content:      msg.Content,
			Name:         msg.Name,
			ToolCallID:   msg.ToolCallID,
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
		openaiMessages = append(openaiMessages, m)
	}

	// Convert tools to OpenAI format
	openaiTools := make([]openai.Tool, 0, len(tools))
	for _, tool := range tools {
		openaiTools = append(openaiTools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
			},
		})
	}

	req := openai.ChatCompletionRequest{
		Model:       p.model,
		Messages:    openaiMessages,
		Tools:       openaiTools,
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
