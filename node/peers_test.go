package node

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
)

// A peer can be named two ways, and both have to work.
//
// The NAME is what the reminder lists and what a tool argument usually carries; the IDENTITY is
// what a reader pastes back from a transcript or from a picker row, and — once nodes are discovered
// rather than configured — what the sender itself will have. A unique PREFIX of an id works too,
// because nobody copies thirty-two hex characters when eight will do.
func TestLookupAcceptsANameAnIdentityOrAPrefix(t *testing.T) {
	usePinnedBase(t)
	writePinned(t, `{
  "abcd1111abcd1111abcd1111abcd1111": {"alias": "home-nas", "first_seen": "2026-10-01T00:00:00Z"},
  "abcd2222abcd2222abcd2222abcd2222": {"alias": "hk-vps", "first_seen": "2026-10-02T00:00:00Z"}
}`)
	dir, err := newPeerDirectory([]config.PeerConfig{
		{Name: "home-nas", URL: "https://127.0.0.1:1"},
		{Name: "hk-vps", URL: "https://127.0.0.1:1"},
	})
	require.NoError(t, err)
	known, err := openKnownPeers(config.PeerIdentitiesPath(), nil, nil)
	require.NoError(t, err)
	dir.known = known

	ctx := context.Background()
	for _, ref := range []string{
		"home-nas",
		"abcd1111abcd1111abcd1111abcd1111",
		"abcd1111abcd1111abcd1111abcd1111"[:12],
	} {
		by, err := dir.Lookup(ctx, ref)
		require.NoError(t, err, "looking up %q", ref)
		require.Equal(t, "home-nas", by.Name, "looking up %q", ref)
	}

	// An ambiguous prefix is refused rather than guessed: sending work to the wrong machine is worse
	// than asking for more characters.
	_, err = dir.Lookup(ctx, "abcd")
	require.Error(t, err)
	require.Contains(t, err.Error(), "matches 2 nodes")

	// An unknown name gets the configured set back, WITH the identities this machine holds — the
	// useful answer is the list it could have picked from.
	_, err = dir.Lookup(ctx, "nope")
	var unknown *UnknownPeerError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, []string{"hk-vps (abcd2222)", "home-nas (abcd1111)"}, unknown.Known)

	// An identity this machine HAS a record for, but whose name is no longer configured, is a
	// different answer from "never heard of it" — and the fix is different too.
	writePinned(t, `{"abcd3333abcd3333abcd3333abcd3333": {"alias": "retired", "first_seen": "2026-10-01T00:00:00Z"}}`)
	known2, err := openKnownPeers(config.PeerIdentitiesPath(), nil, nil)
	require.NoError(t, err)
	dir.known = known2
	_, err = dir.Lookup(ctx, "abcd3333abcd3333abcd3333abcd3333")
	require.Error(t, err)
	require.Contains(t, err.Error(), "last reached as \"retired\"")
	require.NotErrorAs(t, err, &unknown, "that is not the unknown-name answer")
}
