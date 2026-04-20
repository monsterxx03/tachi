package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type AnthropicProvider struct {
	client anthropic.Client
	model  string
}

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

func (p *AnthropicProvider) Name() string {
	return "anthropic"
}

func (p *AnthropicProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*Response, error) {
	var systemPrompt string
	var anthropicMessages []anthropic.MessageParam

	for i := 0; i < len(messages); i++ {
		msg := messages[i]

		if msg.Role == "system" {
			systemPrompt = msg.Content
			continue
		}

		if msg.Role == "tool" {
			// Merge consecutive tool messages into a single user message
			var blocks []anthropic.ContentBlockParamUnion
			for ; i < len(messages) && messages[i].Role == "tool"; i++ {
				blocks = append(blocks, anthropic.NewToolResultBlock(
					messages[i].ToolCallID,
					messages[i].Content,
					messages[i].IsError,
				))
			}
			i-- // outer loop will increment
			anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleUser,
				Content: blocks,
			})
			continue
		}

		var contentBlocks []anthropic.ContentBlockParamUnion

		if msg.Role == "assistant" {
			// Reconstruct thinking blocks for conversation history
			for _, tb := range msg.ThinkingBlocks {
				switch tb.Type {
				case "thinking":
					contentBlocks = append(contentBlocks, anthropic.NewThinkingBlock(tb.Signature, tb.Thinking))
				case "redacted_thinking":
					contentBlocks = append(contentBlocks, anthropic.NewRedactedThinkingBlock(tb.Data))
				}
			}

			if msg.Content != "" {
				contentBlocks = append(contentBlocks, anthropic.NewTextBlock(msg.Content))
			}

			for _, tc := range msg.ToolCalls {
				var input map[string]any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
					return nil, fmt.Errorf("failed to unmarshal tool call arguments: %w", err)
				}
				contentBlocks = append(contentBlocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Function.Name))
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
		Model:        anthropic.Model(p.model),
		MaxTokens:    int64(opts.MaxTokens),
		Messages:     anthropicMessages,
		Tools:        anthropicTools,
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}

	if systemPrompt != "" {
		req.System = []anthropic.TextBlockParam{{
			Text:         systemPrompt,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}
	}

	if opts.ThinkingBudget > 0 {
		req.Thinking = anthropic.ThinkingConfigParamOfEnabled(opts.ThinkingBudget)
	}

	resp, err := p.client.Messages.New(ctx, req)
	if err != nil {
		return nil, err
	}

	response := &Response{
		FinishReason: string(resp.StopReason),
		Usage: &Usage{
			InputTokens:              resp.Usage.InputTokens,
			OutputTokens:             resp.Usage.OutputTokens,
			CacheCreationInputTokens: resp.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     resp.Usage.CacheReadInputTokens,
		},
	}

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			response.Content += block.Text
		case "thinking":
			response.ThinkingBlocks = append(response.ThinkingBlocks, ThinkingBlock{
				Type:      "thinking",
				Thinking:  block.Thinking,
				Signature: block.Signature,
			})
			response.Reasoning += block.Thinking
		case "redacted_thinking":
			response.ThinkingBlocks = append(response.ThinkingBlocks, ThinkingBlock{
				Type: "redacted_thinking",
				Data: block.Data,
			})
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
