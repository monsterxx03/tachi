package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

type RunResult struct {
	Response       string
	IterationsUsed int
	Duration       time.Duration // total wall-clock duration of the turn (excludes subagent internal iterations)
	ExitReason     string
	Error          error
	Usage          *llm.Usage // optional: token usage from the final turn
	TraceID        string     // trace ID for this turn, for log correlation
}

// FormatTurnSummary returns a concise human-readable turn summary string
// suitable for appending to the assistant's response. It includes the number
// of iterations (API calls), wall-clock duration, and optionally the trace ID.
// Returns empty string when all values are zero/empty.
func FormatTurnSummary(iterations int, duration time.Duration, traceID string) string {
	if iterations <= 0 && duration <= 0 && traceID == "" {
		return ""
	}
	var parts []string
	if iterations > 0 {
		parts = append(parts, fmt.Sprintf("%d 次迭代", iterations))
	}
	if duration > 0 {
		parts = append(parts, formatTurnDuration(duration))
	}
	if traceID != "" {
		parts = append(parts, fmt.Sprintf("trace: %s", traceID))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("\n\n*(回合: %s)*", strings.Join(parts, ", "))
}

// formatTurnDuration formats a time.Duration as a concise human-readable string
// without parentheses (unlike the display-oriented variant in tui/chatview.go).
func formatTurnDuration(d time.Duration) string {
	if d < time.Millisecond {
		return "<1ms"
	}
	if d < time.Second {
		return fmt.Sprintf("%.0fms", float64(d.Microseconds())/1000)
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	minutes := int(d.Minutes())
	seconds := d.Seconds() - float64(minutes*60)
	return fmt.Sprintf("%dm%.0fs", minutes, seconds)
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
	AgentEventSteerCheck       = "steer_check"        // agent requests TUI to check for pending input
	AgentEventUsage            = "usage"              // incremental usage update after each API call
	AgentEventAutoCompactStart = "auto_compact_start" // agent is about to begin auto-compaction
	AgentEventAutoCompactDone  = "auto_compact_done"  // agent completed auto-compaction
	AgentEventSubagentToolCall = "subagent_tool_call" // real-time subagent internal tool call
)

type AgentEvent struct {
	Type             string
	TextDelta        string
	ThinkingDelta    string
	ToolName         string
	ToolID           string
	ToolArgs         string
	ToolResult       string
	ToolIsError      bool
	ToolDiff         string
	ToolDuration     time.Duration    // Wall-clock duration of tool execution
	Questions        []tools.Question // For AskUserQuestion tool
	Result           *RunResult
	Messages         []llm.Message
	Usage            *llm.Usage
	Title            string // For AgentEventSessionTitle
	CompactSummary   string // For AgentEventAutoCompactDone: LLM-generated summary
	OldMsgCount      int    // For AgentEventAutoCompactDone: message count before compact
	IterCount        int    // For AgentEventSubagentDone: sub-agent iteration count
	SubagentToolName string // For AgentEventSubagentToolCall: internal tool name
	SubagentToolDone bool   // For AgentEventSubagentToolCall: true if tool completed
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

// hasBashCall returns true if any tool call in the batch is Bash.
// Used to trigger LSP file cleanup after destructive filesystem operations.
func hasBashCall(calls []llm.ToolCall) bool {
	for _, tc := range calls {
		if tc.Function.Name == tools.ToolNameBash {
			return true
		}
	}
	return false
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

// recordAssistantTurn persists an assistant response (text, usage, thinking
// blocks) to the session store. Safe to call with zero values.
func (a *AIAgent) recordAssistantTurn(text string, usage *llm.Usage, thinkBlocks []llm.ThinkingBlock) {
	for _, tb := range thinkBlocks {
		a.recordSession(&session.Message{
			Type:      session.MessageTypeThinking,
			Content:   tb.Thinking,
			Signature: tb.Signature,
		})
	}
	if text != "" || usage != nil {
		su := usageToSession(usage)
		if su != nil {
			su.EstimatedInputTokens = a.lastInputTokens
		}
		a.recordSession(&session.Message{
			Type:    session.MessageTypeAssistant,
			Content: text,
			Usage:   su,
		})
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

		// Save and restore lastInputTokens so one-off calls don't
		// pollute the main conversation's context estimate (used by
		// TokenWarningReminder and the TUI statusbar context fraction).
		savedTokens := a.lastInputTokens
		defer func() { a.lastInputTokens = savedTokens }()

		if a.memory != nil {
			defer func() { a.memory.SkipWrites = false }()
			a.memory.SkipWrites = true // suppress memory writes for one-off runs (e.g. /commit, /init)
		}

		// Suppress session persistence for one-off runs.
		// One-off tasks (/commit, /review, sub-agents, dreams) produce
		// messages that are internal tooling artifacts — they must not
		// persist into the main conversation history.
		a.skipSessionWrites = true
		defer func() { a.skipSessionWrites = false }()

		if provider == nil {
			provider = a.provider
		}

		if opts.MaxTokens <= 0 {
			opts.MaxTokens = DefaultMaxTokens
		}

		// Build fresh messages: system + wrapped user message, no history
		messages := make([]llm.Message, 0, 2)
		if systemPrompt != "" {
			messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
		}

		rctx := a.buildReminderContext(true, false)
		wrappedUser := a.reminderCollector.WrapUserMessage(ctx, userMessage, rctx)
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
			opts.MaxTokens = DefaultMaxTokens
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
		wrappedUser := a.reminderCollector.WrapUserMessage(ctx, userMessage, rctx)
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
			providerName := a.provider.Name()
			if a.cfg != nil {
				if pn := config.ResolveProviderName(a.cfg); pn != "" {
					providerName = pn
				}
			}
			wd, _ := os.Getwd()
			if _, err := a.sessionManager.New(providerName, wd); err != nil {
				a.logger.Logf(ctx, "Agent: failed to create session: %v", err)
			}
			// Update logger with session ID for debug log tracking
			if cur := a.sessionManager.Current(); cur != nil {
				a.logger = a.logger.With("session_id", cur.ID)
			}
			// Notify memory backend that a new session has started
			a.StartSessionMemory()
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

		a.EstimateAndUpdateTokens(messages)
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
	// Record the turn start time for Duration tracking in RunResult.
	a.turnStart = time.Now()

	// Generate a trace ID for this turn and inject it into the context.
	// The logger's textHandler extracts trace_id from ctx automatically,
	// so all log calls within this turn will carry it — no need to mutate a.logger.
	traceID := logger.NewTraceID()
	a.turnTraceID = traceID
	ctx = logger.WithTraceID(ctx, traceID)
	ctx = logger.WithLogger(ctx, a.logger)

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
	// Also inject session ID into context for tool execution (so SavePlan
	// and other tools can associate their output with the current session).
	if a.sessionManager != nil && a.sessionManager.Current() != nil {
		ctx = tools.WithSessionID(ctx, a.sessionManager.Current().ID)
	}

	apiCallCount := 0
	lengthContinueRetries := 0

	a.iterationBudget = NewIterationBudget(a.maxIterations)

	for {
		if !a.iterationBudget.consume() {
			ch <- AgentEvent{
				Type:   AgentEventError,
				Result: &RunResult{ExitReason: "budget_exhausted", IterationsUsed: apiCallCount, Duration: time.Since(a.turnStart), Error: fmt.Errorf("iteration budget exhausted")},
			}
			return
		}

		select {
		case <-ctx.Done():
			ch <- AgentEvent{
				Type:     AgentEventError,
				Messages: messages,
				Result:   &RunResult{ExitReason: "interrupted", IterationsUsed: apiCallCount, Duration: time.Since(a.turnStart), Error: ctx.Err()},
			}
			return
		default:
		}

		// ── Auto-compact check (before LLM call) ──
		// Check happens at the loop top so it fires regardless of the
		// previous iteration's finish reason (tool_calls, stop, length).
		// Compaction replaces messages with a shorter history, and the
		// next iteration continues normally with the new context.
		if a.shouldAutoCompact() {
			ch <- AgentEvent{Type: AgentEventAutoCompactStart}
			summary, newHistory, err := a.doCompact(ctx, messages)
			if err != nil {
				a.logger.Logf(ctx, "Auto compact failed: %v", err)
				ch <- AgentEvent{Type: AgentEventAutoCompactDone, Result: &RunResult{Error: err}}
				// Non-fatal: continue with the existing (over-large)
				// history. The next iteration will try again — eventual
				// success if the LLM responds before hitting the limit.
			} else {
				oldMsgCount := len(messages)
				messages = newHistory
				a.setCompactCooldown()
				a.logger.Logf(ctx, "Auto compact completed, new history has %d messages", len(messages))
				ch <- AgentEvent{
					Type:           AgentEventAutoCompactDone,
					CompactSummary: summary,
					OldMsgCount:    oldMsgCount,
				}
			}
			continue // next iteration with new (or original) history
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
				Result: &RunResult{ExitReason: "error", IterationsUsed: apiCallCount, Duration: time.Since(a.turnStart), Error: fmt.Errorf("API call failed: %w", err)},
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
				Result: &RunResult{ExitReason: exitReason, IterationsUsed: apiCallCount, Duration: time.Since(a.turnStart), Error: err},
			}
			return
		}

		if !a.handleFinishReason(ctx, acc, &messages, ch, apiCallCount, &lengthContinueRetries) {
			return
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
	switch acc.finishReason {
	case "tool_calls", "tool_use":
		return a.handleToolCallFinish(ctx, acc, messages, ch, apiCallCount, lengthRetries)
	case "max_tokens", "length":
		return a.handleLengthFinish(ctx, acc, messages, ch, apiCallCount, lengthRetries)
	default:
		return a.handleStopFinish(ctx, acc, messages, ch, apiCallCount, lengthRetries)
	}
}

// handleToolCallFinish processes a tool-call response: records the assistant
// turn, executes tools, handles steer input, and injects loop reminders.
func (a *AIAgent) handleToolCallFinish(
	ctx context.Context,
	acc *streamAccumulator,
	messages *[]llm.Message,
	ch chan<- AgentEvent,
	_ int,
	lengthRetries *int,
) bool {
	a.recordAssistantTurn(acc.text.String(), acc.usage, acc.thinkBlocks)

	*messages = append(*messages, acc.assistantMessage())

	// Emit incremental usage update after each tool-call API round
	// so the TUI can update totalUsage and status bar in real time.
	if acc.usage != nil {
		ch <- AgentEvent{Type: AgentEventUsage, Usage: acc.usage}
	}

	toolMsgs, err := a.executeToolCalls(ctx, acc.toolCalls, ch)
	if err != nil {
		ch <- AgentEvent{
			Type:     AgentEventError,
			Messages: *messages,
			Result:   &RunResult{ExitReason: "cancelled", Duration: time.Since(a.turnStart), Error: err},
		}
		return false
	}
	a.logger.Logf(ctx, "Agent: executeToolCalls returned %d tool messages for %d tool calls",
		len(toolMsgs), len(acc.toolCalls))
	*messages = append(*messages, toolMsgs...)
	*lengthRetries = 0

	// --- LSP File Sync: sync modified files to LSP servers ---
	if a.lspManager != nil {
		for _, tc := range acc.toolCalls {
			if fp := tools.ExtractFilePath(tc.Function.Name, tc.Function.Arguments); fp != "" {
				if syncErr := a.lspManager.SyncFile(ctx, fp); syncErr != nil {
					a.logger.Logf(ctx, "LSP: file sync error for %s: %v", fp, syncErr)
				}
			}
		}
		// Close any open files that no longer exist on disk (deleted/renamed/moved
		// by tool operations like Bash rm/mv). This keeps the LSP server's file
		// index consistent with the actual filesystem.
		if hasBashCall(acc.toolCalls) {
			a.lspManager.CloseMissingFiles(ctx)
		}
		// Wait briefly for async diagnostics to arrive after file sync.
		a.lspManager.WaitForDiagnostics(ctx, 2*time.Second)
	}

	// --- Steer Point: inject pending user input after tool results ---
	if a.steerRespCh != nil {
		ch <- AgentEvent{Type: AgentEventSteerCheck}
		select {
		case steerText := <-a.steerRespCh:
			if steerText != "" {
				*messages = append(*messages, llm.Message{Role: llm.RoleSteer, Content: steerText})
				a.logger.Logf(ctx, "Agent: steer: injected RoleSteer msg, steerText=%q", truncateForLog(steerText, 80))
				a.recordSession(&session.Message{
					Type:    session.MessageTypeUser,
					Content: steerText,
				})
			}
		case <-ctx.Done():
			return false
		}
	}

	// --- Loop Reminders: inject iteration/token warnings ---
	a.EstimateAndUpdateTokens(*messages)
	rctx := a.buildReminderContext(false, true)
	// Populate tool names so reminders (e.g. LSPDiagnostics) can filter by tool.
	rctx.ToolNames = make([]string, 0, len(acc.toolCalls))
	for _, tc := range acc.toolCalls {
		rctx.ToolNames = append(rctx.ToolNames, tc.Function.Name)
	}
	if block := a.reminderCollector.Collect(ctx, rctx); block != "" {
		*messages = append(*messages, llm.Message{Role: "user", Content: block})
		a.logger.Logf(ctx, "Agent: loop reminder injected, block=%q", truncateForLog(block, 200))
	}

	return true
}

const maxLengthContinueRetries = 3

// handleLengthFinish processes a truncated (length/max_tokens) response:
// records partial output, appends a continuation prompt, and handles
// exhaustion after too many retries.
func (a *AIAgent) handleLengthFinish(
	ctx context.Context,
	acc *streamAccumulator,
	messages *[]llm.Message,
	ch chan<- AgentEvent,
	apiCallCount int,
	lengthRetries *int,
) bool {
	*lengthRetries++
	a.logger.Logf(context.Background(), "Agent: text=%s, finish_reason=%s, continuation retry %d/%d", acc.text.String(), acc.finishReason, *lengthRetries, maxLengthContinueRetries)

	a.recordAssistantTurn(acc.text.String(), acc.usage, acc.thinkBlocks)

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
		a.logger.Logf(context.Background(), "Agent: length continuation exhausted after %d retries", maxLengthContinueRetries)
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
				Duration:       time.Since(a.turnStart),
				ExitReason:     "length_exhausted",
				Error:          fmt.Errorf("response truncated after %d continuation attempts", maxLengthContinueRetries),
				Usage:          acc.usage,
				TraceID:        a.turnTraceID,
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

	// Wrap the continuation message with reminders
	rctx := a.buildReminderContext(false, false)
	wrappedContinuation := a.reminderCollector.WrapUserMessage(ctx, continuationText, rctx)

	*messages = append(*messages, llm.Message{Role: "user", Content: wrappedContinuation})
	return true
}

// handleStopFinish processes a normal stop response: records the assistant
// turn, emits TurnComplete, and stores turn-level memory.
func (a *AIAgent) handleStopFinish(
	_ context.Context,
	acc *streamAccumulator,
	messages *[]llm.Message,
	ch chan<- AgentEvent,
	apiCallCount int,
	lengthRetries *int,
) bool {
	*lengthRetries = 0
	msg := acc.assistantMessage()
	msg.ToolCalls = nil
	*messages = append(*messages, msg)

	a.recordAssistantTurn(acc.text.String(), acc.usage, acc.thinkBlocks)

	ch <- AgentEvent{
		Type: AgentEventTurnComplete, Messages: *messages, Usage: acc.usage,
		Result: &RunResult{Response: acc.text.String(), IterationsUsed: apiCallCount, Duration: time.Since(a.turnStart), ExitReason: "stop", Usage: acc.usage, TraceID: a.turnTraceID},
	}

	// Store turn-level memory after a complete response
	a.storeTurnMemory(collectTurnMessages(messages, acc.text.String()))

	return false
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
		SkipRecall:      a.memory != nil && a.memory.SkipRecall,
		SessionID:       a.sessionID(),
	}
}

// sessionID returns the current session's ID, or empty string if no session.
func (a *AIAgent) sessionID() string {
	if a.sessionManager != nil && a.sessionManager.Current() != nil {
		return a.sessionManager.Current().ID
	}
	return ""
}
