package agent

import (
	"context"

	"github.com/monsterxx03/tachi/agent/deepresearch"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// deepResearchRunnerAdapter adapts tools.SubagentRunner to deepresearch.SubagentRunner.
// The engine's interface only needs Run(ctx, prompt, allowedTools), which matches
// the SubagentRunner's RunSubagent semantics.
type deepResearchRunnerAdapter struct {
	runner tools.SubagentRunner
}

func (a *deepResearchRunnerAdapter) Run(ctx context.Context, prompt string, allowedTools []string) (string, error) {
	result, _, _, err := a.runner.RunSubagent(ctx, tools.SubagentArgs{
		Prompt:       prompt,
		AllowedTools: allowedTools,
	})
	if err != nil {
		return "", err
	}
	return result, nil
}

// NewDeepResearch creates a DeepResearch engine from the agent's configuration
// and sub-systems. Returns nil when deep research is not enabled in config.
//
// The returned engine uses the agent's SubagentRunner for research sub-agents
// and the agent's main provider (or a named provider from config) for query
// generation and report writing.
//
// Safe to call after Configure(). The engine is reusable across calls.
func (a *AIAgent) NewDeepResearch(cfg *config.Config) (*deepresearch.DeepResearch, error) {
	runner := &deepResearchRunnerAdapter{runner: a.Config.SubagentRunner}

	lg := a.Config.Logger

	return deepresearch.New(
		&cfg.DeepResearch,
		cfg.Providers,
		a.Config.Provider,
		runner,
		lg,
		cfg.MaxTokens,
	), nil
}

// NewDeepResearchWithProvider creates a DeepResearch engine using a specific
// LLM provider instead of the agent's default. Use this when the caller has
// a separate provider instance (e.g. channel mode's global provider).
func (a *AIAgent) NewDeepResearchWithProvider(
	cfg *config.Config,
	provider llm.Provider,
) (*deepresearch.DeepResearch, error) {
	runner := &deepResearchRunnerAdapter{runner: a.Config.SubagentRunner}

	lg := a.Config.Logger

	return deepresearch.New(
		&cfg.DeepResearch,
		cfg.Providers,
		provider,
		runner,
		lg,
		cfg.MaxTokens,
	), nil
}
