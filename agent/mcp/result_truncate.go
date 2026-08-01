package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

const (
	defaultToolResultMaxAge = 24 * time.Hour
)

// truncateToolOutput checks if a tool result exceeds maxChars and, if so,
// saves the full output to disk and returns a compact message with the file
// path so the LLM can read more via ReadFile — no preview content is included
// to avoid context bloat.
//
// When maxChars <= 0, the result is returned unchanged (no limit).
func (m *Manager) truncateToolOutput(ctx context.Context, result string, maxChars int, fileDir string, toolName string) string {
	if maxChars <= 0 || len(result) <= maxChars {
		return result
	}

	// Sanitize tool name for use in filename.
	safeName := sanitizeForFilename(toolName)
	filename := fmt.Sprintf("%s_%d.txt", safeName, time.Now().UnixNano())
	filepath := filepath.Join(fileDir, filename)

	// Write the full result to disk.
	if err := fileutil.WriteFilePrivate(filepath, []byte(result)); err != nil {
		m.logger.Error(ctx, "MCP: truncateToolOutput: failed to write file", err, "path", filepath)
		// Fall back to simple truncation.
		return hardTruncate(result, maxChars, toolName)
	}

	m.logger.Info(ctx, "MCP: tool result too large, saved to file", "tool", toolName, "char_count", len(result), "path", filepath)

	// Best-effort background cleanup of old files.
	go m.cleanupOldToolResults(ctx, fileDir, defaultToolResultMaxAge)

	var sb strings.Builder
	fmt.Fprintf(&sb, "[OUTPUT TOO LARGE] Full output (%d chars) exceeds limit (%d chars).\n",
		len(result), maxChars)
	fmt.Fprintf(&sb, "Use ReadFile to read the full output from:\n  %s",
		filepath)
	return sb.String()
}

// hardTruncate performs a simple truncation at maxChars without file persistence.
// Used as fallback when file I/O fails.
func hardTruncate(result string, maxChars int, _ string) string {
	truncated := result[:maxChars]
	return fmt.Sprintf(
		"[OUTPUT TRUNCATED at %d chars]\n%s\n...\n[... %d chars truncated. "+
			"Use pagination or filtering on the MCP server if available.]",
		maxChars, truncated, len(result)-maxChars,
	)
}

// sanitizeForFilename replaces characters that are problematic in filenames.
func sanitizeForFilename(name string) string {
	return strutil.SanitizeFilename(name, 0)
}

// cleanupOldToolResults removes tool result files older than maxAge from fileDir.
// Errors are logged but not returned — cleanup is best-effort.
func (m *Manager) cleanupOldToolResults(ctx context.Context, fileDir string, maxAge time.Duration) {
	entries, err := os.ReadDir(fileDir)
	if err != nil {
		if !os.IsNotExist(err) {
			m.logger.Error(ctx, "MCP: cleanupOldToolResults: read dir", err, "dir", fileDir)
		}
		return
	}

	cutoff := time.Now().Add(-maxAge)
	removed := 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(fileDir, entry.Name())
			if err := os.Remove(path); err != nil {
				m.logger.Error(ctx, "MCP: cleanupOldToolResults: remove", err, "path", path)
			} else {
				removed++
			}
		}
	}

	if removed > 0 {
		m.logger.Info(ctx, "MCP: cleanupOldToolResults: removed old files", "count", removed, "dir", fileDir, "max_age", maxAge)
	}
}
