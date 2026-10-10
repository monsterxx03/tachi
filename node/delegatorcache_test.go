package node

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// The cache every entry point holds: one delegator per peer list, rebuilt when the list changes
// and absent when there is nothing to delegate to.

func TestTheDelegatorFollowsThePeerList(t *testing.T) {
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	peers := []config.PeerConfig{{Name: "nas", URL: "http://127.0.0.1:1"}}
	cache := NewDelegatorCache("", nil)

	first := cache.For(peers)
	require.NotNil(t, first)
	require.Same(t, first, cache.For(peers), "the same list must not rebuild (records are re-scanned)")

	// A changed address is a different node to talk to: the directory must not be reused.
	moved := []config.PeerConfig{{Name: "nas", URL: "http://127.0.0.1:2"}}
	second := cache.For(moved)
	require.NotNil(t, second)
	require.NotSame(t, first, second)

	// A changed NOTE rebuilds too: it is what the reminder says about an unreachable node.
	noted := []config.PeerConfig{{Name: "nas", URL: "http://127.0.0.1:2", Note: "家里的 NAS"}}
	require.NotSame(t, second, cache.For(noted))
}

func TestNoPeersMeansNoDelegator(t *testing.T) {
	// Empty is a state, not a broken one: with nothing to delegate to, the tools are absent
	// rather than present and always failing.
	cache := NewDelegatorCache("", nil)
	require.Nil(t, cache.For(nil))
	require.Nil(t, cache.For([]config.PeerConfig{}))
}

func TestAFailedOpenIsNotCached(t *testing.T) {
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	peers := []config.PeerConfig{{Name: "nas", URL: "http://127.0.0.1:1"}}
	attempts := 0
	cache := NewDelegatorCache("", nil)
	cache.openFunc = func(p []config.PeerConfig, ownName string, log *logger.Logger) (*Sender, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("the records directory is not writable")
		}
		return OpenSender(p, "", nil)
	}

	// A failure must not be remembered: the next turn tries again, so a transient problem heals
	// instead of disabling delegation for the process's lifetime.
	require.Nil(t, cache.For(peers))
	require.NotNil(t, cache.For(peers))
	require.Equal(t, 2, attempts)
}
