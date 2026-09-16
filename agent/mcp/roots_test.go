package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRootsForDirsIsTheOneRule pins what every frontend that declares the `roots` capability
// hands a server: file:// URIs for the directories that EXIST, deduplicated and in a stable
// order. The two constraints are not cosmetic — a server realpath-resolves each root and drops
// the ones it cannot (so a stale root is noise at best), and it CACHES the list (so an answer
// that reshuffles between two asks for the same workspace set is pure churn).
func TestRootsForDirsIsTheOneRule(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	missing := filepath.Join(t.TempDir(), "unmounted")

	got := RootsForDirs([]string{b, a, a, missing, "", b})
	require.Len(t, got, 2, "only the two real directories, once each")
	// Sorted by path — so the answer is the same list whatever order the caller collected them in.
	assert.Equal(t, "file://"+a, got[0].URI)
	assert.Equal(t, "file://"+b, got[1].URI)
	assert.Equal(t, filepath.Base(a), got[0].Name, "the name is what a user reads when deciding")

	assert.Empty(t, RootsForDirs(nil))
	assert.Empty(t, RootsForDirs([]string{missing, ""}))
}
