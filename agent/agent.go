package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
)

const defaultMaxTokens = 4096

type IterationBudget struct {
	Remaining int
	Unlimited bool // When true, consume() always returns true.
	Parent    *IterationBudget
}

type AIAgent struct {
	model              string
	provider           llm.Provider
	maxIterations      int
	toolRegistry       *tools.Registry
	iterationBudget    *IterationBudget
	confirmRespCh      chan bool
	askUserRespCh      chan tools.AskUserResult
	skipEditConfirm    bool
	sessionManager     *session.Manager
	reminderCollector  *systemreminder.Collector
	contextWindow      int64
	lastInputTokens    int64
	titleModelProvider llm.Provider // optional: dedicated provider for title generation
	titleGenEnabled    bool         // whether LLM-based title generation is active
	commitProvider     llm.Provider // optional: dedicated provider for /commit messages
}

func NewAIAgent(provider llm.Provider, model string, maxIterations int) *AIAgent {
	return &AIAgent{
		model:         model,
		provider:      provider,
		maxIterations: maxIterations,
		toolRegistry:  tools.NewRegistry(),
		confirmRespCh: make(chan bool, 1),
		askUserRespCh: make(chan tools.AskUserResult, 1),
		reminderCollector: systemreminder.NewCollector(
			systemreminder.DateReminder{},
			systemreminder.ProjectContextReminder{},
			systemreminder.GitReminder{},
			systemreminder.IterationWarningReminder{Threshold: config.DefaultIterationWarningThreshold},
			systemreminder.TokenWarningReminder{ThresholdPct: config.DefaultTokenWarningThresholdPct},
		),
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

// Model returns the current model name.
func (a *AIAgent) Model() string {
	return a.model
}

func (a *AIAgent) SetSkipEditConfirm(skip bool) {
	a.skipEditConfirm = skip
}

func (a *AIAgent) SetSessionManager(sm *session.Manager) {
	a.sessionManager = sm
}

// SetTitleModelProvider sets a dedicated LLM provider for title generation.
// When nil (the default), title generation falls back to a.truncation-based title.
func (a *AIAgent) SetTitleModelProvider(provider llm.Provider) {
	a.titleModelProvider = provider
}

// SetTitleGenEnabled enables or disables LLM-based title generation.
func (a *AIAgent) SetTitleGenEnabled(enabled bool) {
	a.titleGenEnabled = enabled
}

// SetupTitleProvider resolves and creates a dedicated LLM provider for title
// generation from config. When title_provider is empty or not found, title
// generation falls back to the main conversation provider.
func (a *AIAgent) SetupTitleProvider(cfg *config.Config) {
	a.titleGenEnabled = cfg.TitleGenerationEnabled()

	tpName := cfg.EffectiveTitleProvider()
	if tpName == "" {
		return
	}

	tpCfg := cfg.FindProvider(tpName)
	if tpCfg == nil {
		debuglog.Log("Agent: title_provider %q not found in providers list, falling back to main model", tpName)
		return
	}

	resolved, err := config.ResolveProviderConfig(tpCfg)
	if err != nil {
		debuglog.Log("Agent: failed to resolve title provider %q: %v, falling back to main model", tpName, err)
		return
	}

	tp, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		debuglog.Log("Agent: failed to create title provider %q: %v, falling back to main model", tpName, err)
		return
	}

	a.titleModelProvider = tp
	debuglog.Log("Agent: using title provider %q (%s/%s) for session title generation", tpName, resolved.Type, resolved.Model)
}

// SetupCommitProvider resolves and creates a dedicated LLM provider for /commit
// message generation from config. When commit_provider is empty or not found,
// commit generation falls back to the main conversation provider.
func (a *AIAgent) SetupCommitProvider(cfg *config.Config) {
	cpName := cfg.EffectiveCommitProvider()
	if cpName == "" {
		return
	}

	cpCfg := cfg.FindProvider(cpName)
	if cpCfg == nil {
		debuglog.Log("Agent: commit_provider %q not found in providers list, falling back to main model", cpName)
		return
	}

	resolved, err := config.ResolveProviderConfig(cpCfg)
	if err != nil {
		debuglog.Log("Agent: failed to resolve commit provider %q: %v, falling back to main model", cpName, err)
		return
	}

	cp, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		debuglog.Log("Agent: failed to create commit provider %q: %v, falling back to main model", cpName, err)
		return
	}

	a.commitProvider = cp
	debuglog.Log("Agent: using commit provider %q (%s/%s) for /commit message generation", cpName, resolved.Type, resolved.Model)
}

