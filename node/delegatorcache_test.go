package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
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
	cache := NewDelegatorCache("", Discovery{}, nil, nil)

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
	cache := NewDelegatorCache("", Discovery{}, nil, nil)
	require.Nil(t, cache.For(t.Context(), nil))
	require.Nil(t, cache.For(t.Context(), []config.PeerConfig{}))
}

// With discovery ON and nothing configured, there is still a sending side: the whole point of
// discovery is a node that has no config line, and refusing here would make that impossible.
func TestDiscoveryAloneStillYieldsASender(t *testing.T) {
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	cache := NewDelegatorCache("", Discovery{Enabled: true}, nil, nil)
	require.NotNil(t, cache.For(t.Context(), nil), "a machine with discovery on can still delegate")
	cache.Close()
}

func TestAFailedOpenIsNotCached(t *testing.T) {
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	peers := []config.PeerConfig{{Name: "nas", URL: "https://127.0.0.1:1"}}
	attempts := 0
	cache := NewDelegatorCache("", Discovery{}, nil, nil)
	cache.openFunc = func(p []config.PeerConfig, ownName string, log *logger.Logger, store *sightingsStore) (*Sender, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("the records directory is not writable")
		}
		return OpenSender(p, "", nil, store)
	}

	// A failure must not be remembered: the next turn tries again, so a transient problem heals
	// instead of disabling delegation for the process's lifetime.
	require.Nil(t, cache.For(t.Context(), peers))
	require.NotNil(t, cache.For(t.Context(), peers))
	require.Equal(t, 2, attempts)
}

// WarmUp is the startup warmup, and the two halves of it are exactly the two halves of the rule it
// serves: the round really happens with nobody asking for it (the FIRST turn's reminder is what
// needs a peer's identity, and every identity comes out of a handshake), and the entry point does
// not wait for it (a peer that is down, slow, or gone must not hold a window).
func TestWarmUpAsksEveryPeerWithoutWaitingForIt(t *testing.T) {
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	peers := []config.PeerConfig{{Name: "nas", URL: "https://nas.example:8443"}}
	dir, err := newPeerDirectory(peers)
	require.NoError(t, err)

	// A node that takes its time answering: the network is the reason nothing here may block.
	asked := make(chan string, 1)
	release := make(chan struct{})
	dir.resolveEntry = func(ctx context.Context, e peerEntry) (*a2a.AgentCard, string, error) {
		asked <- e.Label
		<-release
		return nil, "", errors.New("nothing answered at that address")
	}

	cache := NewDelegatorCache("", Discovery{}, nil, nil)
	cache.openFunc = func([]config.PeerConfig, string, *logger.Logger, *sightingsStore) (*Sender, error) {
		return &Sender{peers: dir}, nil
	}

	returned := make(chan struct{})
	go func() { cache.WarmUp(t.Context(), peers); close(returned) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("WarmUp waited for a node that had not answered: a peer would hold up startup")
	}

	// ...and the round happens on its own, which is the other half: without it this would be a
	// no-op that returns quickly, proving nothing.
	select {
	case got := <-asked:
		require.Equal(t, "nas", got)
	case <-time.After(5 * time.Second):
		t.Fatal("the warmup never asked the node: the first turn would pay for the handshake")
	}
	close(release)
}

