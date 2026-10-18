package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// sessionSwitchNotifier is the optional half of a SessionManager: a manager that can tell its
// host the conversation moved. Only a manager that somebody installed a hook on does anything
// with it (session.Manager.NotifySessionSwitch), so the ordinary frontends are unaffected —
// but the type assertion stays here rather than on the interface so that a test double does
// not have to implement a notification it will never receive.
type sessionSwitchNotifier interface {
	NotifySessionSwitch(old, next *session.Session)
}

// CompleteCompact finalizes a compaction: creates the new compacted session
// and returns the new conversation history ready for RunConversationStream.
//
// sm is a parameter rather than a.sessionManager because channel mode finalizes
// through its own session.Manager built from the thread ID. When sm is nil the
// agent's own session manager is used.
//
// On failure it best-effort deletes the orphaned child session, mirroring the
// cleanup that the auto-compact path performed inline.
func (a *AIAgent) CompleteCompact(sm SessionManager, systemPrompt, summary string) ([]llm.Message, error) {
	if sm == nil {
		sm = a.Config.SessionManager
	}
	if sm == nil {
		return nil, fmt.Errorf("no session manager available to finalize compact")
	}

	newHistory, err := FinalizeCompact(sm, systemPrompt, summary)
	if err != nil {
		// FinalizeCompact may have created a new session (sm.New succeeded)
		// before failing. Try to clean up the orphan.
		if cur := sm.Current(); cur != nil && cur.CompactedParentID != "" {
			_ = sm.Delete(cur.ID) // best-effort
		}
		return nil, err
	}

	// Recompute the token estimate (and its breakdown) for the history the
	// conversation now has. Without this the estimate still describes the history
	// that was just summarised, and nothing would correct it until the next API
	// call — which a MANUAL /compact never makes, so every frontend (context
	// ring, context popover, token warnings) would keep showing the pre-compaction
	// numbers. Passing nil for the run state is the documented way to prime the
	// estimate outside a turn.
	a.EstimateAndUpdateTokens(nil, newHistory)

	return newHistory, nil
}