// CommitProvider returns the dedicated commit provider, or nil if none is configured
// (caller should fall back to the main provider).
func (a *AIAgent) CommitProvider() llm.Provider {
	return a.commitProvider
}

// RunOneOffStream runs a single-turn streaming conversation with a clean
// history (no inherited messages) using the given provider. No session
// recording is performed — this is for one-off tasks like /commit or /init.
// If provider is nil, falls back to a.provider.
func (a *AIAgent) RunOneOffStream(
	ctx context.Context,
	provider llm.Provider,
	systemPrompt string,
	userMessage string,
	opts llm.ChatOptions,
) <-chan AgentEvent {
	ch := make(chan AgentEvent, 64)

	go func() {
		defer close(ch)

		if provider == nil {
			provider = a.provider
		}

		if opts.MaxTokens <= 0 {
			opts.MaxTokens = defaultMaxTokens
		}

		// Build fresh messages: system + wrapped user message, no history
		messages := make([]llm.Message, 0, 2)
		if systemPrompt != "" {
			messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
		}

		rctx := a.buildReminderContext(true)
		wrappedUser := a.reminderCollector.WrapUserMessage(userMessage, rctx)
		messages = append(messages, llm.Message{Role: "user", Content: wrappedUser})

		a.runAgentLoop(ctx, provider, messages, opts, ch)
	}()

	return ch
}

// generateTitle uses the LLM to produce a concise session title from the first
// user message. Falls back to truncation on any error, empty response, or when
// title generation is disabled.
func (a *AIAgent) generateTitle(ctx context.Context, firstMessage string) string {
	if !a.titleGenEnabled {
		return session.ExtractTitle(firstMessage)
	}

	p := a.titleModelProvider
	if p == nil {
		p = a.provider
	}

	messages := []llm.Message{
		{
			Role:    "system",
			Content: "Generate a short, concise title (max 50 characters) for a conversation that starts with this user message. Output ONLY the title, no quotes, no explanation, no preamble.",
		},
		{
			Role:    "user",
			Content: firstMessage,
		},
	}

	resp, err := p.CreateChat(ctx, messages, nil, llm.ChatOptions{MaxTokens: 500})
	if err != nil {
		debuglog.Log("Agent: failed to generate title: %v, falling back to truncation", err)
		return session.ExtractTitle(firstMessage)
	}

	title := strings.TrimSpace(resp.Content)
	if title == "" {
		debuglog.Log("Agent: LLM returned empty title, falling back to truncation")
		return session.ExtractTitle(firstMessage)
	}

	// Enforce max length via existing ExtractTitle
	return session.ExtractTitle(title)
}

// SetContextWindow sets the model's context window size for token-warning reminders.
func (a *AIAgent) SetContextWindow(window int64) {
	a.contextWindow = window
}

// SetReminderCollector replaces the default reminder collector. Useful for
// tests or when callers want full control over which reminders fire.
func (a *AIAgent) SetReminderCollector(c *systemreminder.Collector) {
	a.reminderCollector = c
}

// SessionManager returns the session manager, or nil if none is set.
func (a *AIAgent) SessionManager() *session.Manager {
	return a.sessionManager
}

