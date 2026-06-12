package memory

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// TopicBackend implements the Backend interface using local Markdown topic files
// searched via ripgrep. It is the memory backend for the AutoDream system.
//
// Key characteristics:
//   - Recall: searches topics/*.md and inbox.md via `rg` (ripgrep)
//   - Store: only handles DirectContent (appends to inbox.md); other scopes are no-op
//   - Memory is produced offline by the Dream sub-agent, not in real-time
//   - Searches both global (~/.tachi/memory/) and project (<git-root>/.tachi/memory/) domains
type TopicBackend struct {
	globalDir  string // ~/.tachi/memory/
	projectDir string // <git-root>/.tachi/memory/ (may be empty)
	rgPath     string // resolved path to rg binary (empty if unavailable)
	logger     *debuglog.Logger
}

// NewTopicBackend creates a TopicBackend.
// globalDir is required (typically ~/.tachi/memory/).
// projectDir may be empty if not in a git repository.
func NewTopicBackend(cfg Config) (*TopicBackend, error) {
	globalDir := filepath.Join(cfg.BaseDir, "memory")
	if err := os.MkdirAll(filepath.Join(globalDir, "topics"), 0700); err != nil {
		return nil, fmt.Errorf("topic: create global memory dir: %w", err)
	}

	// Determine project directory from current working directory.
	projectDir := ""
	if cwd, err := os.Getwd(); err == nil {
		if root := findGitRoot(cwd); root != "" {
			projectDir = filepath.Join(root, ".tachi", "memory")
			// Don't create project dir here — it's created on demand by Dream.
		}
	}

	logger := debuglog.DefaultLogger.WithSource("memory:topic")

	// Check if rg (ripgrep) is available.
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		logger.Log("rg not found in PATH — Recall will be unavailable")
	}

	return &TopicBackend{
		globalDir:  globalDir,
		projectDir: projectDir,
		rgPath:     rgPath,
		logger:     logger,
	}, nil
}

// Recall searches topic files and inbox for matching content.
func (t *TopicBackend) Recall(ctx context.Context, query string, limit int) ([]Entry, error) {
	if query == "" {
		return nil, nil
	}
	if t.rgPath == "" {
		return nil, nil // rg unavailable — silently return empty
	}
	if limit <= 0 {
		limit = 10
	}

	var allResults []Entry

	// 1. Search global topics + inbox
	results := t.searchDir(ctx, filepath.Join(t.globalDir, "topics"), query)
	allResults = append(allResults, results...)
	results = t.searchFile(ctx, filepath.Join(t.globalDir, "inbox.md"), query)
	allResults = append(allResults, results...)

	// 2. Search project topics + inbox (if available)
	if t.projectDir != "" {
		results = t.searchDir(ctx, filepath.Join(t.projectDir, "topics"), query)
		allResults = append(allResults, results...)
		results = t.searchFile(ctx, filepath.Join(t.projectDir, "inbox.md"), query)
		allResults = append(allResults, results...)
	}

	// Sort by score descending, truncate to limit.
	sort.Slice(allResults, func(i, j int) bool {
		return allResults[i].Score > allResults[j].Score
	})
	if len(allResults) > limit {
		allResults = allResults[:limit]
	}

	return allResults, nil
}

// Store handles memory writes. For TopicBackend, only DirectContent writes
// (from MemoryRecord tool) are processed — they're appended to inbox.md.
// All other scopes (turn/compact/session) are no-ops because memory is
// produced asynchronously by the Dream sub-agent.
func (t *TopicBackend) Store(ctx context.Context, opts StoreOptions) error {
	if opts.DirectContent == "" {
		return nil // no-op for non-direct writes
	}

	// Determine target domain: project if available, else global.
	targetDir := t.globalDir
	if t.projectDir != "" {
		targetDir = t.projectDir
		// Ensure project memory dir exists on first write.
		os.MkdirAll(targetDir, 0700)
	}

	inboxPath := filepath.Join(targetDir, "inbox.md")
	entry := fmt.Sprintf("\n## %s\n\n%s\n\n---\n",
		time.Now().Format(time.RFC3339), opts.DirectContent)

	f, err := os.OpenFile(inboxPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("topic: open inbox: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("topic: write inbox: %w", err)
	}

	t.logger.Log("wrote to inbox: %s (%d bytes)", inboxPath, len(entry))
	return nil
}

// Forget removes a memory entry by ID. For topic backend, this would
// require finding and removing a specific block from a topic file.
func (t *TopicBackend) Forget(ctx context.Context, id string) error {
	// TODO: implement block removal by searching for the ID across topic files.
	return nil
}

// Observe is a no-op for TopicBackend (no real-time observation needed).
func (t *TopicBackend) Observe(ctx context.Context, opts ObserveOptions) error {
	return nil
}

// --- Search implementation ---

// searchDir searches all .md files in a directory for matching blocks.
func (t *TopicBackend) searchDir(ctx context.Context, dir, query string) []Entry {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}

	// Use rg to find files containing the query.
	args := []string{"-l", "-i", "--glob", "*.md", query, dir}
	cmd := exec.CommandContext(ctx, t.rgPath, args...)
	out, err := cmd.Output()
	if err != nil {
		// Exit code 1 = no match (normal), other = real error.
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
			return nil
		}
		t.logger.Log("rg search failed in %s: %v", dir, err)
		return nil
	}

	files := strings.Split(strings.TrimSpace(string(out)), "\n")
	var results []Entry
	for _, f := range files {
		if f == "" {
			continue
		}
		entries := t.extractMatchingBlocks(f, query)
		results = append(results, entries...)
	}
	return results
}

