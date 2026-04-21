package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/tools"
)

const defaultMaxTokens = 4096

type IterationBudget struct {
	Remaining int
	Parent    *IterationBudget
}

type AIAgent struct {
	model           string
	provider        llm.Provider
	maxIterations   int
	toolRegistry    *tools.Registry
	iterationBudget *IterationBudget
	budgetGraceCall bool
}

func NewAIAgent(provider llm.Provider, model string, maxIterations int) *AIAgent {
	return &AIAgent{
		model:           model,
		provider:        provider,
		maxIterations:   maxIterations,
		toolRegistry:    tools.NewRegistry(),
		iterationBudget: &IterationBudget{Remaining: maxIterations},
	}
}

func (a *AIAgent) SetProvider(provider llm.Provider, model string) {
	a.provider = provider
	a.model = model
}

func (a *AIAgent) RegisterTools() {
	a.toolRegistry.Register(tools.ReadTool{})
	a.toolRegistry.Register(tools.WriteTool{})
	a.toolRegistry.Register(tools.EditTool{})
	a.toolRegistry.Register(tools.GlobTool{})
	a.toolRegistry.Register(tools.GrepTool{})
	a.toolRegistry.Register(tools.BashTool{})
}

func (a *AIAgent) RegisterTool(tool tools.Tool) {
	a.toolRegistry.Register(tool)
}

type RunResult struct {
	Response       string
	IterationsUsed int
	ExitReason     string
	Error          error
}

func (a *AIAgent) RunConversation(ctx context.Context, userMessage string, systemPrompt string, opts llm.ChatOptions) *RunResult {
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = defaultMaxTokens
	}

	messages := make([]llm.Message, 0)

	if systemPrompt != "" {
		messages = append(messages, llm.Message{
			Role:    "system",
			Content: systemPrompt,
		})
	}

	messages = append(messages, llm.Message{
		Role:    "user",
		Content: userMessage,
	})

	llmTools := a.buildLLMTools(a.toolRegistry.GetSchemas())

	apiCallCount := 0
	lengthContinueRetries := 0
	const maxLengthContinueRetries = 3

	for {
		if !a.iterationBudget.consume() && !a.budgetGraceCall {
			return &RunResult{
				ExitReason:     "budget_exhausted",
				IterationsUsed: apiCallCount,
				Error:          fmt.Errorf("iteration budget exhausted"),
			}
		}

		select {
		case <-ctx.Done():
			return &RunResult{
				ExitReason:     "interrupted",
				IterationsUsed: apiCallCount,
				Error:          ctx.Err(),
			}
		default:
		}

		apiCallCount++
		response, err := a.provider.CreateChat(ctx, messages, llmTools, opts)
		if err != nil {
			return &RunResult{
				ExitReason:     "error",
				IterationsUsed: apiCallCount,
				Error:          fmt.Errorf("API call failed: %w", err),
			}
		}


		switch response.FinishReason {
		case "stop", "end_turn":
			messages = append(messages, llm.Message{
				Role:           "assistant",
				Content:        response.Content,
				ThinkingBlocks: response.ThinkingBlocks,
			})

			return &RunResult{
				Response:       response.Content,
				IterationsUsed: apiCallCount,
				ExitReason:     "stop",
			}

		case "tool_calls", "tool_use":
			assistantMsg := llm.Message{
				Role:           "assistant",
				Content:        response.Content,
				ThinkingBlocks: response.ThinkingBlocks,
			}

			for _, tc := range response.ToolCalls {
				assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, llm.ToolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Function: llm.ToolCallFunction{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
			messages = append(messages, assistantMsg)

			for _, tc := range response.ToolCalls {
				result, execErr := a.toolRegistry.Invoke(tc.Function.Name, tc.Function.Arguments)
				toolMsg := llm.Message{
					Role:       "tool",
					ToolCallID: tc.ID,
				}
				if execErr != nil {
					toolMsg.Content = fmt.Sprintf("Error: %v", execErr)
					toolMsg.IsError = true
				} else {
					toolMsg.Content = result
				}
				messages = append(messages, toolMsg)
			}
			continue

		case "max_tokens", "length":
			lengthContinueRetries++
			if lengthContinueRetries >= maxLengthContinueRetries {
				return &RunResult{
					ExitReason:     "length_exhausted",
					IterationsUsed: apiCallCount,
					Error:          fmt.Errorf("response truncated after %d continuation attempts", maxLengthContinueRetries),
				}
			}

			messages = append(messages, llm.Message{
				Role:           "assistant",
				Content:        response.Content,
				ThinkingBlocks: response.ThinkingBlocks,
			})
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "Please continue where you left off.",
			})
			continue

		default:
			return &RunResult{
				Response:       response.Content,
				IterationsUsed: apiCallCount,
				ExitReason:     "stop",
			}
		}
	}
}

