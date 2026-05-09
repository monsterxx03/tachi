package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	outputModeFiles   = "files_with_matches"
	outputModeContent = "content"
	outputModeCount   = "count"
)

type GrepResult struct {
	DurationMs int64    `json:"durationMs"`
	NumFiles   int      `json:"numFiles"`
	NumLines   int      `json:"numLines,omitempty"`
	NumMatches int      `json:"numMatches,omitempty"`
	Filenames  []string `json:"filenames"`
	Content    string   `json:"content,omitempty"`
	Mode       string   `json:"mode"`
	Truncated  bool     `json:"truncated"`
}

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	Type            string `json:"type"`
	OutputMode      string `json:"output_mode"`
	CaseInsensitive bool   `json:"case_insensitive"`
	Multiline       bool   `json:"multiline"`
	ContextLines    *int   `json:"context_lines"`
	MaxResults      *int   `json:"max_results"`
}

type GrepTool struct{}

func (t GrepTool) Name() string { return ToolNameGrep }
func (t GrepTool) Description() string {
	return "Search file contents using ripgrep. " +
		"Supports regex patterns, file type filtering, and glob filtering. " +
		"Returns matching file paths by default, or matching lines with context in content mode."
}
func (t GrepTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"pattern":          {Type: "string", Description: "Regular expression pattern to search for"},
		"path":             {Type: "string", Description: "File or directory to search in (defaults to current directory)"},
		"glob":             {Type: "string", Description: "Glob pattern to filter files (e.g. \"*.js\", \"*.{ts,tsx}\")"},
		"type":             {Type: "string", Description: "File type filter using ripgrep's --type (e.g. \"go\", \"py\", \"js\")"},
		"output_mode":      {Type: "string", Description: "Output mode: \"files_with_matches\" (default), \"content\", or \"count\""},
		"case_insensitive": {Type: "boolean", Description: "Case insensitive search"},
		"multiline":        {Type: "boolean", Description: "Enable multiline mode where . matches newlines"},
		"context_lines":    {Type: "integer", Description: "Number of context lines to show before and after each match (content mode only)"},
		"max_results":      {Type: "integer", Description: "Maximum number of results to return (default 200)"},
	}
}
func (t GrepTool) Required() []string { return []string{"pattern"} }
func (t GrepTool) Parallel() bool     { return true }

func (t GrepTool) ExecuteContext(parentCtx context.Context, args string) (string, error) {
	if err := checkRipgrep(); err != nil {
		return "", err
	}

	var a grepArgs
	if err := parseArgs(args, &a); err != nil {
		return "", err
	}

	if a.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}

	if a.OutputMode == "" {
		a.OutputMode = outputModeFiles
	}
	switch a.OutputMode {
	case outputModeFiles, outputModeContent, outputModeCount:
	default:
		return "", fmt.Errorf("invalid output_mode: %q (must be files_with_matches, content, or count)", a.OutputMode)
	}

	maxResults := 200
	if a.MaxResults != nil && *a.MaxResults > 0 {
		maxResults = *a.MaxResults
	}

	absPath, err := resolveSearchPath(parentCtx, a.Path)
	if err != nil {
		return "", err
	}

	rgArgs := buildRgArgs(&a)
	rgArgs = append(rgArgs, absPath)

	// Create timeout context, merging with parent context if provided
	var cancelFn context.CancelFunc
	var ctx context.Context
	if parentCtx == nil {
		ctx, cancelFn = context.WithTimeout(context.Background(), 30*time.Second)
	} else {
		ctx, cancelFn = context.WithTimeout(parentCtx, 30*time.Second)
	}
	defer cancelFn()

	start := time.Now()
	cmd := exec.CommandContext(ctx, "rg", rgArgs...)
	output, err := cmd.Output()
	duration := time.Since(start).Milliseconds()

	if err != nil {
		if isRgNoMatch(err) {
			return emptyGrepResult(duration, a.OutputMode)
		}
		if msg, ok := rgErrorMessage(err); ok {
			return "", fmt.Errorf("ripgrep error: %s", msg)
		}
		return "", fmt.Errorf("ripgrep failed: %w", err)
	}

	raw := strings.TrimSpace(string(output))
	if raw == "" {
		return emptyGrepResult(duration, a.OutputMode)
	}

	var result GrepResult
	switch a.OutputMode {
	case outputModeFiles:
		result = buildFilesResult(raw, absPath, maxResults, duration)
	case outputModeContent:
		result = buildContentResult(raw, absPath, maxResults, duration)
	case outputModeCount:
		result = buildCountResult(raw, absPath, maxResults, duration)
	}

	return marshalResult(result)
}

