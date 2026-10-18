package memory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

func setupTopicBackend(t *testing.T) (*TopicBackend, string) {
	t.Helper()
	requireRipgrep(t)

	tmpDir := t.TempDir()

	backend, err := NewTopicBackend(config.MemoryConfig{
		BaseDir: tmpDir,
	}, nil)
	if err != nil {
		t.Fatalf("NewTopicBackend: %v", err)
	}
	// Override projectDir for testing.
	backend.projectDir = ""

	return backend, tmpDir
}

// requireRipgrep skips tests when ripgrep (rg) is not available in PATH.
// This avoids failures in CI environments that don't have rg installed.
func requireRipgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not found in PATH — skipping test")
	}
}

func TestTopicBackend_StoreDirectContent(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t)
	ctx := t.Context()

	err := backend.Store(ctx, "用户偏好：代码注释用英文", nil, ScopeAuto)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Verify inbox.md was created.
	inboxPath := filepath.Join(tmpDir, "memory", "inbox.md")
	content, err := os.ReadFile(inboxPath)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}

	if !strings.Contains(string(content), "代码注释用英文") {
		t.Errorf("inbox should contain stored content, got: %s", content)
	}
	if !strings.Contains(string(content), "---") {
		t.Error("inbox entry should end with HR separator")
	}
}

func TestTopicBackend_Recall_TopicFiles(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t)
	ctx := t.Context()

	// Create a topic file with content.
	topicsDir := filepath.Join(tmpDir, "memory", "topics")
	os.MkdirAll(topicsDir, 0755)

	topicContent := `# Design Decisions

## 2026-06-10: 选择了 iter 包做流式处理

来源: session 2026-06-10-abc123
状态: active
关键词: iter, stream, GC, channel

选择 iter 包因为 GC 压力减少了 30%。

---

## 2026-06-08: 数据库选型用 SQLite

来源: session 2026-06-08-def456
状态: superseded
关键词: database, sqlite, postgresql

当时认为 SQLite 够用，后来改了。

---
`
	os.WriteFile(filepath.Join(topicsDir, "design-decisions.md"), []byte(topicContent), 0644)

	// Search for "iter".
	results, err := backend.Recall(ctx, "iter", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 result for 'iter'")
	}

	// Should find the iter block.
	found := false
	for _, r := range results {
		if strings.Contains(r.Content, "iter") {
			found = true
			if r.Score <= 0 {
				t.Error("score should be positive")
			}
		}
	}
	if !found {
		t.Error("expected to find 'iter' in results")
	}
}

