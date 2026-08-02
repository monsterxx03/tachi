package discord

import (
	"context"
)

func (ch *DiscordChannel) sendGreeting() {
	if ch.cfg.Greeting == "" {
		return
	}
	if len(ch.cfg.AllowedUsers) == 0 {
		ch.logger.Warn(context.Background(), "discord: greeting configured but no allowed_users to send to")
		return
	}
	sess := ch.session
	if sess == nil {
		return
	}
	// Create/open a DM channel with the first allowed user.
	dmChannel, err := sess.UserChannelCreate(ch.cfg.AllowedUsers[0])
	if err != nil {
		ch.logger.Error(context.Background(), "discord: create DM channel for greeting", err)
		return
	}
	if _, err := sess.ChannelMessageSend(dmChannel.ID, ch.cfg.Greeting); err != nil {
		ch.logger.Error(context.Background(), "discord: send greeting error", err)
	}
}

// applyProxy configures the discordgo session to use a proxy for both
// REST API calls (sess.Client) and Gateway WebSocket connection (sess.Dialer).
// It is a no-op when cfg.Proxy is empty.
