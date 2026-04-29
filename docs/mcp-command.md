# /mcp command

## Problem

Users have MCP (Model Context Protocol) servers configured but no way to inspect or manage them from within an active Tachi session. They must restart the session to change server state.

## Behavior

A `/mcp` slash command with three subcommands:

- **`/mcp list`** — prints all configured MCP servers with their current status (enabled/disabled, connected/disconnected).
- **`/mcp toggle <name>`** — enables or disables a specific MCP server at runtime, no restart required.
- **`/mcp reconnect <name>`** — reconnects to a server that has dropped its connection.

## Implementation notes

- Wire into the existing slash-command framework in `tui/commands.go`.
- Output display can reuse the provider-selection overlay pattern, or just render as a simple message in the chat view.
- Auto-complete server names for `toggle` and `reconnect` if the tab-completion system supports it.
