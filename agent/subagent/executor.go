package subagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
)

// Executor implements tools.SubagentRunner by creating and running child
// agents. It also manages the concurrency semaphore for parallel execution.
type Executor struct {
	agent       Agent
	cfg         config.SubagentConfig
	sem         chan struct{} // concurrency semaphore
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
		sem:   make(chan struct{}, maxConc),
	}
}

// EnableWorktree creates and attaches a WorktreeManager from config.
func (e *Executor) EnableWorktree(logger *debuglog.Logger) {
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
func (e *Executor) RunSubagent(
	ctx context.Context,
	args tools.SubagentArgs,
) (string, string, error) {
	// Acquire concurrency semaphore
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return "", "", ctx.Err()
	}

	provider := e.agent.SubagentProvider()

	maxIterations := args.MaxIterations
	if maxIterations <= 0 {
		maxIterations = e.cfg.MaxIterations
		if maxIterations <= 0 {
			maxIterations = DefaultMaxIterations
		}
	}

	shortID := uuid.New().String()[:8]
	thinking := e.cfg.Thinking
	branch := args.WorktreeBranch

	// If worktree is enabled, delegate to WorktreeManager
	if e.worktreeMgr != nil {
		result, err := e.worktreeMgr.Create(ctx, branch, func(worktreeCtx context.Context, wtPath string) (string, error) {
			e.agent.Logger().Log("[subagent:%s] worktree created at %s (branch=%s)", shortID, wtPath, fallbackIfEmpty(branch, "detached"))
			return e.run(worktreeCtx, shortID, args, provider, maxIterations, thinking, branch, wtPath)
		})
		return result, shortID, err
	}

	result, err := e.run(ctx, shortID, args, provider, maxIterations, thinking, "", "")
	return result, shortID, err
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
) (string, error) {
	subagentSessionID := shortID
	if sm := e.agent.SessionManager(); sm != nil {
		if cur := sm.Current(); cur != nil {
			subagentSessionID = cur.ID + ":" + shortID
		}
	}

	childLogger := e.agent.Logger().WithSessionID(subagentSessionID).WithPrefix(fmt.Sprintf("[subagent:%s]", shortID))

	// Build allowed tools list — exclude AskUserQuestion and SubAgent
	allowedTools := buildAllowedTools(e, args.AllowedTools)

	child := e.agent.NewChildAgent(childLogger, provider, maxIterations, allowedTools, subagentSessionID)
	childLogger.Log("starting | prompt_len=%d tools=%d max_iters=%d thinking=%v worktree=%v session_id=%s",
		len(args.Prompt), len(allowedTools), maxIterations, thinking, worktreePath != "", subagentSessionID)

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
	if sm := e.agent.SessionManager(); sm != nil {
		if cur := sm.Current(); cur != nil {
			var recErr error
			rec, recErr = newRecorder(cur.ID, shortID)
			if recErr != nil {
				childLogger.Log("failed to create recorder: %v", recErr)
			} else {
				defer rec.close()
				rec.record(&session.Message{
					Type:    session.MessageTypeUser,
					Content: args.Prompt,
				})
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

	flushThinking := func() {
		if rec == nil || thinkingBuf.Len() == 0 {
			return
		}
		rec.record(&session.Message{
			Type:    session.MessageTypeThinking,
			Content: thinkingBuf.String(),
		})
		thinkingBuf.Reset()
	}

	flushText := func() {
		if rec == nil || sb.Len() == 0 {
			return
		}
		rec.record(&session.Message{
			Type:    session.MessageTypeAssistant,
			Content: sb.String(),
		})
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
				rec.record(&session.Message{
					Type:       session.MessageTypeToolCall,
					Name:       event.ToolName,
					Args:       event.ToolArgs,
					ToolCallID: event.ToolID,
				})
			}

		case StreamEventToolResult:
			if rec != nil {
				rec.record(&session.Message{
					Type:       session.MessageTypeToolResult,
					Name:       event.ToolName,
					Result:     event.ToolResult,
					IsError:    event.ToolIsError,
					ToolCallID: event.ToolID,
				})
			}
			iterCount++

		case StreamEventTurnComplete:
			flushThinking()
			if rec != nil && (sb.Len() > 0 || event.Usage != nil) {
				rec.record(&session.Message{
					Type:    session.MessageTypeAssistant,
					Content: sb.String(),
					Usage:   usageToSession(event.Usage),
				})
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
			childLogger.Log("completed with error | iters=%d duration=%s output_len=%d err=%v",
				iterCount, duration, len(finalResult), errVal)
			if errVal != nil {
				return finalResult, errVal
			}
			return finalResult, fmt.Errorf("sub-agent error")
		}
	}

	flushThinking()
	flushText()

	duration := time.Since(startTime)
	childLogger.Log("completed | iters=%d duration=%s output_len=%d",
		iterCount, duration, len(finalResult))

	return finalResult, nil
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