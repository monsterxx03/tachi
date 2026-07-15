package memory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

func setupTopicBackend(t *testing.T) (*TopicBackend, string) {
	t.Helper()
	requireRipgrep(t)

	tmpDir := t.TempDir()

	backend, err := NewTopicBackend(Config{
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

	err := backend.Store(ctx, StoreOptions{
		DirectContent: "用户偏好：代码注释用英文",
	})
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

func TestTopicBackend_StoreNonDirect_NoOp(t *testing.T) {
	backend, tmpDir := setupTopicBackend(t)
	ctx := t.Context()

	// Non-direct store should be no-op.
	err := backend.Store(ctx, StoreOptions{
		Scope:        StoreScopeTurn,
		TurnMessages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// inbox.md should NOT exist.
	inboxPath := filepath.Join(tmpDir, "memory", "inbox.md")
	if _, err := os.Stat(inboxPath); err == nil {
		t.Error("inbox.md should not be created for non-direct stores")
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
	err := backend.Store(ctx, StoreOptions{
		DirectContent: "记住：部署用 make deploy",
	})
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
	}

	for _, tt := range tests {
		got := extractTitle(tt.block)
		if got != tt.want {
			t.Errorf("extractTitle(%q) = %q, want %q", tt.block[:min(len(tt.block), 20)], got, tt.want)
		}
	}
}

func TestComputeScore(t *testing.T) {
	// computeScore now measures only text relevance (keyword matches, recency).
	// Memory lifecycle factors (decay, superseded, reinforcements) are applied
	// in Recall() from authoritative FactState data.

	// Block with keyword in title should get title bonus (0.5 + 0.2 = 0.7).
	withTitle := "## Database\n\n关键词: storage\n\nWe chose SQLite."
	titleScore := computeScore(withTitle, "database")
	if titleScore < 0.65 || titleScore > 0.75 {
		t.Errorf("title match score should be ~0.7, got %f", titleScore)
	}

	// Block with keyword in 关键词 line should get keyword bonus (0.5 + 0.2 = 0.7).
	withKeyword := "## Storage\n\n关键词: database\n\nWe chose SQLite."
	keywordScore := computeScore(withKeyword, "database")
	if keywordScore < 0.65 || keywordScore > 0.75 {
		t.Errorf("keyword match score should be ~0.7, got %f", keywordScore)
	}

	// Block with keyword in body only gets base score (0.5).
	bodyOnly := "## Storage\n\n关键词: sql\n\nWe used a database."
	bodyScore := computeScore(bodyOnly, "database")
	if bodyScore < 0.45 || bodyScore > 0.55 {
		t.Errorf("body-only match score should be ~0.5, got %f", bodyScore)
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

	backendA, err := NewTopicBackend(Config{BaseDir: dirA}, nil)
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

	backendB, err := NewTopicBackend(Config{BaseDir: dirB}, nil)
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
