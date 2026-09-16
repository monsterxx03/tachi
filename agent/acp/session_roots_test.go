package acp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionRootsAreSharedWithTheMCPAnswer pins the object the ACP session and its MCP
// connections both read: the servers answer `roots/list` from it, so it must follow the workspace
// when `session/resume` replaces the additional directories — and the session's own readers must
// not be able to reach in and mutate it through a returned slice.
func TestSessionRootsAreSharedWithTheMCPAnswer(t *testing.T) {
	cwd, extra := t.TempDir(), t.TempDir()
	roots := newSessionRoots(cwd, []string{extra})

	got := roots.mcpRoots()
	require.Len(t, got, 2)
	assert.Equal(t, "file://"+cwd, got[0].URI)
	assert.Equal(t, "file://"+extra, got[1].URI)

	// Someone building a prompt from Dirs() must not be able to write through to the session.
	dirs := roots.Dirs()
	dirs[0] = "/nope"
	assert.Equal(t, []string{extra}, roots.Dirs())

	// A resume whose additionalDirectories is the complete resulting list: the MCP answer
	// follows, which is the whole reason this is an object rather than a captured copy.
	roots.Set(cwd, nil)
	got = roots.mcpRoots()
	require.Len(t, got, 1)
	assert.Equal(t, "file://"+cwd, got[0].URI)
	assert.Equal(t, cwd, roots.Cwd())
	assert.Empty(t, roots.Dirs())
}
