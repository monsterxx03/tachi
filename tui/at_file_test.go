package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/monsterxx03/tachi/agent/sessionref"
	"github.com/monsterxx03/tachi/session"
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

func atLabels(matches []atMatch) []string {
	var out []string
	for _, m := range matches {
		out = append(out, m.Label)
	}
	return out
}

// TestAtSessionCompletion covers the popup's other namespace: "@session:" searches
// the session store instead of the working directory, and Tab inserts the
// reference the expansion reads back — never the session's content.
func TestAtSessionCompletion(t *testing.T) {
	seedAtFileDir(t)
	dir := t.TempDir()
	store, err := session.NewFileStore(dir)
	require.NoError(t, err)
	other := &session.Session{
		ID: "2026-10-04-100000-hugo0001", Title: "安装Hugo",
		UpdatedAt: time.Date(2026, 10, 4, 20, 32, 0, 0, time.Local),
	}
	require.NoError(t, store.CreateSession(other))
	require.NoError(t, store.CreateSession(&session.Session{
		ID: "2026-10-04-200000-other002", Title: "别的会话",
	}))

	i := NewInputArea(10, "", nil)
	i.SetSessionSource(sessionref.New(store.ListSessions, dir))
	i.SetCurrentSession("2026-10-04-200000-other002")

	// The bare namespace opens on every OTHER conversation.
	i.textarea.SetValue("@session:")
	i.updateCompletions()
	require.Len(t, i.atMatches, 1, "the current session is not offered")
	assert.Equal(t, "@session:2026-10-04-100000-hugo0001", i.atMatches[0].Ref)
	assert.Contains(t, i.atMatches[0].Label, "安装Hugo")
	assert.Contains(t, i.atMatches[0].Label, "2026-10-04 20:32")

	// A query narrows it, and the file namespace is untouched by a session row.
	i.textarea.SetValue("@session:hugo")
	i.updateCompletions()
	require.Len(t, i.atMatches, 1)

	i, _ = i.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, "@session:2026-10-04-100000-hugo0001 ", i.textarea.Value())
}

// Without a session store the namespace offers nothing — the file completion a
// frontend with no store still has keeps working.
func TestAtSessionCompletionWithoutAStore(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)

	i.textarea.SetValue("@session:hugo")
	i.updateCompletions()

	assert.Nil(t, i.atMatches)
}

// TestAtFileCompletion covers the @-file completion flow end to end against a
// real directory: "@" lists the immediate entries, a partial name searches the
// tree, and Tab inserts the reference.
func TestAtFileCompletion(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)

	i.textarea.SetValue("@")
	i.updateCompletions()
	require.NotNil(t, i.atMatches, "typing @ opens the completion list")
	assert.ElementsMatch(t, []string{"src", "readme.md"}, atLabels(i.atMatches))

	i.textarea.SetValue("@readme")
	i.updateCompletions()
	require.NotEmpty(t, i.atMatches)

	i, _ = i.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, "@readme.md ", i.textarea.Value(), "Tab accepts the match and appends a space")
	assert.Nil(t, i.atMatches, "the popup closes once the reference is complete")
}

// TestAtFileCompletionAfterNewline pins the widened trigger rule: a reference
// typed at the start of a new line completes just like one after a space.
func TestAtFileCompletionAfterNewline(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)

	i.textarea.SetValue("看一下\n@readme")
	i.updateCompletions()

	require.NotEmpty(t, i.atMatches)
	assert.Contains(t, atLabels(i.atMatches), "readme.md")
}

// TestAtFileCompletionNotTriggeredMidWord ensures a bare email-like "@" inside
// a word is not treated as a reference.
func TestAtFileCompletionNotTriggeredMidWord(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)

	i.textarea.SetValue("mail@readme")
	i.updateCompletions()

	assert.Nil(t, i.atMatches)
}
