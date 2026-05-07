package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/monsterxx03/tachi/pkg/debuglog"
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
	return ProviderTypeAnthropic
}

func (p *AnthropicProvider) buildRequest(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*anthropic.MessageNewParams, error) {
	var systemPrompt string
	var anthropicMessages []anthropic.MessageParam

	for i := 0; i < len(messages); i++ {
		msg := messages[i]

		if msg.Role == "system" {
			systemPrompt = msg.Content
			continue
		}

		if msg.Role == "tool" {
			var blocks []anthropic.ContentBlockParamUnion
			for ; i < len(messages) && messages[i].Role == "tool"; i++ {
				blocks = append(blocks, anthropic.NewToolResultBlock(
					messages[i].ToolCallID,
					messages[i].Content,
					messages[i].IsError,
				))
			}
			i--
			anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleUser,
				Content: blocks,
			})
			continue
		}

		var contentBlocks []anthropic.ContentBlockParamUnion

		if msg.Role == "assistant" {
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
					// Arguments may be incomplete (e.g. truncated stream);
					// degrade gracefully so the error doesn't abort the entire request.
					debuglog.Log(ctx, "anthropic: failed to unmarshal tool call %s arguments: %v (args: %s)", tc.ID, err, tc.Function.Arguments)
					input = map[string]any{}
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

	req := &anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: int64(opts.MaxTokens),
		Messages:  anthropicMessages,
		Tools:     anthropicTools,
		// FIXME: why auto prompt on top level not working?
		// CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}

	if systemPrompt != "" {
		req.System = []anthropic.TextBlockParam{{
			Text:         systemPrompt,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}
	}

	req.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}}

	return req, nil
}

func (p *AnthropicProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*Response, error) {
	req, err := p.buildRequest(ctx, messages, tools, opts)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Messages.New(ctx, *req)
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

func (p *AnthropicProvider) CreateChatStream(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (<-chan StreamEvent, error) {
	req, err := p.buildRequest(ctx, messages, tools, opts)
	if err != nil {
		return nil, err
	}

	stream := p.client.Messages.NewStreaming(ctx, *req)

	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		acc := anthropic.Message{}
		for stream.Next() {
			event := stream.Current()
			acc.Accumulate(event)

			switch ev := event.AsAny().(type) {
			case anthropic.ContentBlockStartEvent:
				block := ev.ContentBlock
				if block.Type == "tool_use" {
					ch <- StreamEvent{
						Type: StreamEventToolUseStart,
						ToolCall: &ToolCall{
							ID:   block.ID,
							Type: "function",
							Function: ToolCallFunction{
								Name: block.Name,
							},
						},
					}
				}
			case anthropic.ContentBlockDeltaEvent:
				switch delta := ev.Delta.AsAny().(type) {
				case anthropic.TextDelta:
					ch <- StreamEvent{Type: StreamEventTextDelta, TextDelta: delta.Text}
				case anthropic.ThinkingDelta:
					ch <- StreamEvent{Type: StreamEventThinkingDelta, ThinkingDelta: delta.Thinking}
				case anthropic.SignatureDelta:
					ch <- StreamEvent{Type: StreamEventSignatureDelta, SignatureDelta: delta.Signature}
				case anthropic.InputJSONDelta:
					ch <- StreamEvent{Type: StreamEventInputJSONDelta, InputDelta: delta.PartialJSON}
				}
			case anthropic.MessageDeltaEvent:
				ch <- StreamEvent{
					Type:         StreamEventMessageDelta,
					FinishReason: string(ev.Delta.StopReason),
					Usage: &Usage{
						InputTokens:              ev.Usage.InputTokens,
						OutputTokens:             ev.Usage.OutputTokens,
						CacheCreationInputTokens: ev.Usage.CacheCreationInputTokens,
						CacheReadInputTokens:     ev.Usage.CacheReadInputTokens,
					},
				}
			}
		}

		if stream.Err() != nil {
			ch <- StreamEvent{Type: StreamEventError, Error: stream.Err()}
			return
		}

		ch <- StreamEvent{
			Type:         StreamEventDone,
			FinishReason: string(acc.StopReason),
			Usage: &Usage{
				InputTokens:              acc.Usage.InputTokens,
				OutputTokens:             acc.Usage.OutputTokens,
				CacheCreationInputTokens: acc.Usage.CacheCreationInputTokens,
				CacheReadInputTokens:     acc.Usage.CacheReadInputTokens,
			},
		}
	}()

	return ch, nil
}