const (
	AgentEventTextDelta     = "text_delta"
	AgentEventThinkingDelta = "thinking_delta"
	AgentEventToolCallStart = "tool_call_start"
	AgentEventToolCallArgs  = "tool_call_args"
	AgentEventToolResult    = "tool_result"
	AgentEventTurnComplete  = "turn_complete"
	AgentEventError         = "error"
)

type AgentEvent struct {
	Type          string
	TextDelta     string
	ThinkingDelta string
	ToolName      string
	ToolID        string
	ToolArgs      string
	ToolResult    string
	ToolIsError   bool
	Result        *RunResult
	Messages      []llm.Message
	Usage         *llm.Usage
}

// RunConversationStream runs a streaming agent conversation loop.
// It accepts existing message history for multi-turn support.
// Returns a channel of AgentEvents that the TUI consumes.
func (a *AIAgent) RunConversationStream(ctx context.Context, history []llm.Message, userMessage string, systemPrompt string, opts llm.ChatOptions) <-chan AgentEvent {
	ch := make(chan AgentEvent, 64)

	go func() {
		defer close(ch)

		if opts.MaxTokens <= 0 {
			opts.MaxTokens = defaultMaxTokens
		}

		messages := make([]llm.Message, len(history))
		copy(messages, history)

		if len(messages) == 0 && systemPrompt != "" {
			messages = append(messages, llm.Message{
				Role:    "system",
				Content: systemPrompt,
			})
		}

		messages = append(messages, llm.Message{
			Role:    "user",
			Content: userMessage,
		})

		toolSchemas := a.toolRegistry.GetSchemas()
		llmTools := a.buildLLMTools(toolSchemas)

		apiCallCount := 0
		lengthContinueRetries := 0
		const maxLengthContinueRetries = 3

		for {
			if !a.iterationBudget.consume() && !a.budgetGraceCall {
				ch <- AgentEvent{
					Type: AgentEventError,
					Result: &RunResult{
						ExitReason:     "budget_exhausted",
						IterationsUsed: apiCallCount,
						Error:          fmt.Errorf("iteration budget exhausted"),
					},
				}
				return
			}

			select {
			case <-ctx.Done():
				ch <- AgentEvent{
					Type: AgentEventError,
					Result: &RunResult{
						ExitReason:     "interrupted",
						IterationsUsed: apiCallCount,
						Error:          ctx.Err(),
					},
				}
				return
			default:
			}

			apiCallCount++
			streamCh, err := a.provider.CreateChatStream(ctx, messages, llmTools, opts)
			if err != nil {
				ch <- AgentEvent{
					Type: AgentEventError,
					Result: &RunResult{
						ExitReason:     "error",
						IterationsUsed: apiCallCount,
						Error:          fmt.Errorf("API call failed: %w", err),
					},
				}
				return
			}

			var textContent strings.Builder
			var thinkingContent strings.Builder
			var currentToolCalls []llm.ToolCall
			var currentToolArgs []strings.Builder
			var thinkingBlocks []llm.ThinkingBlock
			var finishReason string
			var turnUsage *llm.Usage

			for event := range streamCh {
				switch event.Type {
				case llm.StreamEventTextDelta:
					textContent.WriteString(event.TextDelta)
					ch <- AgentEvent{Type: AgentEventTextDelta, TextDelta: event.TextDelta}

				case llm.StreamEventThinkingDelta:
					thinkingContent.WriteString(event.ThinkingDelta)
					ch <- AgentEvent{Type: AgentEventThinkingDelta, ThinkingDelta: event.ThinkingDelta}

				case llm.StreamEventToolUseStart:
					if event.ToolCall != nil {
						currentToolCalls = append(currentToolCalls, *event.ToolCall)
						currentToolArgs = append(currentToolArgs, strings.Builder{})
						ch <- AgentEvent{
							Type:     AgentEventToolCallStart,
							ToolName: event.ToolCall.Function.Name,
							ToolID:   event.ToolCall.ID,
						}
					}

				case llm.StreamEventInputJSONDelta:
					if len(currentToolArgs) > 0 {
						currentToolArgs[len(currentToolArgs)-1].WriteString(event.InputDelta)
					}

				case llm.StreamEventMessageDelta, llm.StreamEventDone:
					finishReason = event.FinishReason
					if event.Usage != nil {
						turnUsage = event.Usage
					}

				case llm.StreamEventError:
					ch <- AgentEvent{
						Type: "error",
						Result: &RunResult{
							ExitReason:     "error",
							IterationsUsed: apiCallCount,
							Error:          event.Error,
						},
					}
					return
				}
			}

			for i := range currentToolCalls {
				if i < len(currentToolArgs) {
					currentToolCalls[i].Function.Arguments = currentToolArgs[i].String()
				}
			}

			if thinkingContent.Len() > 0 {
				thinkingBlocks = append(thinkingBlocks, llm.ThinkingBlock{
					Type:     "thinking",
					Thinking: thinkingContent.String(),
				})
			}


			switch finishReason {
			case "stop", "end_turn":
				assistantMsg := llm.Message{
					Role:           "assistant",
					Content:        textContent.String(),
					ThinkingBlocks: thinkingBlocks,
				}
				messages = append(messages, assistantMsg)

				ch <- AgentEvent{
					Type:     AgentEventTurnComplete,
					Messages: messages,
					Usage:    turnUsage,
					Result: &RunResult{
						Response:       textContent.String(),
						IterationsUsed: apiCallCount,
						ExitReason:     "stop",
					},
				}
				return

			case "tool_calls", "tool_use":
				assistantMsg := llm.Message{
					Role:           "assistant",
					Content:        textContent.String(),
					ThinkingBlocks: thinkingBlocks,
					ToolCalls:      currentToolCalls,
				}
				messages = append(messages, assistantMsg)

				for _, tc := range currentToolCalls {
					ch <- AgentEvent{
						Type:     AgentEventToolCallArgs,
						ToolName: tc.Function.Name,
						ToolID:   tc.ID,
						ToolArgs: tc.Function.Arguments,
					}

					result, execErr := a.toolRegistry.Invoke(tc.Function.Name, tc.Function.Arguments)
					toolMsg := llm.Message{
						Role:       "tool",
						ToolCallID: tc.ID,
					}
					if execErr != nil {
						toolMsg.Content = fmt.Sprintf("Error: %v", execErr)
						toolMsg.IsError = true
						ch <- AgentEvent{
							Type:        AgentEventToolResult,
							ToolName:    tc.Function.Name,
							ToolID:      tc.ID,
							ToolResult:  toolMsg.Content,
							ToolIsError: true,
						}
					} else {
						toolMsg.Content = result
						ch <- AgentEvent{
							Type:       AgentEventToolResult,
							ToolName:   tc.Function.Name,
							ToolID:     tc.ID,
							ToolResult: result,
						}
					}
					messages = append(messages, toolMsg)
				}
				continue

			case "max_tokens", "length":
				lengthContinueRetries++
				if lengthContinueRetries >= maxLengthContinueRetries {
					ch <- AgentEvent{
						Type: "error",
						Result: &RunResult{
							ExitReason:     "length_exhausted",
							IterationsUsed: apiCallCount,
							Error:          fmt.Errorf("response truncated after %d continuation attempts", maxLengthContinueRetries),
						},
					}
					return
				}
				messages = append(messages, llm.Message{
					Role:           "assistant",
					Content:        textContent.String(),
					ThinkingBlocks: thinkingBlocks,
				})
				messages = append(messages, llm.Message{
					Role:    "user",
					Content: "Please continue where you left off.",
				})
				continue

			default:
				messages = append(messages, llm.Message{
					Role:           "assistant",
					Content:        textContent.String(),
					ThinkingBlocks: thinkingBlocks,
				})
				ch <- AgentEvent{
					Type:     AgentEventTurnComplete,
					Messages: messages,
					Usage:    turnUsage,
					Result: &RunResult{
						Response:       textContent.String(),
						IterationsUsed: apiCallCount,
						ExitReason:     "stop",
					},
				}
				return
			}
		}
	}()

	return ch
}

