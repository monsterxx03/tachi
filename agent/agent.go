package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/tools"
)

const defaultMaxTokens = 4096

type IterationBudget struct {
	Remaining int
	Parent    *IterationBudget
}

type AIAgent struct {
	model             string
	provider          llm.Provider
	maxIterations     int
	toolRegistry      *tools.Registry
	iterationBudget   *IterationBudget
	confirmRespCh     chan bool
	askUserRespCh     chan tools.AskUserResult
	skipEditConfirm   bool
}

func NewAIAgent(provider llm.Provider, model string, maxIterations int) *AIAgent {
	return &AIAgent{
		model:           model,
		provider:        provider,
		maxIterations:   maxIterations,
		toolRegistry:    tools.NewRegistry(),
		iterationBudget: &IterationBudget{Remaining: maxIterations},
		confirmRespCh:   make(chan bool, 1),
		askUserRespCh:   make(chan tools.AskUserResult, 1),
	}
}

// RespondToAskUser is called by TUI to respond to an AskUserQuestion request
func (a *AIAgent) RespondToAskUser(answers map[string]string, annotations map[string]string) {
	select {
	case a.askUserRespCh <- tools.AskUserResult{Answers: answers, Annotations: annotations}:
	default:
		// Channel already has a value or is not waiting
	}
}

// ConfirmTool is called by TUI to respond to a confirmation request
func (a *AIAgent) ConfirmTool(confirmed bool) {
	select {
	case a.confirmRespCh <- confirmed:
	default:
		// Channel already has a value or is not waiting
	}
}

func (a *AIAgent) SetProvider(provider llm.Provider, model string) {
	a.provider = provider
	a.model = model
}

func (a *AIAgent) SetSkipEditConfirm(skip bool) {
	a.skipEditConfirm = skip
}

func (a *AIAgent) RegisterTools() {
	a.toolRegistry.Register(tools.ReadTool{})
	a.toolRegistry.Register(tools.WriteTool{})
	a.toolRegistry.Register(tools.EditTool{})
	a.toolRegistry.Register(tools.GlobTool{})
	a.toolRegistry.Register(tools.GrepTool{})
	a.toolRegistry.Register(tools.BashTool{})
	a.toolRegistry.Register(tools.AskUserTool{})
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
	ch := a.RunConversationStream(ctx, nil, userMessage, systemPrompt, opts)
	var result *RunResult
	for event := range ch {
		switch event.Type {
		case AgentEventTurnComplete:
			result = event.Result
		case AgentEventError:
			result = event.Result
		case AgentEventToolConfirmation:
			a.ConfirmTool(true)
		}
	}
	if result == nil {
		result = &RunResult{ExitReason: "error", Error: fmt.Errorf("no result received")}
	}
	return result
}

const (
	AgentEventTextDelta         = "text_delta"
	AgentEventThinkingDelta     = "thinking_delta"
	AgentEventToolCallStart     = "tool_call_start"
	AgentEventToolCallArgs      = "tool_call_args"
	AgentEventToolConfirmation  = "tool_confirmation"
	AgentEventToolResult        = "tool_result"
	AgentEventTurnComplete      = "turn_complete"
	AgentEventError             = "error"
	AgentEventAskUser           = "ask_user_question"
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
	ToolDiff      string
	Questions     []tools.Question // For AskUserQuestion tool
	Result        *RunResult
	Messages      []llm.Message
	Usage         *llm.Usage
}

var errCancelled = fmt.Errorf("edit cancelled by user")

type streamAccumulator struct {
	text           strings.Builder
	thinking       strings.Builder
	signature      strings.Builder
	toolCalls      []llm.ToolCall
	toolArgs       []strings.Builder
	toolIndexMap   map[int]int // OpenAI tool index -> toolArgs slice index
	thinkBlocks    []llm.ThinkingBlock
	finishReason   string
	usage          *llm.Usage
}

func (acc *streamAccumulator) finalize() {
	for i := range acc.toolCalls {
		if i < len(acc.toolArgs) {
			acc.toolCalls[i].Function.Arguments = acc.toolArgs[i].String()
		}
	}
	if acc.thinking.Len() > 0 {
		acc.thinkBlocks = append(acc.thinkBlocks, llm.ThinkingBlock{
			Type:      "thinking",
			Thinking:  acc.thinking.String(),
			Signature: acc.signature.String(),
		})
	}
}

func (acc *streamAccumulator) assistantMessage() llm.Message {
	return llm.Message{
		Role:           "assistant",
		Content:        acc.text.String(),
		ThinkingBlocks: acc.thinkBlocks,
		ToolCalls:      acc.toolCalls,
	}
}

