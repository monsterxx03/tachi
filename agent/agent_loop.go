package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/hooks"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
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

// Exit reasons for RunResult.ExitReason — the terminal-outcome protocol
// shared with frontends (TUI, ACP, CLI exit codes). Compare against these
// constants rather than literals.
const (
	ExitReasonStop            = "stop"
	ExitReasonError           = "error"
	ExitReasonInterrupted     = "interrupted"
	ExitReasonCancelled       = "cancelled"
	ExitReasonBudgetExhausted = "budget_exhausted"
	ExitReasonLengthExhausted = "length_exhausted"
)

// Finish reasons reported by providers on stream completion, as normalized
// per provider (OpenAI / Anthropic variants listed together).
const (
	finishReasonToolCalls = "tool_calls" // OpenAI
	finishReasonToolUse   = "tool_use"   // Anthropic
	finishReasonMaxTokens = "max_tokens" // Anthropic
	finishReasonLength    = "length"     // OpenAI
)

var (
	errCancelled                  = fmt.Errorf("edit cancelled by user")
	errParallelConfirmUnsupported = fmt.Errorf("tool requiring confirmation cannot run in parallel group")
	errParallelAskUserUnsupported = fmt.Errorf("tool requiring user input cannot run in parallel group")
)

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
func (a *AIAgent) recordAssistantTurn(rs *RunState, text string, usage *llm.Usage, thinkBlocks []llm.ThinkingBlock) {
	for _, tb := range thinkBlocks {
		a.recordSession(rs, &session.Message{
			Type:      session.MessageTypeThinking,
			Content:   tb.Thinking,
			Signature: tb.Signature,
		})
	}
	if text != "" || usage != nil {
		su := usageToSession(usage)
		if su != nil {
			// One-off runs never update the conversation estimate —
			// leave 0 rather than attributing the main conversation's
			// estimate to a sidecar transcript.
			if !rs.SkipSessionWrites {
				su.EstimatedInputTokens = a.conv.tokens()
			}
		}
		a.recordSession(rs, &session.Message{
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
			a.ConfirmTool(ConfirmAllowOnce)
		}
	}
	if result == nil {
		result = &RunResult{ExitReason: ExitReasonError, Error: fmt.Errorf("no result received")}
	}
	return result
}

// RunOneOffStream runs a single-turn streaming conversation with a clean
// history (no inherited messages) using the given provider. No session
// recording is performed — this is for one-off tasks like /commit or /init.
// If provider is nil, falls back to a.Config.Provider.
//
// meta controls the one-off transcript sidecar (empty Kind = no recording):
// the run's full execution is written to a sidecar JSONL file, keeping it
// out of the main session history while leaving a trail for troubleshooting.
// See docs/2026-07-24-oneoff-transcript-design.md.
// ropts optionally restrict the tool set for this run (e.g. WithToolSet for
// /commit). The registry itself is untouched; see agent/toolview.go.
func (a *AIAgent) RunOneOffStream(
	ctx context.Context,
	provider llm.Provider,
	systemPrompt string,
	userMessage string,
	opts llm.ChatOptions,
	ropts ...RunOption,
) <-chan AgentEvent {
	ch := make(chan AgentEvent, 64)

	go func() {
		defer close(ch)

		params := applyRunOptions(ropts)

		if provider == nil {
			provider = a.Config.Provider
		}

		if opts.MaxTokens <= 0 {
			opts.MaxTokens = DefaultMaxTokens
		}

		// Fresh history (nil) — one-off runs never inherit messages.
		messages, reminderBlock := a.prepareTurnMessages(ctx, nil, userMessage, systemPrompt)

		// Create RunState with SkipSessionWrites=true. One-off runs never
		// publish their RunState to a.currentRun — the main conversation's
		// state (messages, token estimate) stays untouched.
		rs := &RunState{
			Budget:            NewIterationBudget(a.Config.MaxIterations),
			SkipSessionWrites: true,
		}

		// Set up one-off recorder via WithOneOffMeta when the caller wants
		// a sidecar transcript (e.g. /commit, /review, github, dream).
		if params != nil && params.oneoffMeta != nil {
			params.oneoffMeta.SystemPrompt = systemPrompt
			rs.OneoffRec = a.startOneoffRecorder(ctx, *params.oneoffMeta, provider)
		}
		defer func() {
			if rs.OneoffRec != nil {
				a.stopOneoffRecorder(ctx, rs)
			}
		}()

		traceID := logger.NewTraceID()
		rs.begin(traceID)
		ctx = logger.WithTraceID(ctx, traceID)
		ctx = logger.WithLogger(ctx, a.Config.Logger)

		// Record the user turn to the sidecar (no-op without a recorder —
		// rs.SkipSessionWrites is always true here, so this never touches
		// the main session history).
		a.recordUserTurn(rs, userMessage, reminderBlock)

		// Fire turn_start hook (paired with turn_complete/turn_truncated in runLoop)
		a.dispatchEvent(ctx, hooks.EventTurnStart, hooks.Payload{
			UserMessage: userMessage,
		})

		a.runLoop(ctx, rs, &runInput{
			Provider: provider,
			Messages: messages,
			Opts:     opts,
			Params:   params,
		}, ch)
	}()

	return ch
}

// RunConversationStream runs a streaming agent conversation loop.
// It accepts existing message history for multi-turn support.
// Returns a channel of AgentEvents that the TUI consumes.
//
// ropts optionally restrict the tool set for this run (e.g. WithNoTools for
// /compact). The registry itself is untouched; see agent/toolview.go.
func (a *AIAgent) RunConversationStream(ctx context.Context, history []llm.Message, userMessage string, systemPrompt string, opts llm.ChatOptions, ropts ...RunOption) <-chan AgentEvent {
	ch := make(chan AgentEvent, 64)

	go func() {
		defer close(ch)

		params := applyRunOptions(ropts)

		if opts.MaxTokens <= 0 {
			opts.MaxTokens = DefaultMaxTokens
		}

		messages, reminderBlock := a.prepareTurnMessages(ctx, history, userMessage, systemPrompt)

		// Attach pending images from params as multi-modal content parts
		// on the trailing user message, so providers can format them correctly (e.g.
		// Anthropic image blocks, OpenAI multi-content arrays).
		if params != nil && len(params.pendingImages) > 0 {
			userMsg := &messages[len(messages)-1]
			parts := make([]llm.ContentPart, 0, 1+len(params.pendingImages))
			parts = append(parts, llm.ContentPart{
				Type: llm.ContentPartText,
				Text: userMsg.Content,
			})
			parts = append(parts, params.pendingImages...)
			userMsg.ContentParts = parts
		}

		rs := &RunState{
			Budget: NewIterationBudget(a.Config.MaxIterations),
		}

		// WithOneOffMeta (channel ambient turns): the run is recorded to a
		// sidecar transcript and kept out of the main session history.
		// Set up BEFORE ensureSessionAndRecordUser so the user turn lands
		// in the sidecar too.
		if params != nil && params.oneoffMeta != nil {
			params.oneoffMeta.SystemPrompt = systemPrompt
			rs.OneoffRec = a.startOneoffRecorder(ctx, *params.oneoffMeta, a.Config.Provider)
			rs.SkipSessionWrites = true
		}
		defer func() {
			if rs.OneoffRec != nil {
				a.stopOneoffRecorder(ctx, rs)
			}
		}()

		traceID := logger.NewTraceID()
		rs.begin(traceID)
		ctx = logger.WithTraceID(ctx, traceID)
		ctx = logger.WithLogger(ctx, a.Config.Logger)

		a.mu.Lock()
		a.currentRun = rs
		a.mu.Unlock()

		a.ensureSessionAndRecordUser(ctx, rs, userMessage, reminderBlock, ch)

		// Fire turn_start hook before entering agent loop
		a.dispatchEvent(ctx, hooks.EventTurnStart, hooks.Payload{
			UserMessage: userMessage,
		})

		a.EstimateAndUpdateTokens(rs, messages)
		a.runLoop(ctx, rs, &runInput{
			Provider: a.Config.Provider,
			Messages: messages,
			Opts:     opts,
			Params:   params,
		}, ch)
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

// prepareTurnMessages builds the initial message slice for a turn: copies
// history, ensures the system prompt is present, collects the system-reminder
// block, and appends the reminder-wrapped user message. Returns the messages
// and the raw reminder block (for session recording, which stores the block
// separately from the user text).
func (a *AIAgent) prepareTurnMessages(
	ctx context.Context,
	history []llm.Message,
	userMessage string,
	systemPrompt string,
) (msgs []llm.Message, reminderBlock string) {
	messages := make([]llm.Message, len(history))
	copy(messages, history)

	isFirstMessage := len(messages) == 0

	if systemPrompt != "" {
		if len(messages) == 0 {
			messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
		} else if messages[0].Role != "system" {
			// History loaded from disk (e.g. after channel mode agent eviction via /model)
			// doesn't include the ephemeral system prompt. Prepend it so the LLM receives
			// full context. When history already starts with a system message (cached agent
			// path), keep the existing one — it will be updated on next GetLastMessages.
			withSystem := make([]llm.Message, 1, 1+len(messages))
			withSystem[0] = llm.Message{Role: "system", Content: systemPrompt}
			messages = append(withSystem, messages...)
		}
	}

	// When resuming a session, date/project-context/git reminders were
	// not stored — they're ephemeral by design. Re-inject them into the
	// wrapped user message (not as a separate message, which would
	// violate the user/assistant alternation requirement of LLM APIs).
	// historyHasReminder prevents duplication on subsequent turns.
	reminderIsFirst := isFirstMessage || (len(history) > 0 && !historyHasReminder(history))

	rctx := a.buildReminderContext(reminderIsFirst, false)
	rctx.CurrentPrompt = userMessage
	reminderBlock = a.collectReminders(ctx, rctx)
	wrappedUser := userMessage
	if reminderBlock != "" {
		wrappedUser = reminderBlock + userMessage
	}
	a.conv.setMessageDate(rctx.Now.Format("2006-01-02"))

	messages = append(messages, llm.Message{Role: "user", Content: wrappedUser})
	return messages, reminderBlock
}

// recordUserTurn persists the reminder block (if any) and the original user
// message to the session, in the same order the LLM sees them (reminder
// prepended to the user message). The original user text is stored without
// the system-reminder wrapper.
func (a *AIAgent) recordUserTurn(rs *RunState, userMessage, reminderBlock string) {
	if reminderBlock != "" {
		a.recordSession(rs, &session.Message{
			Type:    session.MessageTypeReminder,
			Content: reminderBlock,
		})
	}
	a.recordSession(rs, &session.Message{
		Type:    session.MessageTypeUser,
		Content: userMessage,
	})
}

// ensureSessionAndRecordUser creates the session on first use (with
// session_start hook), records the user turn, and generates a title for
// brand-new sessions. No-op when no session manager is configured.
func (a *AIAgent) ensureSessionAndRecordUser(
	ctx context.Context,
	rs *RunState,
	userMessage string,
	reminderBlock string,
	ch chan<- AgentEvent,
) {
	if a.Config.SessionManager == nil {
		return
	}

	if !a.Config.SessionManager.HasCurrent() {
		providerName := a.Config.Provider.Name()
		if a.Config.FullConfig != nil {
			if pn := config.ResolveProviderName(a.Config.FullConfig); pn != "" {
				// Resolve alias to the actual provider name for session storage,
				// so that session metadata and /usage show the real provider name.
				providerName = a.Config.FullConfig.ResolveAlias(pn)
			}
		}
		wd, _ := os.Getwd()
		if _, err := a.Config.SessionManager.New(providerName, wd); err != nil {
			a.Config.Logger.Error(ctx, "Agent: failed to create session", err)
		}
		// Update logger with session ID for debug log tracking
		if cur := a.Config.SessionManager.Current(); cur != nil {
			a.Config.Logger = a.Config.Logger.With("session_id", cur.ID)

			// Inherit a pending per-session thinking override set via /thinking
			// before a session existed (e.g. right after startup): write it into
			// the new session's meta so the first turn uses it, then clear it.
			if p := a.Config.PendingSessionThinking; p != "" {
				cur.ThinkingLevel = p
				cur.UpdatedAt = time.Now()
				if err := a.Config.SessionManager.UpdateMeta(cur); err != nil {
					a.Config.Logger.Error(ctx, "Agent: failed to persist pending thinking override", err)
				}
				a.Config.PendingSessionThinking = ""
			}
		}

		// Fire session_start hook
		a.dispatchEvent(ctx, hooks.EventSessionStart, hooks.Payload{
			Provider: providerName,
		})
	}

	a.recordUserTurn(rs, userMessage, reminderBlock)

	// Set title from first user message (LLM-generated or truncated)
	if curr := a.Config.SessionManager.Current(); curr != nil && curr.Title == "" {
		title := a.generateTitle(ctx, userMessage)
		a.Config.SessionManager.SetTitle(title)
		// Notify TUI immediately so statusbar can refresh before LLM finishes
		ch <- AgentEvent{Type: AgentEventSessionTitle, Title: title}
	}
}

// runInput aggregates the per-run inputs of runLoop: the provider to call,
// the initial message slice, chat options, and the parsed RunOption set
// (tool view, pending images, steer channel, one-off meta).
type runInput struct {
	Provider llm.Provider
	Messages []llm.Message
	Opts     llm.ChatOptions
	Params   *runParams
}

// runLoop is the shared event loop used by both RunConversationStream
// and RunOneOffStream. It handles iteration budgets, streaming, tool
// execution, length continuation, and system-reminder injection.
//
// State lives at two levels: RunState (per-run; Messages is mu-guarded for
// concurrent readers, the rest is loop-goroutine only) and long-lived
// AIAgent fields (Config / conv / currentRun). Handlers emit their own
// terminal events; the loop itself only routes control flow via loopOutcome.
func (a *AIAgent) runLoop(
	ctx context.Context,
	rs *RunState,
	in *runInput,
	ch chan<- AgentEvent,
) {
	// Apply the per-run tool view (if any) before anything reads the tool set.
	// Carrying it on the context rather than on the agent keeps it immutable
	// and scoped to this goroutine — nothing to restore, nothing to race on.
	ctx = withToolView(ctx, buildToolView(in.Params))

	opts := in.Opts

	// Apply the agent-level default thinking config when the caller didn't
	// specify one explicitly. This makes the model's thinking_level config
	// (resolved into AgentConfig.Thinking/ThinkingEffort) effective across
	// all frontends without touching every call site — while explicit
	// overrides (/commit disables thinking, /review opts) keep priority.
	if opts.Thinking == nil {
		opts.Thinking = a.Config.Thinking
	}
	if opts.ThinkingEffort == "" {
		opts.ThinkingEffort = a.Config.ThinkingEffort
	}

	// Inject the current session ID so it can be forwarded as the
	// x-tachi-session-id header on outgoing LLM API requests.
	if a.Config.SessionManager != nil && a.Config.SessionManager.Current() != nil {
		opts.SessionID = a.Config.SessionManager.Current().ID
		ctx = tools.WithSessionID(ctx, a.Config.SessionManager.Current().ID)
	}

	// Initialize rs.Messages with the initial conversation history so that
	// terminalError and other early-exit paths carry the message context.
	// Must hold rs.mu because GetLastMessages() reads via rs.mu.RLock()
	// from concurrent goroutines (channel mode).
	rs.mu.Lock()
	rs.Messages = in.Messages
	rs.mu.Unlock()

	for {
		if !rs.Budget.consume() {
			err := errors.New("iteration budget exhausted")
			ch <- a.terminalError(ctx, rs, ExitReasonBudgetExhausted, err, nil)
			return
		}

		select {
		case <-ctx.Done():
			ch <- a.terminalError(ctx, rs, ExitReasonInterrupted, ctx.Err(), rs.Messages)
			return
		default:
		}

		// ── Auto-compact check (before LLM call) ──
		// Checked at the loop top so it fires regardless of the previous
		// iteration's finish reason (tool_calls, stop, length). When a
		// compaction was attempted (success or failure), skip straight to
		// the next iteration with the new (or original) history.
		if newCtx, compacted := a.maybeAutoCompact(ctx, rs, &opts, ch); compacted {
			// Compaction swaps the current session; carry the refreshed
			// context (new session ID) forward into subsequent iterations.
			ctx = newCtx
			continue
		}

		rs.APICalls++

		// Rebuild tool schemas each iteration. When ToolSearch is active,
		// newly discovered MCP tools are added to the list, enabling the LLM
		// to call them. The tool list is monotonic (only grows), minimizing
		// prompt cache invalidations. A per-run tool view (if set) narrows
		// this to the run's allowed subset.
		llmTools := buildLLMTools(a.filterActiveSchemas(a.resolve(ctx).schemas()))

		streamCh, err := in.Provider.CreateChatStream(ctx, rs.Messages, llmTools, opts)
		if err != nil {
			ch <- a.terminalError(ctx, rs, ExitReasonError, fmt.Errorf("API call failed: %w", err), nil)
			return
		}

		// stream_start: fires when the LLM emits its first output delta
		// (thinking/text/tool-use), so integrations flip to "working" as soon
		// as generation begins — before any tool executes.
		//
		// Kept synchronous to preserve ordering (stream_start must precede
		// tool_call); hook commands default to Async: true, so the hot path
		// is not blocked unless a user explicitly configures async: false.
		acc, err := consumeStream(streamCh, ch, rs.APICalls, func() {
			a.dispatchEvent(ctx, hooks.EventStreamStart, hooks.Payload{})
		})
		if err != nil {
			exitReason := ExitReasonError
			if ctx.Err() != nil {
				exitReason = ExitReasonInterrupted
			}
			ch <- a.terminalError(ctx, rs, exitReason, err, rs.Messages)
			return
		}

		if a.handleFinishReason(ctx, acc, rs, in.Params, ch) == outcomeStop {
			return
		}
	}
}

// terminalError fires the "error" hook and returns the terminal
// AgentEventError for the loop, unifying the four error-exit sites. msgs is
// attached to the event only on resume-oriented exits (interrupted), so
// frontends can continue the partial turn; hard failures pass nil to leave
// the frontend's history untouched.
func (a *AIAgent) terminalError(ctx context.Context, rs *RunState, exitReason string, err error, msgs []llm.Message) AgentEvent {
	a.dispatchEvent(ctx, hooks.EventError, hooks.Payload{
		ErrorMessage: err.Error(),
	})
	return AgentEvent{
		Type:     AgentEventError,
		Messages: msgs,
		Result:   &RunResult{ExitReason: exitReason, IterationsUsed: rs.APICalls, Duration: rs.elapsed(), Error: err},
	}
}

// maybeAutoCompact compacts the in-flight history when the token estimate
// crosses the configured threshold. It reports whether a compaction was
// attempted (success or failure) — in both cases the loop should continue
// to the next iteration rather than proceeding to an LLM call.
//
// One-off runs (rs.SkipSessionWrites) never compact: the estimate reflects
// the main conversation, and doCompact would swap the main session out from
// under a side-channel run.
//
// On success rs.Messages is replaced with the shorter history and the
// returned context carries the NEW session's ID (compaction swaps the
// current session); on failure the original history and ctx are kept and
// the next iteration retries — eventual success if the LLM responds before
// hitting the limit.
func (a *AIAgent) maybeAutoCompact(ctx context.Context, rs *RunState, opts *llm.ChatOptions, ch chan<- AgentEvent) (context.Context, bool) {
	if rs.SkipSessionWrites {
		return ctx, false
	}
	if !a.shouldAutoCompact() {
		return ctx, false
	}

	ch <- AgentEvent{Type: AgentEventAutoCompactStart}
	summary, newHistory, err := a.doCompact(ctx, rs.Messages)
	if err != nil {
		a.Config.Logger.Error(ctx, "Auto compact failed", err)
		ch <- AgentEvent{Type: AgentEventAutoCompactDone, Result: &RunResult{Error: err}}
		return ctx, true
	}

	oldMsgCount := len(rs.Messages)
	rs.mu.Lock()
	rs.Messages = newHistory
	rs.mu.Unlock()
	a.setCompactCooldown()
	// Compact swapped the current session — refresh session-scoped values
	// captured before the loop so subsequent LLM calls and tool executions
	// are associated with the NEW session.
	if a.Config.SessionManager != nil && a.Config.SessionManager.Current() != nil {
		opts.SessionID = a.Config.SessionManager.Current().ID
		ctx = tools.WithSessionID(ctx, a.Config.SessionManager.Current().ID)
	}
	a.Config.Logger.Info(ctx, "Auto compact completed", "msgCount", len(rs.Messages))
	ch <- AgentEvent{
		Type:           AgentEventAutoCompactDone,
		CompactSummary: summary,
		OldMsgCount:    oldMsgCount,
	}
	return ctx, true
}

// loopOutcome tells runAgentLoop whether to continue iterating or stop.
// Handlers emit their own terminal events before returning outcomeStop, so
// the loop needs no error-vs-success distinction.
type loopOutcome int

const (
	outcomeContinue loopOutcome = iota // proceed to the next iteration
	outcomeStop                        // exit the loop (terminal event already emitted)
)

// handleFinishReason processes the LLM's finish reason and updates loop state
// accordingly.
func (a *AIAgent) handleFinishReason(
	ctx context.Context,
	acc *streamAccumulator,
	rs *RunState,
	params *runParams,
	ch chan<- AgentEvent,
) loopOutcome {
	switch acc.finishReason {
	case finishReasonToolCalls, finishReasonToolUse:
		return a.handleToolCallFinish(ctx, acc, rs, params, ch)
	case finishReasonMaxTokens, finishReasonLength:
		// Truncated: tool_use deltas may be incomplete (arguments cut off),
		// so never execute them — handleLengthFinish drops them and asks
		// the model to retry.
		return a.handleLengthFinish(ctx, acc, rs, ch)
	default:
		if len(acc.toolCalls) > 0 {
			// Some providers/models report "stop" (or leave it blank) on a
			// message that still carries tool_use deltas — e.g. OpenAI
			// Responses derives the finish reason from the LAST output item
			// only, so a reasoning model appending text after a function
			// call yields "stop" with pending tool calls. Dropping them
			// would end the turn without executing the tools (the pane
			// flips to idle while the user expects work to continue) —
			// execute them instead.
			return a.handleToolCallFinish(ctx, acc, rs, params, ch)
		}
		return a.handleStopFinish(ctx, acc, rs, ch)
	}
}

// handleToolCallFinish orchestrates a tool-call response through its phases:
// record the turn, execute tools, sync LSP, apply steer input, inject loop
// reminders. Each phase is a named method below.
func (a *AIAgent) handleToolCallFinish(
	ctx context.Context,
	acc *streamAccumulator,
	rs *RunState,
	params *runParams,
	ch chan<- AgentEvent,
) loopOutcome {
	a.recordToolCallTurn(acc, rs, ch)

	if outcome := a.executeAndAppendTools(ctx, acc, rs, ch); outcome == outcomeStop {
		return outcomeStop
	}

	a.syncLSPAfterTools(ctx, acc)

	if outcome := a.applySteer(ctx, rs, params, ch); outcome == outcomeStop {
		return outcomeStop
	}

	a.injectLoopReminders(ctx, acc, rs)
	return outcomeContinue
}

// recordToolCallTurn persists the assistant turn (text, usage, thinking) and
// appends the assistant message carrying the tool calls to the loop state.
// Also emits the incremental usage event so the TUI can update totalUsage
// and the status bar in real time.
func (a *AIAgent) recordToolCallTurn(acc *streamAccumulator, rs *RunState, ch chan<- AgentEvent) {
	a.recordAssistantTurn(rs, acc.text.String(), acc.usage, acc.thinkBlocks)

	rs.append(acc.assistantMessage())

	if acc.usage != nil {
		ch <- AgentEvent{Type: AgentEventUsage, Usage: acc.usage}
	}
}

// executeAndAppendTools runs the requested tool calls and appends their
// result messages. Returns outcomeStop when execution is cancelled — the
// error event is emitted before returning.
func (a *AIAgent) executeAndAppendTools(
	ctx context.Context,
	acc *streamAccumulator,
	rs *RunState,
	ch chan<- AgentEvent,
) loopOutcome {
	toolMsgs, err := a.executeToolCalls(ctx, rs, acc.toolCalls, ch)
	if err != nil {
		ch <- AgentEvent{
			Type:     AgentEventError,
			Messages: rs.Messages,
			Result:   &RunResult{ExitReason: ExitReasonCancelled, Duration: rs.elapsed(), Error: err},
		}
		return outcomeStop
	}
	a.Config.Logger.Info(ctx, "Agent: executeToolCalls returned", "toolMsgCount", len(toolMsgs), "toolCallCount", len(acc.toolCalls))
	rs.append(toolMsgs...)
	rs.LengthRetries = 0
	return outcomeContinue
}

// syncLSPAfterTools syncs files touched by tool calls to LSP servers and
// closes files that no longer exist on disk (deleted/renamed/moved by Bash),
// keeping the LSP file index consistent with the actual filesystem.
func (a *AIAgent) syncLSPAfterTools(ctx context.Context, acc *streamAccumulator) {
	if a.Config.LSPManager == nil {
		return
	}
	for _, tc := range acc.toolCalls {
		if fp := tools.ExtractFilePath(tc.Function.Name, tc.Function.Arguments); fp != "" {
			if syncErr := a.Config.LSPManager.SyncFile(ctx, fp); syncErr != nil {
				a.Config.Logger.Error(ctx, "LSP: file sync error", syncErr, "file", fp)
			}
		}
	}
	if hasBashCall(acc.toolCalls) {
		a.Config.LSPManager.CloseMissingFiles(ctx)
	}
	// Wait briefly for async diagnostics to arrive after file sync.
	a.Config.LSPManager.WaitForDiagnostics(ctx, 2*time.Second)
}

// defaultSteerTimeout bounds how long applySteer waits for the frontend to
// answer a SteerCheck. A healthy frontend replies within milliseconds; the
// timeout guards against a desynced or stuck frontend (e.g. a channel
// mismatch after a concurrent turn rebuilt the steer channel) permanently
// blocking the loop. Steer is an optional injection — continuing without it
// is always safe.
const defaultSteerTimeout = 5 * time.Second

// applySteer injects pending user input after tool results. Returns
// outcomeStop when the context is cancelled while waiting for the TUI's
// steer response. If no steer input arrives within the steer timeout
// (runParams.steerTimeout or defaultSteerTimeout), the loop continues as if
// nothing was steered and logs a warning so the desync is visible instead of
// hanging forever.
func (a *AIAgent) applySteer(ctx context.Context, rs *RunState, params *runParams, ch chan<- AgentEvent) loopOutcome {
	if params == nil || params.steerCh == nil {
		return outcomeContinue
	}
	timeout := params.steerTimeout
	if timeout == 0 {
		timeout = defaultSteerTimeout
	}
	ch <- AgentEvent{Type: AgentEventSteerCheck}
	select {
	case steerInput := <-params.steerCh:
		// Empty text AND no images = "nothing to steer" signal.
		if steerInput.Text == "" && len(steerInput.Images) == 0 {
			return outcomeContinue
		}
		msg := llm.Message{Role: llm.RoleSteer, Content: steerInput.Text}
		if len(steerInput.Images) > 0 {
			msg.ContentParts = make([]llm.ContentPart, 0, 1+len(steerInput.Images))
			msg.ContentParts = append(msg.ContentParts, llm.ContentPart{Type: llm.ContentPartText, Text: steerInput.Text})
			msg.ContentParts = append(msg.ContentParts, steerInput.Images...)
		}
		rs.append(msg)
		a.Config.Logger.Info(ctx, "Agent: steer: injected RoleSteer msg", "steerText", strutil.Truncate(steerInput.Text, 80))
		if steerInput.Text != "" {
			a.recordSession(rs, &session.Message{
				Type:    session.MessageTypeUser,
				Content: steerInput.Text,
			})
		}
		return outcomeContinue
	case <-time.After(timeout):
		// No steer input in time — frontend may be desynced or stuck. Never
		// block the loop forever; log so the mismatch is diagnosable.
		a.Config.Logger.Warn(ctx, "Agent: steer: timed out waiting for steer response, continuing without steer", "timeout", timeout.String())
		return outcomeContinue
	case <-ctx.Done():
		ch <- AgentEvent{
			Type:     AgentEventError,
			Messages: rs.Messages,
			Result:   &RunResult{ExitReason: ExitReasonInterrupted, Duration: rs.elapsed(), Error: ctx.Err()},
		}
		return outcomeStop
	}
}

// injectLoopReminders refreshes the token estimate and appends any active
// system reminders as a user message, so the next LLM call sees them.
func (a *AIAgent) injectLoopReminders(ctx context.Context, acc *streamAccumulator, rs *RunState) {
	a.EstimateAndUpdateTokens(rs, rs.Messages)
	rctx := a.buildReminderContext(false, true)
	// Populate tool names so reminders (e.g. LSPDiagnostics) can filter by tool.
	rctx.ToolNames = make([]string, 0, len(acc.toolCalls))
	for _, tc := range acc.toolCalls {
		rctx.ToolNames = append(rctx.ToolNames, tc.Function.Name)
	}
	if block := a.collectReminders(ctx, rctx); block != "" {
		rs.append(llm.Message{Role: "user", Content: block})
		a.Config.Logger.Info(ctx, "Agent: loop reminder injected", "block", strutil.Truncate(block, 200))
		a.recordSession(rs, &session.Message{
			Type:    session.MessageTypeReminder,
			Content: block,
		})
	}
}

const maxLengthContinueRetries = 3

// handleLengthFinish processes a truncated (length/max_tokens) response:
// it records the partial output, then either stops (retries exhausted) or
// appends a continuation prompt for the next iteration.
func (a *AIAgent) handleLengthFinish(
	ctx context.Context,
	acc *streamAccumulator,
	rs *RunState,
	ch chan<- AgentEvent,
) loopOutcome {
	rs.LengthRetries++
	a.Config.Logger.Info(ctx, "Agent: continuation", "text", acc.text.String(), "finishReason", acc.finishReason, "retry", rs.LengthRetries, "maxRetries", maxLengthContinueRetries)

	a.recordAssistantTurn(rs, acc.text.String(), acc.usage, acc.thinkBlocks)

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
	rs.append(msg)

	if rs.LengthRetries >= maxLengthContinueRetries {
		return a.lengthExhausted(ctx, acc, rs, ch)
	}
	a.appendLengthContinuation(ctx, acc, rs)
	return outcomeContinue
}

// lengthExhausted stops the loop after maxLengthContinueRetries truncated
// responses, delivering the partial output as a normal turn completion.
func (a *AIAgent) lengthExhausted(
	ctx context.Context,
	acc *streamAccumulator,
	rs *RunState,
	ch chan<- AgentEvent,
) loopOutcome {
	a.Config.Logger.Info(ctx, "Agent: length continuation exhausted", "maxRetries", maxLengthContinueRetries)

	// Return the partial output as a normal turn completion instead
	// of an error — the user already saw the text streaming, and
	// discarding it (or showing a red error) is worse than delivering
	// what we have with a note that it was truncated.
	ch <- AgentEvent{
		Type:     AgentEventTurnComplete,
		Messages: rs.Messages,
		Usage:    acc.usage,
		Result: &RunResult{
			Response:       acc.text.String(),
			IterationsUsed: rs.APICalls,
			Duration:       rs.elapsed(),
			ExitReason:     ExitReasonLengthExhausted,
			Error:          fmt.Errorf("response truncated after %d continuation attempts", maxLengthContinueRetries),
			Usage:          acc.usage,
			TraceID:        rs.trace(),
		},
	}

	// Fire turn_complete hook with error info so external integrations
	// know the turn ended (even if truncated). Without this, the hook
	// system may be stuck in "working" state.
	a.dispatchEvent(ctx, hooks.EventTurnComplete, hooks.Payload{
		TurnCount:    rs.APICalls,
		ErrorMessage: fmt.Sprintf("response truncated after %d continuation attempts", maxLengthContinueRetries),
	})

	return outcomeStop
}

// appendLengthContinuation records a context-aware continuation prompt and
// appends it (reminder-wrapped) so the next iteration resumes the response.
func (a *AIAgent) appendLengthContinuation(ctx context.Context, acc *streamAccumulator, rs *RunState) {
	continuationText := continuationPrompt(acc)

	// Wrap the continuation message with reminders
	rctx := a.buildReminderContext(false, false)
	rctx.CurrentPrompt = continuationText
	reminderBlock := a.collectReminders(ctx, rctx)
	// Record reminder before user message so session file order matches LLM input
	if reminderBlock != "" {
		a.recordSession(rs, &session.Message{
			Type:    session.MessageTypeReminder,
			Content: reminderBlock,
		})
	}
	a.recordSession(rs, &session.Message{
		Type:    session.MessageTypeUser,
		Content: continuationText,
	})

	wrappedContinuation := continuationText
	if reminderBlock != "" {
		wrappedContinuation = reminderBlock + continuationText
	}

	rs.append(llm.Message{Role: "user", Content: wrappedContinuation})

	// Fire turn_truncated hook to indicate the turn is continuing
	a.dispatchEvent(ctx, hooks.EventTurnTruncated, hooks.Payload{
		TurnCount:   rs.LengthRetries,
		UserMessage: continuationText,
	})
}

// continuationPrompt picks the continuation instruction matching what was
// truncated, so the model knows how to recover. Pure decision, no effects.
func continuationPrompt(acc *streamAccumulator) string {
	switch {
	case len(acc.toolCalls) > 0:
		return "Your previous tool call was interrupted by the output token limit. Please retry the tool call."
	case len(acc.thinkBlocks) > 0 && acc.text.Len() == 0:
		return "Please continue with your response. Break your output into smaller chunks to avoid hitting the output token limit."
	default:
		return "Please continue where you left off. Break your output into smaller chunks to avoid hitting the output token limit."
	}
}

// handleStopFinish processes a normal stop response: records the assistant
// turn, emits TurnComplete, and stores turn-level memory.
func (a *AIAgent) handleStopFinish(
	ctx context.Context,
	acc *streamAccumulator,
	rs *RunState,
	ch chan<- AgentEvent,
) loopOutcome {
	rs.LengthRetries = 0
	msg := acc.assistantMessage()
	// handleFinishReason routes any stop response carrying tool calls to
	// handleToolCallFinish, so a stop reaching here carries none; the nil
	// assignment stays as a defensive drop so the recorded history never
	// holds unpaired tool_use blocks.
	msg.ToolCalls = nil
	rs.append(msg)

	a.recordAssistantTurn(rs, acc.text.String(), acc.usage, acc.thinkBlocks)

	ch <- AgentEvent{
		Type: AgentEventTurnComplete, Messages: rs.Messages, Usage: acc.usage,
		Result: &RunResult{Response: acc.text.String(), IterationsUsed: rs.APICalls, Duration: rs.elapsed(), ExitReason: ExitReasonStop, Usage: acc.usage, TraceID: rs.trace()},
	}

	// Fire turn_complete hook
	a.dispatchEvent(ctx, hooks.EventTurnComplete, hooks.Payload{
		TurnCount: rs.APICalls,
	})

	return outcomeStop
}

// filterActiveSchemas filters tool schemas for the LLM API call.
// Two layers of filtering are applied:
//
//  1. Mode-based: in chat/plan mode, destructive tools are excluded so the
//     LLM cannot see or call them. This replaces the old savedTools approach
//     that mutated the registry at mode-change time — instead the registry
//     is never touched, and the filter runs every iteration.
//
//  2. MCP ToolSearch: when the deferred MCP pool is non-empty, only
//     discovered MCP tools (plus all built-ins) are included.
//
// When ToolSearch is not active (no MCP manager, e.g. no MCP servers):
//   - Only mode filtering applies
func (a *AIAgent) filterActiveSchemas(schemas []tools.Schema) []tools.Schema {
	// Layer 1: mode-based destructive tool filtering
	if a.Mode() != ModeAuto {
		schemas = filterDestructiveSchemas(schemas, a.Config.ToolRegistry)
		if len(schemas) == 0 {
			return nil
		}
	}

	// Layer 2: MCP ToolSearch filtering (existing logic)
	pool := a.DeferredPool()
	set := a.discoveredSet()
	if pool == nil || pool.Len() == 0 {
		return schemas
	}

	active := make([]tools.Schema, 0, len(schemas))
	seen := make(map[string]bool)

	for _, s := range schemas {
		name := s.Name
		switch {
		case !tools.IsMCPSchema(name):
			active = append(active, s)
			seen[name] = true
		case tools.IsMCPSearchTool(name):
			active = append(active, s)
			seen[name] = true
		case set != nil && set.Contains(name):
			active = append(active, s)
			seen[name] = true
		default:
		}
	}

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

// filterDestructiveSchemas removes schemas for tools that implement
// DestructiveDetector and return true. Non-destructive tools and tools
// that don't implement DestructiveDetector are kept.
func filterDestructiveSchemas(schemas []tools.Schema, reg *tools.Registry) []tools.Schema {
	filtered := make([]tools.Schema, 0, len(schemas))
	for _, s := range schemas {
		tool := reg.GetTool(s.Name)
		if tool == nil {
			// Unknown tools (e.g. MCP tools not yet registered) are kept
			// so the user can still discover them via MCPSearchTools.
			filtered = append(filtered, s)
			continue
		}
		if dd, ok := tool.(tools.DestructiveDetector); ok && dd.IsDestructive() {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
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
	return systemreminder.Context{
		IsFirstMessage:  isFirstMessage,
		Now:             time.Now(),
		LastMessageDate: a.conv.messageDate(),
		IsToolResult:    isToolResult,
		SkipRecall:      a.Config.Memory != nil && a.Config.Memory.SkipRecall,
		SessionID:       a.sessionID(),
		Logger:          a.Config.Logger,
	}
}

// ParentSessionID returns the current session's ID, or empty string if
// no session is active. This satisfies the subagent.Agent interface.
func (a *AIAgent) ParentSessionID() string {
	return a.sessionID()
}

// collectReminders calls Collect on the reminder collector. Returns empty
// string when the collector is nil (safer than panicking on nil interface).
func (a *AIAgent) collectReminders(ctx context.Context, rctx systemreminder.Context) string {
	if a.Config.ReminderCollector == nil {
		return ""
	}
	return a.Config.ReminderCollector.Collect(ctx, rctx)
}

// sessionID returns the current session's ID, or empty string if no session.
func (a *AIAgent) sessionID() string {
	if a.Config.SessionManager != nil && a.Config.SessionManager.Current() != nil {
		return a.Config.SessionManager.Current().ID
	}
	return ""
}
