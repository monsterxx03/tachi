package memory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/pkg/debuglog"
)

func setupTopicBackend(t *testing.T) (*TopicBackend, string) {
	t.Helper()
	requireRipgrep(t)

	tmpDir := t.TempDir()

	backend, err := NewTopicBackend(Config{
		BaseDir: tmpDir,
	})
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
	ctx := context.Background()

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
	ctx := context.Background()

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
	ctx := context.Background()

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
	ctx := context.Background()

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

	results, err := backend.Recall(ctx, "database", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Active should score higher than superseded.
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
	ctx := context.Background()

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
	ctx := context.Background()

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
	ctx := context.Background()

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
		logger:     debuglog.DefaultLogger.WithSource("test"),
	}

	results, err := backend.Recall(context.Background(), "vim", 10)
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

	blocks := splitByHR(content)
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
	active := "## Database Choice\n\n状态: active\n关键词: database, sqlite\n\nWe chose SQLite."
	superseded := "## Old Choice\n\n状态: superseded\n关键词: database, postgres\n\nWe used Postgres."

	activeScore := computeScore(active, "database")
	supersededScore := computeScore(superseded, "database")

	if activeScore <= supersededScore {
		t.Errorf("active (%f) should score higher than superseded (%f)", activeScore, supersededScore)
	}
}
