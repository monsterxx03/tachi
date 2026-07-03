// Package chrome implements the channel.Channel interface for Chrome
// Extension communication via WebSocket over localhost.
//
// The extension connects to Tachi at ws://127.0.0.1:<port>/ws. No native
// host manifest is needed — just a running `tachi channel` process.
//
// Architecture:
//
//	tachi channel  ←→  ws://127.0.0.1:18520/ws  ←→  Chrome Extension
//	   (Go)                                          (Sidepanel/Popup)
package chrome

import (
	"context"
	"fmt"
	"os"

	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

func init() {
	channel.Register("chrome", func(rawCfg map[string]any) (channel.Channel, error) {
		if enabled, ok := rawCfg["enabled"]; ok {
			if b, ok := enabled.(bool); ok && !b {
				return nil, fmt.Errorf("chrome: disabled")
			}
		}

		port := DefaultPort
		if p, ok := rawCfg["port"]; ok {
			if pi, ok := p.(int); ok && pi > 0 && pi < 65536 {
				port = pi
			} else if pf, ok := p.(float64); ok && pf > 0 && pf < 65536 {
				port = int(pf)
			}
		}

		return NewChromeChannel("chrome", port), nil
	})
}

// ChromeChannel implements channel.Channel for Chrome Extension communication
// via a localhost WebSocket server.
//
// It starts an HTTP server on 127.0.0.1:<port> with a /ws WebSocket endpoint.
// The extension connects via WebSocket (no native host manifest needed) and
// sends JSON messages (ChromeRequest); the server processes them through the
// channel.MessageHandler pipeline and returns JSON responses (ChromeResponse).
type ChromeChannel struct {
	name   string
	port   int
	server *Server
	logger *debuglog.Logger
}

// NewChromeChannel creates a ChromeChannel.
func NewChromeChannel(name string, port int) *ChromeChannel {
	return &ChromeChannel{
		name:   name,
		port:   port,
		logger: debuglog.DefaultLogger.WithSource("channel:chrome"),
	}
}

// Name returns the channel type identifier.
func (c *ChromeChannel) Name() string { return c.name }

// OnStart implements channel.Channel. Creates the WebSocket server.
func (c *ChromeChannel) OnStart(_ context.Context) error {
	c.server = NewServer(c.port)
	c.logger.Log("chrome: channel ready (port=%d pid=%d)", c.port, os.Getpid())
	return nil
}

// Run implements channel.Channel. Starts the WebSocket server and blocks
// until ctx is cancelled or the server fails.
func (c *ChromeChannel) Run(ctx context.Context, handler channel.MessageHandler) error {
	c.logger.Log("chrome: starting WebSocket server on 127.0.0.1:%d", c.port)

	if c.server == nil {
		c.server = NewServer(c.port)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.server.Start(handler)
	}()

	select {
	case <-ctx.Done():
		c.logger.Log("chrome: context cancelled, shutting down")
		_ = c.server.Close()
		return nil
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("chrome: %w", err)
		}
		return nil
	}
}

// Send implements channel.MessageSender for proactive message delivery.
func (c *ChromeChannel) Send(_ context.Context, msg channel.OutgoingMessage) error {
	if c.server == nil {
		return fmt.Errorf("chrome: server not started")
	}
	c.logger.Log("chrome: proactive send thread=%s", msg.ThreadID)
	return c.server.Send(msg.ThreadID, msg.Content)
}

// compile-time interface checks
var _ channel.Channel = (*ChromeChannel)(nil)
var _ channel.MessageSender = (*ChromeChannel)(nil)
