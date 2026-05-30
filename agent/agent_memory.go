package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// MemoryBackend returns the configured memory backend, or nil if memory is disabled.
func (a *AIAgent) MemoryBackend() memory.Backend {
	return a.memoryBackend
}

// RecordMemory implements tools.MemoryRecorder. It persists an explicit
// LLM-initiated memory to the memory backend, associated with the current
// session. Returns an error if memory is not configured or no session is active.
func (a *AIAgent) RecordMemory(ctx context.Context, content string, tags []string) error {
	if a.memoryBackend == nil {
		return fmt.Errorf("memory backend not configured")
	}
	if a.sessionManager == nil {
		return fmt.Errorf("session manager not configured")
	}
	sess := a.sessionManager.Current()
	if sess == nil {
		return fmt.Errorf("no active session")
	}

	storeCtx, cancel := context.WithTimeout(ctx, a.memoryTimeout)
	defer cancel()

	err := a.memoryBackend.Store(storeCtx, memory.StoreOptions{
		Scope:         memory.StoreScopeTurn,
		SessionID:     sess.ID,
		Tags:          withRepoTag(tags),
		DirectContent: content,
	})
	if err != nil {
		a.logger.Log("RecordMemory: store failed: %v", err)
		return err
	}
	a.logger.Log("RecordMemory: stored content=%q tags=%v", truncateForLog(content, 60), tags)
	return nil
}

// StartSessionMemory notifies the memory backend that a new session has begun.
// Called after session creation in RunConversationStream and ResumeSession.
// No-ops when memory is not configured.
func (a *AIAgent) StartSessionMemory() {
	if a.memoryBackend == nil || a.sessionManager == nil {
		return
	}
	sess := a.sessionManager.Current()
	if sess == nil {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), a.memoryTimeout)
		defer cancel()
		if err := a.memoryBackend.Store(ctx, memory.StoreOptions{
			Scope:     memory.StoreScopeStart,
			SessionID: sess.ID,
			Tags:      withRepoTag(nil),
		}); err != nil {
			a.logger.Log("Memory(start): start session failed: %v", err)
		}
	}()
}

// collectTurnMessages extracts the last user message from the conversation
// history and pairs it with the current assistant response text.
func collectTurnMessages(messages *[]llm.Message, assistantText string) []memory.Message {
	for i := len(*messages) - 1; i >= 0; i-- {
		if (*messages)[i].Role == "user" {
			return []memory.Message{
				{Role: "user", Content: (*messages)[i].Content},
				{Role: "assistant", Content: assistantText},
			}
		}
	}
	return nil
}

// withRepoTag appends a "project:<name>" tag to the given tag slice when
// the current working directory is inside a git repository. Returns the
// original slice unchanged otherwise.
func withRepoTag(tags []string) []string {
	if tag := repoTag(); tag != "" {
		return append(tags, tag)
	}
	return tags
}

// normalizeRepoPaths expands ~ to the home directory and cleans each path.
// This way users can write ~/repos/tachi in config and it will match
// the absolute path returned by git rev-parse --show-toplevel.
func normalizeRepoPaths(paths []string) []string {
	if len(paths) == 0 {
		return paths
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return paths
	}
	normalized := make([]string, 0, len(paths))
	for _, p := range paths {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "~/") {
			s = filepath.Join(home, s[2:])
		} else if s == "~" {
			s = home
		}
		normalized = append(normalized, filepath.Clean(s))
	}
	return normalized
}

// getRepoName returns the name of the current git repository (the basename
// of the repo root, e.g. "tachi"). Returns empty string if not in a git repo.
func getRepoName() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(out)))
}

// repoTag returns a tag in the form "project:<name>" when inside a git repo,
// or empty string otherwise.
func repoTag() string {
	if name := getRepoName(); name != "" {
		return "project:" + name
	}
	return ""
}

// isRepoExcluded checks whether the current git repo root is in the
// exclude_repos list. If we're not in a git repo, returns false.
func (a *AIAgent) isRepoExcluded() bool {
	if len(a.excludeRepos) == 0 {
		return false
	}
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return false
	}
	repoRoot := strings.TrimSpace(string(out))
	return slices.ContainsFunc(a.excludeRepos, func(excluded string) bool {
		return filepath.Clean(repoRoot) == filepath.Clean(excluded)
	})
}

