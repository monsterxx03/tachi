package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// decayCacheTTL is how long cached decay states remain valid before
// being re-read from disk. Balances accuracy vs I/O.
const decayCacheTTL = 30 * time.Second

// SessionProvider provides access to recent session metadata for temporal
// query fallback. When the keyword-based topic search returns no results
// (or very low confidence), TopicBackend falls back to recent session
// summaries — this handles queries like "what did we talk about recently"
// that grep-based search can never match.
type SessionProvider interface {
	RecentSessions(ctx context.Context, limit int) ([]RecentSession, error)
}

// RecentSession is a lightweight summary of a session for recall fallback.
type RecentSession struct {
	ID             string
	Title          string
	RecentMessages []string // last N user message texts for temporal context
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// TopicBackend implements the Backend interface using local Markdown topic files
// searched via ripgrep. It is the memory backend for the AutoDream system.
//
// Key characteristics:
//   - Recall: searches topics/*.md and inbox.md via `rg` (ripgrep)
//   - Store: only handles DirectContent (appends to inbox.md); other scopes are no-op
//   - Memory is produced offline by the Dream sub-agent, not in real-time
//   - Searches both global (~/.tachi/memory/) and project (<git-root>/.tachi/memory/) domains
//   - Optional KeywordExtractor for improved text-search recall
//   - Optional SessionProvider for temporal query fallback
type TopicBackend struct {
	globalDir  string // ~/.tachi/memory/
	projectDir string // <git-root>/.tachi/memory/ (may be empty)
	rgPath     string // resolved path to rg binary (empty if unavailable)
	logger     *logger.Logger
	extractor  KeywordExtractor // optional: extracts keywords from query for better recall

	sessionProvider SessionProvider // optional: for temporal query fallback
	halfLifeDays    int             // decay half-life in days (default 7)

	// reinforceMu protects concurrent writes to last_dream.json from
	// simultaneous ReinforceFact calls (e.g. parallel MemoryRecallReminder
	// and MemoryRecallTool hits).
	reinforceMu sync.Mutex

	// decayCache caches loaded fact_states to avoid re-reading last_dream.json
	// on every Recall call. Invalidated on ReinforceFact or after decayCacheTTL.
	decayCacheMu   sync.RWMutex
	decayCache     map[string]*FactState
	decayCacheTime time.Time
}

// NewTopicBackend creates a TopicBackend.
// globalDir is required (typically ~/.tachi/memory/).
// projectDir may be empty if not in a git repository.
func NewTopicBackend(cfg Config, l *logger.Logger) (*TopicBackend, error) {
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


	// Check if rg (ripgrep) is available.
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		l.Logf(context.Background(), "rg not found in PATH — Recall will be unavailable")
	}

	halfLife := cfg.DecayHalfLifeDays
	if halfLife <= 0 {
		halfLife = 7
	}

	return &TopicBackend{
		globalDir:    globalDir,
		projectDir:   projectDir,
		rgPath:       rgPath,
		logger:       l,
		halfLifeDays: halfLife,
	}, nil
}

// SetKeywordExtractor configures an optional keyword extractor.
// When set, Recall() will first extract keywords from the user query
// and search using those keywords instead of the raw query text,
// improving recall for text-based (rg) search.
func (t *TopicBackend) SetKeywordExtractor(ext KeywordExtractor) {
	t.extractor = ext
}

