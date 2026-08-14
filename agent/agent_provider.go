package agent

import (
	"context"
	"errors"
	"strings"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// SetTitleGenEnabled enables or disables LLM-based title generation.
func (a *AIAgent) SetTitleGenEnabled(enabled bool) {
	a.titleGenEnabled = enabled
}

// resolveProviderByName resolves a configured provider name to an llm.Provider
// via the shared config.BuildProvider (the single home of the
// ProviderConfig → NewProvider dance). Returns nil on
// any failure with a warn/error log naming the provider — callers decide
// between silent fallback (dedicated providers) and fail-fast (adversarial
// review, checked later at /review start). Shared by every Setup*Provider
// method.
func (a *AIAgent) resolveProviderByName(cfg *config.Config, purpose, name string) llm.Provider {
	resolved, err := llm.BuildProvider(cfg, name)
	if errors.Is(err, llm.ErrProviderNotFound) {
		a.Config.Logger.Info(context.Background(), "Agent: "+purpose+" provider not found, falling back to main model", "provider", name)
		return nil
	}
	if err != nil {
		a.Config.Logger.Error(context.Background(), "Agent: failed to resolve "+purpose+" provider, falling back to main model", err, "provider", name)
		return nil
	}
	a.Config.Logger.Info(context.Background(), "Agent: using "+purpose+" provider", "provider", name, "type", resolved.Type, "model", resolved.Model)
	return resolved.Provider
}

// setupDedicatedProvider resolves the named provider (via the shared
// config.BuildProvider) and assigns it through the given setter, skipping
// when the name is empty or resolution fails. Shared by the Setup*Provider
// methods — they differ only in the config field, purpose string and
// assignment target.
//
// The resolved provider is wrapped for usage billing here: Setup resolves
// config NAMES into brand-new provider instances (BuildProvider), which
// never pass through wrapUsageProviders — wrapping in the assign path is the
// single choke point that covers title/commit/review/run/subagent alike.
// Caller-pre-resolved providers bypass this method entirely and are wrapped
// by wrapUsageProviders instead (WrapRecordingProvider is idempotent per
// recorder, so no path can double-wrap).
func (a *AIAgent) setupDedicatedProvider(cfg *config.Config, purpose, name string, assign func(llm.Provider)) {
	if cfg == nil || name == "" {
		return
	}
	if p := a.resolveProviderByName(cfg, purpose, name); p != nil {
		assign(wrapForUsage(p, a.usageRecorder(), cfg))
	}
}

// SetupTitleProvider resolves and creates a dedicated LLM provider for title
// generation from config. When title_provider is empty or not found, title
// generation falls back to the main conversation provider.
func (a *AIAgent) SetupTitleProvider(cfg *config.Config) {
	if cfg == nil {
		return
	}
	a.titleGenEnabled = cfg.TitleGeneration == nil || *cfg.TitleGeneration
	a.setupDedicatedProvider(cfg, "title", cfg.TitleProvider, func(p llm.Provider) {
		a.titleModelProvider = p
	})
}

// SetupCommitProvider resolves and creates a dedicated LLM provider for /commit
// message generation from config. When commit_provider is empty or not found,
// commit generation falls back to the main conversation provider.
func (a *AIAgent) SetupCommitProvider(cfg *config.Config) {
	a.setupDedicatedProvider(cfg, "commit", cfg.CommitProvider, func(p llm.Provider) {
		a.Config.CommitProvider = p
	})
}

// CommitProvider returns the dedicated commit provider, or nil if none is configured
// (caller should fall back to the main provider).
func (a *AIAgent) CommitProvider() llm.Provider {
	return a.Config.CommitProvider
}

// ReviewProvider returns the dedicated review provider, or nil if none is configured
// (caller should fall back to the main provider).
func (a *AIAgent) ReviewProvider() llm.Provider {
	return a.Config.ReviewProvider
}

// SetupReviewProvider resolves and creates a dedicated LLM provider for /review
// code review from config. When review.provider is empty or not found, /review
// falls back to the main conversation provider.
func (a *AIAgent) SetupReviewProvider(cfg *config.Config) {
	a.setupDedicatedProvider(cfg, "review", cfg.Review.Provider, func(p llm.Provider) {
		a.Config.ReviewProvider = p
	})
}

// ResolveAdversarialRoundModels validates the configured adversarial review
// providers and assigns a provider to every round, in one call — the single
// entry point both frontends (TUI / ACP) use. It combines the fail-fast
// check (cmds.CheckAdversarialProviders: configured-but-unresolvable model
// names abort before round 1) with the round assignment
// (cmds.ResolveRoundModels: models modulo-cycled, judge fixed on the final
// round, fallback used when no models are configured).
//
// cfg distinguishes "configured" from "not configured" (the Adversarial
// pointer is nil when unconfigured). rounds comes from
// cmds.ResolveReviewRounds.
func (a *AIAgent) ResolveAdversarialRoundModels(cfg *config.Config, fallback llm.Provider, rounds int) ([]llm.Provider, error) {
	models := a.Config.AdversarialModels
	judge := a.Config.AdversarialJudge
	if err := cmds.CheckAdversarialProviders(cfg, models, judge); err != nil {
		return nil, err
	}
	return cmds.ResolveRoundModels(models, judge, fallback, rounds), nil
}

// SetupAdversarialProviders resolves review.adversarial.models / judge_model
// names into llm.Provider at Configure time, mirroring SetupReviewProvider.
// Unresolvable names are recorded as nil with a warn log — the fail-fast
// check ("configured but failed to resolve → abort before round 1") happens
// in the /review handlers (sendReviewCommand / handleACPReview), which have
// access to the config to distinguish "configured" from "not configured".
//
// Idempotent: resets Config.AdversarialModels/Judge before populating, so
// repeated calls (Configure + an explicit re-setup) never accumulate
// entries — appending would silently grow the slice and trip
// cmds.CheckAdversarialProviders' length comparison (misreporting a
// configured-but-unresolved fail-fast on an otherwise healthy setup).
func (a *AIAgent) SetupAdversarialProviders(cfg *config.Config) {
	if cfg == nil || cfg.Review.Adversarial == nil {
		return
	}
	a.Config.AdversarialModels = nil
	a.Config.AdversarialJudge = nil
	adv := cfg.Review.Adversarial
	for _, name := range adv.Models {
		a.Config.AdversarialModels = append(a.Config.AdversarialModels, a.resolveProviderByName(cfg, "adversarial review", name))
	}
	if adv.JudgeModel != "" {
		a.Config.AdversarialJudge = a.resolveProviderByName(cfg, "adversarial review", adv.JudgeModel)
	}
}

// RunProvider returns the dedicated run provider, or nil if none is configured
// (caller should fall back to the main provider).
func (a *AIAgent) RunProvider() llm.Provider {
	return a.Config.RunProvider
}

// SetupRunProvider resolves and creates a dedicated LLM provider for tachi -p
// run mode from config. When run_provider is empty or not found, run mode
// falls back to the main conversation provider.
func (a *AIAgent) SetupRunProvider(cfg *config.Config) {
	a.setupDedicatedProvider(cfg, "run", cfg.RunProvider, func(p llm.Provider) {
		a.Config.RunProvider = p
	})
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
		p = a.Provider()
	}

	// Usage billing: title generation is a direct CreateChat call — tag the
	// kind so ledger rows are grouped under title. Session anchoring already
	// happens via chatOpts.SessionID below.
	ctx = llm.WithUsageKind(ctx, llm.UsageKindTitle)

	messages := []llm.Message{
		{
			Role:    "system",
			Content: "Generate a short, concise title (max 10 characters) for a conversation that starts with this user message. Output ONLY the title, no quotes, no explanation, no preamble.",
		},
		{
			Role:    "user",
			Content: firstMessage,
		},
	}

	thinkingDisabled := false
	chatOpts := llm.ChatOptions{MaxTokens: 4096, Thinking: &thinkingDisabled}
	if a.Config.SessionManager != nil && a.Config.SessionManager.Current() != nil {
		chatOpts.SessionID = a.Config.SessionManager.Current().ID
	}
	resp, err := p.CreateChat(ctx, messages, nil, chatOpts)
	if err != nil {
		a.Config.Logger.Error(ctx, "Agent: failed to generate title, falling back to truncation", err)
		return session.ExtractTitle(firstMessage)
	}

	title := strings.TrimSpace(resp.Content)
	if title == "" {
		a.Config.Logger.Info(ctx, "Agent: LLM returned empty title, falling back to truncation")
		return session.ExtractTitle(firstMessage)
	}

	a.Config.Logger.Info(ctx, "Agent: LLM generated title", "title", title)

	// Enforce max length via existing ExtractTitle
	return session.ExtractTitle(title)
}
