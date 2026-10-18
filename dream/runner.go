package dream

import (
	"context"
	"fmt"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
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
		provider = llm.WrapRecordingProvider(provider, cfg.Recorder, nil, nil)
	} else {
		provider = agent.WrapProviderForUsage(provider, cfg.Config)
	}

	// Take the inbox aside before anything is read: from here on, a memory
	// recorded by a live conversation lands in a fresh inbox.md that this run
	// never touches, so clearing the consumed snapshot cannot delete it.
	inboxTaken, inboxErr := TakeInbox(plan.Group.MemoryRoot, l)
	if inboxErr != nil {
		// A failure here must not abort the whole dream: the sessions are still
		// worth consolidating, and the inbox is left exactly as it was.
		l.Error(ctx, "dream: inbox handoff failed; continuing without inbox", inboxErr)
	}

	// Capture the watermark BEFORE building summaries: messages arriving
	// after this point are not included in the prompt, so they must remain
	// eligible for the next dream. Using the completion time as LastDreamAt
	// would silently skip them (their session's UpdatedAt would be older
	// than the watermark).
	snapshotAt := time.Now()

	// Build session summaries (pre-filtered to user+assistant only), bounded
	// by the per-run prompt budget. `through` is how far this run may move each
	// session's watermark.
	summaryBudget := 0
	if cfg.Config != nil {
		summaryBudget = cfg.Config.Dream.MaxSummaryChars
	}
	summaries, through := buildSessionSummaries(
		plan.ActiveSessions, loadMessages, plan.LastState, cfg.MaxMessageChars, summaryBudget, snapshotAt, l)

	// Build prompt.
	systemPrompt, userPrompt := BuildPrompt(plan, summaries, cfg.MaxMessageChars)

	// Create a sandboxed agent (bare: SkipConfigure skips built-in tools /
	// skills / reminders so the whitelist below is exhaustive).
	maxIter := cfg.MaxIter
	if maxIter <= 0 {
		maxIter = 30
	}
	dreamAgent, _, _ := agent.NewAIAgentWithConfig(ctx, agent.AgentConfig{
		Resolved:       &llm.ResolvedProvider{Provider: provider},
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
		// The agent saw the inbox but did not finish: put the raw material back
		// so the next run can complete the job.
		if inboxTaken {
			FinishInbox(plan.Group.MemoryRoot, false, l)
		}
		return State{}, lastErr
	}

	// Post-dream: scan topic files and update decay states. Reload the
	// latest on-disk state first so reinforcements recorded by TopicBackend
	// while this dream was running are preserved — ScanTopicFacts merges
	// them into the fresh scan (plan.LastState.FactStates is stale by the
	// duration of the dream).
	latestState := LoadState(plan.Group.MemoryRoot)
	halfLifeDays := 0
	if cfg.Config != nil {
		halfLifeDays = cfg.Config.Memory.DecayHalfLifeDays
	}
	factStates := ScanTopicFacts(plan.Group.MemoryRoot, latestState.FactStates, l, halfLifeDays)

	// Close the handoff: on success the consumed snapshot is gone, whatever the
	// agent did with it. inbox.md itself is never touched — it belongs to the
	// live writers.
	if inboxTaken {
		FinishInbox(plan.Group.MemoryRoot, true, l)
	}

	state := State{
		LastDreamAt:       snapshotAt,
		SessionsDreamed:   len(summaries),
		SessionWatermarks: mergeWatermarks(plan.LastState.SessionWatermarks, through),
		FactStates:        factStates,
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
		resolved, err := llm.BuildProvider(cfg.Config, cfg.DreamProvider)
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

// Context around the watermark, and the default prompt budget. The budget is
// mirrored by config.dream.max_summary_chars; it exists because a run that
// overflows the model's window fails completely, and a failed run advances no
// watermark.
const (
	contextPairsBefore       = 2
	defaultSummaryCharBudget = 100_000
)

// buildSessionSummaries loads the sessions in the caller's order (newest
// activity first) and returns both the prompt material and the watermark each
// session may advance to.
//
// Within one session the pairs are taken OLDEST first: the watermark is a
// cursor, so a session too long for this run has to continue from where it
// stopped next time, not skip its middle. A session that does not fit is left
// out and its watermark is untouched.
func buildSessionSummaries(
	sessions []*session.Session,
	loadMessages func(string) ([]session.Message, error),
	lastState State,
	maxMessageChars, budget int,
	snapshotAt time.Time,
	logger *logger.Logger,
) ([]SessionSummary, map[string]time.Time) {
	if maxMessageChars <= 0 {
		maxMessageChars = 2000
	}
	if budget <= 0 {
		budget = defaultSummaryCharBudget
	}

	var summaries []SessionSummary
	through := make(map[string]time.Time, len(sessions))
	remaining := budget

	for _, sess := range sessions {
		cutoff, ok := lastState.SessionWatermarks[sess.ID]
		if !ok {
			cutoff = lastState.LastDreamAt
		}

		msgs, err := loadMessages(sess.ID)
		if err != nil {
			if logger != nil {
				logger.Error(context.Background(), "failed to load messages", err, "id", sess.ID)
			}
			continue
		}

		pairs := FilterSessionMessages(msgs)
		if len(pairs) == 0 {
			// Nothing extractable here. Mark it read so every run does not
			// load the same file again for nothing.
			through[sess.ID] = snapshotAt
			continue
		}

		// Find the first pair that starts after this session's watermark. If
		// the watermark is zero (first dream), include all pairs.
		firstNewIdx := 0
		if !cutoff.IsZero() {
			firstNewIdx = -1
			for i, p := range pairs {
				if p.Timestamp.After(cutoff) {
					firstNewIdx = i
					break
				}
			}
			if firstNewIdx == -1 {
				// No new pairs (UpdatedAt moved but no conversation did — a
				// title edit, say). Nothing left to do for this session.
				through[sess.ID] = snapshotAt
				continue
			}
		}

		// NEW pairs are taken first. Progress must not depend on whether the
		// context pairs happen to fit the budget: taking context first could
		// spend everything on turns an earlier run already read, and the
		// cursor would never move.
		var included []MessagePair
		lastIncluded := time.Time{}
		stopped := false
		for i := firstNewIdx; i < len(pairs); i++ {
			cost := pairCost(pairs[i], maxMessageChars)
			if cost > remaining {
				stopped = true
				break
			}
			remaining -= cost
			included = append(included, pairs[i])
			lastIncluded = pairs[i].Timestamp
		}
		if len(included) == 0 {
			// Not even one new pair fits: stop, and leave this session (and
			// every later one) for the next run.
			break
		}

		// Context pairs go in FRONT, chronologically, and only while the budget
		// still holds them.
		contextStart := firstNewIdx
		for c := 0; c < contextPairsBefore && contextStart > 0; c++ {
			cand := pairs[contextStart-1]
			cost := pairCost(cand, maxMessageChars)
			if cost > remaining {
				break
			}
			remaining -= cost
			contextStart--
		}

		summary := SessionSummary{ID: sess.ID, Title: sess.Title}
		summary.Messages = append(summary.Messages, pairs[contextStart:firstNewIdx]...)
		summary.Messages = append(summary.Messages, included...)
		summaries = append(summaries, summary)

		if !stopped {
			// Everything this session had was included.
			through[sess.ID] = snapshotAt
		} else {
			// Advance only as far as the last NEW pair actually included.
			through[sess.ID] = lastIncluded
		}

		if stopped {
			break
		}
	}

	return summaries, through
}

// pairCost is what one exchange adds to the prompt, after the per-message
// truncation BuildPrompt applies.
func pairCost(p MessagePair, maxMessageChars int) int {
	return min(len(p.User), maxMessageChars) + min(len(p.Assistant), maxMessageChars)
}

// mergeWatermarks layers this run's progress over the previous map.
func mergeWatermarks(previous, updates map[string]time.Time) map[string]time.Time {
	if len(previous) == 0 && len(updates) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(previous)+len(updates))
	for id, at := range previous {
		out[id] = at
	}
	for id, at := range updates {
		out[id] = at
	}
	return out
}