// A cache that has been closed warms nothing: Close is terminal, and the round it would start is a
// sender and a browser the entry point is in the middle of shutting down (see
// TestAClosedCacheHandsOutNothing).
func TestWarmUpOnAClosedCacheOpensNothing(t *testing.T) {
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	peers := []config.PeerConfig{{Name: "nas", URL: "https://nas.example:8443"}}
	opens := 0
	cache := NewDelegatorCache("", Discovery{}, nil, nil)
	opened := make(chan struct{}, 1)
	cache.openFunc = func([]config.PeerConfig, string, *logger.Logger, *sightingsStore) (*Sender, error) {
		opens++
		opened <- struct{}{}
		return &Sender{peers: nil}, nil
	}

	cache.Close()
	cache.WarmUp(t.Context(), peers)
	select {
	case <-opened:
		t.Fatalf("a closed cache opened %d senders; it must open none", opens)
	case <-time.After(100 * time.Millisecond):
	}
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
	cache := NewDelegatorCache("", Discovery{Enabled: true}, nil, nil)
	cache.openFunc = func(p []config.PeerConfig, ownName string, log *logger.Logger, store *sightingsStore) (*Sender, error) {
		opens++
		return OpenSender(p, "", nil, store)
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

// The switch decides whether the hearsay half exists at all, and the loop's life is the cache's: it
// comes up with the sending side, and it goes down BEFORE the store it writes into.
func TestGossipComesUpAndGoesDownWithTheCache(t *testing.T) {
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	// Off: nothing of it exists — no store, no loop, so no packet can be sent.
	quiet := NewDelegatorCache("", Discovery{Enabled: true, SelfAddrs: []string{lanAddr}}, nil, nil)
	require.NotNil(t, quiet.For(t.Context(), nil))
	require.Nil(t, quiet.gossip, "gossip off means no loop runs")
	quiet.Close()

	// On: the loop comes up with the store, carries this machine's own account of where it is, and does
	// not outlive a Close. It has nothing to ask here (no peers, no sightings), which is a round with
	// nobody in it rather than an error.
	cache := NewDelegatorCache("", Discovery{Gossip: true, SelfAddrs: []string{lanAddr}}, nil, nil)
	require.NotNil(t, cache.For(t.Context(), nil), "gossip alone is still a sending side")
	require.NotNil(t, cache.gossip, "and the loop runs when it is on")
	require.Equal(t, []string{lanAddr}, cache.gossip.selfAddrs)
	require.NotNil(t, cache.store, "the store it feeds came up with it")

	closed := make(chan struct{})
	go func() {
		cache.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return: the loop does not leave on its cancel")
	}
	require.Nil(t, cache.gossip, "and the cache holds neither half afterwards")
	require.Nil(t, cache.store)
	require.False(t, cache.discovery.any(),
		"and both switches read as off: gossip is a way discovery can be on, so a closed cache must "+
			"not still say it looks for nodes")
}

// 五个 gossip 数字从配置走到发送侧：零值也原样带过去，由循环决定默认（默认值只写在 node/gossip.go
// 一处，这里不替它填）。
func TestTheConfiguredGossipNumbersReachTheSendingSide(t *testing.T) {
	cfg := &config.Config{}
	cfg.Node.Discovery.Gossip.Enabled = true
	cfg.Node.Discovery.Gossip.Period = 90 * time.Second
	cfg.Node.Discovery.Gossip.Fanout = 5
	cfg.Node.Discovery.Gossip.Nudge = 3 * time.Second
	cfg.Node.Discovery.Gossip.Timeout = time.Second
	cfg.Node.Discovery.Gossip.Mute = time.Minute

	d := DiscoveryOf(cfg)
	require.True(t, d.Gossip)
	require.Equal(t, gossipTuning{
		Period: 90 * time.Second, Fanout: 5, Nudge: 3 * time.Second,
		Timeout: time.Second, Mute: time.Minute,
	}, d.GossipTuning)

	// 一行都不写就是零：循环用它自己的默认。
	require.Equal(t, gossipTuning{}, DiscoveryOf(&config.Config{}).GossipTuning)
}

// Where this machine introduces itself by gossip is a claim like every other one, and the receiving end
// keeps only what it could dial (node/gossip.go isGossipAddr: https, and an address on a private,
// link-local or CGNAT network). The mapping keeps what the operator wrote — it is the same key the Card
// is published under — and the loop is built over the part of it that survives that check, so an
// operator whose `public_url` is a name hears about it once (startDiscovery) instead of being left with
// a switch that looks on and introduces this machine to nobody.
func TestOnlyAnAddressAPeerCouldDialIsUsedToIntroduceOurself(t *testing.T) {
	cfg := &config.Config{}
	// The shape config.go's own example has: a name, not an address. A peer can read this machine's
	// Card at it and can never be handed it as somewhere to dial.
	cfg.Node.PublicURL = "https://nas.tail-scale.ts.net:7080"
	require.Equal(t, []string{cfg.Node.PublicURL}, DiscoveryOf(cfg).SelfAddrs,
		"the mapping keeps what the operator wrote")
	require.Empty(t, gossipSelfAddrs(DiscoveryOf(cfg).SelfAddrs), "and the loop is not built over it")

	// A private address, as written (with the spaces an operator leaves behind): exactly what the far
	// end accepts.
	cfg.Node.PublicURL = "  " + lanAddr + "  "
	require.Equal(t, []string{lanAddr}, DiscoveryOf(cfg).SelfAddrs)
	require.Equal(t, []string{lanAddr}, gossipSelfAddrs(DiscoveryOf(cfg).SelfAddrs))

	// A public address is dropped too — this route is for machines that can reach each other, and asking
	// to be dialled from wherever is the thing that check exists for (§六) — and so is plaintext.
	require.Empty(t, gossipSelfAddrs([]string{"https://8.8.8.8:7080"}))
	require.Empty(t, gossipSelfAddrs([]string{"http://192.168.1.9:7080"}), "https is the other half")
}
