package memory

import (
	"context"
	"crypto/sha256"
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

	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// decayCacheTTL is how long cached decay states remain valid before
// being re-read from disk. Balances accuracy vs I/O.
const decayCacheTTL = 30 * time.Second

// Memory recall scoring constants — tuned heuristics, adjust together.
const (
	// decayFloor: fully-decayed facts still retain this fraction of score.
	decayFloor = 0.3
	// decayWeight: how much the decay state (0..1) pulls the multiplier
	// from decayFloor up toward 1.0.
	decayWeight = 0.7
	// supersededPenalty: fixed downrank for facts marked superseded by dream.
	supersededPenalty = 0.3
	// reinforcementThreshold / reinforcementBonus: facts recalled at least
	// this many times get a small boost.
	reinforcementThreshold = 3
	reinforcementBonus     = 0.1
	// sessionFallbackScore: confidence given to session-summary entries. The
	// fallback only fires when NO topic block matched at all, so this number
	// never competes with a real hit — it is here so the entries sort sanely
	// if a caller ever mixes them.
	sessionFallbackScore = 0.3
	// sessionFallbackLimit caps how many recent sessions the fallback injects.
	// Five of them (the old limit) spent roughly a kilotoken per user message
	// on turns where nothing relevant was found; three still answers "what did
	// we talk about recently".
	sessionFallbackLimit = 3
	// recentWindow: entries younger than this get a recency bonus.
	recentWindow = 7 * 24 * time.Hour
	// titleBonus / keywordLineBonus / recencyBonus: scoring bumps on top of
	// term coverage. A block matching every query term already scores 1.0, so
	// these only separate equally-covered blocks.
	titleBonus       = 0.2
	keywordLineBonus = 0.2
	recencyBonus     = 0.1
	// maxLiteralSearchWords / maxLiteralCJKRunes bound when a query is already
	// a literal a whitespace-segmented or CJK text search can use as-is — see
	// shouldSkipExtraction, which is the only reader.
	maxLiteralSearchWords = 3
	maxLiteralCJKRunes    = 10

	// maxSearchTerms bounds how many literal strings one query sends to rg and
	// scores against. An extracted keyword expands into a phrase, its words and
	// (for CJK) its bigrams; five keywords could otherwise produce a hundred
	// terms, most of them generic fragments.
	maxSearchTerms = 24

	// coverageWeight scales coverage below 1.0 so the bonuses can still
	// separate two blocks that both contain every term.
	coverageWeight = 0.7

	// Term weights. A phrase is the most specific thing the extractor produced,
	// a whole CJK term is next, a bigram is a fragment that can appear in any
	// sentence, and a short ASCII word ("go", "db") has little discriminating
	// power on its own.
	weightPhrase    = 1.6
	weightCJKTerm   = 1.2
	weightBigram    = 0.6
	weightWord      = 1.0
	weightShortWord = 0.5
)

// recallStopWords are dropped before searching. They are the words a question
// is made of rather than the words it asks about: leaving them in gives every
// block a small match score for "what" and "怎么", which is exactly the noise
// the coverage ranking exists to avoid.
//
// Kept short and conservative on purpose — an over-eager list silently loses
// recall, which is the failure this whole path is being fixed for.
var recallStopWords = map[string]struct{}{
	// English function words.
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "of": {}, "to": {},
	"in": {}, "on": {}, "at": {}, "for": {}, "with": {}, "from": {}, "by": {},
	"is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {},
	"do": {}, "does": {}, "did": {}, "how": {}, "what": {}, "why": {},
	"when": {}, "where": {}, "which": {}, "who": {}, "i": {}, "you": {},
	"we": {}, "they": {}, "it": {}, "this": {}, "that": {}, "my": {},
	"me": {}, "our": {}, "your": {}, "its": {},
	// Chinese equivalents, including the bigrams they break into.
	"怎么": {}, "什么": {}, "如何": {}, "为什": {}, "这个": {}, "那个": {},
	"这些": {}, "那些": {}, "我们": {}, "你们": {}, "他们": {}, "可以": {},
	"是不": {}, "不是": {}, "一下": {}, "现在": {}, "之前": {}, "以前": {},
	"然后": {}, "还是": {}, "就是": {}, "已经": {}, "需要": {}, "应该": {},
	"关于": {}, "以及": {}, "一个": {}, "的话": {}, "了吗": {}, "最近": {},
}

// SessionProvider provides access to recent session metadata for temporal
// query fallback. When the keyword-based topic search returns NOTHING at all,
// TopicBackend falls back to recent session summaries — this handles queries
// like "what did we talk about recently" that grep-based search can never
// match. The provider is responsible for skipping the conversation being
// served (see agent.topicSessionProvider): injecting the current session as a
// "past session" is how the memory block ended up quoting the user back at
// themselves on every turn.

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
	// simultaneous ReinforceFacts calls (e.g. parallel MemoryRecallReminder
	// and MemoryRecallTool hits).
	reinforceMu sync.Mutex

	// decayCache caches loaded fact_states to avoid re-reading last_dream.json
	// on every Recall call. Invalidated on ReinforceFacts or after decayCacheTTL.
	decayCacheMu   sync.RWMutex
	decayCache     map[string]*FactState
	decayCacheTime time.Time
}

