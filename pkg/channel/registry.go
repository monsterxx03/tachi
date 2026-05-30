package channel

import "maps"

import "sync"

// Factory creates a Channel from its raw YAML config map.
//
// The rawCfg parameter is the channel-specific YAML subtree decoded
// into map[string]any. Implementations are responsible for decoding
// it into their own typed config struct (via yaml marshal/unmarshal,
// or simple map lookups for small configs).
//
// Example:
//
//	channel.Register("mybot", func(rawCfg map[string]any) (channel.Channel, error) {
//	    return NewMyBotChannel(rawCfg)
//	})
type Factory func(rawCfg map[string]any) (Channel, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register registers a channel factory. Call in an init() function:
//
//	func init() {
//	    channel.Register("weixin", weixinFactory)
//	}
//
// Panics if name is empty or factory is nil.
func Register(name string, factory Factory) {
	if name == "" {
		panic("channel.Register: empty name")
	}
	if factory == nil {
		panic("channel.Register: nil factory for " + name)
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("channel.Register: duplicate name: " + name)
	}
	registry[name] = factory
}

// ListRegistered returns a snapshot of all registered channel names
// and their factories. Safe for concurrent use.
func ListRegistered() map[string]Factory {
	registryMu.RLock()
	defer registryMu.RUnlock()
	m := make(map[string]Factory, len(registry))
	maps.Copy(m, registry)
	return m
}
