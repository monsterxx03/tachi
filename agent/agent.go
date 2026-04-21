package agent

import (
	"context"
	"fmt"
	"log"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/tools"
)

const defaultMaxTokens = 32000

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

func (a *AIAgent) RegisterTools() {
	a.toolRegistry.Register(tools.ReadTool{})
	a.toolRegistry.Register(tools.WriteTool{})
	a.toolRegistry.Register(tools.EditTool{})
	a.toolRegistry.Register(tools.GlobTool{})
	a.toolRegistry.Register(tools.GrepTool{})
	a.toolRegistry.Register(tools.BashTool{})
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

	toolSchemas := a.toolRegistry.GetSchemas()
	llmTools := make([]llm.Tool, 0, len(toolSchemas))
	for _, schema := range toolSchemas {
		params := struct {
			Type       string `json:"type"`
			Properties map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		}{
			Type: schema.Parameters.Type,
			Properties: make(map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			}),
			Required: schema.Parameters.Required,
		}
		for name, prop := range schema.Parameters.Properties {
			params.Properties[name] = struct {
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
			Parameters:  params,
		})
	}

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

		if response.Usage != nil {
			log.Printf("[usage] input=%d output=%d cache_create=%d cache_read=%d",
				response.Usage.InputTokens, response.Usage.OutputTokens,
				response.Usage.CacheCreationInputTokens, response.Usage.CacheReadInputTokens)
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