// NewTopicBackend creates a TopicBackend.
// globalDir is required (typically ~/.tachi/memory/).
// projectDir may be empty if not in a git repository.
func NewTopicBackend(cfg config.MemoryConfig, l *logger.Logger) (*TopicBackend, error) {
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
		l.Info(context.Background(), "rg not found in PATH — Recall will be unavailable")
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

// SessionProviderWired reports whether SetSessionProvider has been given a provider.
//
// It exists because the wiring is attached to the BACKEND OBJECT, while the callers that
// attach it run at different moments (SetSessionManager at construction, AIAgent.ReloadMemory
// after it rebuilds the backend). Losing it that way has no other symptom — recall just stops
// supplementing temporal queries — so this is what a regression test can hold on to.
func (t *TopicBackend) SessionProviderWired() bool {
	return t.sessionProvider != nil
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

	// Turn the query into literal search terms. The LLM extractor bridges a
	// natural-language question to words a topic file would contain, but its
	// phrases are 2-3 words long and rarely appear verbatim (that is what left
	// most recalls empty), so every keyword is ALSO split into its words and —
	// for CJK, which has no word boundaries — into bigrams.
	keywords := []string{query}
	if t.extractor != nil {
		if shouldSkipExtraction(query) {
			t.logger.Info(ctx, "short query — skipping keyword extraction", "query", query)
		} else if kws, err := t.extractor.ExtractKeywords(ctx, query); err == nil && len(kws) > 0 {
			keywords = kws
			t.logger.Info(ctx, "keywords extracted", "keywords", keywords, "query", query)
		} else if err != nil {
			// The raw question still goes through the same expansion below:
			// its words are a poor man's extraction, and searching the whole
			// sentence verbatim (the old behaviour) could never match.
			t.logger.Error(ctx, "keyword extraction failed, falling back to query terms", err)
		}
	}
	terms := buildSearchTerms(keywords)
	if len(terms) == 0 {
		return nil, nil
	}

	// The project domain follows THIS turn's working directory, not the
	// process cwd (see ProjectMemoryDir).
	projectRoot := t.projectRootFor(ctx)

	// Load decay states from last_dream.json in both domains.
	decayStates := t.loadDecayStates(projectRoot)

	var allResults []Entry

	// 1. Search global topics + inbox
	results := t.searchDir(ctx, filepath.Join(t.globalDir, "topics"), terms)
	allResults = append(allResults, results...)
	results = t.searchFile(ctx, filepath.Join(t.globalDir, InboxFile), terms)
	allResults = append(allResults, results...)
	// A running dream has moved its inbox snapshot aside (see dream/inbox.go);
	// recall must still see what is pending consolidation.
	results = t.searchFile(ctx, filepath.Join(t.globalDir, InboxProcessingFile), terms)
	allResults = append(allResults, results...)

	// 2. Search project topics + inbox (if available)
	if projectRoot != "" {
		results = t.searchDir(ctx, filepath.Join(projectRoot, "topics"), terms)
		allResults = append(allResults, results...)
		results = t.searchFile(ctx, filepath.Join(projectRoot, InboxFile), terms)
		allResults = append(allResults, results...)
		results = t.searchFile(ctx, filepath.Join(projectRoot, InboxProcessingFile), terms)
		allResults = append(allResults, results...)
	}

	// Apply memory lifecycle factors from FactState:
	// - Decay multiplier: adjusts score based on time since last reinforcement
	// - Superseded penalty: facts marked superseded by dream are heavily downranked
	// - Reinforcement bonus: facts that have been frequently recalled are boosted
	for i := range allResults {
		if fs, ok := decayStates[allResults[i].ID]; ok {
			decayMultiplier := decayFloor + decayWeight*fs.Decay
			allResults[i].Score *= decayMultiplier

			if fs.Superseded {
				allResults[i].Score -= supersededPenalty
			}

			if fs.Reinforcements >= reinforcementThreshold {
				allResults[i].Score += reinforcementBonus
			}
		}
	}

	// The same fact can be found twice — inbox and a topic file (before the
	// next dream folds one into the other), global and project — and both
	// copies would otherwise be injected. Dedupe AFTER the lifecycle factors
	// so the surviving copy is the one that actually ranks higher.
	allResults = dedupeEntries(allResults)

	// Sort by score descending, truncate to limit.
	sort.Slice(allResults, func(i, j int) bool {
		return allResults[i].Score > allResults[j].Score
	})
	if len(allResults) > limit {
		allResults = allResults[:limit]
	}

	// Session fallback: ONLY when no topic block matched anything. The fallback
	// exists for temporal/navigational queries ("最近聊过什么") that grep can
	// never match — but "no match" is also what an unrelated question looks
	// like, so this is capped and the provider skips the current conversation.
	if len(allResults) == 0 && t.sessionProvider != nil {
		sessionEntries, err := t.fetchRecentSessions(ctx)
		if err != nil {
			t.logger.Error(ctx, "session fallback", err)
		} else if len(sessionEntries) > 0 {
			allResults = sessionEntries
			t.logger.Info(ctx, "session fallback: no topic match, injecting recent sessions",
				"sessions", len(sessionEntries))
		}
	}

	return allResults, nil
}

// fetchRecentSessions retrieves recent session summaries from the SessionProvider
// and converts them to memory Entry format.
// These entries serve as a fallback when keyword-based topic search finds
// nothing, enabling temporal/navigational queries like "what did we talk about
// recently". Each entry includes the session title and the most recent user
// messages for meaningful context about what was discussed.
//
// The provider, not this function, decides WHICH sessions: it knows the
// conversation being served and must leave it out.
func (t *TopicBackend) fetchRecentSessions(ctx context.Context) ([]Entry, error) {
	sessions, err := t.sessionProvider.RecentSessions(ctx, sessionFallbackLimit)
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
		fmt.Fprintf(&content, "Session: %s\nDate: %s\n",
			title, s.CreatedAt.Format(strutil.TimeFormatDateTimeShort))
		for i, msg := range s.RecentMessages {
			if i > 0 {
				content.WriteByte('\n')
			}
			fmt.Fprintf(&content, "  User: %s", msg)
		}
		entries = append(entries, Entry{
			ID:        "session:" + s.ID,
			SessionID: s.ID,
			Summary:   title,
			Content:   content.String(),
			Timestamp: s.CreatedAt.Unix(),
			Score:     sessionFallbackScore,
		})
	}
	return entries, nil
}

