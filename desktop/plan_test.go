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

	vo := svc.GetPlan(sid, "")
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
	if len(vo.Plans) != 2 {
		t.Errorf("Plans = %d entries, want 2 (the older plan is still listed)", len(vo.Plans))
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

	vo := svc.GetPlan(sid, "")
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

	vo := svc.GetPlan(sid, "")
	if vo.Title != "已完成的计划" {
		t.Fatalf("a completed plan must still be readable: %+v", vo)
	}
}

func TestGetPlan_UnreadableFile(t *testing.T) {
	root := t.TempDir()
	_, svc, sid := newRootsApp(t, root)
	writePlan(t, root, "broken", sid, `{"title":"坏掉的`)

	vo := svc.GetPlan(sid, "")
	if vo.Note == "" {
		t.Fatal("an unreadable plan must say so instead of rendering an empty panel")
	}
	if vo.Path == "" {
		t.Error("Path should still point at the file so the reader can open it")
	}
}

func TestGetPlan_NoSession(t *testing.T) {
	_, svc, _ := newRootsApp(t, t.TempDir())
	if vo := svc.GetPlan("", ""); vo.Note == "" {
		t.Error("GetPlan(\"\") should explain the missing session")
	}
}

// A session accumulates plans, so the panel needs the list, not just the newest: the
// payload carries every remaining plan (newest first) and can be asked for a specific one.
func TestGetPlan_ListsTheSessionsPlans(t *testing.T) {
	root := t.TempDir()
	_, svc, sid := newRootsApp(t, root)

	older := writePlan(t, root, "first", sid,
		`{"title":"旧计划","content":"旧","steps":[{"content":"a","status":"completed"},{"content":"b","status":"pending"}]}`)
	newer := writePlan(t, root, "second", sid,
		`{"title":"新计划","content":"新","steps":[{"content":"c","status":"completed"}]}`)
	ageFile(t, older, time.Hour)

	vo := svc.GetPlan(sid, "")
	if len(vo.Plans) != 2 {
		t.Fatalf("Plans = %d entries, want 2", len(vo.Plans))
	}
	if vo.Plans[0].Path != newer || vo.Plans[1].Path != older {
		t.Errorf("the list must be newest first: %+v", vo.Plans)
	}
	if vo.Plans[1].Done != 1 || vo.Plans[1].Total != 2 {
		t.Errorf("a list entry must carry its progress: %+v", vo.Plans[1])
	}

	// Asking for a specific plan returns THAT one, with the same list attached.
	other := svc.GetPlan(sid, older)
	if other.Title != "旧计划" || other.Path != older {
		t.Fatalf("GetPlan(path) returned the wrong plan: %+v", other)
	}
	if len(other.Plans) != 2 {
		t.Errorf("the list must travel with a specific plan too: %+v", other.Plans)
	}
}

// The path comes from the frontend, so it must be checked: asking for (or deleting) another
// session's plan is refused rather than answered.
func TestGetPlanAndDeletePlan_RefuseForeignPaths(t *testing.T) {
	root := t.TempDir()
	_, svc, sid := newRootsApp(t, root)
	writePlan(t, root, "mine", sid,
		`{"title":"我的","content":"","steps":[{"content":"a","status":"pending"}]}`)
	foreign := writePlan(t, root, "theirs", "2026-01-01-000000-ffffffff",
		`{"title":"别人的","content":"","steps":[{"content":"a","status":"pending"}]}`)

	if vo := svc.GetPlan(sid, foreign); vo.Note == "" || vo.Title != "" {
		t.Errorf("reading another session's plan should be refused: %+v", vo)
	}
	if got := svc.DeletePlan(sid, foreign); got == "ok" {
		t.Error("deleting another session's plan must be refused")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("the refused file must still exist: %v", err)
	}
}

func TestDeletePlan_RemovesOwnPlan(t *testing.T) {
	root := t.TempDir()
	_, svc, sid := newRootsApp(t, root)
	path := writePlan(t, root, "gone", sid,
		`{"title":"要删掉的","content":"","steps":[{"content":"a","status":"pending"}]}`)

	if got := svc.DeletePlan(sid, path); got != "ok" {
		t.Fatalf("DeletePlan = %q, want ok", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the plan file should be gone: %v", err)
	}
	if vo := svc.GetPlan(sid, ""); len(vo.Plans) != 0 {
		t.Errorf("the list should be empty now: %+v", vo.Plans)
	}
}
