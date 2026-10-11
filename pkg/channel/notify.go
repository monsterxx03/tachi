package channel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// NotifyConfigKey is the per-channel switch that opts a channel in as a
// notification target: `channel.channels.<name>.notify: true`.
//
// It is a GENERIC key, not a field of any channel's own config struct, because
// the question it answers belongs to whoever mounts the NotifyUser tool and not
// to the channel implementation. Being turned on is still not enough — the
// channel must also implement NotificationChannel, so the switch is the user's
// policy on top of the implementation's capability.
const NotifyConfigKey = "notify"

// NotificationChannel is an optional interface for channels that can deliver a
// direct message to the user on their own initiative — without an inbound
// message to answer (see pkg/channel's MessageSender for the request-response
// counterpart).
//
// Two things an implementation MUST get right, because the process that mounts
// this interface is not necessarily the process running the full channel:
//
//   - SEND ONLY. Preparing to notify must not enter a receive loop, must not
//     consume inbound messages, and must not start a long-lived goroutine. The
//     caller never closes a notifier, so a notifier that owns a connection or a
//     goroutine is a leak by construction.
//   - NEVER LIE. No usable send path (no credentials, no known recipient, a
//     platform refusal) is an ERROR. Returning nil without delivering anything
//     makes the model tell the user something was sent when nothing was.
//
// NotifyUser must be safe to call concurrently.
type NotificationChannel interface {
	Channel

	// NotifyUser delivers text to the user's direct-message conversation on
	// this channel, returning an error when it was not delivered.
	//
	// Addressing belongs to the channel: only it knows what a direct message to
	// the user is called on its platform, and who the user is. The caller never
	// passes a thread or a user ID.
	NotifyUser(ctx context.Context, text string) error
}

// NotifyRequested reports whether a channel's raw config asks it to serve as a
// notification tool. A missing key, or a value that is not a truthy bool/number,
// means no.
func NotifyRequested(rawCfg map[string]any) bool {
	v, ok := rawCfg[NotifyConfigKey]
	if !ok {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case int:
		return val != 0
	case float64:
		return val != 0
	default:
		return false
	}
}

// notifierTarget is one resolved notification route: the channel's name (for
// logs, errors and the tool's own description) and the instance that delivers.
type notifierTarget struct {
	name string
	ch   NotificationChannel
}

// NotifierSet is every notified-capable channel the config justified, in a
// deterministic order.
//
// It is built once per agent (mount time) and then only read, so it holds no
// lock: an implementation's own laziness is the implementation's to guard.
type NotifierSet struct {
	targets []notifierTarget
}

// Empty reports whether there is nothing to notify through. A nil set is empty,
// so a caller can compare the result of NotifiersFromConfig without a nil check.
func (s *NotifierSet) Empty() bool {
	return s == nil || len(s.targets) == 0
}

// Channels names the channels that can carry a notification, in attempt order.
// The tool uses it to tell the model what it can reach.
func (s *NotifierSet) Channels() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.targets))
	for _, t := range s.targets {
		names = append(names, t.name)
	}
	return names
}

// Notify delivers text through the first channel that accepts it, and returns
// that channel's name.
//
// Every channel is tried before giving up, and the error names each attempt with
// its own reason: with more than one route configured, "weixin has no stored
// login" and "discord refused the DM" are different problems and collapsing them
// into one sentence would hide which one the user has to fix.
func (s *NotifierSet) Notify(ctx context.Context, text string) (string, error) {
	if s.Empty() {
		return "", errors.New("no notification channel is configured")
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("notification text is empty")
	}

	failures := make([]string, 0, len(s.targets))
	for _, t := range s.targets {
		if err := t.ch.NotifyUser(ctx, text); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", t.name, err))
			continue
		}
		return t.name, nil
	}
	return "", fmt.Errorf("no notification channel accepted the message: %s", strings.Join(failures, "; "))
}

// NotifiersFromConfig resolves the notification channels from raw channel
// configs — the shape config.ChannelConfig.ActiveChannels() returns (channel
// name → that channel's raw YAML subtree).
//
// It takes the raw map rather than *config.ChannelConfig on purpose: the
// Factory signature already speaks this shape, and taking it keeps this package
// out of the config package's import graph.
//
// Constructing a channel is cheap and has no network side effect (a channel
// connects in Run, not in its factory), which is what makes this safe to call at
// tool-mount time. A channel that is switched on but unusable — no factory
// registered, the factory refuses the config, no NotificationChannel
// implementation between them — is skipped with a log line: a missing
// notification route must never stop an agent from starting.
func NotifiersFromConfig(raw map[string]map[string]any, log *logger.Logger) *NotifierSet {
	set := &NotifierSet{}
	if len(raw) == 0 {
		return set
	}

	// ActiveChannels returns a map, so the enabled names must be ordered here or
	// both "the first channel that accepts" and the tool schema built from these
	// names would change from run to run.
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	registered := ListRegistered()
	for _, name := range names {
		rawCfg := raw[name]
		if !NotifyRequested(rawCfg) {
			continue
		}

		factory, ok := registered[name]
		if !ok {
			notifySkip(log, name, "no factory registered for this channel name (import its package?)")
			continue
		}

		ch, err := factory(rawCfg)
		if err != nil {
			notifySkip(log, name, fmt.Sprintf("create channel: %v", err))
			continue
		}

		notifier, ok := ch.(NotificationChannel)
		if !ok {
			notifySkip(log, name, "this channel does not implement NotificationChannel")
			continue
		}

		set.targets = append(set.targets, notifierTarget{name: name, ch: notifier})
	}
	return set
}

// notifySkip records a channel that asked to be a notification tool and cannot
// be one. It is a warning rather than an error: the user edited a config that
// this binary cannot honour, and the reason is the only thing that makes it
// fixable. A nil logger is allowed (bare agents and tests build without one).
func notifySkip(log *logger.Logger, name, reason string) {
	if log == nil {
		return
	}
	log.Warn(context.Background(), "channel: notify switch is on but this channel cannot notify",
		"channel", name, "reason", reason)
}