// Store appends a direct memory write (from the MemoryRecord tool) to the
// chosen domain's inbox. Memory produced asynchronously by the Dream sub-agent
// does not pass through Store.
//
// Tags become a 关键词 line in the entry. That is not decoration: recall scores
// a hit on the 关键词 line as evidence that the fact was written FOR this kind
// of query, and the old code accepted tags and threw them away.
func (t *TopicBackend) Store(ctx context.Context, content string, tags []string, scope StoreScope) error {
	targetDir, err := t.domainDir(ctx, scope)
	if err != nil {
		return err
	}
	if targetDir == "" {
		return fmt.Errorf("topic: no memory domain for scope %q", scope)
	}
	// Ensure project memory dir exists on first write.
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		return fmt.Errorf("topic: create memory dir: %w", err)
	}

	inboxPath := filepath.Join(targetDir, InboxFile)
	var entry strings.Builder
	fmt.Fprintf(&entry, "\n## %s\n\n", time.Now().Format(time.RFC3339))
	if line := formatTagLine(tags); line != "" {
		entry.WriteString(line)
		entry.WriteString("\n\n")
	}
	entry.WriteString(content)
	entry.WriteString("\n\n---\n")

	f, err := os.OpenFile(inboxPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("topic: open inbox: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(entry.String()); err != nil {
		return fmt.Errorf("topic: write inbox: %w", err)
	}

	t.logger.Info(ctx, "wrote to inbox", "path", inboxPath, "bytes", entry.Len(), "scope", string(scope))
	return nil
}

