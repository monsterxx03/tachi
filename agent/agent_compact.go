package agent

import (
	"context"
	"fmt"

	"github.com/monsterxx03/tachi/llm"
)

// doCompact performs a synchronous session compaction:
//  1. Sends the full conversation history + compact instruction to the LLM
//     via provider.CreateChat (non-streaming, no agent loop).
//  2. Stores the old session's memory.
//  3. Creates a new session with the compacted summary via FinalizeCompact.
//  4. Notifies the memory backend of the new session.
//
// It returns the LLM-generated summary, the new (compacted) conversation
// history, and any error encountered.
//
// It uses context.Background() with the configured timeout so that a
// conversation interruption (ctx cancellation) does not produce orphan
// sessions. The conversation's cancellation signal is forwarded to the
// compact context so that user-initiated aborts are still respected.
//
// doCompact does NOT clear or restore the tool registry — it calls
// CreateChat directly with tools=nil, so no registry state is affected.
func (a *AIAgent) doCompact(ctx context.Context, messages []llm.Message) (summary string, newHistory []llm.Message, err error) {
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

	// 2. Generate summary via the compact strategy.
	summary, err = a.compactStrategy.Compact(compactCtx, messages, a.cfg.Compact.MaxTokens)
	if err != nil {
		return "", nil, fmt.Errorf("compact LLM call: %w", err)
	}

	// 3. Persist the old session to memory, create the new compacted session,
	// and notify memory that a new session started.
	systemPrompt := ""
	if len(messages) > 0 && messages[0].Role == "system" {
		systemPrompt = messages[0].Content
	}
	newHistory, err = a.CompleteCompact(a.sessionManager, systemPrompt, summary)
	if err != nil {
		return "", nil, fmt.Errorf("finalize compact: %w", err)
	}

	return summary, newHistory, nil
}

// SetCompactStrategy replaces the strategy used for auto-compact summary
// generation. Tests use this to inject a fake that returns a fixed summary
// without needing a real LLM provider.
func (a *AIAgent) SetCompactStrategy(s CompactStrategy) {
	a.compactStrategy = s
}
