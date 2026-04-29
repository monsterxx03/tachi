# Pending input during streaming

## Problem

The input area is disabled while the LLM is streaming (`stateStreaming`). Users can't type or queue messages until the turn completes, forcing them to wait — sometimes for long tool-calling loops.

## Behavior

Input remains enabled during `stateStreaming`. Messages submitted during streaming are queued and sent automatically when the current turn finishes.

- **Queueing**: on Enter during streaming, the message is pushed to a `pendingQueue []string` on `Model`. The input clears and is ready for the next entry.
- **Pending indicators**: each queued message appears in the chat view as a dim italic placeholder, e.g. `[pending] your message here`, rendered below the last real message.
- **Auto-drain**: in the `AgentEventTurnComplete` handler, if `pendingQueue` is non-empty, join all entries with `"\n\n"`, clear the queue, and auto-send as a single message.
- **Ctrl+C**: cancels the current stream and drops the pending queue (user interrupted for a reason).
- **Confirmation / AskUser states**: input stays disabled during these — the user must respond to the active prompt first.
- **`/clear`** also drains/clears the pending queue.
- **Edge case**: modifying the textarea during streaming works normally — any keypress edits the current pending draft, same as `stateIdle`.

## Implementation notes

- Change `setState` in `tui/model.go`: `input.SetEnabled(st == stateIdle || st == stateStreaming)`.
- Add `pendingQueue` field to `Model` and drain logic in the `AgentEventTurnComplete` handler.
- Reuse the `chatMessage` struct with a special `Role` like `"pending"` for the placeholder display.
- Ctrl+C handler should clear `pendingQueue` alongside canceling the stream.