// FinalizeCompact creates a new session containing the compacted summary,
// links it bidirectionally to the old session, and returns the new conversation
// history (with system prompt prepended) ready for RunConversationStream.
//
// The old session's meta is updated with compacted_child_id.
// The thread binding (ThreadID) is migrated here: the new session inherits
// the old session's ThreadID and the old session releases it. Without this,
// FindByThreadID (channel mode's session lookup) would keep resolving to the
// pre-compact session and reload its full history on every turn — causing
// repeated auto-compaction right after a compaction reset the context. This
// applies to every entry point (TUI, ACP, channel), including the auto-compact
// path inside the agent loop, where no caller can perform the migration.
//
// Prefer AIAgent.CompleteCompact, which additionally cleans up orphaned
// sessions on failure; call this directly only when there is no agent.
//
// Parameters:
//   - sm: session manager with the OLD session loaded as current
//   - systemPrompt: prepended as the first message in the returned history
//   - summary: the LLM-generated summary text
//
// Returns:
//   - newHistory: []llm.Message ready for RunConversationStream
//   - err: any error during session creation or persistence
func FinalizeCompact(sm SessionManager, systemPrompt string, summary string) ([]llm.Message, error) {
	oldSess := sm.Current()
	if oldSess == nil {
		return nil, fmt.Errorf("no active session to compact")
	}

	// 1. Create new session (becomes current)
	newSess, err := sm.New(oldSess.ProviderName, oldSess.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("create compact session: %w", err)
	}

	// 2. Write compact messages to the new session
	now := time.Now()
	if err := sm.AppendMessage(&session.Message{
		Type:      session.MessageTypeAssistant,
		Timestamp: now,
		Content:   fmt.Sprintf("历史摘要：\n\n%s\n\n---\n以上是之前对话的压缩摘要。", summary),
	}); err != nil {
		return nil, fmt.Errorf("append summary message: %w", err)
	}
	if err := sm.AppendMessage(&session.Message{
		Type:      session.MessageTypeUser,
		Timestamp: now,
		Content:   "请基于以上摘要继续对话。",
	}); err != nil {
		return nil, fmt.Errorf("append continue message: %w", err)
	}

	// 3. Update new session meta
	newSess.Title = oldSess.Title
	newSess.CompactedParentID = oldSess.ID
	newSess.CompactedParentTitle = oldSess.Title
	newSess.ThreadID = oldSess.ThreadID // migrate the thread binding, if any
	// The workspace travels with the conversation. sm.New carries only the provider
	// and the PRIMARY directory, so every other workspace field has to be copied here
	// by hand: a field that is not listed in this block is silently zeroed, not
	// reported. Dropping AdditionalDirs shrinks the compacted session to one tree
	// while the tools keep writing into the others — a root set the session no longer
	// records (and a checkpoint manager that no longer covers them).
	newSess.AdditionalDirs = append([]string(nil), oldSess.AdditionalDirs...)
	// ProjectID is the same trap with a wider blast radius: the desktop resolves a
	// member's roots from the PROJECT, and WorkingDir/AdditionalDirs above are only
	// the snapshot taken when the session was created. Dropping the id would leave the
	// child reading that stale snapshot as its own roots — tools, prompt and checkpoints
	// agreeing with each other in the WRONG tree (the failure RootsFunc exists to prevent),
	// plus a sidebar group and write guards the conversation silently left.
	newSess.ProjectID = oldSess.ProjectID
	// The pin travels with the conversation, and it MOVES rather than being copied: the
	// sidebar's row for this conversation is the child now (the parent folds under it as
	// 「压缩前」), so a pin left on the parent would be invisible state that also keeps an
	// ancestor out of session cleanup forever. Same shape as ThreadID below — one binding,
	// released from the record it no longer means anything on.
	newSess.Pinned = oldSess.Pinned
	// The cron-protection flag is a property of the CONVERSATION, and the child is
	// the row a cleanup would consider (the parent folds away). Move it, exactly
	// like the pin: a copy left on the parent would keep a hidden ancestor alive
	// forever, while the child — the target a job resolves to — would be the one
	// that could be deleted.
	newSess.CronBound = oldSess.CronBound
	// Same trap, worse consequence: a field left out of this block is silently zeroed, and for
	// these two that means a compacted remote execution comes back looking like an ordinary
	// local conversation — drivable from a frontend, and folded into the user's long-term
	// memory by the next Dream pass (design §2.2, §2.3).
	newSess.Kind = oldSess.Kind
	newSess.SkipDream = oldSess.SkipDream
	// SkipApproval is deliberately NOT copied, and that is the one entry in this block that is
	// here to say so: it is the parent's answer about the asks in the conversation the reader was
	// sitting in, and the child is a different conversation that nobody was asked about. The
	// desktop re-points the live agent at the child's record when it re-homes the run
	// (followCompaction) — without that, the agent would keep approving what the child's record
	// says must be asked about.
	// The source node travels too, and unlike Pinned this is a COPY rather than a move: the
	// parent is still a remote execution record and stays one (it is readable, and a reader
	// has to be able to tell whose work it was), while the child is the row the conversation
	// continues in. Left out of this block it is silently zeroed on the child — and the child
	// is what a list shows.
	newSess.SourceNodeID = oldSess.SourceNodeID
	// The logical conversation travels with the conversation too, and for the same reason: a
	// delegated task's record and a reminder's idea of "this conversation" are filed under it,
	// and a compaction must not make them look like another conversation's work. Written from
	// the OLD session's identity — including the fallback, which is what makes this work for a
	// session created before the field existed.
	newSess.LogicalID = oldSess.LogicalConversationID()
	if err := sm.UpdateMeta(newSess); err != nil {
		return nil, fmt.Errorf("update new session meta: %w", err)
	}

	// The conversation just MOVED, and a host that keeps state keyed by session id has to hear
	// it here rather than figure it out later. A receiving node protects its sessions with
	// persisted holds and remembers a context's session by id: the child is unprotected and
	// already being written to at this moment, so "the host will notice at the next task" is a
	// window in which a cleanup in another process deletes the session a running turn is using
	// (design §9.4). The notification happens BEFORE the old session's own bookkeeping is
	// updated below, which is the order the design fixes: protect the new one, persist the new
	// binding, then release what the old one no longer needs.
	if notifier, ok := sm.(sessionSwitchNotifier); ok {
		notifier.NotifySessionSwitch(oldSess, newSess)
	}

	// 4. Update old session meta
	oldSess.CompactedChildID = newSess.ID
	// Release the thread binding so FindByThreadID resolves to the new
	// session (it is newer, and channel mode must not reload the
	// pre-compact history). Also lets session cleanup reclaim the old
	// session instead of pinning every compacted ancestor forever.
	oldSess.ThreadID = ""
	// The pin moved to the child above; clear it here so exactly one record of this
	// conversation is pinned. Left alone it would be invisible state on a folded-away row
	// AND a permanent exemption from session cleanup for an ancestor nobody can see.
	oldSess.Pinned = false
	// The cron binding moved to the child above; clear it here so the cleanup
	// does not keep an ancestor nobody can see. The job still resolves the
	// conversation through the child's logical id.
	oldSess.CronBound = false
	if err := sm.UpdateMeta(oldSess); err != nil {
		// Non-fatal: log but continue. The new session is already created.
		// The old session's compacted_child_id is missing, but the new
		// session's compacted_parent_id is correct — one-way link still works.
		_ = err // best-effort
	}

	// 5. Build and return history
	return buildCompactHistory(systemPrompt, summary), nil
}

