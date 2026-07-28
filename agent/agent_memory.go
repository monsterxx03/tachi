package agent

import (
	"context"
	"fmt"

	"github.com/monsterxx03/tachi/agent/memory"
)

// MemoryBackend returns the configured memory backend, or nil if memory is disabled.
func (a *AIAgent) MemoryBackend() memory.Backend {
	if a.memory == nil {
		return nil
	}
	return a.memory.Backend
}

// RecordMemory implements tools.MemoryRecorder. It persists an explicit
// LLM-initiated memory to the backend's inbox (the only live write path —
// bulk memory production is the Dream pipeline's job, writing topic files
// offline). Tags are accepted for tool-API compatibility and logged; the
// TopicBackend stores content only.
// Returns an error if memory is not configured or no session is active.
func (a *AIAgent) RecordMemory(ctx context.Context, content string, tags []string) error {
	if a.memory == nil {
		return fmt.Errorf("memory backend not configured")
	}
	if a.sessionManager == nil {
		return fmt.Errorf("session manager not configured")
	}
	sess := a.sessionManager.Current()
	if sess == nil {
		return fmt.Errorf("no active session")
	}

	storeCtx, cancel := context.WithTimeout(ctx, a.cfg.Memory.Timeout)
	defer cancel()

	if err := a.memory.Backend.Store(storeCtx, content); err != nil {
		a.logger.Error(ctx, "RecordMemory: store failed", err)
		return err
	}
	a.logger.Info(ctx, "RecordMemory: stored", "content", truncateForLog(content, 60), "tags", fmt.Sprintf("%v", tags))
	return nil
}
