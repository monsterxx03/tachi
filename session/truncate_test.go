package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeForTest returns a store over a temp dir with one session whose
// messages.jsonl holds the given lines.
func storeForTest(t *testing.T, lines ...string) (*FileStore, string) {
	t.Helper()
	s, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateSession(&Session{ID: "s1"}))
	writeLines(t, s.messagesPath("s1"), lines)
	return s, "s1"
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func readLines(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// TestTruncateMessagesKeepsThePrefixVerbatim is the property the whole feature
// rests on: what stays behind must be byte-identical to what was there, or the
// prompt a rewind rebuilds would differ from the one that was sent and every
// turn since would miss the provider's prefix cache.
func TestTruncateMessagesKeepsThePrefixVerbatim(t *testing.T) {
	s, id := storeForTest(t, `{"a":1}`, `{"b":"两个"}`, `{"c":3}`, `{"d":4}`)
	before := readLines(t, s.messagesPath(id))

	res, err := s.TruncateMessages(id, 2, "turn-2")
	require.NoError(t, err)
	assert.Equal(t, 2, res.Kept)
	assert.Equal(t, 2, res.Removed)

	after := readLines(t, s.messagesPath(id))
	assert.Equal(t, string(before[:len(`{"a":1}`+"\n"+`{"b":"两个"}`+"\n")]), string(after),
		"the kept prefix must be the same bytes")
	assert.Equal(t, `{"c":3}`+"\n"+`{"d":4}`+"\n", string(readLines(t, res.Sidecar)),
		"the removed tail must move to the sidecar unchanged")
	assert.Equal(t, filepath.Join(s.sessionDir(id), "rewound"), filepath.Dir(res.Sidecar))
}

// TestTruncateMessagesNoOpWhenNothingToRemove keeps the common case cheap: a
// rewind to the newest checkpoint must not write a sidecar or touch the file.
func TestTruncateMessagesNoOpWhenNothingToRemove(t *testing.T) {
	s, id := storeForTest(t, `{"a":1}`, `{"b":2}`)
	before := readLines(t, s.messagesPath(id))

	res, err := s.TruncateMessages(id, 5, "turn-9")
	require.NoError(t, err)
	assert.Equal(t, 2, res.Kept)
	assert.Zero(t, res.Removed)
	assert.Empty(t, res.Sidecar)
	assert.Equal(t, string(before), string(readLines(t, s.messagesPath(id))))
	_, err = os.Stat(filepath.Join(s.sessionDir(id), "rewound"))
	assert.True(t, os.IsNotExist(err), "no tail means no sidecar directory")
}

// TestTruncateMessagesToZeroMovesEverything covers rewinding to the very first
// turn, where the whole history is the tail.
func TestTruncateMessagesToZeroMovesEverything(t *testing.T) {
	s, id := storeForTest(t, `{"a":1}`, `{"b":2}`)

	res, err := s.TruncateMessages(id, 0, "turn-1")
	require.NoError(t, err)
	assert.Zero(t, res.Kept)
	assert.Equal(t, 2, res.Removed)

	assert.Empty(t, readLines(t, s.messagesPath(id)))
	assert.Equal(t, `{"a":1}`+"\n"+`{"b":2}`+"\n", string(readLines(t, res.Sidecar)))
}

// TestTruncateMessagesWithoutATrailingNewlineCountsTheLastRecord: a file whose
// last line lacks a newline still holds that record, and a rewind past it must
// not silently leave it behind.
func TestTruncateMessagesWithoutATrailingNewlineCountsTheLastRecord(t *testing.T) {
	s, id := storeForTest(t, `{"a":1}`)
	require.NoError(t, os.WriteFile(s.messagesPath(id), []byte(`{"a":1}`+"\n"+`{"b":2}`), 0o600))

	res, err := s.TruncateMessages(id, 1, "turn-2")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Kept)
	assert.Equal(t, 1, res.Removed)
	assert.Equal(t, `{"a":1}`+"\n", string(readLines(t, s.messagesPath(id))))
	assert.Equal(t, `{"b":2}`, string(readLines(t, res.Sidecar)))
}

// TestTruncateMessagesWithNoFileIsANoOp keeps the first turn of a session from
// failing: there may be nothing recorded at all yet.
func TestTruncateMessagesWithNoFileIsANoOp(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateSession(&Session{ID: "s1"}))

	res, err := s.TruncateMessages("s1", 0, "turn-1")
	require.NoError(t, err)
	assert.Zero(t, res.Removed)
	assert.Empty(t, res.Sidecar)
}

// TestSplitJSONLAtCoverEveryCutPoint pins the ONE statement of "where does record
// number N start", which a rewind and a fork share: a keep PAST the end of the
// blob has to keep the whole blob. The loop that finds the keep-th newline never
// gets there in that case, so without the guard at the end the split calls an
// uncut history an empty head, and the fork that asked for it starts with no
// conversation at all.
func TestSplitJSONLAtCoverEveryCutPoint(t *testing.T) {
	const twoLines = `{"a":1}` + "\n" + `{"b":2}` + "\n"
	const twoLinesNoTrailing = `{"a":1}` + "\n" + `{"b":2}`

	tests := []struct {
		name      string
		data      string
		keep      int
		wantHead  string
		wantTail  string
		wantTotal int
	}{
		{"keep past the end keeps everything", twoLines, 5, twoLines, "", 2},
		{"keep past the end without a trailing newline", twoLinesNoTrailing, 5, twoLinesNoTrailing, "", 2},
		{"keep equal to the count keeps everything", twoLines, 2, twoLines, "", 2},
		{"keep equal without a trailing newline", twoLinesNoTrailing, 2, twoLinesNoTrailing, "", 2},
		{"keep inside splits", twoLines, 1, `{"a":1}` + "\n", `{"b":2}` + "\n", 2},
		{"keep zero takes everything", twoLines, 0, "", twoLines, 2},
		{"an empty blob has nothing to split", "", 3, "", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			head, tail, total := splitJSONLAt([]byte(tt.data), tt.keep)
			assert.Equal(t, tt.wantHead, string(head), "head")
			assert.Equal(t, tt.wantTail, string(tail), "tail")
			assert.Equal(t, tt.wantTotal, total, "total")
		})
	}
}
