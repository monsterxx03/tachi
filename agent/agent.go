package agent

import (
	"context"
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
	lastMessageDate    string // calendar date (2006-01-02) of last processed user message; empty initially
	titleModelProvider llm.Provider // optional: dedicated provider for title generation
	titleGenEnabled    bool         // whether LLM-based title generation is active
	commitProvider     llm.Provider // optional: dedicated provider for /commit messages
	logger             *debuglog.Logger

	// Subagent-related fields
	subagentProvider       llm.Provider // sub-agent dedicated provider (nil = fallback to main)
	subagentModel          string       // sub-agent dedicated model ("" = fallback to main)
	subagentMaxIterations  int          // sub-agent default iteration limit
	subagentMaxConcurrency int          // sub-agent max concurrent instances
	subagentMaxOutputChars int          // sub-agent output truncation threshold
	subagentThinking       bool         // whether sub-agents enable thinking

	// Worktree-related fields
	subagentWorktree        bool   // enable git worktree isolation
	subagentWorktreeDir     string // worktree storage directory
	subagentWorktreeCleanup bool   // clean up after completion
	subagentWorktreeBranch  string // default branch for worktree checkout
}

func NewAIAgent(provider llm.Provider, model string, maxIterations int) *AIAgent {
	return &AIAgent{
		model:           model,
		provider:        provider,
		maxIterations:   maxIterations,
		titleGenEnabled: true,
		toolRegistry:    tools.NewRegistry(),
		confirmRespCh:   make(chan bool, 1),
		askUserRespCh:   make(chan tools.AskUserResult, 1),
		logger:          debuglog.DefaultLogger,
		reminderCollector: systemreminder.NewCollector(
			systemreminder.DateReminder{},
			systemreminder.ProjectContextReminder{},
			systemreminder.GitReminder{},
			systemreminder.IterationWarningReminder{Threshold: 5},
			systemreminder.TokenWarningReminder{ThresholdPct: 80},
		),
	}
}

// SetLogger overrides the agent's logger. Channel callers use this to inject
// a channel-specific logger so debug output is tagged with the correct source.
func (a *AIAgent) SetLogger(l *debuglog.Logger) {
	a.logger = l
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
	a.titleGenEnabled = cfg.TitleGeneration == nil || *cfg.TitleGeneration

	tpName := cfg.TitleProvider
	if tpName == "" {
		return
	}

	tpCfg := cfg.FindProvider(tpName)
	if tpCfg == nil {
		a.logger.Log("Agent: title_provider %q not found in providers list, falling back to main model", tpName)
		return
	}

	resolved, err := config.ResolveProviderConfig(tpCfg)
	if err != nil {
		a.logger.Log("Agent: failed to resolve title provider %q: %v, falling back to main model", tpName, err)
		return
	}

	tp, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		a.logger.Log("Agent: failed to create title provider %q: %v, falling back to main model", tpName, err)
		return
	}

	a.titleModelProvider = tp
	a.logger.Log("Agent: using title provider %q (%s/%s) for session title generation", tpName, resolved.Type, resolved.Model)
}

// SetupCommitProvider resolves and creates a dedicated LLM provider for /commit
// message generation from config. When commit_provider is empty or not found,
// commit generation falls back to the main conversation provider.
func (a *AIAgent) SetupCommitProvider(cfg *config.Config) {
	cpName := cfg.CommitProvider
	if cpName == "" {
		return
	}

	cpCfg := cfg.FindProvider(cpName)
	if cpCfg == nil {
		a.logger.Log("Agent: commit_provider %q not found in providers list, falling back to main model", cpName)
		return
	}

	resolved, err := config.ResolveProviderConfig(cpCfg)
	if err != nil {
		a.logger.Log("Agent: failed to resolve commit provider %q: %v, falling back to main model", cpName, err)
		return
	}

	cp, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		a.logger.Log("Agent: failed to create commit provider %q: %v, falling back to main model", cpName, err)
		return
	}

	a.commitProvider = cp
	a.logger.Log("Agent: using commit provider %q (%s/%s) for /commit message generation", cpName, resolved.Type, resolved.Model)
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
		a.lastMessageDate = rctx.Now.Format("2006-01-02")
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

	thinkingDisabled := false
	chatOpts := llm.ChatOptions{MaxTokens: 500, Thinking: &thinkingDisabled}
	if a.sessionManager != nil && a.sessionManager.Current() != nil {
		chatOpts.SessionID = a.sessionManager.Current().ID
	}
	resp, err := p.CreateChat(ctx, messages, nil, chatOpts)
	if err != nil {
		a.logger.Log("Agent: failed to generate title: %v, falling back to truncation", err)
		return session.ExtractTitle(firstMessage)
	}

	title := strings.TrimSpace(resp.Content)
	if title == "" {
		a.logger.Log("Agent: LLM returned empty title, falling back to truncation")
		return session.ExtractTitle(firstMessage)
	}

	a.logger.Log("Agent: LLM generated title: %s", title)

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
		a.logger.Log("Agent: failed to record session message: %v", err)
	}
}

