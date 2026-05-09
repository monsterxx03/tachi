package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/transcript"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

const (
	defaultSubagentMaxIterations  = 50
	defaultSubagentMaxConcurrency = 4
	defaultSubagentMaxOutputChars = 16384
)

const subagentSystemPrompt = `You are a focused sub-agent of Tachi, an AI coding assistant. Complete the delegated task efficiently and return a clear summary.

Rules:
- Stay strictly on-task. Do not explore tangents or make unrelated changes.
- Use tools aggressively — read files, search code, run commands as needed.
- DO NOT ask the user questions. If you need input, explain what's missing in your summary.
- DO NOT attempt to delegate to sub-agents — you cannot spawn further sub-agents.
- File edits are auto-confirmed. Be careful — double-check before writing.
- If the task is too large for your budget, return your best partial results with a note about what remains.
- Format your output for the main agent to read: structured, concise, actionable.

Your output goes directly back to the main agent — no preamble, no closing remarks like "I've completed the task". Just the findings.`

const subagentWorktreePrompt = `
You are working in an isolated git worktree. Your working directory is a
temporary checkout of the repository — changes here will NOT affect the main
working tree unless you push or create a PR from this branch.

- All file paths are relative to your worktree directory.
- Use Bash to run git commands — they operate on this worktree in isolation.
- Your worktree starts from %s (detached HEAD). You can commit, push, and
  create branches as needed without affecting the main worktree.
- Any file modifications you make will be automatically collected as a patch
  and returned to the parent agent. You do NOT need to output diffs manually.
- If you need to persist changes beyond the patch, push to remote.
- IMPORTANT: In detached HEAD mode, commits not attached to a branch will be
  garbage collected after ~28 days. Always push or create a branch to persist.`

// SubagentExecutor implements tools.SubagentRunner by creating and running child
// AIAgent instances. It also manages the concurrency semaphore for parallel
// sub-agent execution.
type SubagentExecutor struct {
	parentAgent *AIAgent
	logger      *debuglog.Logger
	sem         chan struct{} // concurrency semaphore, size = MaxConcurrency
	worktreeMgr *WorktreeManager
}

// NewSubagentExecutor creates a new SubagentExecutor bound to the given parent agent.
func NewSubagentExecutor(parent *AIAgent) *SubagentExecutor {
	maxConc := parent.SubagentMaxConcurrency()
	return &SubagentExecutor{
		parentAgent: parent,
		logger:      parent.logger,
		sem:         make(chan struct{}, maxConc),
	}
}

// SetWorktreeManager sets the worktree manager for git worktree isolation.
// When nil (default), worktree isolation is disabled.
func (e *SubagentExecutor) SetWorktreeManager(wm *WorktreeManager) {
	e.worktreeMgr = wm
}

