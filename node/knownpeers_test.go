package node

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

const (
	nasID  = "aaaa1111aaaa1111aaaa1111aaaa1111"
	vpsID  = "bbbb2222bbbb2222bbbb2222bbbb2222"
	nasURL = "https://100.64.0.3:7080"
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

// emptyPins is a pin record with nothing in it, over a file of its own: what every test that needs
// identities to be optional starts from.
func emptyPins(t *testing.T) *knownPeers {
	t.Helper()
	p, err := openKnownPeers(t.TempDir()+"/peer-identities.json", nil)
	require.NoError(t, err)
	return p
}

// The lookup the sidebar needs: "which of my machines is this node id". Only the RECORD answers it —
// the caller's own name never does (any key can claim any name) — and only OUTBOUND contact writes
// that record, so an id this machine has never reached resolves to nothing. That is a correct
// answer, and the one a receive-only machine gets for every caller it has.
func TestPeerAliasesAnswersWhichMachineANodeIDIs(t *testing.T) {
	usePinnedBase(t)
	writePinned(t, `{
  "`+nasID+`": {"alias": "home-nas", "url": "`+nasURL+`", "first_seen": "2026-10-01T00:00:00Z"},
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

// A record keyed by something that is not a node id is refused rather than migrated: keys that are
// names are the shape before this one, so every identity check this file exists for would silently
// miss and the next contact would be pinned afresh — a new first-contact window, which is the one
// thing the record is for.
//
// The read-only path (a label on a row) has nothing to guess at either; the write path says what to
// do about it, and does NOT touch the file.
func TestARecordKeyedBySomethingOtherThanAnIDIsRefused(t *testing.T) {
	usePinnedBase(t)
	legacy := `{"home-nas": {"node_id": "` + nasID + `", "url": "` + nasURL + `", "first_seen": "2026-10-01T00:00:00Z"}}`
	writePinned(t, legacy)

	require.Empty(t, PeerAliases(), "nothing here is keyed by an identity, so there is no label to give")

	_, err := openKnownPeers(config.PeerIdentitiesPath(), nil)
	require.Error(t, err, "a record we cannot interpret must not be read as an empty one")
	require.Contains(t, err.Error(), "not a node id", "the reason has to name what is wrong")
	require.Contains(t, err.Error(), config.PeerIdentitiesPath(), "and where it is")
	require.Contains(t, err.Error(), "Delete it", "and what to do about it")

	raw, readErr := os.ReadFile(config.PeerIdentitiesPath())
	require.NoError(t, readErr)
	require.Equal(t, legacy, string(raw), "refusing means refusing: the file is left exactly as it was")
}

// A rename is a rename: the table is keyed by identity, so one machine has ONE record and its label
// follows the last contact. The shape before this one left a second, stale record behind for it.
func TestARenameKeepsOneRecordAndFollowsIt(t *testing.T) {
	usePinnedBase(t)
	p, err := openKnownPeers(config.PeerIdentitiesPath(), nil)
	require.NoError(t, err)

	require.NoError(t, p.confirm("nas", nasURL, nasID))
	require.NoError(t, p.confirm("home-nas", nasURL, nasID))

	items := readPinnedPeers()
	require.Len(t, items, 1, "a rename must not leave a second record behind")
	require.Equal(t, "home-nas", items[nasID].Alias, "the label follows the handshake's own words")
	require.False(t, items[nasID].FirstSeen.IsZero(), "the first sighting stays")

	alias, ok := p.aliasFor(nasID)
	require.True(t, ok)
	require.Equal(t, "home-nas", alias)
}

// Two machines sharing a label is now an ordinary state rather than a refusal, and the record says so
// by having two entries: the label stopped being a key when a tool argument started naming machines
// by identity, and with it went the rule that protected the old key. What still separates them is the
// identity — and the config's own duplicate-label check, which is about how a list READS
// (node/peers.go).
func TestTwoMachinesMayShareALabelInTheRecord(t *testing.T) {
	usePinnedBase(t)
	p, err := openKnownPeers(config.PeerIdentitiesPath(), nil)
	require.NoError(t, err)

	require.NoError(t, p.confirm("nas", nasURL, nasID))
	require.NoError(t, p.confirm("nas", "https://100.64.0.4:7080", vpsID))

	items := readPinnedPeers()
	require.Len(t, items, 2, "two identities are two records, whatever they are called")
	require.Equal(t, "nas", items[nasID].Alias)
	require.Equal(t, "nas", items[vpsID].Alias)
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

	p, err := openKnownPeers(config.PeerIdentitiesPath(), nil)
	require.NoError(t, err, "no record yet is not an error")
	require.Error(t, p.confirm("nas", nasURL, nasID))
	require.Empty(t, p.items, "and the failed pin is not kept in memory either")
}

// A node id is the one thing that may be stored as an identity: anything else is a bug this side
// (the id is a hash of the presented key), and a record keyed by it would be a record nothing
// matches.
func TestAnIdentityThatIsNotANodeIDIsRefused(t *testing.T) {
	p := emptyPins(t)
	require.Error(t, p.confirm("nas", nasURL, "home-nas"))
	require.Empty(t, p.items)
}

// The address is how a config line finds the identity it already proved, BEFORE this machine has
// spoken to that peer in this process — which is what lets a restart show the machine it knows rather
// than a row reading "身份未记录" until the first turn happens to contact it.
func TestAPinnedIdentityIsFoundByTheAddressItWasReachedAt(t *testing.T) {
	usePinnedBase(t)
	p, err := openKnownPeers(config.PeerIdentitiesPath(), nil)
	require.NoError(t, err)
	require.NoError(t, p.confirm("home-nas", nasURL, nasID))

	id, ok := p.pinnedIdentityAt(nasURL)
	require.True(t, ok)
	require.Equal(t, nasID, id)

	_, ok = p.pinnedIdentityAt("https://100.64.0.9:7080")
	require.False(t, ok, "an address nobody was reached at has no identity, and none may be invented")

	// It survives a restart, which is the only reason it is on disk at all.
	again, err := openKnownPeers(config.PeerIdentitiesPath(), nil)
	require.NoError(t, err)
	id, ok = again.pinnedIdentityAt(nasURL)
	require.True(t, ok)
	require.Equal(t, nasID, id)
}

// An address that changed hands resolves to the machine that is there NOW: the most recently first
// seen record wins, and the older one is still on disk (a pin is never deleted — node/knownpeers.go).
func TestAnAddressThatChangedHandsResolvesToTheNewerMachine(t *testing.T) {
	usePinnedBase(t)
	writePinned(t, `{
  "`+nasID+`": {"alias": "nas", "url": "`+nasURL+`", "first_seen": "2026-10-01T00:00:00Z"},
  "`+vpsID+`": {"alias": "vps", "url": "`+nasURL+`", "first_seen": "2026-10-02T00:00:00Z"}
}`)

	p, err := openKnownPeers(config.PeerIdentitiesPath(), nil)
	require.NoError(t, err)
	id, ok := p.pinnedIdentityAt(nasURL)
	require.True(t, ok)
	require.Equal(t, vpsID, id)

	require.Len(t, readPinnedPeers(), 2, "resolving is not forgetting")
}

// An empty label is an ordinary record now: a config line's label is optional, and a label that is
// absent has to keep answering "this machine has no name for it" rather than turning into a name.
func TestARecordWithNoLabelIsNotAName(t *testing.T) {
	usePinnedBase(t)
	p, err := openKnownPeers(config.PeerIdentitiesPath(), nil)
	require.NoError(t, err)
	require.NoError(t, p.confirm("", nasURL, nasID))

	_, ok := p.aliasFor(nasID)
	require.False(t, ok, "no label means no label")
	require.Equal(t, map[string]string{}, PeerAliases())

	id, found := p.pinnedIdentityAt(nasURL)
	require.True(t, found, "and it is still a record, filed where it was reached")
	require.Equal(t, nasID, id)
}
