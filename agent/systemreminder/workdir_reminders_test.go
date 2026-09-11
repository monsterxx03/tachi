package systemreminder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/wdctx"
)

// The three reminders that describe a project — .tachi.md, git status, plan tracking —
// must describe the TURN's working directory (wdctx), not the process's.
//
// Every test below pins a directory that is deliberately NOT the process working
// directory (the repo the tests run in), so a regression to os.Getwd /
// config.FindProjectRoot fails here instead of silently describing the wrong tree —
// which is exactly what a multi-session GUI would show its users.

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestProjectContextReminder_UsesWorkingDirFromContext(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".tachi.md"), "# 临时项目约定\n只写事实\n")

	lines := ProjectContextReminder{}.Generate(
		wdctx.WithDir(t.Context(), dir), Context{IsFirstMessage: true})

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "临时项目约定") {
		t.Fatalf("project context did not come from the turn's working directory: %q", joined)
	}
}

func TestProjectContextReminder_NoFileInWorkingDir(t *testing.T) {
	// A directory with no .tachi.md has no context to inject — even though the
	// process working directory (the repo running these tests) HAS one.
	lines := ProjectContextReminder{}.Generate(
		wdctx.WithDir(t.Context(), t.TempDir()), Context{IsFirstMessage: true})
	if len(lines) != 0 {
		t.Fatalf("expected no context, got %q", strings.Join(lines, "\n"))
	}
}

func TestGitReminder_UsesWorkingDirFromContext(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Skipf("git init: %v (%s)", err, out)
	}
	// A uniquely named untracked file: seeing it in the reminder proves the status
	// was read from THIS repository.
	writeFile(t, filepath.Join(dir, "only-in-this-tree.txt"), "x\n")

	lines := GitReminder{}.Generate(
		wdctx.WithDir(t.Context(), dir), Context{IsFirstMessage: true})

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "only-in-this-tree.txt") {
		t.Fatalf("git status did not come from the turn's working directory: %q", joined)
	}
}

func TestPlanTrackingReminder_UsesWorkingDirFromContext(t *testing.T) {
	const sessionID = "sess-42"
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".tachi", "plans", "demo-plan-"+sessionID+".json"), `{
  "title": "临时计划",
  "content": "正文",
  "steps": [{"content": "第一步", "status": "pending"}]
}`)

	lines := PlanTrackingReminder{}.Generate(
		wdctx.WithDir(t.Context(), dir), Context{IsFirstMessage: false, SessionID: sessionID})

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "临时计划") {
		t.Fatalf("plan was not found under the turn's working directory: %q", joined)
	}
	if !strings.Contains(joined, filepath.Join(dir, ".tachi", "plans")) {
		t.Fatalf("plan path should be the one under the turn's working directory: %q", joined)
	}
}

func TestPlanTrackingReminder_OtherSessionIsNotMine(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".tachi", "plans", "demo-plan-someone-else.json"), `{
  "title": "别人的计划",
  "content": "",
  "steps": [{"content": "第一步", "status": "pending"}]
}`)

	lines := PlanTrackingReminder{}.Generate(
		wdctx.WithDir(t.Context(), dir), Context{IsFirstMessage: false, SessionID: "sess-42"})
	if len(lines) != 0 {
		t.Fatalf("another session's plan must not be reported: %q", strings.Join(lines, "\n"))
	}
}