// ClearSession ends the current session so a new one will be created on the next message.
// Used by /new command to start a fresh session.
func (a *AIAgent) ClearSession() {
	if a.sessionManager != nil {
		a.sessionManager.EndCurrent()
	}
}

func (a *AIAgent) recordSession(msg *session.Message) {
	if a.sessionManager == nil {
		return
	}
	if err := a.sessionManager.AppendMessage(msg); err != nil {
		debuglog.Log("Agent: failed to record session message: %v", err)
	}
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

// UnregisterTool removes a tool from the agent's registry by name.
func (a *AIAgent) UnregisterTool(name string) {
	a.toolRegistry.Unregister(name)
}

// ToolSchemas returns all tool schemas currently registered with the agent.
func (a *AIAgent) ToolSchemas() []tools.Schema {
	return a.toolRegistry.GetSchemas()
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
	AgentEventTextDelta        = "text_delta"
	AgentEventThinkingDelta    = "thinking_delta"
	AgentEventToolCallStart    = "tool_call_start"
	AgentEventToolCallArgs     = "tool_call_args"
	AgentEventToolConfirmation = "tool_confirmation"
	AgentEventToolResult       = "tool_result"
	AgentEventTurnComplete     = "turn_complete"
	AgentEventError            = "error"
	AgentEventAskUser          = "ask_user_question"
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
	text         strings.Builder
	thinking     strings.Builder
	signature    strings.Builder
	toolCalls    []llm.ToolCall
	toolArgs     []strings.Builder
	toolIndexMap map[int]int // OpenAI tool index -> toolArgs slice index
	thinkBlocks  []llm.ThinkingBlock
	finishReason string
	usage        *llm.Usage
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

		// Record tool call in session
		a.recordSession(&session.Message{
			Type:       session.MessageTypeToolCall,
			Name:       tc.Function.Name,
			Args:       tc.Function.Arguments,
			ToolCallID: tc.ID,
		})

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

		// Record tool result in session
		a.recordSession(&session.Message{
			Type:       session.MessageTypeToolResult,
			Name:       tc.Function.Name,
			Result:     toolMsg.Content,
			IsError:    toolMsg.IsError,
			ToolCallID: tc.ID,
		})

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
		// Record thinking blocks in session
		for _, tb := range acc.thinkBlocks {
			a.recordSession(&session.Message{
				Type:      session.MessageTypeThinking,
				Content:   tb.Thinking,
				Signature: tb.Signature,
			})
		}
		// Record assistant text in session (may be empty)
		if acc.text.Len() > 0 {
			a.recordSession(&session.Message{
				Type:    session.MessageTypeAssistant,
				Content: acc.text.String(),
			})
		}

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
		// Record thinking blocks in session
		for _, tb := range acc.thinkBlocks {
			a.recordSession(&session.Message{
				Type:      session.MessageTypeThinking,
				Content:   tb.Thinking,
				Signature: tb.Signature,
			})
		}
		// Record partial assistant text
		if acc.text.Len() > 0 {
			a.recordSession(&session.Message{
				Type:    session.MessageTypeAssistant,
				Content: acc.text.String(),
			})
		}
		// Record continuation prompt (original, unwrapped)
		continuationText := "Please continue where you left off."
		a.recordSession(&session.Message{
			Type:    session.MessageTypeUser,
			Content: continuationText,
		})

		// Track input tokens for token-warning reminders
		if acc.usage != nil {
			a.lastInputTokens = acc.usage.InputTokens
		}

		// Wrap the continuation message with reminders
		rctx := a.buildReminderContext(false)
		wrappedContinuation := a.reminderCollector.WrapUserMessage(continuationText, rctx)

		msg := acc.assistantMessage()
		msg.ToolCalls = nil
		*messages = append(*messages, msg)
		*messages = append(*messages, llm.Message{Role: "user", Content: wrappedContinuation})
		return true

	default:
		*lengthRetries = 0
		msg := acc.assistantMessage()
		msg.ToolCalls = nil
		*messages = append(*messages, msg)

		// Record thinking blocks in session
		for _, tb := range acc.thinkBlocks {
			a.recordSession(&session.Message{
				Type:      session.MessageTypeThinking,
				Content:   tb.Thinking,
				Signature: tb.Signature,
			})
		}
		// Append assistant response to session
		if acc.text.Len() > 0 {
			a.recordSession(&session.Message{
				Type:    session.MessageTypeAssistant,
				Content: acc.text.String(),
			})
		}

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

		isFirstMessage := len(messages) == 0

		if len(messages) == 0 && systemPrompt != "" {
			messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
		}

		// Build reminder context for the initial user message.
		rctx := a.buildReminderContext(isFirstMessage)
		wrappedUser := a.reminderCollector.WrapUserMessage(userMessage, rctx)
		messages = append(messages, llm.Message{Role: "user", Content: wrappedUser})

		// Session management: create session if needed and append user message
		if a.sessionManager != nil && !a.sessionManager.HasCurrent() {
			provider := a.provider.Name()
			if _, err := a.sessionManager.New(provider, a.model); err != nil {
				debuglog.Log("Agent: failed to create session: %v", err)
			}
		}
		if a.sessionManager != nil {
			// Record the original user message (without system-reminder wrappers)
			a.recordSession(&session.Message{
				Type:    session.MessageTypeUser,
				Content: userMessage,
			})
			// Set title from first user message (LLM-generated or truncated)
			if curr := a.sessionManager.Current(); curr != nil && curr.Title == "" {
				a.sessionManager.SetTitle(a.generateTitle(ctx, userMessage))
			}
		}

		a.runAgentLoop(ctx, a.provider, messages, opts, ch)
	}()

	return ch
}

