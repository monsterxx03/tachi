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
// IM channels use auto-confirm mode (PermissionModeSkip):
//   - EditFile confirmations: auto-approved
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

// AttachmentType categorises the type of file attachment in an incoming message.
type AttachmentType string

const (
	AttachmentTypeText  AttachmentType = "text"
	AttachmentTypeImage AttachmentType = "image"
	AttachmentTypeFile  AttachmentType = "file"
)

// Attachment represents a file or media attachment received from an IM channel.
type Attachment struct {
	// Type indicates the kind of attachment (text, image, or generic file).
	Type AttachmentType

	// FileName is the original filename, if available (e.g. "report.pdf").
	FileName string

	// MimeType is a best-effort MIME type guess (e.g. "image/jpeg").
	MimeType string

	// Content is the raw decrypted bytes of the attachment.
	Content []byte

	// TextContent is the UTF-8 text extracted from the file (only for text-type
	// files that were successfully decoded). Empty for binary files / images.
	TextContent string

	// Size is the plaintext size in bytes.
	Size int64

	// SavedPath is the local filesystem path where the decrypted file has been
	// persisted. When set, the LLM can use the Bash tool to read/parse the file
	// directly (e.g. pdftotext for PDFs, openpyxl for Excel) instead of relying
	// solely on TextContent. Empty means the file was not saved to disk.
	SavedPath string

	// Error is non-empty when the attachment could not be downloaded or
	// decrypted. Content/TextContent will be nil/empty in this case.
	Error string
}

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

	// Attachments holds any files or media sent alongside the text message.
	// The channel implementation is responsible for downloading and decrypting
	// the attachment data before populating this field.
	Attachments []Attachment

	// Sender is the display name of the message sender.
	//
	// Used for formatting ambient messages in whisper mode (e.g., "[群聊] 张三: ...").
	// If empty, formatting falls back to "unknown".
	Sender string

	// Directed indicates whether this message is explicitly addressed to the agent.
	//
	// Channel implementations set this based on platform semantics:
	//   - Direct messages (1:1 chat) → true
	//   - Group chat with @mention or /command → true
	//   - Group chat ordinary conversation → false (ambient)
	//
	// Default false is the safe conservative choice — channels that don't support
	// directed detection leave this at the default; the manager layer only treats
	// !Directed as ambient when GroupChat is also true.
	Directed bool

	// GroupChat indicates whether the current thread is in group chat mode.
	//
	// Channel implementations set this based on platform fields (e.g., chat type).
	// Once true for a thread, the entire session lifetime stays in group mode.
	//
	// When true, the manager layer:
	//   1. Injects whisper system prompt on session creation (once)
	//   2. Routes non-directed messages through the ambient pipeline
	//
	// Channels that don't support group chat leave this at the default (false),
	// and whisper is never activated.
	GroupChat bool

	// AskUserAnswers is set by interactive channels when an incoming message
	// is a reply to a previously-sent AskUserQuestion prompt. The keys are
	// question indices (e.g. "q0", "q1") and values are the user's answers.
	// When non-nil, the handler routes this message directly to the waiting
	// agent rather than queuing it as a new turn or steer input.
	AskUserAnswers map[string]string

	// ReferencedMessageID is the ID of the message this message is replying to,
	// if the IM platform supports threaded replies (e.g., Discord reply).
	// Empty when the message is not a reply.
	ReferencedMessageID string

	// CancelAskUser is set by interactive channels when the user explicitly
	// cancels an AskUserQuestion prompt (e.g., by clicking a "取消" button).
	// When true, AskUserAnswers is ignored and nil answers are routed to the
	// agent, signalling cancellation.
	CancelAskUser bool
}

