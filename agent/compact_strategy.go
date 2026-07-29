package agent

import (
	"context"
	"fmt"

	"github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/llm"
)

// CompactStrategy generates a conversational summary from the given messages.
//
// The agent loop calls this during auto-compact to replace the growing
// conversation history with a compressed summary. Separating the strategy
// from doCompact lets tests inject a fake that returns a fixed summary
// without needing a real LLM provider.
type CompactStrategy interface {
	// Compact produces a summary of the conversation history.
	Compact(ctx context.Context, messages []llm.Message, maxTokens int) (string, error)
}

// llmCompactStrategy is the production implementation that calls the LLM
// provider to generate a summary.
type llmCompactStrategy struct {
	provider llm.Provider
}

func (s *llmCompactStrategy) Compact(ctx context.Context, messages []llm.Message, maxTokens int) (string, error) {
	compactPrompt := commands.BuildCompactInstruction()
	compactMsgs := make([]llm.Message, len(messages))
	copy(compactMsgs, messages)
	compactMsgs = append(compactMsgs, llm.Message{Role: "user", Content: compactPrompt})

	resp, err := s.provider.CreateChat(ctx, compactMsgs, nil, llm.ChatOptions{
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("compact LLM call: %w", err)
	}
	return resp.Content, nil
}
