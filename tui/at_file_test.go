package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/agent/sessionref"
	"github.com/monsterxx03/tachi/session"
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

// staticAgentSource is the node namespace over a fixed peer list, the way a real one
// sits over node.PeerList.
func staticAgentSource(peers ...agentref.Peer) *agentref.Source {
	return agentref.New(func() []agentref.Peer { return peers })
}

// TestAtSessionCompletion covers the conversation namespace: "#s:" searches the
// session store, and Tab inserts the reference the expansion reads back — never the
// session's content.
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
	i.textarea.SetValue("#s:")
	i.updateCompletions()
	require.Len(t, i.atMatches, 1, "the current session is not offered")
	assert.Equal(t, "#s:2026-10-04-100000-hugo0001", i.atMatches[0].Ref)
	assert.Contains(t, i.atMatches[0].Label, "安装Hugo")
	assert.Contains(t, i.atMatches[0].Label, "2026-10-04 20:32")

	// A query narrows it, and the file namespace is untouched by a session row.
	i.textarea.SetValue("#s:hugo")
	i.updateCompletions()
	require.Len(t, i.atMatches, 1)

	i, _ = i.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, "#s:2026-10-04-100000-hugo0001 ", i.textarea.Value())
}

// Without a session store the namespace offers nothing — the file completion a
// frontend with no store still has keeps working.
func TestAtSessionCompletionWithoutAStore(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)

	i.textarea.SetValue("#s:hugo")
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

// The bare '#' offers the namespaces themselves, so the spelling does not have to be
// remembered; picking one opens it rather than ending the reference. That last part is the whole
// gesture: a trailing space would close the reference before the reader got to type anything.
func TestHashOpensWithBothNamespaces(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)
	i.SetAgentSource(staticAgentSource(agentref.Peer{
		Label: "nas", Name: "nas", NodeID: "cd726b44aa11bb22",
	}))

	i.textarea.SetValue("#")
	i.updateCompletions()

	require.Len(t, i.atMatches, 2)
	assert.Equal(t, []string{"#a:", "#s:"}, atLabels(i.atMatches))
	assert.True(t, i.atMatches[0].Prefix)
	assert.Contains(t, i.atMatches[0].Note, "节点")

	// Typing part of a prefix narrows the list instead of leaving it: one gesture.
	i.textarea.SetValue("#s")
	i.updateCompletions()
	require.Len(t, i.atMatches, 1)
	assert.Equal(t, "#s:", i.atMatches[0].Label)

	// Tab on the first row inserts the prefix, keeps the popup up, and re-reads the SAME
	// reference — so what it now shows is that namespace's rows.
	i.textarea.SetValue("#")
	i.updateCompletions()
	i, _ = i.Update(tea.KeyPressMsg{Code: tea.KeyTab})

	assert.Equal(t, "#a:", i.textarea.Value(), "a namespace row opens the reference, it does not end it")
	require.NotNil(t, i.atMatches, "the popup stays up on the namespace it just opened")
	assert.True(t, i.atMatches[0].Agent)
	assert.Contains(t, i.atMatches[0].Label, "nas")
}

// A node row carries the three things a reader needs to tell machines apart: what the node is
// called, the identity THIS machine pinned — which is the string the model must hand to
// DelegateTask, and therefore what the row's reference inserts — and what the last look found.
func TestAtAgentCompletion(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)
	i.SetAgentSource(staticAgentSource(
		agentref.Peer{Label: "ugreen-nas", Name: "ugreen-nas", NodeID: "cd726b446c34386e",
			Note: "家里的 NAS", Description: "A Tachi node", Reachable: true, Checked: true},
		agentref.Peer{Name: "hk-vps"},
	))

	i.textarea.SetValue("#a:")
	i.updateCompletions()
	// Only the machine this machine can NAME is offered: this picker inserts references, and a
	// reference carries an identity (agentref.Search).
	require.Len(t, i.atMatches, 1)

	// The reference IS the identity: that is what the tool argument takes, and the scan reads back
	// exactly what the row inserts.
	assert.Equal(t, "#a:cd726b446c34386e", i.atMatches[0].Ref)
	assert.Contains(t, i.atMatches[0].Label, "ugreen-nas", "the row says what the node is called")
	assert.Contains(t, i.atMatches[0].Label, "cd726b44", "the pinned identity, truncated for the row")
	assert.Contains(t, i.atMatches[0].Label, "可用")
	assert.Equal(t, "A Tachi node", i.atMatches[0].Note)

	// A query narrows it, and Tab finishes the reference with a space.
	i.textarea.SetValue("#a:ugreen")
	i.updateCompletions()
	require.Len(t, i.atMatches, 1)
	i, _ = i.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, "#a:cd726b446c34386e ", i.textarea.Value())
}

// A '#' that opens no namespace is ordinary text: no popup, and nothing to expand. This is what
// keeps "#include", "#123" and a heading out of the way.
func TestHashWithoutANamespaceIsPlainText(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)
	i.SetAgentSource(staticAgentSource(agentref.Peer{Name: "nas", NodeID: "aa11bb22aa11bb22aa11bb22aa11bb22"}))

	for _, value := range []string{"#include", "#123", "#x:y", "C#a:nas"} {
		i.textarea.SetValue(value)
		i.updateCompletions()
		assert.Nil(t, i.atMatches, "no popup over %q", value)
	}
}

// One message may hold several references, of either mark, and the popup follows the one the
// CARET is in — not "the reference", which stops being a thing the moment there are two.
func TestSeveralReferencesFollowTheCaret(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)
	i.SetAgentSource(staticAgentSource(agentref.Peer{Name: "nas", NodeID: "aa11bb22aa11bb22aa11bb22aa11bb22"}))

	i.textarea.SetValue("#a:nas 和 @readme")
	i.textarea.CursorEnd()
	i.updateCompletions()

	require.NotNil(t, i.atMatches)
	assert.Equal(t, "readme", i.atQuery, "the last reference is the one being edited")
	assert.Equal(t, byte('@'), i.atMark)
	assert.Contains(t, atLabels(i.atMatches), "readme.md")

	// And the row inserts into THAT reference, leaving the earlier one alone.
	i, _ = i.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, "#a:nas 和 @readme.md ", i.textarea.Value())
}

// A picker row's reference must be one the scan reads back: what the popup offers and what the
// backend routes are the same spelling, or the feature is a pair of near-misses.
func TestPickerRowsInsertSpellingTheScannerKnows(t *testing.T) {
	seedAtFileDir(t)
	i := NewInputArea(10, "", nil)
	i.SetAgentSource(staticAgentSource(agentref.Peer{Name: "nas", NodeID: "aa11bb22aa11bb22"}))

	i.textarea.SetValue("#")
	i.updateCompletions()
	require.Len(t, i.atMatches, 2)

	for _, m := range i.atMatches {
		require.NotEmpty(t, m.Ref)
		assert.Equal(t, byte('#'), m.Ref[0])
		ns, _ := atfile.HashQueryNamespace(strings.TrimPrefix(m.Ref, "#"))
		assert.NotEqual(t, atfile.HashNamespaceNone, ns,
			"%q opens the namespace it claims to, as the expansion scan reads it", m.Ref)
	}
}
