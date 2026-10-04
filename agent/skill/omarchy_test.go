package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeSkill drops a minimal SKILL.md under dir/<name>/.
func writeSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n# %s\n\nBody.\n", name, description, name)
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

// fakeOmarchy builds the tree omarchy ships (<root>/default/agents/skills) in a temp dir and
// points OMARCHY_PATH at it, so a test never reads the machine's real one.
func fakeOmarchy(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "default", "agents", "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir omarchy skills: %v", err)
	}
	t.Setenv(omarchySkillFromEnv, root)
	return dir
}

// The point of the whole thing: on a machine whose operating system ships skills for its agents,
// an agent that reads them needs no symlinks and no setup.
func TestTheOperatingSystemsSkillsAreScanned(t *testing.T) {
	dir := fakeOmarchy(t)
	writeSkill(t, dir, "diagnose-crash", "Diagnose why a program crashed")

	// Read through the root alone (not NewStore): the machine's own global tree would otherwise
	// take part, and what is being pinned here is that this directory loads like any other.
	s := newStore([]string{dir}, []string{SourceOmarchy})

	metas := s.List()
	if len(metas) != 1 || metas[0].Name != "diagnose-crash" {
		t.Fatalf("List() = %+v, want the one skill omarchy ships", metas)
	}
	if metas[0].Source != SourceOmarchy {
		t.Errorf("source = %q, want %q", metas[0].Source, SourceOmarchy)
	}

	loaded, err := s.Load("diagnose-crash")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if filepath.Dir(loaded.Dir) != filepath.Clean(dir) {
		t.Errorf("skill loaded from %s, want it under %s", loaded.Dir, dir)
	}
}

// A machine without omarchy has no such directory, and the store must not invent one.
func TestStoreWithoutOmarchyHasNoSkillRoot(t *testing.T) {
	t.Setenv(omarchySkillFromEnv, filepath.Join(t.TempDir(), "not-omarchy"))

	for _, src := range NewStore("").Sources() {
		if src == SourceOmarchy {
			t.Fatalf("the omarchy root was added for a tree that does not exist: %v", NewStore("").Dirs())
		}
	}
}

// Position is the invariant, not presence: the OS root is scanned LAST, so a copy of the same
// name in the project or in ~/.tachi/skills shadows it (that is what makes the user's own version
// of an OS skill the one that runs).
func TestTheOperatingSystemsSkillsAreScannedLast(t *testing.T) {
	dir := fakeOmarchy(t)

	s := NewStore(t.TempDir()) // a project root, so the project-level roots exist too
	dirs, sources := s.Dirs(), s.Sources()
	if len(dirs) != len(sources) {
		t.Fatalf("dirs and sources disagree: %d vs %d", len(dirs), len(sources))
	}
	last := len(dirs) - 1
	if sources[last] != SourceOmarchy {
		t.Fatalf("last source = %q, want %q (dirs: %v)", sources[last], SourceOmarchy, dirs)
	}
	if filepath.Clean(dirs[last]) != filepath.Clean(dir) {
		t.Errorf("last dir = %q, want the omarchy tree %q", dirs[last], dir)
	}
}

// Read-only like the claude/cursor imports: the directory belongs to the package manager, so a
// skill written there would not survive an update.
func TestOmarchySkillsAreNotWritable(t *testing.T) {
	dir := fakeOmarchy(t)

	s := newStore([]string{dir}, []string{SourceOmarchy})
	if _, err := s.Create("new-skill", "d", "body", nil, SourceOmarchy, false); err == nil {
		t.Error("Create with the omarchy source succeeded; it must be refused")
	}
}
