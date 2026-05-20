package agent

import (
	"context"
	"strings"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

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
			Content: "Generate a short, concise title (max 10 characters) for a conversation that starts with this user message. Output ONLY the title, no quotes, no explanation, no preamble.",
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