// consumeStream reads all events from the LLM stream, forwards deltas to the
// event channel, and returns the accumulated result.
func (a *AIAgent) consumeStream(streamCh <-chan llm.StreamEvent, ch chan<- AgentEvent, apiCallCount int) (*streamAccumulator, error) {
	acc := &streamAccumulator{
		toolIndexMap: make(map[int]int),
	}

	for event := range streamCh {
		switch event.Type {
		case llm.StreamEventTextDelta:
			acc.text.WriteString(event.TextDelta)
			ch <- AgentEvent{Type: AgentEventTextDelta, TextDelta: event.TextDelta}

		case llm.StreamEventThinkingDelta:
			acc.thinking.WriteString(event.ThinkingDelta)
			ch <- AgentEvent{Type: AgentEventThinkingDelta, ThinkingDelta: event.ThinkingDelta}

		case llm.StreamEventSignatureDelta:
			acc.signature.WriteString(event.SignatureDelta)

		case llm.StreamEventToolUseStart:
			if event.ToolCall != nil {
				sliceIdx := len(acc.toolCalls)
				acc.toolIndexMap[event.ToolIndex] = sliceIdx
				acc.toolCalls = append(acc.toolCalls, *event.ToolCall)
				acc.toolArgs = append(acc.toolArgs, strings.Builder{})
				ch <- AgentEvent{
					Type:     AgentEventToolCallStart,
					ToolName: event.ToolCall.Function.Name,
					ToolID:   event.ToolCall.ID,
				}
			}

		case llm.StreamEventInputJSONDelta:
			if idx, ok := acc.toolIndexMap[event.ToolIndex]; ok && idx < len(acc.toolArgs) {
				acc.toolArgs[idx].WriteString(event.InputDelta)
			} else if len(acc.toolArgs) > 0 {
				acc.toolArgs[len(acc.toolArgs)-1].WriteString(event.InputDelta)
			}

		case llm.StreamEventMessageDelta, llm.StreamEventDone:
			acc.finishReason = event.FinishReason
			if event.Usage != nil {
				acc.usage = event.Usage
			}

		case llm.StreamEventError:
			return nil, fmt.Errorf("stream error (iteration %d): %w", apiCallCount, event.Error)
		}
	}

	acc.finalize()
	return acc, nil
}

// executeToolCalls invokes each tool call, handling confirmation flow for tools
// that require it. Returns the tool result messages to append to history.
func (a *AIAgent) executeToolCalls(ctx context.Context, toolCalls []llm.ToolCall, ch chan<- AgentEvent) ([]llm.Message, error) {
	var toolMsgs []llm.Message

	for _, tc := range toolCalls {
		ch <- AgentEvent{
			Type:     AgentEventToolCallArgs,
			ToolName: tc.Function.Name,
			ToolID:   tc.ID,
			ToolArgs: tc.Function.Arguments,
		}

		tr := a.toolRegistry.Invoke(ctx, tc.Function.Name, tc.Function.Arguments)

		if tr.Status == tools.ToolResultPendingConfirm {
			if a.skipEditConfirm {
				debuglog.Log("Agent: tool %s skipping confirmation (skip_edit_confirm=true)", tc.Function.Name)
				output, err := a.toolRegistry.ExecuteConfirmed(ctx, tc.Function.Name, tr.Args)
				tr = tools.ToolResult{Status: tools.ToolResultSuccess, Output: output}
				if err != nil {
					tr = tools.ToolResult{Status: tools.ToolResultError, Err: err}
				}
			} else {
				debuglog.Log("Agent: tool %s requires confirmation, diff length: %d", tc.Function.Name, len(tr.Diff))
				ch <- AgentEvent{
					Type:     AgentEventToolConfirmation,
					ToolName: tc.Function.Name,
					ToolID:   tc.ID,
					ToolArgs: tr.Args,
					ToolDiff: tr.Diff,
				}

				select {
				case confirmed := <-a.confirmRespCh:
					if confirmed {
						output, err := a.toolRegistry.ExecuteConfirmed(ctx, tc.Function.Name, tr.Args)
						tr = tools.ToolResult{Status: tools.ToolResultSuccess, Output: output}
						if err != nil {
							tr = tools.ToolResult{Status: tools.ToolResultError, Err: err}
						}
					} else {
						return nil, errCancelled
					}
				case <-ctx.Done():
					tr = tools.ToolResult{Status: tools.ToolResultError, Err: ctx.Err()}
				}
			}
		}

		if tr.Status == tools.ToolResultNeedUserInput {
			debuglog.Log("Agent: AskUserQuestion tool requires user input, %d questions", len(tr.Questions))
			ch <- AgentEvent{
				Type:      AgentEventAskUser,
				ToolName:  tr.Name,
				ToolID:    tc.ID,
				ToolArgs:  tr.Args,
				Questions: tr.Questions,
			}

			select {
			case resp := <-a.askUserRespCh:
				resultData, _ := json.Marshal(map[string]interface{}{
					"questions":   tr.Questions,
					"answers":     resp.Answers,
					"annotations": resp.Annotations,
				})
				tr = tools.ToolResult{Status: tools.ToolResultSuccess, Output: string(resultData)}
			case <-ctx.Done():
				tr = tools.ToolResult{Status: tools.ToolResultError, Err: ctx.Err()}
			}
		}

		toolMsg := llm.Message{Role: "tool", ToolCallID: tc.ID}
		if tr.Status == tools.ToolResultError {
			toolMsg.Content = "Error: " + tr.Err.Error()
			toolMsg.IsError = true
			ch <- AgentEvent{
				Type: AgentEventToolResult, ToolName: tc.Function.Name,
				ToolID: tc.ID, ToolResult: toolMsg.Content, ToolIsError: true,
			}
		} else {
			toolMsg.Content = tr.Output
			ch <- AgentEvent{
				Type: AgentEventToolResult, ToolName: tc.Function.Name,
				ToolID: tc.ID, ToolResult: tr.Output,
			}
		}
		toolMsgs = append(toolMsgs, toolMsg)
	}

	return toolMsgs, nil
}

