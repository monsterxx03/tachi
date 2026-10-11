// Package mock registers a channel that connects to nothing, and exists so a REAL tachi process
// can host a node with no terminal and no IM account.
//
// Why that needs a channel at all: the A2A surface lives inside a process that is alive, and only
// three entries host one (node.StartOrAttachNode) — the TUI, the desktop, and the channel entry.
// The channel entry is the only one that can run headless, and it refuses to start unless a
// channel is enabled. This is that channel: Run blocks until the process is asked to stop, and
// nothing here touches the network.
//
// It is deliberately NOT a user-facing channel, and nothing makes it one: only main_channels.go
// reads the registry, so "mock" is invisible unless a config names it. A config that DOES name it
// gets a process that sits still and serves the node — which is exactly what a headless host is,
// and why an itest can use it as one (itest.md, "A node with no terminal").
package mock

import (
	"context"

	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/logger"
)

func init() {
	channel.Register("mock", func(map[string]any) (channel.Channel, error) {
		// No config to parse: there is nothing to point at, and a knob nobody sets is a knob that
		// lies about doing something.
		return &Channel{}, nil
	})
}

// Channel is the inert channel: no connection, no credentials, no threads.
type Channel struct{}

var _ channel.Channel = (*Channel)(nil)

// Name identifies it in the manager's logs and in the registry.
func (c *Channel) Name() string { return "mock" }

// OnStart has nothing to prepare — there is nothing to connect to.
func (c *Channel) OnStart(context.Context) error { return nil }

// Run blocks until the process shuts down, and that IS its job: the manager waits for every
// channel to exit, and the node service stays up exactly as long as the process does.
//
// It says what it is once at startup: a log line that reads "nothing connects anywhere, this
// process only exists to serve the node" saves whoever finds this process from having to guess.
func (c *Channel) Run(ctx context.Context, _ channel.MessageHandler) error {
	logger.New("channel.mock").Info(ctx, "channel: mock is running",
		"what", "it connects to nothing; it keeps this process — and the node service inside it — alive")
	<-ctx.Done()
	return nil
}