// domainDir resolves a scope to a directory. Project knowledge asked for
// outside a project is stored globally rather than dropped.
func (t *TopicBackend) domainDir(ctx context.Context, scope StoreScope) (string, error) {
	projectRoot := t.projectRootFor(ctx)
	switch scope {
	case ScopeAuto:
		if projectRoot != "" {
			return projectRoot, nil
		}
		return t.globalDir, nil
	case ScopeGlobal:
		return t.globalDir, nil
	case ScopeProject:
		if projectRoot != "" {
			return projectRoot, nil
		}
		t.logger.Info(context.Background(), "project memory requested outside a git repository — storing globally")
		return t.globalDir, nil
	default:
		return "", fmt.Errorf("topic: unknown memory scope %q", scope)
	}
}

// formatTagLine renders tags as a 关键词 line, dropping blanks and duplicates.
// Capped because the line is a search aid, not a place to dump the request.
func formatTagLine(tags []string) string {
	const maxTags = 8
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(strings.ReplaceAll(tag, "\n", " "))
		if tag == "" {
			continue
		}
		key := strings.ToLower(tag)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, tag)
		if len(out) == maxTags {
			break
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "关键词: " + strings.Join(out, ", ")
}

// --- Search implementation ---

// shouldSkipExtraction reports whether a query is ALREADY usable as a literal
// search string, so the keyword-extraction round-trip can be skipped.
//
// The question is whether the query would match anything verbatim — ripgrep -F
// takes it as one literal — and that turns on the writing system, not on a
// word count:
//
//   - ASCII text is whitespace-segmented, so a short query is a term while a
//     longer one is a sentence ("how do I rotate the api key" appears nowhere).
//     Word count separates those two.
//   - CJK text has NO whitespace boundaries, so word count says nothing about
//     it. "client_id 长什么样" is two whitespace words and a Chinese question:
//     counting words skipped the extraction that would have found "client_id",
//     and the literal query matched nothing. The same count also paid for a
//     round-trip on "domain_by_mi_ids" — one 16-rune token that is already the
//     perfect literal.
//
// So a CJK query is judged by LENGTH instead: a CJK term is a few characters
// and does match verbatim, while past maxLiteralCJKRunes the query is phrasing
// a question whose wording a topic file will not happen to share. The bound is
// a heuristic either way, and it errs toward extracting — an extra round-trip
// costs latency, a missed one costs recall silently.
func shouldSkipExtraction(query string) bool {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return true
	}
	if !strutil.ContainsCJK(trimmed) {
		return len(strings.Fields(trimmed)) <= maxLiteralSearchWords
	}
	return len([]rune(trimmed)) <= maxLiteralCJKRunes
}

// searchTerm is one literal string ripgrep can look for, plus the weight it
// carries in the ranking. Weight is what makes "数据库连接池" (a phrase the fact
// itself is likely to contain) count for more than the bigram "连接" (which
// half the files contain).
type searchTerm struct {
	text   string
	weight float64
}

