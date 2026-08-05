package agent

import (
	"context"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// RunCommitOneOff starts a one-off /commit run on a clean context (no
// conversation history). Thinking is always disabled — the commit task is
// simple and avoiding thinking saves tokens/latency. The dedicated commit
// provider is used when configured, and the prompt's co-author trailer
// follows the model that actually runs (not the main provider's).
//
// userPrompt overrides the default CommitUserPrompt when non-empty (tachi -c
// appends -p / stdin content to the base prompt). maxTokens <= 0 falls back
// to config.DefaultMaxTokens. Callers drain the returned event stream and
// deliver the output per frontend.
func (a *AIAgent) RunCommitOneOff(ctx context.Context, systemPrompt, sessionID string, maxTokens int, userPrompt string) <-chan AgentEvent {
	commitProvider := a.CommitProvider()
	model := a.Model()
	if commitProvider != nil {
		// The co-author trailer must reflect the model that actually runs.
		model = commitProvider.Model()
	}
	if maxTokens <= 0 {
		maxTokens = config.DefaultMaxTokens
	}
	thinkingDisabled := false
	opts := llm.ChatOptions{
		MaxTokens: maxTokens,
		Thinking:  &thinkingDisabled,
	}
	prompt := cmds.CommitUserPrompt(model)
	if userPrompt != "" {
		prompt = userPrompt
	}
	return a.RunOneOffStream(ctx, commitProvider, systemPrompt, prompt, opts,
		WithToolSet(tools.ToolNameBash),
		WithOneOffMeta(&OneOffMeta{Kind: llm.UsageKindCommit, SessionID: sessionID}))
}
