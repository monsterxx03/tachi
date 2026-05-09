# Thinking display — collapsed with expand toggle

## Problem

Thinking blocks from the LLM (especially long chain-of-thought from reasoning models) can be enormous. They push actual assistant responses far out of view, degrading the chat experience.

## Behavior

- **Default collapse**: only the last 5 lines of each thinking block are shown, followed by a `… (N more lines)` indicator, styled dim + italic.
- **Ctrl+O toggle**: while the agent is streaming, Ctrl+O switches the main view area between:
  - Full chat view (default).
  - Thinking-only view — a dedicated scrollable component showing the full live thinking output in real time.
- Pressing Ctrl+O again (or when streaming ends) returns to full chat view.
- Historical (non-streaming) thinking blocks remain collapsed to last 5 lines in the chat view. A per-block expand keybinding could be added later.

## Implementation notes

- The thinking-only view could be a separate `ThinkingView` component that uses the full available height, with scroll support for long output.
- Needs a new keybinding registered in the TUI key handler for `Ctrl+O`.
- State management: track whether thinking-only mode is active; switch rendering in `View()` accordingly.
- Collapse logic for historical blocks lives in the chat message rendering code.
