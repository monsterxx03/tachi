package systemreminder

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/wdctx"
)

// The rules that ride with .tachi.md are emitted HERE (not written into the file),
// precisely so they apply to every repository rather than to the one that happens to
// carry them. These tests pin that they accompany the content, ahead of it, and only
// when there is content at all.
func TestProjectContextReminder_CarriesTheFileRules(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".tachi.md"), "# 临时项目约定\n只写事实\n")

	lines := ProjectContextReminder{}.Generate(
		wdctx.WithDir(t.Context(), dir), Context{IsFirstMessage: true})
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, projectContextRules) {
		t.Fatalf("the file rules did not ride with the injected context:\n%s", joined)
	}
	// Order matters: the rules are the contract for the file below them, so a reader
	// that stops early has still seen how to treat what follows.
	rules := strings.Index(joined, projectContextRules)
	content := strings.Index(joined, "临时项目约定")
	if rules < 0 || content < 0 || rules > content {
		t.Fatalf("rules must precede the file content (rules=%d content=%d):\n%s", rules, content, joined)
	}
}

func TestProjectContextReminder_RulesDoNotReplaceTheFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".tachi.md"), "# 临时项目约定\n只写事实\n")

	lines := ProjectContextReminder{}.Generate(
		wdctx.WithDir(t.Context(), dir), Context{IsFirstMessage: true})
	joined := strings.Join(lines, "\n")

	// The project's own words must survive verbatim, heading intact — the rules are an
	// addition, never a re-rendering of them.
	if !strings.Contains(joined, "# 临时项目约定\n只写事实\n") {
		t.Fatalf("file content was altered:\n%s", joined)
	}
}

func TestProjectContextReminder_SilentAfterTheFirstMessage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".tachi.md"), "# 临时项目约定\n")

	if lines := (ProjectContextReminder{}).Generate(
		wdctx.WithDir(t.Context(), dir), Context{IsFirstMessage: false}); len(lines) != 0 {
		t.Fatalf("context must be injected once per conversation, got %q", strings.Join(lines, "\n"))
	}
}