// SetSessionProvider configures an optional SessionProvider for temporal
// query fallback. When the keyword-based search returns no results (or very
// low confidence), TopicBackend will supplement results with recent session
// summaries — handling queries like "what did we talk about recently" that
// content-based grep cannot match.
func (t *TopicBackend) SetSessionProvider(sp SessionProvider) {
	t.sessionProvider = sp
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

	// Extract keywords for better text-search recall.
	// Falls back to raw query if extractor is not set or fails.
	keywords := []string{query}
	if t.extractor != nil {
		if kws, err := t.extractor.ExtractKeywords(ctx, query); err == nil && len(kws) > 0 {
			keywords = kws
			t.logger.Logf(ctx, "keywords extracted: %v (from %q)", keywords, query)
		} else if err != nil {
			t.logger.Logf(ctx, "keyword extraction failed, falling back to raw query: %v", err)
		}
	}

	// Load decay states from last_dream.json in both domains.
	decayStates := t.loadDecayStates()

	var allResults []Entry

	// 1. Search global topics + inbox
	results := t.searchDir(ctx, filepath.Join(t.globalDir, "topics"), keywords)
	allResults = append(allResults, results...)
	results = t.searchFile(ctx, filepath.Join(t.globalDir, "inbox.md"), keywords)
	allResults = append(allResults, results...)

	// 2. Search project topics + inbox (if available)
	if t.projectDir != "" {
		results = t.searchDir(ctx, filepath.Join(t.projectDir, "topics"), keywords)
		allResults = append(allResults, results...)
		results = t.searchFile(ctx, filepath.Join(t.projectDir, "inbox.md"), keywords)
		allResults = append(allResults, results...)
	}

	// Apply memory lifecycle factors from FactState:
	// - Decay multiplier: adjusts score based on time since last reinforcement
	// - Superseded penalty: facts marked superseded by dream are heavily downranked
	// - Reinforcement bonus: facts that have been frequently recalled are boosted
	for i := range allResults {
		if fs, ok := decayStates[allResults[i].ID]; ok {
			decayMultiplier := 0.3 + 0.7*fs.Decay
			allResults[i].Score *= decayMultiplier

			if fs.Superseded {
				allResults[i].Score -= 0.3
			}

			if fs.Reinforcements >= 3 {
				allResults[i].Score += 0.1
			}
		}
	}

	// Sort by score descending, truncate to limit.
	sort.Slice(allResults, func(i, j int) bool {
		return allResults[i].Score > allResults[j].Score
	})
	if len(allResults) > limit {
		allResults = allResults[:limit]
	}

	// Session fallback: when topic search returns no results or very low
	// confidence (top score < 0.3), supplement with recent session summaries.
	// This handles temporal/navigational queries like "什么我们最近聊过什么"
	// that keyword-based grep cannot match, without hardcoding any trigger words.
	//
	// Session entries get a medium confidence score (0.7) so they rank above
	// weak topic matches (decayed/superseded) but below strong content hits.
	if (len(allResults) == 0 || allResults[0].Score < 0.3) && t.sessionProvider != nil {
		sessionEntries, err := t.fetchRecentSessions(ctx, limit)
		if err != nil {
			t.logger.Logf(ctx, "session fallback: %v", err)
		} else if len(sessionEntries) > 0 {
			allResults = append(sessionEntries, allResults...)
			sort.Slice(allResults, func(i, j int) bool {
				return allResults[i].Score > allResults[j].Score
			})
			if len(allResults) > limit {
				allResults = allResults[:limit]
			}
			t.logger.Logf(ctx, "session fallback: %d session(s) added to recall results (topic results: %d)",
				len(sessionEntries), len(allResults)-len(sessionEntries))
		}
	}

	return allResults, nil
}

// fetchRecentSessions retrieves recent session summaries from the SessionProvider
// and converts them to memory Entry format with a medium confidence score (0.7).
// These entries serve as a fallback when keyword-based topic search fails,
// enabling temporal/navigational queries like "what did we talk about recently".
// Each entry includes the session title and the most recent user messages for
// meaningful context about what was discussed.
func (t *TopicBackend) fetchRecentSessions(ctx context.Context, limit int) ([]Entry, error) {
	sessions, err := t.sessionProvider.RecentSessions(ctx, limit)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(sessions))
	for _, s := range sessions {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		// Build a rich content string with session metadata + recent user messages.
		var content strings.Builder
		content.WriteString(fmt.Sprintf("Session: %s\nDate: %s\n",
			title, s.CreatedAt.Format("2006-01-02 15:04")))
		for i, msg := range s.RecentMessages {
			if i > 0 {
				content.WriteByte('\n')
			}
			content.WriteString(fmt.Sprintf("  User: %s", msg))
		}
		entries = append(entries, Entry{
			ID:        "session:" + s.ID,
			SessionID: s.ID,
			Summary:   title,
			Content:   content.String(),
			Timestamp: s.CreatedAt.Unix(),
			Score:     0.7,
		})
	}
	return entries, nil
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

	t.logger.Logf(ctx, "wrote to inbox: %s (%d bytes)", inboxPath, len(entry))
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
func (t *TopicBackend) searchDir(ctx context.Context, dir string, keywords []string) []Entry {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}

	// Use rg with -e flags for each keyword (OR logic).
	// -F enables fixed-string matching to avoid regex metacharacter issues.
	args := []string{"-F", "-l", "-i", "--glob", "*.md"}
	for _, kw := range keywords {
		args = append(args, "-e", kw)
	}
	args = append(args, dir)
	t.logger.Logf(ctx, "rg %s", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, t.rgPath, args...)
	out, err := cmd.Output()
	if err != nil {
		// Exit code 1 = no match (normal), other = real error.
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
			return nil
		}
		t.logger.Logf(ctx, "rg search failed in %s: %v", dir, err)
		return nil
	}

	files := strings.Split(strings.TrimSpace(string(out)), "\n")
	var results []Entry
	for _, f := range files {
		if f == "" {
			continue
		}
		entries := t.extractMatchingBlocks(f, keywords)
		results = append(results, entries...)
	}
	return results
}