// storeTurnMemory writes the current turn's conversation to the memory backend.
// Called after each assistant response completes (StoreScopeTurn).
// No-ops when skipMemory is true (e.g. /commit, /init, sub-agents).
func (a *AIAgent) storeTurnMemory(turnMsgs []memory.Message) {
	if len(turnMsgs) == 0 {
		return
	}
	if a.memoryBackend == nil || a.sessionManager == nil {
		return
	}
	if a.skipMemory {
		return
	}
	if a.isRepoExcluded() {
		return
	}
	sess := a.sessionManager.Current()
	if sess == nil {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), a.memoryTimeout)
		defer cancel()
		if err := a.memoryBackend.Store(ctx, memory.StoreOptions{
			Scope:        memory.StoreScopeTurn,
			SessionID:    sess.ID,
			Tags:         withRepoTag(nil),
			TurnMessages: turnMsgs,
		}); err != nil {
			a.logger.Log("Memory(turn): store failed: %v", err)
		}
	}()
}

// StoreCompactMemory writes the current session's messages to the memory
// backend before context compaction (StoreScopeCompact).
// Exported so the TUI can call it before starting the compact LLM stream.
func (a *AIAgent) StoreCompactMemory() {
	if a.memoryBackend == nil || a.sessionManager == nil {
		return
	}
	if a.isRepoExcluded() {
		return
	}
	sess := a.sessionManager.Current()
	if sess == nil {
		return
	}

	msgs, err := a.sessionManager.LoadMessages()
	if err != nil {
		a.logger.Log("Memory(compact): load messages failed: %v", err)
		return
	}

	memMsgs := sessionMessagesToMemory(msgs)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), a.memoryTimeout)
		defer cancel()
		if err := a.memoryBackend.Store(ctx, memory.StoreOptions{
			Scope:           memory.StoreScopeCompact,
			SessionID:       sess.ID,
			Tags:            withRepoTag(nil),
			SessionMessages: memMsgs,
		}); err != nil {
			a.logger.Log("Memory(compact): store failed: %v", err)
		}
	}()
}

// StoreSessionMemory writes a session summary to the memory backend.
// Called at session end or shutdown (StoreScopeSession).
// Exported so the TUI can call it before ending/quitting.
// Uses a unified interface — each backend handles its own format internally.
func (a *AIAgent) StoreSessionMemory() {
	if a.memoryBackend == nil || a.sessionManager == nil {
		return
	}
	if a.isRepoExcluded() {
		return
	}
	sess := a.sessionManager.Current()
	if sess == nil || sess.Title == "" {
		return
	}

	msgs, err := a.sessionManager.LoadMessages()
	if err != nil {
		a.logger.Log("Memory(session): load messages failed: %v", err)
		// Still try to write with just the title
	}

	memMsgs := sessionMessagesToMemory(msgs)

	ctx, cancel := context.WithTimeout(context.Background(), a.memoryTimeout)
	defer cancel()
	if err := a.memoryBackend.Store(ctx, memory.StoreOptions{
		Scope:           memory.StoreScopeSession,
		SessionID:       sess.ID,
		SessionTitle:    sess.Title,
		Tags:            withRepoTag(nil),
		SessionMessages: memMsgs,
	}); err != nil {
		a.logger.Log("Memory(session): store failed: %v", err)
	}
}

// sessionMessagesToMemory converts session.Message slice to memory.Message slice.
func sessionMessagesToMemory(msgs []session.Message) []memory.Message {
	result := make([]memory.Message, 0, len(msgs))
	for _, m := range msgs {
		// Only include user and assistant messages
		if m.Type != session.MessageTypeUser && m.Type != session.MessageTypeAssistant {
			continue
		}
		role := "user"
		if m.Type == session.MessageTypeAssistant {
			role = "assistant"
		}
		result = append(result, memory.Message{
			Role:    role,
			Content: m.Content,
		})
	}
	return result
}
