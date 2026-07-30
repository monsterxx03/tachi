package agent

import (
	"context"
	"fmt"
)

// Session mode identifiers.
// These define the operating mode of an AIAgent, affecting tool availability
// and (in future) system prompt and permission behavior.
const (
	// ModeAuto is the default mode with full tool access:
	// edit files, run commands, browse the web, use MCP servers, etc.
	ModeAuto = "auto"

	// ModeChat is a read-only conversation mode.
	// Destructive tools (WriteFile, EditFile, Bash, etc.) are hidden from the LLM.
	ModeChat = "chat"

	// ModePlan is a read-only planning mode.
	// Destructive tools are hidden; the LLM uses read-only tools to explore
	// the codebase and the save_plan tool to produce a structured plan document.
	ModePlan = "plan"
)

// ValidMode reports whether mode is a known session mode identifier.
func ValidMode(mode string) bool {
	switch mode {
	case ModeAuto, ModeChat, ModePlan:
		return true
	default:
		return false
	}
}

// Mode returns the agent's current session mode.
func (a *AIAgent) Mode() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.mode
}

// SetMode changes the agent's session mode. Switching modes affects which
// tools are visible to the LLM:
//   - ModeAuto:  full tool access
//   - ModeChat:  hides destructive tools at the schema filter level
//   - ModePlan:  hides destructive tools (same as ModeChat)
//
// Unlike the old savedTools approach, this does NOT mutate the tool registry.
// Mode filtering is applied every iteration in filterActiveSchemas, so no
// save/restore dance is needed — the registry stays intact and mode only
// affects what schemas the LLM sees.
//
// Returns an error if mode is unknown.
func (a *AIAgent) SetMode(mode string) error {
	if !ValidMode(mode) {
		return fmt.Errorf("unknown mode: %s (supported: %s, %s, %s)", mode, ModeAuto, ModeChat, ModePlan)
	}

	a.mu.Lock()
	if mode == a.mode {
		a.mu.Unlock()
		return nil // already in this mode
	}
	a.mode = mode
	a.mu.Unlock()

	// Best-effort persist mode to session metadata (outside the lock —
	// UpdateMeta does file I/O).
	if a.Config.SessionManager != nil {
		if curr := a.Config.SessionManager.Current(); curr != nil {
			curr.Mode = mode
			if err := a.Config.SessionManager.UpdateMeta(curr); err != nil {
				a.Config.Logger.Error(context.Background(), "Agent: failed to persist mode", err, "mode", mode)
			}
		}
	}

	return nil
}
