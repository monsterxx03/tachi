package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/wdctx"
)

// planTestCtx returns a context carrying a working dir (tmpDir) and session ID.
func planTestCtx(t *testing.T, sessionID string) (context.Context, string) {
	t.Helper()
	tmpDir := t.TempDir()
	ctx := wdctx.WithDir(context.Background(), tmpDir)
	ctx = WithSessionID(ctx, sessionID)
	return ctx, tmpDir
}

// listPlanFiles returns all plan files under tmpDir/.tachi/plans/.
func listPlanFiles(t *testing.T, tmpDir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(tmpDir, ".tachi", "plans", "*.json"))
	if err != nil {
		t.Fatalf("glob plans: %v", err)
	}
	return matches
}

func readPlan(t *testing.T, path string) SavePlanParams {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	var p SavePlanParams
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	return p
}

// TestSavePlan_RepeatedSaveOverwrites verifies that calling SavePlan multiple
// times with the same title in the same session produces exactly one file
// whose content reflects the latest save. (Previously each call created a
// new timestamped file, leaving stale duplicates that misled the
// plan-tracking reminder.)
func TestSavePlan_RepeatedSaveOverwrites(t *testing.T) {
	ctx, tmpDir := planTestCtx(t, "sess123")
	tool := SavePlanTool{}

	v1 := `{"title": "My Plan", "content": "version 1", "steps": [{"content": "step a", "status": "pending"}]}`
	if _, err := tool.ExecuteContext(ctx, v1); err != nil {
		t.Fatalf("first save: %v", err)
	}

	v2 := `{"title": "My Plan", "content": "version 2", "steps": [{"content": "step a", "status": "completed"}]}`
	if _, err := tool.ExecuteContext(ctx, v2); err != nil {
		t.Fatalf("second save: %v", err)
	}

	files := listPlanFiles(t, tmpDir)
	if len(files) != 1 {
		t.Fatalf("expected 1 plan file after 2 saves, got %d: %v", len(files), files)
	}

	p := readPlan(t, files[0])
	if p.Content != "version 2" {
		t.Errorf("expected latest content %q, got %q", "version 2", p.Content)
	}
	if p.Steps[0].Status != "completed" {
		t.Errorf("expected latest step status %q, got %q", "completed", p.Steps[0].Status)
	}
}

// TestSavePlan_DifferentTitlesSeparateFiles verifies that distinct plan
// titles within one session still get their own files.
func TestSavePlan_DifferentTitlesSeparateFiles(t *testing.T) {
	ctx, tmpDir := planTestCtx(t, "sess123")
	tool := SavePlanTool{}

	a := `{"title": "Plan A", "content": "a", "steps": [{"content": "s", "status": "pending"}]}`
	b := `{"title": "Plan B", "content": "b", "steps": [{"content": "s", "status": "pending"}]}`
	if _, err := tool.ExecuteContext(ctx, a); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if _, err := tool.ExecuteContext(ctx, b); err != nil {
		t.Fatalf("save B: %v", err)
	}

	files := listPlanFiles(t, tmpDir)
	if len(files) != 2 {
		t.Fatalf("expected 2 plan files for 2 titles, got %d: %v", len(files), files)
	}
}

// TestSavePlan_FilenameFormat verifies the filename is {slug}-{sessionID}.json
// (no per-call timestamp), which is what PlanTrackingReminder's suffix
// matching relies on.
func TestSavePlan_FilenameFormat(t *testing.T) {
	ctx, tmpDir := planTestCtx(t, "abc12345")
	tool := SavePlanTool{}

	args := `{"title": "Fix Memory Bugs", "content": "c", "steps": [{"content": "s", "status": "pending"}]}`
	if _, err := tool.ExecuteContext(ctx, args); err != nil {
		t.Fatalf("save: %v", err)
	}

	files := listPlanFiles(t, tmpDir)
	if len(files) != 1 {
		t.Fatalf("expected 1 plan file, got %d", len(files))
	}
	want := "fix-memory-bugs-abc12345.json"
	if got := filepath.Base(files[0]); got != want {
		t.Errorf("filename: got %q, want %q", got, want)
	}
}

// TestSavePlan_InvalidStepStatus verifies step status validation still works.
func TestSavePlan_InvalidStepStatus(t *testing.T) {
	ctx, _ := planTestCtx(t, "sess123")
	tool := SavePlanTool{}

	args := `{"title": "P", "content": "c", "steps": [{"content": "s", "status": "bogus"}]}`
	if _, err := tool.ExecuteContext(ctx, args); err == nil {
		t.Error("expected error for invalid step status")
	}
}

// TestPlanSlug_LongCJKTitleStaysWritable pins the file-name rule against the failure it
// used to have: planSlug keeps CJK as-is, and capping it with a BYTE slice can cut a
// character in half. The slug then is not valid UTF-8, so the plan file cannot be
// created at all — the tool returns "illegal byte sequence" and nothing is saved.
func TestPlanSlug_LongCJKTitleStaysWritable(t *testing.T) {
	title := "Desktop plan 面板（P1 显示 + P2 plan 模式 + 提醒会话化）以及更多中文标题字符用来越过截断长度"

	slug := planSlug(title)
	if !utf8.ValidString(slug) {
		t.Fatalf("slug is not valid UTF-8 (%q) — the file write would fail", slug)
	}
	if n := utf8.RuneCountInString(slug); n > maxPlanSlugRunes {
		t.Errorf("slug has %d runes, want at most %d", n, maxPlanSlugRunes)
	}

	// And the whole path must be creatable: this is the assertion that would have caught
	// the bug (planSlug alone only shows a bad string).
	ctx, tmpDir := planTestCtx(t, "sess-cjk")
	args, err := json.Marshal(SavePlanParams{
		Title:   title,
		Content: "正文",
		Steps:   []SavePlanStep{{Content: "第一步", Status: "pending"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := (SavePlanTool{}).ExecuteContext(ctx, string(args)); err != nil {
		t.Fatalf("saving a plan with a long CJK title failed: %v", err)
	}
	if files := listPlanFiles(t, tmpDir); len(files) != 1 {
		t.Fatalf("expected 1 plan file, got %d", len(files))
	}
}