func TestTopicBackend_Recall_SupersededPenalty(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t)
	ctx := t.Context()

	topicsDir := filepath.Join(tmpDir, "memory", "topics")
	os.MkdirAll(topicsDir, 0755)

	topicContent := `# Decisions

## Active fact

状态: active
关键词: database

This is the current decision about database.

---

## Old fact

状态: superseded
关键词: database

This was overridden.

---
`
	os.WriteFile(filepath.Join(topicsDir, "test.md"), []byte(topicContent), 0644)

	// Compute FactIDs and create last_dream.json with superseded state.
	// The superseded penalty is now applied from authoritative FactState data
	// (not text matching), so we need to set up the state file.
	blocks := SplitByHR(topicContent)
	var activeID, supersededID string
	for _, b := range blocks {
		b = strings.TrimSpace(b)
		if strings.Contains(b, "current decision") {
			activeID = FactID("test.md", b)
		}
		if strings.Contains(b, "overridden") {
			supersededID = FactID("test.md", b)
		}
	}
	if activeID == "" || supersededID == "" {
		t.Fatal("could not find fact IDs")
	}

	memoryDir := filepath.Join(tmpDir, "memory")
	os.MkdirAll(memoryDir, 0755)
	stateJSON := fmt.Sprintf(`{
  "last_dream_at": "2026-06-16T00:00:00Z",
  "sessions_dreamed": 1,
  "fact_states": {
    "%s": {
      "id": "%s",
      "topic_file": "test.md",
      "decay": 1.0,
      "reinforcements": 0,
      "last_reinforced": "2026-06-16T00:00:00Z",
      "created_at": "2026-06-16T00:00:00Z",
      "superseded": false
    },
    "%s": {
      "id": "%s",
      "topic_file": "test.md",
      "decay": 0.5,
      "reinforcements": 0,
      "last_reinforced": "2026-06-09T00:00:00Z",
      "created_at": "2026-06-09T00:00:00Z",
      "superseded": true
    }
  }
}`, activeID, activeID, supersededID, supersededID)
	os.WriteFile(filepath.Join(memoryDir, DreamStateFile), []byte(stateJSON), 0644)

	results, err := backend.Recall(ctx, "database", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Active should score higher than superseded
	// (superseded gets -0.3 penalty + lower decay multiplier).
	var activeScore, supersededScore float64
	for _, r := range results {
		if strings.Contains(r.Content, "current decision") {
			activeScore = r.Score
		}
		if strings.Contains(r.Content, "overridden") {
			supersededScore = r.Score
		}
	}

	if activeScore <= supersededScore {
		t.Errorf("active (%f) should score higher than superseded (%f)", activeScore, supersededScore)
	}
}

func TestTopicBackend_Recall_Inbox(t *testing.T) {
	backend, _ := setupTopicBackend(t)
	ctx := t.Context()

	// Write directly to inbox.
	err := backend.Store(ctx, "记住：部署用 make deploy", nil, ScopeAuto)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Should be searchable immediately.
	results, err := backend.Recall(ctx, "deploy", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected to find 'deploy' in inbox")
	}

	found := false
	for _, r := range results {
		if strings.Contains(r.Content, "make deploy") {
			found = true
		}
	}
	if !found {
		t.Error("expected inbox content in results")
	}
}

func TestTopicBackend_Recall_EmptyQuery(t *testing.T) {
	backend, _ := setupTopicBackend(t)
	ctx := t.Context()

	results, err := backend.Recall(ctx, "", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("empty query should return 0 results, got %d", len(results))
	}
}

func TestTopicBackend_Recall_NoMatch(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t)
	ctx := t.Context()

	topicsDir := filepath.Join(tmpDir, "memory", "topics")
	os.MkdirAll(topicsDir, 0755)
	os.WriteFile(filepath.Join(topicsDir, "test.md"), []byte("# Hello\n\nworld\n\n---\n"), 0644)

	results, err := backend.Recall(ctx, "zzzznonexistent", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for non-matching query, got %d", len(results))
	}
}