// searchFile searches a single file for matching blocks.
func (t *TopicBackend) searchFile(ctx context.Context, path string, keywords []string) []Entry {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	// Check if file contains any keyword.
	args := []string{"-F", "-i", "-q"}
	for _, kw := range keywords {
		args = append(args, "-e", kw)
	}
	args = append(args, path)
	t.logger.Logf(ctx, "rg %s", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, t.rgPath, args...)
	if err := cmd.Run(); err != nil {
		return nil // no match
	}
	return t.extractMatchingBlocks(path, keywords)
}

// --- Decay state helpers ---

// loadDecayStates returns fact_states from last_dream.json (both domains),
// using an in-memory cache with TTL to avoid repeated disk reads.
// Decay values are recalculated from LastReinforced at read time for accuracy.
func (t *TopicBackend) loadDecayStates() map[string]*FactState {
	// Fast path: check cache under read lock.
	t.decayCacheMu.RLock()
	if t.decayCache != nil && time.Since(t.decayCacheTime) < decayCacheTTL {
		cached := t.decayCache
		t.decayCacheMu.RUnlock()
		return cached
	}
	t.decayCacheMu.RUnlock()

	// Slow path: reload from disk.
	result := make(map[string]*FactState)
	for _, dir := range []string{t.globalDir, t.projectDir} {
		if dir == "" {
			continue
		}
		states := loadFactStatesFromFile(dir)
		for k, v := range states {
			if _, exists := result[k]; !exists {
				result[k] = v
			}
		}
	}

	// Recalculate decay in real-time from LastReinforced so values stay
	// accurate between dream runs (stored values may be hours/days stale).
	halfLife := float64(t.halfLifeDays) * 24 * 3600
	for _, fs := range result {
		if !fs.LastReinforced.IsZero() {
			elapsed := time.Since(fs.LastReinforced).Seconds()
			fs.Decay = math.Exp(-math.Ln2 * elapsed / halfLife)
		}
	}

	// Update cache.
	t.decayCacheMu.Lock()
	t.decayCache = result
	t.decayCacheTime = time.Now()
	t.decayCacheMu.Unlock()

	return result
}

// invalidateDecayCache clears the cached decay states, forcing the next
// loadDecayStates call to re-read from disk. Called after ReinforceFact writes.
func (t *TopicBackend) invalidateDecayCache() {
	t.decayCacheMu.Lock()
	t.decayCache = nil
	t.decayCacheMu.Unlock()
}

// dreamStateJSON mirrors dream.State for deserializing last_dream.json
// without importing the dream package (avoids circular dependency).
type dreamStateJSON struct {
	LastDreamAt     time.Time             `json:"last_dream_at"`
	SessionsDreamed int                   `json:"sessions_dreamed"`
	TopicsCreated   int                   `json:"topics_created"`
	FactsAdded      int                   `json:"facts_added"`
	FactsSuperseded int                   `json:"facts_superseded"`
	FactsPruned     int                   `json:"facts_pruned"`
	Errors          []string              `json:"errors,omitempty"`
	FactStates      map[string]*FactState `json:"fact_states,omitempty"`
}

// loadFactStatesFromFile reads last_dream.json from the given memory dir
// and returns the fact_states map, or nil if the file doesn't exist.
func loadFactStatesFromFile(memoryDir string) map[string]*FactState {
	data, err := os.ReadFile(filepath.Join(memoryDir, DreamStateFile))
	if err != nil {
		return nil
	}
	var state dreamStateJSON
	if err := json.Unmarshal(data, &state); err != nil {
		return nil
	}
	return state.FactStates
}

// writeFactStates persists fact_states back to last_dream.json, preserving
// all existing top-level fields. Uses a temp-file + rename to avoid
// corruption on crash (atomic write).
func writeFactStates(memoryDir string, updateFn func(map[string]*FactState)) error {
	statePath := filepath.Join(memoryDir, DreamStateFile)

	// Read existing state, preserving all fields.
	data, err := os.ReadFile(statePath)
	var state dreamStateJSON
	if err == nil {
		if uerr := json.Unmarshal(data, &state); uerr != nil {
			// Corrupt file — start fresh.
			state = dreamStateJSON{}
		}
	}
	if state.FactStates == nil {
		state.FactStates = make(map[string]*FactState)
	}

	updateFn(state.FactStates)

	out, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	// Write to temp file first, then rename for atomicity.
	tmpPath := statePath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, statePath)
}

