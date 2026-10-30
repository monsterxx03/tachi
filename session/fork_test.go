package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forkStore returns a store over a temp dir with no session in it yet, so each
// test can plant exactly the source it needs.
func forkStore(t *testing.T) *FileStore {
	t.Helper()
	s, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	return s
}

// forkSource builds a session with a history worth copying: records (one of them
// carrying an image part), a request log, an image blob, and a metadata record
// whose every situational field must be left behind.
func forkSource(t *testing.T, s *FileStore) *Session {
	t.Helper()
	src := &Session{
		ID:             "s1",
		Title:          "改导出",
		ProviderName:   "alpha",
		WorkingDir:     "/tmp/ws",
		AdditionalDirs: []string{"/tmp/shared"},
		ProjectID:      "p1",
		ThinkingLevel:  "high",
		SkipDream:      true,
		SkipApproval:   true,
		// The fields a fork must NOT carry: they describe the source's situation.
		ThreadID:             "guild:1:channel:2",
		Kind:                 "a2a_execution",
		SourceNodeID:         "node-9",
		LogicalID:            "conv-1",
		Pinned:               true,
		CompactedChildID:     "child-1",
		CompactedParentID:    "parent-1",
		CompactedParentTitle: "压缩前",
	}
	require.NoError(t, s.CreateSession(src))

	ref := strings.Repeat("a", 64)
	writeLines(t, s.messagesPath("s1"), []string{
		`{"type":"user","content":"第一轮"}`,
		`{"type":"assistant","content":"好"}`,
		`{"type":"user","content":"第二轮的图","parts":[{"type":"image","media_type":"image/png","ref":"` + ref + `"}]}`,
		`{"type":"assistant","content":"看到了"}`,
	})
	writeLines(t, s.apiRequestsPath("s1"), []string{
		`{"seq":1,"system_prompt":"sys"}`,
		`{"seq":2,"system_prompt":"sys"}`,
	})
	require.NoError(t, os.MkdirAll(s.imagesDir("s1"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(s.imagesDir("s1"), ref), []byte("png-bytes"), 0o600))
	return src
}

// TestForkSessionCopiesTheHeadVerbatimAndLeavesTheSourceAlone is the property the
// rest of the feature rests on: the kept records are the SAME BYTES the source had
// (anything else would send a different prefix than the one the provider cached),
// the fork's own files hold them, and the source is byte-for-byte untouched.
func TestForkSessionCopiesTheHeadVerbatimAndLeavesTheSourceAlone(t *testing.T) {
	s := forkStore(t)
	src := forkSource(t, s)
	before := readLines(t, s.messagesPath(src.ID))

	fork, err := s.ForkSession(src, 2, 1, "改导出 · 分支")
	require.NoError(t, err)

	assert.NotEqual(t, src.ID, fork.ID)
	assert.Equal(t, "改导出 · 分支", fork.Title)
	assert.Equal(t, `{"type":"user","content":"第一轮"}`+"\n"+`{"type":"assistant","content":"好"}`+"\n",
		string(readLines(t, s.messagesPath(fork.ID))), "the kept prefix must be the same bytes")
	assert.Equal(t, `{"seq":1,"system_prompt":"sys"}`+"\n", string(readLines(t, s.apiRequestsPath(fork.ID))))
	assert.Equal(t, string(before), string(readLines(t, s.messagesPath(src.ID))),
		"the source conversation must not move")
	assert.Equal(t, `{"seq":1,"system_prompt":"sys"}`+"\n"+`{"seq":2,"system_prompt":"sys"}`+"\n",
		string(readLines(t, s.apiRequestsPath(src.ID))), "the source request log must not move")
	_, err = os.Stat(filepath.Join(s.sessionDir(src.ID), rewoundDirName))
	assert.True(t, os.IsNotExist(err), "a fork discards nothing, so it writes no sidecar")
}

// TestForkSessionCarriesTheImageBlobsTheKeptRecordsReferTo: Message.Parts holds
// only a reference, so a fork that copied the records and not the blobs would
// silently lose the images — the conversion degrades the part and the model stops
// being able to see an image it was given.
func TestForkSessionCarriesTheImageBlobsTheKeptRecordsReferTo(t *testing.T) {
	s := forkStore(t)
	src := forkSource(t, s)
	ref := strings.Repeat("a", 64)

	fork, err := s.ForkSession(src, 4, 2, "带图的分支")
	require.NoError(t, err)

	data, err := s.LoadImage(fork.ID, ref)
	require.NoError(t, err, "the referenced blob must exist in the fork")
	assert.Equal(t, "png-bytes", string(data))

	// A record that was CUT OFF brings no blob: nothing that could be converted has
	// a dangling reference.
	uncut, err := s.ForkSession(src, 2, 1, "不带图的分支")
	require.NoError(t, err)
	_, err = s.LoadImage(uncut.ID, ref)
	assert.Error(t, err, "a blob no kept record refers to must not be copied")
}

// TestForkSessionBlobMissingInTheSourceIsNotFatal: the same part degrades in the
// source, so failing the fork over it would take the whole conversation away from
// the reader who came to re-ask a question.
func TestForkSessionBlobMissingInTheSourceIsNotFatal(t *testing.T) {
	s := forkStore(t)
	src := forkSource(t, s)
	ref := strings.Repeat("a", 64)
	require.NoError(t, os.Remove(filepath.Join(s.imagesDir(src.ID), ref)))

	fork, err := s.ForkSession(src, 4, 2, "缺图的分支")
	require.NoError(t, err)
	assert.NotEmpty(t, fork.ID)
	_, err = s.LoadImage(fork.ID, ref)
	assert.Error(t, err, "the blob is still missing — it just did not fail the fork")
}

// TestForkSessionCopiesOnlyWhatMakesItTheSamePlaceToWork pins the whitelist. The
// situational fields are not decoration: a copied ThreadID makes an incoming
// channel message land in the fork, and a copied parent link makes the desktop
// render the original as a folded 「压缩前」 row (and blocks rewinding the fork).
func TestForkSessionCopiesOnlyWhatMakesItTheSamePlaceToWork(t *testing.T) {
	s := forkStore(t)
	src := forkSource(t, s)

	fork, err := s.ForkSession(src, 2, 1, "分支")
	require.NoError(t, err)

	assert.Equal(t, "alpha", fork.ProviderName)
	assert.Equal(t, "/tmp/ws", fork.WorkingDir)
	assert.Equal(t, []string{"/tmp/shared"}, fork.AdditionalDirs)
	assert.Equal(t, "p1", fork.ProjectID)
	assert.Equal(t, "high", fork.ThinkingLevel)
	assert.True(t, fork.SkipDream, "the reader's own standing choice follows the conversation")
	assert.True(t, fork.SkipApproval, "a branch of a conversation worked in 「无需审批」 stays that way")

	assert.Empty(t, fork.ThreadID, "a channel thread belongs to the session it was opened for")
	assert.Empty(t, fork.Kind, "the fork is an ordinary conversation, not a subsystem's")
	assert.Empty(t, fork.SourceNodeID)
	assert.Empty(t, fork.LogicalID)
	assert.False(t, fork.Pinned, "a pin is a promise about the source's row")
	assert.Empty(t, fork.CompactedChildID, "a fork is a sibling, not a successor")
	assert.Empty(t, fork.CompactedParentID)
	assert.Empty(t, fork.CompactedParentTitle)
	assert.False(t, fork.CreatedAt.IsZero())
	assert.False(t, fork.UpdatedAt.Before(fork.CreatedAt))

	// The copy is a session in its own right: it is listed, and it parses back.
	listed, err := s.ListSessions()
	require.NoError(t, err)
	ids := make([]string, 0, len(listed))
	for _, x := range listed {
		ids = append(ids, x.ID)
	}
	assert.Contains(t, ids, fork.ID)
	meta, err := s.LoadMeta(fork.ID)
	require.NoError(t, err)
	assert.Equal(t, "分支", meta.Title)
}

// TestForkSessionCanStartFromAFreshConversation covers a fork at the FIRST turn:
// nothing is copied, and the fork is still a session with the source's workspace.
func TestForkSessionCanStartFromAFreshConversation(t *testing.T) {
	s := forkStore(t)
	src := forkSource(t, s)

	fork, err := s.ForkSession(src, 0, 0, "从头开始")
	require.NoError(t, err)
	assert.Empty(t, readLines(t, s.messagesPath(fork.ID)))
	_, err = os.Stat(s.apiRequestsPath(fork.ID))
	assert.True(t, os.IsNotExist(err), "nothing to record means no request log at all")
	assert.Equal(t, "/tmp/ws", fork.WorkingDir)
}

// TestForkSessionKeepsTheLastRecordWithoutATrailingNewline: the split is the same
// one a rewind uses, and a file whose last line lacks its newline still holds that
// record.
func TestForkSessionKeepsTheLastRecordWithoutATrailingNewline(t *testing.T) {
	s := forkStore(t)
	require.NoError(t, s.CreateSession(&Session{ID: "s1"}))
	require.NoError(t, os.WriteFile(s.messagesPath("s1"), []byte(`{"a":1}`+"\n"+`{"b":2}`), 0o600))

	fork, err := s.ForkSession(&Session{ID: "s1"}, 2, 0, "分支")
	require.NoError(t, err)
	assert.Equal(t, `{"a":1}`+"\n"+`{"b":2}`, string(readLines(t, s.messagesPath(fork.ID))))
}

// TestForkSessionKeepsTheWholeHistoryWhenTheCutIsPastItsEnd: a cut point is a
// checkpoint's record count, and a checkpoint can outlive the history it counts —
// a rewind truncates the conversation first and drops the stale checkpoints after
// it, so a failure in between leaves a count past the new end of the file. "Keep
// the first N records" with N past the end means keeping them all; the other
// reading is a fork that looks like a new conversation while carrying the title of
// the turn it was cut at.
func TestForkSessionKeepsTheWholeHistoryWhenTheCutIsPastItsEnd(t *testing.T) {
	s := forkStore(t)
	src := forkSource(t, s)
	before := readLines(t, s.messagesPath(src.ID))

	fork, err := s.ForkSession(src, 9, 9, "分支")
	require.NoError(t, err)

	assert.Equal(t, string(before), string(readLines(t, s.messagesPath(fork.ID))),
		"a cut past the end of the history keeps every record")
	assert.Equal(t, `{"seq":1,"system_prompt":"sys"}`+"\n"+`{"seq":2,"system_prompt":"sys"}`+"\n",
		string(readLines(t, s.apiRequestsPath(fork.ID))))
}

// TestForkPublishesOnlyAWholeSession pins the write order: a fork's directory is
// built first and its meta.json written LAST, and a session directory without a
// meta.json is not a session at all (ListSessions skips what it cannot parse). So
// a fork interrupted halfway leaves an invisible directory rather than one row in
// the sidebar whose conversation stops mid-turn, and the failure path removes it —
// nothing else would ever collect it, because the cleanup sweep only walks
// sessions it can list.
func TestForkPublishesOnlyAWholeSession(t *testing.T) {
	s := forkStore(t)
	src := forkSource(t, s)

	// The premise, stated on a directory built by hand: history but no meta.json is
	// not in the list.
	half := filepath.Join(s.BaseDir(), "half-built")
	require.NoError(t, os.MkdirAll(half, 0o700))
	writeLines(t, filepath.Join(half, "messages.jsonl"), []string{`{"a":1}`})
	list, err := s.ListSessions()
	require.NoError(t, err)
	for _, x := range list {
		assert.NotEqual(t, "half-built", x.ID)
	}
	require.NoError(t, os.RemoveAll(half))

	// …and a fork that finished is: history, meta.json, and in the list.
	fork, err := s.ForkSession(src, 2, 1, "分支")
	require.NoError(t, err)
	assert.FileExists(t, s.metaPath(fork.ID))
	assert.FileExists(t, s.messagesPath(fork.ID))
	list, err = s.ListSessions()
	require.NoError(t, err)
	found := false
	for _, x := range list {
		if x.ID == fork.ID {
			found = true
		}
	}
	assert.True(t, found, "a finished fork is a session like any other")
}

// TestForkSessionReportsAStoreItCannotWriteInto: a fork that cannot even create
// its directory fails in words and publishes nothing — the source stays the only
// session in the list.
func TestForkSessionReportsAStoreItCannotWriteInto(t *testing.T) {
	base := t.TempDir()
	s, err := NewFileStore(base)
	require.NoError(t, err)
	require.NoError(t, s.CreateSession(&Session{ID: "s1"}))
	writeLines(t, s.messagesPath("s1"), []string{`{"a":1}`})
	require.NoError(t, os.Chmod(base, 0o500))
	t.Cleanup(func() { _ = os.Chmod(base, 0o700) })

	if os.Geteuid() == 0 {
		t.Skip("root can write into a read-only directory")
	}
	fork, err := s.ForkSession(&Session{ID: "s1"}, 1, 0, "分支")
	require.Error(t, err)
	assert.Nil(t, fork)
	entries, rerr := os.ReadDir(base)
	require.NoError(t, rerr)
	require.Len(t, entries, 1, "only the source may be left")
	assert.Equal(t, "s1", entries[0].Name())
}

// TestManagerForkLeavesTheSourceCurrent: which session a manager is ON is its
// owner's decision (the desktop loads the fork itself), so a fork must not move it.
func TestManagerForkLeavesTheSourceCurrent(t *testing.T) {
	s := forkStore(t)
	require.NoError(t, s.CreateSession(&Session{ID: "s1", Title: "改导出"}))
	writeLines(t, s.messagesPath("s1"), []string{`{"a":1}`, `{"b":2}`})
	m := NewManagerWithStore(s, nil)
	_, err := m.Load("s1")
	require.NoError(t, err)

	fork, err := m.Fork(1, 0, "改导出 · 分支")
	require.NoError(t, err)
	assert.Equal(t, "s1", m.Current().ID, "a fork does not move the manager")
	assert.NotEqual(t, "s1", fork.ID)
	assert.Equal(t, `{"a":1}`+"\n", string(readLines(t, s.messagesPath(fork.ID))))
}

// TestManagerForkWithoutAStoreThatCanCopy refuses in words rather than copying
// nothing: the manager's store is an interface, and a store written for a test has
// no session directory to build.
func TestManagerForkWithoutAStoreThatCanCopy(t *testing.T) {
	m := NewManagerWithStore(memoryStore{}, nil)
	m.SetCurrent(&Session{ID: "s1"})
	_, err := m.Fork(1, 0, "分支")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot fork")
}

// memoryStore is a Store with no filesystem behind it: the shape a test double
// has. Every method is written out (no embedded nil interface) so that adding one
// to Store breaks this file at compile time rather than panicking here.
type memoryStore struct{}

func (memoryStore) CreateSession(*Session) error                 { return nil }
func (memoryStore) LoadMeta(string) (*Session, error)            { return nil, os.ErrNotExist }
func (memoryStore) AppendMessage(string, *Message) error         { return nil }
func (memoryStore) LoadMessages(string) ([]Message, error)       { return nil, nil }
func (memoryStore) LoadImage(string, string) ([]byte, error)     { return nil, os.ErrNotExist }
func (memoryStore) ReplaceLastMessage(string, *Message) error    { return nil }
func (memoryStore) UpdateMeta(*Session) error                    { return nil }
func (memoryStore) ListSessions() ([]*Session, error)            { return nil, nil }
func (memoryStore) DeleteSession(string) error                   { return nil }
func (memoryStore) AppendAPIRequest(string, *APIRequest) error   { return nil }
func (memoryStore) LoadAPIRequests(string) ([]APIRequest, error) { return nil, nil }
func (memoryStore) TruncateMessages(string, int, string) (TruncateResult, error) {
	return TruncateResult{}, nil
}
func (memoryStore) TruncateAPIRequests(string, int, string) (TruncateResult, error) {
	return TruncateResult{}, nil
}