func TestTopicBackend_Recall_DualDomain(t *testing.T) {
	requireRipgrep(t)
	tmpDir := t.TempDir()
	globalDir := filepath.Join(tmpDir, "global", "memory")
	projectDir := filepath.Join(tmpDir, "project", "memory")

	os.MkdirAll(filepath.Join(globalDir, "topics"), 0755)
	os.MkdirAll(filepath.Join(projectDir, "topics"), 0755)

	// Write different content to each domain.
	os.WriteFile(
		filepath.Join(globalDir, "topics", "prefs.md"),
		[]byte("# Prefs\n\n## User likes vim\n\n关键词: vim, editor\n\nvim is preferred.\n\n---\n"),
		0644,
	)
	os.WriteFile(
		filepath.Join(projectDir, "topics", "arch.md"),
		[]byte("# Arch\n\n## Uses vim keybindings in TUI\n\n关键词: vim, tui\n\nbubbletea with vim keys.\n\n---\n"),
		0644,
	)

	rgPath, _ := exec.LookPath("rg")
	backend := &TopicBackend{
		globalDir:  globalDir,
		projectDir: projectDir,
		rgPath:     rgPath,
		logger:     logger.Default().With("source", "test"),
	}

	results, err := backend.Recall(t.Context(), "vim", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	// Should find results from both domains.
	if len(results) < 2 {
		t.Errorf("expected results from both domains, got %d", len(results))
	}
}

func TestSplitByHR(t *testing.T) {
	content := `# Title

## Block 1

Some content here.

---

## Block 2

More content.

---

## Block 3

Final block.`

	blocks := SplitByHR(content)
	if len(blocks) != 3 {
		t.Errorf("expected 3 blocks, got %d", len(blocks))
		for i, b := range blocks {
			t.Logf("block %d: %q", i, b)
		}
	}
}

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		block string
		want  string
	}{
		{"## My Title\n\nContent", "My Title"},
		{"# Top Level\n\nContent", "Top Level"},
		{"No header\njust text", "No header"},
		{"", ""},

		// A topic file has no horizontal rule between its "# <file>" heading
		// and its first "## <entry>", so SplitByHR hands the first entry to
		// extractTitle welded to the file preamble. The entry's heading must
		// win on LEVEL even though the file's comes first — otherwise the
		// first entry of every file is labelled with the file's name.
		{"# MCP 规范\n\n## 2026-07-01: DCR 查询\n\n关键词: MCP\n\n正文", "2026-07-01: DCR 查询"},
		// ...and with no H2 at all the file heading is the right answer.
		{"# Only A File Heading\n\nSome prose", "Only A File Heading"},
	}

	for _, tt := range tests {
		got := extractTitle(tt.block)
		if got != tt.want {
			t.Errorf("extractTitle(%q) = %q, want %q", tt.block[:min(len(tt.block), 20)], got, tt.want)
		}
	}
}

func TestScoreBlock(t *testing.T) {
	// The score is coverage of the query's terms plus small bonuses; lifecycle
	// factors (decay, superseded, reinforcements) are applied in Recall().

	// A hit in the title must outrank the same term in the body only.
	terms := buildSearchTerms([]string{"database"})
	withTitle := "## Database\n\n关键词: storage\n\nWe chose SQLite."
	bodyOnly := "## Storage\n\n关键词: sql\n\nWe used a database."

	okTitle, titleScore := scoreBlock(withTitle, terms)
	okBody, bodyScore := scoreBlock(bodyOnly, terms)
	if !okTitle || !okBody {
		t.Fatal("both blocks contain the term and must match")
	}
	if titleScore <= bodyScore {
		t.Errorf("title hit (%f) should outrank body-only (%f)", titleScore, bodyScore)
	}

	// The core fix: a block that answers the WHOLE query must outrank one that
	// shares a single word with it. Under the old fixed base both scored 0.5.
	multi := buildSearchTerms([]string{"sqlite 连接池"})
	full := "## 数据库\n\n关键词: sqlite, 连接池\n\n我们用 SQLite 的连接池。"
	partial := "## 数据库\n\n关键词: sqlite\n\n我们用 SQLite。"
	okFull, fullScore := scoreBlock(full, multi)
	okPartial, partialScore := scoreBlock(partial, multi)
	if !okFull || !okPartial {
		t.Fatal("both blocks contain at least one term")
	}
	if fullScore <= partialScore {
		t.Errorf("full coverage (%f) should outrank partial (%f)", fullScore, partialScore)
	}

	// No term → no result.
	if ok, _ := scoreBlock("## Unrelated\n\nNothing here.", terms); ok {
		t.Error("a block with no matching term must not match")
	}
}

