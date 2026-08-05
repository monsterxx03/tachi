package subagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/pkg/syncx"
	"github.com/monsterxx03/tachi/session"
)

// Executor implements tools.SubagentRunner by creating and running child
// agents. It also manages the concurrency semaphore for parallel execution.
type Executor struct {
	agent       Agent
	cfg         config.SubagentConfig
	sem         *syncx.Semaphore // concurrency semaphore
	worktreeMgr *WorktreeManager
}

// NewExecutor creates a new Executor bound to the given parent agent.
func NewExecutor(a Agent, cfg config.SubagentConfig) *Executor {
	maxConc := cfg.MaxConcurrency
	if maxConc <= 0 {
		maxConc = DefaultMaxConcurrency
	}
	return &Executor{
		agent: a,
		cfg:   cfg,
		sem:   syncx.NewSemaphore(maxConc),
	}
}

// EnableWorktree creates and attaches a WorktreeManager from config.
func (e *Executor) EnableWorktree(logger *logger.Logger) {
	e.worktreeMgr = NewWorktreeManager(e.cfg, logger)
}

// AvailableToolNames returns all tool names the sub-agent can use.
// Excludes AskUserQuestion and SubAgent (which sub-agents cannot use).
func (e *Executor) AvailableToolNames() []string {
	allNames := e.agent.ToolNames()
	names := make([]string, 0, len(allNames))
	for _, name := range allNames {
		if name == tools.ToolNameAskUser || name == tools.ToolNameSubAgent {
			continue
		}
		names = append(names, name)
	}
	return names
}

// MaxOutputChars returns the configured output truncation threshold.
func (e *Executor) MaxOutputChars() int {
	if e.cfg.MaxOutputChars > 0 {
		return e.cfg.MaxOutputChars
	}
	return DefaultMaxOutputChars
}

// RunSubagent creates and runs a child agent to execute the given task.
// It blocks until the child completes or the context is cancelled.
// Returns the SubagentResult which includes output, statistics, and tool call summary.
func (e *Executor) RunSubagent(
	ctx context.Context,
	args tools.SubagentArgs,
) (string, string, *tools.SubagentResult, error) {
	// Acquire concurrency semaphore
	if err := e.sem.Acquire(ctx); err != nil {
		return "", "", nil, err
	}
	defer e.sem.Release()

	provider := e.agent.SubagentProvider()

	maxIterations := e.cfg.MaxIterations
	if maxIterations <= 0 {
		maxIterations = DefaultMaxIterations
	}

	shortID := strutil.ShortUUID(8)
	thinking := e.cfg.Thinking
	branch := args.WorktreeBranch

	// If worktree is enabled, delegate to WorktreeManager
	if e.worktreeMgr != nil {
		result, stats, err := e.worktreeMgr.Create(ctx, branch, func(worktreeCtx context.Context, wtPath string) (string, *tools.SubagentResult, error) {
			e.agent.Logger().Info(worktreeCtx, "[subagent] worktree created", "id", shortID, "path", wtPath, "branch", fallbackIfEmpty(branch, "detached"))
			return e.run(worktreeCtx, shortID, args, provider, maxIterations, thinking, branch, wtPath)
		})
		return result, shortID, stats, err
	}

	result, stats, err := e.run(ctx, shortID, args, provider, maxIterations, thinking, "", "")
	return result, shortID, stats, err
}