// runAgentLoop is the shared event loop used by both RunConversationStream
// and RunOneOffStream. It handles iteration budgets, streaming, tool
// execution, length continuation, and system-reminder injection.
func (a *AIAgent) runAgentLoop(
	ctx context.Context,
	provider llm.Provider,
	messages []llm.Message,
	opts llm.ChatOptions,
	ch chan<- AgentEvent,
) {
	llmTools := a.buildLLMTools(a.toolRegistry.GetSchemas())

	apiCallCount := 0
	lengthContinueRetries := 0

	// Initialize iteration budget.
	if a.maxIterations == 0 {
		a.iterationBudget = &IterationBudget{Unlimited: true}
	} else {
		a.iterationBudget = &IterationBudget{Remaining: a.maxIterations}
	}

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
		streamCh, err := provider.CreateChatStream(ctx, messages, llmTools, opts)
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

		// Track input tokens for token-warning reminders
		if acc.usage != nil {
			a.lastInputTokens = acc.usage.InputTokens
		}

		if !a.handleFinishReason(ctx, acc, &messages, ch, apiCallCount, &lengthContinueRetries) {
			return
		}

		// After tool results, inject system-reminder warnings.
		if a.shouldInjectLoopReminder() {
			rctx := a.buildReminderContext(false)
			if block := a.reminderCollector.Collect(rctx); block != "" {
				messages = append(messages, llm.Message{Role: "user", Content: block})
			}
		}
	}
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

// buildReminderContext constructs the systemreminder.Context used when
// generating reminders for a user message (or loop injection).
func (a *AIAgent) buildReminderContext(isFirstMessage bool) systemreminder.Context {
	iterLeft := 0
	if a.iterationBudget != nil {
		iterLeft = a.iterationBudget.Remaining
	}
	return systemreminder.Context{
		IsFirstMessage: isFirstMessage,
		IterationsLeft: iterLeft,
		MaxIterations:  a.maxIterations,
		InputTokens:    a.lastInputTokens,
		ContextWindow:  a.contextWindow,
		Now:            time.Now(),
	}
}

// shouldInjectLoopReminder returns true when we should inject a
// system-reminder user message before the next API call.
// Currently true whenever iteration or token warnings are likely to be
// active. The actual filtering is done by Collect().
func (a *AIAgent) shouldInjectLoopReminder() bool {
	return a.reminderCollector != nil
}