// handleFinishReason processes the LLM's finish reason and updates messages accordingly.
// Returns true if the agent loop should continue, false if it should stop.
func (a *AIAgent) handleFinishReason(
	ctx context.Context,
	acc *streamAccumulator,
	messages *[]llm.Message,
	ch chan<- AgentEvent,
	apiCallCount int,
	lengthRetries *int,
) bool {
	const maxLengthContinueRetries = 3

	switch acc.finishReason {
	case "tool_calls", "tool_use":
		*messages = append(*messages, acc.assistantMessage())

		toolMsgs, err := a.executeToolCalls(ctx, acc.toolCalls, ch)
		if err != nil {
			ch <- AgentEvent{
				Type:   AgentEventError,
				Result: &RunResult{ExitReason: "cancelled", Error: err},
			}
			return false
		}
		*messages = append(*messages, toolMsgs...)
		*lengthRetries = 0
		return true

	case "max_tokens", "length":
		*lengthRetries++
		if *lengthRetries >= maxLengthContinueRetries {
			ch <- AgentEvent{
				Type: AgentEventError,
				Result: &RunResult{
					ExitReason:     "length_exhausted",
					IterationsUsed: apiCallCount,
					Error:          fmt.Errorf("response truncated after %d continuation attempts", maxLengthContinueRetries),
				},
			}
			return false
		}
		msg := acc.assistantMessage()
		msg.ToolCalls = nil
		*messages = append(*messages, msg)
		*messages = append(*messages, llm.Message{Role: "user", Content: "Please continue where you left off."})
		return true

	default:
		*lengthRetries = 0
		msg := acc.assistantMessage()
		msg.ToolCalls = nil
		*messages = append(*messages, msg)
		ch <- AgentEvent{
			Type: AgentEventTurnComplete, Messages: *messages, Usage: acc.usage,
			Result: &RunResult{Response: acc.text.String(), IterationsUsed: apiCallCount, ExitReason: "stop"},
		}
		return false
	}
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
			messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
		}
		messages = append(messages, llm.Message{Role: "user", Content: userMessage})

		llmTools := a.buildLLMTools(a.toolRegistry.GetSchemas())

		apiCallCount := 0
		lengthContinueRetries := 0

		for {
			if !a.iterationBudget.consume() {
				ch <- AgentEvent{
					Type:   AgentEventError,
					Result: &RunResult{ExitReason: "budget_exhausted", IterationsUsed: apiCallCount, Error: fmt.Errorf("iteration budget exhausted")},
				}
				return
			}

			select {
			case <-ctx.Done():
				ch <- AgentEvent{
					Type:   AgentEventError,
					Result: &RunResult{ExitReason: "interrupted", IterationsUsed: apiCallCount, Error: ctx.Err()},
				}
				return
			default:
			}

			apiCallCount++
			streamCh, err := a.provider.CreateChatStream(ctx, messages, llmTools, opts)
			if err != nil {
				ch <- AgentEvent{
					Type:   AgentEventError,
					Result: &RunResult{ExitReason: "error", IterationsUsed: apiCallCount, Error: fmt.Errorf("API call failed: %w", err)},
				}
				return
			}

			acc, err := a.consumeStream(streamCh, ch, apiCallCount)
			if err != nil {
				exitReason := "error"
				if ctx.Err() != nil {
					exitReason = "interrupted"
				}
				ch <- AgentEvent{
					Type:   AgentEventError,
					Result: &RunResult{ExitReason: exitReason, IterationsUsed: apiCallCount, Error: err},
				}
				return
			}

			if !a.handleFinishReason(ctx, acc, &messages, ch, apiCallCount, &lengthContinueRetries) {
				return
			}
		}
	}()

	return ch
}

func (a *AIAgent) buildLLMTools(toolSchemas []tools.Schema) []llm.Tool {
	llmTools := make([]llm.Tool, 0, len(toolSchemas))
	for _, schema := range toolSchemas {
		props := make(map[string]llm.ToolParameterProperty, len(schema.Parameters.Properties))
		for name, prop := range schema.Parameters.Properties {
			props[name] = llm.ToolParameterProperty{Type: prop.Type, Description: prop.Description, Items: prop.Items}
		}
		llmTools = append(llmTools, llm.NewTool(schema.Name, schema.Description, props, schema.Parameters.Required))
	}
	return llmTools
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
