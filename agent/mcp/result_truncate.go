package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/debuglog"
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
func truncateToolOutput(result string, maxChars int, fileDir string, toolName string) string {
	if maxChars <= 0 || len(result) <= maxChars {
		return result
	}

	// Sanitize tool name for use in filename.
	safeName := sanitizeForFilename(toolName)
	filename := fmt.Sprintf("%s_%d.txt", safeName, time.Now().UnixNano())
	filepath := filepath.Join(fileDir, filename)

	// Ensure the directory exists.
	if err := os.MkdirAll(fileDir, 0700); err != nil {
		debuglog.DefaultLogger.Log("MCP: truncateToolOutput: failed to create dir %s: %v", fileDir, err)
		// Fall back to simple truncation without file persistence.
		return hardTruncate(result, maxChars, toolName)
	}

	// Write the full result to disk.
	if err := os.WriteFile(filepath, []byte(result), 0600); err != nil {
		debuglog.DefaultLogger.Log("MCP: truncateToolOutput: failed to write file %s: %v", filepath, err)
		// Fall back to simple truncation.
		return hardTruncate(result, maxChars, toolName)
	}

	debuglog.DefaultLogger.Log("MCP: tool %s result too large (%d chars), saved to %s", toolName, len(result), filepath)

	// Best-effort background cleanup of old files.
	go cleanupOldToolResults(fileDir, defaultToolResultMaxAge)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(
		"[OUTPUT TOO LARGE] Full output (%d chars) exceeds limit (%d chars).\n",
		len(result), maxChars,
	))
	sb.WriteString(fmt.Sprintf(
		"Use ReadFile to read the full output from:\n  %s",
		filepath,
	))
	return sb.String()
}

// hardTruncate performs a simple truncation at maxChars without file persistence.
// Used as fallback when file I/O fails.
func hardTruncate(result string, maxChars int, toolName string) string {
	truncated := result[:maxChars]
	return fmt.Sprintf(
		"[OUTPUT TRUNCATED at %d chars]\n%s\n...\n[... %d chars truncated. "+
			"Use pagination or filtering on the MCP server if available.]",
		maxChars, truncated, len(result)-maxChars,
	)
}

// sanitizeForFilename replaces characters that are problematic in filenames.
func sanitizeForFilename(name string) string {
	// Replace MCP prefix separator and other special chars.
	r := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
		" ", "_",
	)
	return r.Replace(name)
}

// cleanupOldToolResults removes tool result files older than maxAge from fileDir.
// Errors are logged but not returned — cleanup is best-effort.
func cleanupOldToolResults(fileDir string, maxAge time.Duration) {
	entries, err := os.ReadDir(fileDir)
	if err != nil {
		if !os.IsNotExist(err) {
			debuglog.DefaultLogger.Log("MCP: cleanupOldToolResults: read dir %s: %v", fileDir, err)
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
				debuglog.DefaultLogger.Log("MCP: cleanupOldToolResults: remove %s: %v", path, err)
			} else {
				removed++
			}
		}
	}

	if removed > 0 {
		debuglog.DefaultLogger.Log("MCP: cleanupOldToolResults: removed %d old files from %s (maxAge=%s)", removed, fileDir, maxAge)
	}
}