func TestBuildSearchTerms(t *testing.T) {
	// A CJK phrase has no word boundaries: the phrase alone would only match a
	// file that spells it exactly, so its bigrams are what make recall work.
	terms := buildSearchTerms([]string{"记忆系统"})
	texts := termTexts(terms)
	want := []string{"记忆系统", "记忆", "系统"}
	for _, w := range want {
		if !slices.Contains(texts, w) {
			t.Errorf("terms %v missing %q", texts, w)
		}
	}

	// A multi-word phrase keeps the phrase AND its words.
	terms = buildSearchTerms([]string{"memory system"})
	texts = termTexts(terms)
	if !slices.Contains(texts, "memory system") || !slices.Contains(texts, "memory") || !slices.Contains(texts, "system") {
		t.Errorf("phrase expansion: got %v", texts)
	}

	// Function words are dropped: they match everything and rank nothing.
	terms = buildSearchTerms([]string{"what did we do"})
	if len(terms) != 0 {
		t.Errorf("stop words should produce no terms, got %v", termTexts(terms))
	}

	// Duplicates collapse (case-insensitively) and the phrase's heavy weight
	// survives a later, lighter mention.
	terms = buildSearchTerms([]string{"SQLite", "sqlite"})
	if len(terms) != 1 {
		t.Errorf("duplicate terms should collapse, got %v", termTexts(terms))
	}
}

func TestTopicBackend_Recall_DecayWeighting(t *testing.T) {
	ctx := t.Context()

	// Create topic content that two independent backends can share.
	topicContent := `# DB

## Database Choice

状态: active
关键词: database, sqlite

We chose SQLite.

---
`

	// Compute the fact ID that extractMatchingBlocks will produce.
	blocks := SplitByHR(topicContent)
	var factID string
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block != "" && strings.Contains(block, "SQLite") {
			factID = FactID("db.md", block)
			break
		}
	}
	if factID == "" {
		t.Fatal("could not find fact ID")
	}

	// --- Backend A: no decay data (baseline) ---
	dirA := t.TempDir()
	topicsA := filepath.Join(dirA, "memory", "topics")
	os.MkdirAll(topicsA, 0755)
	os.WriteFile(filepath.Join(topicsA, "db.md"), []byte(topicContent), 0644)

	backendA, err := NewTopicBackend(config.MemoryConfig{BaseDir: dirA}, nil)
	if err != nil {
		t.Fatalf("NewTopicBackend A: %v", err)
	}
	backendA.projectDir = ""

	resultsA, err := backendA.Recall(ctx, "database", 10)
	if err != nil {
		t.Fatalf("Recall A: %v", err)
	}
	if len(resultsA) == 0 {
		t.Fatal("backend A: expected at least 1 result")
	}
	normalScore := resultsA[0].Score
	t.Logf("score without decay data: %f", normalScore)

	// --- Backend B: with decay data (last_dream.json written BEFORE creation) ---
	dirB := t.TempDir()
	memoryB := filepath.Join(dirB, "memory")
	topicsB := filepath.Join(memoryB, "topics")
	os.MkdirAll(topicsB, 0755)
	os.WriteFile(filepath.Join(topicsB, "db.md"), []byte(topicContent), 0644)

	stateJSON := fmt.Sprintf(`{
  "last_dream_at": "2026-06-01T00:00:00Z",
  "sessions_dreamed": 1,
  "fact_states": {
    "%s": {
      "id": "%s",
      "topic_file": "db.md",
      "decay": 0.1,
      "reinforcements": 0,
      "last_reinforced": "2026-05-01T00:00:00Z",
      "created_at": "2026-05-01T00:00:00Z",
      "superseded": false
    }
  }
}`, factID, factID)
	os.WriteFile(filepath.Join(memoryB, DreamStateFile), []byte(stateJSON), 0644)

	backendB, err := NewTopicBackend(config.MemoryConfig{BaseDir: dirB}, nil)
	if err != nil {
		t.Fatalf("NewTopicBackend B: %v", err)
	}
	backendB.projectDir = ""

	resultsB, err := backendB.Recall(ctx, "database", 10)
	if err != nil {
		t.Fatalf("Recall B: %v", err)
	}
	if len(resultsB) == 0 {
		t.Fatal("backend B: expected at least 1 result with decay data")
	}
	decayedScore := resultsB[0].Score
	t.Logf("score with decay=0.1: %f (vs normal: %f)", decayedScore, normalScore)

	if decayedScore >= normalScore {
		t.Errorf("decayed score (%f) should be lower than normal score (%f)", decayedScore, normalScore)
	}

	// Expected decay multiplier: 0.3 + 0.7*0.1 = 0.37
	expectedRatio := decayedScore / normalScore
	if expectedRatio < 0.30 || expectedRatio > 0.44 {
		t.Errorf("decay ratio should be ~0.37, got %f", expectedRatio)
	}
}

