package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePlan writes a plan document where SavePlanTool writes it:
// <root>/.tachi/plans/<title-slug>-<sessionID>.json.
func writePlan(t *testing.T, root, slug, sid, body string) string {
	t.Helper()
	dir := filepath.Join(root, ".tachi", "plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, slug+"-"+sid+".json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return path
}

func ageFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// The panel shows the NEWEST plan of THIS session. Same-session plans with different
// titles coexist (the file name carries a title slug), so "which one" is a real question
// the panel has to answer; other sessions' plans share a directory and must never leak in.
func TestGetPlan_NewestOfThisSessionWins(t *testing.T) {
	root := t.TempDir()
	_, svc, sid := newRootsApp(t, root)

	older := writePlan(t, root, "first", sid,
		`{"title":"旧计划","content":"旧","steps":[{"content":"a","status":"pending"}]}`)
	newer := writePlan(t, root, "second", sid,
		`{"title":"新计划","content":"新正文","steps":[{"content":"b","status":"completed"},{"content":"c","status":"in_progress"}]}`)
	ageFile(t, older, time.Hour)

	vo := svc.GetPlan(sid)
	if vo.Title != "新计划" {
		t.Fatalf("Title = %q, want 新计划", vo.Title)
	}
	if vo.Path != newer {
		t.Errorf("Path = %q, want %q", vo.Path, newer)
	}
	if len(vo.Steps) != 2 || vo.Steps[0].Status != "completed" || vo.Steps[1].Status != "in_progress" {
		t.Errorf("Steps = %+v, want the saved statuses in order", vo.Steps)
	}
	if vo.Content != "新正文" {
		t.Errorf("Content = %q", vo.Content)
	}
	if vo.Others != 1 {
		t.Errorf("Others = %d, want 1 (the older plan is still there)", vo.Others)
	}
	if vo.UpdatedAt == "" {
		t.Error("UpdatedAt is empty; the panel needs to show when the plan was last touched")
	}
}

func TestGetPlan_IgnoresOtherSessions(t *testing.T) {
	root := t.TempDir()
	_, svc, sid := newRootsApp(t, root)
	writePlan(t, root, "someone-else", "2026-01-01-000000-ffffffff",
		`{"title":"别人的计划","content":"","steps":[{"content":"a","status":"pending"}]}`)

	vo := svc.GetPlan(sid)
	if vo.Title != "" || len(vo.Steps) != 0 {
		t.Fatalf("another session's plan leaked into the panel: %+v", vo)
	}
	if !strings.Contains(vo.Note, "还没有计划") {
		t.Errorf("Note = %q, want it to say there is no plan yet", vo.Note)
	}
}

// A finished plan is still the plan: unlike the tracking reminder (which only fires for
// work in progress), the panel must be able to show what was completed.
func TestGetPlan_CompletedPlanIsStillShown(t *testing.T) {
	root := t.TempDir()
	_, svc, sid := newRootsApp(t, root)
	writePlan(t, root, "done", sid,
		`{"title":"已完成的计划","content":"","steps":[{"content":"a","status":"completed"}]}`)

	vo := svc.GetPlan(sid)
	if vo.Title != "已完成的计划" {
		t.Fatalf("a completed plan must still be readable: %+v", vo)
	}
}

func TestGetPlan_UnreadableFile(t *testing.T) {
	root := t.TempDir()
	_, svc, sid := newRootsApp(t, root)
	writePlan(t, root, "broken", sid, `{"title":"坏掉的`)

	vo := svc.GetPlan(sid)
	if vo.Note == "" {
		t.Fatal("an unreadable plan must say so instead of rendering an empty panel")
	}
	if vo.Path == "" {
		t.Error("Path should still point at the file so the reader can open it")
	}
}

func TestGetPlan_NoSession(t *testing.T) {
	_, svc, _ := newRootsApp(t, t.TempDir())
	if vo := svc.GetPlan(""); vo.Note == "" {
		t.Error("GetPlan(\"\") should explain the missing session")
	}
}
