package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

type RunResult struct {
	Response       string
	IterationsUsed int
	ExitReason     string
	Error          error
	Usage          *llm.Usage // optional: token usage from the final turn
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
	AgentEventSessionTitle     = "session_title"
	AgentEventSubagentStart    = "subagent_start"
	AgentEventSubagentDone     = "subagent_done"
	AgentEventSteerCheck       = "steer_check" // agent requests TUI to check for pending input
	AgentEventUsage            = "usage"       // incremental usage update after each API call
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
	ToolDuration  time.Duration    // Wall-clock duration of tool execution
	Questions     []tools.Question // For AskUserQuestion tool
	Result        *RunResult
	Messages      []llm.Message
	Usage         *llm.Usage
	Title         string // For AgentEventSessionTitle
}

var (
	errCancelled                  = fmt.Errorf("edit cancelled by user")
	errParallelConfirmUnsupported = fmt.Errorf("tool requiring confirmation cannot run in parallel group")
	errParallelAskUserUnsupported = fmt.Errorf("tool requiring user input cannot run in parallel group")
)

// truncateForLog caps s at maxLen characters for use in debug log messages.
// A "..." suffix is appended when truncation occurs.
func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// usageToSession converts an llm.Usage to a session.Usage for persistence.
func usageToSession(u *llm.Usage) *session.Usage {
	if u == nil {
		return nil
	}
	return &session.Usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
	}
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
		defer func() { a.steerRespCh = nil; a.skipMemory = false }()
		a.skipMemory = true // suppress memory writes for one-off runs (e.g. /commit, /init)

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

		rctx := a.buildReminderContext(true, false)
		wrappedUser := a.reminderCollector.WrapUserMessage(userMessage, rctx)
		a.lastMessageDate = rctx.Now.Format("2006-01-02")
		messages = append(messages, llm.Message{Role: "user", Content: wrappedUser})

		a.runAgentLoop(ctx, provider, messages, opts, ch)
	}()

	return ch
}

// RunConversationStream runs a streaming agent conversation loop.
// It accepts existing message history for multi-turn support.
// Returns a channel of AgentEvents that the TUI consumes.
func (a *AIAgent) RunConversationStream(ctx context.Context, history []llm.Message, userMessage string, systemPrompt string, opts llm.ChatOptions) <-chan AgentEvent {
	ch := make(chan AgentEvent, 64)

	go func() {
		defer close(ch)
		defer func() { a.steerRespCh = nil }()

		if opts.MaxTokens <= 0 {
			opts.MaxTokens = defaultMaxTokens
		}

		messages := make([]llm.Message, len(history))
		copy(messages, history)

		isFirstMessage := len(messages) == 0

		if len(messages) == 0 && systemPrompt != "" {
			messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
		}

		// When resuming a session, date/project-context/git reminders were
		// not stored — they're ephemeral by design. Re-inject them into the
		// wrapped user message (not as a separate message, which would
		// violate the user/assistant alternation requirement of LLM APIs).
		// historyHasReminder prevents duplication on subsequent turns.
		reminderIsFirst := isFirstMessage || (len(history) > 0 && !historyHasReminder(history))

		rctx := a.buildReminderContext(reminderIsFirst, false)
		wrappedUser := a.reminderCollector.WrapUserMessage(userMessage, rctx)
		a.lastMessageDate = rctx.Now.Format("2006-01-02")

		userMsg := llm.Message{Role: "user", Content: wrappedUser}

		// Attach pending images as multi-modal content parts.
		// When images are present, build ContentParts with text + images so
		// providers can format them correctly (e.g. Anthropic image blocks,
		// OpenAI multi-content arrays).
		if len(a.pendingImages) > 0 {
			parts := make([]llm.ContentPart, 0, 1+len(a.pendingImages))
			parts = append(parts, llm.ContentPart{
				Type: llm.ContentPartText,
				Text: wrappedUser,
			})
			parts = append(parts, a.pendingImages...)
			userMsg.ContentParts = parts
			a.pendingImages = nil // consumed
		}

		messages = append(messages, userMsg)

		// Session management: create session if needed and append user message
		if a.sessionManager != nil && !a.sessionManager.HasCurrent() {
			provider := a.provider.Name()
			wd, _ := os.Getwd()
			if _, err := a.sessionManager.New(provider, a.model, wd); err != nil {
				a.logger.Log("Agent: failed to create session: %v", err)
			}
			// Update logger with session ID for debug log tracking
			if cur := a.sessionManager.Current(); cur != nil {
				a.logger = a.logger.WithSessionID(cur.ID)
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
				title := a.generateTitle(ctx, userMessage)
				a.sessionManager.SetTitle(title)
				// Notify TUI immediately so statusbar can refresh before LLM finishes
				ch <- AgentEvent{Type: AgentEventSessionTitle, Title: title}
			}
		}

		a.runAgentLoop(ctx, a.provider, messages, opts, ch)
	}()

	return ch
}

