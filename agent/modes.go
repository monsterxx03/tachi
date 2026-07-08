package agent

import (
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
)

// ValidMode reports whether mode is a known session mode identifier.
func ValidMode(mode string) bool {
	switch mode {
	case ModeAuto, ModeChat:
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
//
// Returns an error if mode is unknown.
func (a *AIAgent) SetMode(mode string) error {
	if mode == a.mode {
		return nil // already in this mode
	}
	if !ValidMode(mode) {
		return fmt.Errorf("unknown mode: %s (supported: %s, %s)", mode, ModeAuto, ModeChat)
	}

	switch mode {
	case ModeAuto:
		// Restore destructive tools that were saved when entering chat mode.
		for name, tool := range a.savedTools {
			if a.toolRegistry.GetTool(name) == nil {
				a.toolRegistry.Register(tool)
				a.logger.Log("Agent: restored tool %s for auto mode", name)
			}
		}
		a.savedTools = make(map[string]tools.Tool)

	case ModeChat:
		// Save and remove destructive tools.
		for _, name := range a.toolRegistry.GetToolNames() {
			tool := a.toolRegistry.GetTool(name)
			if tool == nil {
				continue
			}
			if dd, ok := tool.(tools.DestructiveDetector); ok && dd.IsDestructive() {
				a.savedTools[name] = tool
				a.toolRegistry.Unregister(name)
				a.logger.Log("Agent: removed tool %s for chat mode", name)
			}
		}
	}

	a.mode = mode
	return nil
}
