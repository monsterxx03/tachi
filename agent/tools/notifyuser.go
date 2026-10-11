package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// NotifySink is what "notify the user" MEANS in the entry that mounted the tool:
// a set of channels that can carry a direct message, resolved from that entry's
// config.
//
// It is an interface defined here rather than a concrete *channel.NotifierSet
// because pkg/channel already imports this package (ToolProvider.ChannelTools),
// so the dependency cannot run the other way. The same reason a delivery tool
// takes a callback (see SendFileCallback).
type NotifySink interface {
	// Channels names the routes a notification can take, in attempt order. The
	// tool description tells the model what it can reach with them.
	Channels() []string
	// Notify delivers text through the first route that accepts it, returning
	// that route's name, or an error naming every route it tried and why each
	// refused.
	Notify(ctx context.Context, text string) (string, error)
}

// NotifyUserTool lets the model reach the user proactively on a chat channel
// (IM direct message) instead of only answering the message in front of it.
//
// It is mounted only when the local config justifies it — an enabled channel
// that opted in via its `notify` switch and implements channel.NotificationChannel
// — so the tool's presence IS the statement that a notification route exists.
// The sink is a set, and it is read-only after mounting: the set holds no lock
// and each channel's own laziness is guarded inside the channel.
type NotifyUserTool struct {
	sink NotifySink
}

// NewNotifyUserTool creates the tool over a resolved sink. A nil or empty sink
// makes every call fail; that combination is not the normal path (the mount
// point checks first), so it reports rather than silently succeeding.
func NewNotifyUserTool(sink NotifySink) *NotifyUserTool {
	return &NotifyUserTool{sink: sink}
}

func (t *NotifyUserTool) Name() string { return ToolNameNotifyUser }

func (t *NotifyUserTool) Description() string {
	s := "Send the user a direct message on a chat channel (IM), reaching them " +
		"outside this conversation. Use it when the result has to arrive while " +
		"nobody is watching this session: a long task finished, a question needs " +
		"a human decision, a reminder they asked for. It is NOT how you answer " +
		"the message you are reading — reply normally for that."

	// Naming the routes is the difference between a tool the model can reason
	// about and one it has to probe. The names come from a sorted set, so the
	// schema is byte-identical between two builds of the same config (tools sit
	// at the front of the request prefix; see docs/agents/prompt-cache.md).
	if names := t.channels(); len(names) > 0 {
		s += " Available channels: " + strings.Join(names, ", ") + "."
	}
	return s
}

func (t *NotifyUserTool) channels() []string {
	if t.sink == nil {
		return nil
	}
	return t.sink.Channels()
}

func (t *NotifyUserTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"message": {
			Type:        "string",
			Description: "The notification text to deliver to the user",
		},
	}
}

func (t *NotifyUserTool) Required() []string { return []string{"message"} }

// Parallel is false so two notifications in one turn arrive in the order the
// model asked for them — same reason as SendFile.
func (t *NotifyUserTool) Parallel() bool { return false }

func (t *NotifyUserTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var p struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(p.Message) == "" {
		return "", fmt.Errorf("message is required")
	}
	if t.sink == nil {
		return "", fmt.Errorf("no notification channel is configured")
	}

	used, err := t.sink.Notify(ctx, p.Message)
	if err != nil {
		// A failed delivery is a FAILED call: the model must not tell the user
		// something arrived when no channel took it.
		return "", fmt.Errorf("notify the user: %w", err)
	}
	return fmt.Sprintf("✅ 已通过 %s 通知用户", used), nil
}
