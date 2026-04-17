package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicProvider implements Provider for Anthropic Messages API
type AnthropicProvider struct {
	client anthropic.Client
	model  string
}

// NewAnthropicProvider creates a new Anthropic provider
// baseURL can be empty for default Anthropic API, or set to a custom endpoint (e.g., proxy URL)
func NewAnthropicProvider(apiKey, baseURL, model string) *AnthropicProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	client := anthropic.NewClient(opts...)
	return &AnthropicProvider{
		client: client,
		model:  model,
	}
}

// Name returns the provider name
func (p *AnthropicProvider) Name() string {
	return "anthropic"
}

// CreateChat sends a chat request to Anthropic
func (p *AnthropicProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, maxTokens int) (*Response, error) {
	var systemPrompt string
	anthropicMessages := make([]anthropic.MessageParam, 0)

	for _, msg := range messages {
		if msg.Role == "system" {
			systemPrompt = msg.Content
			continue
		}

		var contentBlocks []anthropic.ContentBlockParamUnion

		if msg.Role == "tool" {
			contentBlocks = append(contentBlocks, anthropic.NewToolResultBlock(
				msg.ToolCallID,
				msg.Content,
				false,
			))
		} else if len(msg.ToolCalls) > 0 {
			if msg.Content != "" {
				contentBlocks = append(contentBlocks, anthropic.NewTextBlock(msg.Content))
			}
			for _, tc := range msg.ToolCalls {
				var input map[string]any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
					return nil, fmt.Errorf("failed to unmarshal tool call arguments: %w", err)
				}
				contentBlocks = append(contentBlocks, anthropic.NewToolUseBlock(
					tc.ID,
					input,
					tc.Function.Name,
				))
			}
		} else {
			contentBlocks = append(contentBlocks, anthropic.NewTextBlock(msg.Content))
		}

		role := anthropic.MessageParamRole(msg.Role)
		anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
			Role:    role,
			Content: contentBlocks,
		})
	}

	// Convert tools to Anthropic format
	anthropicTools := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		anthropicTools = append(anthropicTools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        tool.Name,
				Description: anthropic.String(tool.Description),
				InputSchema: anthropic.ToolInputSchemaParam{
					Type:       "object",
					Properties: tool.Parameters.Properties,
					Required:   tool.Parameters.Required,
				},
			},
		})
	}

	req := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: int64(maxTokens),
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages:  anthropicMessages,
		Tools:     anthropicTools,
	}

	resp, err := p.client.Messages.New(ctx, req)
	if err != nil {
		return nil, err
	}

	response := &Response{
		FinishReason: string(resp.StopReason),
	}

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			response.Content += block.Text
		case "tool_use":
			argsJSON, err := json.Marshal(block.Input)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal tool input: %w", err)
			}
			response.ToolCalls = append(response.ToolCalls, ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      block.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}

	return response, nil
}
