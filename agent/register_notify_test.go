package agent

import (
	"context"
	"sync"
	"testing"

	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/stretchr/testify/assert"
)

// agentTestNotifyChannel is the name the fake below registers under. It must be
// unique process-wide: channel.Register panics on a duplicate, and the registry
// lives for the whole test binary.
const agentTestNotifyChannel = "agent-test-notify"

// agentTestNotifier is a channel with the one extra capability the mount looks
// for — a way to reach the user unprompted.
type agentTestNotifier struct{}

func (agentTestNotifier) Name() string                             { return agentTestNotifyChannel }
func (agentTestNotifier) OnStart(context.Context) error            { return nil }
func (agentTestNotifier) NotifyUser(context.Context, string) error { return nil }

func (agentTestNotifier) Run(ctx context.Context, _ channel.MessageHandler) error {
	<-ctx.Done()
	return nil
}

// registerAgentTestNotifier puts the fake into the process-wide registry exactly
// once: channel.Register panics on a duplicate, and several tests here need it.
var registerAgentTestNotifier = sync.OnceFunc(func() {
	channel.Register(agentTestNotifyChannel, func(map[string]any) (channel.Channel, error) {
		return agentTestNotifier{}, nil
	})
})

// agentTestChannelConfig is a config whose only channel is the fake above,
// switched on or off by the caller.
func agentTestChannelConfig(notify bool) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Channel.Channels = map[string]map[string]any{
		agentTestNotifyChannel: {"enabled": true, "notify": notify},
	}
	return cfg
}

func notifyCapableAgent(t *testing.T, cfg *config.Config) *AIAgent {
	t.Helper()
	a := newBareTestAgent(t, nil, 50)
	a.Config.FullConfig = cfg
	return a
}

// The tool is mounted exactly when the local config names an enabled channel that
// opted in AND implements the notification interface — the tool's presence is the
// statement that a route exists, so a config that justifies nothing must leave it
// absent rather than present-and-failing.
func TestRegisterTools_NotifyUserMounting(t *testing.T) {
	registerAgentTestNotifier()

	t.Run("mounted when a channel opts in", func(t *testing.T) {
		a := notifyCapableAgent(t, agentTestChannelConfig(true))
		a.RegisterTools()
		assert.NotNil(t, a.GetTool(agenttools.ToolNameNotifyUser))
	})

	t.Run("absent without the switch", func(t *testing.T) {
		a := notifyCapableAgent(t, agentTestChannelConfig(false))
		a.RegisterTools()
		assert.Nil(t, a.GetTool(agenttools.ToolNameNotifyUser))
	})

	t.Run("absent when the channel is disabled", func(t *testing.T) {
		cfg := agentTestChannelConfig(true)
		cfg.Channel.Channels[agentTestNotifyChannel]["enabled"] = false
		a := notifyCapableAgent(t, cfg)
		a.RegisterTools()
		assert.Nil(t, a.GetTool(agenttools.ToolNameNotifyUser))
	})

	t.Run("absent when a channel does not implement the interface", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.Channel.Channels = map[string]map[string]any{
			// "mock" is registered (channel/mock) and cannot notify.
			"mock": {"enabled": true, "notify": true},
		}
		a := notifyCapableAgent(t, cfg)
		a.RegisterTools()
		assert.Nil(t, a.GetTool(agenttools.ToolNameNotifyUser))
	})

	t.Run("absent without a config", func(t *testing.T) {
		a := newBareTestAgent(t, nil, 50)
		a.RegisterTools()
		assert.Nil(t, a.GetTool(agenttools.ToolNameNotifyUser))
	})
}

// Turning the switch off while the process runs must take the tool away:
// RegisterTools only adds, so without the explicit drop the model would keep a
// tool whose every call must fail.
func TestReloadTools_DropsNotifyUserWhenTheSwitchGoesOff(t *testing.T) {
	registerAgentTestNotifier()

	cfg := agentTestChannelConfig(true)
	a := notifyCapableAgent(t, cfg)
	a.RegisterTools()
	assert.NotNil(t, a.GetTool(agenttools.ToolNameNotifyUser), "precondition: mounted")

	// What saving a setting does: the shared config object changes in place.
	cfg.Channel.Channels[agentTestNotifyChannel]["notify"] = false
	assert.NoError(t, a.ReloadTools())
	assert.Nil(t, a.GetTool(agenttools.ToolNameNotifyUser), "the tool must not outlive its justification")

	// ...and back on again, which the same rebuild has to pick up.
	cfg.Channel.Channels[agentTestNotifyChannel]["notify"] = true
	assert.NoError(t, a.ReloadTools())
	assert.NotNil(t, a.GetTool(agenttools.ToolNameNotifyUser))
}