// buildSearchTerms expands the strings a query produced into the literals to
// search for.
//
// One extracted keyword yields up to three kinds of term:
//   - the phrase itself (heaviest: the most specific thing we were given),
//   - each of its words,
//   - for CJK runs, every two-character slice — Chinese has no word boundaries,
//     so "记忆系统" would otherwise only match a file that spells it exactly.
//
// Duplicates collapse to their heaviest weight, and the list is capped at
// maxSearchTerms. Terms are returned in a stable order (phrases first, then by
// insertion), which keeps the rg command line and the logs readable.
func buildSearchTerms(keywords []string) []searchTerm {
	var terms []searchTerm
	seen := make(map[string]int, len(keywords)*4)

	add := func(text string, weight float64) {
		text = strings.TrimSpace(text)
		key := strings.ToLower(text)
		if text == "" || isRecallStopWord(key) {
			return
		}
		if i, ok := seen[key]; ok {
			if weight > terms[i].weight {
				terms[i].weight = weight
			}
			return
		}
		seen[key] = len(terms)
		terms = append(terms, searchTerm{text: text, weight: weight})
	}

	for _, kw := range keywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		if isAllStopWords(kw) {
			continue
		}
		add(kw, weightPhrase)
		for _, piece := range strings.Fields(kw) {
			piece = strings.Trim(piece, termTrimCutset)
			if piece == "" {
				continue
			}
			if !strutil.ContainsCJK(piece) {
				add(piece, asciiTermWeight(piece))
				continue
			}
			runes := []rune(piece)
			if len(runes) == 1 {
				add(piece, weightShortWord)
				continue
			}
			add(piece, weightCJKTerm)
			for i := 0; i+1 < len(runes); i++ {
				add(string(runes[i:i+2]), weightBigram)
			}
		}
	}

	if len(terms) > maxSearchTerms {
		// Keep the heaviest; stable sort means equal weights keep insertion
		// order, so phrases survive a tie with the words they contain.
		sort.SliceStable(terms, func(i, j int) bool { return terms[i].weight > terms[j].weight })
		terms = terms[:maxSearchTerms]
	}
	return terms
}

// termTrimCutset is punctuation stripped from both ends of a whitespace word:
// the extractor writes phrases like "database, sqlite", and a literal search
// for "database," would miss the plain word.
const termTrimCutset = " \t" + ".,;:!?()[]{}\"'`，。；：！？（）【】《》、“”‘’"

// asciiTermWeight gives long words more weight than two-letter ones; a query
// containing "go" should not outrank one containing "sqlite" on that term.
func asciiTermWeight(word string) float64 {
	if len([]rune(word)) >= 4 {
		return weightWord
	}
	return weightShortWord
}

func isRecallStopWord(word string) bool {
	_, ok := recallStopWords[word]
	return ok
}

// isAllStopWords reports whether a phrase is made only of function words
// ("what did we do") — adding it as a phrase would match every file that
// happens to contain those words, with the phrase's heavy weight.
func isAllStopWords(phrase string) bool {
	sawWord := false
	for _, field := range strings.Fields(phrase) {
		field = strings.Trim(field, termTrimCutset)
		if field == "" {
			continue
		}
		sawWord = true
		if !isRecallStopWord(strings.ToLower(field)) {
			return false
		}
	}
	return sawWord
}

// termTexts is what rg receives: the literals, in weight order.
func termTexts(terms []searchTerm) []string {
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = t.text
	}
	return out
}

// searchDir searches all .md files in a directory for matching blocks.
func (t *TopicBackend) searchDir(ctx context.Context, dir string, terms []searchTerm) []Entry {
	if !fileutil.Exists(dir) {
		return nil
	}

	// Use rg with -e flags for each term (OR logic).
	// -F enables fixed-string matching to avoid regex metacharacter issues.
	args := []string{"-F", "-l", "-i", "--glob", "*.md"}
	for _, term := range termTexts(terms) {
		args = append(args, "-e", term)
	}
	args = append(args, dir)
	t.logger.Info(ctx, "rg", "args", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, t.rgPath, args...)
	out, err := cmd.Output()
	if err != nil {
		// Exit code 1 = no match (normal), other = real error.
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
			return nil
		}
		t.logger.Error(ctx, "rg search failed", err, "dir", dir)
		return nil
	}

	var results []Entry
	for _, f := range strutil.SplitBy(string(out), "\n") {
		entries := t.extractMatchingBlocks(f, terms)
		results = append(results, entries...)
	}
	return results
}