// run is the internal method that creates and runs the child agent.
func (e *Executor) run(
	ctx context.Context,
	shortID string,
	args tools.SubagentArgs,
	provider llm.Provider,
	maxIterations int,
	thinking bool,
	branch string,
	worktreePath string,
) (string, *tools.SubagentResult, error) {
	// Usage billing: subagent calls flow through child.RunOneOffStream which
	// has no one-off meta — tag the kind here so ledger rows are grouped
	// under subagent (and the composite session ID is normalized to the
	// parent session by RecordingProvider).
	ctx = llm.WithUsageKind(ctx, llm.UsageKindSubagent)
	subagentSessionID := shortID
	if parentID := e.agent.ParentSessionID(); parentID != "" {
		subagentSessionID = parentID + ":" + shortID
	}

	childLogger := e.agent.Logger().With("session_id", subagentSessionID).With("prefix", fmt.Sprintf("[subagent:%s]", shortID))

	// Build allowed tools list — exclude AskUserQuestion and SubAgent
	allowedTools := buildAllowedTools(e, args.AllowedTools)

	child := e.agent.NewChildAgent(childLogger, provider, maxIterations, allowedTools, subagentSessionID)
	childLogger.Info(ctx, "starting", "promptLen", len(args.Prompt), "tools", len(allowedTools), "maxIters", maxIterations, "thinking", thinking, "worktree", worktreePath != "", "sessionID", subagentSessionID)

	// Build system prompt
	systemPrompt := SystemPrompt
	if worktreePath != "" {
		source := branch
		if source == "" {
			source = "HEAD"
		}
		systemPrompt += fmt.Sprintf(WorktreePromptFmt, source)
	}

	// Set up recorder for persisting execution details
	var rec *recorder
	if parentID := e.agent.ParentSessionID(); parentID != "" {
		var recErr error
		rec, recErr = newRecorder(parentID, shortID, childLogger)
		if recErr != nil {
			childLogger.Error(ctx, "failed to create recorder", recErr)
		} else {
			defer func() {
				if err := rec.close(); err != nil {
					childLogger.Error(ctx, "subagent: failed to close recorder", err)
				}
			}()
			if err := rec.record(&session.Message{
				Type:    session.MessageTypeUser,
				Content: args.Prompt,
			}); err != nil {
				childLogger.Error(ctx, "subagent: failed to record user prompt", err)
			}
		}
	}

	ch := child.Run(ctx, provider, systemPrompt, args.Prompt, llm.ChatOptions{
		MaxTokens: 4096,
		Thinking:  &thinking,
		SessionID: subagentSessionID,
	})

	// Consume events, collect result + stats
	var sb strings.Builder
	var thinkingBuf strings.Builder
	var finalResult string // accumulated text result returned at end
	startTime := time.Now()
	iterCount := 0
	toolCalls := make(tools.ToolCallCount)

	// Extract event sink for forwarding internal tool calls to parent TUI.
	eventSink := tools.GetSubagentEventSink(ctx)

	flushThinking := func() {
		if rec == nil || thinkingBuf.Len() == 0 {
			return
		}
		if err := rec.record(&session.Message{
			Type:    session.MessageTypeThinking,
			Content: thinkingBuf.String(),
		}); err != nil {
			childLogger.Error(ctx, "subagent: failed to record thinking", err)
		}
		thinkingBuf.Reset()
	}

	flushText := func() {
		if rec == nil || sb.Len() == 0 {
			return
		}
		if err := rec.record(&session.Message{
			Type:    session.MessageTypeAssistant,
			Content: sb.String(),
		}); err != nil {
			childLogger.Error(ctx, "subagent: failed to record assistant text", err)
		}
	}

	for event := range ch {
		switch event.Type {
		case StreamEventThinkingDelta:
			thinkingBuf.WriteString(event.ThinkingDelta)

		case StreamEventTextDelta:
			flushThinking()
			sb.WriteString(event.TextDelta)

		case StreamEventToolCallArgs:
			flushThinking()
			flushText()
			sb.Reset()
			if rec != nil {
				if err := rec.record(&session.Message{
					Type:       session.MessageTypeToolCall,
					Name:       event.ToolName,
					Args:       event.ToolArgs,
					ToolCallID: event.ToolID,
				}); err != nil {
					childLogger.Error(ctx, "subagent: failed to record tool call", err)
				}
			}

		case StreamEventToolResult:
			// Forward to parent event sink for real-time TUI display.
			if eventSink != nil {
				eventSink.SendToolResultEvent(event.ToolName, event.ToolResult, event.ToolIsError)
			}
			if rec != nil {
				if err := rec.record(&session.Message{
					Type:       session.MessageTypeToolResult,
					Name:       event.ToolName,
					Result:     event.ToolResult,
					IsError:    event.ToolIsError,
					ToolCallID: event.ToolID,
				}); err != nil {
					childLogger.Error(ctx, "subagent: failed to record tool result", err)
				}
			}
			toolCalls.Add(event.ToolName)
			iterCount++

		case StreamEventTurnComplete:
			flushThinking()
			if rec != nil && (sb.Len() > 0 || event.Usage != nil) {
				if err := rec.record(&session.Message{
					Type:    session.MessageTypeAssistant,
					Content: sb.String(),
					Usage:   usageToSession(event.Usage),
				}); err != nil {
					childLogger.Error(ctx, "subagent: failed to record final assistant text", err)
				}
			}
			finalResult = sb.String()
			sb.Reset()

		case StreamEventError:
			flushThinking()
			flushText()

			duration := time.Since(startTime)
			var errVal error
			if event.Error != nil {
				errVal = event.Error
			}
			childLogger.Info(ctx, "completed with error", "iters", iterCount, "duration", duration, "outputLen", len(finalResult), "toolCalls", toolCalls.String(), "err", errVal)
			stats := &tools.SubagentResult{
				Output:          finalResult,
				ShortID:         shortID,
				IterCount:       iterCount,
				Duration:        duration,
				ToolCallSummary: toolCalls,
			}
			if errVal != nil {
				return finalResult, stats, errVal
			}
			return finalResult, stats, fmt.Errorf("sub-agent error")
		}
	}

	flushThinking()
	flushText()

	duration := time.Since(startTime)
	stats := &tools.SubagentResult{
		Output:          finalResult,
		ShortID:         shortID,
		IterCount:       iterCount,
		Duration:        duration,
		ToolCallSummary: toolCalls,
	}
	childLogger.Info(ctx, "completed", "iters", iterCount, "duration", duration, "outputLen", len(finalResult), "toolCalls", toolCalls.String())

	return finalResult, stats, nil
}

// buildAllowedTools builds the list of allowed tool names, always excluding
// AskUserQuestion and SubAgent (which sub-agents cannot use).
func buildAllowedTools(e *Executor, allowList []string) []string {
	if len(allowList) == 0 {
		// No explicit filter → use all available tool names
		return e.AvailableToolNames()
	}
	// Filter the provided list, excluding disallowed tools
	result := make([]string, 0, len(allowList))
	for _, name := range allowList {
		if name == tools.ToolNameAskUser || name == tools.ToolNameSubAgent {
			continue
		}
		result = append(result, name)
	}
	return result
}

// usageToSession converts an llm.Usage to a session.Usage for persistence.
func usageToSession(u *llm.Usage) *session.Usage {
	if u == nil {
		return nil
	}
	return &session.Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
	}
}