// ReinforceFact strengthens a fact's decay state when recalled.
// It searches both global and project last_dream.json for the fact,
// increments its reinforcement counter, resets decay to 1.0,
// and updates the last_reinforced timestamp.
func (t *TopicBackend) ReinforceFact(ctx context.Context, entryID string) error {
	t.reinforceMu.Lock()
	defer t.reinforceMu.Unlock()

	for _, dir := range []string{t.globalDir, t.projectDir} {
		if dir == "" {
			continue
		}
		statePath := filepath.Join(dir, DreamStateFile)
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			continue
		}

		// Check if the fact exists before incurring write I/O.
		states := loadFactStatesFromFile(dir)
		if _, ok := states[entryID]; !ok {
			continue
		}

		err := writeFactStates(dir, func(states map[string]*FactState) {
			fs := states[entryID]
			fs.Reinforcements++
			fs.LastReinforced = time.Now()
			fs.Decay = 1.0
		})
		if err != nil {
			t.logger.Logf(ctx, "ReinforceFact: write %s: %v", statePath, err)
		}
	}
	return nil
}
func (t *TopicBackend) extractMatchingBlocks(path string, keywords []string) []Entry {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	blocks := SplitByHR(string(content))
	var entries []Entry

	filename := filepath.Base(path)

	// Lowercase all keywords once for case-insensitive matching.
	keywordLower := make([]string, len(keywords))
	for i, kw := range keywords {
		keywordLower[i] = strings.ToLower(kw)
	}

	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if !matchesAnyKeyword(block, keywordLower) {
			continue
		}

		entry := Entry{
			ID:      FactID(filename, block),
			Summary: extractTitle(block),
			Content: truncateContent(block, 1000),
			Score:   computeScoreMulti(block, keywordLower),
		}

		// Try to extract timestamp from "来源:" or date in title.
		if ts := extractTimestamp(block); ts > 0 {
			entry.Timestamp = ts
		}

		entries = append(entries, entry)
	}

	return entries
}

// matchesAnyKeyword returns true if block contains any of the given
// (already lowercased) keywords.
func matchesAnyKeyword(block string, keywords []string) bool {
	blockLower := strings.ToLower(block)
	for _, kw := range keywords {
		if strings.Contains(blockLower, kw) {
			return true
		}
	}
	return false
}

// --- Scoring ---

// computeScoreMulti calculates a relevance score for a block against multiple keywords.
// It measures text relevance only (keyword matches, title hit, recency).
// Memory lifecycle factors (decay, superseded status, reinforcement count) are
// applied separately in Recall() using authoritative FactState data.
func computeScoreMulti(block string, keywords []string) float64 {
	score := 0.5

	// Title match bonus.
	title := strings.ToLower(extractTitle(block))
	for _, kw := range keywords {
		if strings.Contains(title, kw) {
			score += 0.2
			break
		}
	}

	// Keyword line match bonus.
	for _, kw := range keywords {
		if matchesKeywordLine(block, kw) {
			score += 0.2
			break
		}
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

// computeScore calculates a relevance score for a block against a single query.
// Delegates to computeScoreMulti for consistency.
func computeScore(block, queryLower string) float64 {
	return computeScoreMulti(block, []string{queryLower})
}

// matchesKeywordLine checks if the query matches a "关键词:" or "Keywords:" line.
func matchesKeywordLine(block, queryLower string) bool {
	for line := range strings.SplitSeq(block, "\n") {
		lineLower := strings.ToLower(line)
		if (strings.HasPrefix(lineLower, "关键词:") || strings.HasPrefix(lineLower, "keywords:")) &&
			strings.Contains(lineLower, queryLower) {
			return true
		}
	}
	return false
}

// --- Markdown parsing helpers ---

// SplitByHR splits markdown content by horizontal rules (---).
// Preserves H1/H2 headers as part of the following block.
func SplitByHR(content string) []string {
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
	for line := range strings.SplitSeq(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "## "); ok {
			return after
		}
		if after, ok := strings.CutPrefix(trimmed, "# "); ok {
			return after
		}
	}
	// No header found — use first non-empty line truncated.
	for line := range strings.SplitSeq(block, "\n") {
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
	for line := range strings.SplitSeq(block, "\n") {
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

func truncateContent(s string, maxLen int) string {
	// Truncate at rune boundary.
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
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
