package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/monsterxx03/tachi/pkg/fileindex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedAtFileDir creates a directory with a couple of entries and makes it the
// process working directory, which is the root @-file completion searches.
func seedAtFileDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.md"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	t.Chdir(dir)
}

func atFilePaths(matches []fileindex.Match) []string {
	var out []string
	for _, m := range matches {
		out = append(out, m.Path)
	}
	return out
}

// TestAtFileCompletion covers the @-file completion flow end to end against a
// real directory: "@" lists the immediate entries, a partial name searches the
// tree, and Tab inserts the reference.
func TestAtFileCompletion(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)

	i.textarea.SetValue("@")
	i.updateCompletions()
	require.NotNil(t, i.atFileMatches, "typing @ opens the completion list")
	assert.ElementsMatch(t, []string{"src", "readme.md"}, atFilePaths(i.atFileMatches))

	i.textarea.SetValue("@readme")
	i.updateCompletions()
	require.NotEmpty(t, i.atFileMatches)

	i, _ = i.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, "@readme.md ", i.textarea.Value(), "Tab accepts the match and appends a space")
	assert.Nil(t, i.atFileMatches, "the popup closes once the reference is complete")
}

// TestAtFileCompletionAfterNewline pins the widened trigger rule: a reference
// typed at the start of a new line completes just like one after a space.
func TestAtFileCompletionAfterNewline(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)

	i.textarea.SetValue("看一下\n@readme")
	i.updateCompletions()

	require.NotEmpty(t, i.atFileMatches)
	assert.Contains(t, atFilePaths(i.atFileMatches), "readme.md")
}

// TestAtFileCompletionNotTriggeredMidWord ensures a bare email-like "@" inside
// a word is not treated as a reference.
func TestAtFileCompletionNotTriggeredMidWord(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)

	i.textarea.SetValue("mail@readme")
	i.updateCompletions()

	assert.Nil(t, i.atFileMatches)
}
