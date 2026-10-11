package node

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// The lookup the sidebar needs: "which of my machines is this node id". Only the PINNED record
// answers it — the caller's own name never does (any key can claim any name) — and only OUTBOUND
// contact writes that record, so an id this machine has never reached resolves to nothing. That is
// a correct answer, and the one a receive-only machine gets for every caller it has.
func TestPeerAliasesAnswersWhichMachineANodeIDIs(t *testing.T) {
	base := t.TempDir()
	config.SetBaseDir(base)
	t.Cleanup(func() { config.SetBaseDir("") })

	require.NoError(t, fileutil.WriteFilePrivate(config.PeerIdentitiesPath(), []byte(`{
  "home-nas": {"node_id": "aaaa1111aaaa1111aaaa1111aaaa1111", "first_seen": "2026-10-01T00:00:00Z"},
  "hk-vps":   {"node_id": "bbbb2222bbbb2222bbbb2222bbbb2222", "first_seen": "2026-10-02T00:00:00Z"}
}`)))

	peers := PeerAliases()
	require.Equal(t, "home-nas", peers["aaaa1111aaaa1111aaaa1111aaaa1111"])
	require.Equal(t, "hk-vps", peers["bbbb2222bbbb2222bbbb2222bbbb2222"])
	require.Empty(t, peers["cccc3333cccc3333cccc3333cccc3333"],
		"an id this machine has never reached has no name here, and inventing one is the bug")

	// A record that cannot be read yields nothing rather than failing: a label is not worth a
	// sidebar that does not draw.
	require.NoError(t, os.WriteFile(config.PeerIdentitiesPath(), []byte("{not json"), 0o600))
	require.Empty(t, PeerAliases())
}
