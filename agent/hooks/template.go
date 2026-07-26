package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// expandVars replaces template variables in a hook command string.
// Supported variables (defined in docs/2026-07-26-hook-system-and-herdr-integration.md §3.3):
//
//	{{HOOKS_DIR}}     — the hooks directory (~/.tachi/hooks/)
//	{{SESSION_ID}}    — current session ID
//	{{WORKSPACE_DIR}} — current workspace directory
//	{{TIMESTAMP}}     — current ISO 8601 timestamp
//
// Variables that are empty or unavailable are replaced with an empty string.
func expandVars(cmd string, sessionID, workspaceDir string) string {
	cmd = strings.ReplaceAll(cmd, "{{HOOKS_DIR}}", hooksDir())
	cmd = strings.ReplaceAll(cmd, "{{SESSION_ID}}", sessionID)
	cmd = strings.ReplaceAll(cmd, "{{WORKSPACE_DIR}}", workspaceDir)
	cmd = strings.ReplaceAll(cmd, "{{TIMESTAMP}}", time.Now().Format(time.RFC3339))
	return cmd
}

// hooksDir returns the default hooks directory (~/.tachi/hooks/), falling
// back to "~/.tachi/hooks" (literal) when the home directory cannot be
// determined.
func hooksDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/.tachi/hooks"
	}
	return filepath.Join(home, ".tachi", "hooks")
}
