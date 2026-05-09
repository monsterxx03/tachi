# Paste threshold — collapsed display

## Problem

Pasting very large blocks of text into the TUI input floods the chat UI with raw content, making it hard to read the conversation. The content is still meant for the LLM — just not for the human's eyes in the input box.

## Behavior

When pasted text exceeds a threshold (e.g. 5 lines), show a compact placeholder instead of the full text in the textarea.

- **Placeholder format**: `[Pasted +779 lines]` or `[Pasted 3.2KB]`, styled dim.
- **Hidden buffer**: the full text is stored separately and sent to the agent on submit.
- **On Enter**: placeholder is transparently replaced with the full text before the message reaches the agent.
- **On any edit keystroke**: collapse is removed entirely — the paste is "committed" and further editing works normally on the full text.

## Implementation notes

- Primary work in `tui/input.go`.
- Intercept `tea.PasteMsg`, check size, store full text in a hidden field, set a truncated/placeholder value in the textarea.
- The threshold should be configurable (e.g. line count or byte size), with a sensible default.