func TestTopicBackend_ReinforceFact(t *testing.T) {
	tmpDir := t.TempDir()
	globalDir := filepath.Join(tmpDir, "memory")

	rgPath, _ := exec.LookPath("rg")
	backend := &TopicBackend{
		globalDir:  globalDir,
		projectDir: "",
		rgPath:     rgPath,
		logger:     logger.Default().With("source", "test"),
	}

	// Create a last_dream.json with a fact that has low decay.
	factID := "topic:test.md:abc12345"
	stateJSON := fmt.Sprintf(`{
  "last_dream_at": "2026-06-01T00:00:00Z",
  "sessions_dreamed": 1,
  "fact_states": {
    "%s": {
      "id": "%s",
      "topic_file": "test.md",
      "decay": 0.3,
      "reinforcements": 2,
      "last_reinforced": "2026-05-30T00:00:00Z",
      "created_at": "2026-05-01T00:00:00Z",
      "superseded": false
    }
  }
}`, factID, factID)
	os.MkdirAll(globalDir, 0755)
	os.WriteFile(filepath.Join(globalDir, DreamStateFile), []byte(stateJSON), 0644)

	// Reinforce the fact.
	if err := backend.ReinforceFact(t.Context(), factID); err != nil {
		t.Fatalf("ReinforceFact: %v", err)
	}

	// Reload and verify reinforcement.
	states := loadFactStatesFromFile(globalDir)
	fs, ok := states[factID]
	if !ok {
		t.Fatal("fact not found after reinforcement")
	}
	if fs.Reinforcements != 3 {
		t.Errorf("Reinforcements: expected 3, got %d", fs.Reinforcements)
	}
	if fs.Decay != 1.0 {
		t.Errorf("Decay: expected 1.0 after reinforcement, got %f", fs.Decay)
	}
	if fs.LastReinforced.Before(time.Now().Add(-5 * time.Second)) {
		t.Error("LastReinforced should be recent")
	}

	// Verify other state fields are preserved.
	data, _ := os.ReadFile(filepath.Join(globalDir, DreamStateFile))
	if !strings.Contains(string(data), `"last_dream_at"`) {
		t.Error("last_dream.json should still have last_dream_at field")
	}
	if !strings.Contains(string(data), `"sessions_dreamed": 1`) {
		t.Error("last_dream.json should preserve sessions_dreamed")
	}
}

func TestTopicBackend_ReinforceFact_MissingFact(t *testing.T) {
	tmpDir := t.TempDir()
	globalDir := filepath.Join(tmpDir, "memory")
	os.MkdirAll(globalDir, 0755)

	// No last_dream.json exists.
	rgPath, _ := exec.LookPath("rg")
	backend := &TopicBackend{
		globalDir:  globalDir,
		projectDir: "",
		rgPath:     rgPath,
		logger:     logger.Default().With("source", "test"),
	}

	// Should not error on missing fact/file.
	if err := backend.ReinforceFact(t.Context(), "topic:nonexistent:00000000"); err != nil {
		t.Errorf("ReinforceFact on missing fact should not error: %v", err)
	}
}

// --- Keyword extraction skip ---

