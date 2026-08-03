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

	// maxGrepMaxResults caps max_results both in the schema and at runtime.
	// The schema constraint is a hint to the model; ExecuteContext clamps to
	// this value so out-of-range inputs can never inflate the context window.
	maxGrepMaxResults = 1000
)

// GrepResult holds structured grep output for internal formatting.
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
	FixedString     bool   `json:"fixed_string"`
	Multiline       bool   `json:"multiline"`
	ContextLines    *int   `json:"context_lines"`
	MaxResults      *int   `json:"max_results"`
}

type GrepTool struct{}

func (t GrepTool) Name() string { return ToolNameGrep }
func (t GrepTool) Description() string {
	return "Search file contents using ripgrep. " +
		"Supports regex or fixed-string patterns, file type filtering, and glob filtering. " +
		"Returns matching file paths by default, or matching lines with context in content mode. " +
		"Use fixed_string=true when searching for literal text containing regex special characters."
}
func (t GrepTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"pattern":          {Type: "string", Description: "Pattern to search for (regex by default; literal when fixed_string=true)"},
		"path":             {Type: "string", Description: "File or directory to search in (defaults to current directory)"},
		"glob":             {Type: "string", Description: "Glob pattern to filter files (e.g. \"*.js\", \"*.{ts,tsx}\")"},
		"type":             {Type: "string", Description: "File type filter using ripgrep's --type (e.g. \"go\", \"py\", \"js\")"},
		"output_mode":      {Type: "string", Description: "Output mode: \"files_with_matches\" (default), \"content\", or \"count\"", Enum: []string{"files_with_matches", "content", "count"}, Default: "files_with_matches"},
		"case_insensitive": {Type: "boolean", Description: "Case insensitive search"},
		"fixed_string":     {Type: "boolean", Description: "Treat pattern as a literal string, not regex (use when the pattern contains . * [ ( etc.)"},
		"multiline":        {Type: "boolean", Description: "Enable multiline mode where . matches newlines"},
		"context_lines":    {Type: "integer", Description: "Number of context lines to show before and after each match (content mode only)"},
		"max_results":      {Type: "integer", Description: "Maximum number of results to return (content lines or files; default 200)", Minimum: new(1.0), Maximum: new(float64(maxGrepMaxResults))},
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
	// Clamp to the schema-declared upper bound. Schema constraints are hints
	// to the model, not enforcement — the runtime is the last line of defense
	// against out-of-range values (e.g. a model passing 100000).
	maxResults = min(maxResults, maxGrepMaxResults)

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
	_ = time.Since(start).Milliseconds() // duration kept for potential future use

	if err != nil {
		if isRgNoMatch(err) {
			return noMatchResult(a.OutputMode), nil
		}
		if msg, ok := rgErrorMessage(err); ok {
			return "", fmt.Errorf("ripgrep error: %s", msg)
		}
		return "", fmt.Errorf("ripgrep failed: %w", err)
	}

	raw := strings.TrimSpace(string(output))
	if raw == "" {
		return noMatchResult(a.OutputMode), nil
	}

	switch a.OutputMode {
	case outputModeFiles:
		return formatFilesOutput(raw, absPath, maxResults)
	case outputModeContent:
		return formatContentOutput(raw, absPath, maxResults)
	case outputModeCount:
		return formatCountOutput(raw, absPath, maxResults)
	}

	return "", nil
}

// noMatchResult returns a short string indicating no matches were found.
func noMatchResult(mode string) string {
	switch mode {
	case outputModeFiles:
		return "(no matching files)"
	case outputModeCount:
		return "(0 matches)"
	default:
		return "(no matches)"
	}
}

// buildRgArgs constructs the ripgrep command-line arguments.
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
	if a.FixedString {
		args = append(args, "-F")
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

// --- Plain text output formatters ---

// formatFilesOutput formats "files_with_matches" results as one relative path per line.
func formatFilesOutput(raw, basePath string, maxResults int) (string, error) {
	lines := strings.Split(raw, "\n")
	var b strings.Builder
	count := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if count >= maxResults {
			fmt.Fprintf(&b, "... (showing %d of %d+ matching files)\n", maxResults, count)
			break
		}
		b.WriteString(toRelativePath(line, basePath))
		b.WriteByte('\n')
		count++
	}

	return strings.TrimRight(b.String(), "\n"), nil
}

// formatContentOutput formats "content" results as:
//
//	relative/path:line:content
//	relative/path:line:more
//	...
//	(truncated note if applicable)
func formatContentOutput(raw, basePath string, maxResults int) (string, error) {
	lines := strings.Split(raw, "\n")
	var b strings.Builder
	count := 0

	for _, line := range lines {
		if count >= maxResults {
			fmt.Fprintf(&b, "... (showing %d of %d+ matching lines)\n", maxResults, count)
			break
		}
		if line == "" {
			b.WriteByte('\n')
			count++
			continue
		}
		b.WriteString(convertPathInLine(line, basePath))
		b.WriteByte('\n')
		count++
	}

	return strings.TrimRight(b.String(), "\n"), nil
}

// formatCountOutput formats "count" results as:
//
//	relative/path: N
//	relative/path: M
//	(total: N+M matches across 2 files)
func formatCountOutput(raw, basePath string, maxResults int) (string, error) {
	lines := strings.Split(raw, "\n")
	var b strings.Builder
	totalMatches := 0
	count := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if count >= maxResults {
			fmt.Fprintf(&b, "... (showing %d of %d+ files)\n", maxResults, count)
			break
		}
		rel := convertPathInLine(line, basePath)
		b.WriteString(rel)
		b.WriteByte('\n')

		if parts := strings.SplitN(rel, ":", 2); len(parts) == 2 {
			if n, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
				totalMatches += n
			}
		}
		count++
	}

	if totalMatches > 0 {
		fmt.Fprintf(&b, "(total: %d matches across %d files)\n", totalMatches, count)
	}

	return strings.TrimRight(b.String(), "\n"), nil
}

// convertPathInLine transforms an absolute path prefix in a ripgrep output line
// to a relative path, preserving the ":line:" suffix.
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
