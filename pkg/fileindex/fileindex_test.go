package fileindex

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireRipgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not installed")
	}
}

// writeFile creates a file under root and returns its slash-separated path.
func writeFile(t *testing.T, root, rel string, data string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(data), 0o644))
}

func hasPath(matches []Match, path string) bool {
	for _, m := range matches {
		if m.Path == path {
			return true
		}
	}
	return false
}

func TestSearchEmptyQueryListsImmediateEntries(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "zed.go", "x")
	writeFile(t, root, "zeta/inner.go", "x")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))

	matches, err := New(nil).Search(root, "", 10)
	require.NoError(t, err)
	require.Len(t, matches, 2, ".git must be skipped and subdirectory contents hidden")

	assert.Equal(t, "zeta", matches[0].Path, "directories come first")
	assert.True(t, matches[0].IsDir)
	assert.Equal(t, "zed.go", matches[1].Path)
	assert.False(t, matches[1].IsDir)
}

func TestImmediateRespectsLimit(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		writeFile(t, root, name, "x")
	}

	matches, err := New(nil).Immediate(root, 2)
	require.NoError(t, err)
	assert.Len(t, matches, 2)
}

func TestSearchFuzzyOverTree(t *testing.T) {
	requireRipgrep(t)
	root := t.TempDir()
	writeFile(t, root, "src/handler/agent.go", "x")
	writeFile(t, root, "docs/guide.md", "x")

	ix := New(nil)

	matches, err := ix.Search(root, "agent", 20)
	require.NoError(t, err)
	assert.True(t, hasPath(matches, "src/handler/agent.go"))
	assert.False(t, hasPath(matches, "docs/guide.md"))

	// A slash in the query narrows the walk to that prefix.
	matches, err = ix.Search(root, "src/handler/agent", 20)
	require.NoError(t, err)
	assert.True(t, hasPath(matches, "src/handler/agent.go"))
}

// TestSearchForceIncludesTachi covers the .tachi exception: its contents are
// normally gitignored, yet stay referenceable.
func TestSearchForceIncludesTachi(t *testing.T) {
	requireRipgrep(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	writeFile(t, root, ".gitignore", ".tachi\nsecret.txt\n")
	writeFile(t, root, ".tachi/sessions/s1.json", "{}")
	writeFile(t, root, "secret.txt", "shh")

	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = root
	require.NoError(t, cmd.Run())

	ix := New(nil)

	matches, err := ix.Search(root, "tachi", 20)
	require.NoError(t, err)
	assert.True(t, hasPath(matches, ".tachi/sessions/s1.json"), "gitignored .tachi files are force-included")

	matches, err = ix.Search(root, "secret", 20)
	require.NoError(t, err)
	assert.False(t, hasPath(matches, "secret.txt"), "other gitignored files stay hidden")
}

func TestSearchCachesUntilInvalidated(t *testing.T) {
	requireRipgrep(t)
	root := t.TempDir()
	writeFile(t, root, "first.go", "x")

	ix := NewWithTTL(nil, time.Minute)
	_, err := ix.Search(root, "first", 20)
	require.NoError(t, err)

	writeFile(t, root, "second_unique.go", "x")

	matches, err := ix.Search(root, "secondunique", 20)
	require.NoError(t, err)
	assert.False(t, hasPath(matches, "second_unique.go"), "a cached index does not see new files")

	ix.Invalidate(root)

	matches, err = ix.Search(root, "secondunique", 20)
	require.NoError(t, err)
	assert.True(t, hasPath(matches, "second_unique.go"))
}

func TestSearchWithoutCacheRebuildsEachTime(t *testing.T) {
	requireRipgrep(t)
	root := t.TempDir()
	writeFile(t, root, "first.go", "x")

	ix := NewWithTTL(nil, 0)
	_, err := ix.Search(root, "first", 20)
	require.NoError(t, err)

	writeFile(t, root, "second_unique.go", "x")

	matches, err := ix.Search(root, "secondunique", 20)
	require.NoError(t, err)
	assert.True(t, hasPath(matches, "second_unique.go"))
}

func TestSearchMissingRootReportsError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")

	ix := New(nil)
	_, err := ix.Search(root, "x", 10)
	assert.Error(t, err)

	// The failure is cached, so a second call keeps reporting it.
	_, err = ix.Search(root, "x", 10)
	assert.Error(t, err)

	_, err = ix.Immediate(root, 10)
	assert.Error(t, err)
}

func TestSearchIsConcurrencySafe(t *testing.T) {
	requireRipgrep(t)
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c/d.go"} {
		writeFile(t, root, name, "x")
	}

	ix := New(nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				_, _ = ix.Search(root, "go", 20)
				_, _ = ix.Search(root, "", 20)
			}
		}()
	}
	wg.Wait()
}
