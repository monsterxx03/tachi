package agent

import (
	"context"

	"github.com/monsterxx03/tachi/agent/deepresearch"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
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
// and sub-systems. It never returns a nil engine — there is no config switch that
// turns deep research off, so a construction ERROR is the only way to fail.
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
		a.Provider(),
		runner,
		lg,
		cfg.MaxTokens,
	), nil
}

