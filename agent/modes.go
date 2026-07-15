package agent

import (
	"context"
	"fmt"

	"github.com/monsterxx03/tachi/agent/tools"
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
	return a.mode
}

// SetMode changes the agent's session mode. Switching modes affects which
// tools are visible to the LLM:
//   - ModeAuto:  full tool access (restores any tools previously hidden)
//   - ModeChat:  hides destructive tools (WriteFile, EditFile, Bash, etc.)
//   - ModePlan:  hides destructive tools (same as ModeChat)
//
// Returns an error if mode is unknown.
func (a *AIAgent) SetMode(mode string) error {
	if mode == a.mode {
		return nil // already in this mode
	}
	if !ValidMode(mode) {
		return fmt.Errorf("unknown mode: %s (supported: %s, %s, %s)", mode, ModeAuto, ModeChat, ModePlan)
	}

	switch mode {
	case ModeAuto:
		// Restore destructive tools that were saved when entering chat/plan mode.
		for name, tool := range a.savedTools {
			if a.toolRegistry.GetTool(name) == nil {
				a.toolRegistry.Register(tool)
				a.logger.Logf(context.Background(), "Agent: restored tool %s for auto mode", name)
			}
		}
		a.savedTools = make(map[string]tools.Tool)

	case ModeChat, ModePlan:
		// Save and remove destructive tools.
		for _, name := range a.toolRegistry.GetToolNames() {
			tool := a.toolRegistry.GetTool(name)
			if tool == nil {
				continue
			}
			if dd, ok := tool.(tools.DestructiveDetector); ok && dd.IsDestructive() {
				a.savedTools[name] = tool
				a.toolRegistry.Unregister(name)
				a.logger.Logf(context.Background(), "Agent: removed tool %s for %s mode", name, mode)
			}
		}
	}

	a.mode = mode

	// Best-effort persist mode to session metadata.
	if a.sessionManager != nil {
		if curr := a.sessionManager.Current(); curr != nil {
			curr.Mode = mode
			if err := a.sessionManager.UpdateMeta(curr); err != nil {
				a.logger.Logf(context.Background(), "Agent: failed to persist mode %s: %v", mode, err)
			}
		}
	}

	return nil
}
