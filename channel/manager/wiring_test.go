package manager

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// The registry is process-global and Register panics on a duplicate name, so the fake channels are
// registered once per test binary (both entry points that call AddEnabled read the same registry).
const (
	testEnabledChannel = "add-enabled-test"
	testFailingChannel = "add-enabled-test-fails"
)

var registerTestFactories sync.Once

func registerEnabledTestFactories() {
	registerTestFactories.Do(func() {
		channel.Register(testEnabledChannel, func(map[string]any) (channel.Channel, error) {
			return &mockChannel{name: testEnabledChannel}, nil
		})
		channel.Register(testFailingChannel, func(map[string]any) (channel.Channel, error) {
			return nil, errors.New("token is empty")
		})
	})
}

// TestAddEnabled is the one definition of "config.yaml's channel block becomes running channels":
// the enabled ones are built and added, a name no factory answers for is reported rather than
// dropped, and a disabled one is not built at all.
func TestAddEnabled(t *testing.T) {
	registerEnabledTestFactories()

	cfg := config.DefaultConfig()
	cfg.Channel.Channels = map[string]map[string]any{
		testEnabledChannel: {"enabled": true},
		"add-enabled-test-absent": {"enabled": true},
		"add-enabled-test-off":    {"enabled": false},
	}
	mgr := mustNewManager(t, cfg)
	t.Cleanup(mgr.Close)

	added, missing, err := mgr.AddEnabled(cfg)
	require.NoError(t, err)
	assert.Equal(t, []string{testEnabledChannel}, added)
	assert.Equal(t, []string{"add-enabled-test-absent"}, missing, "an unregistered name is reported, not silently dropped")
	assert.Len(t, mgr.channels, 1, "only the enabled, registered channel was added")
}

// TestAddEnabled_FactoryErrorNamesTheChannel: a channel that cannot be built is the caller's
// decision (fatal for the channel entry, logged-and-continue for a host that has other frontends),
// so the error has to say WHICH channel failed.
func TestAddEnabled_FactoryErrorNamesTheChannel(t *testing.T) {
	registerEnabledTestFactories()

	cfg := config.DefaultConfig()
	cfg.Channel.Channels = map[string]map[string]any{
		testFailingChannel: {"enabled": true},
	}
	mgr := mustNewManager(t, cfg)
	t.Cleanup(mgr.Close)

	_, _, err := mgr.AddEnabled(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), testFailingChannel)
}

// cfgWithProvider is a config New/NewWith can resolve a default provider from — the same shape
// mustNewManager builds, for the call path (NewWith) that does not go through it.
func cfgWithProvider() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Provider = "test"
	cfg.Providers = []config.ProviderConfig{
		{Name: "test", Type: "openai", Model: "test-model", APIKey: "sk-test"},
	}
	return cfg
}

// TestNewWith_DoesNotCloseABorrowedDelegator: the merged headless host hands its own sending side to
// the channel manager. Closing that manager (a shutdown order simpler than the truth) must not take
// the host's delegation with it — DelegatorCache.Close is terminal, so the phone's turns and the
// node picker would lose every peer for the rest of the run.
func TestNewWith_DoesNotCloseABorrowedDelegator(t *testing.T) {
	registerEnabledTestFactories()

	ctx := context.Background()
	cfg := cfgWithProvider()
	peers := []config.PeerConfig{{Name: "peer", URL: "https://127.0.0.1:9999"}}

	shared := node.NewDelegatorCache("host", node.Discovery{}, logger.New("test"), nil, t.TempDir())
	mgr, err := NewWith(cfg, Dependencies{Delegator: shared})
	require.NoError(t, err)
	require.NotNil(t, mgr.delegator)
	assert.False(t, mgr.ownsDelegator)

	mgr.Close()

	assert.NotNil(t, shared.ForSender(ctx, peers), "a borrowed cache must survive the manager that borrowed it")
	shared.Close()

	// The other half of the rule: a cache the manager BUILT is its own to close.
	own := mustNewManager(t, cfg)
	assert.True(t, own.ownsDelegator)
	own.Close()
	assert.Nil(t, own.delegator.ForSender(ctx, peers), "a cache the manager built is closed with it")
}