// OutgoingAttachment represents a file or media attachment to be sent
// to an IM channel as part of an OutgoingMessage. The channel implementation
// decides how to deliver it (e.g., CDN upload for WeChat iLink).
//
// Either Data or LocalPath must be set. When LocalPath is set, the channel
// reads the file from disk at send time rather than keeping it in memory.
type OutgoingAttachment struct {
	// Type indicates the kind of attachment.
	Type AttachmentType

	// FileName is the original filename (e.g. "report.pdf").
	FileName string

	// MimeType is a best-effort MIME type guess.
	MimeType string

	// Data is the raw byte content of the file.
	// Leave nil when LocalPath is set.
	Data []byte

	// LocalPath is an alternative to Data — when set, the channel reads the
	// file from this local path at send time instead of keeping it in memory.
	// This avoids buffering large files during the agent turn.
	LocalPath string
}

// Question represents a single structured question from the agent, used by
// interactive channels to render AskUserQuestion prompts as platform-native
// UI (cards, buttons, forms) rather than plain text.
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multi_select"`
}

// QuestionOption is a single choice within a Question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// OutgoingMessage represents a response to send back to the IM channel.
type OutgoingMessage struct {
	// ThreadID routes the reply to the correct conversation.
	ThreadID string

	// Content is the response text to send. May be empty if the message
	// consists only of attachments.
	Content string

	// Attachments holds files or media to send alongside or instead of
	// the text reply. The channel implementation is responsible for
	// uploading and delivering each attachment.
	Attachments []OutgoingAttachment

	// ReplyTo references the IncomingMessage.MessageID this is replying to,
	// so the IM backend can thread the reply correctly.
	ReplyTo string
}

// HandlerResult wraps the outcome of a MessageHandler call.
//
// When Steered is true, the message was injected into an already-running
// agent turn via the steer mechanism and Reply should be discarded —
// the channel should only stop its typing indicator without sending a reply.
//
// When Buffered is true, the message was accepted into the whisper ambient
// buffer and will be processed in a future ambient turn. The channel should
// not send any reply or stop its typing indicator.
//
// When Dropped is true, the message was silently discarded (e.g. whisper
// disabled and the message was a non-directed group chat message). The
// channel should not send any reply.
type HandlerResult struct {
	Reply    OutgoingMessage // The final reply; zero value when Steered, Buffered, or Dropped.
	Steered  bool            // True if the message was injected as steer input.
	Buffered bool            // True if the message was buffered for ambient processing.
	Dropped  bool            // True if the message was silently discarded (no reply needed).
	Streamed bool            // True if the reply was sent via streaming card; channel should not sendMessage again.
	Err      error           // Non-nil when processing failed.

	// WorkDir is the resolved working directory for the thread after processing
	// the message. Set by the manager; channel implementations can use this to
	// display context (e.g., updating the Discord channel topic). Empty if
	// the handler hasn't started an agent turn (steer/buffer/slash-command).
	WorkDir string

	// Model is the resolved model name (e.g. "gpt-4o", "claude-sonnet")
	// for the thread after processing the message. Set by the manager;
	// channel implementations can display this alongside working directory
	// and git branch info (e.g., updating the Discord channel topic).
	Model string
}

// MessageHandler processes an incoming message and returns a response.
//
// Channels MUST check HandlerResult.Steered before sending a reply.
// When Steered is true:
//   - Reply is a zero-value OutgoingMessage (no reply to send)
//   - The channel should stop its typing indicator and continue
//     waiting for the original conversation to finish naturally
//
// The handler may block for an arbitrary amount of time when processing
// the first message for a thread (LLM inference + tool execution).
// Subsequent messages for the same thread while an agent turn is active
// are injected via steer and return immediately with Steered=true.
type MessageHandler func(ctx context.Context, msg IncomingMessage) HandlerResult

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

// MessageSender is an optional interface for channels that support
// proactive message delivery (not just request-response).
// Required for cron and future push notification features.
type MessageSender interface {
	// Send delivers a message to the specified thread.
	// Returns error if the thread is unknown or delivery fails.
	Send(ctx context.Context, msg OutgoingMessage) error
}

