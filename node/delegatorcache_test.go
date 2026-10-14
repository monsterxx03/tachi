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

	peers := []config.PeerConfig{{Name: "nas", URL: "https://127.0.0.1:1"}}
	cache := NewDelegatorCache("", Discovery{}, nil)

	first := cache.For(t.Context(), peers)
	require.NotNil(t, first)
	require.Same(t, first, cache.For(t.Context(), peers), "the same list must not rebuild (records are re-scanned)")

	// A changed address is a different node to talk to: the directory must not be reused.
	moved := []config.PeerConfig{{Name: "nas", URL: "https://127.0.0.1:2"}}
	second := cache.For(t.Context(), moved)
	require.NotNil(t, second)
	require.NotSame(t, first, second)

	// A changed NOTE rebuilds too: it is what the reminder says about an unreachable node.
	noted := []config.PeerConfig{{Name: "nas", URL: "https://127.0.0.1:2", Note: "家里的 NAS"}}
	require.NotSame(t, second, cache.For(t.Context(), noted))
}

func TestNoPeersMeansNoDelegator(t *testing.T) {
	// Empty is a state, not a broken one: with nothing to delegate to, the tools are absent
	// rather than present and always failing.
	cache := NewDelegatorCache("", Discovery{}, nil)
	require.Nil(t, cache.For(t.Context(), nil))
	require.Nil(t, cache.For(t.Context(), []config.PeerConfig{}))
}

// With discovery ON and nothing configured, there is still a sending side: the whole point of
// discovery is a node that has no config line, and refusing here would make that impossible.
func TestDiscoveryAloneStillYieldsASender(t *testing.T) {
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	cache := NewDelegatorCache("", Discovery{Enabled: true}, nil)
	require.NotNil(t, cache.For(t.Context(), nil), "a machine with discovery on can still delegate")
	cache.Close()
}

func TestAFailedOpenIsNotCached(t *testing.T) {
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	peers := []config.PeerConfig{{Name: "nas", URL: "https://127.0.0.1:1"}}
	attempts := 0
	cache := NewDelegatorCache("", Discovery{}, nil)
	cache.openFunc = func(p []config.PeerConfig, ownName string, log *logger.Logger, browser *mdnsBrowser) (*Sender, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("the records directory is not writable")
		}
		return OpenSender(p, "", nil, browser)
	}

	// A failure must not be remembered: the next turn tries again, so a transient problem heals
	// instead of disabling delegation for the process's lifetime.
	require.Nil(t, cache.For(t.Context(), peers))
	require.NotNil(t, cache.For(t.Context(), peers))
	require.Equal(t, 2, attempts)
}

// Close is terminal, and the OPEN is what carries the side effects: an entry point calls it on its
// way out while turn goroutines are still winding down, and a sender built afterwards would start a
// second browser — exactly the thing Close exists to stop.
func TestAClosedCacheHandsOutNothing(t *testing.T) {
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	peers := []config.PeerConfig{{Name: "nas", URL: "https://127.0.0.1:1"}}
	opens := 0
	cache := NewDelegatorCache("", Discovery{Enabled: true}, nil)
	cache.openFunc = func(p []config.PeerConfig, ownName string, log *logger.Logger, browser *mdnsBrowser) (*Sender, error) {
		opens++
		return OpenSender(p, "", nil, browser)
	}
	require.NotNil(t, cache.For(t.Context(), peers))
	require.Equal(t, 1, opens)

	cache.Close()
	require.Nil(t, cache.For(t.Context(), peers), "a closed cache delegates nothing")
	require.Nil(t, cache.ForSender(t.Context(), peers))
	require.Nil(t, cache.For(t.Context(), nil),
		"nor does discovery rescue it: the switch goes off with the browser")
	require.Equal(t, 1, opens, "and nothing more is opened")
}
