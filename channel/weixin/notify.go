package weixin

import (
	"context"
	"errors"
	"fmt"

	"github.com/monsterxx03/tachi/pkg/channel"
)

// The channel is a notification route: it can reach the user unprompted.
var _ channel.NotificationChannel = (*Channel)(nil)

// notifySender is the send-only state of NotifyUser: the credentials read from
// the state dir and a client of its own.
//
// Its own, and NOT ch.cli / ch.accountID / ch.botToken, for two reasons that
// both come from the same fact — a notification may run while another process
// (or this process's own Run) owns the receive loop:
//
//   - Run writes those fields when it logs in, on its own goroutine. A
//     notification reading them would be a data race on three strings decided by
//     timing; this struct carries its own copy, written once and then only read.
//   - the notify path must behave the same whether or not this process happens
//     to be running the channel, and one code path is the only way to know it
//     does.
type notifySender struct {
	cli          *client
	userID       string
	contextToken string
}

// NotifyUser implements channel.NotificationChannel.
//
// It is the send-only half of this channel: no getUpdates loop, no
// notifyStart/notifyStop (that is the receive session's lifecycle), no QR login —
// the message goes out with the credentials already on disk, the same way the
// startup greeting does.
func (ch *Channel) NotifyUser(ctx context.Context, text string) error {
	s, err := ch.notifySender()
	if err != nil {
		return err
	}
	if err := ch.sendTextVia(s.cli, s.userID, s.contextToken, text); err != nil {
		return fmt.Errorf("weixin: send to %s: %w", s.userID, err)
	}
	ch.logger.Info(ctx, "weixin: notified user", "user", s.userID, "chars", len(text))
	return nil
}

// notifySender returns the prepared send-only sender, preparing it on first use.
//
// A FAILED preparation is deliberately not remembered: the reason is almost
// always "this machine has not logged in yet", and the channel process finishing
// its login is exactly what makes the next call succeed. Credentials are read
// from disk on every attempt for the same reason. Two goroutines racing to
// prepare build two senders and one wins — cheaper to allow than to lock against,
// since a loser holds no resource.
//
// The context token may legitimately be empty: the startup greeting is sent with
// none, and the platform accepts it.
func (ch *Channel) notifySender() (*notifySender, error) {
	if s := ch.notify.Load(); s != nil {
		return s, nil
	}

	accountID, data, ok, err := ch.storedAccount()
	if err != nil {
		return nil, fmt.Errorf("weixin: read stored account: %w", err)
	}
	if !ok {
		return nil, errors.New("weixin: no stored login on this machine — run the channel once and scan the QR code")
	}
	if data.UserID == "" {
		return nil, fmt.Errorf("weixin: stored account %s has no user id", accountID)
	}

	// Mirror what Run sets on the client it sends replies with, so a
	// notification and a reply carry identical headers.
	cli := newClient()
	cli.SetRouteTag(ch.cfg.RouteTag)
	cli.SetBotAgent(ch.cfg.BotAgent)
	cli.SetBaseURL(data.BaseURL)
	cli.SetBotToken(data.Token)

	s := &notifySender{
		cli:          cli,
		userID:       data.UserID,
		contextToken: ch.store.loadContextToken(accountID, data.UserID),
	}
	ch.notify.Store(s)
	return s, nil
}
