package agent

import (
	"context"
	"fmt"

	"github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/llm"
)

// doCompact performs a synchronous session compaction:
//  1. Sends the full conversation history + compact instruction to the LLM
//     via provider.CreateChat (non-streaming, no agent loop).
//  2. Stores the old session's memory.
//  3. Creates a new session with the compacted summary via FinalizeCompact.
//  4. Notifies the memory backend of the new session.
//
// It uses context.Background() with the configured timeout so that a
// conversation interruption (ctx cancellation) does not produce orphan
// sessions. The conversation's cancellation signal is forwarded to the
// compact context so that user-initiated aborts are still respected.
//
// doCompact does NOT clear or restore the tool registry — it calls
// CreateChat directly with tools=nil, so no registry state is affected.
func (a *AIAgent) doCompact(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
	// 1. Independent timeout: use Background() so conversation cancellation
	//    doesn't leave orphan sessions behind.
	compactCtx, cancel := context.WithTimeout(context.Background(), a.cfg.Compact.Timeout)
	defer cancel()

	// Forward conversation cancellation — user Ctrl+C still aborts compact.
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-compactCtx.Done():
		}
	}()

	// 2. Build the compact prompt (language-aware).
	language := ""
	if a.cfg != nil {
		language = a.cfg.Language
	}
	compactPrompt := commands.BuildCompactInstruction(language)

	// 3. Append the compact instruction as a user message.
	compactMsgs := make([]llm.Message, len(messages))
	copy(compactMsgs, messages)
	compactMsgs = append(compactMsgs, llm.Message{Role: "user", Content: compactPrompt})

	// 4. Call the provider directly (non-streaming, no tools).
	resp, err := a.provider.CreateChat(compactCtx, compactMsgs, nil, llm.ChatOptions{
		MaxTokens: a.cfg.Compact.MaxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("compact LLM call: %w", err)
	}

	summary := resp.Content

	// 5. Persist the old session to memory before compaction.
	a.StoreCompactMemory()

	// 6. Create the new session with the compacted summary.
	systemPrompt := ""
	if len(messages) > 0 && messages[0].Role == "system" {
		systemPrompt = messages[0].Content
	}
	newHistory, err := FinalizeCompact(a.sessionManager, systemPrompt, summary)
	if err != nil {
		// FinalizeCompact may have created a new session (sm.New succeeded)
		// before failing. Try to clean up the orphan.
		if cur := a.sessionManager.Current(); cur != nil && cur.CompactedParentID != "" {
			_ = a.sessionManager.Delete(cur.ID) // best-effort
		}
		return nil, fmt.Errorf("finalize compact: %w", err)
	}

	// 7. Notify the memory backend that a new session has started.
	a.StartSessionMemory()

	return newHistory, nil
}