// TestTopicBackend_Recall_MixedCJKQueryUsesTheExtractor is the companion to
// the short-query test: a mixed CJK/ASCII query is a phrase ripgrep cannot
// match literally, so it MUST reach the extractor. The old word-count rule
// skipped it — "client_id 长什么样" is two whitespace words — and the search
// then ran on the whole phrase and found nothing.
func TestTopicBackend_Recall_MixedCJKQueryUsesTheExtractor(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t)
	ctx := t.Context()

	topicsDir := filepath.Join(tmpDir, "memory", "topics")
	os.MkdirAll(topicsDir, 0755)
	os.WriteFile(filepath.Join(topicsDir, "iam.md"), []byte(`## client_id 格式

关键词: client_id, 十六进制

client_id 是 16 位十六进制字符串。

---
`), 0644)

	// The extractor returns the term the phrase actually contains, which the
	// literal query could never match.
	ext := &countingExtractor{kws: []string{"client_id"}}
	backend.SetKeywordExtractor(ext)

	results, err := backend.Recall(ctx, "client_id 长什么样", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if ext.calls != 1 {
		t.Fatalf("extractor calls = %d, want 1 — a mixed CJK query is not a literal", ext.calls)
	}
	if len(results) == 0 {
		t.Error("expected the entry the extracted keyword matches, got none")
	}
}

// countingExtractor counts ExtractKeywords calls and returns fixed keywords.
type countingExtractor struct {
	calls int
	kws   []string
}

func (e *countingExtractor) ExtractKeywords(ctx context.Context, query string) ([]string, error) {
	e.calls++
	return e.kws, nil
}

func TestShouldSkipExtraction(t *testing.T) {
	skip := []string{
		"",
		"   ",
		"database",
		"dark theme",
		"db connection pool",
		"sqlite 配置",
		"连接池",
		"数据库连接池配置", // 8 runes — still keyword-like
	}
	for _, q := range skip {
		if !shouldSkipExtraction(q) {
			t.Errorf("expected skip for %q", q)
		}
	}

	useLLM := []string{
		"what database did we choose for the project", // more than 3 words
		"我们之前讨论过的数据库连接池配置是什么",                         // long CJK sentence, no spaces

		// Mixed CJK/ASCII phrases. These are the entries the old word-count
		// rule got wrong in BOTH directions, so each one is pinned here:
		// a query whose literal form cannot appear must reach the extractor,
		// and an already-perfect literal must not spend a round-trip.
		"client_id 长什么样",      // 2 whitespace words — the count said "term"
		"域账号和 MI_ID 之间怎么互相查",  // 3 words
		"MCP 服务重启之后工具列表怎么刷新",  // 2 words
		"上次排查的那个踢登问题",         // 1 word, 11 runes — one past the bound
		"怎么查某个人的登录记录",         // 1 word, 11 runes
		"memory decay 是怎么实现的", // 3 words
		"批量给用户开通 MCP 权限的脚本",   // 3 words
	}
	for _, q := range useLLM {
		if shouldSkipExtraction(q) {
			t.Errorf("expected LLM extraction for %q", q)
		}
	}

	// The other direction: a long token with no CJK is a single literal, not a
	// sentence — the old rule extracted for it purely because it exceeded 12
	// runes, spending a call on a query that was already the ideal search term.
	for _, q := range []string{"domain_by_mi_ids", "get_workspace_users", "notifications/tools/list_changed"} {
		if !shouldSkipExtraction(q) {
			t.Errorf("expected skip for %q: it is one literal, not a sentence", q)
		}
	}
}

