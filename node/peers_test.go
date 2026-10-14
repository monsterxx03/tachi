package node

import (
	"context"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
)

// A peer is named ONE way: its identity. Not a label, not a rule with exceptions — a label is a
// string its owner chooses, and a delegation must not go wherever a name currently points.
//
// A unique PREFIX of an id works, because nobody copies thirty-two hex characters when eight will
// do, and an ambiguous prefix is refused rather than guessed at.
func TestLookupResolvesAnIdentityOrAUniquePrefixOfOne(t *testing.T) {
	usePinnedBase(t)
	writePinned(t, `{
  "abcd1111abcd1111abcd1111abcd1111": {"alias": "home-nas", "url": "https://127.0.0.1:1", "first_seen": "2026-10-01T00:00:00Z"},
  "abcd2222abcd2222abcd2222abcd2222": {"alias": "hk-vps", "url": "https://127.0.0.1:2", "first_seen": "2026-10-02T00:00:00Z"}
}`)
	// Two DIFFERENT addresses: the address is what a config line's pin is filed under, so two lines
	// sharing one would be two lines pointing at one machine (which the directory merges).
	dir, err := newPeerDirectory([]config.PeerConfig{
		{Name: "home-nas", URL: "https://127.0.0.1:1"},
		{Name: "hk-vps", URL: "https://127.0.0.1:2"},
	})
	require.NoError(t, err)
	known, err := openKnownPeers(config.PeerIdentitiesPath(), nil)
	require.NoError(t, err)
	dir.known = known

	ctx := context.Background()
	for _, ref := range []string{
		"abcd1111abcd1111abcd1111abcd1111",
		"abcd1111abcd1111abcd1111abcd1111"[:12],
	} {
		by, err := dir.Lookup(ctx, ref)
		require.NoError(t, err, "looking up %q", ref)
		require.Equal(t, "home-nas", by.Name(), "looking up %q", ref)
		require.Equal(t, "abcd1111abcd1111abcd1111abcd1111", by.Identity)
	}

	// An ambiguous prefix is refused rather than guessed: sending work to the wrong machine is worse
	// than asking for more characters.
	_, err = dir.Lookup(ctx, "abcd")
	require.Error(t, err)
	require.Contains(t, err.Error(), "matches 2 nodes")

	// A LABEL is refused, and the refusal names the identity to use instead — the mistake a reader
	// will actually make, since a label is what every row and the reminder show.
	_, err = dir.Lookup(ctx, "home-nas")
	require.Error(t, err)
	require.Contains(t, err.Error(), "label")
	require.Contains(t, err.Error(), "abcd1111", "the answer has to include what to use")

	// An unknown id gets the whole directory back, shown the way a reader sees it, so the model can
	// pick something real.
	_, err = dir.Lookup(ctx, "ffff")
	var unknown *UnknownPeerError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, []string{"hk-vps (abcd2222)", "home-nas (abcd1111)"}, unknown.Known)
}

// A tool argument can name a machine this one has never met, and naming it is what connects: the
// resolution asks the entry that holds no identity yet, and the handshake that comes back is what
// makes the name resolvable.
//
// This is the pipe entry's only way in. An interactive turn is told the identities by the reminder's
// own probe, but `-p` runs with reminders off by construction, so without this a delegation from a
// one-off run could never be addressed at all (design §二: 启动后/刷新时去读一次卡片).
func TestNamingAMachineThisOneHasNotMetIsWhatConnectsToOne(t *testing.T) {
	const id = "99998888999988889999888899998888"
	dir, err := newPeerDirectory([]config.PeerConfig{{Name: "nas", URL: "https://127.0.0.1:2"}})
	require.NoError(t, err)
	dir.known = emptyPins(t)

	reads := 0
	dir.resolveEntry = func(_ context.Context, e peerEntry) (*a2a.AgentCard, string, error) {
		reads++
		// The contact: the handshake proves the identity, and the card is what it reads on the way.
		require.Equal(t, "https://127.0.0.1:2", e.Addrs[0])
		require.NoError(t, dir.known.confirm("nas", "https://127.0.0.1:2", id))
		return &a2a.AgentCard{Name: "Tachi"}, "https://127.0.0.1:2", nil
	}

	got, err := dir.Lookup(context.Background(), id)
	require.NoError(t, err, "the machine was reached by being named")
	require.Equal(t, id, got.Identity)
	require.Equal(t, "nas", got.Name())
	require.Equal(t, 1, reads)

	// And now it is a machine like any other: the answer is in the directory, so the next lookup
	// costs nothing.
	again, err := dir.Lookup(context.Background(), id[:8])
	require.NoError(t, err)
	require.Equal(t, id, again.Identity)
	require.Equal(t, 1, reads, "a machine once reached is not reached again to be named")
}

// A pin whose machine is no longer anywhere in the directory is a real answer, and the one `Lookup`
// gives is the same one `byIdentity` would: the machine cannot be reached, because there is no entry
// to reach it at.
func TestLookupRefusesAnIdentityWithNoEntry(t *testing.T) {
	usePinnedBase(t)
	writePinned(t, `{"abcd3333abcd3333abcd3333abcd3333": {"alias": "retired", "first_seen": "2026-10-01T00:00:00Z"}}`)
	dir, err := newPeerDirectory(nil)
	require.NoError(t, err)
	known, err := openKnownPeers(config.PeerIdentitiesPath(), nil)
	require.NoError(t, err)
	dir.known = known

	// The pin is there, and it is not an entry: nothing configured names it, nothing announced it.
	_, ok := dir.byIdentity("abcd3333abcd3333abcd3333abcd3333")
	require.False(t, ok)

	_, err = dir.Lookup(context.Background(), "abcd3333abcd3333abcd3333abcd3333")
	require.Error(t, err)
	var unknown *UnknownPeerError
	require.NotErrorAs(t, err, &unknown, "the identity IS known — it is the machine that is missing")
}
