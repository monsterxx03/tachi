package discord

import (
	"context"
	"time"
)

const (
	// typingInterval is the minimum interval between typing indicator sends.
	// Discord's API rate-limits typing indicators to roughly once per 10 seconds
	// per channel. Use 10.5s to provide a safety margin against rate limiting.
	typingInterval = 10500 * time.Millisecond
)

// startTypingLoop starts a goroutine that sends typing indicators to the
// specified channel at regular intervals. The loop stops when the context
// is cancelled. Returns a cancel function.
//
// Usage:
//
//	stopTyping := ch.startTypingLoop(ctx, channelID)
//	defer stopTyping()
func (ch *DiscordChannel) startTypingLoop(ctx context.Context, channelID string) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		if !ch.cfg.Typing {
			return
		}
		sess := ch.session
		if sess == nil {
			return
		}

		// Send typing indicator immediately on start.
		_ = sess.ChannelTyping(channelID)

		ticker := time.NewTicker(typingInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = sess.ChannelTyping(channelID)
			}
		}
	}()

	return cancel
}
