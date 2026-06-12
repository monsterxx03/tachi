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
		option.WithHeader("User-Agent", userAgent()),
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

// collectToolMessages gathers consecutive tool messages starting at startIdx,
// and optionally merges a trailing steer message as a text block into the same
// user message (to avoid violating Anthropic's strict user/assistant alternating
// requirement). Returns the content blocks and the index to resume iteration from.
func collectToolMessages(messages []Message, start int) ([]anthropic.ContentBlockParamUnion, int) {
	var blocks []anthropic.ContentBlockParamUnion

	// Consume consecutive tool messages.
	end := start
	for ; end < len(messages) && messages[end].Role == "tool"; end++ {
		msg := messages[end]
		// When the tool message carries image content parts, build a
		// multi-content tool_result block (text + images). Otherwise fall
		// back to the simple string-only helper.
		if len(msg.ContentParts) > 0 {
			content := make([]anthropic.ToolResultBlockParamContentUnion, 0, 1+len(msg.ContentParts))
			if msg.Content != "" {
				content = append(content, anthropic.ToolResultBlockParamContentUnion{
					OfText: &anthropic.TextBlockParam{Text: msg.Content},
				})
			}
			for _, part := range msg.ContentParts {
				switch part.Type {
				case ContentPartImage:
					content = append(content, anthropic.ToolResultBlockParamContentUnion{
						OfImage: &anthropic.ImageBlockParam{
							Source: anthropic.ImageBlockParamSourceUnion{
								OfBase64: &anthropic.Base64ImageSourceParam{
									Data:      part.Data,
									MediaType: anthropic.Base64ImageSourceMediaType(part.MediaType),
								},
							},
						},
					})
				}
			}
			blocks = append(blocks, anthropic.ContentBlockParamUnion{
				OfToolResult: &anthropic.ToolResultBlockParam{
					ToolUseID: msg.ToolCallID,
					Content:   content,
					IsError:   anthropic.Bool(msg.IsError),
				},
			})
		} else {
			blocks = append(blocks, anthropic.NewToolResultBlock(
				msg.ToolCallID,
				msg.Content,
				msg.IsError,
			))
		}
	}

	// If the next message is steer or a regular user message, merge it as a
	// text block into the same user message (tool results are already user-role;
	// a separate user message would create two consecutive user messages in
	// violation of Anthropic's strict alternation requirement). This handles:
	//   1. Steer: user input injected at tool-call boundaries during a turn
	//   2. Resume: session ended with tool results; user's next message would
	//      otherwise form consecutive user messages
	//   3. Loop reminders: system warnings injected after tool results
	if end < len(messages) && (messages[end].Role == RoleSteer || messages[end].Role == "user") {
		blocks = append(blocks, anthropic.NewTextBlock(messages[end].Content))
		end++ // consumed
	}

	return blocks, end
}

// effortFromString converts a thinking effort string to the Anthropic SDK type.
// Empty string defaults to "high". Recognized values: "low", "medium", "high", "xhigh", "max".
func effortFromString(effort string) anthropic.OutputConfigEffort {
	if effort == "" {
		return anthropic.OutputConfigEffortHigh
	}
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		return anthropic.OutputConfigEffort(effort)
	default:
		return anthropic.OutputConfigEffortHigh
	}
}

func (p *AnthropicProvider) buildRequest(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*anthropic.MessageNewParams, error) {
	var systemPrompt string
	var anthropicMessages []anthropic.MessageParam

	for i := 0; i < len(messages); {
		msg := messages[i]

		if msg.Role == "system" {
			systemPrompt = msg.Content
			i++
			continue
		}

		if msg.Role == "tool" {
			blocks, next := collectToolMessages(messages, i)
			i = next
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
		} else if len(msg.ContentParts) > 0 {
			// Multi-modal message (text + images)
			for _, part := range msg.ContentParts {
				switch part.Type {
				case ContentPartText:
					if part.Text != "" {
						contentBlocks = append(contentBlocks, anthropic.NewTextBlock(part.Text))
					}
				case ContentPartImage:
					contentBlocks = append(contentBlocks, anthropic.NewImageBlockBase64(part.MediaType, part.Data))
				}
			}
		} else {
			contentBlocks = append(contentBlocks, anthropic.NewTextBlock(msg.Content))
		}

		role := anthropic.MessageParamRole(msg.Role)
		anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
			Role:    role,
			Content: contentBlocks,
		})
		i++
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

	if opts.Thinking != nil && !*opts.Thinking {
		disabled := anthropic.NewThinkingConfigDisabledParam()
		req.Thinking = anthropic.ThinkingConfigParamUnion{OfDisabled: &disabled}
	} else {
		req.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}}
		req.OutputConfig = anthropic.OutputConfigParam{
			Effort: effortFromString(opts.ThinkingEffort),
		}
	}

	return req, nil
}

func (p *AnthropicProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*Response, error) {
	req, err := p.buildRequest(ctx, messages, tools, opts)
	if err != nil {
		return nil, err
	}

	reqOpts := []option.RequestOption{}
	if opts.SessionID != "" {
		reqOpts = append(reqOpts, option.WithHeader("x-tachi-session-id", opts.SessionID))
	}

	resp, err := p.client.Messages.New(ctx, *req, reqOpts...)
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

	reqOpts := []option.RequestOption{}
	if opts.SessionID != "" {
		reqOpts = append(reqOpts, option.WithHeader("x-tachi-session-id", opts.SessionID))
	}

	stream := p.client.Messages.NewStreaming(ctx, *req, reqOpts...)

	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		var lastUsage *Usage // from MessageDeltaEvent — preferred over acc.Usage
		acc := anthropic.Message{}
		for stream.Next() {
			event := stream.Current()
			acc.Accumulate(event)

			switch ev := event.AsAny().(type) {
			case anthropic.ContentBlockStartEvent:
				block := ev.ContentBlock
				if block.Type == "tool_use" {
					ch <- StreamEvent{
						Type:      StreamEventToolUseStart,
						ToolIndex: int(ev.Index),
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
					ch <- StreamEvent{Type: StreamEventInputJSONDelta, ToolIndex: int(ev.Index), InputDelta: delta.PartialJSON}
				}
			case anthropic.MessageDeltaEvent:
				lastUsage = &Usage{
					InputTokens:              ev.Usage.InputTokens,
					OutputTokens:             ev.Usage.OutputTokens,
					CacheCreationInputTokens: ev.Usage.CacheCreationInputTokens,
					CacheReadInputTokens:     ev.Usage.CacheReadInputTokens,
				}
				ch <- StreamEvent{
					Type:         StreamEventMessageDelta,
					FinishReason: string(ev.Delta.StopReason),
					Usage:        lastUsage,
				}
			}
		}

		if stream.Err() != nil {
			ch <- StreamEvent{Type: StreamEventError, Error: stream.Err()}
			return
		}

		// Prefer the usage from MessageDeltaEvent (authoritative API accounting).
		// Fall back to the accumulated usage from the SDK (OutputTokens only from
		// MessageDeltaEvent; InputTokens from MessageStartEvent).
		finishUsage := lastUsage
		if finishUsage == nil {
			finishUsage = &Usage{
				InputTokens:              acc.Usage.InputTokens,
				OutputTokens:             acc.Usage.OutputTokens,
				CacheCreationInputTokens: acc.Usage.CacheCreationInputTokens,
				CacheReadInputTokens:     acc.Usage.CacheReadInputTokens,
			}
		}
		ch <- StreamEvent{
			Type:         StreamEventDone,
			FinishReason: string(acc.StopReason),
			Usage:        finishUsage,
		}
	}()

	return ch, nil
}