func (a *AIAgent) buildLLMTools(toolSchemas []tools.Schema) []llm.Tool {
	llmTools := make([]llm.Tool, 0, len(toolSchemas))
	for _, schema := range toolSchemas {
		props := make(map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		})
		for name, prop := range schema.Parameters.Properties {
			props[name] = struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			}{
				Type:        prop.Type,
				Description: prop.Description,
			}
		}
		llmTools = append(llmTools, llm.Tool{
			Name:        schema.Name,
			Description: schema.Description,
			Parameters: struct {
				Type       string `json:"type"`
				Properties map[string]struct {
					Type        string `json:"type"`
					Description string `json:"description"`
				} `json:"properties"`
				Required []string `json:"required"`
			}{
				Type:       schema.Parameters.Type,
				Properties: props,
				Required:   schema.Parameters.Required,
			},
		})
	}
	return llmTools
}

// GetToolArgsPreview extracts a short preview from tool call arguments JSON
func GetToolArgsPreview(name, argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return argsJSON
	}
	switch name {
	case "ReadFile":
		if p, ok := args["path"].(string); ok {
			return p
		}
	case "WriteFile":
		if p, ok := args["path"].(string); ok {
			return p
		}
	case "EditFile":
		if p, ok := args["file_path"].(string); ok {
			return p
		}
	case "Glob":
		if p, ok := args["pattern"].(string); ok {
			return p
		}
	case "Grep":
		if p, ok := args["pattern"].(string); ok {
			return p
		}
	case "Bash":
		if c, ok := args["command"].(string); ok {
			if len(c) > 60 {
				return c[:60] + "..."
			}
			return c
		}
	case "WebSearch":
		if q, ok := args["query"].(string); ok {
			if len(q) > 60 {
				return q[:60] + "..."
			}
			return q
		}
	}
	return argsJSON
}

func (b *IterationBudget) consume() bool {
	if b.Remaining > 0 {
		b.Remaining--
		return true
	}
	if b.Parent != nil {
		return b.Parent.consume()
	}
	return false
}
