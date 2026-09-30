package agent

import (
	"context"

	"github.com/monsterxx03/tachi/agent/hooks"
	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/permission/jevfiles"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
)

// Asking the human at the console is one shape, and it is written here once.
//
// Every gate that can interrupt a turn needs the same three steps: fire the
// permission-request hook, hand the frontend a confirmation card carrying the
// AGENT-rendered preview, and park until the answer arrives or the turn is
// cancelled. Three gates do it (the bash ask, the MCP search cost card, the file
// tools' judgment ask) and they must not drift: the TUI and the desktop both key
// their chrome off this event pair, and a card that skipped the hook would leave
// the transcript without the permission event the other one has.
//
// What it deliberately does NOT do is decide what an answer MEANS. Whether a
// refusal aborts the turn (the bash ask) or merely refuses this call (the file
// tools'), and which session-wide switch an 「本会话全部允许」 flips, stay with each
// gate — that is where those differences belong, and a helper that guessed one
// would silently give the others its own semantics.
//
// NOTE for parallel callers: the answer channel is per AGENT and holds one
// answer, so a gate that can run inside a parallel group must serialize its park
// (PermissionState.askMu) — two cards waiting at once would race for a single
// keystroke.
func (a *AIAgent) askFrontend(ctx context.Context, tc llm.ToolCall, preview string, ch chan<- AgentEvent) (ConfirmResponse, error) {
	a.dispatchEvent(ctx, hooks.EventPermissionRequest, hooks.Payload{
		ToolName: tc.Function.Name,
		ToolID:   tc.ID,
		ToolArgs: tc.Function.Arguments,
	})
	ch <- AgentEvent{
		Type:     AgentEventToolConfirmation,
		ToolName: tc.Function.Name,
		ToolID:   tc.ID,
		ToolArgs: tc.Function.Arguments,
		ToolDiff: preview,
	}

	select {
	case resp := <-a.Channels.ConfirmResp:
		return resp, nil
	case <-ctx.Done():
		return ConfirmDeny, ctx.Err()
	}
}

// fileToolNames maps a tool call's name onto the two things the file gate needs:
// the policy's tool identifier (which rule set applies) and the judgment's Tool
// (which tool the question is asked about). It is the ONE place the two
// spellings are related, and the bool is the gate's own guard — a name that is
// not one of the three never reaches a file rule check.
func fileToolNames(name string) (permission.FileTool, jevfiles.Tool, bool) {
	switch name {
	case tools.ToolNameRead:
		return permission.FileToolRead, jevfiles.ToolRead, true
	case tools.ToolNameWrite:
		return permission.FileToolWrite, jevfiles.ToolWrite, true
	case tools.ToolNameEdit:
		return permission.FileToolEdit, jevfiles.ToolEdit, true
	}
	return 0, "", false
}