func TestTopicBackend_Recall_ShortQuerySkipsExtractor(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t)
	ctx := t.Context()

	topicsDir := filepath.Join(tmpDir, "memory", "topics")
	os.MkdirAll(topicsDir, 0755)
	os.WriteFile(filepath.Join(topicsDir, "db.md"), []byte(`## Database

关键词: sqlite, database

We chose SQLite.

---
`), 0644)

	// Extractor returns keywords that match nothing — if a search is driven
	// by these keywords, results must be empty.
	ext := &countingExtractor{kws: []string{"unrelated"}}
	backend.SetKeywordExtractor(ext)

	// Short query: extractor must not be called; the raw query drives search.
	results, err := backend.Recall(ctx, "sqlite", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if ext.calls != 0 {
		t.Errorf("extractor should not be called for short query, got %d calls", ext.calls)
	}
	if len(results) == 0 {
		t.Error("expected results from raw-query search")
	}

	// Long query: extractor is called and its (non-matching) keywords drive
	// the search — proving the raw query was replaced.
	results, err = backend.Recall(ctx, "what database technology did we choose for this project", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if ext.calls != 1 {
		t.Errorf("extractor should be called once for long query, got %d", ext.calls)
	}
	if len(results) != 0 {
		t.Errorf("expected no results with extractor keywords, got %d", len(results))
	}
}

// The failure this path was fixed for: the extractor returns 2-3 word phrases
// that never appear verbatim, so searching ONLY the phrases found nothing and
// recall fell back to unrelated sessions on 72% of turns.
func TestTopicBackend_Recall_SplitsExtractedPhrases(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t)
	ctx := t.Context()

	topicsDir := filepath.Join(tmpDir, "memory", "topics")
	os.MkdirAll(topicsDir, 0755)
	os.WriteFile(filepath.Join(topicsDir, "db.md"), []byte(`# DB

## 数据库选型

状态: active
关键词: sqlite, database

这个项目用 SQLite 作为数据库。

---
`), 0644)

	// Neither "database connection pool" nor "database technology" appears in
	// the file; "database" does.
	backend.SetKeywordExtractor(&countingExtractor{kws: []string{"database connection pool"}})

	results, err := backend.Recall(ctx, "what database technology did we choose for this project", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected the word 'database' from the extracted phrase to match the topic")
	}
	if !strings.Contains(results[0].Content, "SQLite") {
		t.Errorf("wrong entry returned: %q", results[0].Content)
	}
}

// The same fact can sit in inbox and a topic file at once (dream has not folded
// it in yet); injecting it twice wastes context and inflates its apparent
// confidence.
func TestTopicBackend_Recall_DedupesIdenticalContent(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t)
	ctx := t.Context()

	block := "## Shared fact\n\n关键词: sqlite\n\nWe chose SQLite for the project.\n"
	topicsDir := filepath.Join(tmpDir, "memory", "topics")
	os.MkdirAll(topicsDir, 0755)
	os.WriteFile(filepath.Join(topicsDir, "db.md"), []byte(block+"\n---\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "memory", "inbox.md"), []byte("\n"+block+"\n---\n"), 0644)

	results, err := backend.Recall(ctx, "sqlite", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected the duplicate to collapse to 1 result, got %d", len(results))
	}
}

// Scope decides which inbox a direct write lands in. Before scope existed every
// write in a git repo landed in that project's inbox, so a user preference was
// invisible everywhere else.
func TestTopicBackend_StoreScopeAndTags(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t)
	ctx := t.Context()
	backend.projectDir = filepath.Join(tmpDir, "project-memory")

	globalInbox := filepath.Join(tmpDir, "memory", InboxFile)
	projectInbox := filepath.Join(backend.projectDir, InboxFile)

	if err := backend.Store(ctx, "用户偏好：回答尽量简短", []string{"preference", " style ", "preference"}, ScopeGlobal); err != nil {
		t.Fatalf("Store(global): %v", err)
	}
	globalBody, err := os.ReadFile(globalInbox)
	if err != nil {
		t.Fatalf("read global inbox: %v", err)
	}
	if !strings.Contains(string(globalBody), "回答尽量简短") {
		t.Fatal("global write did not land in the global inbox")
	}
	// tags become a searchable 关键词 line, deduped.
	if !strings.Contains(string(globalBody), "关键词: preference, style") {
		t.Fatalf("tags should be written as a 关键词 line, got: %s", globalBody)
	}
	if _, err := os.Stat(projectInbox); !os.IsNotExist(err) {
		t.Fatal("a global write must not create the project inbox")
	}

	if err := backend.Store(ctx, "本项目部署用 make deploy", nil, ScopeProject); err != nil {
		t.Fatalf("Store(project): %v", err)
	}
	if _, err := os.Stat(projectInbox); err != nil {
		t.Fatal("project write did not land in the project inbox")
	}

	// Auto keeps the old rule: project when one exists.
	if err := backend.Store(ctx, "自动选择", nil, ScopeAuto); err != nil {
		t.Fatalf("Store(auto): %v", err)
	}
	projectBody, _ := os.ReadFile(projectInbox)
	if !strings.Contains(string(projectBody), "自动选择") {
		t.Fatal("auto write should go to the project inbox when a project exists")
	}

	if err := backend.Store(ctx, "x", nil, StoreScope("bogus")); err == nil {
		t.Fatal("unknown scope must be rejected")
	}
}

