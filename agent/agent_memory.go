package agent

import (
	"context"
	"fmt"

	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// MemoryBackend returns the configured memory backend, or nil if memory is disabled.
func (a *AIAgent) MemoryBackend() memory.Backend {
	if a.Config.Memory == nil {
		return nil
	}
	return a.Config.Memory.Backend
}

// RecordMemory implements tools.MemoryRecorder. It persists an explicit
// LLM-initiated memory to the backend's inbox (the only live write path —
// bulk memory production is the Dream pipeline's job, writing topic files
// offline). Tags are accepted for tool-API compatibility and logged; the
// TopicBackend stores content only.
// Returns an error if memory is not configured or no session is active.
func (a *AIAgent) RecordMemory(ctx context.Context, content string, tags []string) error {
	if a.Config.Memory == nil {
		return fmt.Errorf("memory backend not configured")
	}
	if a.Config.SessionManager == nil {
		return fmt.Errorf("session manager not configured")
	}
	sess := a.Config.SessionManager.Current()
	if sess == nil {
		return fmt.Errorf("no active session")
	}

	storeCtx, cancel := context.WithTimeout(ctx, a.Config.FullConfig.Memory.Timeout)
	defer cancel()

	if err := a.Config.Memory.Backend.Store(storeCtx, content); err != nil {
		a.Config.Logger.Error(ctx, "RecordMemory: store failed", err)
		return err
	}
	a.Config.Logger.Info(ctx, "RecordMemory: stored", "content", strutil.Truncate(content, 60), "tags", fmt.Sprintf("%v", tags))
	return nil
}
