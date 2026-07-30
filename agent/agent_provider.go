package agent

import (
	"context"
	"strings"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

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
		a.Config.Logger.Info(context.Background(), "Agent: title provider not found, falling back to main model", "provider", tpName)
		return
	}

	resolved, err := config.ResolveProviderConfig(tpCfg)
	if err != nil {
		a.Config.Logger.Error(context.Background(), "Agent: failed to resolve title provider, falling back to main model", err, "provider", tpName)
		return
	}

	tp, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		a.Config.Logger.Error(context.Background(), "Agent: failed to create title provider, falling back to main model", err, "provider", tpName)
		return
	}

	a.titleModelProvider = tp
	a.Config.Logger.Info(context.Background(), "Agent: using title provider for session title generation", "provider", tpName, "type", resolved.Type, "model", resolved.Model)
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
		a.Config.Logger.Info(context.Background(), "Agent: commit provider not found, falling back to main model", "provider", cpName)
		return
	}

	resolved, err := config.ResolveProviderConfig(cpCfg)
	if err != nil {
		a.Config.Logger.Error(context.Background(), "Agent: failed to resolve commit provider, falling back to main model", err, "provider", cpName)
		return
	}

	cp, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		a.Config.Logger.Error(context.Background(), "Agent: failed to create commit provider, falling back to main model", err, "provider", cpName)
		return
	}

	a.Config.CommitProvider = cp
	a.Config.Logger.Info(context.Background(), "Agent: using commit provider for /commit message generation", "provider", cpName, "type", resolved.Type, "model", resolved.Model)
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
	if cfg == nil {
		return
	}
	rpName := cfg.Review.Provider
	if rpName == "" {
		return
	}

	rpCfg := cfg.FindProvider(rpName)
	if rpCfg == nil {
		a.Config.Logger.Info(context.Background(), "Agent: review provider not found, falling back to main model", "provider", rpName)
		return
	}

	resolved, err := config.ResolveProviderConfig(rpCfg)
	if err != nil {
		a.Config.Logger.Error(context.Background(), "Agent: failed to resolve review provider, falling back to main model", err, "provider", rpName)
		return
	}

	rp, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		a.Config.Logger.Error(context.Background(), "Agent: failed to create review provider, falling back to main model", err, "provider", rpName)
		return
	}

	a.Config.ReviewProvider = rp
	a.Config.Logger.Info(context.Background(), "Agent: using review provider for /review code review", "provider", rpName, "type", resolved.Type, "model", resolved.Model)
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
	rpName := cfg.RunProvider
	if rpName == "" {
		return
	}

	rpCfg := cfg.FindProvider(rpName)
	if rpCfg == nil {
		a.Config.Logger.Info(context.Background(), "Agent: run provider not found, falling back to main model", "provider", rpName)
		return
	}

	resolved, err := config.ResolveProviderConfig(rpCfg)
	if err != nil {
		a.Config.Logger.Error(context.Background(), "Agent: failed to resolve run provider, falling back to main model", err, "provider", rpName)
		return
	}

	rp, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		a.Config.Logger.Error(context.Background(), "Agent: failed to create run provider, falling back to main model", err, "provider", rpName)
		return
	}

	a.Config.RunProvider = rp
	a.Config.Logger.Info(context.Background(), "Agent: using run provider for tachi -p mode", "provider", rpName, "type", resolved.Type, "model", resolved.Model)
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
		p = a.Config.Provider
	}

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
