package tools

import (
	"context"
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

// GlobTool finds files matching a glob pattern using ripgrep
type GlobTool struct{}

func (t GlobTool) Name() string        { return "Glob" }
func (t GlobTool) Description() string {
	return "Find files matching a glob pattern using ripgrep. " +
		"Supports glob patterns like \"**/*.js\" or \"src/**/*.ts\". " +
		"Returns matching file paths sorted by modification time. " +
		"Use this tool when you need to find files by name patterns."
}
func (t GlobTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"pattern": {Type: "string", Description: "The glob pattern to match (e.g., **/*.ts)"},
		"path":    {Type: "string", Description: "The directory to search in (defaults to current directory)"},
	}
}
func (t GlobTool) Required() []string    { return []string{"pattern"} }
func (t GlobTool) Parallel() bool       { return true }

func (t GlobTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	if err := checkRipgrep(); err != nil {
		return "", err
	}

	var argsMap struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := parseArgs(args, &argsMap); err != nil {
		return "", err
	}

	if argsMap.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}

	absSearchDir, err := resolveSearchPath(ctx, argsMap.Path)
	if err != nil {
		return "", err
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

	// Create timeout context, merging with parent context if provided
	var cancelFn context.CancelFunc
	var execCtx context.Context
	if ctx == nil {
		execCtx, cancelFn = context.WithTimeout(context.Background(), 30*time.Second)
	} else {
		execCtx, cancelFn = context.WithTimeout(ctx, 30*time.Second)
	}
	defer cancelFn()

	start := time.Now()
	cmd := exec.CommandContext(execCtx, "rg", rgArgs...)
	cmd.Dir = searchBaseDir
	output, err := cmd.Output()
	if err != nil {
		if isRgNoMatch(err) {
			return marshalResult(GlobResult{
				DurationMs: time.Since(start).Milliseconds(),
				Filenames:  []string{},
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
		filenames = append(filenames, toRelativePath(filepath.Join(searchBaseDir, line), absSearchDir))
	}

	// Apply limit
	const maxResults = 100
	truncated := len(filenames) > maxResults
	if truncated {
		filenames = filenames[:maxResults]
	}

	return marshalResult(GlobResult{
		DurationMs: duration,
		NumFiles:   len(filenames),
		Filenames:  filenames,
		Truncated:  truncated,
	})
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