// AvailableToolNames returns all tool names the sub-agent can use (for description).
// Excludes AskUserQuestion and SubAgent (which sub-agents cannot use).
// Uses ToolNames() instead of ToolSchemas() to avoid infinite recursion
// (GetSchemas → ToSchema → Description → AvailableToolNames → GetSchemas → ...).
func (e *SubagentExecutor) AvailableToolNames() []string {
	allNames := e.parentAgent.ToolNames()
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
func (e *SubagentExecutor) MaxOutputChars() int {
	return e.parentAgent.SubagentMaxOutputChars()
}

// RunSubagent creates and runs a child AIAgent to execute the given task.
// It blocks until the child completes or the context is cancelled.
func (e *SubagentExecutor) RunSubagent(
	ctx context.Context,
	args tools.SubagentArgs,
) (string, error) {
	// Acquire concurrency semaphore (block until slot available or ctx cancelled)
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Determine provider and model (fallback logic)
	provider := e.parentAgent.SubagentProvider()
	model := e.parentAgent.SubagentModel()

	// Determine iteration budget
	maxIterations := args.MaxIterations
	if maxIterations <= 0 {
		maxIterations = e.parentAgent.SubagentMaxIterations()
	}

	// Create child agent with a unique ID for logging
	shortID := uuid.New().String()[:8]

	// Determine thinking configuration
	thinking := e.parentAgent.SubagentThinking()

	// Determine branch for worktree
	branch := args.WorktreeBranch

	// If worktree is enabled, delegate to WorktreeManager
	if e.worktreeMgr != nil {
		return e.worktreeMgr.Create(ctx, branch, func(worktreeCtx context.Context, wtPath string) (string, error) {
			e.logger.Log("[subagent:%s] worktree created at %s (branch=%s)", shortID, wtPath, fallbackIfEmpty(branch, "detached"))
			return e.runChildAgent(worktreeCtx, shortID, args, provider, model, maxIterations, thinking, branch, wtPath)
		})
	}

	return e.runChildAgent(ctx, shortID, args, provider, model, maxIterations, thinking, "", "")
}

// runChildAgent is the internal method that creates and runs the child AIAgent.
// When worktreePath is non-empty, the worktree prompt is appended to the system prompt.
func (e *SubagentExecutor) runChildAgent(
	ctx context.Context,
	shortID string,
	args tools.SubagentArgs,
	provider llm.Provider,
	model string,
	maxIterations int,
	thinking bool,
	branch string,
	worktreePath string,
) (string, error) {
	childLogger := e.logger.WithPrefix(fmt.Sprintf("[subagent:%s]", shortID))

	child := NewAIAgent(provider, model, maxIterations)
	child.SetSkipEditConfirm(true)
	child.SetLogger(childLogger)
	child.SetReminderCollector(nil) // Disable all system reminders

	// Register filtered tools
	child.RegisterToolsForSubagent(e.parentAgent, args.AllowedTools)

	// Propagate transcript builder from context (parent injected it via
	// transcript.WithBuilder before calling RunSubagent).
	if subBuilder := transcript.BuilderFromContext(ctx); subBuilder != nil {
		child.SetTranscriptBuilder(subBuilder)
	}

	// Build system prompt
	systemPrompt := subagentSystemPrompt
	if worktreePath != "" {
		// Explain which branch the worktree starts from.
		// It is always detached HEAD — the branch is just the starting point.
		source := branch
		if source == "" {
			source = "HEAD"
		}
		systemPrompt += fmt.Sprintf(subagentWorktreePrompt, source)
	}

	// Compose the subagent session ID for the x-tachi-session-id header.
	// Format: <main_session_id>:<subagent_id> so backend analytics can
	// correlate sub-agent API calls with their parent session.
	subagentSessionID := shortID
	if sm := e.parentAgent.SessionManager(); sm != nil {
		if cur := sm.Current(); cur != nil {
			subagentSessionID = cur.ID + ":" + shortID
		}
	}

	childLogger.Log("starting | prompt_len=%d tools=%d max_iters=%d thinking=%v worktree=%s session_id=%s",
		len(args.Prompt), len(child.ToolSchemas()), maxIterations, thinking, fallbackIfEmpty(worktreePath, "no"), subagentSessionID)

	// Run via RunOneOffStream
	ch := child.RunOneOffStream(ctx, provider, systemPrompt, args.Prompt, llm.ChatOptions{
		MaxTokens: defaultMaxTokens,
		Thinking:  &thinking,
		SessionID: subagentSessionID,
	})

	// Consume events, collect result + stats
	var sb strings.Builder
	startTime := time.Now()
	iterCount := 0

	for event := range ch {
		switch event.Type {
		case AgentEventTextDelta:
			sb.WriteString(event.TextDelta)
		case AgentEventToolResult:
			iterCount++
		case AgentEventError:
			duration := time.Since(startTime)
			var errVal error
			if event.Result != nil {
				errVal = event.Result.Error
			}
			childLogger.Log("completed with error | iters=%d duration=%s output_len=%d err=%v",
				iterCount, duration, sb.Len(), errVal)
			if errVal != nil {
				return sb.String(), errVal
			}
			return sb.String(), fmt.Errorf("sub-agent error")
		}
	}

	// Log stats
	duration := time.Since(startTime)
	childLogger.Log("completed | iters=%d duration=%s output_len=%d",
		iterCount, duration, sb.Len())

	return sb.String(), nil
}

func fallbackIfEmpty(s string, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// RegisterToolsForSubagent registers a filtered subset of the parent's tools
// on the child agent. If allowedTools is empty, all parent tools are registered
// EXCEPT AskUserQuestion and SubAgent. If allowedTools is non-empty, only those
// tools are registered (still excluding AskUserQuestion and SubAgent).
func (a *AIAgent) RegisterToolsForSubagent(parent *AIAgent, allowedTools []string) {
	parentSchemas := parent.toolRegistry.GetSchemas()

	// Build a set of allowed names for quick lookup
	var allowSet map[string]bool
	if len(allowedTools) > 0 {
		allowSet = make(map[string]bool, len(allowedTools))
		for _, name := range allowedTools {
			allowSet[name] = true
		}
	}

	for _, schema := range parentSchemas {
		name := schema.Name

		// Never register these in sub-agents
		if name == tools.ToolNameAskUser || name == tools.ToolNameSubAgent {
			continue
		}

		// If allowedTools was specified, only register tools in the whitelist
		if allowSet != nil && !allowSet[name] {
			continue
		}

		// Get the actual tool from parent's registry and register it
		tool := parent.toolRegistry.GetTool(name)
		if tool != nil {
			a.toolRegistry.Register(tool)
		}
	}
}

// --- AIAgent subagent-related fields and methods ---

// SetupSubagentProvider resolves and creates a dedicated LLM provider for
// sub-agent execution from config. Falls back to main provider when not set.
func (a *AIAgent) SetupSubagentProvider(cfg *config.Config) {
	sc := cfg.Subagent

	// Store config values
	a.subagentMaxIterations = sc.MaxIterations
	a.subagentMaxConcurrency = sc.MaxConcurrency
	a.subagentMaxOutputChars = sc.MaxOutputChars
	a.subagentThinking = sc.Thinking

	if sc.Provider == "" {
		return
	}

	pCfg := cfg.FindProvider(sc.Provider)
	if pCfg == nil {
		a.logger.Log("Agent: subagent.provider %q not found in providers list, falling back to main model", sc.Provider)
		return
	}

	// If subagent has a model override, apply it
	overridden := *pCfg
	if sc.Model != "" {
		overridden.Model = sc.Model
	}

	resolved, err := config.ResolveProviderConfig(&overridden)
	if err != nil {
		a.logger.Log("Agent: failed to resolve subagent provider %q: %v, falling back to main model", sc.Provider, err)
		return
	}

	sp, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		a.logger.Log("Agent: failed to create subagent provider %q: %v, falling back to main model", sc.Provider, err)
		return
	}

	a.subagentProvider = sp
	a.subagentModel = resolved.Model
	a.logger.Log("Agent: using subagent provider %q (%s/%s)", sc.Provider, resolved.Type, resolved.Model)
}

// SubagentProvider returns the sub-agent provider or falls back to main.
func (a *AIAgent) SubagentProvider() llm.Provider {
	if a.subagentProvider != nil {
		return a.subagentProvider
	}
	return a.provider
}

// SubagentModel returns the sub-agent model or falls back to main.
func (a *AIAgent) SubagentModel() string {
	if a.subagentModel != "" {
		return a.subagentModel
	}
	return a.model
}

// SubagentMaxIterations returns the configured max iterations for sub-agents.
// Returns hardcoded default (50) when config value is 0.
func (a *AIAgent) SubagentMaxIterations() int {
	if a.subagentMaxIterations > 0 {
		return a.subagentMaxIterations
	}
	return defaultSubagentMaxIterations
}

// SubagentMaxConcurrency returns the concurrency semaphore limit.
// Returns hardcoded default (4) when config value is 0.
func (a *AIAgent) SubagentMaxConcurrency() int {
	if a.subagentMaxConcurrency > 0 {
		return a.subagentMaxConcurrency
	}
	return defaultSubagentMaxConcurrency
}

// SubagentMaxOutputChars returns the output truncation threshold.
// Returns hardcoded default (16384) when config value is 0.
func (a *AIAgent) SubagentMaxOutputChars() int {
	if a.subagentMaxOutputChars > 0 {
		return a.subagentMaxOutputChars
	}
	return defaultSubagentMaxOutputChars
}

// SubagentThinking returns whether sub-agents should enable thinking.
func (a *AIAgent) SubagentThinking() bool {
	return a.subagentThinking
}