// historyHasReminder checks whether the given message history already contains
// a synthetic <system-reminder> block injected by a previous resume. This
// prevents duplication when the TUI re-sends the full history on subsequent
// user messages within the same resumed session.
func historyHasReminder(history []llm.Message) bool {
	for _, msg := range history {
		if msg.Role == "user" && strings.HasPrefix(msg.Content, "<system-reminder>") {
			return true
		}
	}
	return false
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
	// Capture the final message slice (including all assistant/tool messages
	// appended during the loop) so callers can read it via GetLastMessages()
	// after the event channel is drained. The closure captures messages by
	// reference, so it sees the value at the time runAgentLoop returns.
	defer func() { a.lastMessages = messages }()

	// Inject the current session ID so it can be forwarded as the
	// x-tachi-session-id header on outgoing LLM API requests.
	if a.sessionManager != nil && a.sessionManager.Current() != nil {
		opts.SessionID = a.sessionManager.Current().ID
	}

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

		// Rebuild tool schemas each iteration. When ToolSearch is active,
		// newly discovered MCP tools are added to the list, enabling the LLM
		// to call them. The tool list is monotonic (only grows), minimizing
		// prompt cache invalidations.
		llmTools := buildLLMTools(a.filterActiveSchemas(a.toolRegistry.GetSchemas()))

		streamCh, err := provider.CreateChatStream(ctx, messages, llmTools, opts)
		if err != nil {
			ch <- AgentEvent{
				Type:   AgentEventError,
				Result: &RunResult{ExitReason: "error", IterationsUsed: apiCallCount, Error: fmt.Errorf("API call failed: %w", err)},
			}
			return
		}

		acc, err := consumeStream(streamCh, ch, apiCallCount)
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

		// --- Steer Point: inject pending user input after tool results ---
		// Only trigger after tool calls (not length continuation), to avoid
		// consecutive user messages in providers that require alternating roles.
		if (acc.finishReason == "tool_calls" || acc.finishReason == "tool_use") && a.steerRespCh != nil {
			ch <- AgentEvent{Type: AgentEventSteerCheck}
			select {
			case steerText := <-a.steerRespCh:
				if steerText != "" {
					// Use internal "steer" role — provider converters handle this
					// differently based on API protocol requirements:
					//   - Anthropic: merged into tool_result user message as text block
					//   - OpenAI: mapped to "user" role (no alternation conflict)
					messages = append(messages, llm.Message{Role: llm.RoleSteer, Content: steerText})
					a.logger.Log("Agent: steer: injected RoleSteer msg, steerText=%q", truncateForLog(steerText, 80))
					a.recordSession(&session.Message{
						Type:    session.MessageTypeUser,
						Content: steerText,
					})
				}
			case <-ctx.Done():
				return
			}
		}

		// After tool results, inject system-reminder warnings.
		if a.shouldInjectLoopReminder() {
			rctx := a.buildReminderContext(false, true)
			if block := a.reminderCollector.Collect(rctx); block != "" {
				messages = append(messages, llm.Message{Role: "user", Content: block})
				a.logger.Log("Agent: loop reminder injected, block=%q", truncateForLog(block, 200))
			}
		}
	}
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
		// Record assistant response with usage (always record when we have text or usage)
		if acc.text.Len() > 0 || acc.usage != nil {
			a.recordSession(&session.Message{
				Type:    session.MessageTypeAssistant,
				Content: acc.text.String(),
				Usage:   usageToSession(acc.usage),
			})
		}

		*messages = append(*messages, acc.assistantMessage())

		// Emit incremental usage update after each tool-call API round
		// so the TUI can update totalUsage and status bar in real time.
		if acc.usage != nil {
			ch <- AgentEvent{Type: AgentEventUsage, Usage: acc.usage}
		}

		toolMsgs, err := a.executeToolCalls(ctx, acc.toolCalls, ch)
		if err != nil {
			ch <- AgentEvent{
				Type:   AgentEventError,
				Result: &RunResult{ExitReason: "cancelled", Error: err},
			}
			return false
		}
		a.logger.Log("Agent: executeToolCalls returned %d tool messages for %d tool calls",
			len(toolMsgs), len(acc.toolCalls))
		*messages = append(*messages, toolMsgs...)
		*lengthRetries = 0
		return true

	case "max_tokens", "length":
		*lengthRetries++
		a.logger.Log("Agent: text=%s, finish_reason=%s, continuation retry %d/%d", acc.text.String(), acc.finishReason, *lengthRetries, maxLengthContinueRetries)

		// Record thinking blocks in session
		for _, tb := range acc.thinkBlocks {
			a.recordSession(&session.Message{
				Type:      session.MessageTypeThinking,
				Content:   tb.Thinking,
				Signature: tb.Signature,
			})
		}
		// Record partial assistant response with usage
		if acc.text.Len() > 0 || acc.usage != nil {
			a.recordSession(&session.Message{
				Type:    session.MessageTypeAssistant,
				Content: acc.text.String(),
				Usage:   usageToSession(acc.usage),
			})
		}

		// Append the assistant message to history so it is preserved for
		// session resume — whether we continue or stop exhausted.
		msg := acc.assistantMessage()
		// API protocol requires every tool_use to be paired with a
		// tool_result; truncated tool calls that were never executed cannot
		// satisfy this constraint, so we drop them and guide the model to
		// retry via the context-aware continuation prompt below.
		if len(msg.ToolCalls) > 0 {
			msg.ToolCalls = nil
		}
		*messages = append(*messages, msg)

		if *lengthRetries >= maxLengthContinueRetries {
			a.logger.Log("Agent: length continuation exhausted after %d retries", maxLengthContinueRetries)
			// Return the partial output as a normal turn completion instead
			// of an error — the user already saw the text streaming, and
			// discarding it (or showing a red error) is worse than delivering
			// what we have with a note that it was truncated.
			ch <- AgentEvent{
				Type:     AgentEventTurnComplete,
				Messages: *messages,
				Usage:    acc.usage,
				Result: &RunResult{
					Response:       acc.text.String(),
					IterationsUsed: apiCallCount,
					ExitReason:     "length_exhausted",
					Error:          fmt.Errorf("response truncated after %d continuation attempts", maxLengthContinueRetries),
					Usage:          acc.usage,
				},
			}

			// Store turn-level memory after a truncated response
			a.storeTurnMemory(collectTurnMessages(messages, acc.text.String()))

			return false
		}

		// Record continuation prompt (original, unwrapped)
		var continuationText string
		if len(acc.toolCalls) > 0 {
			continuationText = "Your previous tool call was interrupted by the output token limit. Please retry the tool call."
		} else if len(acc.thinkBlocks) > 0 && acc.text.Len() == 0 {
			continuationText = "Please continue with your response. Break your output into smaller chunks to avoid hitting the output token limit."
		} else {
			continuationText = "Please continue where you left off. Break your output into smaller chunks to avoid hitting the output token limit."
		}
		a.recordSession(&session.Message{
			Type:    session.MessageTypeUser,
			Content: continuationText,
		})

		// Track input tokens for token-warning reminders
		if acc.usage != nil {
			a.lastInputTokens = acc.usage.InputTokens
		}

		// Wrap the continuation message with reminders
		rctx := a.buildReminderContext(false, false)
		wrappedContinuation := a.reminderCollector.WrapUserMessage(continuationText, rctx)

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
		// Record assistant response with usage
		if acc.text.Len() > 0 || acc.usage != nil {
			a.recordSession(&session.Message{
				Type:    session.MessageTypeAssistant,
				Content: acc.text.String(),
				Usage:   usageToSession(acc.usage),
			})
		}

		ch <- AgentEvent{
			Type: AgentEventTurnComplete, Messages: *messages, Usage: acc.usage,
			Result: &RunResult{Response: acc.text.String(), IterationsUsed: apiCallCount, ExitReason: "stop", Usage: acc.usage},
		}

		// Store turn-level memory after a complete response
		a.storeTurnMemory(collectTurnMessages(messages, acc.text.String()))

		return false
	}
}