func emptyGrepResult(duration int64, mode string) (string, error) {
	return marshalResult(GrepResult{
		DurationMs: duration,
		Filenames:  []string{},
		Mode:       mode,
	})
}

func buildRgArgs(a *grepArgs) []string {
	args := []string{
		"--hidden",
		"--max-columns", "500",
		"--glob", "!.git",
		"--glob", "!.svn",
		"--glob", "!.hg",
	}

	switch a.OutputMode {
	case outputModeFiles:
		args = append(args, "-l")
	case outputModeCount:
		args = append(args, "-c")
	case outputModeContent:
		args = append(args, "-n")
		if a.ContextLines != nil && *a.ContextLines > 0 {
			args = append(args, "-C", strconv.Itoa(*a.ContextLines))
		}
	}

	if a.CaseInsensitive {
		args = append(args, "-i")
	}
	if a.Multiline {
		args = append(args, "-U", "--multiline-dotall")
	}
	if a.Type != "" {
		args = append(args, "--type", a.Type)
	}
	if a.Glob != "" {
		args = append(args, "--glob", a.Glob)
	}

	args = append(args, "-e", a.Pattern)

	return args
}

func buildFilesResult(raw, basePath string, maxResults int, duration int64) GrepResult {
	lines := strings.Split(raw, "\n")
	filenames := make([]string, 0, min(len(lines), maxResults))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		filenames = append(filenames, toRelativePath(line, basePath))
	}

	truncated := len(filenames) > maxResults
	if truncated {
		filenames = filenames[:maxResults]
	}

	return GrepResult{
		DurationMs: duration,
		NumFiles:   len(filenames),
		Filenames:  filenames,
		Mode:       outputModeFiles,
		Truncated:  truncated,
	}
}

func buildContentResult(raw, basePath string, maxResults int, duration int64) GrepResult {
	lines := strings.Split(raw, "\n")

	converted := make([]string, 0, min(len(lines), maxResults))
	truncated := false
	for _, line := range lines {
		if len(converted) >= maxResults {
			truncated = true
			break
		}
		if line == "" {
			converted = append(converted, line)
			continue
		}
		converted = append(converted, convertPathInLine(line, basePath))
	}

	fileSet := make(map[string]struct{})
	for _, line := range converted {
		if line == "" || line == "--" {
			continue
		}
		if parts := strings.SplitN(line, ":", 2); len(parts) > 1 {
			fileSet[parts[0]] = struct{}{}
		}
	}

	filenames := make([]string, 0, len(fileSet))
	for f := range fileSet {
		filenames = append(filenames, f)
	}

	return GrepResult{
		DurationMs: duration,
		NumFiles:   len(fileSet),
		NumLines:   len(converted),
		Filenames:  filenames,
		Content:    strings.Join(converted, "\n"),
		Mode:       outputModeContent,
		Truncated:  truncated,
	}
}

func buildCountResult(raw, basePath string, maxResults int, duration int64) GrepResult {
	lines := strings.Split(raw, "\n")
	totalMatches := 0
	var content strings.Builder
	filenames := make([]string, 0, min(len(lines), maxResults))
	truncated := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(filenames) >= maxResults {
			truncated = true
			break
		}

		rel := convertPathInLine(line, basePath)
		if content.Len() > 0 {
			content.WriteByte('\n')
		}
		content.WriteString(rel)

		if parts := strings.SplitN(rel, ":", 2); len(parts) == 2 {
			filenames = append(filenames, parts[0])
			if n, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
				totalMatches += n
			}
		}
	}

	return GrepResult{
		DurationMs: duration,
		NumFiles:   len(filenames),
		NumMatches: totalMatches,
		Filenames:  filenames,
		Content:    content.String(),
		Mode:       outputModeCount,
		Truncated:  truncated,
	}
}

func convertPathInLine(line, basePath string) string {
	if !strings.HasPrefix(line, basePath) {
		return line
	}
	rest := line[len(basePath):]
	idx := strings.Index(rest, ":")
	if idx == -1 {
		return toRelativePath(line, basePath)
	}
	return toRelativePath(line[:len(basePath)+idx], basePath) + rest[idx:]
}