// SlashCommand represents a typed slash command that channels can execute
// programmatically without constructing fake IncomingMessage strings.
//
// This decouples channel lifecycle events from the text-based slash command
// parsing path. For example, a channel detecting a new chat room can call
// Command("new", threadID) instead of building "/new" as a message.
type SlashCommand struct {
	// Name is the command identifier (e.g., "new", "mcp", "usage", "cron", "v", "model").
	Name string

	// ThreadID provides thread context for thread-scoped commands.
	// Required for "new", "usage", "v"; ignored for global commands ("mcp", "cron", "model").
	ThreadID string

	// Args holds command arguments, space-separated (e.g., "gpt-5.2" for "/model gpt-5.2").
	// Empty for argument-less commands.
	Args string
}

// CommandHandler executes a typed SlashCommand and returns the response
// message (text + optional file attachments), the thread's current working
// directory (for channel topic updates, etc.), the resolved model name,
// and an error (if any).
type CommandHandler func(ctx context.Context, cmd SlashCommand) (reply OutgoingMessage, workDir string, model string, err error)

// CommandChannel is an optional interface for channels that need
// programmatic access to manager-level slash commands.
//
// Channels implementing this interface receive a CommandHandler via
// SetCommandHandler, called by the Manager before Run(). The handler
// can then be used at any point during the channel's lifecycle to
// execute state-changing or query operations without constructing
// pseudo-messages.
//
// Example use cases:
//   - A channel detects a new chat room → calls handler(ctx, SlashCommand{Name: "new", ThreadID: tid})
//   - Startup diagnostics → calls handler(ctx, SlashCommand{Name: "mcp"})
//   - Admin commands from platform-specific syntax mapped to slash commands
type CommandChannel interface {
	// SetCommandHandler receives the CommandHandler for programmatic
	// slash command execution. Called by Manager before Run().
	SetCommandHandler(handler CommandHandler)
}

// InteractiveChannel is an optional interface for channels that support
// interactive tool patterns (e.g., AskUserQuestion). Channels that do NOT
// implement this interface default to non-interactive mode: AskUserQuestion
// is unregistered and drainEvents auto-rejects any AskUser events.
type InteractiveChannel interface {
	Channel

	// Interactive returns true if this channel supports interactive
	// tool patterns like AskUserQuestion.
	Interactive() bool

	// PresentQuestions delivers structured questions from the agent to the
	// channel. The channel decides how to present them to the user
	// (interactive cards, buttons, forms, etc.). The user's reply arrives
	// through the normal handler path and is routed to the agent.
	PresentQuestions(ctx context.Context, threadID, replyID string, questions []Question) error
}

// SystemPromptSuffixer is an optional interface for channels that want to
// inject additional instructions into the agent's system prompt. The suffix
// is appended once per turn, after the base system prompt and any
// manager-level suffixes (e.g., whisper mode).
//
// Typical use: a channel with interactive cards (AskUserQuestion) uses this
// to tell the LLM to proactively ask the user for clarification.
type SystemPromptSuffixer interface {
	Channel
	SystemPromptSuffix() string
}

// TurnSummaryPolicy is an optional interface for channels that want to
// control whether assistant replies include the turn summary footer
// (iterations, duration, trace ID) appended by the manager.
//
// Channels that do NOT implement this interface default to showing the
// summary. A channel implementing it and returning false gets clean,
// assistant-only replies — useful for face-style UIs (e.g. the device
// channel) where technical metadata would break the interaction illusion.
type TurnSummaryPolicy interface {
	Channel

	// ShowTurnSummary reports whether assistant replies should append the
	// "回合: N 次迭代, 耗时 xx, trace: xxx" footer. Default is true.
	ShowTurnSummary() bool
}

// Autocompleter is an optional interface for channels that want to receive
// the list of available option values for slash command autocomplete.
// The Manager calls SetProviderNames and SetThinkingLevels before Run().
type Autocompleter interface {
	Channel
	SetProviderNames(names []string)
	// SetThinkingLevels receives the valid /thinking level values
	// (none/low/medium/high/xhigh/max/default) for autocomplete.
	SetThinkingLevels(levels []string)
}