// filterActiveSchemas filters tool schemas for the LLM API call.
// When ToolSearch is active (deferred pool non-empty):
//   - Built-in tools are always included
//   - The MCPSearchTools tool is always included
//   - MCP tools are only included if they've been discovered by the LLM
//
// When ToolSearch is not active (no MCP manager, e.g. no MCP servers):
//   - All tools are included (unchanged behavior)
func (a *AIAgent) filterActiveSchemas(schemas []tools.Schema) []tools.Schema {
	pool := a.DeferredPool()
	set := a.discoveredSet()
	if pool == nil || pool.Len() == 0 {
		// ToolSearch not active — include all schemas as-is
		return schemas
	}

	active := make([]tools.Schema, 0, len(schemas))
	seen := make(map[string]bool)

	for _, s := range schemas {
		name := s.Name
		switch {
		case !tools.IsMCPSchema(name):
			// Built-in tools are always included
			active = append(active, s)
			seen[name] = true
		case tools.IsMCPSearchTool(name):
			// The search tool itself is always included
			active = append(active, s)
			seen[name] = true
		case set != nil && set.Contains(name):
			// Discovered MCP tools are included
			active = append(active, s)
			seen[name] = true
		default:
			// Undiscovered MCP tools — excluded from LLM API call
		}
	}

	// Merge discovered tools that are in deferred pool but not yet registered.
	// This handles the gap between MCPSearchTools discovery and the next
	// filterActiveSchemas call: the tool may be in discoveredSet but not yet
	// in the Registry (lazy registration happens at Invoke time).
	if set != nil {
		for _, name := range set.List() {
			if seen[name] {
				continue
			}
			dt := pool.Get(name)
			if dt != nil {
				active = append(active, dt.Schema)
				seen[name] = true
			}
		}
	}

	return active
}

// buildLLMTools converts tool schemas from the agent's registry into the
// llm.Tool format understood by provider APIs.
func buildLLMTools(toolSchemas []tools.Schema) []llm.Tool {
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
func (a *AIAgent) buildReminderContext(isFirstMessage bool, isToolResult bool) systemreminder.Context {
	iterLeft := 0
	if a.iterationBudget != nil {
		iterLeft = a.iterationBudget.Remaining
	}
	return systemreminder.Context{
		IsFirstMessage:  isFirstMessage,
		IterationsLeft:  iterLeft,
		MaxIterations:   a.maxIterations,
		InputTokens:     a.lastInputTokens,
		ContextWindow:   a.contextWindow,
		Now:             time.Now(),
		LastMessageDate: a.lastMessageDate,
		IsToolResult:    isToolResult,
		SkipRecall:      a.skipMemoryRecall,
	}
}

// shouldInjectLoopReminder returns true when we should inject a
// system-reminder user message before the next API call.
// Currently true whenever iteration or token warnings are likely to be
// active. The actual filtering is done by Collect().
func (a *AIAgent) shouldInjectLoopReminder() bool {
	return a.reminderCollector != nil
}
