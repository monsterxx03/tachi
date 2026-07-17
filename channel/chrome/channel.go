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
	"github.com/monsterxx03/tachi/pkg/logger"
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
	logger *logger.Logger
}

// NewChromeChannel creates a ChromeChannel.
func NewChromeChannel(name string, port int) *ChromeChannel {
	return &ChromeChannel{
		name:   name,
		port:   port,
		logger: logger.New("channel.chrome"),
	}
}

// Name returns the channel type identifier.
func (c *ChromeChannel) Name() string { return c.name }

// OnStart implements channel.Channel. Creates the WebSocket server.
func (c *ChromeChannel) OnStart(ctx context.Context) error {
	c.server = NewServer(c.port)
	c.logger.Info(ctx, "chrome: channel ready", "port", c.port, "pid", os.Getpid())
	return nil
}

// Run implements channel.Channel. Starts the WebSocket server and blocks
// until ctx is cancelled or the server fails.
func (c *ChromeChannel) Run(ctx context.Context, handler channel.MessageHandler) error {
	c.logger.Info(ctx, "chrome: starting WebSocket server", "port", c.port)

	if c.server == nil {
		c.server = NewServer(c.port)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.server.Start(handler)
	}()

	select {
	case <-ctx.Done():
		c.logger.Info(ctx, "chrome: context cancelled, shutting down")
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
	c.logger.Info(context.Background(), "chrome: proactive send", "thread", msg.ThreadID)
	return c.server.Send(msg.ThreadID, msg.Content)
}

// compile-time interface checks
var _ channel.Channel = (*ChromeChannel)(nil)
var _ channel.MessageSender = (*ChromeChannel)(nil)
var _ channel.SystemPromptSuffixer = (*ChromeChannel)(nil)

// SystemPromptSuffix implements channel.SystemPromptSuffixer.
// Tells the agent it's currently operating as a Chrome Extension companion
// in the browser.
func (c *ChromeChannel) SystemPromptSuffix() string {
	return `## Current Channel: Chrome Extension

You are currently operating as a Chrome Extension companion. The user is
interacting with you through a browser sidepanel or popup.

Platform characteristics:
- Full markdown rendering is supported in the extension UI
- The user is in a browser — they may be looking at web pages and can
  share page content with you
- WebSocket connection over localhost — low latency, real-time responses
- You have access to all standard tools including ReadFile, Bash, and
  web operations`
}