// Project scope outside a repository must not drop the memory: it has nowhere
// else to go, so it goes global.
func TestTopicBackend_StoreProjectScopeOutsideRepo(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t) // projectDir is "" here
	if err := backend.Store(t.Context(), "项目事实", nil, ScopeProject); err != nil {
		t.Fatalf("Store(project): %v", err)
	}
	body, err := os.ReadFile(filepath.Join(tmpDir, "memory", InboxFile))
	if err != nil || !strings.Contains(string(body), "项目事实") {
		t.Fatalf("project write outside a repo should fall back to global (err=%v)", err)
	}
}

func TestExtractTimestampReadsOnlyMetadata(t *testing.T) {
	date := func(s string) int64 {
		ts, _ := time.Parse(strutil.TimeFormatDate, s)
		return ts.Unix()
	}

	if got := extractTimestamp("## 2026-06-10: 选择了 SQLite\n\n正文"); got != date("2026-06-10") {
		t.Errorf("heading date: got %d, want %d", got, date("2026-06-10"))
	}
	if got := extractTimestamp("## 2026-06-19T22:32:01+08:00\n\n正文"); got == 0 {
		t.Error("RFC3339 heading date was not parsed")
	}
	if got := extractTimestamp("## T\n\n来源: 2026-06-08\n\n正文"); got != date("2026-06-08") {
		t.Errorf("来源 date: got %d, want %d", got, date("2026-06-08"))
	}
	// A year in prose is not the fact's date — the old scan took it.
	if got := extractTimestamp("## Fact\n\nWe released this in 2024 and it still works."); got != 0 {
		t.Errorf("prose year must not become a timestamp, got %d", got)
	}
}

// Project memory follows the TURN's working directory, not the process cwd: a
// Finder-launched desktop app runs in "/", so resolving the repository from the
// process made project memory silently nonexistent there.
func TestTopicBackend_ProjectDomainFollowsTurnWorkingDir(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t) // built in a directory with no repo

	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx := wdctx.WithDir(t.Context(), repo)

	if err := backend.Store(ctx, "本项目部署用 make deploy", nil, ScopeAuto); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".tachi", "memory", InboxFile)); err != nil {
		t.Fatal("write did not land in the session's project memory")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "memory", InboxFile)); !os.IsNotExist(err) {
		t.Fatal("a project write must not also land in the global inbox")
	}

	// Recall inside the repository sees it...
	results, err := backend.Recall(ctx, "make deploy", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("project memory was not searched for a turn inside the repo")
	}

	// ...and a turn outside any repository does not.
	results, err = backend.Recall(t.Context(), "make deploy", 5)
	if err != nil {
		t.Fatalf("Recall outside: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("project memory leaked into a turn outside the repo: %d results", len(results))
	}
}
