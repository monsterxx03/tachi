package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/memory"
)

func TestCalculateDecay(t *testing.T) {
	// Both zero → brand new, decay = 1.0
	if d := CalculateDecay(time.Time{}, time.Time{}); d != 1.0 {
		t.Errorf("zero time: expected 1.0, got %f", d)
	}

	// Just now → close to 1.0
	now := time.Now()
	d := CalculateDecay(now, time.Time{})
	if d < 0.99 || d > 1.01 {
		t.Errorf("just now: expected ≈1.0, got %f", d)
	}

	// 7 days ago → ~0.5
	weekAgo := now.Add(-7 * 24 * time.Hour)
	d = CalculateDecay(weekAgo, time.Time{})
	if d < 0.45 || d > 0.55 {
		t.Errorf("7 days ago: expected ≈0.5, got %f", d)
	}

	// 14 days ago → ~0.25
	twoWeeksAgo := now.Add(-14 * 24 * time.Hour)
	d = CalculateDecay(twoWeeksAgo, time.Time{})
	if d < 0.20 || d > 0.30 {
		t.Errorf("14 days ago: expected ≈0.25, got %f", d)
	}

	// 30 days ago → very low
	monthAgo := now.Add(-30 * 24 * time.Hour)
	d = CalculateDecay(monthAgo, time.Time{})
	if d > 0.1 {
		t.Errorf("30 days ago: expected <0.1, got %f", d)
	}
}

func TestCalculateDecay_CreatedAtFallback(t *testing.T) {
	now := time.Now()
	twoWeeksAgo := now.Add(-14 * 24 * time.Hour)

	// Never reinforced, created 14 days ago → decays from creation (~0.25).
	// Before the fix, zero LastReinforced always meant decay=1.0 forever.
	d := CalculateDecay(time.Time{}, twoWeeksAgo)
	if d < 0.20 || d > 0.30 {
		t.Errorf("never reinforced, created 14d ago: expected ≈0.25, got %f", d)
	}

	// Recently reinforced wins over old creation time.
	d = CalculateDecay(now, twoWeeksAgo)
	if d < 0.99 || d > 1.01 {
		t.Errorf("reinforced now, created 14d ago: expected ≈1.0, got %f", d)
	}

	// Future timestamp (clock skew) → clamped to 1.0.
	d = CalculateDecay(now.Add(time.Hour), time.Time{})
	if d != 1.0 {
		t.Errorf("future timestamp: expected 1.0, got %f", d)
	}
}

func TestScanTopicFacts_NewFacts(t *testing.T) {
	tmpDir := t.TempDir()
	topicsDir := filepath.Join(tmpDir, "topics")
	os.MkdirAll(topicsDir, 0755)

	// Create a topic file with two facts.
	topicContent := `# Test Topic

## 2026-06-10: First Fact

来源: session abc123
状态: active
关键词: test, first

This is the first fact.

---

## 2026-06-08: Second Fact

来源: session def456
状态: superseded
关键词: test, second

This is the second fact, now outdated.

---
`
	os.WriteFile(filepath.Join(topicsDir, "test.md"), []byte(topicContent), 0644)

	// Scan with empty existing states.
	result := ScanTopicFacts(tmpDir, nil, nil)

	if len(result) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(result))
	}

	// Both should have decay=1.0 (new) and reinforcements=0.
	for id, fs := range result {
		if fs.Decay != 1.0 {
			t.Errorf("%s: expected decay=1.0, got %f", id, fs.Decay)
		}
		if fs.Reinforcements != 0 {
			t.Errorf("%s: expected reinforcements=0, got %d", id, fs.Reinforcements)
		}
		if fs.CreatedAt.IsZero() {
			t.Errorf("%s: CreatedAt should not be zero", id)
		}
		if !fs.LastReinforced.IsZero() {
			t.Errorf("%s: LastReinforced should be zero for new facts", id)
		}
	}

	// Verify superseded flag.
	for _, fs := range result {
		if strings.Contains(fs.ID, "Second") && !fs.Superseded {
			t.Error("Second fact should be superseded")
		}
	}
}

func TestScanTopicFacts_Merge(t *testing.T) {
	tmpDir := t.TempDir()
	topicsDir := filepath.Join(tmpDir, "topics")
	os.MkdirAll(topicsDir, 0755)

	topicContent := `## Persisted Fact

状态: active
关键词: persisted

This fact persists across dreams.

---
`
	os.WriteFile(filepath.Join(topicsDir, "persist.md"), []byte(topicContent), 0644)

	// First scan: creates new fact.
	firstResult := ScanTopicFacts(tmpDir, nil, nil)
	if len(firstResult) != 1 {
		t.Fatalf("first scan: expected 1 fact, got %d", len(firstResult))
	}

	// Simulate reinforcement: manually set decay and counters.
	var factID string
	for id, fs := range firstResult {
		factID = id
		fs.Reinforcements = 3
		fs.LastReinforced = time.Now().Add(-1 * time.Hour)
		fs.Decay = 0.95
	}

	// Second scan: should preserve reinforcement data and recalculate decay.
	secondResult := ScanTopicFacts(tmpDir, firstResult, nil)
	if len(secondResult) != 1 {
		t.Fatalf("second scan: expected 1 fact, got %d", len(secondResult))
	}

	fs := secondResult[factID]
	if fs.Reinforcements != 3 {
		t.Errorf("reinforcements: expected 3, got %d", fs.Reinforcements)
	}
	if fs.LastReinforced.IsZero() {
		t.Error("LastReinforced should be preserved")
	}
	// Decay should be recalculated from LastReinforced (~1 hour ago → close to 1.0).
	if fs.Decay < 0.9 {
		t.Errorf("decay should be close to 1.0 after 1h, got %f", fs.Decay)
	}
}

