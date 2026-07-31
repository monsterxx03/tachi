package hooks

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
)

// expandVars replaces template variables in a hook command string.
// Supported variables (defined in docs/2026-07-26-hook-system-and-herdr-integration.md §3.3):
//
//	{{HOOKS_DIR}}     — the hooks directory (<baseDir>/hooks, default ~/.tachi/hooks)
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

// hooksDir returns the hooks directory under the tachi base directory
// (config.BaseDir()/hooks). Going through BaseDir keeps hooks consistent
// with --home isolation instead of always pointing at ~/.tachi/hooks.
func hooksDir() string {
	return filepath.Join(config.BaseDir(), "hooks")
}