// searchFile searches a single file for matching blocks.
func (t *TopicBackend) searchFile(ctx context.Context, path string, terms []searchTerm) []Entry {
	if !fileutil.Exists(path) {
		return nil
	}
	// Check if file contains any term.
	args := []string{"-F", "-i", "-q"}
	for _, term := range termTexts(terms) {
		args = append(args, "-e", term)
	}
	args = append(args, path)
	t.logger.Info(ctx, "rg", "args", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, t.rgPath, args...)
	if err := cmd.Run(); err != nil {
		return nil // no match
	}
	return t.extractMatchingBlocks(path, terms)
}

// --- Decay state helpers ---

// loadDecayStates returns fact_states from last_dream.json (both domains),
// using an in-memory cache with TTL to avoid repeated disk reads.
// Decay values are recalculated from LastReinforced at read time for accuracy.
func (t *TopicBackend) loadDecayStates(projectRoot string) map[string]*FactState {
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
	for _, dir := range []string{t.globalDir, projectRoot} {
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

	// Recalculate decay in real-time so values stay accurate between dream
	// runs (stored values may be hours/days stale). Facts that were never
	// reinforced decay from their creation time — otherwise they would
	// stay at full strength forever.
	halfLife := float64(t.halfLifeDays) * 24 * 3600
	for _, fs := range result {
		ref := fs.LastReinforced
		if ref.IsZero() {
			ref = fs.CreatedAt
		}
		if ref.IsZero() {
			continue
		}
		elapsed := time.Since(ref).Seconds()
		if elapsed < 0 {
			continue // clock skew — keep stored value
		}
		fs.Decay = math.Exp(-math.Ln2 * elapsed / halfLife)
	}

	// Update cache.
	t.decayCacheMu.Lock()
	t.decayCache = result
	t.decayCacheTime = time.Now()
	t.decayCacheMu.Unlock()

	return result
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

	// Write to temp file first, then rename for atomicity.
	return fileutil.AtomicWriteJSONShared(statePath, &state)
}

// ReinforceFacts strengthens the decay state of the facts a recall returned.
//
// One write per domain: the state file is a whole JSON document, so the old
// per-entry call rewrote it once for every recalled entry — up to five
// read-modify-writes of a file that grows with the memory store, before the
// user's message was even sent.
func (t *TopicBackend) ReinforceFacts(ctx context.Context, entryIDs []string) error {
	ids := filterFactIDs(entryIDs)
	if len(ids) == 0 {
		return nil
	}

	t.reinforceMu.Lock()
	defer t.reinforceMu.Unlock()

	now := time.Now()
	for _, dir := range []string{t.globalDir, t.projectRootFor(ctx)} {
		if dir == "" {
			continue
		}
		statePath := filepath.Join(dir, DreamStateFile)
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			continue
		}

		// Check which ids this domain owns before incurring write I/O.
		states := loadFactStatesFromFile(dir)
		var touched []string
		for _, id := range ids {
			if _, ok := states[id]; ok {
				touched = append(touched, id)
			}
		}
		if len(touched) == 0 {
			continue
		}

		err := writeFactStates(dir, func(states map[string]*FactState) {
			for _, id := range touched {
				fs := states[id]
				if fs == nil {
					continue
				}
				fs.Reinforcements++
				fs.LastReinforced = now
				fs.Decay = 1.0
			}
		})
		if err != nil {
			t.logger.Error(ctx, "ReinforceFacts: write", err, "path", statePath)
		}
	}

	// The cache is what Recall ranks by. Without this invalidation a fact
	// reinforced on this turn kept its old decay for up to decayCacheTTL.
	t.decayCacheMu.Lock()
	t.decayCache = nil
	t.decayCacheMu.Unlock()

	return nil
}