func (a *AIAgent) RegisterTools() {
	a.toolRegistry.Register(tools.NewReadTool())
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

// ToolNames returns registered tool names without triggering Description() calls.
func (a *AIAgent) ToolNames() []string {
	return a.toolRegistry.GetToolNames()
}

// SaveToolRegistry returns a snapshot of all currently registered tools.
// Use RestoreToolRegistry to restore them later.
func (a *AIAgent) SaveToolRegistry() map[string]tools.Tool {
	saved := make(map[string]tools.Tool)
	for _, name := range a.toolRegistry.GetToolNames() {
		if tool := a.toolRegistry.GetTool(name); tool != nil {
			saved[name] = tool
		}
	}
	return saved
}

// RestoreToolRegistry clears the current tool registry and re-registers
// the tools from the given snapshot (typically obtained from SaveToolRegistry).
func (a *AIAgent) RestoreToolRegistry(saved map[string]tools.Tool) {
	// Remove all currently registered tools
	for _, name := range a.toolRegistry.GetToolNames() {
		a.toolRegistry.Unregister(name)
	}
	// Re-register from saved snapshot
	for _, tool := range saved {
		a.toolRegistry.Register(tool)
	}
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
	AgentEventSessionTitle     = "session_title"
	AgentEventSubagentStart    = "subagent_start"
	AgentEventSubagentDone     = "subagent_done"
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
	Title         string // For AgentEventSessionTitle
}

var (
	errCancelled                  = fmt.Errorf("edit cancelled by user")
	errParallelConfirmUnsupported = fmt.Errorf("tool requiring confirmation cannot run in parallel group")
	errParallelAskUserUnsupported = fmt.Errorf("tool requiring user input cannot run in parallel group")
)

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
		a.logger.Log("Agent: finish_reason=%s, continuation retry %d/%d", acc.finishReason, *lengthRetries, maxLengthContinueRetries)
		if *lengthRetries >= maxLengthContinueRetries {
			a.logger.Log("Agent: length continuation exhausted after %d retries", maxLengthContinueRetries)
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
		continuationText := "Please continue where you left off. Break your output into smaller chunks to avoid hitting the output token limit."
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

		// When resuming a session, date/project-context/git reminders were
		// not stored — they're ephemeral by design. Re-inject them into the
		// wrapped user message (not as a separate message, which would
		// violate the user/assistant alternation requirement of LLM APIs).
		// historyHasReminder prevents duplication on subsequent turns.
		reminderIsFirst := isFirstMessage || (len(history) > 0 && !historyHasReminder(history))

		rctx := a.buildReminderContext(reminderIsFirst)
		wrappedUser := a.reminderCollector.WrapUserMessage(userMessage, rctx)
		a.lastMessageDate = rctx.Now.Format("2006-01-02")
		messages = append(messages, llm.Message{Role: "user", Content: wrappedUser})

		// Session management: create session if needed and append user message
		if a.sessionManager != nil && !a.sessionManager.HasCurrent() {
			provider := a.provider.Name()
			wd, _ := os.Getwd()
			if _, err := a.sessionManager.New(provider, a.model, wd); err != nil {
				a.logger.Log("Agent: failed to create session: %v", err)
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
	llmTools := buildLLMTools(a.toolRegistry.GetSchemas())

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

		// After tool results, inject system-reminder warnings.
		if a.shouldInjectLoopReminder() {
			rctx := a.buildReminderContext(false)
			if block := a.reminderCollector.Collect(rctx); block != "" {
				messages = append(messages, llm.Message{Role: "user", Content: block})
			}
		}
	}
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
func (a *AIAgent) buildReminderContext(isFirstMessage bool) systemreminder.Context {
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
		systemreminder.IterationWarningReminder{Threshold: cfg.SystemReminder.IterationWarningThreshold},
		systemreminder.TokenWarningReminder{ThresholdPct: cfg.SystemReminder.TokenWarningThresholdPct},
	}
	if cfg.SystemReminder.GitReminder == nil || *cfg.SystemReminder.GitReminder {
		reminders = append(reminders, systemreminder.GitReminder{})
	}
	a.reminderCollector = systemreminder.NewCollector(reminders...)
	a.reminderCollector.SetLogger(a.logger)

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
	var mgr *mcp.Manager
	if cfg.MCPEnabled() {
		mgr = mcp.NewManager()
		mgr.SetLogger(a.logger)
		mcpTools, errs := mgr.ConnectAll(ctx, cfg.MCPServers)
		for _, err := range errs {
			a.logger.Log("MCP: load error: %v", err)
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		}
		for _, t := range mcpTools {
			a.RegisterTool(t)
			a.logger.Log("MCP: registered tool %s", t.Name())
		}
	}

	// --- SubAgent tool ---
	a.SetupSubagentProvider(cfg)
	a.subagentWorktree = cfg.Subagent.Worktree
	a.subagentWorktreeDir = cfg.Subagent.WorktreeDir
	a.subagentWorktreeBranch = cfg.Subagent.WorktreeBranch
	a.subagentWorktreeCleanup = true
	if cfg.Subagent.WorktreeCleanup != nil {
		a.subagentWorktreeCleanup = *cfg.Subagent.WorktreeCleanup
	}
	executor := NewSubagentExecutor(a)
	if a.SubagentWorktree() {
		executor.SetWorktreeManager(NewWorktreeManager(cfg, a.logger))
	}
	a.RegisterTool(tools.NewSubagentTool(executor))

	return mgr, nil
}

// ResumeSession loads the most recent session from disk, converts it to LLM
// message format, prepends the given system prompt (if non-empty), and attaches
// the session manager to the agent for ongoing session recording.
// Returns the loaded session metadata alongside the messages so callers can
// rebuild the provider to match the session's original provider/model.
func (a *AIAgent) ResumeSession(providerType, systemPrompt string) ([]llm.Message, []session.Message, *session.Session, error) {
	sm, err := session.NewManager()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("session manager: %w", err)
	}

	sessions, err := sm.List()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list sessions: %w", err)
	}
	if len(sessions) == 0 {
		return nil, nil, nil, fmt.Errorf("no sessions to resume")
	}

	latest := sessions[0]
	if _, err := sm.Load(latest.ID); err != nil {
		return nil, nil, nil, fmt.Errorf("load session %s: %w", latest.ID, err)
	}

	// Restore working directory if recorded
	if latest.WorkingDir != "" {
		if err := os.Chdir(latest.WorkingDir); err != nil {
			a.logger.Log("Agent: failed to chdir to %s: %v", latest.WorkingDir, err)
		}
	}

	sessionMsgs, err := sm.LoadMessages()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load messages: %w", err)
	}

	llmMsgs, err := ConvertSessionToLLMMessages(sessionMsgs, providerType)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("convert session messages: %w", err)
	}

	if systemPrompt != "" {
		llmMsgs = append([]llm.Message{{Role: "system", Content: systemPrompt}}, llmMsgs...)
	}

	a.sessionManager = sm
	return llmMsgs, sessionMsgs, latest, nil
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

// --- Worktree accessors ---

// SubagentWorktree returns whether git worktree isolation is enabled.
func (a *AIAgent) SubagentWorktree() bool { return a.subagentWorktree }

// SubagentWorktreeDir returns the worktree storage directory.
func (a *AIAgent) SubagentWorktreeDir() string { return a.subagentWorktreeDir }

// SubagentWorktreeCleanup returns whether worktrees are cleaned up after completion.
func (a *AIAgent) SubagentWorktreeCleanup() bool { return a.subagentWorktreeCleanup }

// SubagentWorktreeBranch returns the default branch for worktree checkout.
func (a *AIAgent) SubagentWorktreeBranch() string { return a.subagentWorktreeBranch }
