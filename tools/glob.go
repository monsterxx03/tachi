package tools

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GlobResult is the result of the Glob tool
type GlobResult struct {
	DurationMs int64    `json:"durationMs"`
	NumFiles   int      `json:"numFiles"`
	Filenames  []string `json:"filenames"`
	Truncated  bool     `json:"truncated"`
}

// extractGlobBaseDirectory extracts the static base directory from a glob pattern.
// The base directory is everything before the first glob special character (* ? [ {).
// Returns the directory portion and the remaining relative pattern.
func extractGlobBaseDirectory(pattern string) (baseDir string, relativePattern string) {
	// Find the first glob special character: *, ?, [, {
	globChars := []string{"*", "?", "[", "{"}
	firstGlobIndex := -1

	for _, char := range globChars {
		idx := strings.Index(pattern, char)
		if idx != -1 && (firstGlobIndex == -1 || idx < firstGlobIndex) {
			firstGlobIndex = idx
		}
	}

	if firstGlobIndex == -1 {
		// No glob characters - this is a literal path
		dir := filepath.Dir(pattern)
		file := filepath.Base(pattern)
		return dir, file
	}

	// Get everything before the first glob character
	staticPrefix := pattern[:firstGlobIndex]

	// Find the last path separator in the static prefix
	lastSepIndex := strings.LastIndex(staticPrefix, "/")
	if lastSepIndex == -1 {
		lastSepIndex = strings.LastIndex(staticPrefix, string(filepath.Separator))
	}

	if lastSepIndex == -1 {
		// No path separator before the glob - pattern is relative to cwd
		return "", pattern
	}

	baseDir = staticPrefix[:lastSepIndex]
	relativePattern = pattern[lastSepIndex+1:]

	return baseDir, relativePattern
}

// GlobFile is the Glob tool implementation using ripgrep
func GlobFile(args string) (string, error) {
	// Check if ripgrep is available
	if _, err := exec.LookPath("rg"); err != nil {
		return "", fmt.Errorf("ripgrep (rg) not found in PATH: %w", err)
	}

	var argsMap struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &argsMap); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if argsMap.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}

	// Default to current working directory
	searchDir := argsMap.Path
	if searchDir == "" {
		searchDir = "."
	}

	// Resolve to absolute path for ripgrep's search directory
	absSearchDir, err := filepath.Abs(searchDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path: %w", err)
	}

	var searchPattern string
	searchBaseDir := absSearchDir

	// Handle absolute paths by extracting the base directory and converting to relative pattern
	// ripgrep's --glob flag only works with relative patterns
	if filepath.IsAbs(argsMap.Pattern) {
		baseDir, relativePattern := extractGlobBaseDirectory(argsMap.Pattern)
		if baseDir != "" {
			searchBaseDir = baseDir
			searchPattern = relativePattern
		} else {
			searchPattern = argsMap.Pattern
		}
	} else {
		searchPattern = argsMap.Pattern
	}

	// Build ripgrep arguments
	rgArgs := []string{
		"--files",
		"--glob", searchPattern,
		"--sort=modified",
		"--no-ignore",
		"--hidden",
	}

	start := time.Now()
	cmd := exec.Command("rg", rgArgs...)
	cmd.Dir = searchBaseDir
	output, err := cmd.Output()
	if err != nil {
		// If no files found, return empty result
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return marshalGlobResult(GlobResult{
				DurationMs: time.Since(start).Milliseconds(),
				NumFiles:   0,
				Filenames:  []string{},
				Truncated:  false,
			})
		}
		return "", fmt.Errorf("ripgrep failed: %w", err)
	}
	duration := time.Since(start).Milliseconds()

	// Parse output - ripgrep returns relative paths from searchBaseDir
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	filenames := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Convert to relative path from original searchDir (absSearchDir)
		relPath, err := filepath.Rel(absSearchDir, filepath.Join(searchBaseDir, line))
		if err != nil {
			relPath = filepath.Join(searchBaseDir, line)
		}
		// Normalize path separators to forward slashes
		relPath = filepath.ToSlash(relPath)
		filenames = append(filenames, relPath)
	}

	// Apply limit
	const maxResults = 100
	truncated := len(filenames) > maxResults
	if truncated {
		filenames = filenames[:maxResults]
	}

	return marshalGlobResult(GlobResult{
		DurationMs: duration,
		NumFiles:   len(filenames),
		Filenames:  filenames,
		Truncated:  truncated,
	})
}

func marshalGlobResult(result GlobResult) (string, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
