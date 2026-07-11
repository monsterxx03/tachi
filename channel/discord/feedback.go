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

// addReaction adds a reaction emoji to a message.
func (ch *DiscordChannel) addReaction(channelID, messageID, emoji string) error {
	sess := ch.session
	if sess == nil || !ch.cfg.Reactions {
		return nil
	}
	return sess.MessageReactionAdd(channelID, messageID, emoji)
}

// removeReaction removes a reaction emoji from a message.
func (ch *DiscordChannel) removeReaction(channelID, messageID, emoji string) error {
	sess := ch.session
	if sess == nil || !ch.cfg.Reactions {
		return nil
	}
	// Remove the bot's own reaction.
	return sess.MessageReactionRemove(channelID, messageID, emoji, "@me")
}

// addProcessingReaction adds the 👀 reaction to indicate the bot is working.
func (ch *DiscordChannel) addProcessingReaction(channelID, messageID string) error {
	return ch.addReaction(channelID, messageID, "👀")
}

// replaceWithDoneReaction removes 👀 and adds ✅ to signal success.
func (ch *DiscordChannel) replaceWithDoneReaction(channelID, messageID string) error {
	_ = ch.removeReaction(channelID, messageID, "👀")
	return ch.addReaction(channelID, messageID, "✅")
}

// replaceWithErrorReaction removes 👀 and adds ❌ to signal failure.
func (ch *DiscordChannel) replaceWithErrorReaction(channelID, messageID string) error {
	_ = ch.removeReaction(channelID, messageID, "👀")
	return ch.addReaction(channelID, messageID, "❌")
}

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

// feedbackOnMessage manages the full reaction lifecycle for a single message:
//   - Adds 👀 at the start
//   - Calls workFn for the actual processing
//   - Replaces 👀 with ✅ on success, ❌ on error
//
// Returns the error from workFn, if any.
func (ch *DiscordChannel) feedbackOnMessage(channelID, messageID string, workFn func() error) error {
	// Phase 1: skip 👀 for very fast paths to avoid pointless API calls.
	// We add 👀 and remove it when done.
	_ = ch.addProcessingReaction(channelID, messageID)

	err := workFn()

	if err != nil {
		_ = ch.replaceWithErrorReaction(channelID, messageID)
	} else {
		_ = ch.replaceWithDoneReaction(channelID, messageID)
	}
	return err
}
