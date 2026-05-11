// Package channel provides the abstraction layer for connecting the AIAgent
// to external IM platforms (WeChat, Slack, Telegram, etc.).
//
// Architecture
//
//	┌──────────────┐     ┌──────────────────┐     ┌─────────┐
//	│  IM Backend   │ ←→ │  Channel impl     │ ←→ │  Agent   │
//	│ (WeChat etc.) │     │ (poll / webhook) │     │ (LLM)   │
//	└──────────────┘     └──────────────────┘     └─────────┘
//
// The Channel interface abstracts message transport. Implementations handle
// the specifics of connecting to an IM backend — long polling, webhooks,
// websockets, etc. — and delegate message processing to a MessageHandler.
//
// The MessageHandler is provided by ChannelManager. It manages per-thread
// sessions, creates agent instances, runs conversations synchronously with
// auto-confirm semantics, and returns responses.
//
// # Confirmation Strategy
//
// IM channels use auto-confirm mode:
//   - EditFile confirmations: auto-approved (skip_edit_confirm=true)
//   - AskUserQuestion: auto-cancelled (returns error to LLM so it can adapt)
//
// This keeps the interaction model simple — one message in, one response out.
//
// # Session Model
//
// Each ThreadID maps to a persistent session (stored on disk, like TUI sessions).
// This gives IM conversations memory across messages. Users can say "also add
// tests for that" and the agent remembers what "that" was.
//
// # Long Polling vs Callback
//
// The Channel interface supports both models naturally:
//   - Long polling: Channel.Run() loops, polls for messages, calls handler synchronously,
//     sends the reply, resumes polling.
//   - Callback/Webhook: Channel implementation receives HTTP callbacks, calls handler,
//     sends the reply.
//
// Initially only long-polling-style channels need to be supported.
package channel

import "context"

// IncomingMessage represents a message received from an IM channel.
type IncomingMessage struct {
	// ThreadID uniquely identifies the conversation thread. In WeChat this
	// might be a user ID; in Slack, a channel+thread_ts combination.
	// Each ThreadID maps to a persistent agent session.
	ThreadID string

	// MessageID is a unique identifier for this specific message. Used for
	// reply tracking (OutgoingMessage.ReplyTo).
	MessageID string

	// Content is the plain-text body of the message.
	Content string

	// ChannelID is an optional higher-level grouping (e.g., Slack channel,
	// WeChat group chat ID). Channels may use this for routing or logging.
	ChannelID string
}

// OutgoingMessage represents a response to send back to the IM channel.
type OutgoingMessage struct {
	// ThreadID routes the reply to the correct conversation.
	ThreadID string

	// Content is the response text to send.
	Content string

	// ReplyTo references the IncomingMessage.MessageID this is replying to,
	// so the IM backend can thread the reply correctly.
	ReplyTo string
}

// MessageHandler processes an incoming message and returns a response.
// It is called synchronously by the channel implementation. The handler
// may block for an arbitrary amount of time (LLM inference + tool execution),
// so channels with strict timeout requirements should wrap calls with a
// context deadline.
type MessageHandler func(ctx context.Context, msg IncomingMessage) (OutgoingMessage, error)

// Channel defines the interface for IM channel backends.
//
// Concrete implementations handle connection establishment, message
// reception, and reply delivery for a specific IM platform. The channel
// implementation is responsible for its own error handling and reconnection
// logic — it should only return from Run() on fatal errors or context
// cancellation.
type Channel interface {
	// Name returns a human-readable identifier for this channel type
	// (e.g., "wechat", "slack", "telegram").
	Name() string

	// Run starts the channel's message loop. The implementation should:
	//
	//   1. Establish a connection to the IM backend (login, websocket, etc.)
	//   2. Enter a receive loop appropriate for the backend:
	//      - Long polling: periodically fetch new messages
	//      - Webhook: listen on an HTTP server
	//      - Event stream: maintain a persistent connection
	//   3. For each incoming message, call handler(ctx, msg) and deliver
	//      the resulting OutgoingMessage back to the IM platform.
	//   4. Return when ctx is cancelled (graceful shutdown) or on
	//      unrecoverable errors.
	//
	// The handler call is synchronous — the channel pauses message
	// consumption until the handler returns. This is the simplest model
	// and works well for long polling. For high-throughput channels that
	// need concurrent processing, the implementation can spawn goroutines
	// per message and call the handler from them.
	Run(ctx context.Context, handler MessageHandler) error

	// OnStart is a lifecycle hook called by the Manager before Run().
	// Channels can use this for pre-start initialization (e.g., loading
	// credentials, warming caches, storing a greeting message).
	//
	// If OnStart returns an error, the channel is considered failed and
	// Run() will not be called for it.
	OnStart(ctx context.Context) error
}
