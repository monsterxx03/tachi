package dream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// RunConfig holds the parameters needed to execute a dream sub-agent.
type RunConfig struct {
	// FallbackProvider is used when DreamConfig doesn't specify its own provider.
	FallbackProvider llm.Provider

	// DreamProvider from config — if set, dream resolves its own provider.
	DreamProvider string // provider name (empty → use fallback)

	// Config is the full config, used to resolve DreamProvider via the shared
	// config.BuildProvider (env-aware API keys + field validation). Required
	// when DreamProvider is set.
	Config *config.Config

	// Recorder, when non-nil, is used for usage-ledger rows instead of the
	// process-wide recorder (agent.WrapProviderForUsage). Tests inject a
	// temp-dir recorder for isolation; production leaves it nil.
	Recorder *llm.UsageRecorder

	MaxIter         int
	MaxTokens       int
	MaxMessageChars int // max chars per message in prompt (default 2000)
	Logger          *logger.Logger
}

// RunDream executes the full dream pipeline for one domain plan.
// It creates a sandboxed sub-agent with restricted tools (ReadFile, Grep, Glob,
// WriteFile) and a PathPolicy limiting writes to the memory directory.
//
// Provider resolution: DreamProvider (from config) > FallbackProvider (main).
func RunDream(ctx context.Context, plan Plan, cfg RunConfig, loadMessages func(id string) ([]session.Message, error)) (State, error) {
	l := cfg.Logger
	l = l.With("source", "dream:run")

	l.Info(ctx, "starting",
		"domain", plan.Group.Domain, "root", plan.Group.Root, "memory_root", plan.Group.MemoryRoot, "active_sessions", len(plan.ActiveSessions))

	// Resolve provider. The ledger row's provider name comes from the
	// provider itself (Provider.ProviderName), so no name is threaded here.
	provider, err := resolveProvider(cfg)
	if err != nil {
		return State{}, err
	}
	// Usage billing: dream agents are bare NewAIAgent constructions whose
	// provider never passes through NewAIAgentWithConfig's wrapping — wrap it
	// here so dream LLM calls land in the ledger (idempotent: an already
	// wrapped fallback, e.g. TUI's main provider, passes through untouched).
	if cfg.Recorder != nil {
		provider = llm.WrapRecordingProvider(provider, cfg.Recorder, nil)
	} else {
		provider = agent.WrapProviderForUsage(provider, cfg.Config)
	}

	// Capture the watermark BEFORE building summaries: messages arriving
	// after this point are not included in the prompt, so they must remain
	// eligible for the next dream. Using the completion time as LastDreamAt
	// would silently skip them (their session's UpdatedAt would be older
	// than the watermark).
	snapshotAt := time.Now()

	// Build session summaries (pre-filtered to user+assistant only).
	summaries := buildSessionSummaries(plan.ActiveSessions, loadMessages, plan.LastState.LastDreamAt, l)

	// Build prompt.
	systemPrompt, userPrompt := BuildPrompt(plan, summaries, cfg.MaxMessageChars)

	// Create a sandboxed agent (bare: SkipConfigure skips built-in tools /
	// skills / reminders so the whitelist below is exhaustive).
	maxIter := cfg.MaxIter
	if maxIter <= 0 {
		maxIter = 30
	}
	dreamAgent, _, _ := agent.NewAIAgentWithConfig(ctx, agent.AgentConfig{
		Resolved:       &config.ResolvedProvider{Provider: provider},
		MaxIterations:  maxIter,
		PermissionMode: agent.PermissionModeSkip,
		SkipConfigure:  true,
	})

	// Register only allowed tools: ReadFile, Grep, Glob, WriteFile.
	dreamAgent.RegisterTool(tools.NewReadTool())
	dreamAgent.RegisterTool(tools.GrepTool{})
	dreamAgent.RegisterTool(tools.GlobTool{})
	dreamAgent.RegisterTool(tools.NewWriteTool())

	// Inject PathPolicy: restrict WriteFile to only the memory directory.
	policy := &tools.PathPolicy{
		AllowedWriteDirs: []string{plan.Group.MemoryRoot},
	}
	ctx = tools.WithPathPolicy(ctx, policy)

	// Set working directory to memory root so relative paths resolve there.
	ctx = wdctx.WithDir(ctx, plan.Group.MemoryRoot)

	// Run the dream agent. Recorded as a one-off transcript (global dir) so
	// bad memory consolidations can be traced back to the exact run.
	eventCh := dreamAgent.RunOneOffStream(ctx, provider, systemPrompt, userPrompt, llm.ChatOptions{
		MaxTokens: cfg.MaxTokens,
	}, agent.WithOneOffMeta(&agent.OneOffMeta{
		Kind:  llm.UsageKindDream,
		Extra: map[string]string{"domain": plan.Group.Domain, "root": plan.Group.Root},
	}))

	// Drain events.
	var lastErr error
	for ev := range eventCh {
		if ev.Type == agent.AgentEventError && ev.Result != nil && ev.Result.Error != nil {
			lastErr = ev.Result.Error
			l.Error(ctx, "dream agent error", lastErr, "domain", plan.Group.Domain, "root", plan.Group.Root)
		}
	}

	if lastErr != nil {
		return State{}, lastErr
	}

	// Post-dream: scan topic files and update decay states. Reload the
	// latest on-disk state first so reinforcements recorded by TopicBackend
	// while this dream was running are preserved — ScanTopicFacts merges
	// them into the fresh scan (plan.LastState.FactStates is stale by the
	// duration of the dream).
	latestState := LoadState(plan.Group.MemoryRoot)
	factStates := ScanTopicFacts(plan.Group.MemoryRoot, latestState.FactStates, l)

	// Ensure inbox.md was cleared. The dream agent is instructed to integrate
	// inbox content into topic files and then clear the inbox. If the agent
	// forgot, force-clear to prevent stale content from accumulating and being
	// re-processed in the next dream.
	ensureInboxCleared(plan.Group.MemoryRoot, l)

	state := State{
		LastDreamAt:     snapshotAt,
		SessionsDreamed: len(plan.ActiveSessions),
		FactStates:      factStates,
	}

	l.Info(ctx, "completed successfully", "domain", plan.Group.Domain, "root", plan.Group.Root)
	return state, nil
}