// ReinforceFact is the single-id convenience wrapper.
func (t *TopicBackend) ReinforceFact(ctx context.Context, entryID string) error {
	return t.ReinforceFacts(ctx, []string{entryID})
}

// filterFactIDs drops ids that cannot be facts: empty ones and session-fallback
// entries ("session:<id>"), which have no FactState and would only make the
// per-domain scans do work.
func filterFactIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" || strings.HasPrefix(id, "session:") {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (t *TopicBackend) extractMatchingBlocks(path string, terms []searchTerm) []Entry {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	blocks := SplitByHR(string(content))
	var entries []Entry

	filename := filepath.Base(path)

	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		matched, score := scoreBlock(block, terms)
		if !matched {
			continue
		}

		entry := Entry{
			ID:      FactID(filename, block),
			Summary: extractTitle(block),
			Content: strutil.Truncate(block, 1000),
			Score:   score,
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

// scoreBlock reports whether a block matches any search term and, if so, how
// well it answers the query.
//
// The score is COVERAGE: the share of the query's total term weight the block
// contains, plus small bonuses for a hit in the title, in the 关键词 line, and
// for recency. Coverage replaced the old fixed 0.5 base, which rated one
// incidental match ("memory" in a file about something else) exactly as high as
// a fact that answered the whole question — leaving ranking to the bonuses.
//
// Memory lifecycle factors (decay, superseded status, reinforcement count) are
// applied separately in Recall() from authoritative FactState data.
func scoreBlock(block string, terms []searchTerm) (bool, float64) {
	blockLower := strings.ToLower(block)
	titleLower := strings.ToLower(extractTitle(block))

	var total, matched float64
	titleHit := false
	keywordLineHit := false
	recencyHit := false
	// strong says at least one WHOLE term matched (a phrase, a CJK term or a
	// real word) rather than only a fragment of one. A block sharing just the
	// bigram "实现" with a question is noise, and injecting it hides the fact
	// that nothing was actually found.
	strong := false
	ts := extractTimestamp(block)
	if ts > 0 {
		recencyHit = time.Since(time.Unix(ts, 0)) < recentWindow
	}

	for _, term := range terms {
		total += term.weight
		termLower := strings.ToLower(term.text)
		if !strings.Contains(blockLower, termLower) {
			continue
		}
		matched += term.weight
		if term.weight >= weightWord {
			strong = true
		}
		if !titleHit && strings.Contains(titleLower, termLower) {
			titleHit = true
		}
		if !keywordLineHit && matchesKeywordLine(blockLower, termLower) {
			keywordLineHit = true
		}
	}

	if matched == 0 || !strong || total <= 0 {
		return false, 0
	}

	score := coverageWeight * matched / total
	if titleHit {
		score += titleBonus
	}
	if keywordLineHit {
		score += keywordLineBonus
	}
	// Recency bonus: if timestamp is recent (within recentWindow).
	if recencyHit {
		score += recencyBonus
	}

	return true, math.Min(score, 1.0)
}

// dedupeEntries keeps one copy of each distinct fact, by CONTENT.
//
// The ID cannot be the key: it includes the file name, so the same fact sitting
// in inbox.md and in a topic file (or in both memory domains) has two IDs. The
// surviving copy is the higher-scoring one.
func dedupeEntries(entries []Entry) []Entry {
	if len(entries) < 2 {
		return entries
	}
	seen := make(map[[sha256.Size]byte]int, len(entries))
	out := entries[:0]
	for _, e := range entries {
		key := sha256.Sum256([]byte(strings.TrimSpace(e.Content)))
		if i, ok := seen[key]; ok {
			if e.Score > out[i].Score {
				out[i] = e
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, e)
	}
	return out
}

// matchesKeywordLine reports whether the query term appears in a "关键词:" or
// "Keywords:" line. Both arguments must already be lowercase.
func matchesKeywordLine(blockLower, queryLower string) bool {
	for line := range strings.SplitSeq(blockLower, "\n") {
		if (strings.HasPrefix(line, "关键词:") || strings.HasPrefix(line, "keywords:")) &&
			strings.Contains(line, queryLower) {
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

// extractTitle returns a block's entry heading, preferring "## " over "# ".
//
// The preference is by HEADING LEVEL, not by position, and that distinction is
// the whole point: SplitByHR cuts on horizontal rules, and a topic file puts no
// rule between its "# <file>" heading and its first "## <entry>", so the first
// entry of every file shares a block with the file preamble. Scanning line by
// line and returning whichever header came first gave that entry the FILE's
// name — both as the Summary the model is shown and as the title the keyword
// bonus scores against, so a keyword in the entry's own title earned nothing.
//
// The merged block itself is deliberately left alone: FactID and the decay
// scan (dream/decay.go splits files with the same SplitByHR) both key off these
// boundaries, so re-cutting them would re-key every existing fact's decay
// state.
func extractTitle(block string) string {
	for _, prefix := range []string{"## ", "# "} {
		for line := range strings.SplitSeq(block, "\n") {
			if after, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
				return after
			}
		}
	}
	// No header found — use first non-empty line truncated.
	for line := range strings.SplitSeq(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			if len(trimmed) > 80 {
				return strutil.Truncate(trimmed, 80)
			}
			return trimmed
		}
	}
	return ""
}

// extractTimestamp returns the date a fact block carries, or 0.
//
// It reads only where the memory format puts a date — the entry heading
// ("## 2026-06-10: …") and metadata lines ("Date:", "日期:", "来源:") — instead
// of scanning the whole block for "202". The old scan could take a year out of
// ordinary prose ("released in 2024") and treat it as the fact's timestamp,
// which feeds the recency bonus.
func extractTimestamp(block string) int64 {
	for line := range strings.SplitSeq(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "## "); ok {
			if ts, ok := parseDatePrefix(rest); ok {
				return ts
			}
			continue
		}
		lower := strings.ToLower(trimmed)
		for _, prefix := range []string{"date:", "日期:", "来源:", "source:"} {
			if !strings.HasPrefix(lower, prefix) {
				continue
			}
			// Prefixes are ASCII, so the byte offset is the same in `trimmed`.
			if ts, ok := parseDatePrefix(strings.TrimSpace(trimmed[len(prefix):])); ok {
				return ts
			}
		}
	}
	return 0
}

// parseDatePrefix parses a date at the START of s, in the forms the memory
// format writes: RFC3339 ("2026-06-19T22:32:01+08:00"), a local timestamp
// ("2026-06-10T09:00:00"), or a plain date ("2026-06-10").
func parseDatePrefix(s string) (int64, bool) {
	if len(s) >= len(time.RFC3339) {
		if t, err := time.Parse(time.RFC3339, s[:len(time.RFC3339)]); err == nil {
			return t.Unix(), true
		}
	}
	if t, err := time.Parse("2006-01-02T15:04:05", s[:min(len(s), len("2006-01-02T15:04:05"))]); err == nil {
		return t.Unix(), true
	}
	if len(s) >= len(strutil.TimeFormatDate) {
		if t, err := time.Parse(strutil.TimeFormatDate, s[:len(strutil.TimeFormatDate)]); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}

// --- Utility functions ---

// ProjectMemoryDir returns the project memory directory for a working
// directory: <git root>/.tachi/memory, or "" when the directory is not inside
// a git repository.
func ProjectMemoryDir(workDir string) string {
	root := findGitRoot(workDir)
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".tachi", "memory")
}

// projectRootFor resolves the project memory directory for ONE call.
//
// The turn's own working directory wins when the context carries one (every
// frontend binds it — desktop, ACP, subagents): resolving from the PROCESS cwd
// made project memory silently vanish wherever the process did not run inside
// the session's repository, which is every Finder-launched desktop app. The
// directory the backend was built in is only the fallback for a call with no
// bound directory (a CLI one-off, a test).
func (t *TopicBackend) projectRootFor(ctx context.Context) string {
	if dir, ok := wdctx.MaybeDir(ctx); ok {
		return ProjectMemoryDir(dir)
	}
	return t.projectDir
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