// Configure wires up all agent sub-systems from config: reminders, built-in
// tools, web search, and MCP server connections. Returns the MCP manager for
// later cleanup (may be nil).
func (a *AIAgent) Configure(ctx context.Context, cfg *config.Config) (*mcp.Manager, error) {
	// --- reminders ---
	reminders := []systemreminder.Reminder{
		systemreminder.DateReminder{},
		systemreminder.ProjectContextReminder{},
		systemreminder.IterationWarningReminder{Threshold: cfg.IterationWarningThreshold()},
		systemreminder.TokenWarningReminder{ThresholdPct: cfg.TokenWarningThresholdPct()},
	}
	if cfg.GitReminderEnabled() {
		reminders = append(reminders, systemreminder.GitReminder{})
	}
	a.reminderCollector = systemreminder.NewCollector(reminders...)

	// --- built-in tools + web search ---
	a.RegisterTools()
	ws := tools.WebSearchTool{
		ProviderType: cfg.WebSearch.Type,
		APIKey:       cfg.WebSearch.Key,
		Timeout:      cfg.WebSearch.Timeout,
		MaxResults:   cfg.WebSearch.MaxResults,
		Proxy:        cfg.WebSearch.Proxy,
	}
	if _, key := ws.ResolveProvider(); key != "" {
		a.RegisterTool(&ws)
	}

	// WebFetch — always registered, no API key needed.
	wf := tools.WebFetchTool{
		Timeout: cfg.WebFetch.Timeout,
		Proxy:   cfg.WebFetch.Proxy,
	}
	a.RegisterTool(&wf)

	// --- MCP servers ---
	if !cfg.MCPEnabled() {
		return nil, nil
	}
	mgr := mcp.NewManager()
	mcpTools, errs := mgr.ConnectAll(ctx, cfg.MCPServers)
	for _, err := range errs {
		debuglog.Log("MCP: load error: %v", err)
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}
	for _, t := range mcpTools {
		a.RegisterTool(t)
		debuglog.Log("MCP: registered tool %s (%s)", t.Name(), t.Description())
	}
	return mgr, nil
}

// ResumeSession loads the most recent session from disk, converts it to LLM
// message format, prepends the given system prompt (if non-empty), and attaches
// the session manager to the agent for ongoing session recording.
func (a *AIAgent) ResumeSession(providerType, systemPrompt string) ([]llm.Message, []session.Message, error) {
	sm, err := session.NewManager()
	if err != nil {
		return nil, nil, fmt.Errorf("session manager: %w", err)
	}

	sessions, err := sm.List()
	if err != nil {
		return nil, nil, fmt.Errorf("list sessions: %w", err)
	}
	if len(sessions) == 0 {
		return nil, nil, fmt.Errorf("no sessions to resume")
	}

	latest := sessions[0]
	if _, err := sm.Load(latest.ID); err != nil {
		return nil, nil, fmt.Errorf("load session %s: %w", latest.ID, err)
	}

	sessionMsgs, err := sm.LoadMessages()
	if err != nil {
		return nil, nil, fmt.Errorf("load messages: %w", err)
	}

	llmMsgs, err := ConvertSessionToLLMMessages(sessionMsgs, providerType)
	if err != nil {
		return nil, nil, fmt.Errorf("convert session messages: %w", err)
	}

	if systemPrompt != "" {
		llmMsgs = append([]llm.Message{{Role: "system", Content: systemPrompt}}, llmMsgs...)
	}

	a.sessionManager = sm
	return llmMsgs, sessionMsgs, nil
}

func (b *IterationBudget) consume() bool {
	if b.Unlimited {
		return true
	}
	if b.Remaining > 0 {
		b.Remaining--
		return true
	}
	if b.Parent != nil {
		return b.Parent.consume()
	}
	return false
}
