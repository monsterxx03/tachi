package acp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

func newTempSessionManager(t *testing.T) *session.Manager {
	t.Helper()
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	return session.NewManagerWithStore(store, logger.Default())
}

func TestFollowCompactedChain_NoChild(t *testing.T) {
	sm := newTempSessionManager(t)
	s, err := sm.New("p", "/tmp")
	require.NoError(t, err)

	got := followCompactedChain(sm, s)
	assert.Equal(t, s.ID, got.ID)
	assert.Equal(t, s.ID, sm.Current().ID)
}

func TestFollowCompactedChain_FollowsToNewest(t *testing.T) {
	sm := newTempSessionManager(t)
	a, err := sm.New("p", "/tmp")
	require.NoError(t, err)
	b, err := sm.New("p", "/tmp")
	require.NoError(t, err)
	c, err := sm.New("p", "/tmp")
	require.NoError(t, err)

	// Link A -> B -> C (as FinalizeCompact would).
	a.CompactedChildID = b.ID
	require.NoError(t, sm.UpdateMeta(a))
	b.CompactedParentID = a.ID
	b.CompactedChildID = c.ID
	require.NoError(t, sm.UpdateMeta(b))
	c.CompactedParentID = b.ID
	require.NoError(t, sm.UpdateMeta(c))

	loaded, err := sm.Load(a.ID)
	require.NoError(t, err)

	got := followCompactedChain(sm, loaded)
	assert.Equal(t, c.ID, got.ID)
	// The newest descendant must become the current session.
	assert.Equal(t, c.ID, sm.Current().ID)
}

func TestFollowCompactedChain_MissingChild(t *testing.T) {
	sm := newTempSessionManager(t)
	a, err := sm.New("p", "/tmp")
	require.NoError(t, err)
	a.CompactedChildID = "nonexistent-session-id"
	require.NoError(t, sm.UpdateMeta(a))

	loaded, err := sm.Load(a.ID)
	require.NoError(t, err)

	// Best-effort: child can't be loaded — stay on A.
	got := followCompactedChain(sm, loaded)
	assert.Equal(t, a.ID, got.ID)
	assert.Equal(t, a.ID, sm.Current().ID)
}

func TestFollowCompactedChain_CycleSafe(t *testing.T) {
	sm := newTempSessionManager(t)
	a, err := sm.New("p", "/tmp")
	require.NoError(t, err)
	b, err := sm.New("p", "/tmp")
	require.NoError(t, err)

	// A -> B -> A (corrupt metadata cycle).
	a.CompactedChildID = b.ID
	require.NoError(t, sm.UpdateMeta(a))
	b.CompactedChildID = a.ID
	require.NoError(t, sm.UpdateMeta(b))

	loaded, err := sm.Load(a.ID)
	require.NoError(t, err)

	// Must terminate; stops at B since A was already seen.
	got := followCompactedChain(sm, loaded)
	assert.Equal(t, b.ID, got.ID)
}

func TestACPSessionManagerRekey(t *testing.T) {
	mgr := NewACPSessionManager()
	sess := mgr.New(t.Context(), "/tmp", nil, nil, nil, nil)
	oldID := sess.ID

	mgr.Rekey(oldID, "new-id")

	_, ok := mgr.Get(oldID)
	assert.False(t, ok, "old ID should no longer resolve")

	got, ok := mgr.Get("new-id")
	assert.True(t, ok, "new ID should resolve")
	assert.Equal(t, "new-id", got.ID)
	assert.Equal(t, "new-id", sess.ID, "session struct ID should be updated")

	// Rekey with unknown from-ID is a no-op.
	mgr.Rekey("missing", "x")
	_, ok = mgr.Get("x")
	assert.False(t, ok)
}
