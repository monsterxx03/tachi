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
	compactCtx, cancel := context.WithTimeout(context.Background(), a.Config.FullConfig.Compact.Timeout)
	defer cancel()

	// Forward conversation cancellation — user Ctrl+C still aborts compact.
	//
	// The channel is captured BY VALUE, not through the variable: compactCtx is reassigned
	// below (to carry the session id), and a closure reading that variable races with the
	// write — the race detector flags it, and a torn read of a context interface could hand
	// the goroutine a context nobody cancels. Snapshotted here, the goroutine only ever sees
	// the channel it is waiting on.
	compactDone := compactCtx.Done()
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-compactDone:
		}
	}()

	// 2. Generate summary via the compact strategy.
	// Usage billing: anchor the compact call to the current session so the
	// summary cost lands in the right session's ledger rows (the strategy
	// itself tags the kind).
	if sid := a.sessionID(); sid != "" {
		compactCtx = llm.WithSessionID(compactCtx, sid)
	}
	summary, err = a.Config.CompactStrategy.Compact(compactCtx, messages, a.Config.FullConfig.Compact.MaxTokens)
	if err != nil {
		return "", nil, fmt.Errorf("compact LLM call: %w", err)
	}

	// 3. Persist the old session to memory, create the new compacted session,
	// and notify memory that a new session started.
	systemPrompt := ""
	if len(messages) > 0 && messages[0].Role == "system" {
		systemPrompt = messages[0].Content
	}
	newHistory, err = a.CompleteCompact(a.Config.SessionManager, systemPrompt, summary)
	if err != nil {
		return "", nil, fmt.Errorf("finalize compact: %w", err)
	}

	return summary, newHistory, nil
}

// SetCompactStrategy replaces the strategy used for auto-compact summary
// generation. Tests use this to inject a fake that returns a fixed summary
// without needing a real LLM provider.
//
// A strategy installed this way is OWNED by the caller: rebindCompactStrategy
// leaves it alone on a provider switch (see there).
func (a *AIAgent) SetCompactStrategy(s CompactStrategy) {
	a.Config.CompactStrategy = s
}

// rebindCompactStrategy re-points the PRODUCTION compact strategy at the
// agent's current main provider.
//
// The strategy is built at construction — and its provider is read nowhere
// else — while SetResolvedProvider replaces a.Config.Resolved wholesale. So
// without this call, /model would switch the conversation to the new provider
// and leave /compact (plus auto-compact) asking the OLD one for the summary:
// the request would go to a model the user just left, and the usage row would
// name it as well. Context window and thinking follow SetResolvedProvider
// already; the compact path was the one dimension that did not.
//
// A strategy the caller OWNS is deliberately untouched: tests inject
// fakeCompactStrategy via SetCompactStrategy precisely to keep the LLM out of
// the loop, and swapping it for the production implementation would turn every
// such test into a real network call. The concrete-type check is what tells
// the two apart — llmCompactStrategy is the one this package builds itself and
// no caller can name.
func (a *AIAgent) rebindCompactStrategy() {
	if _, ok := a.Config.CompactStrategy.(*llmCompactStrategy); !ok {
		return
	}
	a.Config.CompactStrategy = &llmCompactStrategy{provider: a.Config.Resolved.Provider}
}
