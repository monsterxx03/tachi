package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Update must not rewrite a SKILL.md whose frontmatter it cannot parse.
//
// The old branch treated the whole file as a body with an empty frontmatter, so the rewrite folded
// the broken YAML into the new body: the file parsed again the next time (the old block had become
// text), the model got its own metadata as content, `enabled: false` came back as true, and every
// later Update stacked another copy — all of it silently, with an `updated skill` info log.
//
// Refusing is the reversible answer: the reader still has the whole original on disk, plus an error
// that says which file to fix.
func TestUpdateRefusesToRewriteABrokenFrontmatter(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	skillFile := filepath.Join(skillDir, "SKILL.md")
	// `tags: [a` is an unclosed flow sequence — the frontmatter is there, it just does not parse.
	original := "---\nname: demo\ndescription: 坏的\nenabled: false\ntags: [a\n---\n\n原始正文\n"
	if err := os.WriteFile(skillFile, []byte(original), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	s := NewStoreWithDirs([]string{dir}, []string{SourceProject})
	got, err := s.Update("demo", "新描述", "", nil, SourceProject)

	if err == nil {
		t.Fatalf("Update returned no error and rewrote the file; skill = %+v", got)
	}
	// The error has to be actionable on its own: whose file, and where.
	for _, want := range []string{"demo", skillFile, "frontmatter"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}

	after, readErr := os.ReadFile(skillFile)
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}
	if string(after) != original {
		t.Fatalf("file was rewritten:\n--- got ---\n%s\n--- want (unchanged) ---\n%s", after, original)
	}
}