func TestScanTopicFacts_Merge_DecaysFromCreatedAt(t *testing.T) {
	tmpDir := t.TempDir()
	topicsDir := filepath.Join(tmpDir, "topics")
	os.MkdirAll(topicsDir, 0755)

	topicContent := `## Old Fact

状态: active
关键词: old

A fact that was never recalled.

---
`
	os.WriteFile(filepath.Join(topicsDir, "old.md"), []byte(topicContent), 0644)

	// First scan creates the fact state.
	firstResult := ScanTopicFacts(tmpDir, nil, nil)
	if len(firstResult) != 1 {
		t.Fatalf("first scan: expected 1 fact, got %d", len(firstResult))
	}

	// Simulate age: created 14 days ago, never reinforced.
	var factID string
	for id, fs := range firstResult {
		factID = id
		fs.CreatedAt = time.Now().Add(-14 * 24 * time.Hour)
	}

	// Second scan: decay must be recalculated from CreatedAt (~0.25), not
	// stay at 1.0 — never-recalled facts should fade too.
	secondResult := ScanTopicFacts(tmpDir, firstResult, nil)
	fs := secondResult[factID]
	if fs.Decay < 0.20 || fs.Decay > 0.30 {
		t.Errorf("expected decay ≈0.25 for 14-day-old never-reinforced fact, got %f", fs.Decay)
	}
}

func TestScanTopicFacts_Removed(t *testing.T) {
	tmpDir := t.TempDir()
	topicsDir := filepath.Join(tmpDir, "topics")
	os.MkdirAll(topicsDir, 0755)

	// First scan with a fact.
	topicContent := `## Temp Fact

状态: active
关键词: temp

Will be removed.

---
`
	os.WriteFile(filepath.Join(topicsDir, "temp.md"), []byte(topicContent), 0644)

	firstResult := ScanTopicFacts(tmpDir, nil, nil)
	if len(firstResult) != 1 {
		t.Fatalf("first scan: expected 1, got %d", len(firstResult))
	}

	// Remove the file.
	os.Remove(filepath.Join(topicsDir, "temp.md"))

	// Second scan: fact should be gone.
	secondResult := ScanTopicFacts(tmpDir, firstResult, nil)
	if len(secondResult) != 0 {
		t.Errorf("second scan: expected 0 facts after removal, got %d", len(secondResult))
	}
}

func TestScanTopicFacts_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()
	os.MkdirAll(filepath.Join(tmpDir, "topics"), 0755)

	result := ScanTopicFacts(tmpDir, nil, nil)
	if len(result) != 0 {
		t.Errorf("expected 0 facts in empty dir, got %d", len(result))
	}
}

func TestScanTopicFacts_IgnoresNonMD(t *testing.T) {
	tmpDir := t.TempDir()
	topicsDir := filepath.Join(tmpDir, "topics")
	os.MkdirAll(topicsDir, 0755)

	// Create a non-.md file that should be ignored.
	os.WriteFile(filepath.Join(topicsDir, "notes.txt"), []byte("not a topic"), 0644)

	result := ScanTopicFacts(tmpDir, nil, nil)
	if len(result) != 0 {
		t.Errorf("expected 0 facts (non-.md file ignored), got %d", len(result))
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

	blocks := memory.SplitByHR(content)
	if len(blocks) != 3 {
		t.Errorf("expected 3 blocks, got %d", len(blocks))
		for i, b := range blocks {
			t.Logf("block %d: %q", i, b)
		}
	}

	// Verify # Title header is attached to Block 1.
	if len(blocks) > 0 && !strings.Contains(blocks[0], "# Title") {
		t.Error("first block should include # Title")
	}
	if len(blocks) > 0 && !strings.Contains(blocks[0], "Block 1") {
		t.Error("first block should include Block 1")
	}
}

func TestFactID(t *testing.T) {
	id1 := memory.FactID("test.md", "hello world")
	id2 := memory.FactID("test.md", "hello world")
	id3 := memory.FactID("test.md", "different content")
	id4 := memory.FactID("other.md", "hello world")

	// Same content + same file → same ID.
	if id1 != id2 {
		t.Errorf("expected same ID for same input, got %q vs %q", id1, id2)
	}

	// Different content → different ID.
	if id1 == id3 {
		t.Errorf("expected different IDs for different content")
	}

	// Different file → different ID.
	if id1 == id4 {
		t.Errorf("expected different IDs for different files")
	}

	// Verify format: "topic:<filename>:<8 hex chars>"
	if !strings.HasPrefix(id1, "topic:test.md:") {
		t.Errorf("expected id to start with 'topic:test.md:', got %q", id1)
	}
}