// searchFile searches a single file for matching blocks.
func (t *TopicBackend) searchFile(ctx context.Context, path, query string) []Entry {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	// Check if file contains the query at all.
	cmd := exec.CommandContext(ctx, t.rgPath, "-i", "-q", query, path)
	if err := cmd.Run(); err != nil {
		return nil // no match
	}
	return t.extractMatchingBlocks(path, query)
}

// extractMatchingBlocks reads a markdown file, splits by "---" separators,
// and returns blocks that contain the query as Entry values.
func (t *TopicBackend) extractMatchingBlocks(path, query string) []Entry {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	blocks := splitByHR(string(content))
	var entries []Entry

	filename := filepath.Base(path)
	queryLower := strings.ToLower(query)

	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if !containsIgnoreCase(block, queryLower) {
			continue
		}

		entry := Entry{
			ID:      fmt.Sprintf("topic:%s:%d", filename, hashBlock(block)),
			Summary: extractTitle(block),
			Content: truncateContent(block, 1000),
			Score:   computeScore(block, queryLower),
		}

		// Try to extract timestamp from "来源:" or date in title.
		if ts := extractTimestamp(block); ts > 0 {
			entry.Timestamp = ts
		}

		entries = append(entries, entry)
	}

	return entries
}

// --- Scoring ---

// computeScore calculates a relevance score for a block.
func computeScore(block, queryLower string) float64 {
	score := 0.5

	// Title match bonus.
	title := strings.ToLower(extractTitle(block))
	if strings.Contains(title, queryLower) {
		score += 0.2
	}

	// Keyword line match bonus.
	if matchesKeywordLine(block, queryLower) {
		score += 0.2
	}

	// Superseded penalty.
	blockLower := strings.ToLower(block)
	if strings.Contains(blockLower, "状态: superseded") || strings.Contains(blockLower, "status: superseded") {
		score -= 0.3
	}

	// Recency bonus: if timestamp is recent (within 7 days).
	if ts := extractTimestamp(block); ts > 0 {
		age := time.Since(time.Unix(ts, 0))
		if age < 7*24*time.Hour {
			score += 0.1
		}
	}

	return score
}

// matchesKeywordLine checks if the query matches a "关键词:" or "Keywords:" line.
func matchesKeywordLine(block, queryLower string) bool {
	for _, line := range strings.Split(block, "\n") {
		lineLower := strings.ToLower(line)
		if (strings.HasPrefix(lineLower, "关键词:") || strings.HasPrefix(lineLower, "keywords:")) &&
			strings.Contains(lineLower, queryLower) {
			return true
		}
	}
	return false
}

// --- Markdown parsing helpers ---

// splitByHR splits markdown content by horizontal rules (---).
// Preserves H1/H2 headers as part of the following block.
func splitByHR(content string) []string {
	lines := strings.Split(content, "\n")
	var blocks []string
	var current strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// A horizontal rule: a line of 3+ dashes with nothing else.
		if len(trimmed) >= 3 && strings.Trim(trimmed, "-") == "" && !strings.HasPrefix(trimmed, "# ") {
			if current.Len() > 0 {
				blocks = append(blocks, current.String())
				current.Reset()
			}
			continue
		}
		if current.Len() > 0 {
			current.WriteByte('\n')
		}
		current.WriteString(line)
	}
	if current.Len() > 0 {
		blocks = append(blocks, current.String())
	}
	return blocks
}

// extractTitle returns the first ## or # line from a block.
func extractTitle(block string) string {
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			return strings.TrimPrefix(trimmed, "## ")
		}
		if strings.HasPrefix(trimmed, "# ") {
			return strings.TrimPrefix(trimmed, "# ")
		}
	}
	// No header found — use first non-empty line truncated.
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			if len(trimmed) > 80 {
				return trimmed[:80] + "..."
			}
			return trimmed
		}
	}
	return ""
}

// extractTimestamp tries to find a date in the block (from "来源:" line or ## header).
func extractTimestamp(block string) int64 {
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		// Try to find an RFC3339 or date-like pattern.
		if idx := strings.Index(trimmed, "202"); idx >= 0 {
			// Extract potential date substring.
			sub := trimmed[idx:]
			if len(sub) >= 10 {
				// Try parsing "2026-06-10" or full RFC3339.
				if t, err := time.Parse(time.RFC3339, sub[:min(len(sub), 25)]); err == nil {
					return t.Unix()
				}
				if t, err := time.Parse("2006-01-02", sub[:10]); err == nil {
					return t.Unix()
				}
			}
		}
	}
	return 0
}

// --- Utility functions ---

func containsIgnoreCase(s, lowerQuery string) bool {
	return strings.Contains(strings.ToLower(s), lowerQuery)
}

func truncateContent(s string, maxLen int) string {
	// Truncate at rune boundary.
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

func hashBlock(block string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(block))
	return h.Sum32()
}

// findGitRoot walks up from dir looking for .git directory.
func findGitRoot(dir string) string {
	if dir == "" {
		return ""
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}
