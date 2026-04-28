# TODO

## /mcp command

Support a `/mcp` slash command to view and manage MCP servers during a session.

- `/mcp list` — show all configured MCP servers with their status (enabled/disabled, connected/disconnected)
- `/mcp toggle <name>` — enable or disable a specific MCP server without restarting
- `/mcp reconnect <name>` — reconnect to a disconnected server
- Should work in the existing slash-command framework (see `tui/commands.go`)
- UI could reuse the provider-selection overlay pattern or a simple message output

## Paste threshold — collapsed display

When pasting content into the TUI input box that exceeds a certain size threshold,
show a compact placeholder rather than rendering the full text inline, to avoid
flooding the chat UI. The full content is still sent to the LLM.

- **Threshold**: e.g. 5 lines
- **Placeholder format**: `[Pasted +779 lines]` or `[Pasted 3.2KB]`, styled dim
- **Behavior**: full text lives in a hidden buffer, placeholder shown in the textarea
- On Enter: placeholder is replaced with the full text before sending to the agent
- On any edit keystroke: collapse is removed, content reverts to normal textarea display
  (the paste is already "committed" so further editing works normally on the full text)
- Implementation likely in `tui/input.go` — intercept `tea.PasteMsg`, check size,
  store full text separately, set a truncated/placeholder value in the textarea

## Thinking display — collapsed with expand toggle

Optimize how thinking content is shown in the chat view. Thinking blocks can be
very long and push actual assistant responses out of view.

- **Default**: only show the last 5 lines of each thinking block, collapsed with a
  `… (N more lines)` indicator, styled in dim italic
- **Ctrl+O toggle**: while the agent is streaming, Ctrl+O switches the main view
  area between:
  - Full chat view (default)
  - Thinking-only view — shows the full live thinking output in a dedicated
    scrollable component, useful for inspecting the model's reasoning in real time
- Pressing Ctrl+O again (or when streaming ends) returns to full chat view
- The thinking component could be a separate `ThinkingView` that uses the full
  available height, with scroll support for long output
- Non-streaming (historical) thinking blocks in the chat view remain collapsed
  to last 5 lines; could add a per-block expand keybinding later
