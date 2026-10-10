package sessionref

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedStore builds a store with two sessions — "2026-10-04-100000-hugo0001"
// (titled 安装Hugo, two recorded messages) and "2026-10-04-200000-other002"
// (titled 别的会话) — and a Source over it.
func seedStore(t *testing.T) *Source {
	t.Helper()
	dir := t.TempDir()
	store, err := session.NewFileStore(dir)
	require.NoError(t, err)

	base := time.Date(2026, 10, 4, 10, 0, 0, 0, time.Local)
	hugo := &session.Session{
		ID: "2026-10-04-100000-hugo0001", Title: "安装Hugo",
		WorkingDir: "/home/me/repos/blog", CreatedAt: base, UpdatedAt: base,
	}
	other := &session.Session{
		ID: "2026-10-04-200000-other002", Title: "别的会话",
		WorkingDir: "/home/me/repos/tachi", CreatedAt: base, UpdatedAt: base.Add(2 * time.Hour),
	}
	require.NoError(t, store.CreateSession(hugo))
	require.NoError(t, store.CreateSession(other))
	for _, text := range []string{"帮我装 hugo", "装好了"} {
		require.NoError(t, store.AppendMessage(hugo.ID, &session.Message{
			Type: session.MessageTypeUser, Content: text,
		}))
	}

	return New(store.ListSessions, dir)
}

func TestResolveByFullIDCarriesThePointer(t *testing.T) {
	src := seedStore(t)

	target, ok := src.Resolve("2026-10-04-100000-hugo0001")
	require.True(t, ok)
	assert.Equal(t, "安装Hugo", target.Title)
	assert.Equal(t, "/home/me/repos/blog", target.WorkingDir)
	assert.Equal(t, 2, target.Messages, "the pointer states how much there is")
	assert.Equal(t, filepath.Join(src.dir, "2026-10-04-100000-hugo0001", "messages.jsonl"),
		target.Transcript)
}

// A store id is a timestamp with a random tail, so the tail is what a human
// retypes — as long as it names exactly one session.
func TestResolveAcceptsAUniqueSuffix(t *testing.T) {
	src := seedStore(t)

	target, ok := src.Resolve("hugo0001")
	require.True(t, ok)
	assert.Equal(t, "2026-10-04-100000-hugo0001", target.ID)

	_, ok = src.Resolve("nope")
	assert.False(t, ok)
}

// Two sessions ending in the same characters are both plausible answers, and a
// reference that could mean either must expand to neither: silently picking one
// would hand the model the wrong conversation's transcript.
func TestResolveRefusesAnAmbiguousSuffix(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewFileStore(dir)
	require.NoError(t, err)
	for _, id := range []string{"2026-10-04-100000-aaa9999", "2026-10-04-200000-bbb9999"} {
		require.NoError(t, store.CreateSession(&session.Session{ID: id, Title: id}))
	}

	_, ok := New(store.ListSessions, dir).Resolve("9999")
	assert.False(t, ok)

	// The full id is never ambiguous.
	_, ok = New(store.ListSessions, dir).Resolve("2026-10-04-100000-aaa9999")
	assert.True(t, ok)
}

func TestSearchFindsByTitleAndExcludesTheCurrentSession(t *testing.T) {
	src := seedStore(t)

	all := src.Search("", "", 10)
	require.Len(t, all, 2)
	assert.Equal(t, "2026-10-04-200000-other002", all[0].ID, "newest update first")
	assert.Zero(t, all[0].Messages, "searching never counts transcripts")

	hit := src.Search("hugo", "", 10)
	require.Len(t, hit, 1)
	assert.Equal(t, "安装Hugo", hit[0].Title)

	// The picker opens INSIDE the current conversation; offering it would be a
	// row that references itself.
	excludingCurrent := src.Search("", "2026-10-04-100000-hugo0001", 10)
	require.Len(t, excludingCurrent, 1)
	assert.Equal(t, "2026-10-04-200000-other002", excludingCurrent[0].ID)
}

// The ranking the picker leans on: a session whose title the query matches
// cleanly comes before one that merely contains the same characters scattered.
func TestSearchRanksACleanMatchFirst(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewFileStore(dir)
	require.NoError(t, err)

	base := time.Date(2026, 10, 4, 10, 0, 0, 0, time.Local)
	// The scattered match is the NEWER one, so only the score can put the other
	// session on top.
	require.NoError(t, store.CreateSession(&session.Session{
		ID: "2026-10-04-100000-aaa", Title: "Hugo", CreatedAt: base, UpdatedAt: base,
	}))
	require.NoError(t, store.CreateSession(&session.Session{
		ID: "2026-10-04-200000-bbb", Title: "关于 hugo 的杂记", CreatedAt: base, UpdatedAt: base.Add(time.Hour),
	}))

	got := New(store.ListSessions, dir).Search("hugo", "", 10)
	require.Len(t, got, 2)
	assert.Equal(t, "2026-10-04-100000-aaa", got[0].ID)
}

func TestSearchLimit(t *testing.T) {
	src := seedStore(t)
	assert.Len(t, src.Search("", "", 1), 1)
	assert.Len(t, src.Search("", "", 0), 2, "an unset limit still returns everything here")
}

// The picker's rows carry the session's kind, and so does the resolved target: one value from
// one record, so "what the picker offered" and "what the reference means" cannot drift (§13).
func TestRowsAndTargetsCarryTheSessionKind(t *testing.T) {
	managed := &session.Session{ID: "2026-10-10-120000-aaaa1111", Title: "远端执行", Kind: session.KindA2AExecution}
	ordinary := &session.Session{ID: "2026-10-10-130000-bbbb2222", Title: "我自己的对话"}
	src := New(func() ([]*session.Session, error) { return []*session.Session{managed, ordinary}, nil }, t.TempDir())

	rows := src.Search("", "", 10)
	if len(rows) != 2 {
		t.Fatalf("expected both sessions, got %d", len(rows))
	}
	var managedRow, ordinaryRow atfile.SessionTarget
	for _, r := range rows {
		switch r.ID {
		case managed.ID:
			managedRow = r
		case ordinary.ID:
			ordinaryRow = r
		}
	}
	if managedRow.ManagedKind != session.KindA2AExecution {
		t.Errorf("a managed session's row must carry its kind, got %q", managedRow.ManagedKind)
	}
	if ordinaryRow.ManagedKind != "" {
		t.Errorf("an ordinary conversation has no kind, got %q", ordinaryRow.ManagedKind)
	}

	target, ok := src.Resolve(managed.ID)
	if !ok {
		t.Fatal("the resolved target must exist")
	}
	if target.ManagedKind != managedRow.ManagedKind {
		t.Errorf("the picker and the pointer disagree: row=%q target=%q",
			managedRow.ManagedKind, target.ManagedKind)
	}
}