// buildCompactHistory constructs the llm.Message history for the compacted session.
// It includes the system prompt (always present), the summary as an assistant
// message, and a "continue" user message.
//
// This history is non-empty, so RunConversationStream will NOT inject the system
// prompt automatically — the system prompt must be explicitly included here.
func buildCompactHistory(systemPrompt, summary string) []llm.Message {
	return []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "assistant", Content: fmt.Sprintf("历史摘要：\n\n%s\n\n---\n以上是之前对话的压缩摘要。", summary)},
		{Role: "user", Content: "请基于以上摘要继续对话。"},
	}
}

// DrainCompactEvents collects the final assistant response from a RunOneOffStream
// event channel and returns the full response text. Non-text events (tool calls,
// thinking, etc.) are consumed but ignored. Returns an error if the channel
// ends without a complete turn or with an error.
func DrainCompactEvents(ch <-chan AgentEvent) (string, error) {
	var text strings.Builder
	var lastErr error

	for event := range ch {
		switch event.Type {
		case AgentEventTextDelta:
			text.WriteString(event.TextDelta)
		case AgentEventThinkingDelta:
			// Consume but ignore
		case AgentEventToolConfirmation:
			// Should not happen (compact agent doesn't have EditFile),
			// but consume gracefully — just ignore (relevant channel handle)
		case AgentEventAskUser:
			// Should not happen (AskUser unregistered), but consume gracefully
		case AgentEventToolCallStart, AgentEventToolCallArgs, AgentEventToolResult:
			// Consume tool events silently
		case AgentEventTurnComplete:
			if event.Result != nil {
				if event.Result.Response != "" {
					text.Reset()
					text.WriteString(event.Result.Response)
				}
				if event.Result.Error != nil {
					lastErr = event.Result.Error
				}
			}
		case AgentEventError:
			if event.Result != nil {
				if event.Result.Response != "" {
					text.Reset()
					text.WriteString(event.Result.Response)
				}
				if event.Result.Error != nil {
					lastErr = event.Result.Error
				}
			}
		}
	}

	result := strings.TrimSpace(text.String())
	if result == "" && lastErr != nil {
		return "", lastErr
	}
	return result, nil
}
