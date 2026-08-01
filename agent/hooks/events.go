package hooks

// Hook event names dispatched by the agent. They are the contract between
// three parties: the agent loop (producer), built-in integrations such as
// Herdr (consumer), and user-defined command hooks keyed by these names in
// config.yaml. Dispatch and register against these constants rather than
// literals.
const (
	// Session lifecycle.

	// EventSessionStart fires when a new session is created (first user
	// message of a conversation).
	EventSessionStart = "session_start"
	// EventSessionEnd fires on agent Close. It is dispatched synchronously
	// so handlers (e.g. Herdr release) complete before process exit.
	EventSessionEnd = "session_end"

	// Turn lifecycle.

	// EventTurnStart fires when a user message is received, before the
	// agent loop begins.
	EventTurnStart = "turn_start"
	// EventStreamStart fires when the LLM stream emits its first output
	// (thinking, text, or tool-use delta) — the moment the frontend starts
	// rendering. Integrations (e.g. Herdr) use it to flip to "working"
	// before any tool executes.
	EventStreamStart = "stream_start"
	// EventTurnComplete fires when a turn ends — either a normal stop or
	// a length-exhausted truncation (ErrorMessage is set in the latter).
	EventTurnComplete = "turn_complete"
	// EventTurnTruncated fires when the response hit the output token
	// limit and the loop continues with a continuation prompt.
	EventTurnTruncated = "turn_truncated"
	// EventError fires on a terminal agent-loop error.
	EventError = "error"

	// Tool execution.

	// EventToolCall fires before a tool invocation. Every tool_call is
	// paired with a tool_result.
	EventToolCall = "tool_call"
	// EventToolResult fires after a tool invocation completes.
	EventToolResult = "tool_result"
	// EventPermissionRequest fires when a tool needs user confirmation.
	EventPermissionRequest = "permission_request"
	// EventPermissionResult fires when a confirmation is decided
	// (approved or denied).
	EventPermissionResult = "permission_result"
	// EventAskUserQuestion fires when the AskUserQuestion form is shown.
	EventAskUserQuestion = "ask_user_question"
	// EventAskUserResponse fires when the user answers the form.
	EventAskUserResponse = "ask_user_response"
)
