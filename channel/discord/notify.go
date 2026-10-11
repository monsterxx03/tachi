package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// The channel is a notification route: it can reach the user unprompted.
var _ channel.NotificationChannel = (*DiscordChannel)(nil)

// notifyUserID returns the user a notification is addressed to: the first
// configured allowed user, which is the bot's owner by convention.
//
// No guessing when it is empty — see NotifyUser. A wrong recipient for a
// notification is worse than a failed call, because nothing on the receiving side
// tells anyone it went to the wrong person.
func (ch *DiscordChannel) notifyUserID() string {
	for _, id := range ch.cfg.AllowedUsers {
		if id = strings.TrimSpace(id); id != "" {
			return id
		}
	}
	return ""
}

// NotifyUser implements channel.NotificationChannel by opening (or reusing) the
// direct-message conversation with the user and sending the text there.
//
// The DM channel is resolved explicitly through UserChannelCreate rather than
// relying on Discord accepting a user ID as a channel ID: the shortcut is
// undocumented, and this way a failure names the real problem (no shared server
// with the user) instead of looking like a malformed channel.
func (ch *DiscordChannel) NotifyUser(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("discord: notification text is empty")
	}

	userID := ch.notifyUserID()
	if userID == "" {
		return errors.New("discord: no notify target — set allowed_users to the user to notify")
	}

	sess, err := ch.notifySession()
	if err != nil {
		return err
	}

	dm, err := sess.UserChannelCreate(userID)
	if err != nil {
		return fmt.Errorf("discord: open a DM with %s (does that user share a server with the bot?): %w", userID, err)
	}
	if err := ch.sendTextOn(sess, dm.ID, text, nil); err != nil {
		return fmt.Errorf("discord: send to %s: %w", userID, err)
	}

	ch.logger.Info(ctx, "discord: notified user", "user", userID, "chars", len(text))
	return nil
}

// notifySession returns the session to send a notification on: the live one when
// Run() has one, otherwise a REST-only session of its own.
//
// Its own session is NOT Open()ed, and that is the whole point: a notification is
// an HTTP POST, so a second session on the same bot token costs no gateway
// connection, no presence, and consumes no events — which is exactly what makes
// it safe to build in a process that is not the channel. discordgo.New only
// allocates an http.Client and a token; the REST path never checks for a live
// gateway.
//
// The ch.session read mirrors MessageSender.Send: Run assigns it once at startup,
// and afterwards it is read-only.
func (ch *DiscordChannel) notifySession() (*discordgo.Session, error) {
	if sess := ch.session; sess != nil {
		return sess, nil
	}

	ch.notifyMu.Lock()
	defer ch.notifyMu.Unlock()
	if ch.notifySess != nil {
		return ch.notifySess, nil
	}

	if ch.cfg.Token == "" {
		return nil, errors.New("discord: no bot token configured")
	}
	sess, err := discordgo.New("Bot " + ch.cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("discord: create session: %w", err)
	}
	// Same proxy handling as Run: a deployment that needs a proxy to reach
	// Discord needs it for a notification too.
	if err := ch.applyProxy(context.Background(), sess); err != nil {
		return nil, err
	}

	ch.notifySess = sess
	return sess, nil
}
