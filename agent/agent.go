package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/tools"
)

// IterationBudget tracks remaining iterations
type IterationBudget struct {
	Remaining int
	Parent    *IterationBudget
}

// AIAgent is the main agent implementation
type AIAgent struct {
	model           string
	provider        llm.Provider
	maxIterations   int
	toolRegistry    *tools.Registry
	iterationBudget *IterationBudget
	budgetGraceCall bool
}

// NewAIAgent creates a new AIAgent
func NewAIAgent(provider llm.Provider, model string, maxIterations int) *AIAgent {
	return &AIAgent{
		model:           model,
		provider:        provider,
		maxIterations:   maxIterations,
		toolRegistry:    tools.NewRegistry(),
		iterationBudget: &IterationBudget{Remaining: maxIterations},
	}
}

// RegisterTools registers tools with the agent
func (a *AIAgent) RegisterTools() {
	// Register Read tool
	a.toolRegistry.Register(
		"Read",
		"Read the contents of a file",
		map[string]tools.PropertySchema{
			"path":   {Type: "string", Description: "The path to the file to read"},
			"offset": {Type: "number", Description: "Line number to start reading from (1-indexed, default: 1)"},
			"limit":  {Type: "number", Description: "Number of lines to read (default: all lines from offset)"},
		},
		[]string{"path"},
		tools.ReadFile,
	)

	// Register Write tool
	a.toolRegistry.Register(
		"Write",
		"Write content to a file",
		map[string]tools.PropertySchema{
			"path":    {Type: "string", Description: "The path to write to"},
			"content": {Type: "string", Description: "The content to write"},
		},
		[]string{"path", "content"},
		tools.WriteFile,
	)

	// Register Glob tool
	a.toolRegistry.Register(
		"Glob",
		"Find files matching a glob pattern using ripgrep",
		map[string]tools.PropertySchema{
			"pattern": {Type: "string", Description: "The glob pattern to match (e.g., **/*.ts)"},
			"path":    {Type: "string", Description: "The directory to search in (defaults to current directory)"},
		},
		[]string{"pattern"},
		tools.GlobFile,
	)
}

// RunResult represents the result of running the agent
type RunResult struct {
	Response       string
	IterationsUsed int
	ExitReason     string
	Error          error
}

// RunConversation is the main agent loop
func (a *AIAgent) RunConversation(ctx context.Context, userMessage string, systemPrompt string, maxTokens int) *RunResult {
	if maxTokens <= 0 {
		maxTokens = 32000
	}

	// Initialize conversation history
	messages := make([]llm.Message, 0)

	// Add system prompt if provided
	if systemPrompt != "" {
		messages = append(messages, llm.Message{
			Role:    "system",
			Content: systemPrompt,
		})
	}

	// Build tool list for LLM
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
		// Check iteration budget
		if !a.iterationBudget.consume() && !a.budgetGraceCall {
			return &RunResult{
				ExitReason:     "budget_exhausted",
				IterationsUsed: apiCallCount,
				Error:          fmt.Errorf("iteration budget exhausted"),
			}
		}

		// Check context cancellation
		select {
		case <-ctx.Done():
			return &RunResult{
				ExitReason:     "interrupted",
				IterationsUsed: apiCallCount,
				Error:          ctx.Err(),
			}
		default:
		}

		// Build messages for API call
		apiMessages := make([]llm.Message, len(messages))
		copy(apiMessages, messages)

		// Add user message
		apiMessages = append(apiMessages, llm.Message{
			Role:    "user",
			Content: userMessage,
		})

		// Make API call
		apiCallCount++
		response, err := a.provider.CreateChat(ctx, apiMessages, llmTools, maxTokens)
		if err != nil {
			return &RunResult{
				ExitReason:     "error",
				IterationsUsed: apiCallCount,
				Error:          fmt.Errorf("API call failed: %w", err),
			}
		}

		// Handle finish reason
		switch response.FinishReason {
		case "stop", "end_turn":
			// Normal completion - return the response
			assistantMsg := llm.Message{
				Role:    "assistant",
				Content: response.Content,
			}
			messages = append(messages, assistantMsg)

			return &RunResult{
				Response:       cleanResponse(response.Content),
				IterationsUsed: apiCallCount,
				ExitReason:     "stop",
			}

		case "tool_calls", "tool_use":
			// Assistant wants to call tools
			assistantMsg := llm.Message{
				Role:    "assistant",
				Content: response.Content,
			}

			// Convert tool calls to message format
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

			// Execute tool calls
			for _, tc := range response.ToolCalls {
				result, err := a.toolRegistry.Invoke(tc.Function.Name, tc.Function.Arguments)
				if err != nil {
					result = fmt.Sprintf("Error: %v", err)
				}
				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
			}

			// Continue the loop to get the next response
			continue

		case "max_tokens", "length":
			// Response was truncated
			lengthContinueRetries++
			if lengthContinueRetries >= maxLengthContinueRetries {
				return &RunResult{
					ExitReason:     "length_exhausted",
					IterationsUsed: apiCallCount,
					Error:          fmt.Errorf("response truncated after %d continuation attempts", maxLengthContinueRetries),
				}
			}

			// Add continuation message to ask model to continue
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "[System: Your previous response was truncated. Please continue.]",
			})
			continue

		default:
			// Unknown finish reason, treat as stop
			return &RunResult{
				Response:       response.Content,
				IterationsUsed: apiCallCount,
				ExitReason:     "stop",
			}
		}
	}
}

// consume tries to consume one from the budget
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

// cleanResponse removes any thinking blocks from the response
func cleanResponse(s string) string {
	// Remove <think>...</think> blocks
	s = strings.ReplaceAll(s, "<think>", "")
	s = strings.ReplaceAll(s, "</think>", "")
	return strings.TrimSpace(s)
}
