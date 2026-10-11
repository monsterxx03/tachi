package node

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

const (
	nasID = "aaaa1111aaaa1111aaaa1111aaaa1111"
	vpsID = "bbbb2222bbbb2222bbbb2222bbbb2222"
)

func usePinnedBase(t *testing.T) {
	t.Helper()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir("") })
}

func writePinned(t *testing.T, body string) {
	t.Helper()
	require.NoError(t, fileutil.WriteFilePrivate(config.PeerIdentitiesPath(), []byte(body)))
}

// The lookup the sidebar needs: "which of my machines is this node id". Only the RECORD answers it —
// the caller's own name never does (any key can claim any name) — and only OUTBOUND contact writes
// that record, so an id this machine has never reached resolves to nothing. That is a correct
// answer, and the one a receive-only machine gets for every caller it has.
func TestPeerAliasesAnswersWhichMachineANodeIDIs(t *testing.T) {
	usePinnedBase(t)
	writePinned(t, `{
  "`+nasID+`": {"alias": "home-nas", "url": "https://100.64.0.3:7080", "first_seen": "2026-10-01T00:00:00Z"},
  "`+vpsID+`": {"alias": "hk-vps", "first_seen": "2026-10-02T00:00:00Z"}
}`)

	peers := PeerAliases()
	require.Equal(t, "home-nas", peers[nasID])
	require.Equal(t, "hk-vps", peers[vpsID])
	require.Empty(t, peers["cccc3333cccc3333cccc3333cccc3333"],
		"an id this machine has never reached has no name here, and inventing one is the bug")

	// A record that cannot be read yields nothing rather than failing: a label is not worth a
	// sidebar that does not draw.
	require.NoError(t, os.WriteFile(config.PeerIdentitiesPath(), []byte("{not json"), 0o600))
	require.Empty(t, PeerAliases())
}

// A record keyed by ALIAS is refused rather than migrated: its keys are names, so every check this
// file exists for would silently miss and the next contact would be pinned afresh — a new
// first-contact window, which is the one thing the record is for.
//
// The read-only path (a label on a row) has nothing to guess at either; the write path says what to
// do about it, and does NOT touch the file.
func TestARecordKeyedByAliasIsRefusedRatherThanMigrated(t *testing.T) {
	usePinnedBase(t)
	legacy := `{"home-nas": {"node_id": "` + nasID + `", "url": "https://100.64.0.3:7080", "first_seen": "2026-10-01T00:00:00Z"}}`
	writePinned(t, legacy)

	require.Empty(t, PeerAliases(), "nothing here is keyed by an identity, so there is no label to give")

	_, err := openKnownPeers(config.PeerIdentitiesPath(), nil, nil)
	require.Error(t, err, "a record we cannot interpret must not be read as an empty one")
	require.Contains(t, err.Error(), "no name", "the reason has to name what is wrong")
	require.Contains(t, err.Error(), config.PeerIdentitiesPath(), "and where it is")
	require.Contains(t, err.Error(), "Delete it", "and what to do about it")

	raw, readErr := os.ReadFile(config.PeerIdentitiesPath())
	require.NoError(t, readErr)
	require.Equal(t, legacy, string(raw), "refusing means refusing: the file is left exactly as it was")
}

// A rename is a rename: the table is keyed by identity, so one machine has ONE record and its name
// follows the last contact. The old shape left a second, stale record behind for this.
func TestARenameKeepsOneRecordAndFollowsIt(t *testing.T) {
	usePinnedBase(t)
	urls := map[string]string{"nas": "https://100.64.0.3:7080", "home-nas": "https://100.64.0.3:7081"}
	p, err := openKnownPeers(config.PeerIdentitiesPath(), urls, nil)
	require.NoError(t, err)

	require.NoError(t, p.confirm("nas", nasID))
	require.NoError(t, p.confirm("home-nas", nasID))

	items := readPinnedPeers()
	require.Len(t, items, 1, "a rename must not leave a second record behind")
	require.Equal(t, "home-nas", items[nasID].Alias, "the label follows the name the handshake used")
	require.Equal(t, "https://100.64.0.3:7081", items[nasID].URL, "and so does the address")
	require.False(t, items[nasID].FirstSeen.IsZero(), "the first sighting stays")

	alias, ok := p.aliasFor(nasID)
	require.True(t, ok)
	require.Equal(t, "home-nas", alias)
	require.Equal(t, map[string]string{nasID: "home-nas"}, p.identities())
}

// The protection this table exists for, now that the identity is the key: one NAME may not belong to
// two machines. A config line pointed elsewhere — or an impostor claiming a known name — has to be
// refused, and the refusal has to name both ids and the file to edit.
func TestOneNameMayNotBelongToTwoMachines(t *testing.T) {
	usePinnedBase(t)
	p, err := openKnownPeers(config.PeerIdentitiesPath(), nil, nil)
	require.NoError(t, err)
	require.NoError(t, p.confirm("nas", nasID))

	err = p.confirm("nas", vpsID)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	require.Contains(t, err.Error(), nasID, "both ids, so the operator can tell which is which")
	require.Contains(t, err.Error(), vpsID)
	require.Contains(t, err.Error(), config.PeerIdentitiesPath())

	items := readPinnedPeers()
	require.Len(t, items, 1, "and the record is not rewritten by a refusal")
	_, ok := items[nasID]
	require.True(t, ok)
}

// A pin that did not reach disk must not count as verified: the next conversation is checked against
// what is IN the file, so an unwritable path has to fail the handshake rather than pass it.
func TestAPinThatCannotBeWrittenIsNotVerified(t *testing.T) {
	base := t.TempDir()
	config.SetBaseDir(base)
	t.Cleanup(func() { config.SetBaseDir("") })
	// A read-only directory: nothing can be written into it, so the pin cannot land.
	require.NoError(t, os.Chmod(base, 0o500))
	t.Cleanup(func() { _ = os.Chmod(base, 0o700) }) // so the temp tree can be cleaned up

	p, err := openKnownPeers(config.PeerIdentitiesPath(), nil, nil)
	require.NoError(t, err, "no record yet is not an error")
	require.Error(t, p.confirm("nas", nasID))
	require.Empty(t, p.items, "and the failed pin is not kept in memory either")
}

// nodeID is the direction the sender needs: which identity did this name prove?
func TestNodeIDResolvesANameToItsIdentity(t *testing.T) {
	usePinnedBase(t)
	writePinned(t, `{"`+nasID+`": {"alias": "home-nas", "first_seen": "2026-10-01T00:00:00Z"}}`)

	p, err := openKnownPeers(config.PeerIdentitiesPath(), nil, nil)
	require.NoError(t, err)

	id, ok := p.nodeID("home-nas")
	require.True(t, ok)
	require.Equal(t, nasID, id)

	_, ok = p.nodeID("never-reached")
	require.False(t, ok, "a name with no record has no identity, and none may be invented")
}