// resolveProvider picks the provider: DreamProvider config > FallbackProvider.
// resolveProvider picks the provider: DreamProvider config > FallbackProvider.
func resolveProvider(cfg RunConfig) (llm.Provider, error) {
	if cfg.DreamProvider != "" {
		if cfg.Config == nil {
			return nil, fmt.Errorf("dream: config required to resolve provider %q", cfg.DreamProvider)
		}
		resolved, err := cfg.Config.BuildProvider(cfg.DreamProvider)
		if err != nil {
			return nil, fmt.Errorf("dream: resolve provider: %w", err)
		}
		return resolved.Provider, nil
	}

	// Use fallback.
	if cfg.FallbackProvider == nil {
		return nil, fmt.Errorf("dream: no provider available")
	}
	return cfg.FallbackProvider, nil
}

// buildSessionSummaries loads and filters messages for each active session.
// For each session, it includes all conversation turns that started after
// lastDreamAt, plus up to 2 preceding turns for context. If lastDreamAt is
// zero (first dream), all pairs are included.
func buildSessionSummaries(sessions []*session.Session, loadMessages func(string) ([]session.Message, error), lastDreamAt time.Time, logger *logger.Logger) []SessionSummary {
	var summaries []SessionSummary

	for _, sess := range sessions {
		msgs, err := loadMessages(sess.ID)
		if err != nil {
			logger.Error(context.Background(), "failed to load messages", err, "id", sess.ID)
			continue
		}

		pairs := FilterSessionMessages(msgs)
		if len(pairs) == 0 {
			continue
		}

		// Find the first pair whose user message occurred after lastDreamAt.
		// If lastDreamAt is zero (first dream), include all pairs from the beginning.
		firstNewIdx := 0
		if !lastDreamAt.IsZero() {
			firstNewIdx = -1
			for i, p := range pairs {
				if p.Timestamp.After(lastDreamAt) {
					firstNewIdx = i
					break
				}
			}
			if firstNewIdx == -1 {
				// No new pairs in this session (shouldn't normally happen since
				// ActiveSessionsSince already filtered by UpdatedAt, but be safe).
				continue
			}
		}

		// Include 2 preceding turns for context, clamped to start of slice.
		contextWindow := 2
		startIdx := max(firstNewIdx-contextWindow, 0)

		pairs = pairs[startIdx:]

		summaries = append(summaries, SessionSummary{
			ID:       sess.ID,
			Title:    sess.Title,
			Messages: pairs,
		})
	}

	return summaries
}

// ensureInboxCleared verifies that inbox.md is empty after a dream run.
// The dream agent is instructed to integrate inbox content into topic files
// and then clear the inbox. If the agent forgot, we force-clear here to
// prevent stale content from being re-processed in the next dream.
func ensureInboxCleared(memoryRoot string, logger *logger.Logger) {
	inboxPath := filepath.Join(memoryRoot, "inbox.md")
	info, err := os.Stat(inboxPath)
	if os.IsNotExist(err) {
		return // already clean
	}
	if err != nil {
		logger.Error(context.Background(), "ensureInboxCleared: stat failed", err, "path", inboxPath)
		return
	}
	if info.Size() == 0 {
		return // already empty
	}

	logger.Info(context.Background(), "inbox.md has content after dream — force-clearing", "bytes", info.Size())
	if err := fileutil.WriteFileShared(inboxPath, []byte{}); err != nil {
		logger.Error(context.Background(), "ensureInboxCleared: failed to truncate", err, "path", inboxPath)
	}
}
